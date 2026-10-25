package discovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Prefs steer the auto-pick (config DISCOVERY_*).
type Prefs struct {
	Resolution      string   // "1080": preferred resolution
	PreferredGroups []string // release groups in order of preference, e.g. SubsPlease, Erai-raws
	Exclusions      []string // case-insensitive substrings that disqualify a title
}

type Discoverer struct {
	AniList *AniList
	Sources []Source
	Prefs   Prefs
	Log     *slog.Logger
}

// AllSources builds the six ported sources sharing one HTTP client.
func AllSources(client *http.Client) []Source {
	return []Source{
		NewAnimeTosho(client), NewSeaDex(client), NewSubsPlease(client),
		NewNyaa(client), NewYameii(client), NewToonsHub(client),
	}
}

// SelectSources keeps the named ones (config DISCOVERY_SOURCES); empty = all.
func SelectSources(all []Source, names []string) ([]Source, error) {
	if len(names) == 0 {
		return all, nil
	}
	byName := map[string]Source{}
	for _, s := range all {
		byName[s.Name()] = s
	}
	var out []Source
	for _, n := range names {
		s, ok := byName[strings.ToLower(strings.TrimSpace(n))]
		if !ok {
			return nil, fmt.Errorf("unknown discovery source %q", n)
		}
		out = append(out, s)
	}
	return out, nil
}

// Candidate is a ranked result, merged across sources by infohash.
type Candidate struct {
	Result
	Group      string   `json:"group,omitempty"`      // [SubsPlease] → SubsPlease
	Resolution int      `json:"resolution,omitempty"` // 1080, 720, … (0 = unknown)
	Codec      string   `json:"codec,omitempty"`      // H.264 | HEVC | AV1 | ""
	Sources    []string `json:"sources"`
	Batch      bool     `json:"batch"`
	Match      string   `json:"match"` // exact | batch | weak
	Pickable   bool     `json:"pickable"`
	Why        string   `json:"why,omitempty"` // why it can't be auto-picked
}

type SearchResult struct {
	Show       *Show             `json:"show"`
	Episode    int               `json:"episode"` // -1 for a movie
	Mode       Mode              `json:"mode"`
	Candidates []Candidate       `json:"candidates"`
	Picked     *Candidate        `json:"picked"`
	Errors     map[string]string `json:"source_errors,omitempty"`
}

// ErrNoRelease: nothing pickable was found for the episode.
var ErrNoRelease = errors.New("no suitable release found")

// Search looks up one episode (or the movie, episode < 0 / single-episode
// shows) across every source, merges and ranks the results, and picks one.
// SearchOptions override the configured preferences for one search.
type SearchOptions struct {
	Resolution      string   // "" = configured default, "any" = no preference
	PreferredGroups []string // nil = configured default
	Exclusions      []string // added to the configured ones
	Sources         []string // nil = every configured source
	PreferDub       bool
	PreferH264      bool // rank H.264 above HEVC/AV1 (cheaper to decode on a small VPS)
}

func (d *Discoverer) Search(ctx context.Context, show *Show, episode int, preferDub bool) *SearchResult {
	return d.SearchWith(ctx, show, episode, SearchOptions{PreferDub: preferDub})
}

