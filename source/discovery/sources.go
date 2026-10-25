package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Source is one torrent index (anitorrent's per-source modules).
type Source interface {
	Name() string
	Search(ctx context.Context, q Query, mode Mode) ([]Result, error)
}

var trackers = []string{
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://open.stealth.si:80/announce",
	"udp://tracker.torrent.eu.org:451/announce",
	"udp://exodus.desync.com:6969/announce",
	"udp://tracker.coppersurfer.tk:6969/announce",
	"udp://tracker.openbittorrent.com:6969/announce",
	"http://nyaa.tracker.wf:7777/announce",
}

func buildMagnet(hash, name string) string {
	var b strings.Builder
	b.WriteString("magnet:?xt=urn:btih:" + strings.ToLower(hash))
	if name != "" {
		b.WriteString("&dn=" + url.QueryEscape(name))
	}
	for _, t := range trackers {
		b.WriteString("&tr=" + url.QueryEscape(t))
	}
	return b.String()
}

// browserHeaders: nyaa.si sits behind DDoS-Guard, which rejects bare clients.
var browserHeaders = map[string]string{
	"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	"Accept":     "application/xml, text/xml, text/html, application/json, */*",
}

// httpError carries the status so callers can spot rate limiting.
type httpError struct {
	source     string
	status     int
	retryAfter string
}

func (e *httpError) Error() string { return fmt.Sprintf("%s returned HTTP %d", e.source, e.status) }

func get(ctx context.Context, client *http.Client, source, u string, browser bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if browser {
		for k, v := range browserHeaders {
			req.Header.Set(k, v)
		}
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach %s: %w", source, err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return nil, &httpError{source: source, status: res.StatusCode, retryAfter: res.Header.Get("Retry-After")}
	}
	return io.ReadAll(io.LimitReader(res.Body, 32<<20))
}

func getJSON(ctx context.Context, client *http.Client, source, u string, v any) error {
	b, err := get(ctx, client, source, u, false)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s returned an unexpected response: %w", source, err)
	}
	return nil
}

// parseDate accepts the formats the sources use (what JS new Date() parses).
func parseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, layout := range []string{time.RFC1123Z, time.RFC1123, time.RFC3339, "2006-01-02 15:04:05.000Z", "01/02/06"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
