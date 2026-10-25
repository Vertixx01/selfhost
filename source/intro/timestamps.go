// Package intro finds OP/ED boundaries (PROJECT.MD Stage 6): chapters first,
// then series-level audio fingerprints, then per-episode silence/black
// heuristics. The best result per part wins by confidence.
package intro

import (
	"math"
	"slices"
	"strconv"
	"strings"

	"torclient/source/media"
)

// Confidence per tier. Merge keeps the highest per part, so chapters always
// beat fingerprints, which always beat heuristics.
const (
	ConfChapter     = 1.0
	ConfFingerprint = 0.95
	ConfHeuristic   = 0.7 // best case; weaker evidence scores lower
)

type Segment struct {
	Start      float64 `json:"start"`
	End        float64 `json:"end"`
	Confidence float64 `json:"confidence"`
	Source     string  `json:"source"` // chapter | fingerprint | silence | blackdetect | silence+blackdetect
}

func (s *Segment) Duration() float64 { return s.End - s.Start }

// Timestamps is timestamps.json. Intro/Outro are null when not found.
type Timestamps struct {
	Intro    *Segment `json:"intro"`
	Outro    *Segment `json:"outro"`
	Preview  *Segment `json:"preview,omitempty"` // "next episode" chapter, if marked
	Detected bool     `json:"detected"`
}

// Merge combines results from several tiers, keeping the most confident
// segment for each part.
func Merge(results ...Timestamps) Timestamps {
	var out Timestamps
	for _, r := range results {
		out.Intro = better(out.Intro, r.Intro)
		out.Outro = better(out.Outro, r.Outro)
		out.Preview = better(out.Preview, r.Preview)
	}
	out.Detected = out.Intro != nil || out.Outro != nil
	return out
}

func better(a, b *Segment) *Segment {
	if a == nil || (b != nil && b.Confidence > a.Confidence) {
		return b
	}
	return a
}

func Write(path string, ts Timestamps) error { return media.WriteJSON(path, ts) }

// KeyframeTimes builds the -force_key_frames list: a keyframe every segLen
// seconds, re-anchored at each boundary so the HLS muxer starts a segment
// exactly there. (It cuts at the first keyframe ≥ segLen after a segment's
// start, so no grid point may sit within segLen before a boundary; the
// segment before a boundary is segLen..2×segLen long.) A boundary closer
// than segLen to the previous one is dropped. Both renditions must get the
// same list.
func KeyframeTimes(duration, segLen float64, ts Timestamps) []float64 {
	var bounds []float64
	for _, s := range []*Segment{ts.Intro, ts.Outro} {
		if s == nil {
			continue
		}
		for _, b := range []float64{s.Start, s.End} {
			if b > segLen/2 && b < duration-segLen/2 { // too close to an edge to matter
				bounds = append(bounds, round3(b))
			}
		}
	}
	slices.Sort(bounds)
	bounds = slices.Compact(bounds)

	var times []float64
	next := 0.0
	for _, b := range bounds {
		for ; next <= b-segLen+1e-9; next += segLen {
			times = append(times, round3(next))
		}
		if len(times) > 0 && b-times[len(times)-1] < segLen-1e-9 {
			continue // only reachable when the previous time is itself a boundary
		}
		times = append(times, b)
		next = b + segLen
	}
	for ; next < duration; next += segLen {
		times = append(times, round3(next))
	}
	return times
}

// ForceKeyFramesArg formats times for ffmpeg's -force_key_frames.
func ForceKeyFramesArg(times []float64) string {
	parts := make([]string, len(times))
	for i, t := range times {
		parts[i] = strconv.FormatFloat(t, 'f', -1, 64)
	}
	return strings.Join(parts, ",")
}

func round3(f float64) float64 { return math.Round(f*1000) / 1000 }