// SearchWith is Search with per-request options.
func (d *Discoverer) SearchWith(ctx context.Context, show *Show, episode int, o SearchOptions) *SearchResult {
	prefs := d.Prefs
	switch o.Resolution {
	case "":
	case "any":
		prefs.Resolution = ""
	default:
		prefs.Resolution = o.Resolution
	}
	if o.PreferredGroups != nil {
		prefs.PreferredGroups = o.PreferredGroups
	}
	prefs.Exclusions = append(append([]string(nil), d.Prefs.Exclusions...), o.Exclusions...)
	sources := d.Sources
	if o.Sources != nil {
		sources = nil
		for _, s := range d.Sources {
			if contains(o.Sources, s.Name()) {
				sources = append(sources, s)
			}
		}
	}

	mode := ModeSingle
	if show.Episodes == 1 || show.Format == "MOVIE" {
		mode, episode = ModeMovie, -1
	}
	q := Query{
		AnilistID: show.ID, Titles: show.Titles, Episode: episode, EpisodeCount: show.Episodes,
		Resolution: prefs.Resolution, Exclusions: prefs.Exclusions,
	}
	if mode == ModeMovie {
		q = d.AniList.withShowFacts(ctx, q)
	} else {
		q = d.AniList.withEpisodeCandidates(ctx, q)
	}

	res := &SearchResult{Show: show, Episode: episode, Mode: mode, Errors: map[string]string{}}
	var mu sync.Mutex
	var all []Result
	var wg sync.WaitGroup
	for _, src := range sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sctx, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			rs, err := src.Search(sctx, q, mode)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				res.Errors[src.Name()] = err.Error()
				if d.Log != nil {
					d.Log.Warn("discovery source failed", "source", src.Name(), "anilist_id", show.ID, "err", err)
				}
				return
			}
			for i := range rs {
				rs[i].Source = src.Name()
			}
			all = append(all, rs...)
		}()
	}
	wg.Wait()

	res.Candidates = rank(merge(all), prefs, o.PreferDub, o.PreferH264)
	for i := range res.Candidates {
		if res.Candidates[i].Pickable {
			res.Picked = &res.Candidates[i]
			break
		}
	}
	return res
}

// merge combines the same infohash from several sources: best accuracy and
// tier, highest seeder count, all source names.
func merge(rs []Result) []Candidate {
	byHash := map[string]*Candidate{}
	var order []string
	for _, r := range rs {
		c, ok := byHash[r.Hash]
		if !ok {
			byHash[r.Hash] = &Candidate{Result: r, Sources: []string{r.Source}}
			order = append(order, r.Hash)
			continue
		}
		c.Sources = append(c.Sources, r.Source)
		if accuracyRank(r.Accuracy) > accuracyRank(c.Accuracy) {
			c.Accuracy = r.Accuracy
		}
		if r.Tier != 0 && (c.Tier == 0 || r.Tier < c.Tier) {
			c.Tier = r.Tier
		}
		if r.HasSeeders && (!c.HasSeeders || r.Seeders > c.Seeders) {
			c.Seeders, c.Leechers, c.HasSeeders = r.Seeders, r.Leechers, true
		}
		if r.Type == "best" {
			c.Type = "best"
		}
		c.trusted = c.trusted || r.trusted
		c.remake = c.remake && r.remake
		if c.Size == 0 {
			c.Size = r.Size
		}
	}
	out := make([]Candidate, 0, len(order))
	for _, h := range order {
		out = append(out, *byHash[h])
	}
	return out
}

func accuracyRank(a string) int {
	switch a {
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	}
	return 0
}

var resolutionRe = regexp.MustCompile(`(?i)\b(2160|1080|720|576|480)p?\b`)

func resolutionOf(title string) int {
	if m := resolutionRe.FindStringSubmatch(title); m != nil {
		return atoi(m[1])
	}
	return 0
}

// deadAfter: a release a seeder-reporting source shows with 0 seeders and
// older than this is probably unfetchable.
const deadAfter = 7 * 24 * time.Hour

