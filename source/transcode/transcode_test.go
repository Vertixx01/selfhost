package transcode

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"torclient/source/media"
)

func testSettings() Settings {
	return Settings{
		CRFBD: 22, CRFWeb: 21, MaxrateBDKbps: 6000, BufsizeBDKbps: 12000,
		MaxrateWebKbps: 5000, BufsizeWebKbps: 20000, Preset: "ultrafast", Tune: "animation",
		AudioBitrateKbps: 128, AudioChannels: 2, AudioSampleRate: 48000, SegmentDuration: 6,
	}
}

func TestPlanRenditions(t *testing.T) {
	r := planRenditions(media.VideoStream{Width: 1920, Height: 1080, FPS: 23.976, Level: 40}, true)
	if len(r) != 2 || !r[0].Copy || r[0].Level != 40 || r[1].Name != "720p" || r[1].Width != 1280 || r[1].Copy {
		t.Errorf("1080p fast path: %+v", r)
	}
	if r := planRenditions(media.VideoStream{Width: 1280, Height: 720, FPS: 24}, false); len(r) != 1 || r[0].Name != "720p" {
		t.Errorf("720p source must not be upscaled: %+v", r)
	}
	if r := planRenditions(media.VideoStream{Width: 853, Height: 481, FPS: 24}, false); len(r) != 1 || r[0].Height != 480 || r[0].Width != 852 {
		t.Errorf("SD source: %+v", r)
	}
	if r := planRenditions(media.VideoStream{Width: 1440, Height: 1080, FPS: 24}, false); r[1].Width != 960 {
		t.Errorf("4:3 aspect not kept: %+v", r)
	}
	if r := planRenditions(media.VideoStream{Width: 1920, Height: 1080, FPS: 59.94}, false); r[0].Level != 42 {
		t.Errorf("60fps level: %+v", r)
	}
}

func TestCodecStrings(t *testing.T) {
	for _, c := range []struct {
		profile string
		level   int
		want    string
	}{
		{"high", 41, "avc1.640029"}, {"High", 40, "avc1.640028"},
		{"Main", 31, "avc1.4d001f"}, {"Constrained Baseline", 30, "avc1.42401e"},
	} {
		if got := avcCodec(c.profile, c.level); got != c.want {
			t.Errorf("avcCodec(%q,%d) = %s, want %s", c.profile, c.level, got, c.want)
		}
	}
	if aacCodec("HE-AAC") != "mp4a.40.5" || aacCodec("LC") != "mp4a.40.2" || aacCodec("") != "mp4a.40.2" {
		t.Error("aac codec strings")
	}
	if k := keyint(23.976, 6); k != 312 {
		t.Errorf("keyint = %d", k)
	}
}

func TestEncodeArgs(t *testing.T) {
	p := encodePlan{
		Input: "in.mkv", OutDir: "out",
		Video:      media.VideoStream{Index: 0, FPS: 24, VFR: true, FrameRate: "24000/1001"},
		Renditions: []Rendition{{Name: "1080p", Width: 1920, Height: 1080, Level: 41}, {Name: "720p", Width: 1280, Height: 720, Level: 40}},
		Audio: []media.AudioTrack{
			{Index: 1, Rendition: "jpn", Action: "aac"},
			{Index: 2, Rendition: "eng", Action: "copy"},
		},
		Rate: rateControl{CRF: 21, MaxrateKbps: 5000, BufKbps: 20000}, Settings: testSettings(),
		ForceKeyFrames: []float64{0, 6, 9.5}, KeyInfo: "meta/enc.keyinfo",
	}
	a := strings.Join(p.args(), " ")
	for _, want := range []string{
		"[0:0]fps=24000/1001,split=2[s0][s1]",
		"-maxrate 5000k -bufsize 20000k", "-maxrate 2500k -bufsize 10000k",
		"scenecut=0", "-force_key_frames 0,6,9.5", "-level:v 4.1",
		"-map 0:1 -vn -sn -dn -c:a aac -b:a 128k -ac 2 -ar 48000",
		"-map 0:2 -vn -sn -dn -c:a copy",
	} {
		if !strings.Contains(a, want) {
			t.Errorf("args missing %q\n%s", want, a)
		}
	}
	if n := strings.Count(a, "-hls_key_info_file"); n != 4 {
		t.Errorf("key info on %d outputs, want 4", n)
	}

	p.Renditions = []Rendition{{Name: "1080p", Copy: true}}
	p.Audio, p.KeyInfo = nil, ""
	a = strings.Join(p.args(), " ")
	if strings.Contains(a, "filter_complex") || !strings.Contains(a, "-map 0:0 -c:v copy") || strings.Contains(a, "key_info") {
		t.Errorf("copy-only args: %s", a)
	}
}

