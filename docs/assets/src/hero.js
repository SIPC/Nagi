// README 首图：居中放标志（和面板里的 Waves 图标相同，不带底板），背景是一张节点网络，点是节点，线是节点之间的连接。
// hero.html（浅色）和 hero.dark.html（深色）共用这份脚本，按 <html class="dark"> 切换配色。
// 节点位置用固定种子生成，每次渲染结果相同。
const W = 1280;
const H = 640;
const dark = document.documentElement.classList.contains("dark");

const css = `
  :root {
    --bg: #fafafa;
    --dot: oklch(0.55 0.006 260 / 0.55);
    --link: oklch(0.55 0.006 260 / 0.22);
    --fg: #0a0a0a;
    --wave: #3f9cc6;
    --accent-link: rgb(63 156 198 / 0.55);
    --halo: rgb(63 156 198 / 0.14);
  }
  .dark {
    --bg: oklch(0.155 0.002 260);
    --dot: oklch(0.7 0.006 260 / 0.5);
    --link: oklch(0.7 0.006 260 / 0.16);
    --fg: #ededed;
    --wave: #6db9dc;
    --accent-link: rgb(109 185 220 / 0.5);
    --halo: rgb(109 185 220 / 0.16);
  }
  html, body { margin: 0; background: var(--bg); }
  .frame { position: relative; width: ${W}px; height: ${H}px; overflow: hidden; background: var(--bg); }
  .net { position: absolute; inset: 0; }
  .logo {
    position: absolute;
    left: ${W / 2 - 56}px;
    top: ${H / 2 - 33}px;
    width: 112px;
    height: 66px;
  }
`;
document.head.insertAdjacentHTML("beforeend", `<style>${css}</style>`);
document.body.innerHTML = `
  <div class="frame">
    <svg class="net" viewBox="0 0 ${W} ${H}"></svg>
    <svg class="logo" viewBox="4 9 24 14" fill="none"><path d="M6 19c3-3 6-3 10 0s7 3 10 0" stroke="var(--wave)" stroke-width="2.4" stroke-linecap="round" /><path d="M6 13c3-3 6-3 10 0s7 3 10 0" stroke="var(--fg)" stroke-width="2.4" stroke-linecap="round" opacity=".55" /></svg>
  </div>`;

// mulberry32：固定种子的伪随机数
let seed = 20261107;
const rand = () => {
  seed = (seed + 0x6d2b79f5) | 0;
  let t = seed;
  t = Math.imul(t ^ (t >>> 15), t | 1);
  t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
  return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
};

// 越靠近中心越淡，标志周围留白；离中心较远处完全显示。
const cx = W / 2;
const cy = H / 2;
const fade = (x, y) => {
  const r = Math.hypot((x - cx) / 1.6, y - cy);
  const t = Math.min(1, Math.max(0, (r - 120) / 170));
  return t * t * (3 - 2 * t);
};

// 抖动网格上取点，网格外多留一圈，让连线延伸出画面。
const step = 72;
const nodes = [];
for (let gy = -1; gy <= H / step + 1; gy++) {
  for (let gx = -1; gx <= W / step + 1; gx++) {
    const x = gx * step + (gy % 2 ? step / 2 : 0) + (rand() - 0.5) * step * 0.55;
    const y = gy * step * 0.9 + (rand() - 0.5) * step * 0.55;
    const inside = x > 16 && x < W - 16 && y > 16 && y < H - 16;
    nodes.push({ x, y, a: fade(x, y), inside });
  }
}

// 由短到长加边，和已有的边交叉就跳过，得到一张不交叉的三角网；再随机去掉一部分，免得太密。
const cross = (p, q, r, s) => {
  const o = (a, b, c) => (b.x - a.x) * (c.y - a.y) - (b.y - a.y) * (c.x - a.x);
  return o(p, q, r) * o(p, q, s) < 0 && o(r, s, p) * o(r, s, q) < 0;
};
const cand = [];
for (let i = 0; i < nodes.length; i++)
  for (let j = i + 1; j < nodes.length; j++) {
    const d = Math.hypot(nodes[i].x - nodes[j].x, nodes[i].y - nodes[j].y);
    if (d < step * 1.6) cand.push([d, i, j]);
  }
