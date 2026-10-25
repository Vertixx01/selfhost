package utils

import (
	"context"
	"database/sql"
	"errors"

	"torclient/source/models"
)

// ErrQueueEmpty is returned by PopNext when no episode is queued.
var ErrQueueEmpty = errors.New("queue empty")

// PopNext atomically claims the next queued episode (lowest priority number
// first, then oldest) by moving it to 'resolving', and flips its job from
// 'pending' to 'processing'. Two workers can never claim the same episode:
// the select and update are one statement.
func (s *JobStore) PopNext(ctx context.Context) (*models.Episode, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	row := tx.QueryRowContext(ctx,
		`UPDATE episodes SET status = 'resolving', updated_at = `+sqlNow+`
		 WHERE (job_id, ep_id) = (
		     SELECT job_id, ep_id FROM episodes
		     WHERE status = 'queued'
		     ORDER BY priority, created_at, job_id, ep_id
		     LIMIT 1
		 )
		 RETURNING `+episodeColumns)
	ep, err := scanEpisode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrQueueEmpty
	}
	if err != nil {
		return nil, err
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE jobs SET status = 'processing', updated_at = `+sqlNow+`
		 WHERE job_id = ? AND status = 'pending'`, ep.JobID)
	if err != nil {
		return nil, err
	}
	return ep, tx.Commit()
}
