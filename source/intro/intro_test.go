package intro

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"torclient/source/media"
)

func TestFromChapters(t *testing.T) {
	ts := FromChapters([]media.Chapter{
		{Start: 0, End: 95, Title: "Avant"},
		{Start: 95, End: 185, Title: "Opening Theme"},
		{Start: 185, End: 700, Title: "A Part"},
		{Start: 700, End: 1290, Title: "B Part"},
		{Start: 1290, End: 1380, Title: "ED"},
		{Start: 1380, End: 1410, Title: "Next Episode Preview"},
	})
	if ts.Intro == nil || ts.Intro.Start != 95 || ts.Intro.End != 185 || ts.Intro.Source != "chapter" {
		t.Errorf("intro: %+v", ts.Intro)
	}
	if ts.Outro == nil || ts.Outro.Start != 1290 || ts.Outro.Confidence != 1 {
		t.Errorf("outro: %+v", ts.Outro)
	}
	if ts.Preview == nil || ts.Preview.Start != 1380 || !ts.Detected {
		t.Errorf("preview: %+v detected %v", ts.Preview, ts.Detected)
	}

	for _, title := range []string{"OP", "OP1", "op 2", "Opening", "Intro", "オープニング"} {
		if FromChapters([]media.Chapter{{Start: 10, End: 100, Title: title}}).Intro == nil {
			t.Errorf("%q not recognised as OP", title)
		}
	}
	for _, title := range []string{"Prologue", "Operation Start", "Before the Opening Ceremony", "Chapter 01", "Edge"} {
		if ts := FromChapters([]media.Chapter{{Start: 10, End: 100, Title: title}}); ts.Intro != nil || ts.Outro != nil {
			t.Errorf("%q wrongly matched: %+v", title, ts)
		}
	}
	// A 7-minute "Opening" chapter is a mislabel.
	if FromChapters([]media.Chapter{{Start: 0, End: 420, Title: "Opening"}}).Intro != nil {
		t.Error("implausibly long OP chapter accepted")
	}
}

func TestMergePrefersConfidence(t *testing.T) {
	chap := Timestamps{Intro: &Segment{Start: 1, Confidence: ConfChapter, Source: "chapter"}}
	fp := Timestamps{Intro: &Segment{Start: 2, Confidence: ConfFingerprint}, Outro: &Segment{Start: 3, Confidence: ConfFingerprint}}
	heur := Timestamps{Outro: &Segment{Start: 4, Confidence: 0.6}}
	m := Merge(heur, fp, chap)
	if m.Intro.Start != 1 || m.Outro.Start != 3 || !m.Detected {
		t.Errorf("merge: intro %+v outro %+v", m.Intro, m.Outro)
	}
	if Merge(Timestamps{}).Detected {
		t.Error("empty merge marked detected")
	}
}

// hlsCuts simulates ffmpeg's HLS muxer: a new segment starts at the first
// keyframe at least segLen after the current segment's start.
func hlsCuts(kfs []float64, segLen float64) []float64 {
	cuts := []float64{kfs[0]}
	for _, k := range kfs[1:] {
		if k-cuts[len(cuts)-1] >= segLen-1e-9 {
			cuts = append(cuts, k)
		}
	}
	return cuts
}

func TestKeyframeTimesAlignSegments(t *testing.T) {
	ts := Timestamps{
		Intro: &Segment{Start: 85.5, End: 115.0},
		Outro: &Segment{Start: 1250.25, End: 1340.25},
	}
	kfs := KeyframeTimes(1420, 6, ts)
	cuts := hlsCuts(kfs, 6)
	for _, b := range []float64{85.5, 115, 1250.25, 1340.25} {
		if !slices.Contains(cuts, b) {
			t.Errorf("boundary %v is not a segment start", b)
		}
	}
	for i := 1; i < len(cuts); i++ {
		if d := cuts[i] - cuts[i-1]; d < 6-1e-9 || d >= 12 {
			t.Errorf("segment %v→%v is %vs", cuts[i-1], cuts[i], d)
		}
	}
	if kfs[0] != 0 || kfs[len(kfs)-1] >= 1420 {
		t.Errorf("keyframes start %v end %v", kfs[0], kfs[len(kfs)-1])
	}

	// No boundaries → plain 6s grid.
	plain := KeyframeTimes(30, 6, Timestamps{})
	if !slices.Equal(plain, []float64{0, 6, 12, 18, 24}) {
		t.Errorf("plain grid %v", plain)
	}
	// Boundaries 3s apart: the second is dropped, the first kept.
	close := KeyframeTimes(100, 6, Timestamps{Intro: &Segment{Start: 40, End: 43}})
	if !slices.Contains(close, 40) || slices.Contains(close, 43) {
		t.Errorf("close boundaries %v", close)
	}
	if got := ForceKeyFramesArg([]float64{0, 6, 85.5}); got != "0,6,85.5" {
		t.Errorf("arg %q", got)
	}
}

