package transcode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"

	"torclient/source/intro"
	"torclient/source/media"
)

type Transcoder struct {
	FFmpeg, FFprobe string
	Settings        Settings
	Log             *slog.Logger
}

// Request is one episode. OutDir receives the same layout as
// output/{anime_id}/{ep_id}/ (master.m3u8, {rendition}/, audio/, subs/);
// the packager (Stage 8) copies it over.
type Request struct {
	Input      string
	OutDir     string // work/{job}/{ep}/work
	MetaDir    string // enc.key / enc.keyinfo live here
	SubsDir    string // Stage 4 output
	Streams    *media.StreamMap
	Audio      *media.AudioMap
	Subs       *media.SubtitleResult
	Timestamps intro.Timestamps // boundaries known before transcode
	KeyURI     string           // "" = no encryption
	Encoder    string           // EncoderCPU (default) or EncoderNVENC
}

type Result struct {
	Master     string   `json:"master"`
	Renditions []string `json:"renditions"`
	CRF        int      `json:"crf"`
	Encoder    string   `json:"encoder"`
	FastPath   bool     `json:"fast_path"`
	Encrypted  bool     `json:"encrypted"`
	SizeRatio  float64  `json:"size_ratio,omitempty"` // top rendition video bytes / source video bytes
	Warnings   []string `json:"warnings,omitempty"`
}

// Auto-adjust thresholds (§7a): outside these the CRF is nudged and re-run.
const (
	maxSizeRatio = 1.5
	minSizeRatio = 0.3
)

func (t *Transcoder) Run(ctx context.Context, req Request) (*Result, error) {
	sm := req.Streams
	if sm == nil || req.Audio == nil || len(req.Audio.Tracks) == 0 {
		return nil, errors.New("transcode needs a stream map and at least one audio track")
	}
	log := t.Log
	if log == nil {
		log = slog.Default()
	}
	res := &Result{FastPath: t.Settings.allowFastPath(sm)}
	if sm.FastPath && !res.FastPath {
		res.Warnings = append(res.Warnings, fmt.Sprintf("fast path skipped: source video is %d kbit/s, above FAST_PATH_MAX_KBPS=%d; re-encoding to save space", sm.Video.BitRateKbps, t.Settings.FastPathMaxKbps))
	}

	var key *Key
	if req.KeyURI != "" {
		var err error
		if key, err = ensureKey(req.MetaDir, req.KeyURI); err != nil {
			return nil, fmt.Errorf("encryption key: %w", err)
		}
		res.Encrypted = true
	}
	keyInfo := ""
	if key != nil {
		keyInfo = key.InfoPath
	}

	renditions := planRenditions(sm.Video, res.FastPath)
	plan := encodePlan{
		Input: req.Input, OutDir: req.OutDir, Video: sm.Video, Renditions: renditions,
		Audio: req.Audio.Tracks, Rate: t.Settings.rateFor(sm.SourceType), Settings: t.Settings,
		KeyInfo: keyInfo, Encoder: req.Encoder,
	}
	if plan.Encoder == "" {
		plan.Encoder = EncoderCPU
	}

	// -force_key_frames times count from the output's first video frame, as
	// do playlist segment offsets. Boundaries are in source time, so subtract
	// the source time of that first frame. (The container start offset that
	// ffmpeg also removes cancels out.)
	var forced []float64
	for _, kt := range intro.KeyframeTimes(sm.Duration, float64(t.Settings.SegmentDuration), req.Timestamps) {
		forced = append(forced, round3(max(0, kt-sm.Video.StartTime)))
	}
	plan.ForceKeyFrames = forced

	if renditions[0].Copy {
		err := t.fastPath(ctx, &plan)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			res.Warnings = append(res.Warnings, "fast path failed, re-encoding 1080p: "+err.Error())
			log.Warn("fast path failed, falling back to full encode", "err", err)
			res.FastPath = false
			renditions = planRenditions(sm.Video, false)
			plan.Renditions = renditions
		}
	}
	if !res.FastPath {
		if err := t.encodeWithCRFAdjust(ctx, &plan, sm, res, log); err != nil {
			return nil, err
		}
	}
	res.CRF, res.Encoder = plan.Rate.CRF, plan.Encoder

	// Validate everything ffmpeg wrote, collecting what the master needs.
	var videos []videoVariant
	for _, r := range renditions {
		pl, err := readPlaylist(filepath.Join(req.OutDir, r.Name, "stream.m3u8"))
		if err != nil {
			return nil, err
		}
		if pl.Encrypted != res.Encrypted {
			return nil, fmt.Errorf("%s: encryption is %v, want %v", r.Name, pl.Encrypted, res.Encrypted)
		}
		profile := "high"
		if r.Copy {
			profile = sm.Video.Profile
		}
		videos = append(videos, videoVariant{Rendition: r, Playlist: pl, Codec: avcCodec(profile, r.Level), FPS: sm.Video.FPS})
		res.Renditions = append(res.Renditions, r.Name)
	}
	var audios []audioRendition
	for _, tr := range req.Audio.Tracks {
		pl, err := readPlaylist(filepath.Join(req.OutDir, "audio", tr.Rendition, "stream.m3u8"))
		if err != nil {
			return nil, err
		}
		ar := audioRendition{Track: tr, Playlist: pl, Codec: "mp4a.40.2", Channels: t.Settings.AudioChannels}
		if tr.Action == "copy" {
			ar.Codec, ar.Channels = aacCodec(audioProfile(sm, tr.Index)), tr.Channels
		}
		audios = append(audios, ar)
	}

	// Subtitles: map cue time 0 (source) to the video's MPEG-TS clock.
	firstSeg := filepath.Join(req.OutDir, renditions[0].Name, videos[0].Playlist.Segments[0].URI)
	pts0, err := t.firstVideoPTS(ctx, firstSeg, key)
	if err != nil {
		return nil, fmt.Errorf("read first video PTS for subtitle sync: %w", err)
	}
	tsMap := pts0 - int64(math.Round(sm.Video.StartTime*90000))
	subs, warnings := t.writeSubtitles(ctx, req.SubsDir, req.OutDir, req.Subs, tsMap, sm.Duration)
	res.Warnings = append(res.Warnings, warnings...)

	res.Master = filepath.Join(req.OutDir, "master.m3u8")
	if err := writeMaster(res.Master, videos, audios, subs); err != nil {
		return nil, err
	}
	return res, nil
}

