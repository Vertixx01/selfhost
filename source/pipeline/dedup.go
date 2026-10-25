package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"torclient/source/models"
)

// errReused ends the stage loop early: the episode's output was linked from
// an identical, already packaged source.
var errReused = errors.New("output reused from an identical source")

// reuseOutput is infohash dedup (PROJECT.MD Phase 7): if the same file of the
// same torrent was already packaged under another anime/episode ID, build
// this episode's output from it — segments hard-linked (copied where links
// aren't possible), playlists rewritten to this episode's key URI, the key
// copied — and report true. Nothing is downloaded or encoded.
func (p *Pipeline) reuseOutput(ctx context.Context, job *models.Job, ep models.Episode, ps paths, infohash, filePath string, log *slog.Logger) (bool, error) {
	srcs, err := p.store.PackagedFrom(ctx, infohash, filePath, job.AnimeID, ep.EpID)
	if err != nil {
		return false, err
	}
	l := p.cfg.Layout
	for _, s := range srcs {
		src := l.OutputDir(s[0], s[1])
		if !validMaster(src) {
			continue // deleted since, or never published
		}
		if p.cfg.Encrypt {
			if _, err := os.Stat(l.KeyFile(s[0], s[1])); err != nil {
				continue
			}
		}
		staging := filepath.Join(filepath.Dir(ps.Output), "."+ep.EpID+".staging")
		if err := os.RemoveAll(staging); err != nil {
			return false, err
		}
		oldURI := fmt.Sprintf("%s/%s/%s.key", p.cfg.KeyURIBase, s[0], s[1])
		newURI := fmt.Sprintf("%s/%s/%s.key", p.cfg.KeyURIBase, job.AnimeID, ep.EpID)
		if err := linkTree(src, staging, func(rel string, b []byte) []byte {
			if strings.HasSuffix(rel, ".m3u8") {
				return []byte(strings.ReplaceAll(string(b), `URI="`+oldURI+`"`, `URI="`+newURI+`"`))
			}
			return nil
		}); err != nil {
			os.RemoveAll(staging)
			return false, fmt.Errorf("reuse %s/%s: %w", s[0], s[1], err)
		}
		if err := writeJobMeta(filepath.Join(staging, "meta", "job.json"), job, ep); err != nil {
			os.RemoveAll(staging)
			return false, err
		}
		if p.cfg.Encrypt {
			if err := copyFile(l.KeyFile(s[0], s[1]), l.KeyFile(job.AnimeID, ep.EpID)); err != nil {
				os.RemoveAll(staging)
				return false, fmt.Errorf("copy key: %w", err)
			}
		}
		if validMaster(ps.Output) {
			os.RemoveAll(staging)
			return true, nil
		}
		os.RemoveAll(ps.Output) // a partial leftover; the staging copy is complete
		if err := os.Rename(staging, ps.Output); err != nil {
			os.RemoveAll(staging)
			return false, fmt.Errorf("publish output: %w", err)
		}
		os.RemoveAll(ps.RawDir)
		log.Info("same torrent file already packaged; reused its output", "from", s[0]+"/"+s[1], "infohash", infohash)
		return true, nil
	}
	return false, nil
}

// linkTree recreates src under dst with hard links. For each file, rewrite
// may return new contents (written as a fresh file); nil means link as-is.
func linkTree(src, dst string, rewrite func(rel string, b []byte) []byte) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if strings.HasSuffix(rel, ".m3u8") {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if nb := rewrite(filepath.ToSlash(rel), b); nb != nil {
				return os.WriteFile(target, nb, 0o644)
			}
		}
		if err := os.Link(path, target); err == nil {
			return nil
		}
		return copyFile(path, target) // another filesystem, or links unsupported
	})
}
