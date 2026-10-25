package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SubsPlease (anitorrent src/subsplease.js): the site's own JSON API for
// currently airing shows. Fastest for new episodes, limited catalog.
type SubsPlease struct {
	BaseURL string
	Client  *http.Client

	mu       sync.Mutex
	latest   []spEntry
	latestAt time.Time
}

const (
	subsPleaseBase    = "https://subsplease.org/api/"
	subsPleaseDefault = "high"
)

func NewSubsPlease(client *http.Client) *SubsPlease {
	return &SubsPlease{BaseURL: subsPleaseBase, Client: client}
}

func (s *SubsPlease) Name() string { return "subsplease" }

type spEntry struct {
	Key         string `json:"-"`
	Episode     string `json:"episode"`
	ReleaseDate string `json:"release_date"`
	Downloads   []struct {
		Res    string `json:"res"`
		Magnet string `json:"magnet"`
	} `json:"downloads"`
}

// fetchAPI decodes {"Show - 05": {...}, ...}; an empty body or [] means no hits.
func (s *SubsPlease) fetchAPI(ctx context.Context, params url.Values) ([]spEntry, error) {
	b, err := get(ctx, s.Client, "SubsPlease", s.BaseURL+"?"+params.Encode(), false)
	if err != nil {
		return nil, err
	}
	var m map[string]spEntry
	if json.Unmarshal(b, &m) != nil {
		return nil, nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]spEntry, 0, len(m))
	for _, k := range keys {
		e := m[k]
		e.Key = k
		out = append(out, e)
	}
	return out, nil
}

func (s *SubsPlease) latestEntries(ctx context.Context) []spEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.latest == nil || time.Since(s.latestAt) > 10*time.Minute {
		if es, err := s.fetchAPI(ctx, url.Values{"f": {"latest"}, "tz": {"UTC"}}); err == nil && es != nil {
			s.latest, s.latestAt = es, time.Now()
		}
	}
	return s.latest
}

var (
	b32HashRe = regexp.MustCompile(`(?i)xt=urn:btih:([A-Z2-7]{32}|[a-fA-F0-9]{40})`)
	xlRe      = regexp.MustCompile(`(?i)[?&]xl=(\d+)`)
	spRangeRe = regexp.MustCompile(`\d+\s*[-~]\s*\d+`)
	spBatchRe = regexp.MustCompile(`(?i)batch`)
	base32Abc = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
)

func base32ToHex(b32 string) string {
	var bits strings.Builder
	for _, c := range strings.ToUpper(b32) {
		if i := strings.IndexRune(base32Abc, c); i >= 0 {
			fmt.Fprintf(&bits, "%05b", i)
		}
	}
	bs := bits.String()
	var hex strings.Builder
	for i := 0; i+4 <= len(bs); i += 4 {
		n, _ := strconv.ParseInt(bs[i:i+4], 2, 8)
		fmt.Fprintf(&hex, "%x", n)
	}
	return hex.String()
}

func parseMagnet(m string) (string, int64) {
	hash := ""
	if g := b32HashRe.FindStringSubmatch(m); g != nil {
		if len(g[1]) == 40 {
			hash = strings.ToLower(g[1])
		} else {
			hash = base32ToHex(g[1])
		}
	}
	var size int64
	if g := xlRe.FindStringSubmatch(m); g != nil {
		size, _ = strconv.ParseInt(g[1], 10, 64)
	}
	return hash, size
}

func spEpisodeMatches(entryEp string, wanted int) bool {
	if wanted < 0 {
		return true
	}
	e := strings.TrimSpace(entryEp)
	if e == "" || strings.ContainsAny(e, "-~") {
		return false
	}
	n, err := strconv.Atoi(leadingDigits(e))
	return err == nil && n == wanted
}

func leadingDigits(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}

func spIsBatch(e spEntry) bool {
	return spRangeRe.MatchString(e.Episode) || spBatchRe.MatchString(e.Episode)
}

func spEpisodeMatchesAny(e spEntry, ctx *matchContext) bool {
	if len(ctx.candidates) > 0 {
		for n := range ctx.candidates {
			if spEpisodeMatches(e.Episode, n) {
				return true
			}
		}
		return false
	}
	return spEpisodeMatches(e.Episode, ctx.episode)
}

