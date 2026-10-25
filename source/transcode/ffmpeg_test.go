package transcode

import (
	"bytes"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"torclient/source/intro"
	"torclient/source/media"
)

// Real-ffmpeg tests; skipped when ffmpeg/ffprobe aren't on PATH.

func realTranscoder(t *testing.T) (*Transcoder, media.Tools) {
	t.Helper()
	ff, err1 := exec.LookPath("ffmpeg")
	fp, err2 := exec.LookPath("ffprobe")
	if err1 != nil || err2 != nil {
		t.Skip("ffmpeg/ffprobe not on PATH")
	}
	return &Transcoder{FFmpeg: ff, FFprobe: fp, Settings: testSettings()}, media.Tools{FFmpeg: ff, FFprobe: fp}
}

const assDoc = `[Script Info]
ScriptType: v4.00+

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,Arial,48,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,2,0,2,10,10,10,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:02.00,0:00:05.00,Default,,0,0,0,,{\b1}Hello{\b0} world
`

// makeSource: 20s 1080p24 H.264 (keyframe every gopFrames), FLAC jpn + AAC
// eng audio, one ASS track.
func makeSource(t *testing.T, ff string, gopFrames string) string {
	t.Helper()
	dir := t.TempDir()
	ass := filepath.Join(dir, "s.ass")
	os.WriteFile(ass, []byte(assDoc), 0o644)
	out := filepath.Join(dir, "episode.mkv")
	cmd := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=24:duration=20",
		"-f", "lavfi", "-i", "sine=f=440:d=20", "-f", "lavfi", "-i", "sine=f=660:d=20", "-i", ass,
		"-map", "0", "-map", "1", "-map", "2", "-map", "3",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-g", gopFrames,
		"-c:a:0", "flac", "-c:a:1", "aac", "-c:s", "copy",
		"-metadata:s:a:0", "language=jpn", "-metadata:s:a:1", "language=eng", "-metadata:s:s:0", "language=eng",
		out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make source: %v\n%s", err, b)
	}
	return out
}

type prepared struct {
	req Request
	sm  *media.StreamMap
}

func prepare(t *testing.T, tools media.Tools, input string) prepared {
	t.Helper()
	work := t.TempDir()
	meta := filepath.Join(work, "meta")
	sm, err := tools.Analyze(t.Context(), input, meta)
	if err != nil {
		t.Fatal(err)
	}
	am, err := media.MapAudio(sm)
	if err != nil {
		t.Fatal(err)
	}
	subsDir := filepath.Join(work, "subs")
	subs, err := tools.ExtractSubtitles(t.Context(), input, sm, subsDir)
	if err != nil {
		t.Fatal(err)
	}
	return prepared{sm: sm, req: Request{
		Input: input, OutDir: filepath.Join(work, "work"), MetaDir: meta, SubsDir: subsDir,
		Streams: sm, Audio: am, Subs: subs,
	}}
}

func durations(t *testing.T, path string) []float64 {
	t.Helper()
	pl, err := readPlaylist(path)
	if err != nil {
		t.Fatal(err)
	}
	var d []float64
	for _, s := range pl.Segments {
		d = append(d, s.Duration)
	}
	return d
}

