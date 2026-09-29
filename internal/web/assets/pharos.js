// Pharos front end. No dependencies; every feature degrades to plain HTML.
(function () {
  "use strict";

  const $ = (sel, root) => (root || document).querySelector(sel);
  const $$ = (sel, root) => Array.from((root || document).querySelectorAll(sel));
  const tip = $(".tip");
  const csrf = () => ($('meta[name="csrf-token"]') || {}).content || "";

  // ── Tooltip ──────────────────────────────────────────────────────────
  // Content is built with textContent: labels may come from config files.
  function showTip(rows, x, y) {
    if (!tip) return;
    tip.replaceChildren(...rows);
    tip.hidden = false;
    const pad = 12;
    const r = tip.getBoundingClientRect();
    let left = x + pad;
    let top = y + pad;
    if (left + r.width > window.innerWidth - 8) left = x - r.width - pad;
    if (top + r.height > window.innerHeight - 8) top = y - r.height - pad;
    tip.style.left = Math.max(8, left) + "px";
    tip.style.top = Math.max(8, top) + "px";
  }
  function hideTip() { if (tip) tip.hidden = true; }
  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }

  // ── Daily bars ───────────────────────────────────────────────────────
  function bindBars(root) {
    $$("svg.bars", root).forEach((svg) => {
      if (svg.dataset.bound) return;
      svg.dataset.bound = "1";
      svg.addEventListener("pointermove", (ev) => {
        const r = ev.target.closest("rect[data-date]");
        if (!r) return hideTip();
        const rows = [el("div", "t-date", r.dataset.date), el("div", null, r.dataset.level)];
        if (r.dataset.uptime && r.dataset.uptime !== "–") {
          const row = el("div", "t-row"); row.append(el("b", null, r.dataset.uptime)); rows.push(row);
        }
        if (r.dataset.down) rows.push(el("div", null, "▼ " + r.dataset.down));
        if (+r.dataset.inc > 0) rows.push(el("div", null, "⚑ " + r.dataset.inc));
        showTip(rows, ev.clientX, ev.clientY);
      });
      svg.addEventListener("pointerleave", hideTip);
    });
  }

  // ── Partial refresh ──────────────────────────────────────────────────
  // Fetch the current page and swap in elements marked data-live (by id),
  // or the whole <main> on the public page. Charts and open <details> keep
  // their state because they are not replaced.
  let refreshing = false;
  async function refresh(whole) {
    if (refreshing) return;
    refreshing = true;
    try {
      const res = await fetch(location.href, { headers: { "X-Requested-With": "pharos" }, credentials: "same-origin" });
      if (!res.ok) return;
      const doc = new DOMParser().parseFromString(await res.text(), "text/html");
      if (whole) {
        const next = $("#main", doc);
        const cur = $("#main");
        if (next && cur) { cur.replaceWith(next); init(next); }
      } else {
        $$("[data-live][id]", doc).forEach((next) => {
          const cur = document.getElementById(next.id);
          if (cur) { cur.replaceWith(next); init(next); }
        });
      }
    } catch (_) { /* offline: keep the current view */ }
    finally { refreshing = false; }
  }

  function autoRefresh() {
    const main = $("#main[data-refresh]");
    if (!main) return;
    const every = Math.max(15, +main.dataset.refresh || 60) * 1000;
    setInterval(() => { if (!document.hidden) refresh(true); }, every);
    document.addEventListener("visibilitychange", () => { if (!document.hidden) refresh(true); });
  }

  function liveEvents() {
    if (!$("[data-live-page]") || !window.EventSource) return;
    const dot = $("[data-live-dot]");
    let timer = null;
    const es = new EventSource("/admin/events");
    const schedule = () => { clearTimeout(timer); timer = setTimeout(() => refresh(false), 800); };
    es.addEventListener("open", () => dot && dot.classList.add("on"));
    es.addEventListener("error", () => dot && dot.classList.remove("on"));
    es.addEventListener("check", schedule);
    es.addEventListener("status", schedule);
  }

  // ── Forms ────────────────────────────────────────────────────────────
  function bindForms(root) {
    $$("form[data-async]", root).forEach((f) => {
      if (f.dataset.bound) return;
      f.dataset.bound = "1";
      f.addEventListener("submit", async (ev) => {
        ev.preventDefault();
        const btn = $("button", f);
        if (btn) btn.setAttribute("aria-busy", "true");
        try {
          const res = await fetch(f.action, { method: "POST", headers: { Accept: "application/json", "X-CSRF-Token": csrf() }, credentials: "same-origin" });
          if (!res.ok) throw new Error(res.status);
          // A check result arrives through the event stream; refresh anyway
          // in case the stream is not connected.
          setTimeout(() => refresh(false), 1200);
          refresh(false);
        } catch (_) {
          f.submit();
        }
      });
    });
  }

  function bindFilter() {
    const input = $("input[data-filter]");
    if (!input) return;
    const apply = () => {
      const q = input.value.trim().toLowerCase();
      let shown = 0;
      $$(input.dataset.filter).forEach((tr) => {
        const hit = !q || (tr.dataset.text || "").includes(q);
        tr.hidden = !hit;
        if (hit) shown++;
      });
      const empty = $("[data-filter-empty]");
      if (empty) empty.hidden = shown > 0;
    };
    input.addEventListener("input", apply);
    // Re-apply after live updates replace the table.
    new MutationObserver(apply).observe($("#main"), { childList: true, subtree: true });
  }

  function bindCopy(root) {
    $$("[data-copy]", root).forEach((b) => {
      if (b.dataset.bound) return;
      b.dataset.bound = "1";
      b.addEventListener("click", async () => {
        const src = $(b.dataset.copy);
        if (!src) return;
        try {
          await navigator.clipboard.writeText(src.textContent.trim());
          const label = b.textContent;
          b.textContent = b.dataset.copied || "Copied";
          setTimeout(() => (b.textContent = label), 1500);
        } catch (_) {
          const range = document.createRange();
          range.selectNodeContents(src);
          const sel = getSelection(); sel.removeAllRanges(); sel.addRange(range);
        }
      });
    });
  }

  // ── Time formatting ──────────────────────────────────────────────────
  function formatter(lang, tz, opts) {
    try { return new Intl.DateTimeFormat(lang, Object.assign({ timeZone: tz || undefined, hourCycle: "h23" }, opts)); }
    catch (_) { return new Intl.DateTimeFormat(lang, Object.assign({ hourCycle: "h23" }, opts)); }
  }
  // Offset of tz from UTC at time t, in ms (DST aware).
  function tzOffset(t, tz) {
    if (!tz) return -new Date(t).getTimezoneOffset() * 60000;
    const f = formatter("en-US", tz, { year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit" });
    const p = {};
    f.formatToParts(new Date(t)).forEach((x) => (p[x.type] = x.value));
    const asUTC = Date.UTC(+p.year, +p.month - 1, +p.day, +p.hour % 24, +p.minute, +p.second);
    return asUTC - Math.floor(t / 1000) * 1000;
  }
  function fmtMs(v) {
    if (v >= 10000) return (v / 1000).toFixed(0) + " s";
    if (v >= 1000) return (v / 1000).toFixed(2) + " s";
    return Math.round(v) + " ms";
  }

  // ── Latency chart ────────────────────────────────────────────────────
  const NS = "http://www.w3.org/2000/svg";
  function svgEl(tag, attrs, parent) {
    const e = document.createElementNS(NS, tag);
    for (const k in attrs) e.setAttribute(k, attrs[k]);
    if (parent) parent.appendChild(e);
    return e;
  }
  function niceStep(raw) {
    const p = Math.pow(10, Math.floor(Math.log10(raw)));
    for (const m of [1, 2, 2.5, 5, 10]) if (m * p >= raw) return m * p;
    return 10 * p;
  }

  function renderChart(host) {
    const src = $(host.dataset.chart);
    if (!src) return;
    let data;
    try { data = JSON.parse(src.textContent); } catch (_) { return; }
    const L = data.labels || {};
    const pts = data.points || [];
    const tz = data.tz || undefined;
    const lang = data.lang || "en";

    // Fill the table's timestamps in the configured zone.
    const fmtFull = formatter(lang, tz, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
    $$("[data-chart-table] [data-ts]").forEach((td) => { td.textContent = fmtFull.format(new Date(+td.dataset.ts)); });

    function draw() {
      host.querySelectorAll("svg").forEach((s) => s.remove());
      const W = host.clientWidth;
      const H = host.clientHeight || 240;
      if (W < 50) return;
      const m = { l: 56, r: 12, t: 10, b: 26 };
      const pw = W - m.l - m.r;
      const ph = H - m.t - m.b;
      const svg = svgEl("svg", { width: W, height: H, viewBox: `0 0 ${W} ${H}`, tabindex: "0", "aria-hidden": "true" }, host);
      const ok = pts.filter((p) => p.ok > 0);
      const x = (t) => m.l + ((t - data.from) / (data.to - data.from)) * pw;

      if (!ok.length) {
        svgEl("line", { class: "baseline", x1: m.l, x2: W - m.r, y1: m.t + ph + 0.5, y2: m.t + ph + 0.5 }, svg);
        const t = svgEl("text", { class: "empty", x: m.l + pw / 2, y: m.t + ph / 2, "text-anchor": "middle" }, svg);
        t.textContent = L.empty || "No data";
        return;
      }
      const maxV = Math.max(...ok.map((p) => Math.max(p.p95, p.avg))) || 1;
      const step = niceStep(maxV / 4);
      const top = Math.ceil(maxV / step) * step;
      const y = (v) => m.t + ph - (v / top) * ph;

      // Grid and y labels (drawn first so data sits on top).
      for (let v = 0; v <= top + 1e-9; v += step) {
        const yy = Math.round(y(v)) + 0.5;
        svgEl("line", { class: v === 0 ? "baseline" : "grid", x1: m.l, x2: W - m.r, y1: yy, y2: yy }, svg);
        const t = svgEl("text", { class: "axis", x: m.l - 8, y: yy + 4, "text-anchor": "end" }, svg);
        t.textContent = fmtMs(v);
      }

      // X ticks aligned to local time boundaries.
      const span = data.to - data.from;
      const H1 = 3600e3, D1 = 86400e3;
      const candidates = [H1, 2 * H1, 3 * H1, 6 * H1, 12 * H1, D1, 2 * D1, 7 * D1, 14 * D1, 30 * D1];
      const want = Math.max(2, Math.floor(pw / 90));
      const tickStep = candidates.find((s) => span / s <= want) || 30 * D1;
      const labelFmt = tickStep < D1
        ? formatter(lang, tz, { hour: "2-digit", minute: "2-digit" })
        : formatter(lang, tz, { month: "short", day: "numeric" });
      const off = tzOffset(data.from, tz);
      let t0 = Math.ceil((data.from + off) / tickStep) * tickStep - off;
      for (let t = t0; t <= data.to; t += tickStep) {
        const xx = Math.round(x(t)) + 0.5;
        if (xx < m.l + 12 || xx > W - m.r - 12) continue;
        const lbl = svgEl("text", { class: "axis", x: xx, y: H - 6, "text-anchor": "middle" }, svg);
        lbl.textContent = labelFmt.format(new Date(t));
      }

      // Outage spans behind the lines.
      (data.outages || []).forEach((o) => {
        const x1 = Math.max(m.l, x(o.s));
        const x2 = Math.min(W - m.r, x(o.e));
        if (x2 - x1 < 0.5) {
          if (x1 >= m.l && x1 <= W - m.r) svgEl("rect", { class: "outage", x: x1 - 1, y: m.t, width: 2, height: ph }, svg);
          return;
        }
        svgEl("rect", { class: "outage", x: x1, y: m.t, width: x2 - x1, height: ph }, svg);
      });

      // Lines break where a bucket had no successful check.
      const half = data.bucket / 2;
      function paths(key) {
        let line = "", area = "", seg = [];
        const flush = () => {
          if (!seg.length) return;
          line += "M" + seg.map((p) => p.join(",")).join("L");
          area += `M${seg[0][0]},${m.t + ph}L` + seg.map((p) => p.join(",")).join("L") + `L${seg[seg.length - 1][0]},${m.t + ph}Z`;
          seg = [];
        };
        let prevT = null;
        pts.forEach((p) => {
          if (!p.ok || (prevT !== null && p.t - prevT > data.bucket * 1.5)) flush();
          if (p.ok) seg.push([x(p.t + half).toFixed(1), y(p[key]).toFixed(1)]);
          prevT = p.t;
        });
        flush();
        return { line, area };
      }
      const avg = paths("avg");
      const p95 = paths("p95");
      svgEl("path", { class: "area", d: avg.area }, svg);
      svgEl("path", { class: "line-p95", d: p95.line }, svg);
      svgEl("path", { class: "line-avg", d: avg.line }, svg);
      // Lone points (a segment of one bucket) need a visible mark.
      ok.forEach((p, i) => {
        const prev = pts[pts.indexOf(p) - 1], next = pts[pts.indexOf(p) + 1];
        if ((!prev || !prev.ok) && (!next || !next.ok)) svgEl("circle", { class: "dot-avg", cx: x(p.t + half), cy: y(p.avg), r: 3 }, svg);
      });

      // Crosshair.
      const cross = svgEl("line", { class: "cross", y1: m.t, y2: m.t + ph, visibility: "hidden" }, svg);
      const dAvg = svgEl("circle", { class: "dot-avg", r: 4, visibility: "hidden" }, svg);
      const dP95 = svgEl("circle", { class: "dot-p95", r: 4, visibility: "hidden" }, svg);
      const rangeFmt = data.bucket >= D1 ? formatter(lang, tz, { month: "short", day: "numeric" })
        : formatter(lang, tz, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
      const endFmt = formatter(lang, tz, { hour: "2-digit", minute: "2-digit" });
      let idx = -1;
      function show(i, cx, cy) {
        if (i < 0 || i >= pts.length) return;
        idx = i;
        const p = pts[i];
        const xx = x(p.t + half);
        cross.setAttribute("x1", xx); cross.setAttribute("x2", xx); cross.setAttribute("visibility", "visible");
        [[dAvg, "avg"], [dP95, "p95"]].forEach(([d, k]) => {
          if (p.ok) { d.setAttribute("cx", xx); d.setAttribute("cy", y(p[k])); d.setAttribute("visibility", "visible"); }
          else d.setAttribute("visibility", "hidden");
        });
        let when = rangeFmt.format(new Date(p.t));
        if (data.bucket < D1) when += "–" + endFmt.format(new Date(Math.min(p.t + data.bucket, data.to)));
        const rows = [el("div", "t-date", when)];
        if (p.ok) {
          [["avg", "key-avg"], ["p95", "key-p95"]].forEach(([k, cls]) => {
            const row = el("div", "t-row");
            row.append(el("i", "key-line " + cls), el("b", null, fmtMs(p[k])), el("span", null, L[k]));
            rows.push(row);
          });
        } else {
          rows.push(el("div", null, L.nodata));
        }
        const failed = p.n - p.ok;
        rows.push(el("div", "t-date", `${p.n} ${L.checks}` + (failed > 0 ? ` · ${failed} ${L.failed}` : "")));
        const box = svg.getBoundingClientRect();
        showTip(rows, cx != null ? cx : box.left + xx, cy != null ? cy : box.top + m.t + 20);
      }
      function nearest(clientX) {
        const box = svg.getBoundingClientRect();
        const t = data.from + ((clientX - box.left - m.l) / pw) * (data.to - data.from) - half;
        let best = -1, bd = Infinity;
        pts.forEach((p, i) => { const d = Math.abs(p.t - t); if (d < bd) { bd = d; best = i; } });
        return best;
      }
      function hide() {
        [cross, dAvg, dP95].forEach((e) => e.setAttribute("visibility", "hidden"));
        hideTip();
      }
      svg.addEventListener("pointermove", (ev) => show(nearest(ev.clientX), ev.clientX, ev.clientY));
      svg.addEventListener("pointerleave", hide);
      svg.addEventListener("blur", hide);
      svg.addEventListener("keydown", (ev) => {
        if (ev.key === "ArrowRight") { show(Math.min(pts.length - 1, idx < 0 ? 0 : idx + 1)); ev.preventDefault(); }
        else if (ev.key === "ArrowLeft") { show(Math.max(0, idx < 0 ? pts.length - 1 : idx - 1)); ev.preventDefault(); }
        else if (ev.key === "Escape") hide();
      });
    }
    draw();
    if (window.ResizeObserver) {
      let w = host.clientWidth;
      new ResizeObserver(() => { if (host.clientWidth !== w) { w = host.clientWidth; draw(); } }).observe(host);
    }
  }

  function init(root) {
    bindBars(root);
    bindForms(root);
    bindCopy(root);
    $$("[data-chart]", root).forEach(renderChart);
  }

  document.addEventListener("DOMContentLoaded", () => {
    init(document);
    autoRefresh();
    liveEvents();
    bindFilter();
    document.addEventListener("scroll", hideTip, { passive: true });
  });
})();