func TestReadPlaylistValidation(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "seg0000.ts"), []byte("x"), 0o644)
	write := func(body string) string {
		p := filepath.Join(dir, "stream.m3u8")
		os.WriteFile(p, []byte(body), 0o644)
		return p
	}
	ok := "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k\"\n#EXTINF:6.000,\nseg0000.ts\n#EXT-X-ENDLIST\n"
	pl, err := readPlaylist(write(ok))
	if err != nil || !pl.Encrypted || len(pl.Segments) != 1 || pl.PeakBitrate() != 2 {
		t.Fatalf("%+v %v", pl, err)
	}
	if _, err := readPlaylist(write("#EXTM3U\n#EXTINF:6,\nseg0001.ts\n#EXT-X-ENDLIST\n")); err == nil {
		t.Error("missing segment accepted")
	}
	if _, err := readPlaylist(write("#EXTM3U\n#EXTINF:6,\nseg0000.ts\n")); err == nil {
		t.Error("playlist without ENDLIST accepted")
	}
}

func TestKeysReused(t *testing.T) {
	dir := t.TempDir()
	k1, err := ensureKey(dir, "https://keys/a/ep1.key")
	if err != nil {
		t.Fatal(err)
	}
	b1, _ := os.ReadFile(k1.KeyPath)
	k2, _ := ensureKey(dir, "https://keys/a/ep1.key")
	b2, _ := os.ReadFile(k2.KeyPath)
	if string(b1) != string(b2) || string(k1.IV) != string(k2.IV) || len(b1) != 16 {
		t.Error("retry re-keyed the episode")
	}
	k3, _ := ensureKey(dir, "https://keys/a/ep2.key")
	if b3, _ := os.ReadFile(k3.KeyPath); string(b3) == string(b1) {
		t.Error("different URI kept the old key")
	}
}

func TestTimestampMapAndMaster(t *testing.T) {
	dir := t.TempDir()
	vtt := filepath.Join(dir, "eng.vtt")
	os.WriteFile(vtt, []byte("WEBVTT\n\n00:01.000 --> 00:02.000\nHi\n"), 0o644)
	if err := addTimestampMap(vtt, 126000); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(vtt)
	if !strings.HasPrefix(string(b), "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:126000,LOCAL:00:00:00.000\n") {
		t.Errorf("vtt:\n%s", b)
	}

	pl := &MediaPlaylist{Segments: []Segment{{Duration: 6, Bytes: 750000}, {Duration: 6, Bytes: 375000}}, Ended: true}
	apl := &MediaPlaylist{Segments: []Segment{{Duration: 6, Bytes: 96000}}, Ended: true}
	master := filepath.Join(dir, "master.m3u8")
	err := writeMaster(master,
		[]videoVariant{{Rendition: Rendition{Name: "1080p", Width: 1920, Height: 1080}, Playlist: pl, Codec: "avc1.640029", FPS: 23.976}},
		[]audioRendition{
			{Track: media.AudioTrack{Lang: "jpn", Name: "Japanese", Rendition: "jpn", Default: true}, Playlist: apl, Codec: "mp4a.40.2", Channels: 2},
			{Track: media.AudioTrack{Lang: "eng", Name: "English", Rendition: "eng"}, Playlist: apl, Codec: "mp4a.40.5", Channels: 6},
		},
		[]subtitleOutput{
			{Track: media.SubtitleTrack{Lang: "eng", Kind: "signs", Forced: true}, Playlist: "subs/eng_signs.m3u8"},
			{Track: media.SubtitleTrack{Lang: "eng", Kind: "dialogue"}, Playlist: "subs/eng_dialogue.m3u8"},
		})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := os.ReadFile(master)
	lines := strings.Split(string(m), "\n")
	for _, want := range []string{
		`#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",LANGUAGE="ja",NAME="Japanese",DEFAULT=YES,AUTOSELECT=YES,CHANNELS="2",URI="audio/jpn/stream.m3u8"`,
		`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",LANGUAGE="en",NAME="English (Signs)",DEFAULT=NO,AUTOSELECT=YES,FORCED=YES,URI="subs/eng_signs.m3u8"`,
		`#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",LANGUAGE="en",NAME="English",DEFAULT=YES,AUTOSELECT=YES,FORCED=NO,URI="subs/eng_dialogue.m3u8"`,
		// peak 1 Mbit/s video + 128 kbit/s audio; codecs deduplicated.
		`#EXT-X-STREAM-INF:BANDWIDTH=1128000,AVERAGE-BANDWIDTH=878000,RESOLUTION=1920x1080,FRAME-RATE=23.976,CODECS="avc1.640029,mp4a.40.2,mp4a.40.5",AUDIO="audio",SUBTITLES="subs"`,
		"1080p/stream.m3u8",
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("master missing:\n%s\n---\n%s", want, m)
		}
	}
}

