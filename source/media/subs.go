package media

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ErrOnlyBitmapSubs: the episode has subtitles and every track is bitmap
// (PGS etc.), so it would end up with none (PROJECT.MD Stage 4).
var ErrOnlyBitmapSubs = errors.New("all subtitle tracks are bitmap (PGS/VobSub); none can be used for HLS")

// SubtitleResult is subs.json.
type SubtitleResult struct {
	Tracks   []SubtitleTrack `json:"tracks"`
	Skipped  []SkippedTrack  `json:"skipped,omitempty"`
	Warnings []string        `json:"warnings,omitempty"`
}

type SubtitleTrack struct {
	Index    int     `json:"index"`
	Lang     string  `json:"lang"`
	Title    string  `json:"title,omitempty"`
	Codec    string  `json:"codec"`
	Format   string  `json:"format"` // "ass" | "vtt"
	Kind     string  `json:"kind"`   // "dialogue" | "signs"
	Forced   bool    `json:"forced"`
	Default  bool    `json:"default"`
	Coverage float64 `json:"coverage"` // share of the episode with a subtitle on screen
	File     string  `json:"file"`     // filename inside the subs dir
}

// signsCoverage: a track on screen for less than this share of the episode is
// signs/songs, not dialogue (PROJECT.MD Stage 4).
const signsCoverage = 0.30

var utf8BOM = string(rune(0xFEFF))

var signsTitleRe = regexp.MustCompile(`(?i)\bsigns?\b|\bsongs?\b|s\s*&\s*s|forced`)

// ExtractSubtitles writes every usable subtitle track to outDir as
// {lang}.ass / {lang}.vtt ({lang}_dialogue / {lang}_signs when one language
// has several tracks). ASS and WebVTT are copied; other text formats are
// converted to WebVTT; bitmap tracks are skipped. A track that can't be
// extracted is skipped with a warning — only "every track is bitmap" fails.
func (t Tools) ExtractSubtitles(ctx context.Context, input string, sm *StreamMap, outDir string) (*SubtitleResult, error) {
	res := &SubtitleResult{Tracks: []SubtitleTrack{}}
	var text []SubtitleStream
	for _, s := range sm.Subtitles {
		if s.Bitmap {
			res.Skipped = append(res.Skipped, SkippedTrack{s.Index, "bitmap subtitle (" + s.Codec + ")"})
		} else {
			text = append(text, s)
		}
	}
	if len(sm.Subtitles) > 0 && len(text) == 0 {
		return nil, ErrOnlyBitmapSubs
	}
	if len(text) == 0 {
		return res, nil
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}

	// Extract under temp names first (s{index}.ext); final names depend on
	// how many tracks each language has.
	tmpName := func(s SubtitleStream) string { return fmt.Sprintf(".s%d.%s", s.Index, subFormat(s.Codec)) }

	// One pass over the file for all tracks; only if that fails, go track by
	// track so one broken track doesn't lose the rest.
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-copyts", "-i", input}
	for _, s := range text {
		args = append(args, subArgs(s, filepath.Join(outDir, tmpName(s)))...)
	}
	extracted := map[int]bool{}
	if _, err := run(ctx, t.FFmpeg, args...); err == nil {
		for _, s := range text {
			extracted[s.Index] = true
		}
	} else if ctx.Err() != nil {
		return nil, ctx.Err()
	} else {
		for _, s := range text {
			out := filepath.Join(outDir, tmpName(s))
			one := append([]string{"-hide_banner", "-loglevel", "error", "-y", "-copyts", "-i", input}, subArgs(s, out)...)
			if _, err := run(ctx, t.FFmpeg, one...); err == nil {
				extracted[s.Index] = true
			} else if t.mkvextractSub(ctx, input, s, out) == nil {
				extracted[s.Index] = true
			} else {
				res.Warnings = append(res.Warnings, fmt.Sprintf("subtitle stream %d (%s): extraction failed, skipped: %v", s.Index, s.Codec, err))
				res.Skipped = append(res.Skipped, SkippedTrack{s.Index, "extraction failed"})
			}
		}
	}

	var tracks []SubtitleTrack
	for _, s := range text {
		if !extracted[s.Index] {
			continue
		}
		path := filepath.Join(outDir, tmpName(s))
		cov, err := coverage(path, subFormat(s.Codec), sm.Duration)
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("subtitle stream %d: can't measure coverage: %v", s.Index, err))
		}
		tracks = append(tracks, SubtitleTrack{
			Index: s.Index, Lang: s.Language, Title: s.Title, Codec: s.Codec,
			Format: subFormat(s.Codec), Forced: s.Forced, Default: s.Default, Coverage: cov,
		})
	}

	byLang := map[string][]int{}
	for i, tr := range tracks {
		byLang[tr.Lang] = append(byLang[tr.Lang], i)
	}
	for lang, idxs := range byLang {
		for _, i := range idxs {
			tr := &tracks[i]
			tr.Kind = "dialogue"
			if signsTitleRe.MatchString(tr.Title) || (len(idxs) > 1 && tr.Coverage < signsCoverage) {
				tr.Kind = "signs"
			}
		}
		counts := map[string]int{}
		for _, i := range idxs {
			tr := &tracks[i]
			base := lang
			if len(idxs) > 1 {
				base = lang + "_" + tr.Kind
			}
			counts[base]++
			if n := counts[base]; n > 1 {
				base = fmt.Sprintf("%s_%d", base, n)
			}
			tr.File = base + "." + tr.Format
			if err := os.Rename(filepath.Join(outDir, tmpName(SubtitleStream{Index: tr.Index, Codec: tr.Codec})), filepath.Join(outDir, tr.File)); err != nil {
				return nil, err
			}
		}
	}
	sort.Slice(tracks, func(a, b int) bool { return tracks[a].Index < tracks[b].Index })
	res.Tracks = append(res.Tracks, tracks...)
	return res, nil
}

