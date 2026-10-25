package api

import (
	"html/template"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Playback for testing and small setups: serve output/ over HTTP and a
// minimal hls.js player. In production a CDN or nginx serves output/ and the
// frontend has its own player; this needs no extra moving parts.

var mediaTypes = map[string]string{
	".m3u8": "application/vnd.apple.mpegurl",
	".ts":   "video/mp2t",
	".vtt":  "text/vtt; charset=utf-8",
	".ass":  "text/plain; charset=utf-8",
	".json": "application/json",
	".ttf":  "font/ttf",
	".otf":  "font/otf",
}

// handleMedia serves GET /media/{path...} from MediaDir: files only, no
// directory listings, nothing under a dot-directory (.trash, .staging).
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	if s.MediaDir == "" {
		writeError(w, http.StatusNotFound, "media serving disabled")
		return
	}
	rel := path.Clean("/" + r.PathValue("path"))
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
	}
	f, err := os.Open(filepath.Join(s.MediaDir, filepath.FromSlash(rel)))
	if err != nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if ct, ok := mediaTypes[strings.ToLower(path.Ext(rel))]; ok {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if strings.HasSuffix(rel, ".m3u8") {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, st.Name(), st.ModTime(), f) // handles Range requests
}

type playEntry struct{ AnimeID, EpID string }

