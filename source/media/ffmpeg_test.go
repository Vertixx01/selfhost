package media

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run the real ffmpeg/ffprobe on a generated MKV and are skipped
// when the tools aren't on PATH.

func realTools(t *testing.T) Tools {
	t.Helper()
	ff, err1 := exec.LookPath("ffmpeg")
	fp, err2 := exec.LookPath("ffprobe")
	if err1 != nil || err2 != nil {
		t.Skip("ffmpeg/ffprobe not on PATH")
	}
	return Tools{FFmpeg: ff, FFprobe: fp}
}

const testASS = `[Script Info]
ScriptType: v4.00+

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,Test Font,48,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,2,0,2,10,10,10,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
`

// makeEpisode builds a 10s MKV: h264 video, FLAC jpn + AAC eng audio, a full
// ASS track, a 1s "signs" ASS track, an SRT track, a font attachment and two
// chapters. Returns the path and the font bytes.
func makeEpisode(t *testing.T, ff string) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	w := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	full := w("full.ass", testASS+
		"Dialogue: 0,0:00:00.50,0:00:04.50,Default,,0,0,0,,Hello, there\n"+
		"Dialogue: 0,0:00:05.00,0:00:09.50,Default,,0,0,0,,General Kenobi\n")
	signs := w("signs.ass", testASS+"Dialogue: 0,0:00:02.00,0:00:03.00,Default,,0,0,0,,SIGN\n")
	srt := w("es.srt", "1\n00:00:01,000 --> 00:00:03,000\nHola\n")
	chapters := w("ch.txt", ";FFMETADATA1\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=3000\ntitle=Opening\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=3000\nEND=10000\ntitle=A Part\n")
	fontBytes := bytes.Repeat([]byte("not really a font "), 500)
	font := filepath.Join(dir, "TestFont.ttf")
	os.WriteFile(font, fontBytes, 0o644)

	out := filepath.Join(dir, "episode.mkv")
	cmd := exec.Command(ff, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=24:duration=10",
		"-f", "lavfi", "-i", "sine=f=440:d=10",
		"-f", "lavfi", "-i", "sine=f=880:d=10",
		"-i", full, "-i", signs, "-i", srt, "-i", chapters,
		"-map", "0", "-map", "1", "-map", "2", "-map", "3", "-map", "4", "-map", "5", "-map_chapters", "6",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a:0", "flac", "-c:a:1", "aac", "-ac:a:1", "2",
		"-c:s:0", "copy", "-c:s:1", "copy", "-c:s:2", "srt",
		"-metadata:s:a:0", "language=jpn", "-metadata:s:a:1", "language=eng", "-metadata:s:a:1", "title=English Dub",
		"-metadata:s:s:0", "language=eng", "-metadata:s:s:0", "title=Full Subs",
		"-metadata:s:s:1", "language=eng", "-metadata:s:s:1", "title=Signs & Songs", "-disposition:s:1", "forced",
		"-metadata:s:s:2", "language=spa",
		"-attach", font, "-metadata:s:t:0", "mimetype=application/x-truetype-font", "-metadata:s:t:0", "filename=../Test Font.ttf",
		out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("make fixture: %v: %s", err, b)
	}
	return out, fontBytes
}

func TestAnalyzeExtractRealFile(t *testing.T) {
	tools := realTools(t)
	input, fontBytes := makeEpisode(t, tools.FFmpeg)
	work := t.TempDir()
	meta := filepath.Join(work, "meta")

	sm, err := tools.Analyze(t.Context(), input, meta)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"media_info.json", "stream_map.json"} {
		b, err := os.ReadFile(filepath.Join(meta, f))
		if err != nil || !json.Valid(b) {
			t.Errorf("%s missing or invalid: %v", f, err)
		}
	}
	if sm.Video.Codec != "h264" || sm.Video.Width != 320 || sm.Video.FPS != 24 || sm.Video.VFR {
		t.Errorf("video: %+v", sm.Video)
	}
	if len(sm.Audio) != 2 || sm.SourceType != "bd" { // FLAC → BD
		t.Errorf("audio %+v, source %s", sm.Audio, sm.SourceType)
	}
	if len(sm.Chapters) != 2 || sm.Chapters[0].Title != "Opening" || sm.Chapters[1].Start != 3 {
		t.Errorf("chapters: %+v", sm.Chapters)
	}
	if sm.FastPath || !strings.Contains(sm.FastPathReason, "320x240") {
		t.Errorf("320x240 must not be fast path: %v %q", sm.FastPath, sm.FastPathReason)
	}
	if len(sm.Attachments) != 1 || !sm.Attachments[0].Font {
		t.Errorf("attachments: %+v", sm.Attachments)
	}

	kfs, err := tools.Keyframes(t.Context(), input, sm.Video.Index)
	if err != nil || len(kfs) == 0 || kfs[0] != 0 {
		t.Errorf("keyframes %v, %v", kfs, err)
	}

	subsDir := filepath.Join(work, "subs")
	res, err := tools.ExtractSubtitles(t.Context(), input, sm, subsDir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]SubtitleTrack{}
	for _, tr := range res.Tracks {
		files[tr.File] = tr
	}
	if len(files) != 3 || files["eng_dialogue.ass"].Kind != "dialogue" || !files["eng_signs.ass"].Forced || files["spa.vtt"].Format != "vtt" {
		t.Fatalf("tracks: %+v (warnings %v)", res.Tracks, res.Warnings)
	}
	entries, _ := os.ReadDir(subsDir)
	if len(entries) != 3 {
		t.Errorf("subs dir has %d entries, want 3 (temp files left?)", len(entries))
	}
	// -copyts keeps cues aligned with the video; without it they shift by
	// the audio's negative start offset.
	vtt, _ := os.ReadFile(filepath.Join(subsDir, "spa.vtt"))
	if !strings.Contains(string(vtt), "00:01.000 --> 00:03.000") {
		t.Errorf("spa.vtt timing off:\n%s", vtt)
	}
	ass, _ := os.ReadFile(filepath.Join(subsDir, "eng_dialogue.ass"))
	if !strings.Contains(string(ass), "General Kenobi") {
		t.Errorf("dialogue ASS content missing:\n%s", ass)
	}

	fonts, warnings := tools.ExtractFonts(t.Context(), input, sm, filepath.Join(work, ".fonts_temp"))
	if len(warnings) > 0 || len(fonts) != 1 {
		t.Fatalf("fonts %+v warnings %v", fonts, warnings)
	}
	if fonts[0].Filename != "Test Font.ttf" { // "../" stripped
		t.Errorf("font filename %q", fonts[0].Filename)
	}
	if got, _ := os.ReadFile(fonts[0].Path); !bytes.Equal(got, fontBytes) {
		t.Error("extracted font bytes differ")
	}

	am, err := MapAudio(sm)
	if err != nil || len(am.Tracks) != 2 || am.Tracks[0].Action != "aac" || am.Tracks[1].Action != "copy" || !am.Tracks[0].Default {
		t.Errorf("audio map %+v %v", am, err)
	}
}

func TestProbeCorruptFile(t *testing.T) {
	tools := realTools(t)
	bad := filepath.Join(t.TempDir(), "episode.mkv")
	os.WriteFile(bad, bytes.Repeat([]byte{0x42}, 4096), 0o644)
	if _, err := tools.Analyze(t.Context(), bad, t.TempDir()); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("want corrupt-file error, got %v", err)
	}
}
