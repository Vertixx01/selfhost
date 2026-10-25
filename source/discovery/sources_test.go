package discovery

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Fake versions of every remote API, shaped like the real responses seen in
// the live run.

const nyaaRSS = `<?xml version="1.0" encoding="utf-8"?><rss xmlns:nyaa="https://nyaa.si/xmlns/nyaa" version="2.0"><channel>
<item><title>[SubsPlease] Sousou no Frieren - 05 (1080p) [8E3F8FA5].mkv</title><pubDate>Fri, 06 Oct 2023 16:00:00 -0000</pubDate>
<nyaa:seeders>384</nyaa:seeders><nyaa:leechers>3</nyaa:leechers><nyaa:downloads>9000</nyaa:downloads>
<nyaa:infoHash>E971D09C2578AEFD982B7D4059E8BDD1FC3D2CF4</nyaa:infoHash><nyaa:size>1.4 GiB</nyaa:size><nyaa:trusted>Yes</nyaa:trusted><nyaa:remake>No</nyaa:remake></item>
<item><title>[ToonsHub] Sousou no Frieren - 05 1080p &amp; more</title><pubDate>Fri, 06 Oct 2023 17:00:00 -0000</pubDate>
<nyaa:seeders>8</nyaa:seeders><nyaa:infoHash>aa00000000000000000000000000000000000001</nyaa:infoHash><nyaa:size>700 MiB</nyaa:size></item>
<item><title>[SubsPlease] Sousou no Frieren - 06 (1080p)</title><pubDate>Fri, 13 Oct 2023 16:00:00 -0000</pubDate>
<nyaa:seeders>300</nyaa:seeders><nyaa:infoHash>bb00000000000000000000000000000000000002</nyaa:infoHash></item>
</channel></rss>`

type fakeSites struct {
	srv        *httptest.Server
	nyaaHits   atomic.Int32
	nyaa429    atomic.Int32 // remaining 429s to send
	lastNyaaQS atomic.Value
}

