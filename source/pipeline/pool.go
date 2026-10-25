package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"torclient/source/models"
	"torclient/source/utils"
)

// Pool runs workers that claim episodes and push them through the pipeline
// (§5.1). Episodes a previous run left mid-pipeline are resumed first.
//
// Worker count is MAX_CONCURRENT_TORRENTS + MAX_FFMPEG_WORKERS: the pipeline's
// own slots cap downloads and transcodes, and the extra workers let new
// downloads start while earlier episodes are transcoding.
type Pool struct {
	p       *Pipeline
	store   *utils.JobStore
	workers int
	poll    time.Duration
	log     *slog.Logger

	wake chan struct{}
	wg   sync.WaitGroup
}

func NewPool(p *Pipeline, store *utils.JobStore, workers int, log *slog.Logger) *Pool {
	return &Pool{p: p, store: store, workers: max(1, workers), poll: 5 * time.Second, log: log, wake: make(chan struct{}, 1)}
}

// Start launches the workers. claimCtx stops them taking new episodes
// (shutdown step 1); workCtx cancels in-flight work (step 2), which then
// resumes from its checkpoint on the next start.
func (pl *Pool) Start(claimCtx, workCtx context.Context) error {
	resume, err := pl.store.InFlightEpisodes(workCtx)
	if err != nil {
		return err
	}
	if len(resume) > 0 {
		pl.log.Info("resuming episodes left in flight", "count", len(resume))
	}
	queue := make(chan models.Episode, len(resume))
	for _, ep := range resume {
		queue <- ep
	}
	close(queue)

	for range pl.workers {
		pl.wg.Add(1)
		go func() {
			defer pl.wg.Done()
			pl.work(claimCtx, workCtx, queue)
		}()
	}
	return nil
}

// Wake nudges idle workers (called after a job is submitted).
func (pl *Pool) Wake() {
	select {
	case pl.wake <- struct{}{}:
	default:
	}
}

// Wait blocks until every worker has exited.
func (pl *Pool) Wait() { pl.wg.Wait() }

func (pl *Pool) work(claimCtx, workCtx context.Context, resume <-chan models.Episode) {
	for {
		if claimCtx.Err() != nil {
			return
		}
		if ep, ok := <-resume; ok {
			pl.p.Run(workCtx, ep)
			continue
		}
		ep, err := pl.store.PopNext(claimCtx)
		switch {
		case err == nil:
			pl.p.Run(workCtx, *ep)
			pl.Wake() // another queued episode may be waiting for an idle worker
			continue
		case errors.Is(err, utils.ErrQueueEmpty):
		case claimCtx.Err() != nil:
			return
		default:
			pl.log.Error("claim episode", "err", err)
		}
		select {
		case <-claimCtx.Done():
			return
		case <-pl.wake:
		case <-time.After(pl.poll):
		}
	}
}
