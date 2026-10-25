package transcode

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var errNoSegments = errors.New("playlist has no segments")

// MediaPlaylist is the part of an ffmpeg-written stream.m3u8 we need.
type MediaPlaylist struct {
	Segments  []Segment
	Encrypted bool
	Ended     bool // #EXT-X-ENDLIST present (ffmpeg finished cleanly)
}

type Segment struct {
	URI      string
	Duration float64
	Bytes    int64
}

// readPlaylist parses a media playlist and stats every segment, failing if
// one is missing or empty (§7 "HLS segment corruption").
func readPlaylist(path string) (*MediaPlaylist, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	pl := &MediaPlaylist{}
	dur := math.NaN()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "#EXTINF:"):
			v, _, _ := strings.Cut(strings.TrimPrefix(line, "#EXTINF:"), ",")
			if dur, err = strconv.ParseFloat(v, 64); err != nil {
				return nil, fmt.Errorf("%s: bad EXTINF %q", path, line)
			}
		case strings.HasPrefix(line, "#EXT-X-KEY:") && strings.Contains(line, "METHOD=AES-128"):
			pl.Encrypted = true
		case line == "#EXT-X-ENDLIST":
			pl.Ended = true
		case line != "" && !strings.HasPrefix(line, "#"):
			if math.IsNaN(dur) {
				return nil, fmt.Errorf("%s: segment %q without EXTINF", path, line)
			}
			st, err := os.Stat(filepath.Join(filepath.Dir(path), line))
			if err != nil || st.Size() == 0 {
				return nil, fmt.Errorf("%s: segment %s missing or empty", path, line)
			}
			pl.Segments = append(pl.Segments, Segment{URI: line, Duration: dur, Bytes: st.Size()})
			dur = math.NaN()
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(pl.Segments) == 0 {
		return nil, fmt.Errorf("%s: %w", path, errNoSegments)
	}
	if !pl.Ended {
		return nil, fmt.Errorf("%s: no #EXT-X-ENDLIST (encode cut short?)", path)
	}
	return pl, nil
}

func (pl *MediaPlaylist) TotalBytes() int64 {
	var n int64
	for _, s := range pl.Segments {
		n += s.Bytes
	}
	return n
}

func (pl *MediaPlaylist) Duration() float64 {
	var d float64
	for _, s := range pl.Segments {
		d += s.Duration
	}
	return d
}

// PeakBitrate is the highest per-segment bitrate (HLS BANDWIDTH).
func (pl *MediaPlaylist) PeakBitrate() int {
	peak := 0.0
	for _, s := range pl.Segments {
		if s.Duration > 0.5 { // a tiny last segment would overstate the peak
			peak = max(peak, float64(s.Bytes*8)/s.Duration)
		}
	}
	if peak == 0 {
		peak = pl.AverageBitrate()
	}
	return int(math.Ceil(peak))
}

func (pl *MediaPlaylist) AverageBitrate() float64 {
	if d := pl.Duration(); d > 0 {
		return float64(pl.TotalBytes()*8) / d
	}
	return 0
}

// cutTimes returns each segment's start time (output timeline, first
// segment at start).
func (pl *MediaPlaylist) cutTimes(start float64) []float64 {
	times := make([]float64, len(pl.Segments))
	t := start
	for i, s := range pl.Segments {
		times[i] = round3(t)
		t += s.Duration
	}
	return times
}

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }
