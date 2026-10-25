package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"sort"
	"sync"
	"time"
)

const anilistAPI = "https://graphql.anilist.co"

// ErrNotFound: AniList has no anime with that ID.
var ErrNotFound = errors.New("anilist: no such anime")

// AniList queries graphql.anilist.co: show facts for building a Query, and
// the prequel chain shared.js uses to map per-season episode numbers onto
// absolute ones.
type AniList struct {
	URL    string
	Client *http.Client

	mu     sync.Mutex
	chains map[int]chainEntry
}

func NewAniList(client *http.Client) *AniList {
	return &AniList{URL: anilistAPI, Client: client, chains: map[int]chainEntry{}}
}

// Show is what discovery needs to know about an anime.
type Show struct {
	ID         int      `json:"anilist_id"`
	Titles     []string `json:"titles"` // romaji, english, synonyms, native
	Episodes   int      `json:"episodes"`
	Format     string   `json:"format"`
	Status     string   `json:"status"`
	StartYear  int      `json:"start_year,omitempty"`
	NextAiring int      `json:"next_airing_episode,omitempty"`
}

type alTitle struct {
	Romaji  *string `json:"romaji"`
	English *string `json:"english"`
	Native  *string `json:"native"`
}

type alMedia struct {
	ID                int                    `json:"id"`
	Episodes          *int                   `json:"episodes"`
	Format            string                 `json:"format"`
	Status            string                 `json:"status"`
	Title             alTitle                `json:"title"`
	Synonyms          []string               `json:"synonyms"`
	StartDate         *struct{ Year *int }   `json:"startDate"`
	NextAiringEpisode *struct{ Episode int } `json:"nextAiringEpisode"`
	// Relations is nil when the query didn't ask for it at this depth, which
	// the chain walk uses to know it must query further.
	Relations *struct {
		Edges []struct {
			RelationType string  `json:"relationType"`
			Node         alMedia `json:"node"`
		} `json:"edges"`
	} `json:"relations"`
}

const showQuery = `query($id:Int){Media(id:$id,type:ANIME){id episodes format status title{romaji english native} synonyms startDate{year} nextAiringEpisode{episode}}}`

// Show fetches an anime's titles and episode facts.
func (a *AniList) Show(ctx context.Context, id int) (*Show, error) {
	m, failed, err := a.query(ctx, showQuery, id)
	if err != nil {
		return nil, err
	}
	if failed {
		return nil, errors.New("anilist: unavailable or rate limited")
	}
	if m == nil {
		return nil, ErrNotFound
	}
	s := &Show{ID: m.ID, Format: m.Format, Status: m.Status}
	for _, t := range []*string{m.Title.Romaji, m.Title.English} {
		if t != nil && *t != "" {
			s.Titles = append(s.Titles, *t)
		}
	}
	s.Titles = append(s.Titles, m.Synonyms...)
	if m.Title.Native != nil && *m.Title.Native != "" {
		s.Titles = append(s.Titles, *m.Title.Native)
	}
	if m.Episodes != nil {
		s.Episodes = *m.Episodes
	}
	if m.StartDate != nil && m.StartDate.Year != nil {
		s.StartYear = *m.StartDate.Year
	}
	if m.NextAiringEpisode != nil {
		s.NextAiring = m.NextAiringEpisode.Episode
	}
	return s, nil
}

// query returns (media, failed, err): failed is JS `undefined` (rate limited,
// GraphQL errors, retries exhausted); media nil without failed is "no such
// anime". err is only context cancellation.
func (a *AniList) query(ctx context.Context, q string, id int) (*alMedia, bool, error) {
	body, _ := json.Marshal(map[string]any{"query": q, "variables": map[string]int{"id": id}})
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			d := time.Duration(600*attempt+rand.IntN(300)) * time.Millisecond
			select {
			case <-ctx.Done():
				return nil, false, ctx.Err()
			case <-time.After(d):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.URL, bytes.NewReader(body))
		if err != nil {
			return nil, false, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		res, err := a.Client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, false, ctx.Err()
			}
			continue
		}
		var parsed struct {
			Data   *struct{ Media *alMedia } `json:"data"`
			Errors []json.RawMessage         `json:"errors"`
		}
		decodeErr := json.NewDecoder(res.Body).Decode(&parsed)
		res.Body.Close()
		if res.StatusCode == http.StatusTooManyRequests {
			return nil, true, nil
		}
		// AniList answers an unknown ID with 404 + errors and data.Media null.
		if res.StatusCode == http.StatusNotFound && decodeErr == nil && parsed.Data != nil {
			return nil, false, nil
		}
		if res.StatusCode >= 300 || decodeErr != nil {
			continue
		}
		if len(parsed.Errors) > 0 && parsed.Data == nil {
			return nil, true, nil
		}
		if parsed.Data == nil {
			return nil, false, nil
		}
		return parsed.Data.Media, false, nil
	}
	return nil, true, nil
}

