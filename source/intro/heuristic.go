package intro

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"

	"torclient/source/media"
)

// Tier 3: an OP/ED is bracketed by short quiet and/or dark gaps, and TV
// anime OPs/EDs are almost always ~90s (1:29–1:31). Look for two gaps that
// far apart near the start (intro) and end (outro) of the episode.
type HeuristicOptions struct {
	Window      float64 // seconds analysed at each end (default 360)
	PartMin     float64 // shortest OP/ED accepted (default 80)
	PartMax     float64 // longest OP/ED accepted (default 100)
	PartTypical float64 // preferred length (default 90)
}

func (o *HeuristicOptions) defaults() {
	if o.Window <= 0 {
		o.Window = 360
	}
	if o.PartMin <= 0 {
		o.PartMin = 80
	}
	if o.PartMax <= 0 {
		o.PartMax = 100
	}
	if o.PartTypical <= 0 {
		o.PartTypical = 90
	}
}

type interval struct{ start, end float64 }

// gap is a quiet and/or dark stretch; strength 2 when silence and black agree.
type gap struct {
	interval
	silence, black bool
}

func (g gap) strength() int {
	n := 0
	if g.silence {
		n++
	}
	if g.black {
		n++
	}
	return n
}

var (
	silenceStartRe = regexp.MustCompile(`silence_start:\s*(-?[\d.]+)`)
	silenceEndRe   = regexp.MustCompile(`silence_end:\s*(-?[\d.]+)`)
	blackRe        = regexp.MustCompile(`black_start:\s*(-?[\d.]+)\s+black_end:\s*(-?[\d.]+)`)
)

// detectGaps runs silencedetect (if audioIdx ≥ 0) and blackdetect (if
// videoIdx ≥ 0) over [start, start+length) in a single decode, returning
// gaps in episode time.
func detectGaps(ctx context.Context, ffmpeg, input string, audioIdx, videoIdx int, start, length float64) ([]interval, []interval, error) {
	args := []string{"-hide_banner", "-nostats", "-ss", ftoa(start), "-t", ftoa(length), "-i", input}
	if audioIdx >= 0 {
		args = append(args, "-map", "0:"+strconv.Itoa(audioIdx), "-ac", "2",
			"-af", "silencedetect=noise=-30dB:d=0.5", "-f", "null", "-")
	}
	if videoIdx >= 0 {
		// Black detection doesn't need detail: shrink first, it's most of the cost.
		args = append(args, "-map", "0:"+strconv.Itoa(videoIdx), "-an",
			"-vf", "scale=160:-2,blackdetect=d=0.3:pic_th=0.98", "-f", "null", "-")
	}
	_, stderr, err := media.RunCapture(ctx, ffmpeg, args...)
	if err != nil {
		return nil, nil, err
	}
	silences, blacks := parseDetectLog(stderr, start, length)
	return silences, blacks, nil
}

// parseDetectLog reads silencedetect/blackdetect lines. Times in the log are
// relative to the -ss seek point; offset converts them to episode time.
func parseDetectLog(log []byte, offset, length float64) (silences, blacks []interval) {
	open := math.NaN()
	sc := bufio.NewScanner(bytes.NewReader(log))
	for sc.Scan() {
		line := sc.Text()
		if m := silenceStartRe.FindStringSubmatch(line); m != nil {
			open = max(parseF(m[1]), 0)
		}
		if m := silenceEndRe.FindStringSubmatch(line); m != nil && !math.IsNaN(open) {
			silences = append(silences, interval{offset + open, offset + parseF(m[1])})
			open = math.NaN()
		}
		if m := blackRe.FindStringSubmatch(line); m != nil {
			blacks = append(blacks, interval{offset + max(parseF(m[1]), 0), offset + parseF(m[2])})
		}
	}
	if !math.IsNaN(open) { // silence running to the end of the window
		silences = append(silences, interval{offset + open, offset + length})
	}
	return silences, blacks
}

