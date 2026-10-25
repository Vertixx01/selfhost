package transcode

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"torclient/source/media"
)

type videoVariant struct {
	Rendition Rendition
	Playlist  *MediaPlaylist
	Codec     string
	FPS       float64
}

type audioRendition struct {
	Track    media.AudioTrack
	Playlist *MediaPlaylist
	Codec    string
	Channels int
}

// writeMaster writes master.m3u8: one audio group, one subtitle group, and a
// variant per video rendition (highest first). BANDWIDTH is the peak segment
// bitrate of the video plus the most demanding audio rendition, as the HLS
// spec asks; AVERAGE-BANDWIDTH likewise with averages.
func writeMaster(path string, videos []videoVariant, audios []audioRendition, subs []subtitleOutput) error {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-INDEPENDENT-SEGMENTS\n")

	audioPeak, audioAvg := 0, 0.0
	var audioCodecs []string
	for _, a := range audios {
		t := a.Track
		fmt.Fprintf(&b, "#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID=\"audio\",LANGUAGE=\"%s\",NAME=\"%s\",DEFAULT=%s,AUTOSELECT=YES,CHANNELS=\"%d\",URI=\"audio/%s/stream.m3u8\"\n",
			media.BCP47(t.Lang), t.Name, yesNo(t.Default), a.Channels, t.Rendition)
		audioPeak = max(audioPeak, a.Playlist.PeakBitrate())
		audioAvg = max(audioAvg, a.Playlist.AverageBitrate())
		if !slices.Contains(audioCodecs, a.Codec) {
			audioCodecs = append(audioCodecs, a.Codec)
		}
	}

	defSub := defaultSubtitle(subs)
	names := map[string]int{}
	for i, s := range subs {
		name := media.LangName(s.Track.Lang)
		if s.Track.Kind == "signs" {
			name += " (Signs)"
		}
		if names[name]++; names[name] > 1 {
			name += " " + strconv.Itoa(names[name])
		}
		forced := s.Track.Kind == "signs" && s.Track.Forced
		fmt.Fprintf(&b, "#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"subs\",LANGUAGE=\"%s\",NAME=\"%s\",DEFAULT=%s,AUTOSELECT=YES,FORCED=%s,URI=\"%s\"\n",
			media.BCP47(s.Track.Lang), name, yesNo(i == defSub), yesNo(forced), s.Playlist)
	}

	for _, v := range videos {
		codecs := append([]string{v.Codec}, audioCodecs...)
		fmt.Fprintf(&b, "#EXT-X-STREAM-INF:BANDWIDTH=%d,AVERAGE-BANDWIDTH=%d,RESOLUTION=%dx%d,FRAME-RATE=%.3f,CODECS=\"%s\"",
			v.Playlist.PeakBitrate()+audioPeak, int(v.Playlist.AverageBitrate()+audioAvg),
			v.Rendition.Width, v.Rendition.Height, v.FPS, strings.Join(codecs, ","))
		if len(audios) > 0 {
			b.WriteString(",AUDIO=\"audio\"")
		}
		if len(subs) > 0 {
			b.WriteString(",SUBTITLES=\"subs\"")
		}
		fmt.Fprintf(&b, "\n%s/stream.m3u8\n", v.Rendition.Name)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// defaultSubtitle: English dialogue if present, else the first dialogue
// track, else the first track. (Anime is watched in Japanese with subs.)
func defaultSubtitle(subs []subtitleOutput) int {
	first := -1
	for i, s := range subs {
		if s.Track.Kind != "dialogue" {
			continue
		}
		if s.Track.Lang == "eng" {
			return i
		}
		if first < 0 {
			first = i
		}
	}
	if first < 0 && len(subs) > 0 {
		return 0
	}
	return first
}

func yesNo(b bool) string {
	if b {
		return "YES"
	}
	return "NO"
}
