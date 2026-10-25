package discovery

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// Mode is the kind of search (Hayase's single/batch/movie).
type Mode string

const (
	ModeSingle Mode = "single"
	ModeBatch  Mode = "batch"
	ModeMovie  Mode = "movie"
)

// Query is what a source searches for. Episode < 0 means "no episode".
type Query struct {
	AnilistID    int
	Titles       []string // AniList romaji, english, synonyms, native
	Episode      int
	EpisodeCount int
	Resolution   string
	Exclusions   []string

	// Filled from AniList's prequel chain (withEpisodeCandidates / withShowFacts).
	EpisodeCandidates []int    // other numberings of the same episode (absolute counts)
	ForeignTokens     []string // words only sibling seasons use (see foreignTokens)
	NotAired          bool
	ChainSeason       int
	StartYear         int
}

// Result is one torrent a source found.
type Result struct {
	Title     string    `json:"title"`
	Link      string    `json:"magnet"`
	Hash      string    `json:"infohash"`
	Seeders   int       `json:"seeders"`
	Leechers  int       `json:"leechers"`
	Downloads int       `json:"downloads"`
	Size      int64     `json:"size_bytes"`
	Date      time.Time `json:"date"`
	Accuracy  string    `json:"accuracy"`       // high | medium | low
	Type      string    `json:"type,omitempty"` // best | alt | batch
	Source    string    `json:"source"`
	// HasSeeders: the source reports seeder counts (SubsPlease/SeaDex don't).
	HasSeeders bool `json:"-"`

	// Tier is how well the title matches the requested episode: 'A' exact,
	// 'B' a batch that covers it, 'C' weak fallback. Only 'A' is auto-picked.
	Tier    byte `json:"-"`
	trusted bool
	remake  bool
}

// matchContext is shared.js searchContext plus the fields shapeAll sets.
type matchContext struct {
	mode           Mode
	showTokens     map[string]bool
	notAired       bool
	chainSeason    int
	showSeason     int
	seasonMarks    map[string]bool
	showYears      map[string]bool
	minHits        int
	episode        int
	candidates     map[int]bool // nil = none
	exclusions     []string
	resolution     string
	foreignTokens  []string
	chosenEpisodes map[int]bool
	offsetResolved bool
}

func searchContext(q Query, mode Mode) *matchContext {
	primaryTokens := map[string]bool{}
	if ranked := rankTitlesForQuery(q.Titles); len(ranked) > 0 {
		primaryTokens = buildTitleTokens(ranked[:1])
	}
	ctx := &matchContext{
		mode:          mode,
		showTokens:    buildTitleTokens(q.Titles),
		notAired:      q.NotAired,
		chainSeason:   q.ChainSeason,
		showSeason:    detectShowSeason(q.Titles),
		seasonMarks:   seasonMarkerTokens(q.Titles),
		showYears:     yearsForShow(q.Titles, q.StartYear, mode),
		minHits:       1,
		episode:       q.Episode,
		exclusions:    q.Exclusions,
		resolution:    q.Resolution,
		foreignTokens: q.ForeignTokens,
	}
	// anitorrent requires 2 hits only from 3 tokens up. torClient also does
	// for exactly 2: "No Game No Life" is {game, life}, and one hit let
	// "Otome Game no Hametsu Flag..." through as a match.
	if len(primaryTokens) >= 2 {
		ctx.minHits = 2
	}
	if len(q.EpisodeCandidates) > 0 {
		ctx.candidates = intSet(q.EpisodeCandidates)
	}
	return ctx
}

func intSet(ns []int) map[int]bool {
	m := make(map[int]bool, len(ns))
	for _, n := range ns {
		m[n] = true
	}
	return m
}

func yearsForShow(titles []string, startYear int, mode Mode) map[string]bool {
	years := detectShowYears(titles)
	if len(years) > 0 || mode != ModeMovie || startYear == 0 {
		return years
	}
	return toSet(itoa(startYear-1), itoa(startYear), itoa(startYear+1))
}

// withCandidates is ctx with episodeCandidates replaced (a shallow copy, as
// the JS `{...ctx, episodeCandidates}`).
func (ctx *matchContext) withCandidates(c map[int]bool) *matchContext {
	cp := *ctx
	cp.candidates = c
	return &cp
}

func usesOffsetEpisode(ctx *matchContext) bool {
	return len(ctx.candidates) == 1 && ctx.episode >= 0 && !ctx.candidates[ctx.episode]
}

func batchCoversEpisode(title string, ctx *matchContext) bool {
	var wanted []int
	if len(ctx.candidates) > 0 {
		for n := range ctx.candidates {
			wanted = append(wanted, n)
		}
	} else if ctx.episode >= 0 {
		wanted = []int{ctx.episode}
	}
	for _, mk := range titleEpisodeMarkers(title) {
		if mk[1] <= mk[0] {
			continue
		}
		for _, w := range wanted {
			if w >= mk[0] && w <= mk[1] {
				return true
			}
		}
	}
	return false
}

