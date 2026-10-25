// Package discovery finds torrents for an AniList ID and episode.
//
// The matching rules and sources are a Go port of anitorrent
// (https://github.com/anh9000/anitorrent, GPL-3.0), the Hayase extension
// pack: src/lib/shared.js and the per-source files. JavaScript lookaheads,
// which Go's RE2 doesn't support, are replaced by explicit checks after the
// match; each spot says so.
package discovery

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var batchPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bbatch\b`),
	regexp.MustCompile(`(?i)\bcomplete\b`),
	regexp.MustCompile(`(?i)\bseason\s*\d+\b`),
	// /\bs\d{1,2}\b(?!\s*e\d)/i — see seasonOnlyMarker
	regexp.MustCompile(`\b\d{1,3}\s*[-~]\s*\d{1,3}\b`),
}

var stopwords = toSet(
	"the", "and", "for", "with", "from", "this", "that", "her", "his", "are", "was", "were",
	"has", "have", "had", "who", "what", "when", "where", "why", "how", "all", "any", "one", "two",
	"season", "episode", "part", "arc", "movie", "film", "ova", "special",
	// Japanese romanization noise (particles, pronouns, honorifics, arc markers).
	"hen", "boku", "ore", "kimi", "sama", "san", "kun", "chan", "suru", "naru", "nani", "desu",
	"dake", "made", "demo", "inai", "koi", "ken", "shi",
	"dan",
)

func toSet(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

var (
	nonWordRe   = regexp.MustCompile(`[^\w\s]`)
	spacesRe    = regexp.MustCompile(`\s+`)
	ordinalRe   = regexp.MustCompile(`^\d+(st|nd|rd|th)$`)
	langCodeRe  = regexp.MustCompile(`\[[A-Z]{2,3}(?:-[A-Z]{2,3})?\]`)
	allDigitsRe = regexp.MustCompile(`^\d+$`)
)

func escapeQuery(s string) string {
	return strings.TrimSpace(spacesRe.ReplaceAllString(nonWordRe.ReplaceAllString(s, " "), " "))
}

func splitWords(s string) []string {
	var out []string
	for _, w := range spacesRe.Split(s, -1) {
		if w != "" {
			out = append(out, w)
		}
	}
	return out
}

func significantTokens(title string) []string {
	var out []string
	for _, t := range spacesRe.Split(strings.ToLower(escapeQuery(title)), -1) {
		if len(t) >= 3 && !stopwords[t] && !ordinalRe.MatchString(t) {
			out = append(out, t)
		}
	}
	return out
}

func buildTitleTokens(titles []string) map[string]bool {
	tokens := map[string]bool{}
	for _, t := range titles {
		for _, tok := range significantTokens(t) {
			tokens[tok] = true
		}
	}
	return tokens
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// tokenInTitle is /\btok\b/.test(lower) for an all-word-character token.
func tokenInTitle(tok, lower string) bool {
	for i := 0; ; {
		j := strings.Index(lower[i:], tok)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(tok)
		if (start == 0 || !isWordByte(lower[start-1])) && (end == len(lower) || !isWordByte(lower[end])) {
			return true
		}
		i = start + 1
	}
}

func stripLangCodes(title string) string { return langCodeRe.ReplaceAllString(title, " ") }

var (
	leadingGroupRe = regexp.MustCompile(`^\s*[\[(【][^\])】]*[\])】]\s*`)
	seMarkerRe     = regexp.MustCompile(`(?i)\bS\d{1,2}E\d{1,4}\b`)
	// /[\s._][-~]\s*\d{1,4}(?:v\d)?(?=[\s[(]|$)/gi — lookahead checked in code.
	nameDashRe     = regexp.MustCompile(`(?i)[\s._][-~]\s*\d{1,4}(?:v\d)?`)
	epWordRe       = regexp.MustCompile(`(?i)\bEP\.?\s*\d{1,4}\b`)
	trailingJunkRe = regexp.MustCompile(`[\s:_-]+$`)
)

// scanFollowed emulates a global JS regex with a trailing lookahead: it
// returns every match whose following text satisfies follow, retrying one
// character after a rejected match. Only for patterns without a leading
// anchor (^ or \b), which would misbehave on a substring.
func scanFollowed(re *regexp.Regexp, s string, follow func(rest string) bool) [][]int {
	var out [][]int
	for pos := 0; pos <= len(s); {
		loc := re.FindStringSubmatchIndex(s[pos:])
		if loc == nil {
			break
		}
		for k := range loc {
			if loc[k] >= 0 {
				loc[k] += pos
			}
		}
		if follow(s[loc[1]:]) {
			out = append(out, loc)
			pos = max(loc[1], loc[0]+1)
		} else {
			pos = loc[0] + 1
		}
	}
	return out
}

func followedBy(chars string) func(string) bool {
	return func(rest string) bool { return rest == "" || strings.ContainsRune(chars, rune(rest[0])) }
}

const jsSpace = " \t\n\r\f\v"

func showNamePart(title string) string {
	t := leadingGroupRe.ReplaceAllString(title, "")
	if loc := seMarkerRe.FindStringIndex(t); loc != nil && loc[0] > 0 {
		t = t[:loc[0]]
	}
	if m := scanFollowed(nameDashRe, t, followedBy(jsSpace+"[(")); len(m) > 0 {
		if last := m[len(m)-1][0]; last > 0 {
			t = t[:last]
		}
	}
	if loc := epWordRe.FindStringIndex(t); loc != nil && loc[0] > 0 {
		t = t[:loc[0]]
	}
	if i := strings.IndexAny(t, "[("); i >= 0 {
		t = t[:i]
	}
	return trailingJunkRe.ReplaceAllString(strings.TrimSpace(t), "")
}

func nameIntroducesForeignWord(title string, tokens map[string]bool) bool {
	for _, tok := range significantTokens(showNamePart(title)) {
		if allDigitsRe.MatchString(tok) {
			continue
		}
		if !tokens[tok] {
			return true
		}
	}
	return false
}

func resultMatchesShow(title string, tokens map[string]bool, minHits int) bool {
	if len(tokens) == 0 {
		return true
	}
	lower := strings.ToLower(stripLangCodes(title))
	hits := 0
	for tok := range tokens {
		if tokenInTitle(tok, lower) {
			hits++
			if hits >= minHits {
				return true
			}
		}
	}
	return false
}

var romanSeason = map[string]int{"II": 2, "III": 3, "IV": 4, "V": 5, "VI": 6, "VII": 7, "VIII": 8, "IX": 9}

var (
	seasonSRe    = regexp.MustCompile(`(?i)\bS(\d{1,2})(?:E\d|\b)`)
	seasonWordRe = regexp.MustCompile(`(?i)\b(?:Season\s+(\d+)|(\d+)(?:st|nd|rd|th)\s+Season)\b`)
	// Lookahead (?=\s|:|\.|-|$|\[|\() consumed instead: only the group is used.
	romanRe      = regexp.MustCompile(`\b[A-Za-z0-9]+\s+(II|III|IV|V|VI|VII|VIII|IX)(?:\s|:|\.|-|$|\[|\()`)
	partBeforeRe = regexp.MustCompile(`\bpart\s+$`)
	dashLetterRe = regexp.MustCompile(`^-[A-Za-z]`)
)

// detectResultSeason returns the season number a title names, or 0.
func detectResultSeason(t string) int {
	if m := seasonSRe.FindStringSubmatch(t); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	if m := seasonWordRe.FindStringSubmatch(t); m != nil {
		n, _ := strconv.Atoi(m[1] + m[2])
		return n
	}
	if m := romanRe.FindStringSubmatch(t); m != nil {
		return romanSeason[m[1]]
	}
	// /\b([2-9])(?=\s*$|\s*[:\-|(\[])/g, written out.
	for i := 0; i < len(t); i++ {
		c := t[i]
		if c < '2' || c > '9' || (i > 0 && isWordByte(t[i-1])) {
			continue
		}
		rest := strings.TrimLeft(t[i+1:], jsSpace)
		if rest != "" && !strings.ContainsRune(":-|([", rune(rest[0])) {
			continue
		}
		if partBeforeRe.MatchString(strings.ToLower(t[max(0, i-8):i])) {
			continue
		}
		if dashLetterRe.MatchString(t[i+1:]) {
			continue
		}
		return int(c - '0')
	}
	return 0
}

func detectShowSeason(titles []string) int {
	best := 0
	for _, t := range titles {
		best = max(best, detectResultSeason(t))
	}
	return max(best, 1)
}

func seasonMarkerTokens(titles []string) map[string]bool {
	beforeColon := func(raw string) (string, string, bool) {
		if i := strings.Index(raw, ":"); i > 0 {
			return raw[:i], raw[i+1:], true
		}
		return raw, "", false
	}
	franchise := map[string]bool{}
	for _, raw := range titles {
		base, _, _ := beforeColon(raw)
		for _, tok := range significantTokens(base) {
			franchise[tok] = true
		}
	}
	marks := map[string]bool{}
	for _, raw := range titles {
		if detectResultSeason(raw) != 0 {
			continue
		}
		base, after, ok := beforeColon(raw)
		if !ok || len(significantTokens(base)) == 0 {
			continue
		}
		for _, tok := range significantTokens(after) {
			if !franchise[tok] {
				marks[tok] = true
			}
		}
	}
	return marks
}

// resultMatchesSeason; showSeason/chainSeason 0 mean "unknown" (JS null).
func resultMatchesSeason(title string, showSeason int, markers map[string]bool, chainSeason int) bool {
	rs := detectResultSeason(title)
	if chainSeason > 1 && rs == chainSeason {
		return true
	}
	if showSeason > 1 {
		if rs == showSeason {
			return true
		}
		if rs != 0 {
			return false
		}
		return len(markers) > 0 && resultMatchesShow(title, markers, 1)
	}
	return rs == 0 || rs == 1
}

var yearAt = regexp.MustCompile(`^(19[3-9]\d|20\d{2})`)

// detectYears is the global YEAR_RE scan: a year preceded by start or one of
// " ._[(-" and followed by end or one of " ._])-".
func detectYears(s string) map[string]bool {
	years := map[string]bool{}
	for i := 0; i+4 <= len(s); i++ {
		if i > 0 && !strings.ContainsRune(jsSpace+"._[(-", rune(s[i-1])) {
			continue
		}
		m := yearAt.FindString(s[i:])
		if m == "" {
			continue
		}
		if end := i + 4; end < len(s) && !strings.ContainsRune(jsSpace+"._])-", rune(s[end])) {
			continue
		}
		years[m] = true
	}
	return years
}

func detectShowYears(titles []string) map[string]bool {
	years := map[string]bool{}
	for _, t := range titles {
		for y := range detectYears(t) {
			years[y] = true
		}
	}
	return years
}

func resultMatchesYear(title string, showYears map[string]bool) bool {
	if len(showYears) == 0 {
		return true
	}
	ry := detectYears(title)
	if len(ry) == 0 {
		return true
	}
	for y := range ry {
		if showYears[y] {
			return true
		}
	}
	return false
}

// titleHasEpisode reports whether title names episode ep (ep < 0: any).
func titleHasEpisode(title string, ep int) bool {
	if ep < 0 {
		return true
	}
	n := strconv.Itoa(ep)
	pats := episodeRegexps(n)
	for _, re := range pats {
		if re.MatchString(title) {
			return true
		}
	}
	return false
}

// Sources run concurrently, so the cache is guarded.
var (
	episodeReMu    sync.Mutex
	episodeReCache = map[string][]*regexp.Regexp{}
)

func episodeRegexps(n string) []*regexp.Regexp {
	episodeReMu.Lock()
	defer episodeReMu.Unlock()
	if p, ok := episodeReCache[n]; ok {
		return p
	}
	p := []*regexp.Regexp{
		// \b(?!\d) after a digit is just \b.
		regexp.MustCompile(`(?i)\b(?:e|ep|episode\s*|s\d{1,2}e)0*` + n + `\b`),
		// Lookahead (?=[\s\[\(]|$) consumed: only used as a boolean test.
		regexp.MustCompile(`(?i)[\s._][-~]\s+0*` + n + `(?:v\d)?(?:[\s\[\(]|$)`),
		regexp.MustCompile(`(?i)[\[\(]0*` + n + `(?:v\d)?[\]\)]`),
	}
	episodeReCache[n] = p
	return p
}

var (
	batchRangeSERe = regexp.MustCompile(`(?i)\bs\d{1,2}e\d{1,3}\s*[-~]\s*(?:s\d{1,2})?e?\d{1,3}\b`)
	singleSERe     = regexp.MustCompile(`(?i)\bs\d{1,2}e\d{1,3}\b`)
	singleDashRe   = regexp.MustCompile(`\s-\s*\d{1,4}(?:v\d)?\s*(?:\[|\(|$)`)
	seasonOnlyRe   = regexp.MustCompile(`(?i)\bs\d{1,2}\b`)
	episodeNextRe  = regexp.MustCompile(`(?i)^\s*e\d`)
)

// seasonOnlyMarker is /\bs\d{1,2}\b(?!\s*e\d)/i: "S2" not followed by an episode.
func seasonOnlyMarker(title string) bool {
	for _, loc := range seasonOnlyRe.FindAllStringIndex(title, -1) {
		if !episodeNextRe.MatchString(title[loc[1]:]) {
			return true
		}
	}
	return false
}

func looksLikeBatch(title string) bool {
	if batchRangeSERe.MatchString(title) {
		return true
	}
	if singleSERe.MatchString(title) || singleDashRe.MatchString(title) {
		return false
	}
	if seasonOnlyMarker(title) {
		return true
	}
	for _, re := range batchPatterns {
		if re.MatchString(title) {
			return true
		}
	}
	return false
}

func tagAccuracy(tier byte, date time.Time, sourceDefault string) string {
	switch tier {
	case 'A':
		return sourceDefault
	case 'B':
		return "low"
	}
	days := time.Since(date).Hours() / 24
	switch {
	case days < 60:
		return sourceDefault
	case days < 180:
		return "medium"
	}
	return "low"
}

// titleAliases rescue shows whose AniList titles don't match release names.
var titleAliases = map[int][]string{
	185874: {"bleach sennen kessen", "bleach thousand year blood war"},
	108632: {"re zero kara hajimeru", "rezero starting life"},
	21:     {"one piece"},
	235:    {"detective conan", "meitantei conan"},
}

type queries struct{ bases, numbered []string }

func buildQueries(titles []string, limit int, episode int, anilistID int, candidates []int) queries {
	if limit <= 0 {
		limit = 3
	}
	var q queries
	seen := map[string]bool{}
	for _, title := range rankTitlesForQuery(titles) {
		if len(q.bases) >= limit {
			break
		}
		s := trimTitleForQuery(title)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		q.bases = append(q.bases, s)
	}
	var rescue []string
	for _, a := range titleAliases[anilistID] {
		a = strings.TrimSpace(a)
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		rescue = append(rescue, a)
	}
	eps := candidates
	if len(eps) == 0 && episode >= 0 {
		eps = []int{episode}
	}
	withEp := func(s string) []string {
		var out []string
		for _, n := range eps {
			out = append(out, s+" "+pad(n))
		}
		return out
	}
	for _, b := range q.bases {
		q.numbered = append(q.numbered, withEp(b)...)
	}
	q.numbered = append(q.numbered, rescue...)
	for _, r := range rescue {
		q.numbered = append(q.numbered, withEp(r)...)
	}
	return q
}

func pad(n int) string {
	if n >= 0 && n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

var genericQueryWords = toSet("monster", "level", "hero", "world", "girl", "boy", "demon", "devil",
	"dragon", "angel", "king", "queen", "story", "magic", "school", "love", "life", "club", "sword",
	"blood", "dark", "light", "night", "master", "star", "moon", "witch", "ghost", "dead", "zombie", "idol")

var trailingYearRe = regexp.MustCompile(`\s*[(\[]\s*(?:19|20)\d{2}\s*[)\]]\s*$`)

func trimTitleForQuery(title string) string {
	stripped := strings.TrimSpace(trailingYearRe.ReplaceAllString(title, ""))
	raw := stripped
	if raw == "" {
		raw = title
	}
	base := raw
	if i := strings.Index(raw, ":"); i > 0 {
		base = raw[:i]
	}
	if toks := significantTokens(base); len(toks) > 0 {
		return strings.Join(toks[:min(4, len(toks))], " ")
	}
	baseWords := splitWords(escapeQuery(base))
	usePreColon := len(baseWords) >= 2
	source := baseWords
	if !usePreColon {
		source = splitWords(escapeQuery(raw))
	}
	trimmed := append([]string(nil), source[:min(4, len(source))]...)
	if !usePreColon {
		for len(trimmed) > 1 && stopwords[strings.ToLower(trimmed[len(trimmed)-1])] {
			trimmed = trimmed[:len(trimmed)-1]
		}
	}
	if w := strings.Join(trimmed, " "); w != "" {
		return w
	}
	return escapeQuery(raw)
}

// rankTitlesForQuery prefers mostly-Latin titles, and demotes titles whose
// query collapses to one short or generic word.
func rankTitlesForQuery(titles []string) []string {
	type ranked struct {
		t          string
		i          int
		degenerate bool
		asciiRatio float64
	}
	var list []ranked
	for i, t := range titles {
		if strings.TrimSpace(t) == "" || len(significantTokens(t)) == 0 {
			continue
		}
		stripped := utf8.RuneCountInString(strings.Join(strings.Fields(t), ""))
		ascii := len(strings.Join(strings.Fields(escapeQuery(t)), ""))
		qt := splitWords(trimTitleForQuery(t))
		first := ""
		if len(qt) > 0 {
			first = qt[0]
		}
		r := ranked{t: t, i: i, degenerate: len(qt) <= 1 && (len(first) < 4 || genericQueryWords[first])}
		if stripped > 0 {
			r.asciiRatio = float64(ascii) / float64(stripped)
		}
		list = append(list, r)
	}
	var pool []ranked
	for _, r := range list {
		if r.asciiRatio >= 0.5 {
			pool = append(pool, r)
		}
	}
	if len(pool) == 0 {
		pool = list
	}
	sort.SliceStable(pool, func(a, b int) bool {
		if pool[a].degenerate != pool[b].degenerate {
			return !pool[a].degenerate
		}
		return pool[a].i < pool[b].i
	})
	out := make([]string, len(pool))
	for i, r := range pool {
		out[i] = r.t
	}
	return out
}

var (
	markerSERe    = regexp.MustCompile(`(?i)\bs\d{1,2}e(\d{1,4})(?:\s*[-~]\s*(?:s\d{1,2})?e(\d{1,4}))?\b`)
	markerEpRe    = regexp.MustCompile(`(?i)\bep(?:isode)?\.?\s*(\d{1,4})\b`)
	markerDashRe  = regexp.MustCompile(`[\s._]-\s*(\d{1,4})(?:v\d)?\s*`) // + (?=[[(]|$)
	markerRangeRe = regexp.MustCompile(`\b(\d{1,4})\s*[-~]\s*(\d{1,4})\b`)
)

// titleEpisodeMarkers lists the episode ranges a title names.
func titleEpisodeMarkers(title string) [][2]int {
	var out [][2]int
	push := func(a, b string) {
		lo, err := strconv.Atoi(a)
		if err != nil {
			return
		}
		hi := lo
		if b != "" {
			if h, err := strconv.Atoi(b); err == nil {
				hi = h
			}
		}
		out = append(out, [2]int{lo, hi})
	}
	group := func(m []int, k int) string {
		if m[2*k] < 0 {
			return ""
		}
		return title[m[2*k]:m[2*k+1]]
	}
	for _, m := range markerSERe.FindAllStringSubmatchIndex(title, -1) {
		push(group(m, 1), group(m, 2))
	}
	for _, m := range markerEpRe.FindAllStringSubmatchIndex(title, -1) {
		push(group(m, 1), "")
	}
	for _, m := range scanFollowed(markerDashRe, title, followedBy("[(")) {
		push(group(m, 1), "")
	}
	for _, m := range markerRangeRe.FindAllStringSubmatchIndex(title, -1) {
		a, _ := strconv.Atoi(group(m, 1))
		b, _ := strconv.Atoi(group(m, 2))
		if b > a && b-a < 400 {
			push(group(m, 1), group(m, 2))
		}
	}
	return out
}

func hasConflictingEpisode(title string, wanted map[int]bool) bool {
	if len(wanted) == 0 {
		return false
	}
	markers := titleEpisodeMarkers(title)
	if len(markers) == 0 {
		return false
	}
	for _, mk := range markers {
		for w := range wanted {
			if w >= mk[0] && w <= mk[1] {
				return false
			}
		}
	}
	return true
}

func matchesResolution(title, res string) bool {
	return res == "" || strings.Contains(title, res+"p") || strings.Contains(title, res)
}

func hitsExclusion(title string, exclusions []string) bool {
	lower := strings.ToLower(title)
	for _, kw := range exclusions {
		if kw != "" && strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}
