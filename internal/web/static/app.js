"use strict";

/* ============================================================
 * bgplay·vis — live BGP visualizer
 * Layout: local AS at the center, peers on ring 1, transit ASes
 * on further rings. Announce = particle flying along the AS path
 * into a glowing prefix-dot orbiting the origin AS. Withdraw =
 * red particle racing back out.
 * ============================================================ */

const cv = document.getElementById("viz");
const cx2d = cv.getContext("2d");
const feedEl = document.getElementById("feedrows");
const peersEl = document.getElementById("peers");
const detailEl = document.getElementById("detail");
const detailTitle = document.getElementById("detail-title");
const detailBody = document.getElementById("detail-body");
const searchEl = document.getElementById("search");
const connEl = document.getElementById("connstate");
const spark = document.getElementById("spark").getContext("2d");

let W = 0, H = 0, DPR = 1;
function resize() {
  DPR = Math.min(window.devicePixelRatio || 1, 2);
  W = window.innerWidth; H = window.innerHeight;
  cv.width = W * DPR; cv.height = H * DPR;
  cx2d.setTransform(DPR, 0, 0, DPR, 0, 0);
  relayoutAll();
}
window.addEventListener("resize", resize);

const CX = () => W / 2 + 90;   // optical center shifted right of the feed
const CY = () => H / 2 + 20;
const MINWH = () => Math.min(W, H);

/* ---------- graph model ---------- */

const nodes = new Map();     // asn -> node
const edges = new Map();     // "a>b" -> {a,b,heat}
const particles = [];        // in-flight updates
const dots = new Map();      // "peer|prefix" -> prefix dot orbiting origin
const flashes = [];          // radial flashes {x,y,t,color}
const MAX_DOTS = 7000;
const MAX_PARTICLES = 900;
let localAsn = 0;
let highlight = null;        // {asns:Set, edges:Set} from search
let hoverNode = null;

function ringRadius(r) { return MINWH() * (0.10 + Math.min(r, 8) * 0.105); }

function asnHue(asn) { return (asn * 47 + (asn >> 8) * 13) % 360; }

function nodeFor(asn, ring) {
  let n = nodes.get(asn);
  if (!n) {
    const a = Math.random() * Math.PI * 2;
    n = {
      asn, ring, angle: a, count: 0, glow: 0, flashT: 0, flashC: "#3fb950",
      x: CX() + Math.cos(a) * ringRadius(ring),
      y: CY() + Math.sin(a) * ringRadius(ring),
      tx: 0, ty: 0, r: 4, hue: asnHue(asn), label: "AS" + asn, hi: false,
    };
    nodes.set(asn, n);
    relayoutRing(ring);
  } else if (ring < n.ring) {
    const old = n.ring;
    n.ring = ring;
    relayoutRing(ring); relayoutRing(old);
  }
  return n;
}

function relayoutRing(ring) {
  const ringNodes = [...nodes.values()].filter(n => n.ring === ring);
  ringNodes.sort((a, b) => a.angle - b.angle);
  const R = ringRadius(ring);
  const step = (Math.PI * 2) / Math.max(ringNodes.length, 1);
  const jitter = Math.min(step * 0.3, 0.5);
  ringNodes.forEach((n, i) => {
    const a = i * step + (hashN(n.asn) - 0.5) * jitter;
    n.tx = CX() + Math.cos(a) * R;
    n.ty = CY() + Math.sin(a) * R;
  });
}

function relayoutAll() {
  const rings = new Set([...nodes.values()].map(n => n.ring));
  rings.forEach(relayoutRing);
}

function hashN(x) { x = (x ^ 61) ^ (x >>> 16); x = (x + (x << 3)) >>> 0; x = x ^ (x >>> 4); x = Math.imul(x, 0x27d4eb2d); x = x ^ (x >>> 15); return (x >>> 0) / 4294967295; }

function edgeFor(a, b) {
  const k = a < b ? a + ">" + b : b + ">" + a;
  let e = edges.get(k);
  if (!e) { e = { a: nodes.get(a), b: nodes.get(b), heat: 0 }; edges.set(k, e); }
  return e;
}