func matchesAnyEpisode(title string, ctx *matchContext) bool {
	if len(ctx.candidates) > 0 {
		for n := range ctx.candidates {
			if titleHasEpisode(title, n) {
				return true
			}
		}
		return false
	}
	return titleHasEpisode(title, ctx.episode)
}

// classifyResult returns 'A' (exact), 'B' (batch covering the episode), 'C'
// (weak) or 0 (not this show).
func classifyResult(title string, ctx *matchContext) byte {
	if !resultMatchesShow(title, ctx.showTokens, ctx.minHits) {
		return 0
	}
	if len(ctx.showTokens) == 1 && nameIntroducesForeignWord(title, ctx.showTokens) {
		return 0
	}
	if namesSiblingSeason(title, ctx.foreignTokens) || nameMostlyForeign(title, ctx.showTokens) {
		return 0
	}
	if len(ctx.seasonMarks) > 0 && !resultMatchesShow(title, ctx.seasonMarks, 1) && nameIntroducesForeignWord(title, ctx.showTokens) {
		return 0
	}
	seasonOK := usesOffsetEpisode(ctx) || resultMatchesSeason(title, ctx.showSeason, ctx.seasonMarks, ctx.chainSeason)
	yearOK := resultMatchesYear(title, ctx.showYears)
	isBatch := looksLikeBatch(title)
	switch ctx.mode {
	case ModeBatch:
		if seasonOK && yearOK && isBatch {
			return 'A'
		}
		return 'C'
	case ModeMovie:
		if m := seasonSRe.FindStringSubmatch(title); m != nil && atoi(m[1]) >= 1 {
			return 0
		}
		for _, mk := range titleEpisodeMarkers(title) {
			if mk[1] > mk[0] || mk[0] > 1 {
				return 0
			}
		}
		if seasonOK && yearOK {
			return 'A'
		}
		return 'C'
	}
	epOK := ctx.episode < 0 || matchesAnyEpisode(title, ctx)
	if seasonOK && yearOK && epOK {
		if isBatch {
			return 'B'
		}
		return 'A'
	}
	if seasonOK && yearOK && isBatch && batchCoversEpisode(title, ctx) {
		return 'B'
	}
	return 'C'
}

// namesSiblingSeason: the show-name part of the title (before the episode
// marker, so alt names in trailing parentheses don't count) contains a word
// that only a neighbouring season uses.
func namesSiblingSeason(title string, foreign []string) bool {
	if len(foreign) == 0 {
		return false
	}
	name := strings.ToLower(stripLangCodes(showNamePart(title)))
	for _, tok := range foreign {
		if tokenInTitle(tok, name) {
			return true
		}
	}
	return false
}

// nameMostlyForeign (torClient addition): at least two, and at least half,
// of the words in the title's show-name part appear in none of the show's
// AniList titles. Catches a different show that happens to share the
// requested title's few words ("Otome Game no Hametsu Flag ... My Next Life
// as a Villainess" for "No Game No Life") and a later season AniList's
// relations don't link ("Tokyo Revengers: Santen Sensou Hen" for season 1).
// A single stray word ("Journeys" vs AniList's "Journey's") is allowed.
func nameMostlyForeign(title string, showTokens map[string]bool) bool {
	if len(showTokens) == 0 {
		return false
	}
	total, foreign := 0, 0
	for _, tok := range significantTokens(stripLangCodes(showNamePart(title))) {
		if allDigitsRe.MatchString(tok) {
			continue
		}
		total++
		if !showTokens[tok] {
			foreign++
		}
	}
	return foreign >= 2 && 2*foreign >= total
}

func shapeResult(r Result, ctx *matchContext, sourceDefault string) (Result, bool) {
	tier := classifyResult(r.Title, ctx)
	if tier == 0 {
		return r, false
	}
	r.Tier = tier
	r.Accuracy = tagAccuracy(tier, r.Date, sourceDefault)
	if tier == 'B' || looksLikeBatch(r.Title) {
		r.Type = "batch"
	}
	return r, true
}

// candidateWindow: other numberings within a week of the newest exact hit stay.
const candidateWindow = 7 * 24 * time.Hour

