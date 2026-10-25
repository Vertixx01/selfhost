package pipeline

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"torclient/source/models"
)

// packageEpisode is Stage 8. Everything is assembled in a hidden staging dir
// next to the final one and renamed into place last, so output/{anime}/{ep}/
// either doesn't exist or is complete — a crash never leaves a half-copied
// episode where the frontend (or lsyncd) can see it.
func (p *Pipeline) packageEpisode(ctx context.Context, job *models.Job, ep models.Episode, ps paths, log *slog.Logger) error {
	l := p.cfg.Layout
	staging := filepath.Join(filepath.Dir(ps.Output), "."+ep.EpID+".staging")
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	defer os.RemoveAll(staging) // no-op after the final rename

	// Renditions, audio, subs (.vtt/.m3u8/.ass) and master.m3u8 from the transcode.
	if err := copyTree(ps.WorkOut, staging); err != nil {
		return fmt.Errorf("copy transcode output: %w", err)
	}

	// Fonts: real copies from the cache under this episode's own filenames
	// (no symlinks — lsyncd replicates the output tree as-is).
	fonts, err := p.fonts.EpisodeFonts(ctx, ep.JobID, ep.EpID)
	if err != nil {
		return err
	}
	for _, f := range fonts {
		if err := copyFile(f.CachePath, filepath.Join(staging, "subs", "fonts", f.Filename)); err != nil {
			return fmt.Errorf("copy font %s: %w", f.Filename, err)
		}
	}

	for _, name := range []string{"timestamps.json", "media_info.json"} {
		if err := copyFile(filepath.Join(ps.Meta, name), filepath.Join(staging, "meta", name)); err != nil {
			return err
		}
	}
	jobMeta, err := json.MarshalIndent(map[string]any{
		"job_id": job.ID, "anime_id": job.AnimeID, "title": job.Title,
		"episode": map[string]any{"ep_id": ep.EpID, "title": ep.Title, "magnet_url": ep.MagnetURL,
			"provider": ep.Provider, "priority": ep.Priority, "needs_poster": ep.NeedsPoster},
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(staging, "meta", "job.json"), jobMeta, 0o644); err != nil {
		return err
	}

	if err := verifyPackage(staging); err != nil {
		return err
	}

	// The key goes to KEY_DIR before the output appears, so a player never
	// finds segments whose key isn't served yet.
	if p.cfg.Encrypt {
		if err := copyFile(filepath.Join(ps.Meta, "enc.key"), l.KeyFile(job.AnimeID, ep.EpID)); err != nil {
			return fmt.Errorf("publish key: %w", err)
		}
	}

	// §4 orphan guard: never overwrite a valid episode; move a partial one aside.
	if _, err := os.Stat(ps.Output); err == nil {
		if validMaster(ps.Output) {
			log.Warn("valid output appeared while processing; keeping it", "path", ps.Output)
			return nil
		}
		trash := filepath.Join(l.OutputRoot, ".trash", job.AnimeID, fmt.Sprintf("%s-%s", ep.EpID, time.Now().UTC().Format("20060102T150405Z")))
		if err := os.MkdirAll(filepath.Dir(trash), 0o755); err != nil {
			return err
		}
		if err := os.Rename(ps.Output, trash); err != nil {
			return fmt.Errorf("move stale output aside: %w", err)
		}
		log.Warn("moved partial output to trash", "trash", trash)
	}
	if err := os.Rename(staging, ps.Output); err != nil {
		return fmt.Errorf("publish output: %w", err)
	}

	// The raw MKV isn't needed any more; meta/ stays until the cleanup TTL.
	if err := os.RemoveAll(ps.RawDir); err != nil {
		log.Warn("remove raw dir", "err", err)
	}
	return nil
}

// verifyPackage checks that master.m3u8 parses and every playlist it names
// exists, and that each of those names only existing segments.
func verifyPackage(dir string) error {
	uris, err := playlistURIs(filepath.Join(dir, "master.m3u8"))
	if err != nil {
		return err
	}
	if len(uris) == 0 {
		return fmt.Errorf("master.m3u8 lists no renditions")
	}
	for _, u := range uris {
		pl := filepath.Join(dir, filepath.FromSlash(u))
		segs, err := playlistURIs(pl)
		if err != nil {
			return fmt.Errorf("rendition %s: %w", u, err)
		}
		for _, s := range segs {
			if st, err := os.Stat(filepath.Join(filepath.Dir(pl), filepath.FromSlash(s))); err != nil || st.Size() == 0 {
				return fmt.Errorf("rendition %s: segment %s missing", u, s)
			}
		}
	}
	return nil
}

// playlistURIs returns the URIs in an m3u8: plain lines plus URI="..." attributes
// (except key URIs, which point at the key server).
func playlistURIs(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var uris []string
	sc := bufio.NewScanner(f)
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if first {
			if line != "#EXTM3U" {
				return nil, fmt.Errorf("%s: not an m3u8", filepath.Base(path))
			}
			first = false
			continue
		}
		switch {
		case line == "":
		case strings.HasPrefix(line, "#EXT-X-MEDIA:"):
			if _, rest, ok := strings.Cut(line, `URI="`); ok {
				u, _, _ := strings.Cut(rest, `"`)
				uris = append(uris, u)
			}
		case !strings.HasPrefix(line, "#"):
			uris = append(uris, line)
		}
	}
	return uris, sc.Err()
}

// validMaster: output dir has a master playlist that names renditions. The
// packager renames a complete tree into place, so this means "done".
func validMaster(dir string) bool {
	uris, err := playlistURIs(filepath.Join(dir, "master.m3u8"))
	return err == nil && len(uris) > 0
}

func copyTree(src, dst string) error {
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
		return copyFile(path, target)
	})
}

// copyFile copies via a temp file + rename, so dst is never half-written.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
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
