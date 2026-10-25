package pipeline

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"

	"torclient/source/intro"
)

// seriesRunner serializes correlation per anime_id. If a run is requested
// while one is going, it's remembered and done once more afterwards, so no
// episode's fingerprints are missed (PROJECT.MD Stage 6 "race safety").
type seriesRunner struct {
	mu      sync.Mutex
	running map[string]bool
	pending map[string]bool
}

func newSeriesRunner() *seriesRunner {
	return &seriesRunner{running: map[string]bool{}, pending: map[string]bool{}}
}

// Run runs fn for animeID unless a run is in progress, in which case that
// run repeats once it finishes. Blocks only the caller that does the work.
func (s *seriesRunner) Run(ctx context.Context, animeID string, fn func(context.Context)) {
	s.mu.Lock()
	if s.running[animeID] {
		s.pending[animeID] = true
		s.mu.Unlock()
		return
	}
	s.running[animeID] = true
	s.mu.Unlock()

	for {
		fn(ctx)
		s.mu.Lock()
		if !s.pending[animeID] || ctx.Err() != nil {
			delete(s.running, animeID)
			delete(s.pending, animeID)
			s.mu.Unlock()
			return
		}
		delete(s.pending, animeID)
		s.mu.Unlock()
	}
}

// correlateSeries is tier 2 of Stage 6: once enough episodes of an anime
// have fingerprints, correlate them and fold the result into every packaged
// episode's timestamps.json (chapters still win; fingerprints beat
// heuristics). Failures are logged, never fatal.
func (p *Pipeline) correlateSeries(ctx context.Context, animeID string, log *slog.Logger) {
	n, err := p.store.FingerprintedEpisodes(ctx, animeID)
	if err != nil || n < p.cfg.FingerprintMinEpisodes {
		return
	}
	results := map[string]map[string]*intro.Segment{}
	for _, part := range []string{"intro", "outro"} {
		rows, err := p.store.Fingerprints(ctx, animeID, part)
		if err != nil {
			log.Warn("load fingerprints", "err", err)
			return
		}
		fps := map[string]intro.Fingerprint{}
		for _, r := range rows {
			pts, err := intro.DecodePoints(r.Data)
			if err != nil {
				continue
			}
			fps[r.EpID] = intro.Fingerprint{Offset: r.Offset, Points: pts}
		}
		results[part] = intro.Correlate(fps, part)
	}

	updated := 0
	for epID := range unionKeys(results["intro"], results["outro"]) {
		path := filepath.Join(p.cfg.Layout.OutputDir(animeID, epID), "meta", "timestamps.json")
		var ts intro.Timestamps
		if err := readJSON(path, &ts); err != nil {
			continue // not packaged yet; its own packaging will run this again
		}
		merged := intro.Merge(ts, intro.Timestamps{Intro: results["intro"][epID], Outro: results["outro"][epID]})
		if merged.Intro == ts.Intro && merged.Outro == ts.Outro {
			continue
		}
		if err := intro.Write(path, merged); err != nil {
			log.Warn("write series timestamps", "ep_id", epID, "err", err)
			continue
		}
		updated++
	}
	if updated > 0 {
		log.Info("series fingerprint correlation", "anime_id", animeID, "episodes", n, "updated", updated)
	}
}

func unionKeys(maps ...map[string]*intro.Segment) map[string]bool {
	out := map[string]bool{}
	for _, m := range maps {
		for k := range m {
			out[k] = true
		}
	}
	return out
}
