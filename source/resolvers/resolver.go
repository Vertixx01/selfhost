// Package resolvers turns an episode's magnet into the one file to download
// (PROJECT.MD Stage 1).
package resolvers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"torclient/source/discovery"
	"torclient/source/torrent"
)

// ErrNoPlayableFile means the torrent has no video file that survives the
// size and sample filters.
var ErrNoPlayableFile = errors.New("torrent has no playable video file")

// ErrEpisodeNotInTorrent: a multi-file torrent has no file named for the
// wanted episode.
var ErrEpisodeNotInTorrent = errors.New("episode not found in torrent")

const DefaultMinFileSize = 50 << 20 // smaller files are samples / extras

// Containers we accept. MKV is preferred; the others are taken only when the
// torrent has no MKV at all (ffprobe decides later whether they're usable).
var videoExts = map[string]bool{".mkv": true, ".mp4": true, ".m4v": true, ".avi": true}

type Options struct {
	Attempts       int           // metadata fetch attempts (default 3)
	AttemptTimeout time.Duration // per attempt (default 60s)
	Backoff        time.Duration // pause between attempts (default 10s; negative = none)
	MinFileSize    int64         // default DefaultMinFileSize
}

type Resolver struct {
	engine *torrent.Engine
	opts   Options
	log    *slog.Logger
}

func New(engine *torrent.Engine, opts Options, log *slog.Logger) *Resolver {
	if opts.Attempts <= 0 {
		opts.Attempts = 3
	}
	if opts.AttemptTimeout <= 0 {
		opts.AttemptTimeout = 60 * time.Second
	}
	if opts.Backoff < 0 {
		opts.Backoff = 0
	} else if opts.Backoff == 0 {
		opts.Backoff = 10 * time.Second
	}
	if opts.MinFileSize <= 0 {
		opts.MinFileSize = DefaultMinFileSize
	}
	return &Resolver{engine: engine, opts: opts, log: log}
}

type Result struct {
	Handle   *torrent.Handle // still active; pass to torrent.Download (which drops it)
	Infohash string
	File     torrent.File   // the main episode file
	Skipped  []torrent.File // other files, not downloaded
}

// Resolve adds the magnet with storage under rawDir, waits for metadata
// (retrying with backoff), and picks the main episode file. On error the
// torrent is already dropped.
func (r *Resolver) Resolve(ctx context.Context, magnet, rawDir string) (*Result, error) {
	return r.ResolveEpisode(ctx, magnet, rawDir, nil)
}

// ResolveEpisode is Resolve for a torrent that may hold several episodes:
// fileEps are the numbers the wanted episode's file name may carry (see
// SelectEpisodeFile).
func (r *Resolver) ResolveEpisode(ctx context.Context, magnet, rawDir string, fileEps []int) (*Result, error) {
	var h *torrent.Handle
	var lastErr error
	for attempt := 1; attempt <= r.opts.Attempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(r.opts.Backoff):
			}
		}
		var err error
		h, err = r.engine.AddMagnet(magnet, rawDir)
		if err != nil {
			return nil, err // bad magnet or ErrBusy: retrying won't help
		}
		actx, cancel := context.WithTimeout(ctx, r.opts.AttemptTimeout)
		err = h.WaitInfo(actx)
		cancel()
		if err == nil {
			break
		}
		h.Drop() // re-adding re-announces to DHT/trackers
		h = nil
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = err
		r.log.Warn("metadata fetch timed out", "attempt", attempt, "of", r.opts.Attempts)
	}
	if h == nil {
		return nil, fmt.Errorf("no metadata after %d attempts: %w", r.opts.Attempts, lastErr)
	}

	main, skipped, err := SelectEpisodeFile(h.Files(), r.opts.MinFileSize, fileEps)
	if err != nil {
		h.Drop()
		return nil, fmt.Errorf("%s: %w", h.Name(), err)
	}
	for _, f := range skipped {
		r.log.Info("skipping extra file", "infohash", h.Infohash(), "path", f.Path, "bytes", f.Length)
	}
	return &Result{Handle: h, Infohash: h.Infohash(), File: main, Skipped: skipped}, nil
}

