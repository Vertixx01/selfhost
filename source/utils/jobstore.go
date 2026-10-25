package utils

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"torclient/source/models"
)

var ErrNotFound = errors.New("not found")

// ConflictError means some ep_ids are already queued or processing for the
// same anime_id under another job. Nothing was inserted.
type ConflictError struct {
	AnimeID string
	EpIDs   []string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("episodes already in flight for anime %s: %s", e.AnimeID, strings.Join(e.EpIDs, ", "))
}

// querier is satisfied by both *sql.DB and *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type JobStore struct {
	db *sql.DB
}

func NewJobStore(db *sql.DB) *JobStore { return &JobStore{db: db} }

// CreateJob inserts a job and its episodes in one transaction.
//
// If job.ID already exists it returns the stored job with created=false and
// changes nothing (idempotent re-POST). If any episode is in flight for the
// same anime under another job it returns *ConflictError.
func (s *JobStore) CreateJob(ctx context.Context, job models.Job, eps []models.Episode) (*models.Job, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	if existing, err := getJob(ctx, tx, job.ID); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}

	epIDs := make([]string, len(eps))
	for i, ep := range eps {
		epIDs[i] = ep.EpID
	}
	inFlight, err := inFlightEpisodes(ctx, tx, job.AnimeID, epIDs)
	if err != nil {
		return nil, false, err
	}
	if len(inFlight) > 0 {
		return nil, false, &ConflictError{AnimeID: job.AnimeID, EpIDs: inFlight}
	}

	_, err = tx.ExecContext(ctx,
		`INSERT INTO jobs (job_id, anime_id, title, status, callback_url, episode_count)
		 VALUES (?, ?, ?, 'pending', ?, ?)`,
		job.ID, job.AnimeID, job.Title, job.CallbackURL, len(eps))
	if err != nil {
		return nil, false, fmt.Errorf("insert job: %w", err)
	}
	for _, ep := range eps {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO episodes (job_id, ep_id, title, magnet_url, priority, needs_poster, provider, file_episodes)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			job.ID, ep.EpID, ep.Title, ep.MagnetURL, ep.Priority, ep.NeedsPoster, ep.Provider, joinInts(ep.FileEpisodes))
		if err != nil {
			return nil, false, fmt.Errorf("insert episode %s: %w", ep.EpID, err)
		}
	}

	created, err := getJob(ctx, tx, job.ID)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return created, true, nil
}

func inFlightEpisodes(ctx context.Context, q querier, animeID string, epIDs []string) ([]string, error) {
	if len(epIDs) == 0 {
		return nil, nil
	}
	args := []any{animeID}
	for _, id := range epIDs {
		args = append(args, id)
	}
	rows, err := q.QueryContext(ctx,
		`SELECT DISTINCT e.ep_id FROM episodes e JOIN jobs j ON j.job_id = e.job_id
		 WHERE j.anime_id = ? AND e.status NOT IN ('done','failed')
		   AND e.ep_id IN (`+placeholders(len(epIDs))+`)
		 ORDER BY e.ep_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// GetJob returns the job with all its episodes, or ErrNotFound.
func (s *JobStore) GetJob(ctx context.Context, jobID string) (*models.Job, error) {
	return getJob(ctx, s.db, jobID)
}

func getJob(ctx context.Context, q querier, jobID string) (*models.Job, error) {
	row := q.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE job_id = ?`, jobID)
	job, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	rows, err := q.QueryContext(ctx,
		`SELECT `+episodeColumns+` FROM episodes WHERE job_id = ? ORDER BY priority, created_at, ep_id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		ep, err := scanEpisode(rows)
		if err != nil {
			return nil, err
		}
		job.Episodes = append(job.Episodes, *ep)
	}
	return job, rows.Err()
}

type JobFilter struct {
	Status  models.JobStatus // empty = any
	AnimeID string           // empty = any
	Limit   int
	Offset  int
}

// ListJobs returns jobs newest first, without their episodes.
func (s *JobStore) ListJobs(ctx context.Context, f JobFilter) ([]models.Job, error) {
	where, args := []string{"1=1"}, []any{}
	if f.Status != "" {
		where, args = append(where, "status = ?"), append(args, f.Status)
	}
	if f.AnimeID != "" {
		where, args = append(where, "anime_id = ?"), append(args, f.AnimeID)
	}
	args = append(args, f.Limit, f.Offset)

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY created_at DESC, job_id LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []models.Job{}
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, *job)
	}
	return jobs, rows.Err()
}

// AnimeTitle returns the title of the most recent job for animeID, or ErrNotFound
// if no job has used that anime_id yet.
func (s *JobStore) AnimeTitle(ctx context.Context, animeID string) (string, error) {
	var title string
	err := s.db.QueryRowContext(ctx,
		`SELECT title FROM jobs WHERE anime_id = ? ORDER BY created_at DESC LIMIT 1`, animeID).Scan(&title)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return title, err
}

// UpdateEpisodeStatus checkpoints an episode's stage. When the new status is
// terminal it also recomputes the parent job's status in the same transaction,
// and reports whether every episode in the job is now terminal (time to fire
// the callback).
func (s *JobStore) UpdateEpisodeStatus(ctx context.Context, jobID, epID string, status models.EpisodeStatus, progress float64, errMsg string) (jobFinished bool, err error) {
	if !status.Valid() {
		return false, fmt.Errorf("invalid episode status %q", status)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE episodes SET status = ?, progress = ?, error = ?, updated_at = `+sqlNow+`
		 WHERE job_id = ? AND ep_id = ?`,
		status, progress, errMsg, jobID, epID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, ErrNotFound
	}
	if status.Terminal() {
		if jobFinished, err = rollupJob(ctx, tx, jobID); err != nil {
			return false, err
		}
	}
	return jobFinished, tx.Commit()
}

