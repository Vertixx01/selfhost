package utils

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// FontCache is the content-addressed font store (PROJECT.MD Stage 4):
// fonts/{sha256[:16]}/{first-seen filename}, ref-counted per episode_fonts row.
//
// Every operation runs inside one write transaction (BEGIN IMMEDIATE) and
// touches the cache dir while holding it, so a Register can never race a
// Release that is deleting the same hash's directory.
type FontCache struct {
	db     *sql.DB
	layout Layout
}

func NewFontCache(db *sql.DB, layout Layout) *FontCache {
	return &FontCache{db: db, layout: layout}
}

type EpisodeFont struct {
	Hash      string // 16 hex chars
	Filename  string // this episode's name for it (used in output/subs/fonts/)
	CachePath string // canonical copy in the cache
}

// Register adds the font at src for an episode under filename. New content is
// copied into the cache; known content just gains a reference. Registering the
// same (episode, hash, filename) again is a no-op, so a retried stage doesn't
// inflate ref counts. The caller may delete src afterwards.
func (c *FontCache) Register(ctx context.Context, jobID, epID, src, filename string) (string, error) {
	hash, size, err := hashFile(src)
	if err != nil {
		return "", err
	}

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	var canonical string
	err = tx.QueryRowContext(ctx, `SELECT filename FROM font_cache WHERE hash = ?`, hash).Scan(&canonical)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if err := copyFileAtomic(src, c.layout.FontCacheFile(hash, filename)); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO font_cache (hash, filename, size_bytes, ref_count) VALUES (?, ?, ?, 0)`,
			hash, filename, size); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	}

	res, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO episode_fonts (job_id, ep_id, hash, filename) VALUES (?, ?, ?, ?)`,
		jobID, epID, hash, filename)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		if _, err := tx.ExecContext(ctx, `UPDATE font_cache SET ref_count = ref_count + 1 WHERE hash = ?`, hash); err != nil {
			return "", err
		}
	}
	return hash, tx.Commit()
}

// EpisodeFonts lists an episode's fonts for packaging (Stage 8).
func (c *FontCache) EpisodeFonts(ctx context.Context, jobID, epID string) ([]EpisodeFont, error) {
	rows, err := c.db.QueryContext(ctx,
		`SELECT ef.hash, ef.filename, fc.filename FROM episode_fonts ef
		 JOIN font_cache fc ON fc.hash = ef.hash
		 WHERE ef.job_id = ? AND ef.ep_id = ? ORDER BY ef.filename`, jobID, epID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EpisodeFont
	for rows.Next() {
		var f EpisodeFont
		var canonical string
		if err := rows.Scan(&f.Hash, &f.Filename, &canonical); err != nil {
			return nil, err
		}
		f.CachePath = c.layout.FontCacheFile(f.Hash, canonical)
		out = append(out, f)
	}
	return out, rows.Err()
}

// Release drops an episode's font references (cleanup daemon, Stage 11) and
// deletes cache entries nothing references any more.
func (c *FontCache) Release(ctx context.Context, jobID, epID string) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT hash FROM episode_fonts WHERE job_id = ? AND ep_id = ?`, jobID, epID)
	if err != nil {
		return err
	}
	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return err
		}
		hashes = append(hashes, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, h := range hashes { // one decrement per row, matching Register
		if _, err := tx.ExecContext(ctx, `UPDATE font_cache SET ref_count = ref_count - 1 WHERE hash = ?`, h); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM episode_fonts WHERE job_id = ? AND ep_id = ?`, jobID, epID); err != nil {
		return err
	}
	for _, h := range hashes {
		var refs int
		if err := tx.QueryRowContext(ctx, `SELECT ref_count FROM font_cache WHERE hash = ?`, h).Scan(&refs); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue // already removed earlier in this loop
			}
			return err
		}
		if refs > 0 {
			continue
		}
		if err := os.RemoveAll(c.layout.FontCacheDir(h)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM font_cache WHERE hash = ?`, h); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil))[:16], n, nil
}

func copyFileAtomic(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
