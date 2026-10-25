package discovery

import (
	"context"
	"errors"
	"html"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Nyaa covers three anitorrent sources that all read nyaa.si's RSS feed:
//   - nyaa      (src/nyaa.js): full-text search, accuracy "medium"
//   - yameii    (src/yameii.js): only the Yameii uploader (English dubs)
//   - toonshub  (src/toonshub.js): only titles starting with [ToonsHub]
type Nyaa struct {
	name          string
	BaseURL       string
	Client        *http.Client
	uploader      string // ?u= filter
	titlePrefix   string // prepended to queries and required in titles
	sourceDefault string
	tagBatch      bool // nyaa.js marks batch-mode results type "batch" up front
	// RetryDelays are the waits after HTTP 429 (Retry-After wins when ≤ 60s).
	RetryDelays []time.Duration
}

const (
	nyaaBase     = "https://nyaa.si"
	nyaaCategory = "1_2" // Anime - English-translated
)

var nyaaRetry = []time.Duration{1200 * time.Millisecond, 3 * time.Second, 6 * time.Second}

func NewNyaa(client *http.Client) *Nyaa {
	return &Nyaa{name: "nyaa", BaseURL: nyaaBase, Client: client, sourceDefault: "medium", tagBatch: true, RetryDelays: nyaaRetry}
}

func NewYameii(client *http.Client) *Nyaa {
	return &Nyaa{name: "yameii", BaseURL: nyaaBase, Client: client, uploader: "Yameii", sourceDefault: "high", RetryDelays: nyaaRetry}
}

func NewToonsHub(client *http.Client) *Nyaa {
	return &Nyaa{name: "toonshub", BaseURL: nyaaBase, Client: client, titlePrefix: "[ToonsHub]", sourceDefault: "high", RetryDelays: nyaaRetry}
}

func (s *Nyaa) Name() string { return s.name }

var errRateLimited = errors.New("nyaa.si is rate limiting requests")

func (s *Nyaa) rss(ctx context.Context, query string) ([]string, error) {
	v := url.Values{"page": {"rss"}, "c": {nyaaCategory}, "s": {"id"}, "o": {"desc"}}
	q := query
	if s.titlePrefix != "" {
		q = strings.TrimSpace(s.titlePrefix + " " + query)
	}
	if q != "" {
		v.Set("q", q)
	}
	if s.uploader != "" {
		v.Set("u", s.uploader)
	}
	for attempt := 0; ; attempt++ {
		body, err := get(ctx, s.Client, "nyaa.si", s.BaseURL+"/?"+v.Encode(), true)
		var he *httpError
		if errors.As(err, &he) && he.status == http.StatusTooManyRequests {
			if attempt >= len(s.RetryDelays) {
				return nil, errRateLimited
			}
			wait := s.RetryDelays[attempt]
			if ra, err := strconv.Atoi(he.retryAfter); err == nil && ra > 0 && ra <= 60 {
				wait = time.Duration(ra) * time.Second
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait + time.Duration(rand.IntN(400))*time.Millisecond):
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		text := string(body)
		if !strings.Contains(text, "<rss") && !strings.Contains(text, "<item>") {
			return nil, errors.New("nyaa.si returned an unexpected response (likely a DDoS-Guard challenge)")
		}
		return pickItems(text), nil
	}
}

func pickItems(xml string) []string {
	var out []string
	for cursor := 0; ; {
		start := strings.Index(xml[cursor:], "<item>")
		if start < 0 {
			break
		}
		start += cursor
		end := strings.Index(xml[start:], "</item>")
		if end < 0 {
			break
		}
		end += start
		out = append(out, xml[start+6:end])
		cursor = end + 7
	}
	return out
}

func pickTag(xml, tag string) string {
	open, closeTag := "<"+tag+">", "</"+tag+">"
	i := strings.Index(xml, open)
	if i < 0 {
		return ""
	}
	j := strings.Index(xml[i+len(open):], closeTag)
	if j < 0 {
		return ""
	}
	val := xml[i+len(open) : i+len(open)+j]
	if strings.HasPrefix(val, "<![CDATA[") && strings.HasSuffix(val, "]]>") {
		val = val[9 : len(val)-3]
	}
	return strings.TrimSpace(html.UnescapeString(val))
}

var sizeRe = regexp.MustCompile(`(?i)([\d.]+)\s*(KiB|MiB|GiB|TiB|KB|MB|GB|TB|B)`)

func parseSize(text string) int64 {
	m := sizeRe.FindStringSubmatch(text)
	if m == nil {
		return 0
	}
	v, _ := strconv.ParseFloat(m[1], 64)
	mult := map[string]float64{
		"b": 1, "kib": 1 << 10, "kb": 1e3, "mib": 1 << 20, "mb": 1e6,
		"gib": 1 << 30, "gb": 1e9, "tib": 1 << 40, "tb": 1e12,
	}[strings.ToLower(m[2])]
	return int64(v*mult + 0.5)
}

func (s *Nyaa) itemToResult(raw string, exclusions []string, batch bool) (Result, bool) {
	title := pickTag(raw, "title")
	hash := lower(pickTag(raw, "nyaa:infoHash"))
	if title == "" || hash == "" {
		return Result{}, false
	}
	if s.titlePrefix != "" && !strings.Contains(title, s.titlePrefix) {
		return Result{}, false
	}
	if hitsExclusion(title, exclusions) {
		return Result{}, false
	}
	atoiTag := func(t string) int { n, _ := strconv.Atoi(pickTag(raw, t)); return n }
	date, ok := parseDate(pickTag(raw, "pubDate"))
	if !ok {
		date = time.Unix(0, 0)
	}
	r := Result{
		Title: title, Link: buildMagnet(hash, title), Hash: hash,
		Seeders: atoiTag("nyaa:seeders"), Leechers: atoiTag("nyaa:leechers"), Downloads: atoiTag("nyaa:downloads"),
		Size: parseSize(pickTag(raw, "nyaa:size")), Date: date, Accuracy: s.sourceDefault, Source: s.name, HasSeeders: true,
		trusted: strings.EqualFold(pickTag(raw, "nyaa:trusted"), "yes"),
		remake:  strings.EqualFold(pickTag(raw, "nyaa:remake"), "yes"),
	}
	if batch && s.tagBatch {
		r.Type = "batch"
	}
	return r, true
}

func (s *Nyaa) Search(ctx context.Context, q Query, mode Mode) ([]Result, error) {
	if len(q.Titles) == 0 {
		return nil, nil
	}
	mctx := searchContext(q, mode)
	qs := buildQueries(q.Titles, 2, q.Episode, q.AnilistID, q.EpisodeCandidates)
	shaped, err := s.collectFeed(ctx, qs, mctx, mode == ModeBatch)
	if err != nil {
		return nil, err
	}
	out := finalize(shaped, mctx)
	if mode == ModeBatch {
		var batches []Result
		for _, r := range out {
			if looksLikeBatch(r.Title) {
				r.Type, r.Accuracy = "batch", "low"
				batches = append(batches, r)
			}
		}
		return batches, nil
	}
	return out, nil
}

// collectFeed (shared.js): all base queries, then numbered ones one at a
// time until an exact episode turns up. Rate limiting stops a phase.
func (s *Nyaa) collectFeed(ctx context.Context, qs queries, mctx *matchContext, batch bool) ([]Result, error) {
	seen := map[string]bool{}
	var collected, shaped []Result
	var lastErr error
	absorb := func(items []string) {
		for _, raw := range items {
			r, ok := s.itemToResult(raw, mctx.exclusions, batch)
			if !ok || seen[r.Hash] {
				continue
			}
			seen[r.Hash] = true
			collected = append(collected, r)
		}
		shaped = shapeAll(collected, mctx, s.sourceDefault)
	}
	phase := func(list []string, stopWhenFound bool) {
		for _, term := range list {
			items, err := s.rss(ctx, term)
			if err != nil {
				lastErr = err
				if errors.Is(err, errRateLimited) || ctx.Err() != nil {
					return
				}
				continue
			}
			absorb(items)
			if stopWhenFound && hasTier(shaped, 'A') {
				return
			}
		}
	}
	phase(qs.bases, false)
	if len(collected) == 0 && lastErr != nil {
		return nil, lastErr
	}
	if len(qs.numbered) > 0 && !hasTier(shaped, 'A') {
		phase(qs.numbered, true)
	}
	if len(collected) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return shaped, nil
}