// --- prequel chain (shared.js getChain) --------------------------------------

const relationNode = "id episodes format title{romaji english}"

func nestedRelations(depth int) string {
	inner := relationNode
	for i := 0; i < depth; i++ {
		inner = relationNode + " relations{edges{relationType node{" + inner + "}}}"
	}
	return inner
}

var (
	chainQuery = "query($id:Int){Media(id:$id){episodes status startDate{year} nextAiringEpisode{episode} relations{edges{relationType node{" + nestedRelations(2) + "}}}}}"
	stepQuery  = "query($id:Int){Media(id:$id){episodes format status nextAiringEpisode{episode} relations{edges{relationType node{" + relationNode + "}}}}}"
)

type prequel struct {
	episodes int
	tokens   map[string]bool
}

type chain struct {
	counts []prequel // nearest prequel first
	// sequelTokens: title tokens of the show's direct sequels (prequel
	// tokens are in counts). Used to reject sibling-season releases.
	sequelTokens map[string]bool
	nextAiring   int
	startYear    int
	ok           bool
}

type chainEntry struct {
	c  chain
	at time.Time
}

const chainTTL = 6 * time.Hour

func pickPrequel(m *alMedia) *alMedia {
	if m == nil || m.Relations == nil {
		return nil
	}
	var best *alMedia
	for i := range m.Relations.Edges {
		e := &m.Relations.Edges[i]
		if e.RelationType != "PREQUEL" {
			continue
		}
		f := e.Node.Format
		if f != "TV" && f != "ONA" && f != "TV_SHORT" {
			continue
		}
		if best == nil || eps(&e.Node) > eps(best) {
			best = &e.Node
		}
	}
	return best
}

func eps(m *alMedia) int {
	if m.Episodes == nil {
		return 0
	}
	return *m.Episodes
}

func prequelEntry(m *alMedia) prequel {
	var titles []string
	for _, t := range []*string{m.Title.Romaji, m.Title.English} {
		if t != nil && *t != "" {
			titles = append(titles, *t)
		}
	}
	return prequel{episodes: eps(m), tokens: buildTitleTokens(titles)}
}

func airingOf(m *alMedia) int {
	if m != nil && m.NextAiringEpisode != nil && m.NextAiringEpisode.Episode > 0 {
		return m.NextAiringEpisode.Episode
	}
	return 0
}

func (a *AniList) fetchPrequelChain(ctx context.Context, id int) (chain, error) {
	root, failed, err := a.query(ctx, chainQuery, id)
	if err != nil {
		return chain{}, err
	}
	if failed {
		return a.walkFrom(ctx, id, nil, 0, map[int]bool{})
	}
	if root == nil {
		return chain{ok: true}, nil
	}
	c := chain{nextAiring: airingOf(root), ok: true, sequelTokens: a.walkSequels(ctx, root, id)}
	if root.StartDate != nil && root.StartDate.Year != nil {
		c.startYear = *root.StartDate.Year
	}
	seen := map[int]bool{id: true}
	node, lastID := root, id
	for len(c.counts) < 12 {
		p := pickPrequel(node)
		if p == nil || eps(p) == 0 || seen[p.ID] {
			break
		}
		c.counts = append(c.counts, prequelEntry(p))
		seen[p.ID] = true
		lastID = p.ID
		if p.Relations == nil {
			break // deeper than the nested query reached
		}
		node = p
	}
	if len(c.counts) == 0 {
		return c, nil
	}
	rest, err := a.walkFrom(ctx, lastID, c.counts, c.nextAiring, seen)
	rest.startYear, rest.sequelTokens = c.startYear, c.sequelTokens
	return rest, err
}

func (a *AniList) walkFrom(ctx context.Context, start int, counts []prequel, nextAiring int, seen map[int]bool) (chain, error) {
	c := chain{counts: counts, nextAiring: nextAiring, ok: true}
	current := start
	for depth := 0; depth < 12 && current != 0; depth++ {
		m, failed, err := a.query(ctx, stepQuery, current)
		if err != nil {
			return c, err
		}
		if failed {
			c.ok = false
			break
		}
		if m == nil {
			break
		}
		if depth == 0 && c.nextAiring == 0 {
			c.nextAiring = airingOf(m)
		}
		p := pickPrequel(m)
		if p == nil || eps(p) == 0 || seen[p.ID] {
			break
		}
		c.counts = append(c.counts, prequelEntry(p))
		seen[p.ID] = true
		current = p.ID
	}
	return c, nil
}