func newFakeSites(t *testing.T) *fakeSites {
	f := &fakeSites{}
	mux := http.NewServeMux()
	mux.HandleFunc("/anilist", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string
			Variables struct{ ID int }
		}
		json.NewDecoder(r.Body).Decode(&req)
		switch {
		case req.Variables.ID == 999:
			w.WriteHeader(404)
			io.WriteString(w, `{"errors":[{"message":"Not Found."}],"data":{"Media":null}}`)
		case strings.Contains(req.Query, "synonyms"):
			io.WriteString(w, `{"data":{"Media":{"id":154587,"episodes":28,"format":"TV","status":"FINISHED",
				"title":{"romaji":"Sousou no Frieren","english":"Frieren: Beyond Journey's End","native":"葬送のフリーレン"},
				"synonyms":["Frieren at the Funeral"],"startDate":{"year":2023},"nextAiringEpisode":null}}}`)
		default: // chain queries: no prequels
			io.WriteString(w, `{"data":{"Media":{"episodes":28,"status":"FINISHED","startDate":{"year":2023},"nextAiringEpisode":null,"relations":{"edges":[]}}}}`)
		}
	})
	mux.HandleFunc("/mapping", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"154587": 17617, "21": 69}`)
	})
	mux.HandleFunc("/tosho", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("aid") != "17617" {
			io.WriteString(w, `[]`)
			return
		}
		io.WriteString(w, `[{"title":"[SubsPlease] Sousou no Frieren - 05 (1080p) [8E3F8FA5].mkv","info_hash":"e971d09c2578aefd982b7d4059e8bdd1fc3d2cf4",
			"magnet_uri":"magnet:?xt=urn:btih:e971d09c2578aefd982b7d4059e8bdd1fc3d2cf4","seeders":380,"leechers":2,"total_size":1500000000,"timestamp":1696608000},
			{"title":"[EMBER] Frieren: Beyond Journey's End S02E05 [1080p] (Season 2)","info_hash":"cc00000000000000000000000000000000000003","seeders":84,"timestamp":1775503423}]`)
	})
	mux.HandleFunc("/seadex", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"items":[{"expand":{"trs":[{"infoHash":"DD00000000000000000000000000000000000004","releaseGroup":"PMR","dualAudio":true,"isBest":true,
			"created":"2024-03-01 10:00:00.000Z","files":[{"name":"Frieren - S01E01.mkv","length":4000000000},{"name":"Frieren - S01E02.mkv","length":4000000000}]}]}}]}`)
	})
	mux.HandleFunc("/subsplease/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("f") == "search" && strings.Contains(r.URL.Query().Get("s"), "frieren") {
			io.WriteString(w, `{"Sousou no Frieren - 05":{"episode":"05","release_date":"Fri, 06 Oct 2023 16:00:00 +0000","downloads":[
				{"res":"1080","magnet":"magnet:?xt=urn:btih:e971d09c2578aefd982b7d4059e8bdd1fc3d2cf4&xl=1500000000"},
				{"res":"720","magnet":"magnet:?xt=urn:btih:ee00000000000000000000000000000000000005"}]},
				"Sousou no Frieren (01-28)":{"episode":"01-28","release_date":"Fri, 22 Mar 2024 16:00:00 +0000","downloads":[{"res":"1080","magnet":"magnet:?xt=urn:btih:ff00000000000000000000000000000000000006"}]}}`)
			return
		}
		io.WriteString(w, `[]`)
	})
	mux.HandleFunc("/nyaa/", func(w http.ResponseWriter, r *http.Request) {
		f.nyaaHits.Add(1)
		f.lastNyaaQS.Store(r.URL.RawQuery)
		if f.nyaa429.Load() > 0 {
			f.nyaa429.Add(-1)
			w.WriteHeader(429)
			return
		}
		if r.Header.Get("User-Agent") == "" || !strings.Contains(r.Header.Get("User-Agent"), "Mozilla") {
			w.WriteHeader(403) // DDoS-Guard stand-in
			return
		}
		io.WriteString(w, nyaaRSS)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSites) discoverer() *Discoverer {
	c := f.srv.Client()
	al := NewAniList(c)
	al.URL = f.srv.URL + "/anilist"
	tosho := NewAnimeTosho(c)
	tosho.BaseURLs = []string{"http://unreachable.invalid/json", f.srv.URL + "/tosho"} // fallback host
	tosho.MappingURL = f.srv.URL + "/mapping"
	sea := NewSeaDex(c)
	sea.BaseURL = f.srv.URL + "/seadex"
	sp := NewSubsPlease(c)
	sp.BaseURL = f.srv.URL + "/subsplease/"
	var srcs []Source
	srcs = append(srcs, tosho, sea, sp)
	for _, n := range []*Nyaa{NewNyaa(c), NewYameii(c), NewToonsHub(c)} {
		n.BaseURL = f.srv.URL + "/nyaa"
		n.RetryDelays = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
		srcs = append(srcs, n)
	}
	return &Discoverer{AniList: al, Sources: srcs, Prefs: Prefs{Resolution: "1080"}}
}

func TestDiscoverWithFakeSites(t *testing.T) {
	f := newFakeSites(t)
	d := f.discoverer()
	ctx := context.Background()

	show, err := d.AniList.Show(ctx, 154587)
	if err != nil || show.Episodes != 28 || show.Titles[0] != "Sousou no Frieren" || show.Titles[len(show.Titles)-1] != "葬送のフリーレン" {
		t.Fatalf("show %+v %v", show, err)
	}
	if _, err := d.AniList.Show(ctx, 999); err != ErrNotFound {
		t.Fatalf("unknown id: %v", err)
	}

	res := d.Search(ctx, show, 5, false)
	if len(res.Errors) != 0 {
		t.Fatalf("source errors: %v", res.Errors)
	}
	if res.Picked == nil || res.Picked.Hash != "e971d09c2578aefd982b7d4059e8bdd1fc3d2cf4" {
		t.Fatalf("picked %+v", res.Picked)
	}
	if res.Picked.Batch {
		t.Errorf("batch picked over single-episode releases: %s", res.Picked.Title)
	}
	if !slices.Equal(res.FileEpisodes, []int{res.Episode}) && (len(res.FileEpisodes) == 0 || res.FileEpisodes[0] != res.Episode) {
		t.Errorf("file episodes %v for episode %d", res.FileEpisodes, res.Episode)
	}
	// One torrent, seen by three sources, merged.
	for _, s := range []string{"animetosho", "subsplease", "nyaa"} {
		if !contains(res.Picked.Sources, s) {
			t.Errorf("picked sources %v missing %s", res.Picked.Sources, s)
		}
	}
	if res.Picked.Seeders != 384 {
		t.Errorf("seeders not merged to max: %d", res.Picked.Seeders)
	}
	for _, c := range res.Candidates {
		switch c.Hash {
		case "cc00000000000000000000000000000000000003", "bb00000000000000000000000000000000000002":
			t.Errorf("wrong season/episode kept: %s", c.Title)
		case "dd00000000000000000000000000000000000004", "ff00000000000000000000000000000000000006":
			if !c.Batch {
				t.Errorf("batch not flagged: %+v", c)
			}
		}
	}
	// ToonsHub only keeps [ToonsHub] titles and puts the prefix in the query.
	for _, c := range res.Candidates {
		if contains(c.Sources, "toonshub") && !strings.Contains(c.Title, "[ToonsHub]") {
			t.Errorf("toonshub kept %q", c.Title)
		}
	}
}

func TestNyaaRetriesRateLimit(t *testing.T) {
	f := newFakeSites(t)
	f.nyaa429.Store(2)
	n := NewNyaa(f.srv.Client())
	n.BaseURL = f.srv.URL + "/nyaa"
	n.RetryDelays = []time.Duration{5 * time.Millisecond, 5 * time.Millisecond}
	items, err := n.rss(context.Background(), "frieren")
	if err != nil || len(items) != 3 || f.nyaaHits.Load() != 3 {
		t.Fatalf("items %d err %v hits %d", len(items), err, f.nyaaHits.Load())
	}
	f.nyaa429.Store(10)
	if _, err := n.rss(context.Background(), "frieren"); err != errRateLimited {
		t.Fatalf("want errRateLimited, got %v", err)
	}
	y := NewYameii(f.srv.Client())
	y.BaseURL = f.srv.URL + "/nyaa"
	f.nyaa429.Store(0)
	y.rss(context.Background(), "frieren")
	if qs, _ := f.lastNyaaQS.Load().(string); !strings.Contains(qs, "u=Yameii") {
		t.Errorf("yameii query %q", qs)
	}
}

func TestNyaaItemParsing(t *testing.T) {
	n := NewNyaa(nil)
	items := pickItems(nyaaRSS)
	r, ok := n.itemToResult(items[0], nil, false)
	if !ok || r.Seeders != 384 || r.Hash != "e971d09c2578aefd982b7d4059e8bdd1fc3d2cf4" || !r.trusted || r.Size != 1503238554 {
		t.Fatalf("%+v", r)
	}
	if r.Date.Year() != 2023 || !strings.HasPrefix(r.Link, "magnet:?xt=urn:btih:e971") {
		t.Errorf("date/link %v %s", r.Date, r.Link)
	}
	r, _ = n.itemToResult(items[1], nil, false)
	if r.Title != "[ToonsHub] Sousou no Frieren - 05 1080p & more" {
		t.Errorf("entities not decoded: %q", r.Title)
	}
	if _, ok := n.itemToResult(items[0], []string{"subsplease"}, false); ok {
		t.Error("exclusion ignored")
	}
}

func TestSelectSources(t *testing.T) {
	all := AllSources(http.DefaultClient)
	got, err := SelectSources(all, []string{"nyaa", " SeaDex "})
	if err != nil || len(got) != 2 || got[1].Name() != "seadex" {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := SelectSources(all, []string{"piratebay"}); err == nil {
		t.Error("unknown source accepted")
	}
	if got, _ := SelectSources(all, nil); len(got) != 6 {
		t.Errorf("default sources: %d", len(got))
	}
}
