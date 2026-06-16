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
          setActive, jiraBrowseURL, chartLoading } = window.VUtil;
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
    qaCut:       null,    // windowed QA/cycle rollup from /api/team/flow (follows flowRange)
    flowRange:   '12m',   // '6m' | '12m' | '24m' | 'all'
    flowCompare: 'none',  // 'none' | 'prev' | 'yoy'
    flowMetric:  'issues_resolved',
    claudeView:  'resolved', // 'resolved' (count) | 'cycle' (median cycle time, Claude vs other)
    risingOnly: false,
  };

  // ---- Boot ----
  const CHARTS = ['flow-chart', 'claude-chart'];

  async function boot() {
    CHARTS.forEach(id => chartLoading(id, true));
    try {
      const res = await fetch('/metrics.json', { cache: 'no-store' });
      if (!res.ok) throw new Error(res.statusText);
      state.data = await res.json();
    } catch (err) {
      console.error(err);
      document.getElementById('loading').hidden = true;
      document.getElementById('error').hidden = false;
      CHARTS.forEach(id => chartLoading(id, false));
      return;
    }
    document.getElementById('loading').hidden = true;
    wireControls();
    // Fetch the windowed Claude cut for the initial flow range before the first
    // render. Tolerant: on failure renderClaude falls back to the blob's
    // frozen current-window cut, so the page still renders.
    await loadClaudeCut();
    renderAll();
    CHARTS.forEach(id => chartLoading(id, false));
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
      state.qaCut = flow.qa || null;
    } catch (err) {
      console.error(err);
      // Leave state.claudeCut/qaCut as-is; the getters fall back to the blob.
    }
  }

  // The Claude cut to display: the windowed cut from /api/team/flow when
  // available, otherwise the blob's frozen current-window cut.
  function claudeCut() {
    return state.claudeCut || (state.data.team_flow && state.data.team_flow.claude) || {};
  }

  // The QA/cycle rollup to display: the windowed cut from /api/team/flow when
  // available (follows the flow range), otherwise the blob's frozen
  // current-window qa_flow.
  function qaCut() {
    return state.qaCut || state.data.qa_flow || {};
  }

  function wireControls() {
    document.getElementById('flow-range').addEventListener('click', async (e) => {
      const btn = e.target.closest('button'); if (!btn) return;
      state.flowRange = btn.dataset.range;
      setActive('flow-range', btn);
      renderFlow();
      // The Claude cut and the QA/cycle rollup both follow the flow range —
      // re-fetch them server-side, then re-render everything that reads the
      // windowed cut (summary line, highlight tiles, Claude panel). Initiatives
      // stay anchored to "now", so they don't re-render on a range change.
      chartLoading('claude-chart', true);
      await loadClaudeCut();
      chartLoading('claude-chart', false);
      renderSummary();
      renderHighlights();
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
  // Follows the flow range: the window + tiles below reflect the selected range
  // (via the windowed /api/team/flow cut), not the frozen current window.
  function renderSummary() {
    const cut = claudeCut();
    document.getElementById('vel-summary').textContent =
      `Team-wide flow · ${cut.window_start || '?'} → ${cut.window_end || '?'} · cycle time, throughput, and Claude attribution`;
  }

  // ---- Macro highlight tiles ----
  function renderHighlights() {
    const el = document.getElementById('vel-highlights');
    el.innerHTML = '';
    const cut = claudeCut();
    const qf = qaCut();
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
    drawLineSeries(svg, series, { compact: true, overlay, tooltip: 'flow-tooltip' });
    legend('flow-legend', metric.label, overlay ? overlay.label : null, series[series.length - 1]);
  }

  // ---- Claude attribution ----
  // The four stat cards double as a chart selector with two views: the count
  // cards chart Claude-assisted vs all resolved over time; the cycle cards chart
  // median cycle time (days) for Claude-assisted vs other tickets over time.
  const CLAUDE_TILES = [
    { key: 'assisted', view: 'resolved', label: 'Claude-assisted tickets',
      value: c => `${formatNumber(c.claude_issues_resolved || 0)} / ${formatNumber(c.issues_resolved || 0)}` },
    { key: 'share', view: 'resolved', label: 'Claude share',
      value: c => `${c.issues_resolved ? Math.round((c.claude_issues_resolved / c.issues_resolved) * 100) : 0}%` },
    { key: 'cycle_claude', view: 'cycle', label: 'Cycle — Claude',
      value: c => fmtDays(c.median_cycle_hours_claude) },
    { key: 'cycle_other', view: 'cycle', label: 'Cycle — other',
      value: c => fmtDays(c.median_cycle_hours_other) },
  ];

  function renderClaude() {
    const cut = claudeCut();
    document.getElementById('claude-meta').textContent =
      `${cut.window_start || '?'} → ${cut.window_end || '?'} · click a card to switch the chart`;

    const el = document.getElementById('claude-stats');
    el.innerHTML = '';
    for (const t of CLAUDE_TILES) el.appendChild(claudeTile(t, cut));

    renderClaudeChart();
  }

  function claudeTile(t, cut) {
    const card = document.createElement('button');
    card.className = 'stat metric-button' + (t.view === state.claudeView ? ' active' : '');
    card.dataset.view = t.view;
    card.innerHTML = `<div class="label">${escapeHTML(t.label)}</div><div class="value">${escapeHTML(String(t.value(cut)))}</div>`;
    card.addEventListener('click', () => {
      if (state.claudeView === t.view) return;
      state.claudeView = t.view;
      renderClaude();
    });
    return card;
  }

  // hours → median days, rounded to 0.1d; 0 stays 0 (drawn at baseline).
  function cycleDays(hours) { return Math.round((hours || 0) / 24 * 10) / 10; }

  function renderClaudeChart() {
    const win = flowWindow();
    const rows = sliceMonths(win.start, win.end);
    const svg = document.getElementById('claude-chart');
    clearSVG(svg);

    let main, overlay, mainLabel, overlayLabel;
    if (state.claudeView === 'cycle') {
      // Median cycle time (days) — Claude-assisted vs other. Medians don't
      // accumulate, so use a non-cumulative key (no partial-month projection).
      main = annotatePartial(rows.map(r => ({ label: r.month, value: cycleDays(r.median_cycle_hours_other) })), 'median_cycle_days');
      const claude = annotatePartial(rows.map(r => ({ label: r.month, value: cycleDays(r.median_cycle_hours_claude) })), 'median_cycle_days');
      overlay = { label: 'Claude (median d)', points: claude };
      mainLabel = 'Other (median d)';
      overlayLabel = 'Claude (median d)';
    } else {
      main = annotatePartial(rows.map(r => ({ label: r.month, value: r.issues_resolved || 0 })), 'issues_resolved');
      const claude = annotatePartial(rows.map(r => ({ label: r.month, value: r.claude_issues_resolved || 0 })), 'claude_issues_resolved');
      overlay = { label: 'Claude-assisted', points: claude };
      mainLabel = 'All resolved';
      overlayLabel = 'Claude-assisted';
    }
    drawLineSeries(svg, main, { compact: true, overlay, tooltip: 'claude-tooltip' });
    legend('claude-legend', mainLabel, overlayLabel, main[main.length - 1]);
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

  // Epic key as a Jira deep link when a base URL is configured, else plain
  // text. Epic keys aren't anonymized, so this is safe in incognito too.
  function epicKeyHTML(key) {
    const url = jiraBrowseURL(key);
    const safe = escapeHTML(key);
    return url
      ? `<a class="epic-link" href="${escapeHTML(url)}" target="_blank" rel="noopener">${safe}</a>`
      : safe;
  }

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
      <span class="key">${epicKeyHTML(p.epic_key)}</span>
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
