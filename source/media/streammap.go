package media

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
)

var ErrNoVideo = errors.New("no video stream")

// StreamMap is stream_map.json: everything later stages need, parsed out of
// ffprobe once. All Index fields are absolute stream indices (ffmpeg -map 0:N).
type StreamMap struct {
	Container string  `json:"container"`
	Duration  float64 `json:"duration"`
	// StartTime is the container start (ffmpeg shifts outputs so this
	// becomes 0 unless -copyts is used; often slightly negative in MKV).
	StartTime   float64          `json:"start_time"`
	BitRateKbps int              `json:"bit_rate_kbps"`
	Video       VideoStream      `json:"video"`
	Audio       []AudioStream    `json:"audio"`
	Subtitles   []SubtitleStream `json:"subtitles"`
	Attachments []Attachment     `json:"attachments"`
	Chapters    []Chapter        `json:"chapters"`

	SourceType string `json:"source_type"` // "bd" | "web"
	// FastPath: 1080p can be stream-copied (PROJECT.MD §7c). FastPathReason
	// says why not when false.
	FastPath       bool      `json:"fast_path"`
	FastPathReason string    `json:"fast_path_reason,omitempty"`
	Keyframes      []float64 `json:"keyframes,omitempty"` // only collected for fast-path candidates
	Warnings       []string  `json:"warnings,omitempty"`
}

