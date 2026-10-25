package intro

import (
	"context"
	"fmt"
	"sync"

	"torclient/source/media"
)

// Detector runs the per-episode part of Stage 6 (while the MKV is still on
// disk): tier 1 chapters, tier 3 heuristics for whatever chapters didn't
// cover, and fingerprints for the series pool. Tier 2 correlation runs later
// per anime_id (Correlate) once enough episodes have fingerprints; its
// result is folded in with Merge.
type Detector struct {
	FFmpeg     string
	Heuristics HeuristicOptions

	chromaOnce sync.Once
	chroma     bool
}

type EpisodeResult struct {
	Timestamps   Timestamps
	Fingerprints map[string]Fingerprint // "intro"/"outro"; empty without chromaprint or audio
	Warnings     []string
}

// DetectEpisode never fails for "nothing found" — that's Detected=false and
// the episode is transcoded whole. Errors are only context cancellation.
func (d *Detector) DetectEpisode(ctx context.Context, input string, sm *media.StreamMap, am *media.AudioMap) (*EpisodeResult, error) {
	res := &EpisodeResult{Fingerprints: map[string]Fingerprint{}}
	audioIdx := -1
	if am != nil {
		for _, t := range am.Tracks {
			if t.Default {
				audioIdx = t.Index
			}
		}
	}

	chapters := FromChapters(sm.Chapters)
	ts := chapters
	if chapters.Intro == nil || chapters.Outro == nil {
		h, err := FromHeuristics(ctx, d.FFmpeg, input, audioIdx, sm.Video.Index, sm.Duration,
			chapters.Intro == nil, chapters.Outro == nil, d.Heuristics)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			res.Warnings = append(res.Warnings, "silence/black detection failed: "+err.Error())
		}
		ts = Merge(chapters, h)
	}
	if audioIdx < 0 {
		res.Warnings = append(res.Warnings, "no audio: black detection only, no fingerprints")
	}
	res.Timestamps = ts

	d.chromaOnce.Do(func() { d.chroma = HasChromaprint(ctx, d.FFmpeg) })
	if !d.chroma {
		res.Warnings = append(res.Warnings, "ffmpeg has no chromaprint muxer: series fingerprinting off")
	} else if audioIdx >= 0 {
		for _, part := range []string{"intro", "outro"} {
			fp, err := ComputeFingerprint(ctx, d.FFmpeg, input, audioIdx, sm.Duration, part)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s fingerprint failed: %v", part, err))
				continue
			}
			res.Fingerprints[part] = fp
		}
	}
	return res, nil
}
