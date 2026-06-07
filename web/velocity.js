/* Velocity macro page — vanilla ES2020 client (dashboard plan P4).
   Renders the CEO-facing flow story: macro highlight tiles, team-wide monthly
   flow, the Claude-attribution cut, and the relocated surge/initiatives panel.
   Everything here is display-only: nothing on this page feeds the composite
   score or Elo.

   Data sources (architecture Step 2): the Claude-attribution cut for the
   dedicated panel comes from /api/team/flow?from=&to=, so it follows the flow
   range picker (re-fetched on range change) instead of being frozen to the
   current window. /metrics.json is still read once for the full-history
   monthly series (charts window it client-side), qa_flow tiles, projects, and
   the current-window cut used by the summary + headline tiles. */

(() => {
  'use strict';

  const { escapeHTML, formatNumber, fmtDays, shiftMonth, monthDelta, clampMonth,
          setActive } = window.VUtil;
  const { clearSVG, drawLineSeries, legend } = window.VChart;

  // Flow metrics double as the clickable stat cards and the chart series.
  // `value(row)` reads a team_flow.monthly row.
  const FLOW_METRICS = [
    { key: 'issues_resolved',   label: 'Issues resolved', value: r => r.issues_resolved || 0 },
    { key: 'issues_created',    label: 'Issues created',  value: r => r.issues_created || 0 },
    { key: 'prs_merged',        label: 'PRs merged',      value: r => r.prs_merged || 0 },
    { key: 'prs_created',       label: 'PRs opened',      value: r => r.prs_created || 0 },
    { key: 'median_cycle_days', label: 'Median cycle (d)', value: r => Math.round((r.median_cycle_hours || 0) / 24 * 10) / 10 },
    { key: 'story_points',      label: 'Story points',    value: r => r.story_points || 0 },
  ];

  const state = {
    data:        null,
    claudeCut:   null,    // windowed Claude-attribution cut from /api/team/flow (follows flowRange)
    flowRange:   '12m',   // '6m' | '12m' | '24m' | 'all'
    flowCompare: 'none',  // 'none' | 'prev' | 'yoy'
    flowMetric:  'issues_resolved',
    risingOnly: false,
  };

  // ---- Boot ----
  async function boot() {
    try {
      const res = await fetch('/metrics.json', { cache: 'no-store' });
      if (!res.ok) throw new Error(res.statusText);
      state.data = await res.json();
    } catch (err) {
      console.error(err);
      document.getElementById('loading').hidden = true;
      document.getElementById('error').hidden = false;
      return;
    }
    document.getElementById('loading').hidden = true;
    wireControls();
    // Fetch the windowed Claude cut for the initial flow range before the first
    // render. Tolerant: on failure renderClaude falls back to the blob's
    // frozen current-window cut, so the page still renders.
    await loadClaudeCut();
    renderAll();
  }

  // loadClaudeCut fetches the Claude-attribution cut for the current flow
  // window from /api/team/flow and stores it on state.claudeCut. The monthly
  // series the charts use stays on metrics.json (full history, window-
  // independent) — only the cut is window-relative, which is the part the
  // precomputed blob froze to the current window.
  async function loadClaudeCut() {
    const win = flowWindow();
    try {
      const res = await fetch(`/api/team/flow?from=${win.start}&to=${win.end}`, { cache: 'no-store' });
      if (!res.ok) throw new Error(res.statusText);
      const flow = await res.json();
      state.claudeCut = flow.claude || null;
    } catch (err) {
      console.error(err);
      // Leave state.claudeCut as-is; claudeCut() falls back to the blob cut.
    }
  }

  // The Claude cut to display: the windowed cut from /api/team/flow when
  // available, otherwise the blob's frozen current-window cut.
  function claudeCut() {
    return state.claudeCut || (state.data.team_flow && state.data.team_flow.claude) || {};
  }

  function wireControls() {
    document.getElementById('flow-range').addEventListener('click', async (e) => {
      const btn = e.target.closest('button'); if (!btn) return;
      state.flowRange = btn.dataset.range;
      setActive('flow-range', btn);
      renderFlow();
      // The Claude cut follows the flow range — re-fetch it server-side, then
      // re-render that panel. Initiatives stay anchored to "now", so they
      // don't re-render on a range change.
      await loadClaudeCut();
      renderClaude();
    });
    document.getElementById('flow-compare').addEventListener('change', (e) => {
      state.flowCompare = e.target.value;
      renderFlow();
    });
    document.getElementById('surge-rising-only').addEventListener('click', (e) => {
      state.risingOnly = !state.risingOnly;
      e.currentTarget.classList.toggle('active', state.risingOnly);
      e.currentTarget.setAttribute('aria-pressed', String(state.risingOnly));
      renderSurge();
    });
  }

  function renderAll() {
    renderSummary();
    renderHighlights();
    renderFlow();
    renderClaude();
    renderSurge();
    renderFooter();
  }

  function monthlyRows() { return (state.data.team_flow && state.data.team_flow.monthly) || []; }

  // ---- Summary ----
  function renderSummary() {
    const s = state.data;
    const cut = (s.team_flow && s.team_flow.claude) || {};
    document.getElementById('vel-summary').textContent =
      `Team-wide flow · current window ${cut.window_start || '?'} → ${cut.window_end || '?'} · cycle time, throughput, and Claude attribution`;
  }

  // ---- Macro highlight tiles ----
  function renderHighlights() {
    const el = document.getElementById('vel-highlights');
    el.innerHTML = '';
    const cut = (state.data.team_flow && state.data.team_flow.claude) || {};
    const qf = state.data.qa_flow || {};
    const sharePct = cut.issues_resolved ? Math.round((cut.claude_issues_resolved / cut.issues_resolved) * 100) : 0;
    const tiles = [
      { label: 'Resolved (window)', value: formatNumber(cut.issues_resolved || 0) },
      { label: 'Median cycle', value: fmtDays(qf.median_cycle_hours) },
      { label: 'Claude share', value: `${sharePct}%` },
      { label: 'Ready-QA wait', value: fmtDays(qf.median_ready_qa_hours) },
      { label: 'Bugs caught by QA', value: formatNumber(qf.bugs_caught || 0) },
    ];
    for (const t of tiles) el.appendChild(tile(t.label, t.value));
  }

  function tile(label, value) {
    const d = document.createElement('div');
    d.className = 'stat';
    d.innerHTML = `<div class="label">${escapeHTML(label)}</div><div class="value">${escapeHTML(String(value))}</div>`;
    return d;
  }

  // ---- Team flow ----
  function renderFlow() {
    const win = flowWindow();
    document.getElementById('flow-meta').textContent = `${win.label} · ${win.start} → ${win.end}`;
    renderFlowStats(win);
    renderFlowChart(win);
  }

  function flowWindow() {
    const history = monthlyRows();
    const current = state.data.current_month;
    const firstMonth = history[0] ? history[0].month : current;
    const back = (n) => clampMonth(shiftMonth(current, -n), firstMonth);
    switch (state.flowRange) {
      case '6m':  return { start: back(5),  end: current, label: 'Last 6 months' };
      case '24m': return { start: back(23), end: current, label: 'Last 24 months' };
      case 'all': return { start: firstMonth, end: current, label: 'Full history' };
      case '12m':
      default:    return { start: back(11), end: current, label: 'Last 12 months' };
    }
  }

  function compareWindow(win) {
    if (state.flowCompare === 'none') return null;
    const len = monthDelta(win.start, win.end) + 1;
    const offset = state.flowCompare === 'yoy' ? -12 : -len;
    const start = shiftMonth(win.start, offset);
    const end = shiftMonth(win.end, offset);
    const label = state.flowCompare === 'yoy'
      ? `Last year (${start} → ${end})`
      : `Previous (${start} → ${end})`;
    return { start, end, label };
  }

  function sliceMonths(start, end) {
    return monthlyRows().filter(r => r.month >= start && r.month <= end);
  }

  // Window aggregate: sum for counts, median-of-monthly-medians for cycle time
  // (cycle is already a per-month median; summing it would be meaningless).
  function metricTotal(metric, win) {
    const rows = sliceMonths(win.start, win.end);
    if (metric.key === 'median_cycle_days') {
      const vals = rows.map(metric.value).filter(v => v > 0).sort((a, b) => a - b);
      return vals.length ? Math.round(vals[Math.floor(vals.length / 2)] * 10) / 10 : 0;
    }
    return rows.reduce((acc, r) => acc + metric.value(r), 0);
  }

  function metricSeries(metric, win) {
    return sliceMonths(win.start, win.end).map(r => ({ label: r.month, value: metric.value(r) }));
  }

  function renderFlowStats(win) {
    const cmp = compareWindow(win);
    const el = document.getElementById('flow-stats');
    el.innerHTML = '';
    for (const m of FLOW_METRICS) {
      el.appendChild(flowStatCard(m, metricTotal(m, win), cmp ? metricTotal(m, cmp) : null));
    }
  }

  function flowStatCard(metric, value, prior) {
    const card = document.createElement('button');
    card.className = 'stat metric-button' + (metric.key === state.flowMetric ? ' active' : '');
    card.dataset.metric = metric.key;
    let deltaHTML = '';
    if (prior !== null && prior !== undefined) {
      const diff = value - prior;
      const dir = diff > 0 ? 'up' : diff < 0 ? 'down' : 'flat';
      const caret = diff > 0 ? '▲' : diff < 0 ? '▼' : '·';
      const text = prior !== 0 ? `${Math.abs(Math.round((diff / Math.abs(prior)) * 100))}%` : formatNumber(Math.abs(diff));
      deltaHTML = `<div class="delta ${dir}">${caret} ${text} vs prior</div>`;
    }
    card.innerHTML = `<div class="label">${metric.label}</div><div class="value">${formatNumber(value)}</div>${deltaHTML}`;
    card.addEventListener('click', () => {
      state.flowMetric = metric.key;
      renderFlow();
    });
    return card;
  }

  function renderFlowChart(win) {
    const svg = document.getElementById('flow-chart');
    clearSVG(svg);
    const metric = FLOW_METRICS.find(m => m.key === state.flowMetric) || FLOW_METRICS[0];
    const series = annotatePartial(metricSeries(metric, win), metric.key);
    const cmp = compareWindow(win);
    let overlay = null;
    if (cmp) {
      const points = annotatePartial(metricSeries(metric, cmp), metric.key);
      if (points.length) overlay = { label: cmp.label, points };
    }
    drawLineSeries(svg, series, { compact: true, overlay });
    legend('flow-legend', metric.label, overlay ? overlay.label : null, series[series.length - 1]);
  }

  // ---- Claude attribution ----
  function renderClaude() {
    const cut = claudeCut();
    const sharePct = cut.issues_resolved ? Math.round((cut.claude_issues_resolved / cut.issues_resolved) * 100) : 0;
    document.getElementById('claude-meta').textContent =
      `window ${cut.window_start || '?'} → ${cut.window_end || '?'}`;

    const el = document.getElementById('claude-stats');
    el.innerHTML = '';
    el.appendChild(tile('Claude-assisted tickets', `${formatNumber(cut.claude_issues_resolved || 0)} / ${formatNumber(cut.issues_resolved || 0)}`));
    el.appendChild(tile('Claude share', `${sharePct}%`));
    el.appendChild(tile('Cycle — Claude', fmtDays(cut.median_cycle_hours_claude)));
    el.appendChild(tile('Cycle — other', fmtDays(cut.median_cycle_hours_other)));

    // Trend: Claude-resolved vs all-resolved per month, full visible flow range.
    const win = flowWindow();
    const rows = sliceMonths(win.start, win.end);
    const total = annotatePartial(rows.map(r => ({ label: r.month, value: r.issues_resolved || 0 })), 'issues_resolved');
    const claude = annotatePartial(rows.map(r => ({ label: r.month, value: r.claude_issues_resolved || 0 })), 'claude_issues_resolved');
    const svg = document.getElementById('claude-chart');
    clearSVG(svg);
    drawLineSeries(svg, total, { compact: true, overlay: { label: 'Claude-assisted', points: claude } });
    legend('claude-legend', 'All resolved', 'Claude-assisted', total[total.length - 1]);
  }

  // ---- Initiatives / momentum (relocated from the home page) ----
  // Epics are pre-ranked by the backend (momentum = recent-window weekly
  // activity rate ÷ trailing baseline, anchored at the latest cached week), so
  // this is "what's heating up / cooling off right now" — independent of the
  // flow range picker. The optional toggle narrows to new/hot/rising.
  const SURGE_LIMIT = 15;
  const RISING_DIRECTIONS = new Set(['new', 'hot', 'rising']);

  function renderSurge() {
    const el = document.getElementById('surge-projects');
    el.innerHTML = '';
    const all = state.data.projects || [];
    let list = all;
    if (state.risingOnly) list = all.filter(p => RISING_DIRECTIONS.has(p.direction));
    const shown = list.slice(0, SURGE_LIMIT);
    document.getElementById('surge-meta').textContent =
      `${shown.length} shown · ${all.length} active${all.length > SURGE_LIMIT && !state.risingOnly ? ` (top ${SURGE_LIMIT})` : ''} · momentum vs trailing 8wk`;
    for (const p of shown) el.appendChild(projectRow(p));
  }

  // Direction → caret + CSS modifier. Carets only (no emoji, per project rule).
  const DIRECTION_GLYPH = {
    new: '▲', hot: '▲', rising: '▲', steady: '·', cooling: '▼',
  };

  function projectRow(p) {
    const row = document.createElement('div');
    row.className = 'project';
    const summary = p.summary
      ? `<span>${escapeHTML(p.summary)}</span>`
      : `<span class="placeholder">Epic not in cache</span>`;
    const dir = p.direction || 'steady';
    const glyph = DIRECTION_GLYPH[dir] || '·';
    // "new" epics have no baseline, so a ratio is meaningless — show the label.
    const momentum = dir === 'new' ? 'new' : `${(p.momentum || 0).toFixed(1)}x`;
    row.innerHTML = `
      <span class="key">${escapeHTML(p.epic_key)}</span>
      <span class="summary">${summary}</span>
      <span class="momentum">${momentum}</span>
      <span class="direction dir-${dir}">${glyph} ${dir}</span>
      <span class="stat-pill">${p.recent_prs || 0} PRs</span>
      <span class="stat-pill">${p.recent_commits || 0} commits</span>
    `;
    row.title = `momentum ${(p.momentum || 0).toFixed(2)}× · recent 2wk vs trailing 8wk · ` +
      `lifetime ${p.totals.prs} PRs / ${p.totals.commits} commits · peak ${p.peak_week}`;
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
        `${monthlyRows().length} months of history (${s.backfill_start} → ${s.current_month})`;
    }
  }

  // Metrics whose monthly value isn't an accumulating count, so the shared
  // projection treatment omits (rather than extrapolates) their partial month.
  const NON_CUMULATIVE = new Set(['median_cycle_days']);

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

  boot();
})();
