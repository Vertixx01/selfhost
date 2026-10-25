package utils

import "context"

// StoredFingerprint is one row of episode_fingerprints.
type StoredFingerprint struct {
	EpID   string
	Offset float64 // episode time of the first point
	Data   []byte  // raw little-endian chromaprint points
}

// SaveFingerprint upserts an episode's intro or outro fingerprint. Rows are
// keyed by (anime_id, ep_id), not job, so re-processing an episode replaces it.
func (s *JobStore) SaveFingerprint(ctx context.Context, animeID, epID, part string, offset float64, data []byte) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO episode_fingerprints (anime_id, ep_id, part, fingerprint, start_offset)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (anime_id, ep_id, part) DO UPDATE SET
		     fingerprint = excluded.fingerprint, start_offset = excluded.start_offset,
		     created_at = `+sqlNow,
		animeID, epID, part, data, offset)
	return err
}

// Fingerprints loads every stored fingerprint of one part for an anime.
func (s *JobStore) Fingerprints(ctx context.Context, animeID, part string) ([]StoredFingerprint, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ep_id, start_offset, fingerprint FROM episode_fingerprints
		 WHERE anime_id = ? AND part = ? ORDER BY ep_id`, animeID, part)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredFingerprint
	for rows.Next() {
		var f StoredFingerprint
		if err := rows.Scan(&f.EpID, &f.Offset, &f.Data); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FingerprintedEpisodes counts episodes of an anime with an intro fingerprint,
// to decide when FINGERPRINT_MIN_EPISODES is reached.
func (s *JobStore) FingerprintedEpisodes(ctx context.Context, animeID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM episode_fingerprints WHERE anime_id = ? AND part = 'intro'`, animeID).Scan(&n)
	return n, err
}