function ensurePath(path) {
  // path[0] = adjacent peer AS (ring 1), path[last] = origin
  const out = [];
  for (let i = 0; i < path.length; i++) {
    const n = nodeFor(path[i], i + 1);
    out.push(n);
    if (i > 0) edgeFor(path[i - 1], path[i]).heat = Math.min(1, edgeFor(path[i - 1], path[i]).heat + 0.15);
  }
  return out;
}

/* ---------- particles & dots ---------- */

function spawnParticle(pathNodes, color, reverse) {
  if (particles.length > MAX_PARTICLES) return;
  const ns = reverse ? [...pathNodes].reverse() : pathNodes;
  if (ns.length === 0) return;
  particles.push({ ns, seg: 0, t: 0, color, speed: 2.2 + Math.random() * 1.6, trail: [] });
}

function spawnDot(key, originNode, type) {
  if (dots.size >= MAX_DOTS) return;
  if (dots.has(key)) {
    const d = dots.get(key);
    d.dying = 0; d.hue = type === "change" ? 42 : 150;
    d.node = originNode;
    return;
  }
  dots.set(key, {
    node: originNode, key,
    ang: Math.random() * Math.PI * 2,
    rad: 9 + Math.random() * 26 + Math.min(originNode.count * 0.15, 18),
    size: 1.6 + Math.random() * 1.4,
    spin: 0.15 + Math.random() * 0.4,
    hue: type === "change" ? 42 : 150,
    born: performance.now(), dying: 0, dt: 0,
  });
}

function killDot(key) {
  const d = dots.get(key);
  if (d) { d.dying = 1; d.dt = 0; }
}

function flash(n, color) { flashes.push({ n, t: 0, color }); }

/* ---------- event handling ---------- */

const FEED_MAX = 140;

function handleEvent(e, replay) {
  switch (e.type) {
    case "announce":
    case "change": {
      const ns = ensurePath(e.path || []);
      const color = e.type === "change" ? "#d29922" : "#3fb950";
      if (!replay || Math.random() < 0.35) spawnParticle(ns, color);
      if (ns.length) {
        ns[ns.length - 1].count++;
        ns[ns.length - 1].glow = 1;
        ns[0].glow = 1;
        spawnDot(e.peer + "|" + e.pfx, ns[ns.length - 1], e.type);
      }
      if (!replay) feedRow(e);
      break;
    }
    case "withdraw": {
      const ns = ensurePath(e.path || []);
      spawnParticle(ns, "#f85149", true);
      if (ns.length) {
        const o = ns[ns.length - 1];
        o.count = Math.max(0, o.count - 1);
        o.flashT = 0; o.flashC = "#f85149"; flash(o, "#f85149");
      }
      killDot(e.peer + "|" + e.pfx);
      if (!replay) feedRow(e);
      break;
    }
    case "peer_up":
      if (e.pa) nodeFor(e.pa, 1);
      feedRow(e); toast(`peer up — AS${e.pa} (${e.peer})`);
      break;
    case "peer_down":
      feedRow(e); toast(`peer down — AS${e.pa || "?"} (${e.peer})`);
      break;
    case "eor": feedRow(e); break;
    case "flush": feedRow(e); break;
    case "peer_state": break;
  }
}

function feedRow(e) {
  const time = new Date(e.ts || Date.now()).toTimeString().slice(0, 8);
  const row = document.createElement("div");
  row.className = "frow " + e.type;
  let ty = e.type, msg = "";
  switch (e.type) {
    case "announce": ty = "ANN"; msg = `<b>${e.pfx}</b> <span class="path">${pathStr(e.path)}</span>`; break;
    case "withdraw": ty = "WDN"; msg = `<b>${e.pfx}</b> <span class="path">${pathStr(e.path)}</span>`; break;
    case "change":   ty = "CHG"; msg = `<b>${e.pfx}</b> <span class="path">${pathStr(e.path)}</span>`; break;
    case "peer_up":  ty = "UP";  msg = `AS${e.pa} ${e.peer}`; break;
    case "peer_down":ty = "DOWN";msg = `AS${e.pa ?? ""} ${e.peer} ${e.reason ?? ""}`; break;
    case "eor":      ty = "EOR"; msg = `${e.peer} ${e.fam} synced`; break;
    case "flush":    ty = "FLSH";msg = `${e.peer} −${e.n} stale`; break;
    default: return;
  }
  row.innerHTML = `<span class="t">${time}</span><span class="ty">${ty}</span><span class="msg">${msg}</span>`;
  if (e.pfx) row.onclick = () => { searchEl.value = e.pfx; doSearch(); };
  feedEl.prepend(row);
  while (feedEl.children.length > FEED_MAX) feedEl.lastChild.remove();
}

