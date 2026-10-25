package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// AnimeTosho (anitorrent src/animetosho.js): exact AniDB anime-ID lookup via
// an AniList→AniDB mapping, falling back to text search when no exact
// episode hit is found.
type AnimeTosho struct {
	// BaseURLs are tried in order when one can't be reached: some ISPs block
	// the feed. subdomain while animetosho.org itself works.
	BaseURLs   []string
	MappingURL string // {"anilistId": anidbAid}
	Client     *http.Client

	mu        sync.Mutex
	mapping   map[string]int
	mappingAt time.Time
}

const (
	anidbMappingURL   = "https://raw.githubusercontent.com/anh9000/anitorrent/main/data/anilist-to-anidb.json"
	mappingTTL        = 24 * time.Hour
	animeToshoDefault = "high"
)

func NewAnimeTosho(client *http.Client) *AnimeTosho {
	return &AnimeTosho{
		BaseURLs:   []string{"https://feed.animetosho.org/json", "https://animetosho.org/feed/json"},
		MappingURL: anidbMappingURL, Client: client,
	}
}

func (s *AnimeTosho) Name() string { return "animetosho" }

func (s *AnimeTosho) anidbID(ctx context.Context, anilistID int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mapping == nil || time.Since(s.mappingAt) > mappingTTL {
		var raw map[string]json.Number
		if err := getJSON(ctx, s.Client, "anilist-to-anidb mapping", s.MappingURL, &raw); err == nil {
			m := make(map[string]int, len(raw))
			for k, v := range raw {
				if n, err := v.Int64(); err == nil && n > 0 {
					m[k] = int(n)
				}
			}
			s.mapping, s.mappingAt = m, time.Now()
		} else if s.mapping == nil {
			return 0 // retried on the next search
		}
	}
	return s.mapping[strconv.Itoa(anilistID)]
}

type toshoItem struct {
	Title       string      `json:"title"`
	TorrentName string      `json:"torrent_name"`
	InfoHash    string      `json:"info_hash"`
	MagnetURI   string      `json:"magnet_uri"`
	Seeders     json.Number `json:"seeders"`
	Leechers    json.Number `json:"leechers"`
	Downloaded  json.Number `json:"torrent_downloaded_count"`
	TotalSize   json.Number `json:"total_size"`
	Timestamp   int64       `json:"timestamp"`
}

func (s *AnimeTosho) fetch(ctx context.Context, params url.Values, accuracy string) ([]Result, error) {
	var items []toshoItem
	var err error
	for _, base := range s.BaseURLs {
		err = getJSON(ctx, s.Client, "AnimeTosho", base+"?"+params.Encode(), &items)
		var he *httpError
		if err == nil || errors.As(err, &he) || ctx.Err() != nil {
			break // reached the server (or gave up): don't try the next host
		}
	}
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, it := range items {
		if it.InfoHash == "" {
			continue
		}
		title := it.Title
		if title == "" {
			title = it.TorrentName
		}
		link := it.MagnetURI
		if link == "" {
			link = buildMagnet(it.InfoHash, title)
		}
		seeders, _ := it.Seeders.Int64()
		leechers, _ := it.Leechers.Int64()
		dl, _ := it.Downloaded.Int64()
		size, _ := it.TotalSize.Int64()
		date := time.Now()
		if it.Timestamp > 0 {
			date = time.Unix(it.Timestamp, 0)
		}
		out = append(out, Result{
			Title: title, Link: link, Hash: lower(it.InfoHash), Seeders: int(seeders), Leechers: int(leechers),
			Downloads: int(dl), Size: size, Date: date, Accuracy: accuracy, Source: s.Name(), HasSeeders: true,
		})
	}
	return out, nil
}

func (s *AnimeTosho) classifyAndTag(raw []Result, ctx *matchContext) []Result {
	var items []Result
	for _, r := range dedupe(raw) {
		if !hitsExclusion(r.Title, ctx.exclusions) {
			items = append(items, r)
		}
	}
	out := shapeAll(items, ctx, animeToshoDefault)
	if ctx.mode == ModeBatch {
		var batches []Result
		for _, r := range out {
			if looksLikeBatch(r.Title) {
				r.Type, r.Accuracy = "batch", "low"
				batches = append(batches, r)
			}
		}
		return batches
	}
	return out
}

func (s *AnimeTosho) Search(ctx context.Context, q Query, mode Mode) ([]Result, error) {
	mctx := searchContext(q, mode)
	var raw []Result
	if aid := s.anidbID(ctx, q.AnilistID); aid > 0 {
		raw, _ = s.fetch(ctx, url.Values{"aid": {strconv.Itoa(aid)}}, "high") // errors fall through to text search
	}
	results := s.classifyAndTag(raw, mctx)
	if !hasTier(results, 'A') && len(q.Titles) > 0 {
		seen := map[string]bool{}
		for _, r := range results {
			seen[r.Hash] = true
		}
		text, err := s.textSearch(ctx, q, mctx)
		if err != nil && len(results) == 0 {
			return nil, err
		}
		for _, r := range s.classifyAndTag(text, mctx) {
			if !seen[r.Hash] {
				seen[r.Hash] = true
				results = append(results, r)
			}
		}
	}
	return finalize(results, mctx), nil
}

// textSearch: base title queries first; numbered "title 05" queries only if
// those found no exact episode.
func (s *AnimeTosho) textSearch(ctx context.Context, q Query, mctx *matchContext) ([]Result, error) {
	qs := buildQueries(q.Titles, 2, q.Episode, q.AnilistID, nil)
	seen := map[string]Result{}
	var order []string
	var lastErr error
	run := func(list []string) {
		for _, term := range list {
			rs, err := s.fetch(ctx, url.Values{"q": {term}}, "medium")
			if err != nil {
				lastErr = err
				continue
			}
			for _, r := range rs {
				if _, ok := seen[r.Hash]; !ok {
					seen[r.Hash] = r
					order = append(order, r.Hash)
				}
			}
		}
	}
	collected := func() []Result {
		out := make([]Result, 0, len(order))
		for _, h := range order {
			out = append(out, seen[h])
		}
		return out
	}
	run(qs.bases)
	if len(qs.numbered) > 0 && !hasTier(s.classifyAndTag(collected(), mctx), 'A') {
		run(qs.numbered)
	}
	if len(order) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return collected(), nil
}

func hasTier(rs []Result, tier byte) bool {
	for _, r := range rs {
		if r.Tier == tier {
			return true
		}
	}
	return false
}