// SelectMainFile picks the largest MKV that isn't a sample (falling back to
// other video containers when there is no MKV). Everything else is returned
// as skipped.
func SelectMainFile(files []torrent.File, minSize int64) (torrent.File, []torrent.File, error) {
	best, bestMKV := -1, false
	for i, f := range files {
		ext := strings.ToLower(path.Ext(f.Path))
		if !videoExts[ext] || f.Length < minSize || isSample(f.Path) {
			continue
		}
		mkv := ext == ".mkv"
		if best < 0 || (mkv && !bestMKV) || (mkv == bestMKV && f.Length > files[best].Length) {
			best, bestMKV = i, mkv
		}
	}
	if best < 0 {
		return torrent.File{}, nil, ErrNoPlayableFile
	}
	skipped := make([]torrent.File, 0, len(files)-1)
	for i, f := range files {
		if i != best {
			skipped = append(skipped, f)
		}
	}
	return files[best], skipped, nil
}

// SelectEpisodeFile picks the file for one episode. With no episode numbers,
// or a torrent with a single playable video, it is SelectMainFile. Otherwise
// (a batch) it takes the playable file whose name carries the first of eps
// that any file carries: season numbering is listed before absolute.
func SelectEpisodeFile(files []torrent.File, minSize int64, eps []int) (torrent.File, []torrent.File, error) {
	var playable []torrent.File
	for _, f := range files {
		if videoExts[strings.ToLower(path.Ext(f.Path))] && f.Length >= minSize && !isSample(f.Path) {
			playable = append(playable, f)
		}
	}
	if len(eps) == 0 || len(playable) <= 1 {
		return SelectMainFile(files, minSize)
	}
	for _, ep := range eps {
		var hits []torrent.File
		for _, f := range playable {
			if fileHasEpisode(f.Path, ep) {
				hits = append(hits, f)
			}
		}
		if len(hits) == 0 {
			continue
		}
		main, _, err := SelectMainFile(hits, minSize)
		if err != nil {
			return torrent.File{}, nil, err
		}
		skipped := make([]torrent.File, 0, len(files)-1)
		for _, f := range files {
			if f.Index != main.Index {
				skipped = append(skipped, f)
			}
		}
		return main, skipped, nil
	}
	names := make([]string, 0, 3)
	for _, f := range playable[:min(3, len(playable))] {
		names = append(names, path.Base(f.Path))
	}
	return torrent.File{}, nil, fmt.Errorf("%w: no file for episode %v among %d videos (e.g. %s)",
		ErrEpisodeNotInTorrent, eps, len(playable), strings.Join(names, ", "))
}

// trailingGroupsRe strips "[1080p][ABCD1234]"-style tags from a file name's end.
var trailingGroupsRe = regexp.MustCompile(`(\s*[\[(][^\])]*[\])])+\s*$`)

// fileHasEpisode: the base name (without extension) names episode ep, either
// as a release would ("- 05", "S01E05", "[05]") or as a bare trailing number
// ("Show 05.mkv", "05.mkv").
func fileHasEpisode(p string, ep int) bool {
	base := path.Base(p)
	base = strings.TrimSuffix(base, path.Ext(base))
	if discovery.NameHasEpisode(base, ep) {
		return true
	}
	bare := trailingGroupsRe.ReplaceAllString(base, "")
	re := regexp.MustCompile(`(?i)(^|[\s._-])0*` + strconv.Itoa(ep) + `(v\d)?$`)
	return re.MatchString(bare)
}

// isSample matches "sample" anywhere in the filename, or a Sample/ directory.
func isSample(p string) bool {
	parts := strings.Split(strings.ToLower(p), "/")
	if strings.Contains(parts[len(parts)-1], "sample") {
		return true
	}
	for _, dir := range parts[:len(parts)-1] {
		if dir == "sample" || dir == "samples" {
			return true
		}
	}
	return false
}
