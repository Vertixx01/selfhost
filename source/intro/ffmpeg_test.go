package intro

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"torclient/source/media"
)

// Real-ffmpeg tests; skipped when ffmpeg/ffprobe aren't on PATH.

func tools(t *testing.T) media.Tools {
	t.Helper()
	ff, err1 := exec.LookPath("ffmpeg")
	fp, err2 := exec.LookPath("ffprobe")
	if err1 != nil || err2 != nil {
		t.Skip("ffmpeg/ffprobe not on PATH")
	}
	return media.Tools{FFmpeg: ff, FFprobe: fp}
}

func ffmpeg(t *testing.T, bin string, args ...string) {
	t.Helper()
	if b, err := exec.Command(bin, append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, b)
	}
}

// A 300s episode: cold open, 1.5s black+silent gap at 30s, OP 31.5–121.5,
// gap, main part, gap at 203.5, ED 205–295, gap, preview to the end.
func makeShowEpisode(t *testing.T, ff string) string {
	out := filepath.Join(t.TempDir(), "episode.mkv")
	gaps := "between(t,30,31.5)+between(t,121.5,123)+between(t,203.5,205)+between(t,295,296.5)"
	ffmpeg(t, ff,
		"-f", "lavfi", "-i", "testsrc=size=64x64:rate=5:duration=300",
		"-f", "lavfi", "-i", "sine=f=440:d=300",
		"-vf", "drawbox=enable='"+gaps+"':color=black:t=fill",
		"-af", "volume=enable='"+gaps+"':volume=0",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac",
		out)
	return out
}

func TestHeuristicsRealFile(t *testing.T) {
	tl := tools(t)
	input := makeShowEpisode(t, tl.FFmpeg)
	sm, err := tl.Analyze(t.Context(), input, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	am, err := media.MapAudio(sm)
	if err != nil {
		t.Fatal(err)
	}

	d := &Detector{FFmpeg: tl.FFmpeg}
	res, err := d.DetectEpisode(t.Context(), input, sm, am)
	if err != nil {
		t.Fatal(err)
	}
	ts := res.Timestamps
	near := func(name string, got, want float64) {
		if math.Abs(got-want) > 0.5 {
			t.Errorf("%s = %.2f, want %.2f", name, got, want)
		}
	}
	if ts.Intro == nil || ts.Outro == nil {
		t.Fatalf("not detected: %+v (warnings %v)", ts, res.Warnings)
	}
	near("intro.start", ts.Intro.Start, 31.5)
	near("intro.end", ts.Intro.End, 121.5)
	near("outro.start", ts.Outro.Start, 205)
	near("outro.end", ts.Outro.End, 295)
	if ts.Intro.Source != "silence+blackdetect" || ts.Intro.Confidence != ConfHeuristic {
		t.Errorf("intro evidence: %+v", ts.Intro)
	}

	if HasChromaprint(t.Context(), tl.FFmpeg) {
		if fp := res.Fingerprints["outro"]; fp.Offset != 0 || len(fp.Points) == 0 { // 300s < window
			t.Errorf("outro fingerprint offset %v, %d points", fp.Offset, len(fp.Points))
		}
	}

	path := filepath.Join(t.TempDir(), "timestamps.json")
	if err := Write(path, ts); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); len(b) == 0 {
		t.Error("timestamps.json empty")
	}
}

// melody is a deterministic tune: a new pitch every 0.25s chosen by a
// key-dependent pseudo-random sequence. Unlike stationary noise (whose
// fingerprint is nearly constant and matches anything similar), it changes
// over time the way real music does.
func melody(seconds float64, key int) string {
	return fmt.Sprintf("aevalsrc='0.5*sin(2*PI*(220+40*mod(floor(t*4)*%d+%d,29))*t)':s=44100:d=%.1f", key, key%7, seconds)
}

// Three audio-only episodes share an identical 40s "OP" at different
// offsets — offsets that aren't multiples of the Chromaprint hop, as in
// real releases — surrounded by episode-specific audio.
func TestFingerprintCorrelateRealAudio(t *testing.T) {
	tl := tools(t)
	if !HasChromaprint(t.Context(), tl.FFmpeg) {
		t.Skip("ffmpeg has no chromaprint muxer")
	}
	dir := t.TempDir()
	pre := map[string]float64{"ep01": 7.3, "ep02": 20.1, "ep03": 3.7}
	fps := map[string]Fingerprint{}
	for i, id := range []string{"ep01", "ep02", "ep03"} {
		out := filepath.Join(dir, id+".mka")
		ffmpeg(t, tl.FFmpeg,
			"-f", "lavfi", "-i", melody(pre[id], 11+2*i),
			"-f", "lavfi", "-i", melody(40, 7),
			"-f", "lavfi", "-i", melody(30, 23+2*i),
			"-filter_complex", "[0][1][2]concat=n=3:v=0:a=1", "-c:a", "flac", out)
		fp, err := ComputeFingerprint(t.Context(), tl.FFmpeg, out, 0, pre[id]+70, "intro")
		if err != nil {
			t.Fatal(err)
		}
		fps[id] = fp
	}

	// Chromaprint points each summarize ~2.2s of audio, so edges are only
	// accurate to about ±2s (measured: starts ~0.5s early, ends ~1.6s late).
	got := Correlate(fps, "intro")
	for id, start := range pre {
		seg := got[id]
		if seg == nil {
			t.Fatalf("%s: no match", id)
		}
		if math.Abs(seg.Start-start) > 2 || math.Abs(seg.End-(start+40)) > 2 {
			t.Errorf("%s: got %.2f–%.2f, want %.1f–%.1f", id, seg.Start, seg.End, start, start+40)
		}
	}
}
