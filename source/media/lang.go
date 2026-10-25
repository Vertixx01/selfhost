package media

import (
	"regexp"
	"strings"
)

// ISO 639-1 → 639-2/B (the form Matroska uses) for languages seen in anime releases.
var twoToThree = map[string]string{
	"ja": "jpn", "en": "eng", "es": "spa", "fr": "fre", "de": "ger", "it": "ita",
	"pt": "por", "ru": "rus", "zh": "chi", "ko": "kor", "ar": "ara", "pl": "pol",
	"tr": "tur", "id": "ind", "th": "tha", "vi": "vie", "ms": "may", "nl": "dut",
	"sv": "swe", "fi": "fin", "hi": "hin", "he": "heb", "uk": "ukr", "cs": "cze",
	"hu": "hun", "ro": "rum", "el": "gre",
}

// 639-2/T variants that some muxers write instead of /B.
var terminologyToBib = map[string]string{
	"fra": "fre", "deu": "ger", "zho": "chi", "msa": "may", "nld": "dut",
	"ces": "cze", "ron": "rum", "ell": "gre",
}

var langNames = map[string]string{
	"jpn": "Japanese", "eng": "English", "spa": "Spanish", "fre": "French", "ger": "German",
	"ita": "Italian", "por": "Portuguese", "rus": "Russian", "chi": "Chinese", "kor": "Korean",
	"ara": "Arabic", "pol": "Polish", "tur": "Turkish", "ind": "Indonesian", "tha": "Thai",
	"vie": "Vietnamese", "may": "Malay", "und": "Unknown",
}

var langCode = regexp.MustCompile(`^[a-z]{2,3}$`)

// NormalizeLang returns a lowercase 3-letter code, or "und". The result is
// always safe to use in a filename.
func NormalizeLang(tag string) string {
	l := strings.ToLower(strings.TrimSpace(tag))
	if i := strings.IndexAny(l, "-_"); i > 0 { // "ja-JP", "en_US"
		l = l[:i]
	}
	if !langCode.MatchString(l) {
		return "und"
	}
	if t, ok := twoToThree[l]; ok {
		return t
	}
	if b, ok := terminologyToBib[l]; ok {
		return b
	}
	if len(l) != 3 {
		return "und"
	}
	return l
}

// BCP47 returns the tag HLS LANGUAGE attributes want (RFC 5646): the
// 2-letter code where one exists ("jpn" → "ja"), else the 3-letter code.
func BCP47(code string) string {
	for two, three := range twoToThree {
		if three == code {
			return two
		}
	}
	return code
}

// LangName is the human-readable name used in HLS NAME attributes.
func LangName(code string) string {
	if n, ok := langNames[code]; ok {
		return n
	}
	return code
}

// Track-title language hints, checked in order. Short codes need word boundaries
// so "Japanese" doesn't also match "jap" twice and "eng" doesn't match "length".
var titleLangs = []struct {
	lang string
	re   *regexp.Regexp
}{
	{"jpn", regexp.MustCompile(`(?i)japanese|日本語|\b(jpn|jap|jp)\b`)},
	{"eng", regexp.MustCompile(`(?i)english|\b(eng|en)\b|\bdub(bed)?\b`)},
}

// titleLang guesses a language from a track title, or "".
func titleLang(title string) string {
	for _, tl := range titleLangs {
		if tl.re.MatchString(title) {
			return tl.lang
		}
	}
	return ""
}
