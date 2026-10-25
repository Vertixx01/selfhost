package utils

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"torclient/source/models"
)

// GetEpisode returns one episode row, or ErrNotFound.
func (s *JobStore) GetEpisode(ctx context.Context, jobID, epID string) (*models.Episode, error) {
	ep, err := scanEpisode(s.db.QueryRowContext(ctx,
		`SELECT `+episodeColumns+` FROM episodes WHERE job_id = ? AND ep_id = ?`, jobID, epID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return ep, err
}

// InFlightEpisodes lists episodes claimed but not finished — what a crash or
// shutdown left behind. The worker pool resumes these before claiming new ones.
func (s *JobStore) InFlightEpisodes(ctx context.Context) ([]models.Episode, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+episodeColumns+` FROM episodes
		 WHERE status NOT IN ('queued','done','failed') ORDER BY priority, created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Episode
	for rows.Next() {
		ep, err := scanEpisode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ep)
	}
	return out, rows.Err()
}

// SetSourceType records "bd"/"web" from the analyzer.
func (s *JobStore) SetSourceType(ctx context.Context, jobID, epID, sourceType string) error {
	return s.updateEpisode(ctx, jobID, epID, `source_type = ?`, sourceType)
}

// SetOutput records where the packaged episode lives and its total size.
func (s *JobStore) SetOutput(ctx context.Context, jobID, epID, outputPath string, sizeBytes int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE episodes SET output_path = ?, size_bytes = ?, updated_at = `+sqlNow+`
		 WHERE job_id = ? AND ep_id = ?`, outputPath, sizeBytes, jobID, epID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

type EpisodeRef struct{ JobID, EpID string }

// ExpiredEpisodes lists finished episodes whose work dirs are past their TTL
// (measured from the terminal status change).
func (s *JobStore) ExpiredEpisodes(ctx context.Context, doneTTLHours, failedTTLHours int) ([]EpisodeRef, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT job_id, ep_id FROM episodes
		 WHERE (status = 'done'   AND updated_at < strftime('%%Y-%%m-%%dT%%H:%%M:%%fZ','now','-%d hours'))
		    OR (status = 'failed' AND updated_at < strftime('%%Y-%%m-%%dT%%H:%%M:%%fZ','now','-%d hours'))`,
		doneTTLHours, failedTTLHours))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EpisodeRef
	for rows.Next() {
		var r EpisodeRef
		if err := rows.Scan(&r.JobID, &r.EpID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Ping checks the database for /health and /ready.
func (s *JobStore) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
