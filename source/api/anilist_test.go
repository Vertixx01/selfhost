package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"torclient/source/discovery"
	"torclient/source/utils"
)

// stubSource returns one exact 1080p release per requested episode, except
// episode 13, which only has a batch.
type stubSource struct{}

func (stubSource) Name() string { return "stub" }

func (stubSource) Search(_ context.Context, q discovery.Query, mode discovery.Mode) ([]discovery.Result, error) {
	if q.Episode == 13 {
		return nil, nil
	}
	if q.Episode == 14 { // only a season batch covers it
		hash := strings.Repeat("0", 38) + "bb"
		return []discovery.Result{{Title: "[Grp] Show 01-24 [1080p]", Hash: hash, Link: "magnet:?xt=urn:btih:" + hash,
			Accuracy: "high", Seeders: 80, HasSeeders: true, Date: time.Now(), Tier: 'B'}}, nil
	}
	ep := q.Episode
	if mode == discovery.ModeMovie {
		ep = 0
	}
	hash := strings.Repeat("0", 38) + string(rune('a'+ep%26)) + string(rune('a'+ep/26%26))
	return []discovery.Result{{
		Title: "[Grp] Show - 0" + string(rune('0'+ep%10)) + " (1080p)", Hash: hash,
		Link: "magnet:?xt=urn:btih:" + hash, Accuracy: "high", Seeders: 50, HasSeeders: true, Date: time.Now(), Tier: 'A',
	}}, nil
}

