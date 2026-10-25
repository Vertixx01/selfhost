package utils

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"torclient/source/models"
)

func newTestStore(t *testing.T) *JobStore {
	t.Helper()
	db, err := OpenDB(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewJobStore(db)
}

func eps(ids ...string) []models.Episode {
	out := make([]models.Episode, len(ids))
	for i, id := range ids {
		out[i] = models.Episode{EpID: id, MagnetURL: "magnet:?xt=urn:btih:" + id, Priority: i}
	}
	return out
}

func TestCreateJobIdempotentAndConflict(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	job := models.Job{ID: "j1", AnimeID: "a1", Title: "Show"}

	got, created, err := s.CreateJob(ctx, job, eps("S01E01", "S01E02"))
	if err != nil || !created {
		t.Fatalf("first create: created=%v err=%v", created, err)
	}
	if got.Status != models.JobPending || len(got.Episodes) != 2 || got.EpisodeCount != 2 {
		t.Fatalf("unexpected job: %+v", got)
	}

	// Same job_id again: no-op, even with a different payload.
	_, created, err = s.CreateJob(ctx, job, eps("S01E09"))
	if err != nil || created {
		t.Fatalf("re-POST: created=%v err=%v", created, err)
	}

	// Different job, same anime, overlapping ep still queued → conflict, nothing inserted.
	_, _, err = s.CreateJob(ctx, models.Job{ID: "j2", AnimeID: "a1"}, eps("S01E02", "S01E03"))
	var conflict *ConflictError
	if !errors.As(err, &conflict) || len(conflict.EpIDs) != 1 || conflict.EpIDs[0] != "S01E02" {
		t.Fatalf("want conflict on S01E02, got %v", err)
	}
	if _, err := s.GetJob(ctx, "j2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("j2 should not exist, got %v", err)
	}

	// Same ep_id under a different anime is fine.
	if _, _, err := s.CreateJob(ctx, models.Job{ID: "j3", AnimeID: "a2"}, eps("S01E02")); err != nil {
		t.Fatal(err)
	}
}

func TestPopNextAndRollup(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if _, _, err := s.CreateJob(ctx, models.Job{ID: "j1", AnimeID: "a1"}, eps("e1", "e2")); err != nil {
		t.Fatal(err)
	}

	ep, err := s.PopNext(ctx)
	if err != nil || ep.EpID != "e1" || ep.Status != models.EpResolving {
		t.Fatalf("PopNext: %+v %v", ep, err)
	}
	job, _ := s.GetJob(ctx, "j1")
	if job.Status != models.JobProcessing {
		t.Fatalf("job should be processing, got %s", job.Status)
	}

	finished, err := s.UpdateEpisodeStatus(ctx, "j1", "e1", models.EpDone, 100, "")
	if err != nil || finished {
		t.Fatalf("e1 done: finished=%v err=%v", finished, err)
	}
	if _, err := s.PopNext(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PopNext(ctx); !errors.Is(err, ErrQueueEmpty) {
		t.Fatalf("want ErrQueueEmpty, got %v", err)
	}

	finished, err = s.UpdateEpisodeStatus(ctx, "j1", "e2", models.EpFailed, 30, "no seeders")
	if err != nil || !finished {
		t.Fatalf("e2 failed: finished=%v err=%v", finished, err)
	}
	job, _ = s.GetJob(ctx, "j1")
	if job.Status != models.JobFailed {
		t.Fatalf("job should be failed, got %s", job.Status)
	}

	// The failed episode is no longer in flight, so it can be resubmitted.
	if _, _, err := s.CreateJob(ctx, models.Job{ID: "j2", AnimeID: "a1"}, eps("e2")); err != nil {
		t.Fatalf("resubmit failed episode: %v", err)
	}
}

func TestPopNextNoDoubleClaim(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	ids := make([]string, 20)
	for i := range ids {
		ids[i] = "e" + string(rune('a'+i))
	}
	if _, _, err := s.CreateJob(ctx, models.Job{ID: "j1", AnimeID: "a1"}, eps(ids...)); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	claimed := map[string]int{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				ep, err := s.PopNext(ctx)
				if errors.Is(err, ErrQueueEmpty) {
					return
				}
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				claimed[ep.EpID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(claimed) != len(ids) {
		t.Fatalf("claimed %d distinct episodes, want %d", len(claimed), len(ids))
	}
	for id, n := range claimed {
		if n != 1 {
			t.Errorf("%s claimed %d times", id, n)
		}
	}
}