// getChain caches successful chains for chainTTL; failures are retried next time.
func (a *AniList) getChain(ctx context.Context, id int) chain {
	a.mu.Lock()
	if e, ok := a.chains[id]; ok && time.Since(e.at) < chainTTL {
		a.mu.Unlock()
		return e.c
	}
	a.mu.Unlock()
	c, err := a.fetchPrequelChain(ctx, id)
	if err != nil || !c.ok {
		return chain{}
	}
	a.mu.Lock()
	a.chains[id] = chainEntry{c, time.Now()}
	a.mu.Unlock()
	return c
}

func sharesSubSeries(showTokens, prequelTokens map[string]bool) bool {
	if len(showTokens) == 0 || len(prequelTokens) == 0 {
		return true
	}
	shared := 0
	for t := range prequelTokens {
		if showTokens[t] {
			shared++
		}
	}
	return float64(shared)/float64(len(showTokens)) >= 0.5
}

// withEpisodeCandidates fills the chain-derived fields of a single-episode
// query: absolute-number candidates, not-yet-aired, chain season, start year.
func (a *AniList) withEpisodeCandidates(ctx context.Context, q Query) Query {
	if q.Episode < 0 || q.AnilistID == 0 {
		return q
	}
	c := a.getChain(ctx, q.AnilistID)
	if !c.ok {
		return q
	}
	cands := []int{q.Episode}
	showTokens := buildTitleTokens(q.Titles)
	q.ForeignTokens = foreignTokens(c, showTokens)
	running := 0
	for _, p := range c.counts {
		if p.episodes == 0 {
			continue
		}
		if !sharesSubSeries(showTokens, p.tokens) && running >= 10 {
			cands = append(cands, q.Episode+running)
		}
		running += p.episodes
	}
	if running >= 10 {
		cands = append(cands, q.Episode+running)
	}
	if c.nextAiring > 0 && q.Episode >= c.nextAiring {
		q.NotAired = true
	}
	if len(c.counts) > 0 {
		q.ChainSeason = len(c.counts) + 1
	}
	q.StartYear = c.startYear
	if uniq := dedupInts(cands); len(uniq) > 1 {
		q.EpisodeCandidates = uniq
	}
	return q
}

// withShowFacts adds the start year (movie mode uses it for year matching).
func (a *AniList) withShowFacts(ctx context.Context, q Query) Query {
	if q.AnilistID == 0 {
		return q
	}
	if c := a.getChain(ctx, q.AnilistID); c.ok {
		if c.startYear > 0 {
			q.StartYear = c.startYear
		}
		q.ForeignTokens = foreignTokens(c, buildTitleTokens(q.Titles))
	}
	return q
}

// walkSequels collects title tokens of every TV sequel after the show,
// transitively (Tokyo Revengers S1 → Seiya Kessen-hen → Tenjiku-hen → ...):
// a sequel two seasons later restarts numbering just the same. It uses the
// nested relations the chain query already returned, querying AniList only
// where those run out. At most 12 sequels.
func (a *AniList) walkSequels(ctx context.Context, root *alMedia, rootID int) map[string]bool {
	out := map[string]bool{}
	seen := map[int]bool{rootID: true}
	queue := []*alMedia{root}
	for steps := 0; len(queue) > 0 && steps < 12; {
		m := queue[0]
		queue = queue[1:]
		if m.Relations == nil { // beyond the nested query: fetch this node's relations
			fetched, failed, err := a.query(ctx, stepQuery, m.ID)
			if err != nil || failed || fetched == nil {
				continue
			}
			m = fetched
		}
		for i := range m.Relations.Edges {
			e := &m.Relations.Edges[i]
			f := e.Node.Format
			if e.RelationType != "SEQUEL" || seen[e.Node.ID] || (f != "TV" && f != "ONA" && f != "TV_SHORT") {
				continue
			}
			seen[e.Node.ID] = true
			for t := range prequelEntry(&e.Node).tokens {
				out[t] = true
			}
			queue = append(queue, &e.Node)
			steps++
		}
	}
	return out
}

// foreignTokens (torClient addition, not in anitorrent): title words that
// belong only to neighbouring seasons of the franchise. Split-cour shows
// reuse the franchise name and restart numbering, so "Bleach: Sennen Kessen
// Hen - Soukoku Tan - 03" (part 3) otherwise matches part 4 episode 3
// exactly; its "soukoku" never appears in part 4's titles.
func foreignTokens(c chain, showTokens map[string]bool) []string {
	set := map[string]bool{}
	add := func(ts map[string]bool) {
		for t := range ts {
			if !showTokens[t] && !allDigitsRe.MatchString(t) {
				set[t] = true
			}
		}
	}
	for _, p := range c.counts {
		add(p.tokens)
	}
	add(c.sequelTokens)
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func dedupInts(ns []int) []int {
	seen := map[int]bool{}
	var out []int
	for _, n := range ns {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}
