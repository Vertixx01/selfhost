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

// indexTmpl is the home page: request an anime by AniList ID, watch the
// jobs progress, and play what's packaged. All dynamic text goes through
// textContent (never innerHTML) — titles come from third-party sites.
var indexTmpl = template.Must(template.New("index").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>torClient</title><style>` + pageCSS + `
h2{font-size:16px;margin:28px 0 10px}
.card{border:1px solid var(--line);border-radius:8px;background:var(--card);padding:14px;margin-bottom:10px}
form{display:flex;flex-wrap:wrap;gap:12px;align-items:flex-end}
label{display:flex;flex-direction:column;gap:4px;font-size:13px;color:var(--muted)}
label.inline{flex-direction:row;align-items:center;gap:6px;color:var(--fg)}
input[type=number]{width:120px;padding:7px 8px;border:1px solid var(--line);border-radius:6px;background:var(--bg);color:var(--fg);font:inherit}
button{padding:8px 16px;border:0;border-radius:6px;background:var(--accent);color:#fff;font:inherit;font-weight:600;cursor:pointer}
button:disabled{opacity:.5;cursor:wait}
#msg{margin-top:10px;white-space:pre-wrap}
.err{color:#d33}
.job h3{font-size:15px;margin:0 0 8px;display:flex;justify-content:space-between;gap:8px;flex-wrap:wrap}
.ep{display:grid;grid-template-columns:56px 1fr 92px;gap:10px;align-items:center;padding:6px 0;border-top:1px solid var(--line)}
.ep:first-of-type{border-top:0}
.bar{height:8px;background:var(--line);border-radius:4px;overflow:hidden}.bar>i{display:block;height:100%;background:var(--accent)}
.ep .rel{grid-column:2/4;font-size:12px;color:var(--muted);overflow-wrap:anywhere}
.ep a.play{justify-self:end;font-weight:600}
.lib{display:flex;flex-wrap:wrap;gap:8px}.lib a{padding:6px 10px;border:1px solid var(--line);border-radius:6px;background:var(--card);text-decoration:none}
@media (max-width:520px){.ep{grid-template-columns:48px 1fr}.ep a.play,.ep .st{grid-column:2}}
</style></head><body><main>
<h1>torClient</h1>

<div class="card">
<form id="f">
<label>AniList ID<input id="aid" type="number" min="1" required placeholder="e.g. 154587"></label>
<label>First episode<input id="from" type="number" min="0" value="1" required></label>
<label>Episodes<input id="count" type="number" min="1" max="50" value="3" required></label>
<label class="inline"><input id="dub" type="checkbox"> Prefer English dub</label>
<button id="go">Download</button>
</form>
<div id="msg" class="muted">Finds a release for each episode on AnimeTosho, SeaDex, SubsPlease and nyaa.si, then downloads, transcodes and packages it. Movies: leave first episode at 1 and episodes at 1.</div>
</div>

<h2>Jobs</h2>
<div id="jobs"><p class="muted">No jobs yet.</p></div>

<h2>Library</h2>
{{if not .}}<p class="muted">Nothing packaged yet.</p>{{end}}
<div class="lib">{{range .}}<a href="/play/{{.AnimeID}}/{{.EpID}}">{{.AnimeID}} / {{.EpID}}</a>{{end}}</div>

<script>
const $ = id => document.getElementById(id);
const el = (tag, cls, text) => { const e = document.createElement(tag); if (cls) e.className = cls; if (text != null) e.textContent = text; return e; };
const picks = {}; // job_id -> {ep_id: release title}

$("f").onsubmit = async ev => {
  ev.preventDefault();
  const id = +$("aid").value, from = +$("from").value, count = +$("count").value;
  const episodes = Array.from({length: count}, (_, i) => from + i);
  const jobID = "al" + id + "-" + crypto.randomUUID().slice(0, 8);
  $("go").disabled = true;
  $("msg").className = "muted";
  $("msg").textContent = "Searching sources for episodes " + episodes[0] + "–" + episodes[episodes.length - 1] + "… (can take up to a minute)";
  try {
    const r = await fetch("/api/anilist/" + id + "/episodes", {method: "POST", headers: {"Content-Type": "application/json"},
      body: JSON.stringify({job_id: jobID, episodes, dub: $("dub").checked})});
    const d = await r.json();
    const disc = d.discovery || [];
    picks[jobID] = Object.fromEntries(disc.filter(x => x.picked).map(x => [x.ep_id, x.picked.title + "  [" + (x.picked.sources || []).join(", ") + ", " + x.picked.seeders + " seeders]"]));
    const missing = disc.filter(x => !x.picked).map(x => "ep" + x.episode + ": " + x.error);
    if (!r.ok) {
      $("msg").className = "err";
      $("msg").textContent = (d.error || ("HTTP " + r.status)) + (missing.length ? "\n" + missing.join("\n") : "");
    } else {
      $("msg").className = "muted";
      $("msg").textContent = "Queued " + d.job.episodes.length + " episode(s) of " + d.job.title + "." + (missing.length ? "\nSkipped:\n" + missing.join("\n") : "");
      refresh();
    }
  } catch (e) {
    $("msg").className = "err"; $("msg").textContent = "Request failed: " + e;
  } finally { $("go").disabled = false; }
};

function renderJob(job) {
  const card = el("div", "card job");
  const h = el("h3"); h.append(el("span", null, job.title + " (" + job.anime_id + ")"), el("span", "muted", job.status));
  card.append(h);
  for (const ep of job.episodes || []) {
    const row = el("div", "ep");
    row.append(el("strong", null, ep.ep_id));
    const bar = el("div", "bar"), fill = el("i"); fill.style.width = Math.round(ep.progress) + "%"; bar.append(fill);
    row.append(bar);
    if (ep.status === "done") {
      const a = el("a", "play", "▶ Play"); a.href = "/play/" + encodeURIComponent(job.anime_id) + "/" + encodeURIComponent(ep.ep_id); row.append(a);
    } else {
      row.append(el("span", "st " + (ep.status === "failed" ? "err" : "muted"), ep.status + " " + Math.round(ep.progress) + "%"));
    }
    const rel = (picks[job.job_id] || {})[ep.ep_id] || ep.title;
    row.append(el("div", "rel", (ep.error ? "Error: " + ep.error + "\n" : "") + (rel || "")));
    card.append(row);
  }
  return card;
}

async function refresh() {
  try {
    const list = (await (await fetch("/jobs?limit=10")).json()).jobs || [];
    if (!list.length) return;
    const full = await Promise.all(list.map(j => fetch("/job/" + encodeURIComponent(j.job_id)).then(r => r.json())));
    $("jobs").replaceChildren(...full.map(renderJob));
  } catch (e) { /* server restarting; try again next tick */ }
}
refresh();
setInterval(refresh, 2000);
</script>
</main></body></html>`))

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
