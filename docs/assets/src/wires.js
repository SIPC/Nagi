// 配图里的连线：按元素的实际位置画直角连线，改布局时线会跟着走。
// 用法：draw(({ wire, tag, at }) => { wire("#a", "b", "#b", "t", { arrow: true, label: "…" }); })
// 边：t 上、b 下、l 左、r 右。画完后设置 window.__ready，供 render.mjs 截图。
(() => {
  const NS = "http://www.w3.org/2000/svg";
  let frame, origin, svg;

  const el = (q) => (typeof q === "string" ? document.querySelector(q) : q);

  function at(q, side, f = 0.5) {
    const r = el(q).getBoundingClientRect();
    const x = r.left - origin.left;
    const y = r.top - origin.top;
    switch (side) {
      case "t":
        return [x + r.width * f, y];
      case "b":
        return [x + r.width * f, y + r.height];
      case "l":
        return [x, y + r.height * f];
      default:
        return [x + r.width, y + r.height * f];
    }
  }

  function tag(x, y, text, cls = "") {
    const d = document.createElement("div");
    d.className = `tag ${cls}`;
    d.textContent = text;
    d.style.left = `${x}px`;
    d.style.top = `${y}px`;
    frame.appendChild(d);
    return d;
  }

  // o: arrow 画箭头；cls 线型（wave、ok、dead、dash）；fa/fb 起止点在边上的位置（0～1）；
  // align "a" 让终点对齐起点（画直线），"b" 反过来；mid 折线中段的位置；drop 两端都在底边时向下绕多少；
  // label/labelCls/labelDx/labelDy 线上的标注。
  function wire(a, sa, b, sb, o = {}) {
    const p1 = at(a, sa, o.fa ?? 0.5);
    const p2 = at(b, sb, o.fb ?? 0.5);
    const va = sa === "t" || sa === "b";
    const vb = sb === "t" || sb === "b";
    if (o.align === "a") vb ? (p2[0] = p1[0]) : (p2[1] = p1[1]);
    if (o.align === "b") va ? (p1[0] = p2[0]) : (p1[1] = p2[1]);

    const pts = [p1];
    if (va && vb) {
      if (o.drop != null || Math.abs(p1[0] - p2[0]) > 0.5) {
        const m = o.mid ?? (o.drop != null ? Math.max(p1[1], p2[1]) + o.drop : (p1[1] + p2[1]) / 2);
        pts.push([p1[0], m], [p2[0], m]);
      }
    } else if (!va && !vb) {
      if (Math.abs(p1[1] - p2[1]) > 0.5) {
        const m = o.mid ?? (p1[0] + p2[0]) / 2;
        pts.push([m, p1[1]], [m, p2[1]]);
      }
    } else if (va) {
      pts.push([p1[0], p2[1]]);
    } else {
      pts.push([p2[0], p1[1]]);
    }
    pts.push(p2);

    const path = document.createElementNS(NS, "path");
    path.setAttribute("d", "M" + pts.map((p) => `${p[0]} ${p[1]}`).join("L"));
    path.setAttribute("class", `wire ${o.cls ?? ""}`);
    if (o.arrow) path.setAttribute("marker-end", `url(#arr-${o.cls === "wave" || o.cls === "ok" ? o.cls : "line"})`);
    svg.appendChild(path);

    if (o.label) {
      let s = [pts[0], pts[1]];
      if (pts.length === 4) s = [pts[1], pts[2]];
      if (pts.length === 3) {
        const l1 = Math.hypot(pts[1][0] - pts[0][0], pts[1][1] - pts[0][1]);
        const l2 = Math.hypot(pts[2][0] - pts[1][0], pts[2][1] - pts[1][1]);
        s = l1 >= l2 ? [pts[0], pts[1]] : [pts[1], pts[2]];
      }
      const x = (s[0][0] + s[1][0]) / 2 + (o.labelDx ?? 0);
      const y = (s[0][1] + s[1][1]) / 2 + (o.labelDy ?? 0);
      tag(x, y, o.label, o.labelCls ?? "");
    }
    return pts;
  }

  window.draw = (fn) => {
    document.fonts.ready.then(() => {
      frame = document.querySelector(".frame");
      origin = frame.getBoundingClientRect();
      svg = document.createElementNS(NS, "svg");
      svg.setAttribute("class", "wires");
      svg.setAttribute("width", origin.width);
      svg.setAttribute("height", origin.height);
      const marker = (id, color) => `
        <marker id="arr-${id}" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto">
          <path d="M0 1L9 5L0 9z" style="fill: var(--${color})" />
        </marker>`;
      svg.innerHTML = `<defs>${marker("line", "line")}${marker("wave", "wave")}${marker("ok", "success")}</defs>`;
      frame.prepend(svg);
      fn({ wire, tag, at });
      window.__ready = true;
    });
  };
})();
