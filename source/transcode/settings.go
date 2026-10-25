// Package transcode is Stage 7 (PROJECT.MD): one ffmpeg run turns the MKV
// into H.264 HLS renditions plus AAC audio renditions, optionally AES-128
// encrypted, then subtitles become WebVTT playlists and a master playlist
// ties everything together.
package transcode

import (
	"context"
	"fmt"
	"math"
	"strings"

	"torclient/config"
	"torclient/source/media"
)

// Settings are the encode knobs from config (§6, §7a).
type Settings struct {
	CRFBD, CRFWeb                  int
	MaxrateBDKbps, BufsizeBDKbps   int
	MaxrateWebKbps, BufsizeWebKbps int
	Preset, Tune                   string
	AudioBitrateKbps               int
	AudioChannels                  int
	AudioSampleRate                int
	SegmentDuration                int

	// NVENC (GPU) equivalents: preset p1 (fastest) … p7, and constant-quality
	// targets per source type (cq ≈ x264 CRF + 6 gives similar size).
	NvencPreset           string
	NvencCQBD, NvencCQWeb int

	// FastPathMaxKbps: only stream-copy 1080p when the source video is at or
	// below this bitrate (0 = no limit). Streaming WEB-DLs often run 6–9
	// Mbit/s; copying them made 1080p renditions 3–4x a re-encode's size.
	FastPathMaxKbps int
	// Threads caps ffmpeg's CPU threads (0 = all cores), e.g. to match a VPS.
	Threads int
}

// allowFastPath: the analyzer found the stream copyable, and it isn't larger
// than re-encoding would make it. An unknown bitrate counts as too large.
func (s Settings) allowFastPath(sm *media.StreamMap) bool {
	if !sm.FastPath {
		return false
	}
	return s.FastPathMaxKbps <= 0 || (sm.Video.BitRateKbps > 0 && sm.Video.BitRateKbps <= s.FastPathMaxKbps)
}

// CRF bounds for the auto-adjust loop (§7a, §7 table).
const (
	maxCRF       = 28
	minCRFBD     = 18
	minCRFWeb    = 19
	maxCRFPasses = 3 // first encode + at most two adjustments
)

// rateControl is the CRF/VBV for one source type; 720p gets half the caps.
type rateControl struct {
	CRF                  int
	CQ                   int // NVENC; moved in step with CRF by the auto-adjust
	MaxrateKbps, BufKbps int
	MinCRF               int
}

func (s Settings) rateFor(sourceType string) rateControl {
	if sourceType == "bd" {
		return rateControl{s.CRFBD, s.NvencCQBD, s.MaxrateBDKbps, s.BufsizeBDKbps, minCRFBD}
	}
	return rateControl{s.CRFWeb, s.NvencCQWeb, s.MaxrateWebKbps, s.BufsizeWebKbps, minCRFWeb}
}

// Rendition is one video variant.
type Rendition struct {
	Name          string // dir name: "1080p"
	Width, Height int
	Level         int  // H.264 level ×10
	Copy          bool // fast path: stream-copied source
}

// planRenditions picks 1080p and 720p, never upscaling: a 720p source gets
// only 720p, a smaller one a single rendition at its own height. Widths keep
// the source aspect ratio (rounded to even).
func planRenditions(v media.VideoStream, fastPath bool) []Rendition {
	var out []Rendition
	for _, h := range []int{1080, 720} {
		if h <= v.Height {
			out = append(out, Rendition{Name: fmt.Sprintf("%dp", h), Height: h})
		}
	}
	if len(out) == 0 {
		h := v.Height &^ 1
		out = append(out, Rendition{Name: fmt.Sprintf("%dp", h), Height: h})
	}
	for i := range out {
		r := &out[i]
		r.Width = evenRound(float64(v.Width) * float64(r.Height) / float64(v.Height))
		r.Level = levelFor(r.Height, v.FPS)
		if i == 0 && fastPath && r.Height == v.Height {
			r.Copy, r.Width = true, v.Width
			if v.Level > 0 {
				r.Level = v.Level
			}
		}
	}
	return out
}

func evenRound(f float64) int { return int(math.Round(f/2)) * 2 }

// levelFor: 1080p needs 4.1 (4.2 above 30 fps); 720p 4.0 (3.2 above 30 fps
// would be too low for 60 fps at our maxrate, so 4.2 there too).
func levelFor(height int, fps float64) int {
	switch {
	case fps > 30:
		return 42
	case height > 720:
		return 41
	}
	return 40
}

// avc1 codec string (RFC 6381): profile_idc, constraint flags, level_idc.
func avcCodec(profile string, level int) string {
	idc, constraints := 0x64, 0x00 // High
	switch strings.ToLower(profile) {
	case "main":
		idc = 0x4D
	case "baseline":
		idc = 0x42
	case "constrained baseline":
		idc, constraints = 0x42, 0x40
	}
	return fmt.Sprintf("avc1.%02x%02x%02x", idc, constraints, level)
}

// aacCodec maps an AAC profile to its mp4a object type.
func aacCodec(profile string) string {
	switch strings.ToUpper(profile) {
	case "HE-AAC":
		return "mp4a.40.5"
	case "HE-AACV2":
		return "mp4a.40.29"
	}
	return "mp4a.40.2" // LC (also what we encode)
}

// keyint is just above the longest gap our forced-keyframe list can produce
// (2 × segment), so x264 never inserts a keyframe of its own; with
// scenecut=0 the forced list is then the only source of keyframes, which is
// what makes every rendition cut its segments at the same times.
func keyint(fps float64, segment int) int {
	if fps <= 0 {
		fps = 30
	}
	return int(math.Ceil(fps*float64(2*segment))) + int(math.Ceil(fps))
}

// SettingsFrom copies the encode settings out of the loaded config.
func SettingsFrom(c *config.Config) Settings {
	return Settings{
		CRFBD: c.CRFBD, CRFWeb: c.CRFWeb,
		MaxrateBDKbps: c.MaxrateBDKbps, BufsizeBDKbps: c.BufsizeBDKbps,
		MaxrateWebKbps: c.MaxrateWebKbps, BufsizeWebKbps: c.BufsizeWebKbps,
		Preset: c.Preset, Tune: c.Tune,
		AudioBitrateKbps: c.AudioBitrateKbps, AudioChannels: c.AudioChannels,
		AudioSampleRate: c.AudioSampleRate, SegmentDuration: c.SegmentDuration,
		NvencPreset: c.NvencPreset, NvencCQBD: c.NvencCQBD, NvencCQWeb: c.NvencCQWeb,
		FastPathMaxKbps: c.FastPathMaxKbps, Threads: c.FFmpegThreads,
	}
}

// ProbeNVENC reports whether NVENC encoding and CUDA decoding actually work
// here: ffmpeg builds often include h264_nvenc even on machines without an
// NVIDIA GPU (like the Xeon target), so presence in -encoders isn't enough.
func ProbeNVENC(ctx context.Context, ffmpeg string) error {
	_, err := media.Run(ctx, ffmpeg, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=24:duration=0.5",
		"-vf", "hwupload_cuda,scale_cuda=640:360:format=nv12",
		"-c:v", "h264_nvenc", "-preset", "p1", "-f", "null", "-")
	return err
}
