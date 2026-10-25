package media

import (
	"errors"
	"fmt"
	"regexp"
)

var ErrNoAudio = errors.New("no usable audio track")

// AudioMap is audio_map.json (PROJECT.MD Stage 5).
type AudioMap struct {
	Tracks  []AudioTrack   `json:"tracks"`
	Skipped []SkippedTrack `json:"skipped,omitempty"`
}

type AudioTrack struct {
	Index     int    `json:"index"`
	Lang      string `json:"lang"`
	LangFrom  string `json:"lang_from"` // "title" | "tag" | "order" | "none"
	Name      string `json:"name"`      // HLS NAME
	Rendition string `json:"rendition"` // output dir: audio/{rendition}/
	Codec     string `json:"codec"`
	Channels  int    `json:"channels"`
	// Action: "copy" keeps the stream; "aac" re-encodes. Segments are ADTS
	// .aac files, so only AAC can be copied — lossless and AC3/EAC3/Opus/MP3
	// sources are all re-encoded.
	Action  string `json:"action"`
	Default bool   `json:"default"`
}

type SkippedTrack struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

var (
	commentaryRe = regexp.MustCompile(`(?i)commentary`)
	downmixRe    = regexp.MustCompile(`(?i)downmix|stereo`)
	dualAudioRe  = regexp.MustCompile(`(?i)(dual|multi)[ ._-]?audio|(^|[^a-z])jpn?[ ._]?[+&/][ ._]?eng?([^a-z]|$)`)
	dubRe        = regexp.MustCompile(`(?i)(^|[^a-z])(eng(lish)?[ ._-]?)?dub(bed)?([^a-z]|$)`)
)

// MapAudio decides language, keep/skip, copy/re-encode, and the default track.
func MapAudio(sm *StreamMap) (*AudioMap, error) { return MapAudioFor(sm, "") }

// MapAudioFor is MapAudio with the release name as a hint for untagged
// tracks: "Dual Audio" means Japanese + English, "English Dub" with a single
// track means English.
func MapAudioFor(sm *StreamMap, release string) (*AudioMap, error) {
	if len(sm.Audio) == 0 {
		return nil, ErrNoAudio
	}

	am := &AudioMap{}
	var kept []AudioTrack
	for i, a := range sm.Audio {
		if commentaryRe.MatchString(a.Title) {
			am.Skipped = append(am.Skipped, SkippedTrack{a.Index, "commentary track"})
			continue
		}
		t := AudioTrack{Index: a.Index, Codec: a.Codec, Channels: a.Channels, Action: "aac"}
		if a.Codec == "aac" {
			t.Action = "copy"
		}
		// A human-written title ("Japanese 5.1") beats a tag muxers often get
		// wrong; then the tag; then anime convention (first track is Japanese).
		switch {
		case titleLang(a.Title) != "":
			t.Lang, t.LangFrom = titleLang(a.Title), "title"
		case a.Language != "und":
			t.Lang, t.LangFrom = a.Language, "tag"
		case i == 0:
			t.Lang, t.LangFrom = "jpn", "order"
		default:
			t.Lang, t.LangFrom = "und", "none"
		}
		kept = append(kept, t)
	}
	langHints(kept, release)

	// Same language twice: drop a derived stereo downmix when a track with
	// more channels exists; otherwise keep both (a different mix).
	var tracks []AudioTrack
	for _, t := range kept {
		if downmixRe.MatchString(titleOf(sm, t.Index)) && hasWider(kept, t) {
			am.Skipped = append(am.Skipped, SkippedTrack{t.Index, "downmix of a wider " + t.Lang + " track"})
			continue
		}
		tracks = append(tracks, t)
	}
	if len(tracks) == 0 {
		return nil, ErrNoAudio
	}

	seen := map[string]int{}
	for i := range tracks {
		t := &tracks[i]
		seen[t.Lang]++
		t.Rendition, t.Name = t.Lang, LangName(t.Lang)
		if n := seen[t.Lang]; n > 1 {
			t.Rendition = fmt.Sprintf("%s_%d", t.Lang, n)
			t.Name = fmt.Sprintf("%s %d", t.Name, n)
		}
	}

	def := 0
	for i, t := range tracks {
		if t.Lang == "jpn" {
			def = i
			break
		}
	}
	tracks[def].Default = true
	am.Tracks = tracks
	return am, nil
}

func hasWider(tracks []AudioTrack, t AudioTrack) bool {
	for _, o := range tracks {
		if o.Index != t.Index && o.Lang == t.Lang && o.Channels > t.Channels {
			return true
		}
	}
	return false
}

func titleOf(sm *StreamMap, index int) string {
	for _, a := range sm.Audio {
		if a.Index == index {
			return a.Title
		}
	}
	return ""
}

// langHints relabels tracks the stream gave no language for ("order" or
// "none") from the release name.
func langHints(tracks []AudioTrack, release string) {
	guessed := func(t AudioTrack) bool { return t.LangFrom == "order" || t.LangFrom == "none" }
	switch {
	case dualAudioRe.MatchString(release):
		have := map[string]bool{}
		for _, t := range tracks {
			if !guessed(t) {
				have[t.Lang] = true
			}
		}
		for i := range tracks {
			if !guessed(tracks[i]) {
				continue
			}
			for _, l := range []string{"jpn", "eng"} {
				if !have[l] {
					tracks[i].Lang, tracks[i].LangFrom, have[l] = l, "release", true
					break
				}
			}
		}
	case dubRe.MatchString(release) && len(tracks) == 1 && guessed(tracks[0]):
		tracks[0].Lang, tracks[0].LangFrom = "eng", "release"
	}
}
