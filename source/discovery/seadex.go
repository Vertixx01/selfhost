package discovery

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SeaDex (anitorrent src/seadex.js): curated best/alt releases from
// releases.moe, looked up directly by AniList ID. Mostly batches.
type SeaDex struct {
	BaseURL string
	Client  *http.Client
}

const seadexBase = "https://releases.moe/api/collections/entries/records"

func NewSeaDex(client *http.Client) *SeaDex { return &SeaDex{BaseURL: seadexBase, Client: client} }

func (s *SeaDex) Name() string { return "seadex" }

type seadexFile struct {
	Name   string `json:"name"`
	Length int64  `json:"length"`
}

type seadexTorrent struct {
	InfoHash     string       `json:"infoHash"`
	ReleaseGroup string       `json:"releaseGroup"`
	DualAudio    bool         `json:"dualAudio"`
	IsBest       bool         `json:"isBest"`
	Files        []seadexFile `json:"files"`
	Created      string       `json:"created"`
}

var (
	seadexEpRe    = regexp.MustCompile(`(?i)\s*-?\s*S\d{1,2}E\d{1,3}(?:v\d)?\b`)
	seadexDashRe  = regexp.MustCompile(`\s+-\s+\d{1,4}(?:v\d)?\b`)
	videoExtRe    = regexp.MustCompile(`(?i)\.(mkv|mp4|avi|m2ts|webm)$`)
	collapseSpace = regexp.MustCompile(`\s+`)
)

func cleanReleaseTitle(filename string) string {
	s := seadexEpRe.ReplaceAllString(filename, "")
	s = seadexDashRe.ReplaceAllString(s, "")
	s = videoExtRe.ReplaceAllString(s, "")
	return strings.TrimSpace(collapseSpace.ReplaceAllString(s, " "))
}

func seadexBatch(files []seadexFile) bool {
	videos := 0
	var total int64
	for _, f := range files {
		if videoExtRe.MatchString(f.Name) {
			videos++
		}
		total += f.Length
	}
	return videos > 1 || total > 10<<30
}

// Search ignores the episode: SeaDex lists releases per show. Each result
// says whether it's a multi-file batch.
func (s *SeaDex) Search(ctx context.Context, q Query, mode Mode) ([]Result, error) {
	if q.AnilistID == 0 {
		return nil, nil
	}
	u := s.BaseURL + "?expand=trs&perPage=10&filter=" + url.QueryEscape("(alID="+strconv.Itoa(q.AnilistID)+")")
	var data struct {
		Items []struct {
			Expand struct {
				Trs []seadexTorrent `json:"trs"`
			} `json:"expand"`
		} `json:"items"`
	}
	if err := getJSON(ctx, s.Client, "SeaDex", u, &data); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []Result
	for _, item := range data.Items {
		for _, tr := range item.Expand.Trs {
			hash := lower(tr.InfoHash)
			if hash == "" || seen[hash] {
				continue
			}
			seen[hash] = true
			first := ""
			var size int64
			for _, f := range tr.Files {
				if first == "" && f.Name != "" {
					first = f.Name
				}
				size += f.Length
			}
			name := cleanReleaseTitle(first)
			title := name
			if title == "" {
				title = tr.ReleaseGroup
			}
			if title == "" {
				title = "release"
			}
			if tr.ReleaseGroup != "" {
				title = "[" + tr.ReleaseGroup + "] " + title
			}
			if tr.DualAudio {
				title += " (Dual Audio)"
			}
			batch := seadexBatch(tr.Files)
			typ := "alt"
			switch {
			case tr.IsBest:
				typ = "best"
			case batch:
				typ = "batch"
			}
			date, ok := parseDate(tr.Created)
			if !ok {
				date = time.Now()
			}
			r := Result{
				Title: title, Link: buildMagnet(hash, name), Hash: hash, Size: size, Date: date,
				Accuracy: "high", Type: typ, Source: s.Name(),
			}
			// SeaDex releases are per show, not per episode: only a
			// single-file release for a movie is an exact hit. Everything
			// else is a batch (needs file selection) or not episode-specific.
			switch {
			case batch:
				r.Tier = 'B'
			case mode == ModeMovie:
				r.Tier = 'A'
			default:
				r.Tier = 'C'
			}
			out = append(out, r)
		}
	}
	return out, nil
}

func lower(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