func newDiscoveryServer(t *testing.T) *httptest.Server {
	t.Helper()
	anilist := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Variables struct{ ID int } }
		json.NewDecoder(r.Body).Decode(&req)
		switch req.Variables.ID {
		case 1:
			io.WriteString(w, `{"data":{"Media":{"id":1,"episodes":24,"format":"TV","status":"FINISHED","title":{"romaji":"Show"},"synonyms":[],"relations":{"edges":[]}}}}`)
		case 2:
			io.WriteString(w, `{"data":{"Media":{"id":2,"episodes":1,"format":"MOVIE","status":"FINISHED","title":{"romaji":"Show Movie"},"synonyms":[],"relations":{"edges":[]}}}}`)
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"errors":[{"message":"Not Found."}],"data":{"Media":null}}`)
		}
	}))
	t.Cleanup(anilist.Close)

	db, err := utils.OpenDB(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := NewServer(db, utils.NewJobStore(db), slog.New(slog.NewTextHandler(io.Discard, nil)))
	al := discovery.NewAniList(anilist.Client())
	al.URL = anilist.URL
	s.Discovery = &discovery.Discoverer{AniList: al, Sources: []discovery.Source{stubSource{}}, Prefs: discovery.Prefs{Resolution: "1080"}}
	woken := 0
	s.OnJobCreated = func() { woken++ }
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestAnilistEndpoints(t *testing.T) {
	ts := newDiscoveryServer(t)

	code, body := do(t, ts, "GET", "/api/anilist/1/search?episode=5", "")
	if code != 200 || !strings.Contains(body, `"picked":{`) || !strings.Contains(body, `"[Grp] Show - 05 (1080p)"`) {
		t.Fatalf("search: %d %s", code, body)
	}
	if code, _ := do(t, ts, "GET", "/api/anilist/1/search", ""); code != 400 {
		t.Errorf("series search without episode: %d", code)
	}
	if code, _ := do(t, ts, "GET", "/api/anilist/777/search?episode=1", ""); code != 404 {
		t.Errorf("unknown anime: %d", code)
	}

	// Episodes 1, 2 and 13: 13 has no pickable release, so the job gets two.
	code, body = do(t, ts, "POST", "/api/anilist/1/episodes", `{"job_id":"al-1","episodes":[2,1,13],"callback_url":"https://cron.test/hook"}`)
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	var resp struct {
		Job struct {
			AnimeID  string `json:"anime_id"`
			Title    string
			Episodes []struct {
				EpID      string `json:"ep_id"`
				MagnetURL string `json:"magnet_url"`
				Provider  string
			}
		}
		Discovery []struct {
			Episode int
			EpID    string `json:"ep_id"`
			Error   string
		}
	}
	json.Unmarshal([]byte(body), &resp)
	if resp.Job.AnimeID != "1" || resp.Job.Title != "Show" || len(resp.Job.Episodes) != 2 {
		t.Fatalf("job %+v", resp.Job)
	}
	if resp.Job.Episodes[0].EpID != "ep1" || resp.Job.Episodes[1].EpID != "ep2" || resp.Job.Episodes[0].Provider != "stub" {
		t.Errorf("episodes %+v", resp.Job.Episodes)
	}
	if len(resp.Discovery) != 3 || resp.Discovery[2].Episode != 13 || resp.Discovery[2].Error == "" {
		t.Errorf("discovery report %+v", resp.Discovery)
	}

	// Idempotent re-POST: same job, no new search.
	if code, _ := do(t, ts, "POST", "/api/anilist/1/episodes", `{"job_id":"al-1","episodes":[3]}`); code != 200 {
		t.Errorf("re-POST: %d", code)
	}
	// Nothing found at all: 404, no job.
	if code, body := do(t, ts, "POST", "/api/anilist/1/episodes", `{"job_id":"al-2","episodes":[13]}`); code != 404 || !strings.Contains(body, "no suitable release") {
		t.Errorf("none found: %d %s", code, body)
	}
	if code, _ := do(t, ts, "GET", "/job/al-2", ""); code != 404 {
		t.Error("job created although nothing was found")
	}
	// Movie: episodes omitted; ep_id "movie"; custom anime_id.
	code, body = do(t, ts, "POST", "/api/anilist/2/episodes", `{"job_id":"al-3","anime_id":"show-movie"}`)
	if code != 201 || !strings.Contains(body, `"ep_id":"movie"`) || !strings.Contains(body, `"anime_id":"show-movie"`) {
		t.Errorf("movie: %d %s", code, body)
	}
	for name, body := range map[string]string{
		"no episodes":  `{"job_id":"x1"}`,
		"duplicate":    `{"job_id":"x2","episodes":[1,1]}`,
		"bad job id":   `{"job_id":"../x","episodes":[1]}`,
		"bad anime id": `{"job_id":"x3","anime_id":"a/b","episodes":[1]}`,
	} {
		if code, resp := do(t, ts, "POST", "/api/anilist/1/episodes", body); code != 400 {
			t.Errorf("%s: want 400, got %d %s", name, code, resp)
		}
	}
	if code, _ := do(t, ts, "POST", "/api/anilist/2/episodes", `{"job_id":"x4","episodes":[3]}`); code != 400 {
		t.Error("movie with episode 3 accepted")
	}
}

func TestAnilistBatchEpisodes(t *testing.T) {
	ts := newDiscoveryServer(t)
	type job struct {
		Job struct {
			Episodes []struct {
				EpID         string `json:"ep_id"`
				FileEpisodes []int  `json:"file_episodes"`
			}
		}
	}
	// Auto-picked batch (episode 14) and a chosen batch (episode 7) record
	// which file to take; a single-episode release (episode 2) doesn't.
	code, body := do(t, ts, "POST", "/api/anilist/1/episodes", `{"job_id":"b-1","episodes":[2,7,14],
		"picks":{"7":{"magnet":"magnet:?xt=urn:btih:`+strings.Repeat("0", 38)+`cc","title":"[Grp] Show 01-24","source":"nyaa","batch":true}}}`)
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	var resp job
	json.Unmarshal([]byte(body), &resp)
	got := map[string][]int{}
	for _, e := range resp.Job.Episodes {
		got[e.EpID] = e.FileEpisodes
	}
	if len(got) != 3 || got["ep2"] != nil || !slices.Equal(got["ep7"], []int{7}) || !slices.Equal(got["ep14"], []int{14}) {
		t.Errorf("file episodes %v", got)
	}
	// Stored, not just echoed.
	code, body = do(t, ts, "GET", "/job/b-1", "")
	if code != 200 || !strings.Contains(body, `"file_episodes":[14]`) {
		t.Errorf("stored job: %d %s", code, body)
	}
}
