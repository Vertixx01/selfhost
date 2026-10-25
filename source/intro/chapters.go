package intro

import (
	"regexp"

	"torclient/source/media"
)

// Chapter title patterns (PROJECT.MD Stage 6, tier 1). Anchored at the start
// so "Opening" matches but "Before the Opening Ceremony" doesn't; "A Part" /
// "B Part" / "Avant" / "Prologue" are content and never match.
var (
	opTitle      = regexp.MustCompile(`(?i)^\s*(?:(?:op|opening|intro)(?:\s*\d+|\s+(?:theme|song|credits?|sequence))?\s*$|オープニング)`)
	edTitle      = regexp.MustCompile(`(?i)^\s*(?:(?:ed|ending|outro|credits)(?:\s*\d+|\s+(?:theme|song|credits?|sequence))?\s*$|エンディング)`)
	previewTitle = regexp.MustCompile(`(?i)preview|next\s+(?:episode|time)|次回|予告`)
)

// Plausible lengths; a "chapter" outside these is a mislabel.
const (
	minPartLen   = 10.0
	maxIntroLen  = 180.0
	maxOutroLen  = 240.0
	maxPreviewLn = 120.0
)

// FromChapters reads OP/ED/preview boundaries from chapter titles. Chapter
// boundaries are frame-accurate, hence confidence 1.0.
func FromChapters(chapters []media.Chapter) Timestamps {
	var ts Timestamps
	for _, c := range chapters {
		seg := &Segment{Start: c.Start, End: c.End, Confidence: ConfChapter, Source: "chapter"}
		l := seg.Duration()
		switch {
		case ts.Intro == nil && opTitle.MatchString(c.Title) && l >= minPartLen && l <= maxIntroLen:
			ts.Intro = seg
		case ts.Outro == nil && edTitle.MatchString(c.Title) && l >= minPartLen && l <= maxOutroLen:
			ts.Outro = seg
		case ts.Preview == nil && previewTitle.MatchString(c.Title) && l > 0 && l <= maxPreviewLn:
			ts.Preview = seg
		}
	}
	ts.Detected = ts.Intro != nil || ts.Outro != nil
	return ts
}
