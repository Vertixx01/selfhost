package transcode

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"torclient/source/media"
)

// subtitleOutput is one WebVTT rendition written to OutDir/subs/.
type subtitleOutput struct {
	Track    media.SubtitleTrack
	Base     string // file name without extension: "eng", "eng_signs"
	Playlist string // "subs/eng.m3u8"
}

// writeSubtitles converts every extracted track to WebVTT under
// outDir/subs/, adds the X-TIMESTAMP-MAP header that ties cue times to the
// video's MPEG-TS clock, writes a one-segment playlist per track (HLS
// renditions must point at a playlist, not a bare .vtt), and copies the
// original .ass next to it for libass players. tsMap is the MPEG-TS PTS
// (90 kHz) of source time 0.
func (t *Transcoder) writeSubtitles(ctx context.Context, subsDir, outDir string, subs *media.SubtitleResult, tsMap int64, duration float64) ([]subtitleOutput, []string) {
	if subs == nil || len(subs.Tracks) == 0 {
		return nil, nil
	}
	dst := filepath.Join(outDir, "subs")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return nil, []string{"subtitles: " + err.Error()}
	}
	var outs []subtitleOutput
	var warnings []string
	for _, tr := range subs.Tracks {
		src := filepath.Join(subsDir, tr.File)
		base := strings.TrimSuffix(tr.File, filepath.Ext(tr.File))
		vtt := filepath.Join(dst, base+".vtt")

		var err error
		if tr.Format == "ass" {
			// Cue times are already in source time (extracted with -copyts).
			_, err = media.Run(ctx, t.FFmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", src, "-c:s", "webvtt", vtt)
			if err == nil {
				err = copyFile(src, filepath.Join(dst, tr.File))
			}
		} else {
			err = copyFile(src, vtt)
		}
		if err == nil {
			err = addTimestampMap(vtt, tsMap)
		}
		if err == nil {
			err = writeSubtitlePlaylist(filepath.Join(dst, base+".m3u8"), base+".vtt", duration)
		}
		if err != nil {
			if ctx.Err() != nil {
				return outs, warnings
			}
			warnings = append(warnings, fmt.Sprintf("subtitle %s: %v (left out of the master playlist)", tr.File, err))
			continue
		}
		outs = append(outs, subtitleOutput{Track: tr, Base: base, Playlist: "subs/" + base + ".m3u8"})
	}
	return outs, warnings
}

// addTimestampMap inserts X-TIMESTAMP-MAP right after the WEBVTT line.
// Without it players guess the mapping and cues drift from the video.
func addTimestampMap(path string, mpegts int64) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	s := strings.TrimPrefix(string(b), string(rune(0xFEFF)))
	first, rest, _ := strings.Cut(s, "\n")
	if !strings.HasPrefix(strings.TrimSpace(first), "WEBVTT") {
		return errors.New("not a WebVTT file")
	}
	if strings.Contains(s, "X-TIMESTAMP-MAP") {
		return nil
	}
	out := strings.TrimRight(first, "\r") + "\nX-TIMESTAMP-MAP=MPEGTS:" + strconv.FormatInt(mpegts, 10) + ",LOCAL:00:00:00.000\n" + rest
	return os.WriteFile(path, []byte(out), 0o644)
}

func writeSubtitlePlaylist(path, vttName string, duration float64) error {
	pl := fmt.Sprintf("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-MEDIA-SEQUENCE:0\n#EXTINF:%.3f,\n%s\n#EXT-X-ENDLIST\n",
		int(math.Ceil(duration)), duration, vttName)
	return os.WriteFile(path, []byte(pl), 0o644)
}

// firstVideoPTS returns the MPEG-TS start PTS (90 kHz) of a video segment,
// decrypting it first when the rendition is encrypted.
func (t *Transcoder) firstVideoPTS(ctx context.Context, segPath string, key *Key) (int64, error) {
	probePath := segPath
	if key != nil {
		plain, err := decryptSegment(segPath, key)
		if err != nil {
			return 0, err
		}
		tmp := segPath + ".plain.ts"
		if err := os.WriteFile(tmp, plain, 0o600); err != nil {
			return 0, err
		}
		defer os.Remove(tmp)
		probePath = tmp
	}
	out, err := media.Run(ctx, t.FFprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=start_pts", "-of", "csv=p=0", probePath)
	if err != nil {
		return 0, err
	}
	// The value can be printed once per section (stream, program): take the first.
	first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strconv.ParseInt(strings.Trim(strings.TrimSpace(first), ","), 10, 64)
}

// decryptSegment undoes HLS AES-128: CBC over the whole segment with PKCS#7
// padding, using the playlist's IV.
func decryptSegment(path string, key *Key) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	k, err := os.ReadFile(key.KeyPath)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("encrypted segment is not a whole number of AES blocks")
	}
	cipher.NewCBCDecrypter(block, key.IV).CryptBlocks(data, data)
	pad := int(data[len(data)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(data) {
		return nil, errors.New("bad PKCS#7 padding (wrong key or IV?)")
	}
	return data[:len(data)-pad], nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}
