package media

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ffprobe -print_format json output, only the fields we read.
type probeOutput struct {
	Streams  []probeStream  `json:"streams"`
	Format   probeFormat    `json:"format"`
	Chapters []probeChapter `json:"chapters"`
}

type probeStream struct {
	Index            int               `json:"index"`
	CodecType        string            `json:"codec_type"`
	CodecName        string            `json:"codec_name"`
	Profile          string            `json:"profile"`
	Level            int               `json:"level"`
	StartTime        string            `json:"start_time"`
	Width            int               `json:"width"`
	Height           int               `json:"height"`
	PixFmt           string            `json:"pix_fmt"`
	RFrameRate       string            `json:"r_frame_rate"`
	AvgFrameRate     string            `json:"avg_frame_rate"`
	BitRate          string            `json:"bit_rate"`
	BitsPerRawSample string            `json:"bits_per_raw_sample"`
	Channels         int               `json:"channels"`
	ChannelLayout    string            `json:"channel_layout"`
	SampleRate       string            `json:"sample_rate"`
	Disposition      map[string]int    `json:"disposition"`
	Tags             map[string]string `json:"tags"`
}

type probeFormat struct {
	FormatName string `json:"format_name"`
	Duration   string `json:"duration"`
	StartTime  string `json:"start_time"`
	BitRate    string `json:"bit_rate"`
}

type probeChapter struct {
	StartTime string            `json:"start_time"`
	EndTime   string            `json:"end_time"`
	Tags      map[string]string `json:"tags"`
}

// Probe runs ffprobe on input. It returns the raw JSON (saved as
// media_info.json) and the parsed form. Any failure means the file is
// unreadable/corrupt (PROJECT.MD Stage 3).
func (t Tools) Probe(ctx context.Context, input string) ([]byte, *probeOutput, error) {
	raw, err := run(ctx, t.FFprobe, "-v", "error", "-print_format", "json",
		"-show_streams", "-show_format", "-show_chapters", input)
	if err != nil {
		return nil, nil, fmt.Errorf("ffprobe failed (corrupt or unreadable file): %w", err)
	}
	var out probeOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, nil, fmt.Errorf("ffprobe returned invalid JSON: %w", err)
	}
	if len(out.Streams) == 0 {
		return nil, nil, fmt.Errorf("ffprobe found no streams (corrupt or unreadable file)")
	}
	return raw, &out, nil
}

// Keyframes lists the video keyframe timestamps (seconds) from packet flags.
// It only demuxes, so it's much cheaper than decoding.
func (t Tools) Keyframes(ctx context.Context, input string, streamIndex int) ([]float64, error) {
	out, err := run(ctx, t.FFprobe, "-v", "error", "-select_streams", strconv.Itoa(streamIndex),
		"-show_entries", "packet=pts_time,flags", "-of", "csv=p=0", input)
	if err != nil {
		return nil, err
	}
	var kfs []float64
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Split(strings.TrimSpace(line), ",")
		if len(fields) < 2 || !strings.Contains(fields[1], "K") {
			continue
		}
		if ts, err := strconv.ParseFloat(fields[0], 64); err == nil {
			kfs = append(kfs, ts)
		}
	}
	return kfs, nil
}

// tag looks a tag up case-insensitively, also matching Matroska's
// language-suffixed statistics tags ("BPS-eng").
func tag(tags map[string]string, key string) string {
	for k, v := range tags {
		if strings.EqualFold(k, key) || strings.HasPrefix(strings.ToUpper(k), strings.ToUpper(key)+"-") {
			return v
		}
	}
	return ""
}

func parseFloat(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

// parseRate parses "24000/1001" → 23.976; 0 if unknown.
func parseRate(s string) float64 {
	num, den, ok := strings.Cut(s, "/")
	if !ok {
		return parseFloat(s)
	}
	d := parseFloat(den)
	if d == 0 {
		return 0
	}
	return parseFloat(num) / d
}

func kbps(bitsPerSecond string) int { return int(parseFloat(bitsPerSecond) / 1000) }