func TestParseDetectLog(t *testing.T) {
	log := []byte(`[silencedetect @ 0x1] silence_start: 29.9
[silencedetect @ 0x1] silence_end: 31.6 | silence_duration: 1.7
[blackdetect @ 0x2] black_start:30 black_end:31.5 black_duration:1.5
[silencedetect @ 0x1] silence_start: 355.2
`)
	s, b := parseDetectLog(log, 1000, 360)
	if len(s) != 2 || s[0] != (interval{1029.9, 1031.6}) || s[1] != (interval{1355.2, 1360}) {
		t.Errorf("silences %v", s)
	}
	if len(b) != 1 || b[0] != (interval{1030, 1031.5}) {
		t.Errorf("blacks %v", b)
	}
	gaps := fuseGaps(s, b)
	if len(gaps) != 2 || gaps[0].strength() != 2 || gaps[0].start != 1029.9 || gaps[0].end != 1031.6 {
		t.Errorf("fused %+v", gaps)
	}
}

func TestPickPart(t *testing.T) {
	var o HeuristicOptions
	o.defaults()
	g := func(s, e float64, sil, blk bool) gap { return gap{interval{s, e}, sil, blk} }
	gaps := []gap{
		g(0, 0, true, false),       // file start (weak)
		g(20, 21.5, true, true),    // after cold open
		g(50, 51, true, false),     // mid-OP pause (decoy)
		g(111.5, 113, true, true),  // end of OP: 90s after 21.5
		g(140, 140.5, false, true), // scene cut
	}
	seg := pickPart(gaps, o)
	if seg == nil || seg.Start != 21.5 || seg.End != 111.5 || seg.Confidence != ConfHeuristic || seg.Source != "silence+blackdetect" {
		t.Fatalf("picked %+v", seg)
	}
	if pickPart([]gap{g(0, 1, true, true), g(30, 31, true, true)}, o) != nil {
		t.Error("30s span accepted as OP")
	}
}

func TestPointsRoundTrip(t *testing.T) {
	pts := []uint32{0, 1, 0xdeadbeef, math.MaxUint32}
	got, err := DecodePoints(EncodePoints(pts))
	if err != nil || !slices.Equal(got, pts) {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := DecodePoints([]byte{1, 2, 3}); err == nil {
		t.Error("odd length accepted")
	}
}

// synthetic builds a fingerprint: random points with the shared segment
// copied in at pos, each copied point flipped in up to 3 bits (encoder noise).
func synthetic(r *rand.Rand, total, pos int, shared []uint32) []uint32 {
	pts := make([]uint32, total)
	for i := range pts {
		pts[i] = r.Uint32()
	}
	for i, p := range shared {
		for range r.IntN(4) {
			p ^= 1 << r.IntN(32)
		}
		pts[pos+i] = p
	}
	return pts
}

func TestCorrelateSynthetic(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	shared := make([]uint32, pointsFor(90)) // 90s OP
	for i := range shared {
		shared[i] = r.Uint32()
	}
	total := pointsFor(FingerprintWindow)
	starts := map[string]int{"ep01": 100, "ep02": 900, "ep03": 2500}
	fps := map[string]Fingerprint{}
	for id, pos := range starts {
		fps[id] = Fingerprint{Points: synthetic(r, total, pos, shared)}
	}
	// An outro window offset must carry through to episode time.
	fps["ep03"] = Fingerprint{Offset: 1000, Points: fps["ep03"].Points}

	got := Correlate(fps, "intro")
	for id, pos := range starts {
		seg := got[id]
		if seg == nil {
			t.Fatalf("%s: no match", id)
		}
		want := float64(pos) * PointDuration
		if id == "ep03" {
			want += 1000
		}
		if math.Abs(seg.Start-want) > 1 || math.Abs(seg.Duration()-90) > 2 {
			t.Errorf("%s: got %.2f–%.2f, want start %.2f, ~90s", id, seg.Start, seg.End, want)
		}
	}

	// Unrelated episodes: nothing.
	noise := map[string]Fingerprint{
		"a": {Points: synthetic(r, total, 0, nil)},
		"b": {Points: synthetic(r, total, 0, nil)},
	}
	if got := Correlate(noise, "intro"); len(got) != 0 {
		t.Errorf("matched pure noise: %+v", got)
	}
}