function pathStr(p) { return (p || []).map(a => "AS" + a).join("›"); }

function toast(text) {
  const t = document.createElement("div");
  t.className = "toastmsg"; t.textContent = text;
  document.getElementById("toast").appendChild(t);
  setTimeout(() => t.remove(), 4000);
}

/* ---------- websocket ---------- */

let ws, backoff = 1000;
function connect() {
  ws = new WebSocket((location.protocol === "https:" ? "wss://" : "ws://") + location.host + "/ws");
  ws.onopen = () => { backoff = 1000; connEl.textContent = "live"; connEl.className = "ok"; };
  ws.onclose = () => {
    connEl.textContent = "reconnecting"; connEl.className = "bad";
    setTimeout(connect, backoff); backoff = Math.min(backoff * 1.6, 10000);
  };
  ws.onmessage = (m) => {
    const msg = JSON.parse(m.data);
    if (msg.type === "snapshot") {
      onStats(msg.stats, msg.peers);
      (msg.recent || []).forEach(e => handleEvent(e, true));
    } else if (msg.type === "events") {
      for (const e of msg.items) handleEvent(e, false);
    } else if (msg.type === "stats") {
      onStats(msg.stats, msg.peers);
    }
  };
}

function fmt(n) {
  if (n >= 1e6) return (n / 1e6).toFixed(2) + "M";
  if (n >= 1e3) return (n / 1e3).toFixed(1) + "k";
  return "" + n;
}

function onStats(s, peers) {
  if (s.local_asn) localAsn = s.local_asn;
  document.getElementById("stat-prefixes").textContent = fmt(s.prefixes) + " routes";
  document.getElementById("stat-rate").textContent = s.rate + " ev/s";
  drawSpark(s.rate_series || []);
  renderPeers(peers || []);
}

function renderPeers(peers) {
  peersEl.innerHTML = "";
  for (const p of peers) {
    const chip = document.createElement("div");
    const up = p.established;
    const mid = !up && p.info.state !== "down";
    chip.className = "peerchip " + (up ? "up" : mid ? "mid" : "down");
    const fams = Object.keys(p.eor || {}).length;
    const syncTxt = up ? (fams >= 2 ? '<span class="sync">●</span>' : `<span class="ct">${fams}/2</span>`) : "";
    chip.innerHTML = `<span class="st"></span><span class="nm">${esc(p.info.name || "AS" + (p.info.asn || "?"))}</span><span class="ct">${fmt(p.prefixes)}</span>${syncTxt}`;
    chip.title = `${p.info.addr} · ${p.info.state} · ${p.prefixes} prefixes`;
    chip.onclick = () => { searchEl.value = "as" + p.info.asn; doSearch(); };
    peersEl.appendChild(chip);
  }
}

function drawSpark(series) {
  const w = 110, h = 22;
  spark.clearRect(0, 0, w, h);
  if (!series.length) return;
  const max = Math.max(5, ...series);
  spark.strokeStyle = "#39c5cf"; spark.lineWidth = 1;
  spark.beginPath();
  series.forEach((v, i) => {
    const x = (i / (series.length - 1)) * w;
    const y = h - 2 - (v / max) * (h - 4);
    i ? spark.lineTo(x, y) : spark.moveTo(x, y);
  });
  spark.stroke();
}

function esc(s) { return String(s).replace(/[<>&]/g, c => ({ "<": "&lt;", ">": "&gt;", "&": "&amp;" }[c])); }