type VideoStream struct {
	Index       int     `json:"index"`
	Codec       string  `json:"codec"`
	Profile     string  `json:"profile,omitempty"`
	Level       int     `json:"level,omitempty"` // H.264 level ×10 (41 = 4.1)
	StartTime   float64 `json:"start_time"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	PixFmt      string  `json:"pix_fmt"`
	BitDepth    int     `json:"bit_depth"`
	FPS         float64 `json:"fps"`
	FrameRate   string  `json:"frame_rate"` // r_frame_rate, e.g. "24000/1001"
	VFR         bool    `json:"vfr"`
	BitRateKbps int     `json:"bit_rate_kbps"` // 0 if the container doesn't say
}

type AudioStream struct {
	Index       int    `json:"index"`
	Codec       string `json:"codec"`
	Profile     string `json:"profile,omitempty"` // AAC: "LC", "HE-AAC", "HE-AACv2"
	Language    string `json:"language"`          // raw tag, normalized; "und" if missing
	Title       string `json:"title,omitempty"`
	Channels    int    `json:"channels"`
	Layout      string `json:"layout,omitempty"`
	SampleRate  int    `json:"sample_rate"`
	BitRateKbps int    `json:"bit_rate_kbps"`
	Lossless    bool   `json:"lossless"`
	Default     bool   `json:"default"`
}

type SubtitleStream struct {
	Index    int    `json:"index"`
	Codec    string `json:"codec"`
	Language string `json:"language"`
	Title    string `json:"title,omitempty"`
	Forced   bool   `json:"forced"`
	Default  bool   `json:"default"`
	Bitmap   bool   `json:"bitmap"` // PGS/VobSub/DVB: can't go into HLS
}

type Attachment struct {
	Index    int    `json:"index"`
	Filename string `json:"filename"`
	MimeType string `json:"mimetype"`
	Font     bool   `json:"font"`
}

type Chapter struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Title string  `json:"title"`
}

var losslessAudio = map[string]bool{
	"flac": true, "truehd": true, "mlp": true, "alac": true, "dts": true,
}

var bitmapSubs = map[string]bool{
	"hdmv_pgs_subtitle": true, "dvd_subtitle": true, "dvb_subtitle": true, "xsub": true,
}

var fontExts = map[string]bool{".ttf": true, ".otf": true, ".ttc": true, ".otc": true, ".woff": true, ".woff2": true}

// bdBitrateKbps splits BD from WEB sources (PROJECT.MD Stage 3).
const bdBitrateKbps = 4000

// maxKeyframeGap is the longest source GOP the fast path accepts (§7c).
const maxKeyframeGap = 10.0

// Analyze probes input, writes metaDir/media_info.json and
// metaDir/stream_map.json, and returns the stream map.
func (t Tools) Analyze(ctx context.Context, input, metaDir string) (*StreamMap, error) {
	raw, probe, err := t.Probe(ctx, input)
	if err != nil {
		return nil, err
	}
	if err := writeFileAtomic(filepath.Join(metaDir, "media_info.json"), raw); err != nil {
		return nil, err
	}
	sm, err := buildStreamMap(probe)
	if err != nil {
		return nil, err
	}

	if sm.FastPath {
		kfs, err := t.Keyframes(ctx, input, sm.Video.Index)
		if err != nil {
			sm.FastPath, sm.FastPathReason = false, "could not read keyframes: "+err.Error()
		} else if gap := maxGap(kfs, sm.Duration); gap > maxKeyframeGap {
			sm.FastPath, sm.FastPathReason = false, fmt.Sprintf("source keyframe gap %.1fs > %.0fs", gap, maxKeyframeGap)
		} else {
			sm.Keyframes = kfs
		}
	}

	if err := WriteJSON(filepath.Join(metaDir, "stream_map.json"), sm); err != nil {
		return nil, err
	}
	return sm, nil
}

func buildStreamMap(p *probeOutput) (*StreamMap, error) {
	sm := &StreamMap{
		Container:   p.Format.FormatName,
		Duration:    parseFloat(p.Format.Duration),
		StartTime:   parseFloat(p.Format.StartTime),
		BitRateKbps: kbps(p.Format.BitRate),
		Video:       VideoStream{Index: -1},
	}

	audioKbps := 0
	for _, s := range p.Streams {
		title := tag(s.Tags, "title")
		lang := NormalizeLang(tag(s.Tags, "language"))
		switch s.CodecType {
		case "video":
			// Skip cover art; take the first real video stream.
			if sm.Video.Index >= 0 || s.Disposition["attached_pic"] == 1 {
				continue
			}
			sm.Video = videoFrom(s)
		case "audio":
			a := AudioStream{
				Index: s.Index, Codec: s.CodecName, Profile: s.Profile, Language: lang, Title: title,
				Channels: s.Channels, Layout: s.ChannelLayout,
				SampleRate:  int(parseFloat(s.SampleRate)),
				BitRateKbps: streamKbps(s),
				Lossless:    losslessAudio[s.CodecName] || strings.HasPrefix(s.CodecName, "pcm_"),
				Default:     s.Disposition["default"] == 1,
			}
			audioKbps += a.BitRateKbps
			sm.Audio = append(sm.Audio, a)
		case "subtitle":
			sm.Subtitles = append(sm.Subtitles, SubtitleStream{
				Index: s.Index, Codec: s.CodecName, Language: lang, Title: title,
				Forced:  s.Disposition["forced"] == 1,
				Default: s.Disposition["default"] == 1,
				Bitmap:  bitmapSubs[s.CodecName],
			})
		case "attachment":
			name := tag(s.Tags, "filename")
			mime := strings.ToLower(tag(s.Tags, "mimetype"))
			sm.Attachments = append(sm.Attachments, Attachment{
				Index: s.Index, Filename: name, MimeType: mime,
				Font: strings.Contains(mime, "font") || fontExts[strings.ToLower(filepath.Ext(name))],
			})
		}
	}
	if sm.Video.Index < 0 {
		return nil, ErrNoVideo
	}
	for _, c := range p.Chapters {
		sm.Chapters = append(sm.Chapters, Chapter{
			Start: parseFloat(c.StartTime), End: parseFloat(c.EndTime), Title: tag(c.Tags, "title"),
		})
	}

	// Containers often omit the video bitrate; estimate it from the total.
	if sm.Video.BitRateKbps == 0 && sm.BitRateKbps > audioKbps {
		sm.Video.BitRateKbps = sm.BitRateKbps - audioKbps
	}

	sm.SourceType = "web"
	for _, a := range sm.Audio {
		if a.Lossless {
			sm.SourceType = "bd"
		}
	}
	if sm.Video.BitRateKbps > bdBitrateKbps {
		sm.SourceType = "bd"
	}

	v := sm.Video
	if v.Codec == "hevc" {
		sm.Warnings = append(sm.Warnings, "HEVC source: decode to H.264 will be slower")
	}
	if v.BitDepth > 8 {
		sm.Warnings = append(sm.Warnings, fmt.Sprintf("%d-bit video: will be converted to 8-bit yuv420p", v.BitDepth))
	}
	if v.VFR {
		sm.Warnings = append(sm.Warnings, "variable frame rate: transcode will force CFR")
	}

	switch {
	case v.Codec != "h264":
		sm.FastPathReason = "video codec is " + v.Codec
	case v.PixFmt != "yuv420p":
		sm.FastPathReason = "pixel format is " + v.PixFmt
	case v.Height != 1080 || v.Width > 1920:
		sm.FastPathReason = fmt.Sprintf("resolution is %dx%d", v.Width, v.Height)
	case v.VFR:
		sm.FastPathReason = "variable frame rate"
	default:
		sm.FastPath = true
	}
	return sm, nil
}

func videoFrom(s probeStream) VideoStream {
	v := VideoStream{
		Index: s.Index, Codec: s.CodecName, Profile: s.Profile, Level: s.Level,
		StartTime: parseFloat(s.StartTime),
		Width:     s.Width, Height: s.Height, PixFmt: s.PixFmt,
		FPS: parseRate(s.AvgFrameRate), FrameRate: s.RFrameRate,
		BitRateKbps: streamKbps(s),
	}
	if v.FPS == 0 {
		v.FPS = parseRate(s.RFrameRate)
	}
	v.BitDepth, _ = strconv.Atoi(s.BitsPerRawSample)
	if v.BitDepth == 0 {
		v.BitDepth = 8
		for _, d := range []int{10, 12} {
			if strings.Contains(s.PixFmt, strconv.Itoa(d)) {
				v.BitDepth = d
			}
		}
	}
	// r_frame_rate is the base rate, avg_frame_rate the measured one; for
	// CFR they agree. >1% apart means variable frame rate.
	r, avg := parseRate(s.RFrameRate), parseRate(s.AvgFrameRate)
	v.VFR = r > 0 && avg > 0 && math.Abs(r-avg)/r > 0.01
	return v
}

func streamKbps(s probeStream) int {
	if k := kbps(s.BitRate); k > 0 {
		return k
	}
	return kbps(tag(s.Tags, "BPS")) // mkvmerge statistics tag
}

// maxGap is the longest stretch without a keyframe, including the tail.
func maxGap(kfs []float64, duration float64) float64 {
	if len(kfs) == 0 {
		return duration
	}
	gap, prev := kfs[0], kfs[0]
	for _, k := range kfs[1:] {
		gap = max(gap, k-prev)
		prev = k
	}
	return max(gap, duration-prev)
}
