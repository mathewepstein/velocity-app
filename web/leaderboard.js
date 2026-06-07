/* Velocity leaderboard home — vanilla ES2020 client.
   Renders the three sections defined in Phase 5.3: top leaderboard + cohort
   chart, composite-score legend, and the team productivity dashboard with
   surge panel.

   Data sources (architecture Step 2): the per-dev cohort comes from
   /api/contributors?...&sort=elo (current window, elo-ranked server-side) —
   this is the leaderboard reusing the contributors endpoint, not a separate
   /api/leaderboard. /metrics.json is still read once for the team-level
   aggregates (full_history, projects), the current-window bounds, and footer
   meta. */

(() => {
  'use strict';

  const { escapeHTML, formatNumber, shiftMonth, monthDelta, clampMonth,
          isoWeekToApproxMonth, setActive, devLogin } = window.VUtil;
  const { clearSVG, drawEmptyText, niceTicks, drawLineSeries, legend } = window.VChart;

  const TOP_N = 10;

  // Surge-detection moves to the macro Velocity page (dashboard plan P4).
  // Until that page exists the panel stays in the markup but is hidden here,
  // so the work isn't lost and it re-enables with a one-line flip.
  const SHOW_SURGE = false;

  // Team metrics double as the clickable stat cards and the chart series.
  // `value(row)` reads a monthly history row; `active` marks the one metric
  // (distinct active contributors) that isn't a plain monthly sum.
  const TEAM_METRICS = [
    { key: 'prs_merged',           label: 'PRs merged',          value: r => r.prs_merged || 0 },
    { key: 'jira_issues_resolved', label: 'Issues resolved',     value: r => r.jira_issues_resolved || 0 },
    { key: 'prs_reviewed',         label: 'Reviews',             value: r => r.prs_reviewed || 0 },
    { key: 'commits',              label: 'Commits',             value: r => r.commits || 0 },
    { key: 'loc_changed',          label: 'LoC changed',         value: r => (r.loc_added || 0) + (r.loc_deleted || 0) },
    { key: 'active_contributors',  label: 'Active contributors', active: true },
  ];

  const state = {
    data:        null,
    cohort:      [],         // current-window per-dev metrics, elo-ranked, from /api/contributors
    teamRange:   '3m',       // '3m' | '6m' | '12m' | 'ytd' | 'all'
    teamCompare: 'none',     // 'none' | 'prev' | 'yoy'
    teamMetric:  'prs_merged',
    hideSmallProjects: true,
  };

  // ---- Boot ----
  async function boot() {
    try {
      const res = await fetch('/metrics.json', { cache: 'no-store' });
      if (!res.ok) throw new Error(res.statusText);
      state.data = await res.json();

      // The standings cohort: the current window (the same one metrics.json was
      // built for), ranked by Elo server-side. Same []DevWindowMetrics shape as
      // metrics.json.devs, so the rest of the page is unchanged.
      const win = state.data.current.window;
      const cres = await fetch(
        `/api/contributors?from=${win.start}&to=${win.end}&sort=elo`, { cache: 'no-store' });
      if (!cres.ok) throw new Error(cres.statusText);
      state.cohort = await cres.json();
    } catch (err) {
      console.error(err);
      document.getElementById('loading').hidden = true;
      document.getElementById('error').hidden = false;
      return;
    }
    document.getElementById('loading').hidden = true;

    wireControls();
    renderAll();
  }

  function wireControls() {
    document.getElementById('team-range').addEventListener('click', (e) => {
      const btn = e.target.closest('button'); if (!btn) return;
      state.teamRange = btn.dataset.range;
      setActive('team-range', btn);
      renderTeamSection();
    });
    document.getElementById('team-compare').addEventListener('change', (e) => {
      state.teamCompare = e.target.value;
      renderTeamSection();
    });
    if (SHOW_SURGE) {
      document.getElementById('team-hide-small').addEventListener('click', (e) => {
        state.hideSmallProjects = !state.hideSmallProjects;
        e.currentTarget.classList.toggle('active', state.hideSmallProjects);
        e.currentTarget.setAttribute('aria-pressed', String(state.hideSmallProjects));
        renderTeamSurge();
      });
    }
  }

  function renderAll() {
    renderHeader();
    renderLeaderboard();
    renderAsideChart();
    renderTeamSection();
    renderFooter();
  }

  // ---- Header ----
  function renderHeader() {
    const s = state.data;
    const devs = scoredDevs();
    document.getElementById('lb-summary').textContent =
      `${devs.length} devs · current window ${s.current.window.start} → ${s.current.window.end} · ` +
      `cohort scored against the same period for both rating and composite`;

    const lastPlayed = devs
      .map(d => (d.rating.history && d.rating.history.length) ? d.rating.periods_played : 0)
      .reduce((a, b) => Math.max(a, b), 0);
    document.getElementById('lb-meta').textContent =
      lastPlayed > 0 ? `up to ${lastPlayed} bi-weekly periods played` : '';
  }

  // The cohort arrives elo-ranked from /api/contributors?sort=elo, so this only
  // filters — devs without a rating and the synthetic "unknown" bucket drop off
  // the standings — and preserves the server order.
  function scoredDevs() {
    return (state.cohort || [])
      .filter(d => d.rating && d.dev && d.dev.display_name !== 'unknown');
  }

  // ---- Leaderboard table ----
  function renderLeaderboard() {
    const body = document.getElementById('lb-body');
    body.innerHTML = '';
    const devs = scoredDevs().slice(0, TOP_N);
    devs.forEach((d, idx) => body.appendChild(leaderboardRow(d, idx + 1)));
  }

  function leaderboardRow(d, rank) {
    const row = document.createElement('a');
    row.className = 'lb-row lb-row-compact';
    row.href = '/dev/' + (devLogin(d) || '');
    row.setAttribute('role', 'row');

    if (rank <= 3) row.classList.add('medal', 'medal-' + rank);
    if (d.rating.provisional) row.classList.add('provisional');

    const rankCell = cell(String(rank), 'rank');
    if (rank <= 3) rankCell.classList.add('bold');

    const nameCell = cell(d.dev.display_name, 'name');
    if (d.rating.provisional) {
      const badge = document.createElement('span');
      badge.className = 'provisional-badge';
      badge.textContent = 'Provisional';
      badge.title = `Fewer than the configured threshold of established periods played (${d.rating.periods_played}). Rating is still settling.`;
      nameCell.appendChild(badge);
    }

    const ratingCell = cell(Math.round(d.rating.current).toString(), 'num');
    const scoreCell = scoreDeltaCell(d.score, d.rating.delta_period);

    [rankCell, nameCell, ratingCell, scoreCell].forEach(c => row.appendChild(c));
    return row;
  }

  // Composite score with the period rating-delta fused alongside it, e.g.
  // "+6.6 ▲1.2". The big number is the composite; the small carated number is
  // how the Elo rating moved this period.
  function scoreDeltaCell(score, delta) {
    const c = document.createElement('div');
    c.className = 'num score-cell';
    const main = document.createElement('span');
    main.className = 'score-main';
    main.textContent = formatScore(score);
    c.appendChild(main);
    if (delta !== null && delta !== undefined) {
      const dir = delta > 0 ? 'up' : (delta < 0 ? 'down' : 'flat');
      const caret = delta > 0 ? '▲' : (delta < 0 ? '▼' : '·');
      const d = document.createElement('span');
      d.className = 'score-delta ' + dir;
      d.textContent = `${caret}${Math.abs(delta).toFixed(1)}`;
      c.appendChild(d);
    }
    return c;
  }

  // ---- Aside chart (top-N rating over time) ----
  function renderAsideChart() {
    const svg = document.getElementById('aside-chart');
    const legend = document.getElementById('aside-legend');
    clearSVG(svg);
    legend.innerHTML = '';
    drawTopNTrend(svg, legend);
  }

  function drawTopNTrend(svg, legend) {
    const NS = 'http://www.w3.org/2000/svg';
    const vb = { w: 1200, h: 320 };
    const pad = { t: 18, r: 18, b: 28, l: 50 };
    svg.setAttribute('viewBox', `0 0 ${vb.w} ${vb.h}`);

    const devs = scoredDevs().slice(0, TOP_N).filter(d => (d.rating.history || []).length > 1);
    if (devs.length === 0) {
      drawEmptyText(svg, vb, 'Not enough rating history yet to chart.');
      return;
    }

    // Right-align histories on the same axis: pad shorter histories with
    // nulls so devs who joined later don't start at the same x position as
    // long-tenured devs.
    const maxLen = Math.max(...devs.map(d => d.rating.history.length));
    const padded = devs.map(d => {
      const h = d.rating.history;
      const offset = maxLen - h.length;
      const arr = new Array(maxLen).fill(null);
      for (let i = 0; i < h.length; i++) arr[offset + i] = h[i];
      return { dev: d, history: arr };
    });

    let minY = Infinity, maxY = -Infinity;
    for (const p of padded) for (const v of p.history) {
      if (v == null) continue;
      if (v < minY) minY = v;
      if (v > maxY) maxY = v;
    }
    if (!isFinite(minY)) { drawEmptyText(svg, vb, 'No ratings to plot.'); return; }
    const span = (maxY - minY) || 1;
    const yMin = minY - span * 0.05;
    const yMax = maxY + span * 0.05;
    const yRange = yMax - yMin;
    const xStep = maxLen > 1 ? (vb.w - pad.l - pad.r) / (maxLen - 1) : 0;
    const x = i => pad.l + i * xStep;
    const y = v => pad.t + (vb.h - pad.t - pad.b) * (1 - (v - yMin) / yRange);

    // Gridlines + y-axis labels.
    const ticks = niceTicks(yMin, yMax, 4);
    for (const tv of ticks) {
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
      lbl.textContent = String(Math.round(tv));
      svg.appendChild(lbl);
    }

    // 1000 baseline.
    if (yMin <= 1000 && yMax >= 1000) {
      const ref = document.createElementNS(NS, 'line');
      ref.setAttribute('x1', pad.l); ref.setAttribute('x2', vb.w - pad.r);
      ref.setAttribute('y1', y(1000)); ref.setAttribute('y2', y(1000));
      ref.setAttribute('class', 'baseline');
      svg.appendChild(ref);
    }

    // One line per dev. Medal colors for top 3, accent fade for the rest.
    padded.forEach((p, idx) => {
      const rank = idx + 1;
      const colorClass = rank === 1 ? 'gold' : rank === 2 ? 'silver' : rank === 3 ? 'bronze' : 'rest';
      let pathData = '';
      let started = false;
      p.history.forEach((v, i) => {
        if (v == null) { started = false; return; }
        pathData += `${started ? 'L' : 'M'}${x(i)},${y(v)} `;
        started = true;
      });
      if (pathData) {
        const path = document.createElementNS(NS, 'path');
        path.setAttribute('d', pathData.trim());
        path.setAttribute('class', 'aside-line ' + colorClass);
        svg.appendChild(path);
      }
      // Endpoint dot.
      for (let i = p.history.length - 1; i >= 0; i--) {
        if (p.history[i] != null) {
          const dot = document.createElementNS(NS, 'circle');
          dot.setAttribute('cx', x(i)); dot.setAttribute('cy', y(p.history[i]));
          dot.setAttribute('r', rank <= 3 ? 3.5 : 2.5);
          dot.setAttribute('class', 'aside-dot ' + colorClass);
          svg.appendChild(dot);
          break;
        }
      }

      // Legend entry.
      const item = document.createElement('span');
      item.className = 'aside-legend-item ' + colorClass;
      item.innerHTML = `<span class="swatch"></span>${escapeHTML(p.dev.dev.display_name)}`;
      legend.appendChild(item);
    });
  }

  // ---- Team productivity section ----
  function renderTeamSection() {
    const win = teamWindow();
    document.getElementById('team-meta').textContent = `${win.label} · ${win.start} → ${win.end}`;
    renderTeamStats(win);
    renderTeamChart(win);
    const surgeBlock = document.getElementById('team-surge-block');
    if (SHOW_SURGE) {
      renderTeamSurge();
    } else if (surgeBlock) {
      surgeBlock.hidden = true;
    }
  }

  // Resolve the selected range to a [start, end] month span (both YYYY-MM),
  // anchored at the latest cached month. Computed client-side off the team's
  // full monthly history — no server round-trip.
  function teamWindow() {
    const s = state.data;
    const history = s.full_history.monthly || [];
    const current = s.current_month;
    const firstMonth = history[0]?.month || current;
    const back = (n) => clampMonth(shiftMonth(current, -n), firstMonth);
    switch (state.teamRange) {
      case '3m':  return { start: back(2),  end: current, label: 'Last 3 months' };
      case '6m':  return { start: back(5),  end: current, label: 'Last 6 months' };
      case '12m': return { start: back(11), end: current, label: 'Last 12 months' };
      case 'ytd': return { start: `${current.slice(0, 4)}-01`, end: current, label: `YTD ${current.slice(0, 4)}` };
      case 'all':
      default:    return { start: firstMonth, end: current, label: 'Full history' };
    }
  }

  // The comparison span for the overlay: the same-length window shifted back
  // by its own length ('prev') or by 12 months ('yoy'). Null when comparison
  // is off.
  function compareWindow(win) {
    if (state.teamCompare === 'none') return null;
    const len = monthDelta(win.start, win.end) + 1;
    const offset = state.teamCompare === 'yoy' ? -12 : -len;
    const start = shiftMonth(win.start, offset);
    const end = shiftMonth(win.end, offset);
    const label = state.teamCompare === 'yoy'
      ? `Last year (${start} → ${end})`
      : `Previous (${start} → ${end})`;
    return { start, end, label };
  }

  function sliceMonths(history, start, end) {
    return history.filter(r => r.month >= start && r.month <= end);
  }

  // Distinct devs with at least one signal anywhere in [start, end]. Walks
  // each dev's full monthly history (not the window-scoped `monthly`, which
  // only covers the current window) so the count is correct for any range.
  function activeContributorCount(win) {
    let n = 0;
    for (const d of state.cohort || []) {
      const monthly = d.full_history_monthly || d.monthly || [];
      if (monthly.some(r => r.month >= win.start && r.month <= win.end && rowHasSignal(r))) n++;
    }
    return n;
  }

  // Per-month distinct active-dev counts across [start, end], for the
  // active-contributors trend line.
  function activeContributorsByMonth(win) {
    const counts = new Map();
    for (const d of state.cohort || []) {
      for (const r of (d.full_history_monthly || d.monthly || [])) {
        if (r.month < win.start || r.month > win.end) continue;
        if (rowHasSignal(r)) counts.set(r.month, (counts.get(r.month) || 0) + 1);
      }
    }
    return counts;
  }

  function rowHasSignal(r) {
    return (r.prs_merged || 0) + (r.commits || 0) + (r.jira_issues_resolved || 0) +
           (r.prs_reviewed || 0) + (r.jira_issues_touched || 0) > 0;
  }

  // Window total for a metric — a sum across the sliced months, except
  // active_contributors which is a distinct count.
  function metricTotal(metric, win) {
    if (metric.active) return activeContributorCount(win);
    const rows = sliceMonths(state.data.full_history.monthly || [], win.start, win.end);
    return rows.reduce((acc, r) => acc + metric.value(r), 0);
  }

  // Per-month [{label, value}] series for a metric across the window.
  function metricSeries(metric, win) {
    const rows = sliceMonths(state.data.full_history.monthly || [], win.start, win.end);
    if (metric.active) {
      const byMonth = activeContributorsByMonth(win);
      return rows.map(r => ({ label: r.month, value: byMonth.get(r.month) || 0 }));
    }
    return rows.map(r => ({ label: r.month, value: metric.value(r) }));
  }

  function renderTeamStats(win) {
    const cmp = compareWindow(win);
    const el = document.getElementById('team-stats');
    el.innerHTML = '';
    for (const m of TEAM_METRICS) {
      el.appendChild(statCard(m, metricTotal(m, win), cmp ? metricTotal(m, cmp) : null));
    }
  }

  // A clickable stat card that doubles as the chart's metric selector. When a
  // comparison window is active it shows the period-over-period delta.
  function statCard(metric, value, prior) {
    const card = document.createElement('button');
    card.className = 'stat metric-button' + (metric.key === state.teamMetric ? ' active' : '');
    card.dataset.metric = metric.key;
    let deltaHTML = '';
    if (prior !== null && prior !== undefined) {
      const diff = value - prior;
      const dir = diff > 0 ? 'up' : diff < 0 ? 'down' : 'flat';
      const caret = diff > 0 ? '▲' : diff < 0 ? '▼' : '·';
      const text = prior !== 0 ? `${Math.abs(Math.round((diff / Math.abs(prior)) * 100))}%` : formatNumber(Math.abs(diff));
      deltaHTML = `<div class="delta ${dir}">${caret} ${text} vs prior</div>`;
    }
    card.innerHTML = `
      <div class="label">${metric.label}</div>
      <div class="value">${formatNumber(value)}</div>
      ${deltaHTML}
    `;
    card.addEventListener('click', () => {
      state.teamMetric = metric.key;
      renderTeamSection();
    });
    return card;
  }

  function renderTeamChart(win) {
    const svg = document.getElementById('team-chart');
    clearSVG(svg);
    const metric = TEAM_METRICS.find(m => m.key === state.teamMetric) || TEAM_METRICS[0];
    const series = annotatePartial(metricSeries(metric, win), metric.key);
    const cmp = compareWindow(win);
    let overlay = null;
    if (cmp) {
      const points = annotatePartial(metricSeries(metric, cmp), metric.key);
      if (points.length) overlay = { label: cmp.label, points };
    }
    drawLineSeries(svg, series, { compact: true, overlay });
    legend('team-legend', metric.label, overlay ? overlay.label : null, series[series.length - 1]);
  }

  // Metrics whose monthly value isn't an accumulating count, so the shared
  // projection treatment omits (rather than extrapolates) their partial month.
  const NON_CUMULATIVE = new Set(['active_contributors']);

  // Page wrapper over the shared partial-month annotator: supplies this page's
  // NON_CUMULATIVE set + the current-month fraction from the loaded payload.
  function annotatePartial(series, metricKey) {
    return window.VChart.annotatePartial(series, metricKey, {
      currentMonth: state.data && state.data.current_month,
      fraction: window.VChart.currentMonthFraction(
        state.data && state.data.generated_at, state.data && state.data.current_month),
      nonCumulative: NON_CUMULATIVE,
    });
  }

  // ---- Team surge panel ----
  function renderTeamSurge() {
    const el = document.getElementById('team-projects');
    el.innerHTML = '';
    const win = teamWindow();
    const all = state.data.projects || [];
    let filtered = all.filter(p => projectIntersectsWindow(p, win));
    if (state.hideSmallProjects) {
      filtered = filtered.filter(p => (p.totals.prs + p.totals.commits) >= 3);
    }
    // Sort by combined intensity within the window.
    filtered.sort((a, b) => {
      const ai = (a.totals.prs || 0) + (a.totals.commits || 0);
      const bi = (b.totals.prs || 0) + (b.totals.commits || 0);
      return bi - ai;
    });
    document.getElementById('team-surge-meta').textContent =
      `${filtered.length} shown · ${all.length} total · scoped to ${win.label.toLowerCase()}`;
    for (const p of filtered) el.appendChild(projectRow(p));
  }

  // A project "intersects" the window if any of its weekly buckets falls
  // inside [win.start, win.end] (both YYYY-MM). We coarsely map ISO weeks
  // to months via the YYYY-Wnn prefix.
  function projectIntersectsWindow(p, win) {
    const weeks = p.weekly || [];
    if (weeks.length === 0) {
      const peakMonth = isoWeekToApproxMonth(p.peak_week);
      return peakMonth && peakMonth >= win.start && peakMonth <= win.end;
    }
    for (const w of weeks) {
      const m = isoWeekToApproxMonth(w.week);
      if (m && m >= win.start && m <= win.end) return true;
    }
    return false;
  }

  function projectRow(p) {
    const row = document.createElement('div');
    row.className = 'project';
    const loc = (p.totals.loc_added || 0) + (p.totals.loc_deleted || 0);
    const summary = p.summary
      ? `<span>${escapeHTML(p.summary)}</span>`
      : `<span class="placeholder">Epic not in cache</span>`;
    const triggers = (p.triggers || [])
      .map(t => `<span class="trigger">${t.replace(/_/g, ' ')}</span>`).join('');
    row.innerHTML = `
      <span class="key">${escapeHTML(p.epic_key)}</span>
      <span class="summary">${summary}</span>
      <span class="stat-pill">${p.totals.prs} PRs</span>
      <span class="stat-pill">${p.totals.commits} commits</span>
      <span class="stat-pill">${formatNumber(loc)} LoC</span>
      <span class="stat-pill">${p.active_weeks}w active</span>
      <span class="triggers">${triggers}</span>
    `;
    row.title = `Peak ${p.peak_week} · ${p.first_seen_week} → ${p.last_seen_week}`;
    return row;
  }

  // ---- Footer ----
  function renderFooter() {
    const s = state.data;
    if (s.generated_at) {
      document.getElementById('footer-generated').textContent =
        `Generated ${new Date(s.generated_at).toLocaleString()}`;
    }
    if (s.backfill_start && s.current_month) {
      document.getElementById('footer-history').textContent =
        `${s.backfill_start} → ${s.current_month}`;
    }
  }

  // ---- Helpers ----
  function cell(text, cls) {
    const c = document.createElement('div');
    c.setAttribute('role', 'cell');
    if (cls) c.className = cls;
    c.textContent = text;
    return c;
  }

  function formatScore(score) {
    if (!score) return '—';
    const v = score.total;
    const sign = v > 0 ? '+' : '';
    return `${sign}${v.toFixed(1)}`;
  }

  boot();
})();