// shapeAll classifies items. When the episode has several possible numbers
// (absolute vs per-season), it keeps the numbering(s) whose exact hits are
// newest, recording the choice in ctx.
func shapeAll(items []Result, ctx *matchContext, sourceDefault string) []Result {
	shape := func(c *matchContext) []Result {
		var out []Result
		for _, r := range items {
			if s, ok := shapeResult(r, c, sourceDefault); ok {
				out = append(out, s)
			}
		}
		return out
	}
	exact := shape(ctx.withCandidates(nil))
	if ctx.episode >= 0 {
		ctx.chosenEpisodes = map[int]bool{ctx.episode: true}
	}
	if len(ctx.candidates) <= 1 {
		return exact
	}
	type scored struct {
		newest  time.Time
		shaped  []Result
		episode int
	}
	var all []scored
	for _, n := range sortedKeys(ctx.candidates) {
		shaped := shape(ctx.withCandidates(map[int]bool{n: true}))
		newest, ok := newestOf(shaped)
		if !ok {
			continue
		}
		all = append(all, scored{newest, shaped, n})
	}
	if len(all) == 0 {
		return exact
	}
	best := all[0]
	for _, c := range all {
		if c.newest.After(best.newest) {
			best = c
		}
	}
	var kept []scored
	for _, c := range all {
		if best.newest.Sub(c.newest) <= candidateWindow {
			kept = append(kept, c)
		}
	}
	ctx.chosenEpisodes = map[int]bool{}
	for _, c := range kept {
		ctx.chosenEpisodes[c.episode] = true
	}
	ctx.offsetResolved = !ctx.chosenEpisodes[ctx.episode]
	if len(kept) == 1 {
		return best.shaped
	}
	merged := map[string]Result{}
	var order []string
	for _, c := range kept {
		for _, r := range c.shaped {
			prev, ok := merged[r.Hash]
			if !ok {
				order = append(order, r.Hash)
			}
			if !ok || r.Tier < prev.Tier {
				merged[r.Hash] = r
			}
		}
	}
	out := make([]Result, 0, len(order))
	for _, h := range order {
		out = append(out, merged[h])
	}
	return out
}

func newestOf(rs []Result) (time.Time, bool) {
	var newest time.Time
	found := false
	for _, r := range rs {
		if r.Tier != 'A' {
			continue
		}
		if !found || r.Date.After(newest) {
			newest, found = r.Date, true
		}
	}
	return newest, found
}

func qualityRank(r Result) int {
	switch {
	case r.remake:
		return 2
	case r.trusted:
		return 0
	}
	return 1
}

func sortResults(rs []Result, resolution string) []Result {
	hasExact := false
	for _, r := range rs {
		if r.Tier == 'A' {
			hasExact = true
		}
	}
	out := append([]Result(nil), rs...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if hasExact && a.Tier != b.Tier {
			return a.Tier < b.Tier
		}
		if resolution != "" {
			am, bm := matchesResolution(a.Title, resolution), matchesResolution(b.Title, resolution)
			if am != bm {
				return am
			}
		}
		if !hasExact && !a.Date.Equal(b.Date) {
			return a.Date.After(b.Date)
		}
		if qa, qb := qualityRank(a), qualityRank(b); qa != qb {
			return qa < qb
		}
		if a.Seeders != b.Seeders {
			return a.Seeders > b.Seeders
		}
		return a.Date.After(b.Date)
	})
	return out
}

const justReleased = 7 * 24 * time.Hour

func wantedEpisodes(ctx *matchContext) map[int]bool {
	if ctx.mode != ModeSingle || ctx.episode < 0 {
		return nil
	}
	if ctx.chosenEpisodes != nil {
		return ctx.chosenEpisodes
	}
	return map[int]bool{ctx.episode: true}
}

// finalize filters and orders a source's shaped results (max 30).
func finalize(rs []Result, ctx *matchContext) []Result {
	const limit = 30
	hasExact := false
	for _, r := range rs {
		if r.Tier == 'A' {
			hasExact = true
		}
	}
	var kept []Result
	switch {
	case ctx.notAired:
		// Episode hasn't aired per AniList: only brand-new exact hits count.
		for _, r := range rs {
			if r.Tier == 'A' && time.Since(r.Date) <= justReleased {
				kept = append(kept, r)
			}
		}
		if len(kept) == 0 {
			return nil
		}
	case hasExact:
		for _, r := range rs {
			if r.Tier != 'C' {
				kept = append(kept, r)
			}
		}
	default:
		wanted := wantedEpisodes(ctx)
		showSeason := ctx.showSeason
		if ctx.offsetResolved {
			showSeason = 0
		}
		for _, r := range rs {
			if hasConflictingEpisode(r.Title, wanted) || !resultMatchesSeason(r.Title, showSeason, ctx.seasonMarks, ctx.chainSeason) {
				continue
			}
			r.Accuracy = "low"
			kept = append(kept, r)
		}
	}
	out := sortResults(kept, ctx.resolution)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func dedupe(rs []Result) []Result {
	seen := map[string]bool{}
	var out []Result
	for _, r := range rs {
		if r.Hash == "" || seen[r.Hash] {
			continue
		}
		seen[r.Hash] = true
		out = append(out, r)
	}
	return out
}

func sortedKeys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}