cand.sort((a, b) => a[0] - b[0]);
const kept = [];
for (const [, i, j] of cand) {
  if (kept.some(([u, v]) => u !== i && u !== j && v !== i && v !== j && cross(nodes[i], nodes[j], nodes[u], nodes[v]))) continue;
  kept.push([i, j]);
}
const edges = new Map();
for (const [i, j] of kept) if (rand() > 0.28) edges.set(`${i}-${j}`, [i, j]);

// 挑几个离中心有一定距离的节点作为高亮节点，沿网络中最短的路径把它们连起来。
const adj = nodes.map(() => []);
for (const [i, j] of edges.values()) {
  const d = Math.hypot(nodes[i].x - nodes[j].x, nodes[i].y - nodes[j].y);
  adj[i].push([j, d]);
  adj[j].push([i, d]);
}
const pick = (x, y) => {
  let best = -1;
  nodes.forEach((n, i) => {
    if (n.inside && adj[i].length && (best < 0 || Math.hypot(n.x - x, n.y - y) < Math.hypot(nodes[best].x - x, nodes[best].y - y))) best = i;
  });
  return best;
};
const hubs = [
  [250, 150],
  [190, 470],
  [470, 560],
  [860, 90],
  [1090, 210],
  [1010, 520],
].map(([x, y]) => pick(x, y));
const path = (from, to) => {
  const dist = nodes.map(() => Infinity);
  const prev = nodes.map(() => -1);
  const done = nodes.map(() => false);
  dist[from] = 0;
  for (;;) {
    let u = -1;
    for (let i = 0; i < nodes.length; i++) if (!done[i] && (u < 0 || dist[i] < dist[u])) u = i;
    if (u < 0 || dist[u] === Infinity || u === to) break;
    done[u] = true;
    for (const [v, d] of adj[u]) {
      // 绕开中心，也不走画面外的节点。
      const w = d * (1 + 3 * (1 - nodes[v].a)) * (nodes[v].inside ? 1 : 50);
      if (dist[u] + w < dist[v]) {
        dist[v] = dist[u] + w;
        prev[v] = u;
      }
    }
  }
  const out = [];
  for (let v = to; v >= 0 && v !== from; v = prev[v]) out.push([prev[v], v]);
  return out;
};
const lit = new Set();
const ring = [0, 3, 4, 5, 2, 1, 0];
for (let k = 0; k < ring.length - 1; k++) {
  for (const [i, j] of path(hubs[ring[k]], hubs[ring[k + 1]])) lit.add(i < j ? `${i}-${j}` : `${j}-${i}`);
}

const svg = document.querySelector(".net");
const ns = "http://www.w3.org/2000/svg";
const add = (tag, attrs) => {
  const el = document.createElementNS(ns, tag);
  for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, v);
  svg.appendChild(el);
  return el;
};

for (const [key, [i, j]] of edges) {
  const a = nodes[i];
  const b = nodes[j];
  const alpha = Math.min(a.a, b.a);
  if (alpha <= 0.01) continue;
  const on = lit.has(key);
  add("line", {
    x1: a.x.toFixed(1),
    y1: a.y.toFixed(1),
    x2: b.x.toFixed(1),
    y2: b.y.toFixed(1),
    stroke: on ? "var(--accent-link)" : "var(--link)",
    "stroke-width": on ? 1.5 : 1,
    opacity: alpha.toFixed(3),
  });
}
const hubSet = new Set(hubs);
nodes.forEach((n, i) => {
  if (n.a <= 0.01) return;
  if (hubSet.has(i)) {
    add("circle", { cx: n.x.toFixed(1), cy: n.y.toFixed(1), r: 11, fill: "var(--halo)", opacity: n.a.toFixed(3) });
    add("circle", { cx: n.x.toFixed(1), cy: n.y.toFixed(1), r: 4, fill: "var(--wave)", opacity: n.a.toFixed(3) });
  } else {
    add("circle", { cx: n.x.toFixed(1), cy: n.y.toFixed(1), r: 2, fill: "var(--dot)", opacity: n.a.toFixed(3) });
  }
});

window.__ready = true;