// fastPath (§7c): stream-copy the source as the top rendition together with
// the audio, read where the HLS muxer actually cut it, then encode the other
// renditions with keyframes forced at exactly those times.
func (t *Transcoder) fastPath(ctx context.Context, plan *encodePlan) error {
	copyPlan := *plan
	copyPlan.Renditions = plan.Renditions[:1]
	if err := t.runFFmpeg(ctx, &copyPlan); err != nil {
		return err
	}
	if len(plan.Renditions) == 1 {
		return nil
	}
	top, err := readPlaylist(filepath.Join(plan.OutDir, plan.Renditions[0].Name, "stream.m3u8"))
	if err != nil {
		return err
	}
	rest := *plan
	rest.Renditions, rest.Audio = plan.Renditions[1:], nil
	rest.ForceKeyFrames = top.cutTimes(0) // both count from the first frame
	err = t.runFFmpeg(ctx, &rest)
	plan.Encoder = rest.Encoder // a GPU→CPU fallback shows in the result
	return err
}

// encodeWithCRFAdjust encodes, then compares the top rendition's video size
// with the source's; outside 0.3×–1.5× it nudges CRF by one and re-encodes,
// at most twice, within [min, 24].
func (t *Transcoder) encodeWithCRFAdjust(ctx context.Context, plan *encodePlan, sm *media.StreamMap, res *Result, log *slog.Logger) error {
	sourceBytes := float64(sm.Video.BitRateKbps) * 125 * sm.Duration
	for pass := 1; ; pass++ {
		if err := t.runFFmpeg(ctx, plan); err != nil {
			return err
		}
		if sourceBytes <= 0 || pass >= maxCRFPasses {
			return nil
		}
		top, err := readPlaylist(filepath.Join(plan.OutDir, plan.Renditions[0].Name, "stream.m3u8"))
		if err != nil {
			return err
		}
		ratio := float64(top.TotalBytes()) / sourceBytes
		res.SizeRatio = round3(ratio)
		next := plan.Rate.CRF
		switch {
		case ratio > maxSizeRatio && next < maxCRF:
			next++
		case ratio < minSizeRatio && next > plan.Rate.MinCRF:
			next--
		}
		if next == plan.Rate.CRF {
			return nil
		}
		log.Info("output size off target, re-encoding", "ratio", res.SizeRatio, "crf", plan.Rate.CRF, "next_crf", next)
		res.Warnings = append(res.Warnings, fmt.Sprintf("size ratio %.2f at CRF %d, re-encoded at CRF %d", ratio, plan.Rate.CRF, next))
		plan.Rate.CQ += next - plan.Rate.CRF
		plan.Rate.CRF = next
	}
}

// runFFmpeg clears the plan's output dirs, runs it, and on a crash retries
// once single-threaded (§7: OOM/segfault). A failed GPU run is retried on
// the CPU instead (e.g. a codec NVDEC can't decode, or the GPU is busy/lost).
func (t *Transcoder) runFFmpeg(ctx context.Context, plan *encodePlan) error {
	attempt := func(p encodePlan) error {
		for _, dir := range p.outputDirs() {
			if err := os.RemoveAll(dir); err != nil {
				return err
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		_, err := media.Run(ctx, t.FFmpeg, p.args()...)
		return err
	}
	err := attempt(*plan)
	if err == nil || ctx.Err() != nil {
		return err
	}
	if plan.Encoder == EncoderNVENC {
		if t.Log != nil {
			t.Log.Warn("GPU encode failed, retrying on CPU", "err", err)
		}
		plan.Encoder = EncoderCPU
		if err = attempt(*plan); err == nil || ctx.Err() != nil {
			return err
		}
	}
	single := *plan // single-threaded only for this retry, not later CRF passes
	single.SingleThread = true
	if err2 := attempt(single); err2 != nil {
		return fmt.Errorf("ffmpeg failed twice (second run single-threaded): %w", err2)
	}
	return nil
}

func (p encodePlan) outputDirs() []string {
	var dirs []string
	for _, r := range p.Renditions {
		dirs = append(dirs, filepath.Join(p.OutDir, r.Name))
	}
	for _, a := range p.Audio {
		dirs = append(dirs, filepath.Join(p.OutDir, "audio", a.Rendition))
	}
	return dirs
}

func audioProfile(sm *media.StreamMap, index int) string {
	for _, a := range sm.Audio {
		if a.Index == index {
			return a.Profile
		}
	}
	return ""
}