func TestTranscodeEncryptedRealFile(t *testing.T) {
	tc, tools := realTranscoder(t)
	p := prepare(t, tools, makeSource(t, tc.FFmpeg, "250"))
	p.sm.FastPath = false // exercise the full encode
	keyPath, _ := filepath.Abs(filepath.Join(p.req.MetaDir, "enc.key"))
	p.req.KeyURI = "file:" + filepath.ToSlash(keyPath) // lets ffmpeg play it back below
	p.req.Timestamps = intro.Timestamps{Intro: &intro.Segment{Start: 9.5, End: 16}}

	res, err := tc.Run(t.Context(), p.req)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Encrypted || res.FastPath || !slices.Equal(res.Renditions, []string{"1080p", "720p"}) {
		t.Fatalf("result %+v", res)
	}

	// Same cuts in both renditions, and a cut exactly at the OP boundary.
	out := p.req.OutDir
	d1080 := durations(t, filepath.Join(out, "1080p", "stream.m3u8"))
	d720 := durations(t, filepath.Join(out, "720p", "stream.m3u8"))
	if !slices.Equal(d1080, d720) {
		t.Fatalf("renditions cut differently:\n1080p %v\n 720p %v", d1080, d720)
	}
	starts, at := []float64{}, 0.0
	for _, d := range d1080 {
		starts = append(starts, round3(at))
		at += d
	}
	// Playlist offsets count from the first video frame. Half a frame of
	// tolerance: anything looser hides an off-by-one-frame timeline error.
	if p.sm.StartTime == 0 {
		t.Log("note: source container start is 0, so a start-offset mix-up wouldn't show here")
	}
	for _, b := range []float64{9.5, 16} {
		want := b - p.sm.Video.StartTime
		found := false
		for _, s := range starts {
			found = found || math.Abs(s-want) < 0.5/24
		}
		if !found {
			t.Errorf("no segment starts at boundary %.3f (starts %v)", want, starts)
		}
	}

	// Every segment really is AES-128 encrypted MPEG-TS.
	key, err := ensureKey(p.req.MetaDir, p.req.KeyURI)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"1080p/seg0000.ts", "720p/seg0001.ts", "audio/jpn/seg0000.ts", "audio/eng/seg0002.ts"} {
		raw, _ := os.ReadFile(filepath.Join(out, rel))
		plain, err := decryptSegment(filepath.Join(out, rel), key)
		if err != nil || len(plain)%188 != 0 || plain[0] != 0x47 || plain[188] != 0x47 {
			t.Errorf("%s does not decrypt to MPEG-TS: %v", rel, err)
			continue
		}
		// Ciphertext may start with 0x47 by chance (1 in 256): compare instead.
		if bytes.Equal(raw[:188], plain[:188]) {
			t.Errorf("%s is not encrypted", rel)
		}
	}

	// Audio: FLAC re-encoded, AAC copied, both present in the master.
	master, _ := os.ReadFile(res.Master)
	m := string(master)
	for _, want := range []string{`URI="audio/jpn/stream.m3u8"`, `URI="audio/eng/stream.m3u8"`, `URI="subs/eng.m3u8"`,
		`RESOLUTION=1920x1080`, `RESOLUTION=1280x720`, `avc1.640029`, `avc1.640028`, "1080p/stream.m3u8", "720p/stream.m3u8"} {
		if !strings.Contains(m, want) {
			t.Errorf("master missing %q:\n%s", want, m)
		}
	}

	// Subtitles: WebVTT with the TS clock map, playlist, and the ASS kept.
	vtt, _ := os.ReadFile(filepath.Join(out, "subs", "eng.vtt"))
	if !strings.Contains(string(vtt), "X-TIMESTAMP-MAP=MPEGTS:") || !strings.Contains(string(vtt), "Hello") || strings.Contains(string(vtt), `\b1`) {
		t.Errorf("eng.vtt:\n%s", vtt)
	}
	for _, f := range []string{"eng.m3u8", "eng.ass"} {
		if _, err := os.Stat(filepath.Join(out, "subs", f)); err != nil {
			t.Errorf("subs/%s: %v", f, err)
		}
	}

	// ffmpeg can play the whole package back through the master playlist,
	// fetching the key and decrypting every segment.
	play := exec.Command(tc.FFmpeg, "-hide_banner", "-loglevel", "error", "-allowed_extensions", "ALL",
		"-i", res.Master, "-map", "0:v:0", "-map", "0:a:0", "-f", "null", "-")
	if b, err := play.CombinedOutput(); err != nil {
		t.Errorf("playback through master failed: %v\n%s", err, b)
	}
}

func TestTranscodeFastPathRealFile(t *testing.T) {
	tc, tools := realTranscoder(t)
	p := prepare(t, tools, makeSource(t, tc.FFmpeg, "48")) // 2s GOP → fast-path eligible
	if !p.sm.FastPath {
		t.Fatalf("source should be fast-path eligible: %s", p.sm.FastPathReason)
	}
	res, err := tc.Run(t.Context(), p.req)
	if err != nil {
		t.Fatal(err)
	}
	if !res.FastPath || res.Encrypted {
		t.Fatalf("result %+v", res)
	}
	d1080 := durations(t, filepath.Join(p.req.OutDir, "1080p", "stream.m3u8"))
	d720 := durations(t, filepath.Join(p.req.OutDir, "720p", "stream.m3u8"))
	// Cut points must match exactly. Only the final segment's length may
	// differ a little: the copy keeps the MKV's millisecond-rounded last
	// frame duration, the encoder computes its own.
	n := len(d1080)
	if len(d720) != n || !slices.Equal(d1080[:n-1], d720[:n-1]) || math.Abs(d1080[n-1]-d720[n-1]) > 0.1 {
		t.Fatalf("720p not aligned to the copied 1080p:\n1080p %v\n 720p %v", d1080, d720)
	}
}

// GPU path: the same guarantees as the CPU path. Skipped without a usable
// NVIDIA GPU.
func requireNVENC(t *testing.T, tc *Transcoder) {
	t.Helper()
	if err := ProbeNVENC(t.Context(), tc.FFmpeg); err != nil {
		t.Skipf("NVENC not usable here: %v", err)
	}
}

