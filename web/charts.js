/* Velocity shared charting — SVG line-chart rendering + the partial-current-
   month projection treatment, shared across every chart page. Loaded
   (deferred) after util.js and before the per-page script; exposes
   window.VChart. Depends on window.VUtil. No build step — plain global. */
(() => {
  'use strict';

  const { escapeHTML, formatNumber } = window.VUtil;
  const NS = 'http://www.w3.org/2000/svg';

  // Don't extrapolate a partial month from just the first few days.
  const MIN_PROJ_FRACTION = 0.15;

  function clearSVG(svg) { while (svg.firstChild) svg.removeChild(svg.firstChild); }

  function drawEmptyText(svg, vb, text) {
    const t = document.createElementNS(NS, 'text');
    t.setAttribute('x', vb.w / 2); t.setAttribute('y', vb.h / 2);
    t.setAttribute('text-anchor', 'middle');
    t.setAttribute('class', 'chart-empty');
    t.textContent = text;
    svg.appendChild(t);
  }

  // "Nice" axis ticks between lo and hi (~count of them). Epsilon-guarded so
  // floating-point drift doesn't drop the top tick; falls back to [lo].
  function niceTicks(lo, hi, count) {
    if (hi <= lo) return [lo];
    const range = hi - lo;
    const raw = range / Math.max(1, count);
    const mag = Math.pow(10, Math.floor(Math.log10(raw)));
    const norm = raw / mag;
    let step;
    if (norm < 1.5) step = 1 * mag;
    else if (norm < 3) step = 2 * mag;
    else if (norm < 7) step = 5 * mag;
    else step = 10 * mag;
    const out = [];
    const startT = Math.ceil(lo / step) * step;
    for (let v = startT; v <= hi + 1e-9; v += step) out.push(v);
    return out.length ? out : [lo];
  }

  // ---- Partial current-month projection ----
  // Fraction of the current month covered by the data, from generated_at.
  function currentMonthFraction(generatedAt, currentMonth) {
    if (!generatedAt || !currentMonth) return 1;
    const gen = new Date(generatedAt).getTime();
    const [y, m] = currentMonth.split('-').map(Number);
    const start = Date.UTC(y, m - 1, 1), end = Date.UTC(y, m, 1);
    if (!(gen > start)) return 0;
    return Math.min(1, (gen - start) / (end - start));
  }

  // Annotate a series' trailing point, when it's the current (partial) month,
  // with partial/projected/pace so a chart can draw it dashed + projected.
  // ctx: { currentMonth, fraction, nonCumulative:Set }. Non-accumulating
  // metrics (or too-early months) drop the partial point entirely so the chart
  // never shows a misleading end-of-line dropoff.
  function annotatePartial(series, metricKey, ctx) {
    if (!series || !series.length) return series;
    const last = series[series.length - 1];
    if (!ctx || last.label !== ctx.currentMonth) return series;
    const frac = ctx.fraction;
    if (frac >= 1) return series; // data already covers the whole month → complete
    const isNonCumulative = ctx.nonCumulative && ctx.nonCumulative.has(metricKey);
    if (!isNonCumulative && frac >= MIN_PROJ_FRACTION) {
      last.partial = true;
      last.projected = last.value / frac;
      last.pace = paceClass(last.projected, series.length >= 2 ? series[series.length - 2].value : null);
      return series;
    }
    return series.slice(0, -1);
  }

  function paceClass(proj, prior) {
    if (prior == null || prior === 0) return null;
    if (proj > prior * 1.05) return 'above';
    if (proj < prior * 0.95) return 'below';
    return 'on';
  }

  function displayValue(p) { return (p && p.partial && p.projected != null) ? p.projected : p.value; }

  // Legend chip describing the current-month projection.
  function paceBadge(lastPoint) {
    if (!lastPoint || !lastPoint.partial) return '';
    if (lastPoint.projected != null && lastPoint.pace) {
      const caret = lastPoint.pace === 'above' ? '▲' : lastPoint.pace === 'below' ? '▼' : '·';
      return `<span class="pace ${lastPoint.pace}">${caret} ${escapeHTML(lastPoint.label)} pacing ~${formatNumber(Math.round(lastPoint.projected))}</span>`;
    }
    return `<span class="pace on">${escapeHTML(lastPoint.label)} partial</span>`;
  }

  // Legend for a single team/flow line + optional overlay + pace badge.
  function legend(elId, currentLabel, overlayLabel, lastPoint) {
    const el = document.getElementById(elId);
    if (!el) return;
    let html = `<span><span class="swatch current"></span>${escapeHTML(currentLabel)}</span>`;
    if (overlayLabel) html += `<span><span class="swatch overlay"></span>${escapeHTML(overlayLabel)}</span>`;
    html += paceBadge(lastPoint);
    el.innerHTML = html;
  }

  // 0-baseline monthly line chart used by the home + velocity team-flow panels.
  // opts: { compact, overlay:{label, points} }. Solid through complete months,
  // dashed into the projected partial month; uses display (projected) values so
  // a same-period overlay projects its partial month too.
  function drawLineSeries(svg, series, opts = {}) {
    const vb = { w: 1200, h: opts.compact ? 280 : 320 };
    const pad = { t: 18, r: 18, b: 30, l: 56 };
    svg.setAttribute('viewBox', `0 0 ${vb.w} ${vb.h}`);
    if (series.length === 0) { drawEmptyText(svg, vb, 'No data in this period.'); return; }

    const overlay = (opts.overlay && opts.overlay.points.length) ? opts.overlay : null;
    let maxY = 1;
    for (const s of series) { const v = displayValue(s); if (v > maxY) maxY = v; }
    if (overlay) for (const s of overlay.points) { const v = displayValue(s); if (v > maxY) maxY = v; }
    maxY = Math.ceil(maxY * 1.1);

    const n = series.length;
    const xStep = n > 1 ? (vb.w - pad.l - pad.r) / (n - 1) : 0;
    const x = i => pad.l + i * xStep;
    const y = v => pad.t + (vb.h - pad.t - pad.b) * (1 - v / maxY);

    for (const tv of niceTicks(0, maxY, 4)) {
      const gy = y(tv);
      const line = document.createElementNS(NS, 'line');
      line.setAttribute('x1', pad.l); line.setAttribute('x2', vb.w - pad.r);
      line.setAttribute('y1', gy); line.setAttribute('y2', gy);
      line.setAttribute('class', 'grid');
      svg.appendChild(line);
      const lbl = document.createElementNS(NS, 'text');
      lbl.setAttribute('x', pad.l - 8); lbl.setAttribute('y', gy + 4);
      lbl.setAttribute('text-anchor', 'end');
      lbl.setAttribute('class', 'ytick');
      lbl.textContent = formatNumber(tv);
      svg.appendChild(lbl);
    }

    const addPath = (pts, idxs, cls) => {
      if (idxs.length < 2) return;
      const dd = idxs.map((idx, k) => `${k === 0 ? 'M' : 'L'}${x(idx)},${y(displayValue(pts[idx]))}`).join(' ');
      const p = document.createElementNS(NS, 'path');
      p.setAttribute('d', dd); p.setAttribute('class', cls);
      svg.appendChild(p);
    };
    const seq = (a, b) => { const o = []; for (let i = a; i < b; i++) o.push(i); return o; };

    // Overlay first so the current series draws on top.
    if (overlay) addPath(overlay.points, seq(0, overlay.points.length), 'team-line overlay');

    // Main series: solid through complete months, dashed into the partial month.
    const lastPartial = series.length >= 2 && series[series.length - 1].partial;
    if (lastPartial) {
      addPath(series, seq(0, series.length - 1), 'team-line');
      addPath(series, [series.length - 2, series.length - 1], 'team-line partial');
    } else {
      addPath(series, seq(0, series.length), 'team-line');
    }

    series.forEach((s, i) => {
      const dot = document.createElementNS(NS, 'circle');
      dot.setAttribute('cx', x(i)); dot.setAttribute('cy', y(displayValue(s)));
      dot.setAttribute('r', 2.5);
      dot.setAttribute('class', 'team-dot' + (s.partial ? ' partial' : ''));
      svg.appendChild(dot);
    });

    const labelEvery = Math.max(1, Math.ceil(n / 8));
    for (let i = 0; i < n; i += labelEvery) {
      const lbl = document.createElementNS(NS, 'text');
      lbl.setAttribute('x', x(i)); lbl.setAttribute('y', vb.h - pad.b + 16);
      lbl.setAttribute('text-anchor', 'middle');
      lbl.setAttribute('class', 'xtick');
      lbl.textContent = series[i].label;
      svg.appendChild(lbl);
    }
  }

  window.VChart = {
    clearSVG, drawEmptyText, niceTicks,
    currentMonthFraction, annotatePartial, paceClass, displayValue, paceBadge,
    legend, drawLineSeries,
  };
})();
