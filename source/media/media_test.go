package media

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeLang(t *testing.T) {
	for in, want := range map[string]string{
		"ja": "jpn", "JPN": "jpn", "en-US": "eng", "fra": "fre", "spa": "spa",
		"": "und", "und": "und", "japanese": "und", "../x": "und", "e": "und",
	} {
		if got := NormalizeLang(in); got != want {
			t.Errorf("NormalizeLang(%q) = %q, want %q", in, got, want)
		}
	}
}

func stream(idx int, typ, codec string, tags map[string]string) probeStream {
	return probeStream{Index: idx, CodecType: typ, CodecName: codec, Tags: tags, Disposition: map[string]int{}}
}

func video1080(idx int) probeStream {
	v := stream(idx, "video", "h264", nil)
	v.Width, v.Height, v.PixFmt = 1920, 1080, "yuv420p"
	v.RFrameRate, v.AvgFrameRate = "24000/1001", "24000/1001"
	return v
}

func TestBuildStreamMap(t *testing.T) {
	cover := stream(0, "video", "mjpeg", nil)
	cover.Disposition["attached_pic"] = 1
	flac := stream(2, "audio", "flac", map[string]string{"language": "ja"})
	aac := stream(3, "audio", "aac", map[string]string{"language": "eng", "BPS-eng": "192000"})
	pgs := stream(4, "subtitle", "hdmv_pgs_subtitle", nil)
	font := stream(5, "attachment", "ttf", map[string]string{"filename": "A.TTF", "mimetype": "application/octet-stream"})
	pic := stream(6, "attachment", "png", map[string]string{"filename": "cover.png", "mimetype": "image/png"})

	sm, err := buildStreamMap(&probeOutput{
		Streams: []probeStream{cover, video1080(1), flac, aac, pgs, font, pic},
		Format:  probeFormat{Duration: "1420.5", BitRate: "3000000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sm.Video.Index != 1 {
		t.Errorf("cover art taken as video: index %d", sm.Video.Index)
	}
	if sm.SourceType != "bd" {
		t.Errorf("FLAC audio should mean BD, got %s", sm.SourceType)
	}
	if sm.Audio[0].Language != "jpn" || !sm.Audio[0].Lossless || sm.Audio[1].BitRateKbps != 192 {
		t.Errorf("audio parsed wrong: %+v", sm.Audio)
	}
	if sm.Video.BitRateKbps != 3000-192 {
		t.Errorf("video bitrate estimate = %d", sm.Video.BitRateKbps)
	}
	if !sm.Subtitles[0].Bitmap {
		t.Error("PGS not flagged as bitmap")
	}
	if !sm.Attachments[0].Font || sm.Attachments[1].Font {
		t.Errorf("font detection: %+v", sm.Attachments)
	}
	if !sm.FastPath {
		t.Errorf("1080p h264 yuv420p CFR should be a fast-path candidate: %s", sm.FastPathReason)
	}
}

func TestBuildStreamMapWebAndFastPathReasons(t *testing.T) {
	cases := map[string]func(*probeStream){
		"hevc":         func(v *probeStream) { v.CodecName = "hevc" },
		"10-bit":       func(v *probeStream) { v.PixFmt = "yuv420p10le" },
		"720p":         func(v *probeStream) { v.Width, v.Height = 1280, 720 },
		"vfr":          func(v *probeStream) { v.AvgFrameRate = "21/1" },
		"no-bit-depth": func(v *probeStream) {},
	}
	for name, mod := range cases {
		v := video1080(0)
		mod(&v)
		sm, err := buildStreamMap(&probeOutput{
			Streams: []probeStream{v, stream(1, "audio", "aac", nil)},
			Format:  probeFormat{Duration: "1400", BitRate: "2500000"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if sm.SourceType != "web" {
			t.Errorf("%s: want web, got %s", name, sm.SourceType)
		}
		if wantFast := name == "no-bit-depth"; sm.FastPath != wantFast {
			t.Errorf("%s: fast path = %v (%s)", name, sm.FastPath, sm.FastPathReason)
		}
	}

	if _, err := buildStreamMap(&probeOutput{Streams: []probeStream{stream(0, "audio", "aac", nil)}}); !errors.Is(err, ErrNoVideo) {
		t.Errorf("want ErrNoVideo, got %v", err)
	}
}

func TestMaxGap(t *testing.T) {
	if g := maxGap([]float64{0, 4, 6, 18}, 20); g != 12 {
		t.Errorf("maxGap = %v, want 12", g)
	}
	if g := maxGap([]float64{0, 5}, 30); g != 25 {
		t.Errorf("tail gap = %v, want 25", g)
	}
}

func audioMap(t *testing.T, streams ...AudioStream) *AudioMap {
	t.Helper()
	am, err := MapAudio(&StreamMap{Audio: streams})
	if err != nil {
		t.Fatal(err)
	}
	return am
}

func TestMapAudio(t *testing.T) {
	am := audioMap(t,
		AudioStream{Index: 1, Codec: "flac", Language: "und", Channels: 6},                                  // first + untagged → jpn
		AudioStream{Index: 2, Codec: "aac", Language: "jpn", Title: "Japanese Stereo Downmix", Channels: 2}, // dropped
		AudioStream{Index: 3, Codec: "aac", Language: "eng", Title: "Commentary", Channels: 2},              // dropped
		AudioStream{Index: 4, Codec: "eac3", Language: "jpn", Title: "English Dub", Channels: 6},            // title beats tag
		AudioStream{Index: 5, Codec: "aac", Language: "eng", Title: "English (alt mix)", Channels: 2},
	)
	got := map[int]AudioTrack{}
	for _, tr := range am.Tracks {
		got[tr.Index] = tr
	}
	if len(am.Tracks) != 3 || len(am.Skipped) != 2 {
		t.Fatalf("tracks %+v skipped %+v", am.Tracks, am.Skipped)
	}
	if tr := got[1]; tr.Lang != "jpn" || tr.LangFrom != "order" || tr.Action != "aac" || !tr.Default {
		t.Errorf("track 1: %+v", tr)
	}
	if tr := got[4]; tr.Lang != "eng" || tr.LangFrom != "title" || tr.Action != "aac" || tr.Rendition != "eng" {
		t.Errorf("track 4: %+v", tr)
	}
	if tr := got[5]; tr.Action != "copy" || tr.Rendition != "eng_2" || tr.Name != "English 2" {
		t.Errorf("track 5: %+v", tr)
	}

	// Stereo-only Japanese track is kept: there's nothing wider to prefer.
	am = audioMap(t, AudioStream{Index: 1, Codec: "aac", Language: "jpn", Title: "Stereo", Channels: 2})
	if len(am.Tracks) != 1 {
		t.Errorf("lone stereo track dropped: %+v", am)
	}

	if _, err := MapAudio(&StreamMap{}); !errors.Is(err, ErrNoAudio) {
		t.Errorf("no audio: %v", err)
	}
	if _, err := MapAudio(&StreamMap{Audio: []AudioStream{{Index: 1, Title: "Director's Commentary"}}}); !errors.Is(err, ErrNoAudio) {
		t.Errorf("only commentary: %v", err)
	}
}

func TestOnlyBitmapSubsFails(t *testing.T) {
	sm := &StreamMap{Subtitles: []SubtitleStream{{Index: 2, Codec: "hdmv_pgs_subtitle", Bitmap: true}}}
	if _, err := (Tools{}).ExtractSubtitles(t.Context(), "unused.mkv", sm, t.TempDir()); !errors.Is(err, ErrOnlyBitmapSubs) {
		t.Fatalf("want ErrOnlyBitmapSubs, got %v", err)
	}
	res, err := (Tools{}).ExtractSubtitles(t.Context(), "unused.mkv", &StreamMap{}, t.TempDir())
	if err != nil || len(res.Tracks) != 0 {
		t.Fatalf("no subs at all should be fine: %+v %v", res, err)
	}
}

func TestCoverage(t *testing.T) {
	dir := t.TempDir()
	// Styles' Format line has 23 columns; only the [Events] one may be used.
	ass := utf8BOM + `[Script Info]
[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,Arial,48,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,2,0,2,10,10,10,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:00.00,0:00:10.00,Default,,0,0,0,,one, with, commas
Dialogue: 0,0:00:05.00,0:00:15.00,Default,,0,0,0,,overlaps
Comment: 0,0:00:20.00,0:00:90.00,Default,,0,0,0,,ignored
Dialogue: 0,0:01:00.00,0:01:05.00,Default,,0,0,0,,later
`
	os.WriteFile(filepath.Join(dir, "a.ass"), []byte(ass), 0o644)
	if c, err := coverage(filepath.Join(dir, "a.ass"), "ass", 100); err != nil || c != 0.20 {
		t.Errorf("ass coverage = %v, %v; want 0.20", c, err)
	}

	vtt := "WEBVTT\n\n00:01.000 --> 00:03.000 align:start\nHi\n\n01:00:00.000 --> 01:00:04.000\nLater\n"
	os.WriteFile(filepath.Join(dir, "a.vtt"), []byte(vtt), 0o644)
	if c, err := coverage(filepath.Join(dir, "a.vtt"), "vtt", 3600*2); err != nil || c != 6.0/7200 {
		t.Errorf("vtt coverage = %v, %v", c, err)
	}
}

func TestSafeFilename(t *testing.T) {
	for in, want := range map[string]string{
		"SourceHanSans-Bold.otf": "SourceHanSans-Bold.otf",
		"../../etc/passwd":       "passwd",
		`C:\Windows\evil.ttf`:    "evil.ttf",
		"a:b?.ttf":               "a_b_.ttf",
		"..":                     "font7.ttf",
		"":                       "font7.ttf",
		".hidden":                "font7.ttf",
	} {
		if got := safeFilename(in, 7); got != want {
			t.Errorf("safeFilename(%q) = %q, want %q", in, got, want)
		}
	}
	if strings.ContainsAny(safeFilename("x\x00y.ttf", 1), "\x00") {
		t.Error("control char kept")
	}
}