/* ---------- search & detail ---------- */

let searchTimer = 0;
searchEl.addEventListener("input", () => {
  clearTimeout(searchTimer);
  searchTimer = setTimeout(doSearch, 250);
});
searchEl.addEventListener("keydown", e => { if (e.key === "Escape") clearSearch(); });
document.getElementById("detail-close").onclick = clearSearch;

function clearSearch() {
  searchEl.value = "";
  highlight = null;
  detailEl.classList.add("hidden");
  for (const n of nodes.values()) n.hi = false;
}

async function doSearch() {
  const q = searchEl.value.trim();
  if (!q) return clearSearch();
  const res = await fetch("/api/search?q=" + encodeURIComponent(q)).then(r => r.json());
  detailEl.classList.remove("hidden");
  detailTitle.textContent = `${res.kind}: ${res.query} — ${res.total} route${res.total === 1 ? "" : "s"}${res.truncated ? " (truncated)" : ""}`;
  detailBody.innerHTML = "";
  highlight = { asns: new Set(), edges: new Set() };
  for (const n of nodes.values()) n.hi = false;

  if (!res.routes.length) {
    detailBody.innerHTML = `<div style="color:var(--dim);padding:8px">no matching routes in adj-rib-in</div>`;
    return;
  }
  for (const rt of res.routes) {
    (rt.path || []).forEach(a => highlight.asns.add(a));
    for (let i = 1; i < (rt.path || []).length; i++)
      highlight.edges.add(rt.path[i - 1] < rt.path[i] ? rt.path[i - 1] + ">" + rt.path[i] : rt.path[i] + ">" + rt.path[i - 1]);
    const card = document.createElement("div");
    card.className = "routecard";
    card.innerHTML = routeCardHTML(rt);
    card.onclick = () => {
      document.querySelectorAll(".routecard.sel").forEach(x => x.classList.remove("sel"));
      card.classList.add("sel");
      isolateHighlight(rt);
    };
    detailBody.appendChild(card);
  }
  // ensure highlighted nodes exist in the graph
  for (const rt of res.routes.slice(0, 60)) ensurePath(rt.path || []);
  for (const n of nodes.values()) n.hi = highlight.asns.has(n.asn);
}

function isolateHighlight(rt) {
  highlight = { asns: new Set(rt.path || []), edges: new Set() };
  for (let i = 1; i < (rt.path || []).length; i++)
    highlight.edges.add(rt.path[i - 1] < rt.path[i] ? rt.path[i - 1] + ">" + rt.path[i] : rt.path[i] + ">" + rt.path[i - 1]);
  for (const n of nodes.values()) n.hi = highlight.asns.has(n.asn);
}

function routeCardHTML(rt) {
  const path = rt.path || [];
  const chips = path.map((a, i) =>
    `<span class="aschip ${i === 0 ? "peer" : i === path.length - 1 ? "origin" : ""}">AS${a}</span>`).join("→");
  return `
    <div class="pfx">${esc(rt.prefix)}${rt.stale ? '<span class="badge stale">stale</span>' : ""}</div>
    <div class="via">via ${esc(rt.peer)} · since ${new Date(rt.since * 1000).toTimeString().slice(0, 8)}</div>
    <div class="aspath">${chips}</div>
    <div class="kv">
      <span>nexthop</span><b>${esc(rt.nexthop || "—")}</b>
      <span>origin</span><b>${esc(rt.origin || "?")}</b>
      ${rt.local_pref ? `<span>local-pref</span><b>${rt.local_pref}</b>` : ""}
      ${rt.med ? `<span>med</span><b>${rt.med}</b>` : ""}
      ${(rt.communities || []).length ? `<span>communities</span><b>${esc(rt.communities.slice(0, 6).join(" "))}${rt.communities.length > 6 ? "…" : ""}</b>` : ""}
      ${(rt.large_communities || []).length ? `<span>large-comm</span><b>${esc(rt.large_communities.slice(0, 4).join(" "))}</b>` : ""}
      ${rt.aggregator ? `<span>aggregator</span><b>${esc(rt.aggregator)}</b>` : ""}
      ${rt.path_id ? `<span>path-id</span><b>${rt.path_id}</b>` : ""}
    </div>`;
}