func (s *SubsPlease) entryResults(e spEntry, tier byte, batch bool, exclusions []string) []Result {
	date, ok := parseDate(e.ReleaseDate)
	if !ok {
		date = time.Now()
	}
	var out []Result
	for _, dl := range e.Downloads {
		if dl.Magnet == "" {
			continue
		}
		hash, size := parseMagnet(dl.Magnet)
		if hash == "" {
			continue
		}
		title := "[SubsPlease] " + e.Key
		if dl.Res != "" {
			title += " (" + dl.Res + "p)"
		}
		if hitsExclusion(title, exclusions) {
			continue
		}
		r := Result{Title: title, Link: dl.Magnet, Hash: hash, Size: size, Date: date, Source: s.Name(), Tier: tier}
		if batch {
			r.Accuracy = "low"
		} else {
			r.Accuracy = tagAccuracy(tier, date, subsPleaseDefault)
		}
		if batch || tier == 'B' {
			r.Type = "batch"
		}
		out = append(out, r)
	}
	return out
}

type spShaped struct {
	e    spEntry
	Tier byte
}

func (s *SubsPlease) Search(ctx context.Context, q Query, mode Mode) ([]Result, error) {
	if len(q.Titles) == 0 {
		return nil, nil
	}
	mctx := searchContext(q, mode)
	seenKeys := map[string]bool{}
	var entries []spEntry
	var lastErr error
	for _, term := range buildQueries(q.Titles, 3, -1, q.AnilistID, nil).bases {
		es, err := s.fetchAPI(ctx, url.Values{"f": {"search"}, "tz": {"UTC"}, "s": {term}})
		if err != nil {
			lastErr = err
			continue
		}
		for _, e := range es {
			if !seenKeys[e.Key] {
				seenKeys[e.Key] = true
				entries = append(entries, e)
			}
		}
	}
	if len(entries) == 0 && lastErr != nil {
		return nil, lastErr
	}
	if len(entries) == 0 {
		for _, e := range s.latestEntries(ctx) {
			if !seenKeys[e.Key] {
				seenKeys[e.Key] = true
				entries = append(entries, e)
			}
		}
	}

	build := func(c map[int]bool) []spShaped {
		ectx := mctx.withCandidates(c)
		var out []spShaped
		for _, e := range entries {
			tier := classifyResult(e.Key, ectx)
			if tier == 0 {
				continue
			}
			isBatch := spIsBatch(e)
			if (mode == ModeBatch && !isBatch) || (mode == ModeMovie && isBatch) {
				continue
			}
			if mode == ModeSingle {
				if isBatch {
					tier = 'B'
				} else if tier == 'A' && !spEpisodeMatchesAny(e, ectx) {
					tier = 'C'
				}
			}
			out = append(out, spShaped{e, tier})
		}
		return out
	}
	shaped := build(nil)
	hasA := func(xs []spShaped) bool {
		for _, x := range xs {
			if x.Tier == 'A' {
				return true
			}
		}
		return false
	}
	if !hasA(shaped) && len(mctx.candidates) > 0 {
		var best []spShaped
		var bestNewest time.Time
		bestEp, found := 0, false
		for _, n := range sortedKeys(mctx.candidates) {
			cand := build(map[int]bool{n: true})
			var newest time.Time
			hit := false
			for _, x := range cand {
				if x.Tier != 'A' {
					continue
				}
				hit = true
				if t, ok := parseDate(x.e.ReleaseDate); ok && t.After(newest) {
					newest = t
				}
			}
			if hit && (!found || newest.After(bestNewest)) {
				best, bestNewest, bestEp, found = cand, newest, n, true
			}
		}
		if found {
			shaped = best
			mctx.chosenEpisodes = map[int]bool{bestEp: true}
			mctx.offsetResolved = bestEp != mctx.episode
		}
	}

	seen := map[string]bool{}
	var out []Result
	for _, x := range shaped {
		for _, r := range s.entryResults(x.e, x.Tier, mode == ModeBatch, mctx.exclusions) {
			if !seen[r.Hash] {
				seen[r.Hash] = true
				out = append(out, r)
			}
		}
	}
	return finalize(out, mctx), nil
}
