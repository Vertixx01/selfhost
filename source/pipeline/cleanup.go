package pipeline

import (
	"context"
	"log/slog"
	"os"
	"time"

	"torclient/source/utils"
)

// Cleaner is the Stage 11 daemon: it deletes work/{job_id}/{ep_id}/ once an
// episode has been done for WORK_TTL_HOURS (failed: FAILED_WORK_TTL_HOURS)
// and releases its font references. It never touches output/.
type Cleaner struct {
	store              *utils.JobStore
	fonts              *utils.FontCache
	layout             utils.Layout
	doneTTL, failedTTL int
	interval           time.Duration
	log                *slog.Logger
}

func NewCleaner(store *utils.JobStore, fonts *utils.FontCache, layout utils.Layout, doneTTLHours, failedTTLHours int, interval time.Duration, log *slog.Logger) *Cleaner {
	return &Cleaner{store: store, fonts: fonts, layout: layout, doneTTL: doneTTLHours, failedTTL: failedTTLHours, interval: interval, log: log}
}

// Run sweeps once at start and then every interval until ctx ends.
func (c *Cleaner) Run(ctx context.Context) {
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		if n, err := c.Sweep(ctx); err != nil && ctx.Err() == nil {
			c.log.Error("cleanup sweep", "err", err)
		} else if n > 0 {
			c.log.Info("cleanup removed expired work dirs", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sweep removes expired work dirs and returns how many it removed.
func (c *Cleaner) Sweep(ctx context.Context) (int, error) {
	refs, err := c.store.ExpiredEpisodes(ctx, c.doneTTL, c.failedTTL)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, r := range refs {
		// Fonts first: if the process dies between the two steps, the work
		// dir is still there and the next sweep retries both.
		if err := c.fonts.Release(ctx, r.JobID, r.EpID); err != nil {
			return removed, err
		}
		dir := c.layout.EpWorkDir(r.JobID, r.EpID)
		if _, err := os.Stat(dir); err != nil {
			continue // already cleaned
		}
		if err := os.RemoveAll(dir); err != nil {
			return removed, err
		}
		removed++
		os.Remove(c.layout.WorkDir(r.JobID)) // only succeeds once the job dir is empty
	}
	return removed, nil
}