// fuseGaps merges silences and blacks; ones that overlap (within 0.5s) become
// one stronger gap.
func fuseGaps(silences, blacks []interval) []gap {
	var gaps []gap
	for _, s := range silences {
		gaps = append(gaps, gap{interval: s, silence: true})
	}
	for _, b := range blacks {
		merged := false
		for i := range gaps {
			g := &gaps[i]
			if g.silence && !g.black && b.start <= g.end+0.5 && b.end >= g.start-0.5 {
				g.black = true
				g.start, g.end = min(g.start, b.start), max(g.end, b.end)
				merged = true
				break
			}
		}
		if !merged {
			gaps = append(gaps, gap{interval: b, black: true})
		}
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].start < gaps[j].start })
	return gaps
}

// pickPart finds the gap pair whose inner span (first.end → second.start)
// best matches an OP/ED. Returns nil if none fits.
func pickPart(gaps []gap, o HeuristicOptions) *Segment {
	var best *Segment
	bestScore := math.Inf(-1)
	for i, a := range gaps {
		for _, b := range gaps[i+1:] {
			l := b.start - a.end
			if l < o.PartMin || l > o.PartMax {
				continue
			}
			strength := a.strength() + b.strength()
			score := float64(strength) - math.Abs(l-o.PartTypical)/10
			if score <= bestScore {
				continue
			}
			bestScore = score
			best = &Segment{Start: round3(a.end), End: round3(b.start), Confidence: heuristicConf(strength), Source: sourceOf(a, b)}
		}
	}
	return best
}

// heuristicConf: both ends confirmed by silence and black → 0.7; one end → 0.6; else 0.5.
func heuristicConf(strength int) float64 {
	switch {
	case strength >= 4:
		return ConfHeuristic
	case strength == 3:
		return 0.6
	}
	return 0.5
}

func sourceOf(gs ...gap) string {
	s, b := false, false
	for _, g := range gs {
		s, b = s || g.silence, b || g.black
	}
	switch {
	case s && b:
		return "silence+blackdetect"
	case b:
		return "blackdetect"
	}
	return "silence"
}

// FromHeuristics runs tier 3 for the parts requested. The episode's start
// and end count as weak (single-signal) gaps, so an OP at 0:00 or an ED that
// runs to the last frame can still be bracketed, but a real silence+black
// gap nearby wins over the edge.
func FromHeuristics(ctx context.Context, ffmpeg, input string, audioIdx, videoIdx int, duration float64, wantIntro, wantOutro bool, o HeuristicOptions) (Timestamps, error) {
	o.defaults()
	var ts Timestamps
	edge := func(at float64) gap {
		return gap{interval: interval{at, at}, silence: audioIdx >= 0, black: audioIdx < 0 && videoIdx >= 0}
	}

	// Windows never cross the midpoint, so on a short episode the outro
	// search can't find the OP again (and vice versa).
	if wantIntro {
		length := min(o.Window, duration/2)
		s, b, err := detectGaps(ctx, ffmpeg, input, audioIdx, videoIdx, 0, length)
		if err != nil {
			return ts, fmt.Errorf("intro window: %w", err)
		}
		ts.Intro = pickPart(append([]gap{edge(0)}, fuseGaps(s, b)...), o)
	}
	if wantOutro {
		start := max(0, duration-o.Window, duration/2)
		if ts.Intro != nil {
			start = max(start, ts.Intro.End)
		}
		s, b, err := detectGaps(ctx, ffmpeg, input, audioIdx, videoIdx, start, duration-start)
		if err != nil {
			return ts, fmt.Errorf("outro window: %w", err)
		}
		ts.Outro = pickPart(append(fuseGaps(s, b), edge(duration)), o)
	}
	ts.Detected = ts.Intro != nil || ts.Outro != nil
	return ts, nil
}

func parseF(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', 3, 64) }