// rank orders candidates for auto-pick. Only exact single-episode (or movie)
// hits are pickable: the pipeline takes the largest video file of a torrent,
// so a batch would yield the wrong episode. Order among pickables:
//  1. preferred resolution, else the highest one ≤ it, else the rest
//  2. preferred release group (config order)
//  3. dub preference (Yameii first when asked for, last otherwise)
//  4. seeders, in coarse buckets (0, 1–4, 5–19, 20–99, 100+) so availability
//     beats source trust only when the gap is real
//  5. accuracy (exact ID lookup > text match), SeaDex "best"
//  6. trusted uploader, not a remake
//  7. exact seeders, then newest
func rank(cs []Candidate, p Prefs, preferDub, preferH264 bool) []Candidate {
	want := atoi(p.Resolution)
	for i := range cs {
		c := &cs[i]
		c.Group, c.Resolution, c.Codec = groupOf(c.Title), resolutionOf(c.Title), codecOf(c.Title)
		c.Batch = c.Type == "batch" || c.Tier == 'B'
		c.Match = map[byte]string{'A': "exact", 'B': "batch"}[c.Tier]
		if c.Match == "" {
			c.Match = "weak"
		}
		switch {
		case c.Batch:
			c.Why = "batch release (multi-episode torrents aren't supported yet)"
		case c.Tier != 'A' || c.Accuracy == "low":
			c.Why = "not an exact match for this episode"
		case c.HasSeeders && c.Seeders == 0 && time.Since(c.Date) > deadAfter:
			c.Why = "no seeders"
		default:
			c.Pickable = true
		}
	}
	groupRank := func(title string) int {
		lt := strings.ToLower(title)
		for i, g := range p.PreferredGroups {
			if g != "" && strings.Contains(lt, "["+strings.ToLower(g)+"]") {
				return i
			}
		}
		return len(p.PreferredGroups)
	}
	resRank := func(title string) int {
		r := resolutionOf(title)
		switch {
		case want == 0:
			return 0
		case r == want:
			return 0
		case r > 0 && r < want:
			return 1 + (want - r) // nearer below is better
		}
		return 10000 // unknown or above the preference
	}
	dubRank := func(c Candidate) int {
		isDub := c.Source == "yameii" || contains(c.Sources, "yameii")
		if preferDub == isDub {
			return 0
		}
		return 1
	}
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if a.Pickable != b.Pickable {
			return a.Pickable
		}
		if ra, rb := resRank(a.Title), resRank(b.Title); ra != rb {
			return ra < rb
		}
		if ga, gb := groupRank(a.Title), groupRank(b.Title); ga != gb {
			return ga < gb
		}
		if da, db := dubRank(a), dubRank(b); da != db {
			return da < db
		}
		if preferH264 && (a.Codec == "H.264") != (b.Codec == "H.264") {
			return a.Codec == "H.264"
		}
		if sa, sb := seedBucket(a), seedBucket(b); sa != sb {
			return sa > sb
		}
		if aa, ab := accuracyRank(a.Accuracy), accuracyRank(b.Accuracy); aa != ab {
			return aa > ab
		}
		if (a.Type == "best") != (b.Type == "best") {
			return a.Type == "best"
		}
		if qa, qb := qualityRank(a.Result), qualityRank(b.Result); qa != qb {
			return qa < qb
		}
		if a.Seeders != b.Seeders {
			return a.Seeders > b.Seeders
		}
		return a.Date.After(b.Date)
	})
	return cs
}

var (
	groupRe = regexp.MustCompile(`^\s*\[([^\]]+)\]`)
	hevcRe  = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(hevc|x265|h\.?265)(?:$|[^a-z0-9])`)
	av1Re   = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])av1(?:$|[^a-z0-9])`)
	avcRe   = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])(avc|x264|h\.?264)(?:$|[^a-z0-9])`)
)

func groupOf(title string) string {
	if m := groupRe.FindStringSubmatch(title); m != nil {
		return m[1]
	}
	return ""
}

func codecOf(title string) string {
	switch {
	case hevcRe.MatchString(title):
		return "HEVC"
	case av1Re.MatchString(title):
		return "AV1"
	case avcRe.MatchString(title):
		return "H.264"
	}
	return ""
}

// seedBucket: unknown seeder counts (SubsPlease/SeaDex only) sit in the middle.
func seedBucket(c Candidate) int {
	if !c.HasSeeders {
		return 2
	}
	switch n := c.Seeders; {
	case n >= 100:
		return 4
	case n >= 20:
		return 3
	case n >= 5:
		return 2
	case n >= 1:
		return 1
	}
	return 0
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
