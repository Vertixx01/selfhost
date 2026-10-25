package api

import "html/template"

// indexTmpl is the home page: search an anime by AniList ID with options,
// choose a torrent per episode (or let it auto-pick), follow the jobs, and
// manage the library. All dynamic text goes through textContent (never
// innerHTML): release titles come from third-party sites.
var indexTmpl = template.Must(template.New("index").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>torClient</title><style>` + pageCSS + `
h2{font-size:16px;margin:28px 0 10px;display:flex;justify-content:space-between;align-items:baseline;gap:8px;flex-wrap:wrap}
.card{border:1px solid var(--line);border-radius:8px;background:var(--card);padding:14px;margin-bottom:10px}
.row{display:flex;flex-wrap:wrap;gap:12px;align-items:flex-end}
label{display:flex;flex-direction:column;gap:4px;font-size:13px;color:var(--muted)}
label.inline{flex-direction:row;align-items:center;gap:6px;color:var(--fg)}
input[type=number],input[type=text],select{padding:7px 8px;border:1px solid var(--line);border-radius:6px;background:var(--bg);color:var(--fg);font:inherit}
input[type=number]{width:110px}input[type=text]{width:220px;max-width:100%}
details{margin-top:12px}summary{cursor:pointer;color:var(--accent)}
.opts{display:flex;flex-wrap:wrap;gap:12px 20px;margin-top:10px}
button{padding:8px 14px;border:0;border-radius:6px;background:var(--accent);color:#fff;font:inherit;font-weight:600;cursor:pointer}
button.ghost{background:transparent;color:var(--accent);border:1px solid var(--line);font-weight:500;padding:4px 10px}
button.danger{background:transparent;color:#d33;border:1px solid var(--line);font-weight:500;padding:4px 10px}
button:disabled{opacity:.5;cursor:wait}
#msg{margin-top:10px;white-space:pre-wrap}.err{color:#d33}
.tbl{overflow-x:auto;margin:6px 0 14px}
table{border-collapse:collapse;width:100%;font-size:13px}
th,td{text-align:left;padding:5px 8px;border-top:1px solid var(--line);vertical-align:top}
th{color:var(--muted);font-weight:500;white-space:nowrap}
td.t{min-width:260px;overflow-wrap:anywhere}td.n{white-space:nowrap;text-align:right}
tr.off td{color:var(--muted)}
.tag{display:inline-block;padding:0 6px;border-radius:4px;border:1px solid var(--line);font-size:11px;margin-left:4px}
.job h3{font-size:15px;margin:0 0 8px;display:flex;justify-content:space-between;gap:8px;flex-wrap:wrap;align-items:center}
.acts{display:flex;gap:6px;flex-wrap:wrap}
.ep{display:grid;grid-template-columns:56px 1fr auto;gap:10px;align-items:center;padding:6px 0;border-top:1px solid var(--line)}
.ep:first-of-type{border-top:0}
.bar{height:8px;background:var(--line);border-radius:4px;overflow:hidden}.bar>i{display:block;height:100%;background:var(--accent)}
.ep .rel{grid-column:2/4;font-size:12px;color:var(--muted);overflow-wrap:anywhere;white-space:pre-wrap}
.lib{display:flex;flex-direction:column;gap:6px}
.lib .an{font-weight:600;margin-top:6px}
.lib .it{display:flex;align-items:center;gap:8px;flex-wrap:wrap}
@media (max-width:520px){.ep{grid-template-columns:48px 1fr}.ep>:nth-child(3){grid-column:2}}
</style></head><body><main>
<h1>torClient</h1>
<p id="storage" class="muted"></p>

<div class="card">
<div class="row">
<label>AniList ID<input id="aid" type="number" min="1" required placeholder="e.g. 154587"></label>
<label>First episode<input id="from" type="number" min="0" value="1"></label>
<label>Episodes<input id="count" type="number" min="1" max="50" value="3"></label>
<button id="search" type="button">Search</button>
<button id="quick" type="button" class="ghost">Quick download (auto-pick)</button>
</div>
<details><summary>Search options</summary>
<div class="opts">
<label>Resolution<select id="res"><option value="">Server default</option><option>1080</option><option>720</option><option>480</option><option value="2160">2160 (4K)</option><option value="any">Any</option></select></label>
<label>Preferred groups (comma-separated, in order)<input id="groups" type="text" placeholder="SubsPlease, Erai-raws"></label>
<label>Exclude words<input id="exclude" type="text" placeholder="x265, dub, REMUX"></label>
<label class="inline"><input id="dub" type="checkbox"> Prefer English dub</label>
<label class="inline"><input id="h264" type="checkbox"> Prefer H.264 (lighter to decode)</label>
<div><div class="muted" style="font-size:13px;margin-bottom:4px">Sources</div><div class="opts" id="srcs" style="margin-top:0"></div></div>
</div>
</details>
<div id="msg" class="muted">Search shows the torrents found for each episode so you can choose; Quick download picks automatically. Movies: first episode 1, episodes 1.</div>
</div>

<div id="results"></div>

<h2>Jobs</h2>
<div id="jobs"><p class="muted">No jobs yet.</p></div>

<h2>Library</h2>
{{if not .}}<p class="muted">Nothing packaged yet.</p>{{end}}
<div class="lib">{{range .}}<div class="it" data-anime="{{.AnimeID}}" data-ep="{{.EpID}}"><a href="/play/{{.AnimeID}}/{{.EpID}}">{{.AnimeID}} / {{.EpID}}</a><button class="danger del" type="button">Delete</button></div>{{end}}</div>

<script>
const $ = id => document.getElementById(id);
const el = (tag, cls, text) => { const e = document.createElement(tag); if (cls) e.className = cls; if (text != null) e.textContent = text; return e; };
const SOURCES = ["animetosho", "seadex", "subsplease", "nyaa", "yameii", "toonshub"];
for (const s of SOURCES) { const l = el("label", "inline"); const c = el("input"); c.type = "checkbox"; c.checked = true; c.value = s; l.append(c, s); $("srcs").append(l); }
const csv = v => v.split(",").map(x => x.trim()).filter(Boolean);
const gbs = b => (b / 1073741824).toFixed(b >= 1073741824 * 10 ? 0 : 1) + " GB";
const mbs = b => b ? (b >= 1073741824 ? (b / 1073741824).toFixed(2) + " GB" : Math.round(b / 1048576) + " MB") : "?";
const ago = d => { const s = (Date.now() - new Date(d)) / 1000; if (!(s >= 0)) return ""; if (s < 3600) return Math.round(s / 60) + "m"; if (s < 86400) return Math.round(s / 3600) + "h"; if (s < 86400 * 60) return Math.round(s / 86400) + "d"; return Math.round(s / 86400 / 30) + "mo"; };
const say = (text, err) => { $("msg").className = err ? "err" : "muted"; $("msg").textContent = text; };

function options() {
  const sources = [...$("srcs").querySelectorAll("input:checked")].map(c => c.value);
  return {resolution: $("res").value, groups: csv($("groups").value), exclude: csv($("exclude").value),
    sources: sources.length === SOURCES.length ? [] : sources, dub: $("dub").checked, prefer_h264: $("h264").checked};
}
function episodes() {
  const from = +$("from").value, count = +$("count").value;
  return Array.from({length: Math.max(1, count)}, (_, i) => from + i);
}
function busy(on) { $("search").disabled = $("quick").disabled = on; }

let lastSearch = null; // {id, show, eps: [{episode, result}]}

$("search").onclick = async () => {
  const id = +$("aid").value; if (!id) return say("Enter an AniList ID.", true);
  const o = options(), eps = episodes();
  const qs = new URLSearchParams({resolution: o.resolution, groups: o.groups.join(","), exclude: o.exclude.join(","),
    sources: o.sources.join(","), dub: o.dub ? "1" : "", h264: o.prefer_h264 ? "1" : ""});
  busy(true); say("Searching " + eps.length + " episode(s)… (up to a minute)");
  $("results").replaceChildren();
  try {
    const out = [];
    for (let i = 0; i < eps.length; i += 3) { // 3 at a time: nyaa.si rate-limits
      out.push(...await Promise.all(eps.slice(i, i + 3).map(async ep => {
        const r = await fetch("/api/anilist/" + id + "/search?episode=" + ep + "&" + qs);
        const d = await r.json(); if (!r.ok) throw new Error(d.error || ("HTTP " + r.status));
        return {episode: ep, result: d};
      })));
    }
    lastSearch = {id, show: out[0].result.show, eps: out};
    renderResults();
    say("Choose a release for each episode, then Download selected.");
  } catch (e) { say("Search failed: " + e.message, true); } finally { busy(false); }
};

function renderResults() {
  const box = el("div", "card");
  const show = lastSearch.show;
  const h = el("h2"); h.style.marginTop = "0";
  h.append(el("span", null, (show.titles || [])[0] + " — AniList " + show.anilist_id + (show.episodes ? " · " + show.episodes + " episodes" : "")));
  const go = el("button", null, "Download selected"); go.type = "button"; go.onclick = downloadSelected; h.append(go);
  box.append(h);
  for (const {episode, result} of lastSearch.eps) {
    const label = result.mode === "movie" ? "Movie" : "Episode " + episode;
    const cands = result.candidates || [];
    box.append(el("strong", null, label + " — " + (cands.length ? cands.length + " found" : "nothing found") + (result.picked ? "" : " · no automatic pick")));
    const errs = Object.entries(result.source_errors || {}).map(([s, e]) => s + ": " + e);
    if (errs.length) box.append(el("div", "muted", "Source errors: " + errs.join(" · ")));
    if (!cands.length) continue;
    const wrap = el("div", "tbl"), t = el("table"), head = el("tr");
    for (const c of ["", "Release", "Res", "Codec", "Size", "Seeders", "Age", "Found on"]) head.append(el("th", null, c));
    t.append(head);
    cands.slice(0, 20).forEach((c, i) => {
      const tr = el("tr", c.pickable ? null : "off");
      const td0 = el("td"), r = el("input"); r.type = "radio"; r.name = "ep" + episode; r.value = i;
      r.disabled = c.batch; r.checked = result.picked && c.infohash === result.picked.infohash;
      td0.append(r); tr.append(td0);
      const tt = el("td", "t", c.title);
      if (c.match !== "exact") tt.append(el("span", "tag", c.match));
      if (c.why) tt.title = c.why;
      tr.append(tt, el("td", "n", c.resolution ? c.resolution + "p" : "?"), el("td", null, c.codec || "?"),
        el("td", "n", mbs(c.size_bytes)), el("td", "n", c.sources.some(s => ["subsplease", "seadex"].includes(s)) && !c.seeders ? "?" : String(c.seeders)),
        el("td", "n", ago(c.date)), el("td", null, c.sources.join(", ")));
      t.append(tr);
    });
    wrap.append(t); box.append(wrap);
  }
  $("results").replaceChildren(box);
}

async function submit(picks) {
  const id = lastSearch && picks ? lastSearch.id : +$("aid").value;
  if (!id) return say("Enter an AniList ID.", true);
  const eps = picks ? lastSearch.eps.map(e => e.episode) : episodes();
  const body = {job_id: "al" + id + "-" + crypto.randomUUID().slice(0, 8), episodes: eps, ...options()};
  if (picks) body.picks = picks;
  busy(true); say(picks ? "Queuing your selection…" : "Searching and queuing… (up to a minute)");
  try {
    const r = await fetch("/api/anilist/" + id + "/episodes", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify(body)});
    const d = await r.json();
    const missing = (d.discovery || []).filter(x => !x.picked).map(x => "ep" + x.episode + ": " + x.error);
    if (!r.ok) return say((d.error || ("HTTP " + r.status)) + (missing.length ? "\n" + missing.join("\n") : ""), true);
    say("Queued " + d.job.episodes.length + " episode(s) of " + d.job.title + "." + (missing.length ? "\nSkipped:\n" + missing.join("\n") : ""));
    $("results").replaceChildren();
    refresh();
  } catch (e) { say("Request failed: " + e.message, true); } finally { busy(false); }
}
$("quick").onclick = () => submit(null);
function downloadSelected() {
  const picks = {};
  for (const {episode, result} of lastSearch.eps) {
    const r = document.querySelector('input[name="ep' + episode + '"]:checked');
    if (!r) continue;
    const c = result.candidates[+r.value];
    picks[String(episode)] = {magnet: c.magnet, title: c.title, source: c.sources[0] || ""};
  }
  if (!Object.keys(picks).length) return say("Select at least one release.", true);
  lastSearch.eps = lastSearch.eps.filter(e => picks[String(e.episode)]);
  submit(picks);
}

async function act(method, url, confirmText) {
  if (confirmText && !confirm(confirmText)) return;
  const r = await fetch(url, {method});
  if (!r.ok && r.status !== 204) { const d = await r.json().catch(() => ({})); alert(d.error || ("HTTP " + r.status)); }
  refresh();
}
function button(text, cls, onclick) { const b = el("button", cls, text); b.type = "button"; b.onclick = onclick; return b; }

function renderJob(job) {
  const card = el("div", "card job");
  const h = el("h3"); h.append(el("span", null, job.title + " (" + job.anime_id + ") · " + job.status));
  const acts = el("div", "acts");
  const running = (job.episodes || []).some(e => !["done", "failed"].includes(e.status));
  const failed = (job.episodes || []).some(e => e.status === "failed");
  const jid = encodeURIComponent(job.job_id);
  if (running) acts.append(button("Cancel", "danger", () => act("POST", "/job/" + jid + "/cancel", "Cancel the unfinished episodes of this job?")));
  if (failed && !running) acts.append(button("Retry failed", "ghost", () => act("POST", "/job/" + jid + "/retry")));
  if (!running) acts.append(button("Remove", "ghost", () => act("DELETE", "/job/" + jid, "Remove this job from the list? Packaged episodes stay in the library.")));
  h.append(acts); card.append(h);
  for (const ep of job.episodes || []) {
    const row = el("div", "ep");
    row.append(el("strong", null, ep.ep_id));
    const bar = el("div", "bar"), fill = el("i"); fill.style.width = Math.round(ep.progress) + "%"; bar.append(fill);
    row.append(bar);
    if (ep.status === "done") {
      const a = el("a", null, "▶ Play"); a.href = "/play/" + encodeURIComponent(job.anime_id) + "/" + encodeURIComponent(ep.ep_id); row.append(a);
    } else if (ep.status === "failed" && !running) {
      row.append(button("Retry", "ghost", () => fetch("/job/" + jid + "/retry", {method: "POST", headers: {"Content-Type": "application/json"}, body: JSON.stringify({episodes: [ep.ep_id]})}).then(refresh)));
    } else {
      row.append(el("span", ep.status === "failed" ? "err" : "muted", ep.status + " " + Math.round(ep.progress) + "%"));
    }
    row.append(el("div", "rel", (ep.error ? "Error: " + ep.error + "\n" : "") + (ep.title || "")));
    card.append(row);
  }
  return card;
}

document.querySelectorAll(".lib .del").forEach(b => b.onclick = () => {
  const it = b.closest(".it"), a = it.dataset.anime, e = it.dataset.ep;
  act("DELETE", "/api/anime/" + encodeURIComponent(a) + "/episodes/" + encodeURIComponent(e), "Delete " + a + " / " + e + " from disk?").then(() => it.remove());
});

async function refresh() {
  try {
    const st = await (await fetch("/api/storage")).json();
    $("storage").textContent = "Storage: " + gbs(st.used_bytes || 0) + (st.limit_bytes ? " of " + gbs(st.limit_bytes) + " limit" : "") +
      (st.disk_free_bytes != null ? " · disk free " + gbs(st.disk_free_bytes) : "") + (st.auto_delete ? " · oldest episodes auto-deleted at the limit" : "");
    const list = (await (await fetch("/jobs?limit=10")).json()).jobs || [];
    if (!list.length) { $("jobs").replaceChildren(el("p", "muted", "No jobs yet.")); return; }
    const full = await Promise.all(list.map(j => fetch("/job/" + encodeURIComponent(j.job_id)).then(r => r.json())));
    $("jobs").replaceChildren(...full.map(renderJob));
  } catch (e) { /* server restarting; next tick */ }
}
refresh();
setInterval(refresh, 2500);
</script>
</main></body></html>`))