func subFormat(codec string) string {
	if codec == "ass" || codec == "ssa" {
		return "ass"
	}
	return "vtt"
}

func subArgs(s SubtitleStream, out string) []string {
	codec := "webvtt"
	if f := subFormat(s.Codec); f == "ass" || s.Codec == "webvtt" {
		codec = "copy"
	}
	return []string{"-map", "0:" + strconv.Itoa(s.Index), "-c:s", codec, out}
}

// mkvextractSub is the fallback for tracks ffmpeg can't read (e.g. some
// encrypted/odd muxes). mkvextract track IDs follow Matroska track order,
// which matches ffprobe's stream index for MKV input. Only ASS and SRT are
// supported here (SRT is then converted with ffmpeg).
func (t Tools) mkvextractSub(ctx context.Context, input string, s SubtitleStream, out string) error {
	if t.Mkvextract == "" {
		return errors.New("mkvextract not configured")
	}
	switch s.Codec {
	case "ass", "ssa":
		_, err := run(ctx, t.Mkvextract, input, "tracks", fmt.Sprintf("%d:%s", s.Index, out))
		return err
	case "subrip":
		srt := out + ".srt"
		defer os.Remove(srt)
		if _, err := run(ctx, t.Mkvextract, input, "tracks", fmt.Sprintf("%d:%s", s.Index, srt)); err != nil {
			return err
		}
		_, err := run(ctx, t.FFmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", srt, "-c:s", "webvtt", out)
		return err
	}
	return fmt.Errorf("mkvextract fallback doesn't handle %s", s.Codec)
}

// coverage returns the share of duration covered by at least one cue.
func coverage(path, format string, duration float64) (float64, error) {
	if duration <= 0 {
		return 0, nil
	}
	var spans [][2]float64
	var err error
	if format == "ass" {
		spans, err = assSpans(path)
	} else {
		spans, err = vttSpans(path)
	}
	if err != nil {
		return 0, err
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	total, curStart, curEnd := 0.0, -1.0, -1.0
	for _, s := range spans {
		if s[0] > curEnd {
			if curEnd > curStart {
				total += curEnd - curStart
			}
			curStart, curEnd = s[0], s[1]
		} else {
			curEnd = max(curEnd, s[1])
		}
	}
	if curEnd > curStart {
		total += curEnd - curStart
	}
	return min(total/duration, 1), nil
}

// assSpans reads Start/End of every Dialogue line, honouring the [Events]
// Format line's field order.
func assSpans(path string) ([][2]float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	startCol, endCol, nCols := 1, 2, 10 // ASS defaults
	inEvents := false
	var spans [][2]float64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024) // karaoke lines can be long
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), utf8BOM))
		if strings.HasPrefix(line, "[") {
			inEvents = strings.EqualFold(line, "[Events]")
			continue
		}
		key, rest, ok := strings.Cut(line, ":")
		if !ok || !inEvents {
			continue
		}
		switch strings.ToLower(key) {
		case "format":
			cols := strings.Split(rest, ",")
			for i, c := range cols {
				switch strings.ToLower(strings.TrimSpace(c)) {
				case "start":
					startCol = i
				case "end":
					endCol = i
				}
			}
			nCols = len(cols)
		case "dialogue":
			fields := strings.SplitN(rest, ",", nCols) // Text (last) may contain commas
			if len(fields) <= max(startCol, endCol) {
				continue
			}
			s, err1 := assTime(fields[startCol])
			e, err2 := assTime(fields[endCol])
			if err1 == nil && err2 == nil && e > s {
				spans = append(spans, [2]float64{s, e})
			}
		}
	}
	return spans, sc.Err()
}

// assTime parses H:MM:SS.cc.
func assTime(s string) (float64, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 3 {
		return 0, fmt.Errorf("bad ASS time %q", s)
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	sec, err3 := strconv.ParseFloat(parts[2], 64)
	if err := errors.Join(err1, err2, err3); err != nil {
		return 0, err
	}
	return float64(h*3600+m*60) + sec, nil
}

func vttSpans(path string) ([][2]float64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var spans [][2]float64
	for _, line := range strings.Split(string(b), "\n") {
		a, rest, ok := strings.Cut(line, "-->")
		if !ok {
			continue
		}
		bTime, _, _ := strings.Cut(strings.TrimSpace(rest), " ") // drop cue settings
		s, err1 := vttTime(a)
		e, err2 := vttTime(bTime)
		if err1 == nil && err2 == nil && e > s {
			spans = append(spans, [2]float64{s, e})
		}
	}
	return spans, nil
}

// vttTime parses [HH:]MM:SS.mmm.
func vttTime(s string) (float64, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	total := 0.0
	for _, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return 0, err
		}
		total = total*60 + v
	}
	if len(parts) < 2 {
		return 0, fmt.Errorf("bad VTT time %q", s)
	}
	return total, nil
}