// handlePlayIndex lists every packaged episode (GET /play).
func (s *Server) handlePlayIndex(w http.ResponseWriter, r *http.Request) {
	if s.MediaDir == "" {
		writeError(w, http.StatusNotFound, "media serving disabled")
		return
	}
	var eps []playEntry
	animes, _ := os.ReadDir(s.MediaDir)
	for _, a := range animes {
		if !a.IsDir() || strings.HasPrefix(a.Name(), ".") {
			continue
		}
		entries, _ := os.ReadDir(filepath.Join(s.MediaDir, a.Name()))
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				if _, err := os.Stat(filepath.Join(s.MediaDir, a.Name(), e.Name(), "master.m3u8")); err == nil {
					eps = append(eps, playEntry{a.Name(), e.Name()})
				}
			}
		}
	}
	sort.Slice(eps, func(i, j int) bool {
		if eps[i].AnimeID != eps[j].AnimeID {
			return eps[i].AnimeID < eps[j].AnimeID
		}
		return naturalLess(eps[i].EpID, eps[j].EpID)
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	indexTmpl.Execute(w, eps)
}

// handlePlay serves the player page for one episode (GET /play/{anime_id}/{ep_id}).
func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	animeID, epID := r.PathValue("anime_id"), r.PathValue("ep_id")
	if s.MediaDir == "" || !validID(animeID) || !validID(epID) {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if _, err := os.Stat(filepath.Join(s.MediaDir, animeID, epID, "master.m3u8")); err != nil {
		writeError(w, http.StatusNotFound, "episode not packaged (yet)")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	playerTmpl.Execute(w, playEntry{animeID, epID})
}

// naturalLess sorts "ep2" before "ep10".
func naturalLess(a, b string) bool {
	na, nb := strings.TrimLeft(a, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ_-."), strings.TrimLeft(b, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ_-.")
	if len(na) != len(nb) && na != "" && nb != "" && a[:len(a)-len(na)] == b[:len(b)-len(nb)] {
		return len(na) < len(nb)
	}
	return a < b
}

const pageCSS = `
:root{color-scheme:light dark;--bg:#fafafa;--fg:#1a1a1a;--muted:#666;--card:#fff;--line:#ddd;--accent:#3b6fd8}
@media (prefers-color-scheme:dark){:root{--bg:#141414;--fg:#eee;--muted:#9a9a9a;--card:#1e1e1e;--line:#333;--accent:#7aa2ff}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,sans-serif}
main{max-width:960px;margin:0 auto;padding:24px 16px}a{color:var(--accent)}h1{font-size:20px;margin:0 0 16px}
.muted{color:var(--muted)}`

var playerTmpl = template.Must(template.New("player").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>{{.AnimeID}} {{.EpID}}</title><style>` + pageCSS + `
.stage{position:relative;background:#000;border-radius:8px;overflow:hidden}video{display:block;width:100%;aspect-ratio:16/9;background:#000}
#skip{position:absolute;right:16px;bottom:64px;display:none;padding:8px 14px;border:0;border-radius:6px;background:rgba(255,255,255,.9);color:#111;font-weight:600;cursor:pointer}
.controls{display:flex;flex-wrap:wrap;gap:12px;margin:12px 0}label{display:flex;gap:6px;align-items:center}
select{background:var(--card);color:var(--fg);border:1px solid var(--line);border-radius:6px;padding:4px 6px}
#info{white-space:pre-wrap;font:12px/1.4 ui-monospace,monospace}
</style></head><body><main>
<p><a href="/play">&larr; all episodes</a></p>
<h1>{{.AnimeID}} &middot; {{.EpID}}</h1>
<div class="stage"><video id="v" controls playsinline></video><button id="skip" type="button"></button></div>
<div class="controls">
<label>Quality <select id="q"><option value="-1">Auto</option></select></label>
<label>Audio <select id="a"></select></label>
<label>Subtitles <select id="s"><option value="-1">Off</option></select></label>
</div>
<p id="info" class="muted"></p>
<script src="https://cdn.jsdelivr.net/npm/hls.js@1.5.15/dist/hls.min.js" integrity="sha384-oJR5PTkR0gvqJ/UnX1IkFQgxg6BByX7jPWmgSSY5dz0+Yc7O3mloo717zZpF/xpb" crossorigin="anonymous"></script>
<script>
const base = "/media/{{.AnimeID}}/{{.EpID}}/";
const v = document.getElementById("v"), q = document.getElementById("q"), a = document.getElementById("a"), s = document.getElementById("s");
const skip = document.getElementById("skip"), info = document.getElementById("info");
function opt(sel, value, text, on) { const o = new Option(text, value); o.selected = !!on; sel.add(o); }
if (window.Hls && Hls.isSupported()) {
  const hls = new Hls({ subtitleDisplay: true });
  hls.loadSource(base + "master.m3u8");
  hls.attachMedia(v);
  hls.on(Hls.Events.MANIFEST_PARSED, () => {
    hls.levels.forEach((l, i) => opt(q, i, l.height + "p (" + Math.round(l.bitrate / 1000) + " kbit/s)"));
  });
  // Track lists arrive in their own events, after MANIFEST_PARSED.
  hls.on(Hls.Events.AUDIO_TRACKS_UPDATED, () => {
    a.length = 0;
    hls.audioTracks.forEach((t, i) => opt(a, i, t.name + " [" + t.lang + "]", i === hls.audioTrack));
  });
  hls.on(Hls.Events.SUBTITLE_TRACKS_UPDATED, () => {
    s.length = 1;
    hls.subtitleTracks.forEach((t, i) => opt(s, i, t.name + " [" + t.lang + "]", t.default));
    if (hls.subtitleTrack < 0) { const d = hls.subtitleTracks.findIndex(t => t.default); if (d >= 0) { hls.subtitleTrack = d; s.value = d; } }
  });
  q.onchange = () => { hls.currentLevel = +q.value; };
  a.onchange = () => { hls.audioTrack = +a.value; };
  s.onchange = () => { hls.subtitleTrack = +s.value; };
  hls.on(Hls.Events.ERROR, (_, d) => { if (d.fatal) info.textContent = "Playback error: " + d.type + " / " + d.details; });
} else if (v.canPlayType("application/vnd.apple.mpegurl")) {
  v.src = base + "master.m3u8"; // Safari: native HLS
}
fetch(base + "meta/timestamps.json").then(r => r.ok ? r.json() : null).then(ts => {
  if (!ts) return;
  const parts = [["intro", "Skip intro"], ["outro", "Skip outro"]].filter(([k]) => ts[k]);
  info.textContent = parts.length ? parts.map(([k]) => k + ": " + ts[k].start.toFixed(1) + "–" + ts[k].end.toFixed(1) + "s (" + ts[k].source + ")").join("   ") : "No intro/outro detected.";
  v.addEventListener("timeupdate", () => {
    const hit = parts.find(([k]) => v.currentTime >= ts[k].start && v.currentTime < ts[k].end - 1);
    skip.style.display = hit ? "block" : "none";
    if (hit) { skip.textContent = hit[1]; skip.onclick = () => { v.currentTime = ts[hit[0]].end; }; }
  });
});
</script></main></body></html>`))
