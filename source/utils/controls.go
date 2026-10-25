package utils

import (
	"context"
	"errors"
	"fmt"

	"torclient/source/models"
)

// ErrBusy: the operation would collide with work that is still running.
var ErrBusy = errors.New("still in progress")

// CancelJob marks every unfinished episode of a job failed ("cancelled by
// user") and rolls up the job. It returns the cancelled ep_ids and whether
// the job is now finished (callback due). Interrupting running work is the
// pipeline's part (Pipeline.CancelRunning).
func (s *JobStore) CancelJob(ctx context.Context, jobID string) ([]string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if _, err := getJob(ctx, tx, jobID); err != nil {
		return nil, false, err
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT ep_id FROM episodes WHERE job_id = ? AND status NOT IN ('done','failed')`, jobID)
	if err != nil {
		return nil, false, err
	}
	var eps []string
	for rows.Next() {
		var ep string
		if err := rows.Scan(&ep); err != nil {
			rows.Close()
			return nil, false, err
		}
		eps = append(eps, ep)
	}
	rows.Close()
	if len(eps) == 0 {
		return nil, false, tx.Commit()
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE episodes SET status = 'failed', error = 'cancelled by user', updated_at = `+sqlNow+`
		 WHERE job_id = ? AND status NOT IN ('done','failed')`, jobID); err != nil {
		return nil, false, err
	}
	finished, err := rollupJob(ctx, tx, jobID)
	if err != nil {
		return nil, false, err
	}
	return eps, finished, tx.Commit()
}

// RetryEpisodes requeues failed episodes of a job (all failed ones when
// epIDs is empty). An episode whose anime_id/ep_id is in flight under
// another job is refused with a *ConflictError. Returns the requeued ep_ids.
func (s *JobStore) RetryEpisodes(ctx context.Context, jobID string, epIDs []string) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	job, err := getJob(ctx, tx, jobID)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, e := range epIDs {
		want[e] = true
	}
	var retry []string
	for _, ep := range job.Episodes {
		if ep.Status != models.EpFailed || (len(want) > 0 && !want[ep.EpID]) {
			continue
		}
		retry = append(retry, ep.EpID)
	}
	if len(want) > 0 && len(retry) != len(want) {
		return nil, fmt.Errorf("only failed episodes can be retried")
	}
	if len(retry) == 0 {
		return nil, tx.Commit()
	}
	busy, err := inFlightEpisodes(ctx, tx, job.AnimeID, retry)
	if err != nil {
		return nil, err
	}
	if len(busy) > 0 {
		return nil, &ConflictError{AnimeID: job.AnimeID, EpIDs: busy}
	}
	for _, ep := range retry {
		if _, err := tx.ExecContext(ctx,
			`UPDATE episodes SET status = 'queued', progress = 0, error = '', updated_at = `+sqlNow+`
			 WHERE job_id = ? AND ep_id = ?`, jobID, ep); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE jobs SET status = 'processing', updated_at = `+sqlNow+` WHERE job_id = ?`, jobID); err != nil {
		return nil, err
	}
	return retry, tx.Commit()
}

// DeleteJob removes a finished job's records (not its packaged output).
// Font references must already be released (FontCache.Release per episode).
func (s *JobStore) DeleteJob(ctx context.Context, jobID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	job, err := getJob(ctx, tx, jobID)
	if err != nil {
		return err
	}
	for _, ep := range job.Episodes {
		if !ep.Status.Terminal() {
			return fmt.Errorf("%w: episode %s is %s — cancel the job first", ErrBusy, ep.EpID, ep.Status)
		}
	}
	for _, q := range []string{
		`DELETE FROM episode_fonts WHERE job_id = ?`,
		`DELETE FROM episodes WHERE job_id = ?`,
		`DELETE FROM jobs WHERE job_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, jobID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// EpisodeActive reports whether anime_id/ep_id is queued or being processed
// under any job (its output must not be deleted then).
func (s *JobStore) EpisodeActive(ctx context.Context, animeID, epID string) (bool, error) {
	busy, err := inFlightEpisodes(ctx, s.db, animeID, []string{epID})
	return len(busy) > 0, err
}

// MarkOutputDeleted records that a packaged episode's output was deleted:
// its done rows become failed ("output deleted") so the job list stops
// offering Play and Retry downloads it again. Also drops its fingerprints.
func (s *JobStore) MarkOutputDeleted(ctx context.Context, animeID, epID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx,
		`SELECT e.job_id FROM episodes e JOIN jobs j ON j.job_id = e.job_id
		 WHERE j.anime_id = ? AND e.ep_id = ? AND e.status = 'done'`, animeID, epID)
	if err != nil {
		return err
	}
	var jobs []string
	for rows.Next() {
		var j string
		if err := rows.Scan(&j); err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	for _, j := range jobs {
		if _, err := tx.ExecContext(ctx,
			`UPDATE episodes SET status = 'failed', error = 'output deleted', output_path = '', size_bytes = 0, updated_at = `+sqlNow+`
			 WHERE job_id = ? AND ep_id = ?`, j, epID); err != nil {
			return err
		}
		if _, err := rollupJob(ctx, tx, j); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM episode_fingerprints WHERE anime_id = ? AND ep_id = ?`, animeID, epID); err != nil {
		return err
	}
	return tx.Commit()
}