func TestEncodeArgsNVENC(t *testing.T) {
	p := encodePlan{
		Input: "in.mkv", OutDir: "out", Encoder: EncoderNVENC,
		Video:      media.VideoStream{Index: 0, FPS: 24},
		Renditions: []Rendition{{Name: "1080p", Width: 1920, Height: 1080, Level: 41}, {Name: "720p", Width: 1280, Height: 720, Level: 40}},
		Rate:       rateControl{CRF: 22, CQ: 28, MaxrateKbps: 6000, BufKbps: 12000}, Settings: testSettings(),
		ForceKeyFrames: []float64{0, 6, 9.5},
	}
	a := strings.Join(p.args(), " ")
	for _, want := range []string{
		"-hwaccel cuda -hwaccel_output_format cuda -i in.mkv",
		"[s0]scale_cuda=1920:1080:format=nv12[v0]", "[s1]scale_cuda=1280:720:format=nv12[v1]",
		"-c:v h264_nvenc -preset p1", "-cq 28", "-g 312", "-no-scenecut 1", "-forced-idr 1",
		"-force_key_frames 0,6,9.5", "-maxrate 3000k -bufsize 6000k",
	} {
		if !strings.Contains(a, want) {
			t.Errorf("NVENC args missing %q\n%s", want, a)
		}
	}
	if strings.Contains(a, "libx264") || strings.Contains(a, "unsharp") {
		t.Errorf("CPU filters/encoder in GPU plan:\n%s", a)
	}
	// Copy-only plans don't touch the GPU at all.
	p.Renditions = []Rendition{{Name: "1080p", Copy: true}}
	if strings.Contains(strings.Join(p.args(), " "), "hwaccel") {
		t.Error("hwaccel on a copy-only run")
	}
}

func TestAllowFastPath(t *testing.T) {
	sm := func(fast bool, kbps int) *media.StreamMap {
		return &media.StreamMap{FastPath: fast, Video: media.VideoStream{BitRateKbps: kbps}}
	}
	s := Settings{FastPathMaxKbps: 4000}
	for _, c := range []struct {
		sm   *media.StreamMap
		want bool
	}{
		{sm(true, 3500), true},
		{sm(true, 4000), true},
		{sm(true, 8600), false}, // the Bleach WEB-DL case
		{sm(true, 0), false},    // unknown bitrate: don't risk a huge copy
		{sm(false, 1000), false},
	} {
		if got := s.allowFastPath(c.sm); got != c.want {
			t.Errorf("allowFastPath(fast=%v, %d kbps) = %v", c.sm.FastPath, c.sm.Video.BitRateKbps, got)
		}
	}
	if !(Settings{}).allowFastPath(sm(true, 9000)) {
		t.Error("FAST_PATH_MAX_KBPS=0 should mean no limit")
	}
}

func TestThreadCap(t *testing.T) {
	st := testSettings()
	st.Threads = 4
	p := encodePlan{
		Input: "in.mkv", OutDir: "out", Encoder: EncoderCPU, Settings: st,
		Video:      media.VideoStream{Index: 0, FPS: 24},
		Renditions: []Rendition{{Name: "720p", Width: 1280, Height: 720, Level: 40}},
		Rate:       rateControl{CRF: 21, MaxrateKbps: 5000, BufKbps: 10000},
	}
	a := strings.Join(p.args(), " ")
	// Once for decoding/filters (before -i), once for the x264 output.
	if !strings.Contains(a, "-threads 4 -filter_complex_threads 4 -i in.mkv") || strings.Count(a, "-threads 4") != 2 {
		t.Errorf("thread cap missing:\n%s", a)
	}
	p.SingleThread = true
	if a := strings.Join(p.args(), " "); strings.Contains(a, "-threads 4") || !strings.Contains(a, "-threads 1") {
		t.Errorf("crash retry should use 1 thread:\n%s", a)
	}
	p.SingleThread, p.Encoder = false, EncoderNVENC
	if a := strings.Join(p.args(), " "); strings.Contains(a, "-threads") {
		t.Errorf("thread cap applied to GPU run:\n%s", a)
	}
}
