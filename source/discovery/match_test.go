package discovery

import (
	"slices"
	"sort"
	"testing"
	"time"
)

func TestShowNamePart(t *testing.T) {
	for in, want := range map[string]string{
		"[SubsPlease] Sousou no Frieren - 05 (1080p) [8E3F8FA5].mkv": "Sousou no Frieren",
		"Frieren.Beyond.Journeys.End.S01E05.1080p.WEBRip-Arg0.mkv":   "Frieren.Beyond.Journeys.End.",
		"[Grp] Show - 01 - 02 [1080p]":                               "Show - 01", // last dash marker wins (overlapping matches)
		"【Group】 Show EP 12 [1080p]":                                 "Show",
		"[Grp] Show: The Sequel":                                     "Show: The Sequel",
	} {
		if got := showNamePart(in); got != want {
			t.Errorf("showNamePart(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDetectResultSeason(t *testing.T) {
	for in, want := range map[string]int{
		"Show S2 - 05":           2,
		"Show S02E05":            2,
		"Show 2nd Season - 05":   2,
		"Show Season 3":          3,
		"Show II - 05":           2,
		"Show VIII: Finale":      8,
		"Show 2 - 05":            2,
		"Show 3":                 3,
		"Show Part 2":            0, // "part" before the digit
		"Show 2-Kakumei":         0, // digit glued to a word by a dash
		"Show - 05 [1080p]":      0,
		"[SubsPlease] Show - 05": 0,
		"Show 2 (2024) [1080p]":  2,
	} {
		if got := detectResultSeason(in); got != want {
			t.Errorf("detectResultSeason(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestDetectYears(t *testing.T) {
	got := detectYears("Show (2019-2020) [1080p] 20191 x2021")
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !slices.Equal(keys, []string{"2019", "2020"}) {
		t.Errorf("years = %v", keys)
	}
	if !resultMatchesYear("Show (2023)", map[string]bool{"2023": true}) || resultMatchesYear("Show (2019)", map[string]bool{"2023": true}) {
		t.Error("year matching")
	}
}

func TestTitleHasEpisode(t *testing.T) {
	cases := []struct {
		title string
		ep    int
		want  bool
	}{
		{"[SubsPlease] Show - 05 (1080p)", 5, true},
		{"Show - 05v2 [1080p]", 5, true},
		{"Show - 15 (1080p)", 5, false},
		{"Show - 105", 5, false},
		{"Show S01E05 1080p", 5, true},
		{"Show EP05 1080p", 5, true},
		{"Show E05v2", 5, false}, // \b fails before "v": matches the JS
		{"Show [05]", 5, true},
		{"Show [1080p]", 5, false},
		{"anything", -1, true},
	}
	for _, c := range cases {
		if got := titleHasEpisode(c.title, c.ep); got != c.want {
			t.Errorf("titleHasEpisode(%q, %d) = %v", c.title, c.ep, got)
		}
	}
}

func TestLooksLikeBatch(t *testing.T) {
	for in, want := range map[string]bool{
		"Show S01E01-E12 [1080p]":             true,
		"Show S01E01-S01E12":                  true,
		"Show S2 [1080p]":                     true, // season marker, no episode
		"Show S2E05 [1080p]":                  false,
		"Show S2 E05":                         false, // (?!\s*e\d)
		"[SubsPlease] Show - 05 (1080p)":      false,
		"Show (01-12) [BD 1080p]":             true,
		"Show Complete Series":                true,
		"Show Batch":                          true,
		"Show Season 2 [1080p]":               true,
		"[Erai-raws] Show - 05 [1080p][HEVC]": false,
	} {
		if got := looksLikeBatch(in); got != want {
			t.Errorf("looksLikeBatch(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestTitleEpisodeMarkers(t *testing.T) {
	has := func(title string, want [2]int) {
		t.Helper()
		if !slices.Contains(titleEpisodeMarkers(title), want) {
			t.Errorf("markers(%q) = %v, want %v among them", title, titleEpisodeMarkers(title), want)
		}
	}
	has("Show - 05 [1080p]", [2]int{5, 5})
	has("Show S01E01-E12", [2]int{1, 12})
	has("Show Episode 7", [2]int{7, 7})
	has("Show 01 ~ 13 [BD]", [2]int{1, 13})
	if hasConflictingEpisode("Show - 05 [1080p]", map[int]bool{5: true}) || !hasConflictingEpisode("Show - 06 [1080p]", map[int]bool{5: true}) {
		t.Error("conflict detection")
	}
}

func TestQueries(t *testing.T) {
	if got := trimTitleForQuery("Sousou no Frieren"); got != "sousou frieren" {
		t.Errorf("trim = %q", got)
	}
	if got := trimTitleForQuery("Frieren: Beyond Journey's End"); got != "frieren" {
		t.Errorf("trim colon = %q", got)
	}
	// Romaji collapses to a generic word, so the English title is queried first.
	ranked := rankTitlesForQuery([]string{"Ore dake Level Up na Ken", "Solo Leveling", "俺だけレベルアップな件"})
	if !slices.Equal(ranked, []string{"Solo Leveling", "Ore dake Level Up na Ken"}) {
		t.Errorf("ranked = %v", ranked)
	}
	q := buildQueries([]string{"Sousou no Frieren", "Frieren: Beyond Journey's End"}, 2, 5, 154587, nil)
	if !slices.Equal(q.bases, []string{"sousou frieren", "frieren"}) || !slices.Equal(q.numbered, []string{"sousou frieren 05", "frieren 05"}) {
		t.Errorf("queries %+v", q)
	}
	q = buildQueries([]string{"One Piece"}, 2, 1180, 21, nil)
	if !slices.Contains(q.numbered, "one piece 1180") {
		t.Errorf("alias/numbered %+v", q)
	}
}

func TestClassify(t *testing.T) {
	q := Query{AnilistID: 154587, Titles: []string{"Sousou no Frieren", "Frieren: Beyond Journey's End"}, Episode: 5}
	ctx := searchContext(q, ModeSingle)
	for title, want := range map[string]byte{
		"[SubsPlease] Sousou no Frieren - 05 (1080p) [8E3F8FA5].mkv":      'A',
		"[EMBER] Sousou no Frieren S01E05 [1080p]":                        'A',
		"[EMBER] Frieren: Beyond Journey's End S02E05 [1080p] (Season 2)": 'C', // other season
		"[SubsPlease] Sousou no Frieren - 06 (1080p)":                     'C',
		"[Grp] Sousou no Frieren S01E01-E28 [BD 1080p]":                   'B', // batch covering it
		"[SubsPlease] Dandadan - 05 (1080p)":                              0,   // other show
	} {
		if got := classifyResult(title, ctx); got != want {
			t.Errorf("classify(%q) = %q, want %q", title, got, want)
		}
	}

	// Absolute numbering: season 2 episode 5 released as 33.
	q2 := Query{Titles: []string{"Sousou no Frieren 2nd Season"}, Episode: 5, EpisodeCandidates: []int{5, 33}}
	ctx2 := searchContext(q2, ModeSingle)
	now := time.Now()
	shaped := shapeAll([]Result{
		{Title: "[Grp] Sousou no Frieren - 33 [1080p]", Hash: "a", Date: now},
		{Title: "[Grp] Sousou no Frieren - 05 [1080p]", Hash: "b", Date: now.Add(-700 * 24 * time.Hour)},
	}, ctx2, "medium")
	if !ctx2.chosenEpisodes[33] || ctx2.chosenEpisodes[5] || !ctx2.offsetResolved {
		t.Errorf("chosen %v offsetResolved %v", ctx2.chosenEpisodes, ctx2.offsetResolved)
	}
	if len(shaped) == 0 || shaped[0].Hash != "a" || shaped[0].Tier != 'A' {
		t.Errorf("shaped %+v", shaped)
	}

	movie := searchContext(Query{Titles: []string{"Suzume no Tojimari"}, StartYear: 2022, Episode: -1}, ModeMovie)
	if classifyResult("[EMBER] Suzume no Tojimari (2022) (Movie) [1080p]", movie) != 'A' {
		t.Error("movie not matched")
	}
	if classifyResult("[Grp] Suzume no Tojimari - 03 [1080p]", movie) != 0 {
		t.Error("an episode numbered release matched a movie")
	}
	if classifyResult("[Grp] Suzume no Tojimari (2019) [1080p]", movie) != 'C' {
		t.Error("wrong-year movie not demoted")
	}
}

func TestFinalizeNotAired(t *testing.T) {
	ctx := searchContext(Query{Titles: []string{"One Piece"}, Episode: 1181, NotAired: true}, ModeSingle)
	old := Result{Title: "[SubsPlease] One Piece - 1181 (1080p)", Hash: "a", Date: time.Now().Add(-30 * 24 * time.Hour), Tier: 'A'}
	fresh := Result{Title: "[SubsPlease] One Piece - 1181 (1080p)", Hash: "b", Date: time.Now().Add(-time.Hour), Tier: 'A'}
	if got := finalize([]Result{old}, ctx); len(got) != 0 {
		t.Errorf("stale hit kept for unaired episode: %+v", got)
	}
	if got := finalize([]Result{old, fresh}, ctx); len(got) != 1 || got[0].Hash != "b" {
		t.Errorf("fresh hit dropped: %+v", got)
	}
}

func TestRank(t *testing.T) {
	now := time.Now()
	mk := func(title, src, acc string, seeders int, tier byte) Result {
		return Result{Title: title, Hash: title, Source: src, Accuracy: acc, Seeders: seeders, HasSeeders: true, Date: now, Tier: tier}
	}
	cands := merge([]Result{
		mk("[ToonsHub] Show - 05 1080p", "toonshub", "high", 8, 'A'),
		mk("[SubsPlease] Show - 05 (1080p)", "nyaa", "medium", 120, 'A'),
		mk("[SubsPlease] Show - 05 (720p)", "nyaa", "medium", 500, 'A'),
		mk("[Yameii] Show - 05 [English Dub] 1080p", "yameii", "high", 900, 'A'),
		mk("[Grp] Show 01-12 [1080p]", "nyaa", "medium", 999, 'B'),
		mk("[Dead] Show - 05 1080p", "nyaa", "medium", 0, 'A'),
	})
	cands[5].Date = now.Add(-30 * 24 * time.Hour)
	got := rank(cands, Prefs{Resolution: "1080"}, false)
	order := func() []string {
		var o []string
		for _, c := range got {
			o = append(o, c.Title)
		}
		return o
	}
	want := []string{
		"[SubsPlease] Show - 05 (1080p)",         // 1080p, 100+ seeders
		"[ToonsHub] Show - 05 1080p",             // 1080p, fewer seeders despite higher accuracy
		"[Yameii] Show - 05 [English Dub] 1080p", // dub last within 1080p
		"[SubsPlease] Show - 05 (720p)",          // lower resolution
		"[Grp] Show 01-12 [1080p]",               // not pickable: batch
		"[Dead] Show - 05 1080p",                 // not pickable: dead
	}
	if !slices.Equal(order(), want) {
		t.Errorf("order:\n%v\nwant\n%v", order(), want)
	}
	if got[4].Pickable || got[4].Why == "" || got[5].Pickable {
		t.Error("batch/dead marked pickable")
	}
	got = rank(cands, Prefs{Resolution: "1080"}, true)
	if got[0].Source != "yameii" {
		t.Errorf("dub preference ignored: %s", got[0].Title)
	}
	got = rank(cands, Prefs{Resolution: "1080", PreferredGroups: []string{"ToonsHub"}}, false)
	if got[0].Source != "toonshub" {
		t.Errorf("preferred group ignored: %s", got[0].Title)
	}
}

func TestMergeAcrossSources(t *testing.T) {
	cs := merge([]Result{
		{Title: "[SubsPlease] Show - 05 (1080p)", Hash: "h", Source: "subsplease", Accuracy: "high", Tier: 'A'},
		{Title: "[SubsPlease] Show - 05 (1080p) [ABCD].mkv", Hash: "h", Source: "nyaa", Accuracy: "medium", Seeders: 50, HasSeeders: true, Tier: 'A'},
	})
	if len(cs) != 1 || cs[0].Seeders != 50 || cs[0].Accuracy != "high" || !slices.Equal(cs[0].Sources, []string{"subsplease", "nyaa"}) {
		t.Errorf("merged %+v", cs)
	}
}

func TestBase32Magnet(t *testing.T) {
	// 32-char base32 btih (SubsPlease sometimes) → 40-char hex.
	hash, size := parseMagnet("magnet:?xt=urn:btih:MFRGGZDFMZTWQ2LKNNWG23TPOBYXE43U&xl=1234&dn=x")
	if hash != "6162636465666768696a6b6c6d6e6f7071727374" || size != 1234 {
		t.Errorf("hash %q size %d", hash, size)
	}
}

// Split-cour sequels restart episode numbers under the same franchise name:
// part 3's "- 03" must not match part 4 episode 3 (torClient addition).
func TestSiblingSeasonRejected(t *testing.T) {
	titles := []string{"BLEACH: Sennen Kessen-hen - Kashin-tan", "BLEACH: Thousand-Year Blood War - The Calamity"}
	c := chain{ok: true, counts: []prequel{
		{episodes: 14, tokens: buildTitleTokens([]string{"BLEACH: Sennen Kessen-hen - Soukoku-tan", "BLEACH: Thousand-Year Blood War - The Conflict"})},
		{episodes: 13, tokens: buildTitleTokens([]string{"BLEACH: Sennen Kessen-hen - Ketsubetsu-tan", "BLEACH: Thousand-Year Blood War - The Separation"})},
		{episodes: 13, tokens: buildTitleTokens([]string{"BLEACH: Sennen Kessen-hen", "BLEACH: Thousand-Year Blood War"})},
	}}
	foreign := foreignTokens(c, buildTitleTokens(titles))
	if !slices.Equal(foreign, []string{"conflict", "ketsubetsu", "separation", "soukoku"}) {
		t.Fatalf("foreign tokens %v", foreign)
	}
	ctx := searchContext(Query{Titles: titles, Episode: 3, ForeignTokens: foreign, EpisodeCandidates: []int{3, 43}}, ModeSingle)
	for title, want := range map[string]byte{
		"[Erai-raws] Bleach: Sennen Kessen Hen - Soukoku Tan - 03 [1080p][HEVC]":          0,
		"[Erai-raws] Bleach - Sennen Kessen Hen - Ketsubetsu Tan - 03 [1080p]":            0,
		"[Erai-raws] Bleach: Sennen Kessen Hen - Kashin Tan - 03 [1080p DSNP WEBRip]":     'A',
		"[ToonsHub] BLEACH Thousand-Year Blood War S01E43 1080p DSNP WEB-DL (Multi-Subs)": 'A',
		// Alt names in trailing parentheses don't count as the show name.
		"[Grp] Bleach Sennen Kessen Hen Kashin Tan - 03 (prev: Soukoku Tan) [1080p]": 'A',
	} {
		if got := classifyResult(title, ctx); got != want {
			t.Errorf("classify(%q) = %q, want %q", title, got, want)
		}
	}
	// Words the requested show shares with its prequels are never foreign.
	if f := foreignTokens(chain{ok: true, counts: []prequel{{episodes: 28, tokens: buildTitleTokens([]string{"Sousou no Frieren"})}}},
		buildTitleTokens([]string{"Sousou no Frieren 2nd Season"})); len(f) != 0 {
		t.Errorf("Frieren S2 got foreign tokens %v", f)
	}
}

// Wrong picks seen in real use (torClient additions on top of the port).
func TestMostlyForeignNameRejected(t *testing.T) {
	ngnl := searchContext(Query{Titles: []string{"No Game No Life", "No Game, No Life", "NGNL"}, Episode: 1}, ModeSingle)
	tr := searchContext(Query{Titles: []string{"Tokyo Revengers"}, Episode: 1}, ModeSingle)
	frieren := searchContext(Query{Titles: []string{"Sousou no Frieren", "Frieren: Beyond Journey’s End"}, Episode: 5}, ModeSingle)
	for _, c := range []struct {
		ctx   *matchContext
		title string
		want  byte
	}{
		{ngnl, "[Commie] Otome Game no Hametsu Flag shika Nai Akuyaku Reijou ni Tensei shiteshimatta - My Next Life as a Villainess - 01 [BD 1080p AAC]", 0},
		{ngnl, "[HorribleSubs] No Game No Life - 01 [1080p].mkv", 'A'},
		{tr, "[Erai-raws] Tokyo Revengers: Santen Sensou Hen - 01 [1080p DSNP WEB-DL AVC AAC][MultiSub]", 0},
		{tr, "[SubsPlease] Tokyo Revengers - 01 (1080p)", 'A'},
		// One stray word (Journeys vs Journey's) is fine.
		{frieren, "Frieren.Beyond.Journeys.End.S01E05.1080p.WEBRip.DDP2.0.x265-Arg0.mkv", 'A'},
	} {
		if got := classifyResult(c.title, c.ctx); got != c.want {
			t.Errorf("classify(%q) = %q, want %q", c.title, got, c.want)
		}
	}
}
