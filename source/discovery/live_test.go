package discovery

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestLive queries the real AniList and torrent sites (read-only). Run with:
//
//	DISCOVERY_LIVE=1 go test ./source/discovery/ -run Live -v
//
// optionally DISCOVERY_LIVE_ID / DISCOVERY_LIVE_EP to pick the show/episode.
func TestLive(t *testing.T) {
	if os.Getenv("DISCOVERY_LIVE") == "" {
		t.Skip("set DISCOVERY_LIVE=1 to query the real sites")
	}
	id, ep := 154587, 5 // Sousou no Frieren
	if v, err := strconv.Atoi(os.Getenv("DISCOVERY_LIVE_ID")); err == nil {
		id = v
	}
	if v, err := strconv.Atoi(os.Getenv("DISCOVERY_LIVE_EP")); err == nil {
		ep = v
	}
	client := &http.Client{Timeout: 20 * time.Second}
	d := &Discoverer{AniList: NewAniList(client), Sources: AllSources(client), Prefs: Prefs{Resolution: "1080"}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	show, err := d.AniList.Show(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("show: %+v", show)
	res := d.Search(ctx, show, ep, false)
	for name, e := range res.Errors {
		t.Logf("source error %s: %s", name, e)
	}
	perSource := map[string]int{}
	for _, c := range res.Candidates {
		for _, s := range c.Sources {
			perSource[s]++
		}
	}
	t.Logf("candidates: %d, per source: %v", len(res.Candidates), perSource)
	for i, c := range res.Candidates {
		if i == 12 {
			break
		}
		t.Logf("%2d pick=%-5v acc=%-6s tier=%c seed=%-4d %-24v %s  %s", i, c.Pickable, c.Accuracy, max(c.Tier, '-'), c.Seeders, c.Sources, c.Title, c.Why)
	}
	if res.Picked == nil {
		t.Fatalf("nothing picked for %d ep %d", id, ep)
	}
	t.Logf("PICKED: %s [%v] %s", res.Picked.Title, res.Picked.Sources, res.Picked.Hash)
}