/* ---------- rendering ---------- */

let last = performance.now();

function frame(now) {
  const dt = Math.min((now - last) / 1000, 0.05);
  last = now;
  cx2d.clearRect(0, 0, W, H);

  // faint guide rings
  cx2d.strokeStyle = "rgba(56,68,84,0.16)";
  cx2d.lineWidth = 1;
  for (let r = 1; r <= 8; r++) {
    cx2d.beginPath();
    cx2d.arc(CX(), CY(), ringRadius(r), 0, Math.PI * 2);
    cx2d.stroke();
  }

  // edges
  for (const e of edges.values()) {
    if (!e.a || !e.b) continue;
    e.heat *= 0.985;
    const hi = highlight && highlight.edges.has(edgeKeyOf(e));
    cx2d.strokeStyle = hi ? "rgba(57,197,207,0.55)" : `rgba(90,110,135,${0.05 + e.heat * 0.30})`;
    cx2d.lineWidth = hi ? 1.6 : 1;
    cx2d.beginPath(); cx2d.moveTo(e.a.x, e.a.y); cx2d.lineTo(e.b.x, e.b.y); cx2d.stroke();
  }

  // particles
  for (let i = particles.length - 1; i >= 0; i--) {
    const p = particles[i];
    const a = p.ns[p.seg], b = p.ns[p.seg + 1];
    if (!a || !b) { arrive(p); particles.splice(i, 1); continue; }
    const dx = b.x - a.x, dy = b.y - a.y;
    const len = Math.hypot(dx, dy) || 1;
    p.t += (p.speed * 90 * dt) / len;
    if (p.t >= 1) { p.seg++; p.t = 0; continue; }
    const x = a.x + dx * p.t, y = a.y + dy * p.t;
    p.trail.push({ x, y });
    if (p.trail.length > 7) p.trail.shift();
    cx2d.strokeStyle = p.color; cx2d.globalAlpha = 0.5;
    cx2d.beginPath();
    p.trail.forEach((pt, j) => j ? cx2d.lineTo(pt.x, pt.y) : cx2d.moveTo(pt.x, pt.y));
    cx2d.lineTo(x, y); cx2d.stroke();
    cx2d.globalAlpha = 1;
    cx2d.fillStyle = p.color;
    cx2d.shadowColor = p.color; cx2d.shadowBlur = 8;
    cx2d.beginPath(); cx2d.arc(x, y, 2.6, 0, Math.PI * 2); cx2d.fill();
    cx2d.shadowBlur = 0;
  }

  // prefix dots orbiting origins
  for (const [k, d] of dots) {
    const n = d.node;
    d.ang += d.spin * dt * 0.4;
    const x = n.x + Math.cos(d.ang) * (d.rad + n.r);
    const y = n.y + Math.sin(d.ang) * (d.rad + n.r);
    if (d.dying) {
      d.dt += dt;
      const f = Math.max(0, 1 - d.dt / 0.6);
      cx2d.fillStyle = `rgba(248,81,73,${f})`;
      cx2d.beginPath(); cx2d.arc(x, y, d.size + (1 - f) * 5, 0, Math.PI * 2); cx2d.fill();
      if (f <= 0) dots.delete(k);
      continue;
    }
    const age = (performance.now() - d.born) / 1000;
    const s = Math.min(1, age / 0.8);
    const tw = 0.55 + 0.45 * Math.sin(now / 700 + d.ang * 5);
    cx2d.fillStyle = `hsla(${d.hue},80%,62%,${0.35 + 0.45 * tw * s})`;
    cx2d.beginPath(); cx2d.arc(x, y, d.size, 0, Math.PI * 2); cx2d.fill();
  }

  // node flashes (withdraw bursts)
  for (let i = flashes.length - 1; i >= 0; i--) {
    const f = flashes[i];
    f.t += dt;
    const p = f.t / 0.8;
    if (p >= 1) { flashes.splice(i, 1); continue; }
    cx2d.strokeStyle = f.color; cx2d.globalAlpha = (1 - p) * 0.7;
    cx2d.lineWidth = 2;
    cx2d.beginPath(); cx2d.arc(f.n.x, f.n.y, f.n.r + 6 + p * 34, 0, Math.PI * 2); cx2d.stroke();
    cx2d.globalAlpha = 1;
  }

  // nodes
  for (const n of nodes.values()) {
    // ease toward ring target
    n.x += (n.tx - n.x) * Math.min(1, dt * 3);
    n.y += (n.ty - n.y) * Math.min(1, dt * 3);
    n.glow *= 0.94;
    n.r = 3.5 + Math.min(Math.log2(n.count + 1) * 1.4, 16);
    const dim = highlight && !n.hi;
    const alpha = dim ? 0.14 : 0.95;
    const col = `hsl(${n.hue},62%,58%)`;
    if (n.glow > 0.05 || n.hi) {
      cx2d.shadowColor = n.hi ? "#39c5cf" : col;
      cx2d.shadowBlur = n.hi ? 22 : n.glow * 18;
    }
    cx2d.globalAlpha = alpha;
    cx2d.fillStyle = n.hi ? "#39c5cf" : col;
    cx2d.beginPath(); cx2d.arc(n.x, n.y, n.r, 0, Math.PI * 2); cx2d.fill();
    cx2d.shadowBlur = 0;
    if (n.hi || n.r > 6 || n === hoverNode) {
      cx2d.fillStyle = dim ? "rgba(110,118,129,.4)" : "#c9d1d9";
      cx2d.font = "10px ui-monospace, monospace";
      cx2d.textAlign = "center";
      cx2d.fillText(n.label, n.x, n.y - n.r - 4);
      if (n.count > 0) {
        cx2d.fillStyle = "rgba(110,118,129,.8)";
        cx2d.fillText(fmt(n.count), n.x, n.y + n.r + 11);
      }
    }
    cx2d.globalAlpha = 1;
  }

  // center: local AS
  const lc = { x: CX(), y: CY() };
  const pulse = (now / 1400) % 1;
  cx2d.strokeStyle = `rgba(57,197,207,${0.5 * (1 - pulse)})`;
  cx2d.lineWidth = 1.5;
  cx2d.beginPath(); cx2d.arc(lc.x, lc.y, 10 + pulse * 26, 0, Math.PI * 2); cx2d.stroke();
  cx2d.fillStyle = "#0d1117";
  cx2d.strokeStyle = "#39c5cf"; cx2d.lineWidth = 2;
  cx2d.beginPath(); cx2d.arc(lc.x, lc.y, 12, 0, Math.PI * 2); cx2d.fill(); cx2d.stroke();
  cx2d.fillStyle = "#39c5cf";
  cx2d.beginPath(); cx2d.arc(lc.x, lc.y, 5, 0, Math.PI * 2); cx2d.fill();
  cx2d.fillStyle = "#c9d1d9"; cx2d.font = "bold 11px ui-monospace, monospace"; cx2d.textAlign = "center";
  cx2d.fillText(localAsn ? "AS" + localAsn : "you", lc.x, lc.y - 20);

  requestAnimationFrame(frame);
}

function edgeKeyOf(e) { return e.a.asn < e.b.asn ? e.a.asn + ">" + e.b.asn : e.b.asn + ">" + e.a.asn; }

function arrive(p) {
  const dest = p.ns[p.ns.length - 1];
  if (dest && p.color !== "#f85149") flash(dest, p.color);
}

/* ---------- canvas picking ---------- */

cv.addEventListener("mousemove", (ev) => {
  const mx = ev.clientX, my = ev.clientY;
  hoverNode = null;
  for (const n of nodes.values()) {
    if (Math.hypot(n.x - mx, n.y - my) < n.r + 6) { hoverNode = n; break; }
  }
  cv.style.cursor = hoverNode ? "pointer" : "default";
});

cv.addEventListener("click", (ev) => {
  if (hoverNode) {
    searchEl.value = "as" + hoverNode.asn;
    doSearch();
  }
});

/* ---------- boot ---------- */

resize();
connect();
requestAnimationFrame(frame);
