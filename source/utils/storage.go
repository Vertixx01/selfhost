package utils

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DeleteEpisodeOutput removes a packaged episode — output dir, key,
// fingerprints — and marks its job rows "output deleted" (retryable).
// Refuses with ErrBusy while the episode is queued or processing.
func DeleteEpisodeOutput(ctx context.Context, store *JobStore, layout Layout, animeID, epID string) error {
	active, err := store.EpisodeActive(ctx, animeID, epID)
	if err != nil {
		return err
	}
	if active {
		return fmt.Errorf("%w: episode is being processed", ErrBusy)
	}
	out := layout.OutputDir(animeID, epID)
	if _, err := os.Stat(out); errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err := os.RemoveAll(out); err != nil {
		return err
	}
	os.Remove(layout.KeyFile(animeID, epID))
	os.Remove(filepath.Dir(out)) // the anime dir, once empty
	return store.MarkOutputDeleted(ctx, animeID, epID)
}

// StorageGuard enforces MAX_STORAGE_GB over everything torClient keeps on
// disk (work dirs with downloads in progress + the packaged library).
type StorageGuard struct {
	Limit      int64 // bytes; 0 = no limit
	AutoDelete bool  // make room by deleting the oldest packaged episodes
	Dirs       []string
	Store      *JobStore
	Layout     Layout

	mu     sync.Mutex
	used   int64
	at     time.Time
	review sync.Mutex // one reservation/eviction at a time
}

// ErrStorageFull: the limit is reached and nothing could be freed.
var ErrStorageFull = errors.New("storage limit reached")

// Used returns bytes in use, cached for 30s (walking a large library is slow).
func (g *StorageGuard) Used() int64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Since(g.at) < 30*time.Second && !g.at.IsZero() {
		return g.used
	}
	g.used, g.at = dirsSize(g.Dirs), time.Now()
	return g.used
}

func (g *StorageGuard) invalidate() {
	g.mu.Lock()
	g.at = time.Time{}
	g.mu.Unlock()
}

// Full reports whether new submissions should be refused.
func (g *StorageGuard) Full() bool { return g != nil && g.Limit > 0 && g.Used() >= g.Limit }

// Reserve checks that need more bytes fit under the limit before a download
// starts. With AutoDelete it first deletes the oldest packaged episodes
// (never one of keep's anime/ep pairs, never one in progress).
func (g *StorageGuard) Reserve(ctx context.Context, need int64, keepAnime string) error {
	if g == nil || g.Limit <= 0 {
		return nil
	}
	g.review.Lock()
	defer g.review.Unlock()
	g.invalidate()
	used := g.Used()
	if used+need <= g.Limit {
		return nil
	}
	if !g.AutoDelete {
		return fmt.Errorf("%w: %.1f of %.1f GB used, episode needs %.1f GB more (delete episodes or raise MAX_STORAGE_GB)",
			ErrStorageFull, gb(used), gb(g.Limit), gb(need))
	}
	for _, ep := range g.oldestEpisodes() {
		if used+need <= g.Limit {
			break
		}
		if ep.anime == keepAnime {
			continue // don't delete the series being downloaded right now
		}
		err := DeleteEpisodeOutput(ctx, g.Store, g.Layout, ep.anime, ep.ep)
		if errors.Is(err, ErrBusy) || errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		used -= ep.size
	}
	g.invalidate()
	if used+need > g.Limit {
		return fmt.Errorf("%w: %.1f of %.1f GB used even after deleting old episodes; episode needs %.1f GB",
			ErrStorageFull, gb(used), gb(g.Limit), gb(need))
	}
	return nil
}

type packaged struct {
	anime, ep string
	mod       time.Time
	size      int64
}

// oldestEpisodes lists packaged episodes, oldest master.m3u8 first.
func (g *StorageGuard) oldestEpisodes() []packaged {
	var out []packaged
	animes, _ := os.ReadDir(g.Layout.OutputRoot)
	for _, a := range animes {
		if !a.IsDir() || strings.HasPrefix(a.Name(), ".") {
			continue
		}
		eps, _ := os.ReadDir(filepath.Join(g.Layout.OutputRoot, a.Name()))
		for _, e := range eps {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			st, err := os.Stat(filepath.Join(g.Layout.OutputRoot, a.Name(), e.Name(), "master.m3u8"))
			if err != nil {
				continue
			}
			out = append(out, packaged{a.Name(), e.Name(), st.ModTime(),
				dirsSize([]string{filepath.Join(g.Layout.OutputRoot, a.Name(), e.Name())})})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].mod.Before(out[j].mod) })
	return out
}

func dirsSize(dirs []string) int64 {
	var n int64
	for _, d := range dirs {
		filepath.WalkDir(d, func(_ string, e fs.DirEntry, err error) error {
			if err == nil && !e.IsDir() {
				if info, err := e.Info(); err == nil {
					n += info.Size()
				}
			}
			return nil
		})
	}
	return n
}

func gb(b int64) float64 { return float64(b) / (1 << 30) }