func TestTranscodeNVENCRealFile(t *testing.T) {
	tc, tools := realTranscoder(t)
	requireNVENC(t, tc)
	p := prepare(t, tools, makeSource(t, tc.FFmpeg, "250"))
	p.sm.FastPath = false
	keyPath, _ := filepath.Abs(filepath.Join(p.req.MetaDir, "enc.key"))
	p.req.KeyURI = "file:" + filepath.ToSlash(keyPath)
	p.req.Timestamps = intro.Timestamps{Intro: &intro.Segment{Start: 9.5, End: 16}}
	p.req.Encoder = EncoderNVENC

	res, err := tc.Run(t.Context(), p.req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Encoder != EncoderNVENC {
		t.Fatalf("encoder %q (fell back to CPU?) warnings %v", res.Encoder, res.Warnings)
	}
	out := p.req.OutDir
	d1080 := durations(t, filepath.Join(out, "1080p", "stream.m3u8"))
	d720 := durations(t, filepath.Join(out, "720p", "stream.m3u8"))
	if !slices.Equal(d1080, d720) {
		t.Fatalf("NVENC renditions cut differently:\n1080p %v\n 720p %v", d1080, d720)
	}
	starts, at := []float64{}, 0.0
	for _, d := range d1080 {
		starts = append(starts, round3(at))
		at += d
	}
	for _, b := range []float64{9.5, 16} {
		want := b - p.sm.Video.StartTime
		if !slices.ContainsFunc(starts, func(s float64) bool { return math.Abs(s-want) < 0.5/24 }) {
			t.Errorf("no segment starts at boundary %.3f (starts %v)", want, starts)
		}
	}
	// It really is NVENC output (encoder tag) and plays back through the master.
	seg := filepath.Join(out, "1080p", "seg0000.ts")
	key, _ := ensureKey(p.req.MetaDir, p.req.KeyURI)
	plain, err := decryptSegment(seg, key)
	if err != nil || plain[0] != 0x47 {
		t.Fatalf("decrypt: %v", err)
	}
	play := exec.Command(tc.FFmpeg, "-hide_banner", "-loglevel", "error", "-allowed_extensions", "ALL",
		"-i", res.Master, "-map", "0:v:0", "-map", "0:a:0", "-f", "null", "-")
	if b, err := play.CombinedOutput(); err != nil {
		t.Errorf("playback through master failed: %v\n%s", err, b)
	}
}

func TestTranscodeNVENCFastPathRealFile(t *testing.T) {
	tc, tools := realTranscoder(t)
	requireNVENC(t, tc)
	p := prepare(t, tools, makeSource(t, tc.FFmpeg, "48"))
	if !p.sm.FastPath {
		t.Fatalf("not fast-path eligible: %s", p.sm.FastPathReason)
	}
	p.req.Encoder = EncoderNVENC
	res, err := tc.Run(t.Context(), p.req)
	if err != nil {
		t.Fatal(err)
	}
	if !res.FastPath || res.Encoder != EncoderNVENC {
		t.Fatalf("result %+v", res)
	}
	d1080 := durations(t, filepath.Join(p.req.OutDir, "1080p", "stream.m3u8"))
	d720 := durations(t, filepath.Join(p.req.OutDir, "720p", "stream.m3u8"))
	n := len(d1080)
	if len(d720) != n || !slices.Equal(d1080[:n-1], d720[:n-1]) {
		t.Fatalf("NVENC 720p not aligned to copied 1080p:\n1080p %v\n 720p %v", d1080, d720)
	}
}

func TestTranscodeGPUFailureFallsBackToCPU(t *testing.T) {
	tc, tools := realTranscoder(t)
	p := prepare(t, tools, makeSource(t, tc.FFmpeg, "250"))
	p.sm.FastPath = false
	p.req.Encoder = EncoderNVENC
	tc.Settings.NvencPreset = "p99" // invalid: NVENC run fails on any machine
	res, err := tc.Run(t.Context(), p.req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Encoder != EncoderCPU {
		t.Fatalf("encoder %q, want the CPU fallback", res.Encoder)
	}
	if _, err := readPlaylist(filepath.Join(p.req.OutDir, "720p", "stream.m3u8")); err != nil {
		t.Fatal(err)
	}
}

// A copyable 1080p source above FAST_PATH_MAX_KBPS is re-encoded, not copied.
func TestHighBitrateSourceIsReencoded(t *testing.T) {
	tc, tools := realTranscoder(t)
	tc.Settings.FastPathMaxKbps = 500 // the testsrc2 fixture is far above this
	p := prepare(t, tools, makeSource(t, tc.FFmpeg, "48"))
	if !p.sm.FastPath {
		t.Fatalf("fixture should be copyable: %s", p.sm.FastPathReason)
	}
	res, err := tc.Run(t.Context(), p.req)
	if err != nil {
		t.Fatal(err)
	}
	if res.FastPath || !slices.ContainsFunc(res.Warnings, func(w string) bool { return strings.Contains(w, "fast path skipped") }) {
		t.Fatalf("result %+v", res)
	}
	m, _ := os.ReadFile(res.Master)
	if !strings.Contains(string(m), "avc1.640029") { // our own 1080p encode (level 4.1), not the source's
		t.Errorf("1080p not re-encoded:\n%s", m)
	}
}
