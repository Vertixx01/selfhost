package transcode

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"torclient/source/media"
)

// encodePlan is everything one ffmpeg invocation needs.
type encodePlan struct {
	Input      string
	OutDir     string // work/{job}/{ep}/work — mirrors the output layout
	Video      media.VideoStream
	Renditions []Rendition // only the ones this run produces
	Audio      []media.AudioTrack
	Rate       rateControl
	Settings   Settings
	// ForceKeyFrames are output-timeline times for every encoded rendition.
	ForceKeyFrames []float64
	KeyInfo        string // "" = no encryption
	SingleThread   bool   // crash retry (§7: retry once with -threads 1)
	// Encoder: EncoderCPU (libx264) or EncoderNVENC (decode, scale and
	// encode all on the NVIDIA GPU).
	Encoder string
}

const (
	EncoderCPU   = "cpu"
	EncoderNVENC = "nvenc"
)

// args builds one ffmpeg command: a single decode feeding every encoded
// video rendition (split + scale), stream-copied video for the fast path,
// and every audio rendition — so all outputs share one timestamp
// normalization and the source is read once.
func (p encodePlan) args() []string {
	a := []string{"-hide_banner", "-loglevel", "error", "-y"}
	var encoded []Rendition
	for _, r := range p.Renditions {
		if !r.Copy {
			encoded = append(encoded, r)
		}
	}
	gpu := p.Encoder == EncoderNVENC && len(encoded) > 0
	threads := p.Settings.Threads
	if p.SingleThread {
		threads = 1
	}
	if threads > 0 && !gpu {
		a = append(a, "-threads", strconv.Itoa(threads), "-filter_complex_threads", strconv.Itoa(threads))
	}
	if gpu {
		// NVDEC decode straight into GPU memory; frames never touch the CPU.
		a = append(a, "-hwaccel", "cuda", "-hwaccel_output_format", "cuda")
	}
	a = append(a, "-i", p.Input)
	if len(encoded) > 0 {
		// One decode → (cfr) → split → per-rendition scale + 8-bit 4:2:0.
		chain := fmt.Sprintf("[0:%d]", p.Video.Index)
		if p.Video.VFR && p.Video.FrameRate != "" {
			chain += "fps=" + p.Video.FrameRate + ","
		}
		var parts []string
		chain += fmt.Sprintf("split=%d", len(encoded))
		for i := range encoded {
			chain += fmt.Sprintf("[s%d]", i)
		}
		parts = append(parts, chain)
		for i, r := range encoded {
			if gpu {
				// scale_cuda also converts 10-bit to 8-bit NV12. (unsharp has
				// no CUDA version; downloading frames for it would cost more
				// than the GPU saves.)
				parts = append(parts, fmt.Sprintf("[s%d]scale_cuda=%d:%d:format=nv12[v%d]", i, r.Width, r.Height, i))
			} else {
				parts = append(parts, fmt.Sprintf("[s%d]scale=%d:%d:flags=lanczos,unsharp=5:5:0.8:3:3:0.4,format=yuv420p[v%d]", i, r.Width, r.Height, i))
			}
		}
		a = append(a, "-filter_complex", strings.Join(parts, ";"))
	}

	gop := keyint(p.Video.FPS, p.Settings.SegmentDuration)
	enc := 0
	for _, r := range p.Renditions {
		if r.Copy {
			a = append(a, "-map", fmt.Sprintf("0:%d", p.Video.Index), "-c:v", "copy")
		} else {
			maxrate, buf := p.Rate.MaxrateKbps, p.Rate.BufKbps
			if r.Height < 1080 {
				maxrate, buf = maxrate/2, buf/2
			}
			a = append(a, "-map", fmt.Sprintf("[v%d]", enc))
			if gpu {
				// Same keyframe contract as x264: a GOP longer than any gap in
				// the forced list, no scene-cut keyframes, forced ones as IDR.
				preset, cq := p.Settings.NvencPreset, p.Rate.CQ
				if preset == "" {
					preset = "p1"
				}
				if cq <= 0 {
					cq = p.Rate.CRF + 6
				}
				a = append(a, "-c:v", "h264_nvenc", "-preset", preset, "-tune", "hq",
					"-rc", "vbr", "-cq", strconv.Itoa(cq), "-b:v", "0",
					"-g", strconv.Itoa(gop), "-bf", "3", "-no-scenecut", "1", "-forced-idr", "1")
			} else {
				a = append(a, "-c:v", "libx264", "-preset", p.Settings.Preset, "-tune", p.Settings.Tune,
					"-crf", strconv.Itoa(p.Rate.CRF),
					"-x264-params", fmt.Sprintf("keyint=%d:min-keyint=%d:scenecut=0:rc-lookahead=60:ref=4", gop, gop))
			}
			a = append(a,
				"-maxrate", fmt.Sprintf("%dk", maxrate), "-bufsize", fmt.Sprintf("%dk", buf),
				"-profile:v", "high", "-level:v", fmt.Sprintf("%.1f", float64(r.Level)/10),
				"-force_key_frames", joinTimes(p.ForceKeyFrames))
			if threads > 0 && !gpu {
				a = append(a, "-threads", strconv.Itoa(threads))
			}
			enc++
		}
		a = append(a, "-an", "-sn", "-dn")
		a = append(a, p.hlsArgs(filepath.Join(p.OutDir, r.Name))...)
	}

	for _, t := range p.Audio {
		a = append(a, "-map", fmt.Sprintf("0:%d", t.Index), "-vn", "-sn", "-dn")
		if t.Action == "copy" {
			a = append(a, "-c:a", "copy")
		} else {
			a = append(a, "-c:a", "aac", "-b:a", fmt.Sprintf("%dk", p.Settings.AudioBitrateKbps),
				"-ac", strconv.Itoa(p.Settings.AudioChannels), "-ar", strconv.Itoa(p.Settings.AudioSampleRate))
		}
		a = append(a, p.hlsArgs(filepath.Join(p.OutDir, "audio", t.Rendition))...)
	}
	return a
}

func (p encodePlan) hlsArgs(dir string) []string {
	a := []string{"-max_muxing_queue_size", "1024",
		"-f", "hls", "-hls_time", strconv.Itoa(p.Settings.SegmentDuration),
		"-hls_playlist_type", "vod", "-hls_flags", "independent_segments",
		"-hls_segment_type", "mpegts",
		"-hls_segment_filename", filepath.Join(dir, "seg%04d.ts")}
	if p.KeyInfo != "" {
		a = append(a, "-hls_key_info_file", p.KeyInfo)
	}
	return append(a, filepath.Join(dir, "stream.m3u8"))
}

func joinTimes(ts []float64) string {
	parts := make([]string, len(ts))
	for i, t := range ts {
		parts[i] = strconv.FormatFloat(t, 'f', -1, 64)
	}
	return strings.Join(parts, ",")
}
