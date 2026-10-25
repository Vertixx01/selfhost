package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"sync"
	"time"

	"torclient/source/intro"
	"torclient/source/utils"
)

// Notifier POSTs the §3 callback payload when every episode of a job is
// terminal (Stage 9).
type Notifier struct {
	store   *utils.JobStore
	client  *http.Client
	log     *slog.Logger
	backoff []time.Duration // delays between attempts; len+1 attempts total
	wg      sync.WaitGroup
}

func NewNotifier(store *utils.JobStore, log *slog.Logger) *Notifier {
	return &Notifier{
		store:   store,
		client:  &http.Client{Timeout: 15 * time.Second},
		log:     log,
		backoff: []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second},
	}
}

type callbackPayload struct {
	Status   string            `json:"status"`
	JobID    string            `json:"job_id"`
	AnimeID  string            `json:"anime_id"`
	Title    string            `json:"title"`
	Episodes []callbackEpisode `json:"episodes"`
}

type callbackEpisode struct {
	EpID           string            `json:"ep_id"`
	Status         string            `json:"status"`
	OutputPath     string            `json:"output_path"`
	MasterPlaylist string            `json:"master_playlist"`
	SizeBytes      int64             `json:"size_bytes"`
	Error          string            `json:"error"`
	Timestamps     *intro.Timestamps `json:"timestamps"`
}

// JobFinished sends the callback in the background. Wait blocks until all
// sends are done (shutdown).
func (n *Notifier) JobFinished(jobID string) {
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		if err := n.send(context.Background(), jobID); err != nil {
			n.log.Error("callback failed; job stays recorded as finished", "job_id", jobID, "err", err)
		}
	}()
}

func (n *Notifier) Wait() { n.wg.Wait() }

func (n *Notifier) send(ctx context.Context, jobID string) error {
	job, err := n.store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	if job.CallbackURL == "" {
		return nil
	}
	payload := callbackPayload{Status: string(job.Status), JobID: job.ID, AnimeID: job.AnimeID, Title: job.Title}
	for _, ep := range job.Episodes {
		ce := callbackEpisode{EpID: ep.EpID, Status: string(ep.Status), OutputPath: ep.OutputPath, SizeBytes: ep.SizeBytes, Error: ep.Error}
		if ep.OutputPath != "" {
			ce.MasterPlaylist = path.Join(ep.OutputPath, "master.m3u8")
			var ts intro.Timestamps
			if readJSON(path.Join(ep.OutputPath, "meta", "timestamps.json"), &ts) == nil {
				ce.Timestamps = &ts
			}
		}
		payload.Episodes = append(payload.Episodes, ce)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	var lastErr error
	for attempt := 0; attempt <= len(n.backoff); attempt++ {
		if attempt > 0 {
			time.Sleep(n.backoff[attempt-1])
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, job.CallbackURL, bytes.NewReader(body))
		if err != nil {
			return err // malformed URL: retrying won't help
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := n.client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 300 {
				n.log.Info("callback delivered", "job_id", jobID, "status", job.Status, "attempt", attempt+1)
				return nil
			}
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		lastErr = err
		n.log.Warn("callback attempt failed", "job_id", jobID, "attempt", attempt+1, "err", err)
	}
	return fmt.Errorf("after %d attempts: %w", len(n.backoff)+1, lastErr)
}
