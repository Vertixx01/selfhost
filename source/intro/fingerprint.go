package intro

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"sort"
	"strconv"
	"strings"

	"torclient/source/media"
)

// Tier 2 (PROJECT.MD Stage 6): Chromaprint fingerprints of each episode's
// first and last FingerprintWindow seconds; the OP/ED is the longest run
// that matches between episodes of the same series. Same technique as
// Jellyfin's Intro Skipper plugin.

const (
	// PointDuration is the time one Chromaprint point covers: a 1365.33-sample
	// hop at Chromaprint's internal 11025 Hz (measured: 0.1237s with ffmpeg 6).
	PointDuration = 1365.333 / 11025

	FingerprintWindow = 600.0 // seconds fingerprinted at each end

	maxBitErrors = 6   // of 32 bits; more and two points don't "match"
	maxGapSec    = 3.5 // a run survives this much unmatched audio
	minMatchSec  = 15.0
	maxIntroSec  = 150.0
	maxOutroSec  = 200.0
)

// Fingerprint is one window of an episode. Offset is the episode time of
// Points[0], so outro windows map back to real timestamps.
type Fingerprint struct {
	Offset float64
	Points []uint32
}

// HasChromaprint reports whether ffmpeg was built with the chromaprint
// muxer (jellyfin-ffmpeg is). Without it tier 2 is skipped.
func HasChromaprint(ctx context.Context, ffmpeg string) bool {
	out, _, err := media.RunCapture(ctx, ffmpeg, "-hide_banner", "-muxers")
	return err == nil && strings.Contains(string(out), " chromaprint ")
}

// ComputeFingerprint fingerprints audio stream audioIdx over the intro window
// (start of the file) or the outro window (last FingerprintWindow seconds).
func ComputeFingerprint(ctx context.Context, ffmpeg, input string, audioIdx int, duration float64, part string) (Fingerprint, error) {
	offset := 0.0
	if part == "outro" {
		offset = max(0, duration-FingerprintWindow)
	} else if part != "intro" {
		return Fingerprint{}, fmt.Errorf("unknown part %q", part)
	}
	out, _, err := media.RunCapture(ctx, ffmpeg, "-hide_banner", "-loglevel", "error",
		"-ss", ftoa(offset), "-t", ftoa(FingerprintWindow), "-i", input,
		"-map", "0:"+strconv.Itoa(audioIdx), "-ac", "1",
		"-f", "chromaprint", "-fp_format", "raw", "-")
	if err != nil {
		return Fingerprint{}, err
	}
	pts, err := DecodePoints(out)
	if err != nil {
		return Fingerprint{}, err
	}
	return Fingerprint{Offset: offset, Points: pts}, nil
}

// EncodePoints / DecodePoints convert to the raw little-endian form stored in
// episode_fingerprints.fingerprint (same as ffmpeg's -fp_format raw).
func EncodePoints(pts []uint32) []byte {
	b := make([]byte, 4*len(pts))
	for i, p := range pts {
		binary.LittleEndian.PutUint32(b[4*i:], p)
	}
	return b
}

func DecodePoints(b []byte) ([]uint32, error) {
	if len(b)%4 != 0 {
		return nil, errors.New("fingerprint length is not a multiple of 4")
	}
	pts := make([]uint32, len(b)/4)
	for i := range pts {
		pts[i] = binary.LittleEndian.Uint32(b[4*i:])
	}
	return pts, nil
}

// pointsFor converts seconds to a whole number of fingerprint points.
func pointsFor(seconds float64) int { return int(seconds / PointDuration) }

// match is the longest matching run between two fingerprints, in points.
type match struct{ aStart, aEnd, bStart, bEnd int }

func (m match) length() int { return m.aEnd - m.aStart }

// Correlate finds each episode's shared segment among the others: for every
// pair it finds the longest matching run, and each episode keeps the longest
// run it took part in. part ("intro"/"outro") sets the length limit.
// Episodes without a plausible match are absent from the result.
//
// Precision is about ±2s: each point summarizes ~2.2s of audio, so runs
// tend to start slightly early and end ~1.5s late. Long stretches of
// silence or stationary noise fingerprint almost identically and can extend
// a run; snapping edges to nearby silence/black gaps would tighten both.
func Correlate(fps map[string]Fingerprint, part string) map[string]*Segment {
	maxSec := maxIntroSec
	if part == "outro" {
		maxSec = maxOutroSec
	}
	ids := make([]string, 0, len(fps))
	for id := range fps {
		ids = append(ids, id)
	}
	sort.Strings(ids) // deterministic tie-breaks

	best := map[string]*Segment{}
	consider := func(id string, fp Fingerprint, start, end int) {
		seg := &Segment{
			Start:      round3(fp.Offset + float64(start)*PointDuration),
			End:        round3(fp.Offset + float64(end)*PointDuration),
			Confidence: ConfFingerprint,
			Source:     "fingerprint",
		}
		if d := seg.Duration(); d < minMatchSec || d > maxSec {
			return
		}
		if cur := best[id]; cur == nil || seg.Duration() > cur.Duration() {
			best[id] = seg
		}
	}
	for i, a := range ids {
		for _, b := range ids[i+1:] {
			m, ok := longestMatch(fps[a].Points, fps[b].Points)
			if !ok {
				continue
			}
			consider(a, fps[a], m.aStart, m.aEnd)
			consider(b, fps[b], m.bStart, m.bEnd)
		}
	}
	return best
}

// longestMatch finds candidate alignments from exactly-equal points (an
// inverted index), then for the most-voted shifts walks the overlap with a
// bit-error tolerance and keeps the longest run.
func longestMatch(a, b []uint32) (match, bool) {
	index := make(map[uint32][]int, len(b))
	for j, p := range b {
		index[p] = append(index[p], j)
	}
	votes := map[int]int{}
	for i, p := range a {
		for _, j := range index[p] {
			votes[j-i]++
		}
	}
	type shiftVotes struct{ shift, n int }
	var cands []shiftVotes
	for s, n := range votes {
		if n >= 2 {
			cands = append(cands, shiftVotes{s, n})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].n != cands[j].n {
			return cands[i].n > cands[j].n
		}
		return cands[i].shift < cands[j].shift
	})
	if len(cands) > 8 {
		cands = cands[:8]
	}

	maxGap := pointsFor(maxGapSec)
	var best match
	found := false
	for _, c := range cands {
		for _, shift := range []int{c.shift - 1, c.shift, c.shift + 1} { // hop misalignment
			if m, ok := runAt(a, b, shift, maxGap); ok && (!found || m.length() > best.length()) {
				best, found = m, true
			}
		}
	}
	return best, found
}

// runAt aligns a[i] with b[i+shift] and returns the longest stretch of
// matching points, bridging unmatched gaps up to maxGap points.
func runAt(a, b []uint32, shift, maxGap int) (match, bool) {
	lo, hi := max(0, -shift), min(len(a), len(b)-shift)
	runStart, last := -1, -1
	var best match
	found := false
	for i := lo; i < hi; i++ {
		if bits.OnesCount32(a[i]^b[i+shift]) > maxBitErrors {
			continue
		}
		if runStart < 0 || i-last > maxGap {
			runStart = i
		}
		last = i
		if l := last + 1 - runStart; !found || l > best.length() {
			best = match{runStart, last + 1, runStart + shift, last + 1 + shift}
			found = true
		}
	}
	return best, found
}