// SetInfohash records the resolved infohash (Stage 1) for later dedup.
func (s *JobStore) SetInfohash(ctx context.Context, jobID, epID, infohash string) error {
	return s.updateEpisode(ctx, jobID, epID, `infohash = ?`, infohash)
}

// SetSource records the resolved infohash and the file chosen inside the
// torrent (Stage 1); together they identify the source for dedup.
func (s *JobStore) SetSource(ctx context.Context, jobID, epID, infohash, filePath string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE episodes SET infohash = ?, file_path = ?, updated_at = `+sqlNow+` WHERE job_id = ? AND ep_id = ?`,
		infohash, filePath, jobID, epID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PackagedFrom lists finished episodes (anime_id, ep_id) made from the same
// torrent file, newest first, except animeID/epID itself.
func (s *JobStore) PackagedFrom(ctx context.Context, infohash, filePath, animeID, epID string) ([][2]string, error) {
	if infohash == "" || filePath == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT j.anime_id, e.ep_id FROM episodes e JOIN jobs j ON j.job_id = e.job_id
		 WHERE e.infohash = ? AND e.file_path = ? AND e.status = 'done'
		   AND NOT (j.anime_id = ? AND e.ep_id = ?)
		 ORDER BY e.updated_at DESC`, infohash, filePath, animeID, epID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var a, e string
		if err := rows.Scan(&a, &e); err != nil {
			return nil, err
		}
		out = append(out, [2]string{a, e})
	}
	return out, rows.Err()
}

// UpdateProgress sets progress (0-100) without changing the stage.
func (s *JobStore) UpdateProgress(ctx context.Context, jobID, epID string, progress float64) error {
	return s.updateEpisode(ctx, jobID, epID, `progress = ?`, progress)
}

func (s *JobStore) updateEpisode(ctx context.Context, jobID, epID, set string, val any) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE episodes SET `+set+`, updated_at = `+sqlNow+` WHERE job_id = ? AND ep_id = ?`,
		val, jobID, epID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// rollupJob applies the §3 job status rules: all done → completed, any failed
// → failed, otherwise processing. Returns true once no episode is still running.
func rollupJob(ctx context.Context, q querier, jobID string) (bool, error) {
	var total, done, failed int
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        COUNT(*) FILTER (WHERE status = 'done'),
		        COUNT(*) FILTER (WHERE status = 'failed')
		 FROM episodes WHERE job_id = ?`, jobID).Scan(&total, &done, &failed)
	if err != nil {
		return false, err
	}
	status := models.JobProcessing
	switch {
	case total > 0 && done == total:
		status = models.JobCompleted
	case failed > 0:
		status = models.JobFailed
	}
	_, err = q.ExecContext(ctx,
		`UPDATE jobs SET status = ?, updated_at = `+sqlNow+` WHERE job_id = ?`, status, jobID)
	return done+failed == total, err
}

// QueueDepth counts episodes per status, for /health.
func (s *JobStore) QueueDepth(ctx context.Context) (map[models.EpisodeStatus]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM episodes GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[models.EpisodeStatus]int{}
	for rows.Next() {
		var st models.EpisodeStatus
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

const jobColumns = `job_id, anime_id, title, status, callback_url, episode_count, created_at, updated_at`

const episodeColumns = `job_id, ep_id, title, magnet_url, infohash, priority, needs_poster, provider,
	source_type, status, progress, error, size_bytes, output_path, created_at, updated_at, file_episodes`

type scanner interface{ Scan(dest ...any) error }

func scanJob(r scanner) (*models.Job, error) {
	var j models.Job
	err := r.Scan(&j.ID, &j.AnimeID, &j.Title, &j.Status, &j.CallbackURL, &j.EpisodeCount, &j.CreatedAt, &j.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &j, nil
}

func scanEpisode(r scanner) (*models.Episode, error) {
	var e models.Episode
	var fileEps string
	err := r.Scan(&e.JobID, &e.EpID, &e.Title, &e.MagnetURL, &e.Infohash, &e.Priority, &e.NeedsPoster,
		&e.Provider, &e.SourceType, &e.Status, &e.Progress, &e.Error, &e.SizeBytes, &e.OutputPath,
		&e.CreatedAt, &e.UpdatedAt, &fileEps)
	if err != nil {
		return nil, err
	}
	e.FileEpisodes = splitInts(fileEps)
	return &e, nil
}

func joinInts(ns []int) string {
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ",")
}

func splitInts(s string) []int {
	var out []int
	for _, f := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
