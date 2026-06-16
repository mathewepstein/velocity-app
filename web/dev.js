/* Velocity dev page — vanilla ES2020 client.
   Renders one developer's identity strip, clickable metric tiles, trend +
   Elo charts, and composite-score breakdown for the dev whose login is in
   the URL path (/dev/<gh-login>).

   Data source (architecture Step 2): the dev's window metrics come from
   /api/dev/{login}?from=&to= (current cohort recomputed for the selected
   window, so totals/code_impact/composite/rank are window-relative and
   re-rank with the picker). /metrics.json is still read once for page context
   (current month, generated_at, footer meta). A page-level range picker drives
   the whole page; the Elo chart clips to the same window via rating.history_
   dates and labels by month. The Elo rating block itself is window-independent
   (cumulative precompute). */

(() => {
  'use strict';

  const { escapeHTML, formatNumber, shiftMonth, monthDelta, clampMonth, setActive } = window.VUtil;
  const { drawEmptyText, niceTicks, displayValue, paceBadge } = window.VChart;

  // ---- Available metrics. `key` matches the JSON field on dev.totals /
  // dev.monthly; `compute(row)` is used for derived metrics like loc_changed.
  // code_impact is the default after Phase 6: substance-of-contribution
  // composite that replaces raw commits in the scoring weights.
  const METRICS = [
    { key: 'code_impact',          label: 'Code impact', compute: r => Math.round(((r.code_impact ?? 0)) * 10) / 10 },
    { key: 'prs_merged',           label: 'PRs merged' },
    { key: 'prs_reviewed',         label: 'PR reviews' },
    { key: 'jira_issues_resolved',   label: 'Issues resolved' },
    { key: 'jira_issues_progressed', label: 'Issues progressed' },
    { key: 'prs_created',            label: 'PRs opened' },
    { key: 'commits',              label: 'Commits' },
    { key: 'loc_changed',          label: 'LoC changed', compute: r => (r.loc_added || 0) + (r.loc_deleted || 0) },
  ];

  const METRIC_BREAKDOWN_LABELS = {
    prs_merged:           'PRs merged',
    jira_issues_resolved: 'Issues resolved',
    code_impact:          'Code impact',
    prs_reviewed:         'PR reviews',
    prs_created:          'PRs opened',
    jira_issues_progressed: 'Issues progressed',
    active_weeks:         'Active weeks',
    story_points:         'Story points',
    jira_issues_created:  'Issues filed',
    loc_changed:          'LoC changed',
    commits:              'Commits',
  };

  const state = {
    data:       null,           // metrics.json — page context (month math, generated_at, footer meta)
    login:      '',             // gh login (or alias slug in incognito) from the URL
    dev:        null,           // the windowed DevWindowMetrics from /api/dev
    window:     null,           // {start, end, label} resolved from range
    metric:     'code_impact',
    range:      '3m',           // '3m' | '6m' | '12m' | 'ytd' | 'all' — drives the WHOLE page
    comparison: 'none',         // 'none' | 'prior' | 'yoy' | 'qoq' (trend-chart overlay only)
    projectsSort: 'share',      // 'share' | 'absolute' | 'surge'
  };

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
    state.login = loginFromPath();
    document.getElementById('loading').hidden = true;
    wireControls();
    await loadDevWindow();
  }

  // /dev/<login>  or  /dev/  → empty string (falls back to the top dev).
  function loginFromPath() {
    const m = /^\/dev\/?([^\/?#]*)/.exec(location.pathname);
    return m ? decodeURIComponent(m[1] || '') : '';
  }

  // Resolve the selected range to a [start, end] month span, anchored at the
  // latest cached month — mirrors contributors.js contribWindow() so the pages
  // agree on what "3M"/"YTD"/etc. mean.
  function devWindow() {
    const s = state.data;
    const history = (s.full_history && s.full_history.monthly) || [];
    const current = s.current_month;
    const firstMonth = (history[0] && history[0].month) || current;
    const back = (n) => clampMonth(shiftMonth(current, -n), firstMonth);
    switch (state.range) {
      case '6m':  return { start: back(5),  end: current, label: 'Last 6 months' };
      case '12m': return { start: back(11), end: current, label: 'Last 12 months' };
      case 'ytd': return { start: `${current.slice(0, 4)}-01`, end: current, label: `YTD ${current.slice(0, 4)}` };
      case 'all': return { start: firstMonth, end: current, label: 'Full history' };
      case '3m':
      default:    return { start: back(2),  end: current, label: 'Last 3 months' };
    }
  }

  // loadDevWindow fetches this dev's metrics recomputed for the selected window
  // and re-renders. totals/code_impact/composite/rank are window-relative, so a
  // range change genuinely re-ranks the dev against the cohort — the work the
  // precomputed blob couldn't do. The trend chart + Elo chart both narrow to
  // the same window. With no login in the URL we fall back to the top dev by
  // Elo (the "Me" link before a username is configured).
  async function loadDevWindow() {
    const win = devWindow();
    state.window = win;
    try {
      let dev;
      if (state.login) {
        const res = await fetch(
          `/api/dev/${encodeURIComponent(state.login)}?from=${win.start}&to=${win.end}`, { cache: 'no-store' });
        if (res.status === 404) { document.getElementById('not-found').hidden = false; return; }
        if (!res.ok) throw new Error(res.statusText);
        dev = await res.json();
      } else {
        const res = await fetch(
          `/api/contributors?from=${win.start}&to=${win.end}&sort=elo`, { cache: 'no-store' });
        if (!res.ok) throw new Error(res.statusText);
        const cohort = await res.json();
        dev = (cohort || []).find(d => d.dev && d.dev.display_name !== 'unknown') || null;
        if (!dev) { document.getElementById('not-found').hidden = false; return; }
      }
      state.dev = dev;
    } catch (err) {
      console.error(err);
      document.getElementById('error').hidden = false;
      return;
    }
    renderAll();
  }

  function wireControls() {
    document.getElementById('range-picker').addEventListener('click', async (e) => {
      const btn = e.target.closest('button'); if (!btn || btn.dataset.range === state.range) return;
      setActive('range-picker', btn);
      state.range = btn.dataset.range;
      // The window drives the whole page — re-fetch the windowed metrics, then
      // renderAll() (via loadDevWindow) re-renders tiles, score, and both charts.
      await loadDevWindow();
    });
    document.getElementById('comparison-picker').addEventListener('change', (e) => {
      state.comparison = e.target.value;
      renderTrendChart();
    });
    const sortGroup = document.getElementById('projects-sort');
    if (sortGroup) {
      sortGroup.addEventListener('click', (e) => {
        const btn = e.target.closest('button'); if (!btn) return;
        state.projectsSort = btn.dataset.sort;
        setActive('projects-sort', btn);
        renderProjects();
      });
    }
  }

  function renderAll() {
    renderHeader();
    renderMetricButtons();
    renderTrendChart();
    renderEloChart();
    renderBreakdown();
    renderProjects();
    renderFooter();
  }

  // ---- Header ----
  function renderHeader() {
    const d = state.dev;
    document.getElementById('dev-name').textContent = d.dev.display_name;
    document.title = `Velocity — ${d.dev.display_name}`;

    const ghEl = document.getElementById('dev-gh-logins');
    ghEl.innerHTML = '';
    for (const login of d.dev.github_logins || []) {
      const chip = document.createElement('span');
      chip.className = 'gh-chip';
      chip.textContent = login;
      ghEl.appendChild(chip);
    }
    if (d.dev.jira_account_id) {
      document.getElementById('dev-jira').textContent = `Jira ${d.dev.jira_account_id}`;
    }

    const block = document.getElementById('dev-rating-block');
    block.innerHTML = '';
    if (d.rating) {
      const r = d.rating;
      block.innerHTML = `
        <div class="rating-value">${Math.round(r.current)}</div>
        <div class="rating-label">Elo rating</div>
        <div class="rating-delta ${deltaClass(r.delta_period)}">${formatDelta(r.delta_period)} this period</div>
        <div class="rating-periods">${r.periods_played} periods played${r.provisional ? ' · <span class="provisional-tag">Provisional</span>' : ''}</div>
      `;
    }

    const win = state.window;
    document.getElementById('window-meta').textContent =
      `${win.label} · ${win.start} → ${win.end}`;
  }

  // ---- Metric buttons (clickable stat cards) ----
  function renderMetricButtons() {
    const el = document.getElementById('metric-buttons');
    el.innerHTML = '';
    const totals = state.dev.totals || {};
    for (const m of METRICS) {
      const value = m.compute ? m.compute(totals) : (totals[m.key] || 0);
      const btn = document.createElement('button');
      btn.className = 'stat metric-button' + (m.key === state.metric ? ' active' : '');
      btn.dataset.metric = m.key;
      btn.innerHTML = `
        <div class="label">${m.label}</div>
        <div class="value">${formatNumber(value)}</div>
      `;
      btn.addEventListener('click', () => {
        state.metric = m.key;
        renderMetricButtons();
        renderTrendChart();
      });
      el.appendChild(btn);
    }
  }

  // ---- Trend chart ----
  // Charts the page window (state.window, driven by the range picker) off this
  // dev's full monthly history; the comparison picker overlays a prior slice
  // aligned by index. Series comes from dev.full_history_monthly when present;
  // legacy payloads fall back to dev.monthly (current window only).
  function renderTrendChart() {
    const svg = document.getElementById('trend-chart');
    const tooltip = document.getElementById('trend-tooltip');
    const full = state.dev.full_history_monthly || state.dev.monthly || [];
    const win = state.window;
    const series = annotatePartial(sliceByMonth(full, win.start, win.end)
      .map(r => ({ label: r.month, value: valueFor(state.metric, r) })), state.metric);
    const overlay = buildHistoricalOverlay(full, win);
    if (overlay) annotatePartial(overlay.points, state.metric); // no-op for prior-period overlays
    drawLineChart(svg, tooltip, series, overlay);
    const pace = document.getElementById('trend-pace');
    if (pace) pace.innerHTML = paceBadge(series[series.length - 1]);

    const label = METRICS.find(m => m.key === state.metric)?.label || state.metric;
    const span = state.comparison === 'none'
      ? `${win.label} (${series.length} months)`
      : `${win.label} vs ${overlay?.label || 'comparison'}`;
    document.getElementById('trend-meta').textContent = `${label} · ${span}`;
    const legend = document.getElementById('overlay-legend');
    legend.hidden = !overlay;
    if (overlay) {
      legend.lastChild && (legend.lastChild.textContent = ' ' + overlay.label);
    }
  }

  function sliceByMonth(rows, start, end) {
    return rows.filter(r => r.month >= start && r.month <= end);
  }

  // Builds the comparison overlay from this dev's own historical months,
  // shifted back from the selected range. Returns null when comparison is off
  // or the shifted slice is entirely outside the cached range (e.g. YoY before
  // backfill_start). Aligned by index: overlay point i maps to series point i.
  function buildHistoricalOverlay(full, win) {
    const winLen = monthDelta(win.start, win.end) + 1;
    const offset = state.comparison === 'prior' ? -winLen
                 : state.comparison === 'yoy'   ? -12
                 : state.comparison === 'qoq'   ? -3
                 : 0;
    if (offset === 0) return null;
    const start = shiftMonth(win.start, offset);
    const end   = shiftMonth(win.end,   offset);
    const rows  = sliceByMonth(full, start, end);
    if (rows.length === 0) return null;
    const points = rows.map(r => ({ label: r.month, value: valueFor(state.metric, r) }));
    const label = state.comparison === 'prior' ? `Previous (${start} → ${end})`
                : state.comparison === 'yoy'   ? `Last year (${start} → ${end})`
                : `Prev quarter (${start} → ${end})`;
    return { label, points };
  }

  function valueFor(metricKey, row) {
    const m = METRICS.find(x => x.key === metricKey);
    if (m && m.compute) return m.compute(row);
    return row[metricKey] || 0;
  }


  // ---- Elo history ----
  // The rating trajectory is bi-weekly and cumulative (a precompute — never
  // recomputed per window). We clip the VIEW to the selected month window via
  // the parallel history_dates and label the x-axis by month so "Pn" never
  // appears. Periods the dev sat out have no entry, so a short window can show
  // very few points — handled by drawLineChart's empty/single-point paths.
  function renderEloChart() {
    const svg = document.getElementById('elo-chart');
    const tooltip = document.getElementById('elo-tooltip');
    const r = state.dev.rating || {};
    const history = r.history || [];
    const dates = r.history_dates || [];
    const win = state.window;
    const series = [];
    for (let i = 0; i < history.length; i++) {
      const date = dates[i] || '';
      const month = date.slice(0, 7); // YYYY-MM
      if (win && month && (month < win.start || month > win.end)) continue;
      series.push({ label: date ? monthLabel(date) : `P${i + 1}`, value: history[i] });
    }
    document.getElementById('elo-meta').textContent = series.length
      ? `${series.length} period${series.length === 1 ? '' : 's'} in window · current ${Math.round(r.current || 0)}`
      : `no rated periods in window · current ${Math.round(r.current || 0)}`;
    drawLineChart(svg, tooltip, series, null, { compact: true, baseline: 1000 });
  }

  const MONTH_ABBR = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

  // "2026-04-15" → "Apr '26". Parses the YYYY-MM prefix directly (no Date, so
  // no timezone surprises).
  function monthLabel(date) {
    const y = date.slice(2, 4);
    const mo = parseInt(date.slice(5, 7), 10);
    const name = MONTH_ABBR[mo - 1] || '';
    return `${name} '${y}`;
  }

  // ---- Score breakdown ----
  function renderBreakdown() {
    const wrap = document.getElementById('dev-breakdown');
    wrap.innerHTML = '';
    const score = state.dev.score;
    document.getElementById('breakdown-meta').textContent =
      score ? `total ${formatScore(score)}` : '';
    if (!score) {
      const empty = document.createElement('div');
      empty.className = 'lb-breakdown-empty';
      empty.textContent = 'No composite score for this dev (insufficient activity in this window).';
      wrap.appendChild(empty);
      return;
    }
    const entries = Object.entries(score.breakdown || {})
      .map(([k, v]) => ({ key: k, value: v, label: METRIC_BREAKDOWN_LABELS[k] || k }))
      .filter(e => Math.abs(e.value) > 1e-9)
      .sort((a, b) => Math.abs(b.value) - Math.abs(a.value));
    if (entries.length === 0) {
      const empty = document.createElement('div');
      empty.className = 'lb-breakdown-empty';
      empty.textContent = 'Every metric contributed zero (cohort spread is too tight for this dev to move).';
      wrap.appendChild(empty);
      return;
    }
    const bars = document.createElement('div');
    bars.className = 'lb-bars';
    const maxAbs = Math.max(...entries.map(e => Math.abs(e.value)));
    entries.forEach(e => {
      const row = document.createElement('div');
      row.className = 'lb-bar-row';
      row.innerHTML = `
        <div class="lb-bar-label">${e.label}</div>
        <div class="lb-bar-track"><div class="lb-bar ${e.value > 0 ? 'pos' : 'neg'}" style="width:${(Math.abs(e.value) / maxAbs) * 100}%"></div></div>
        <div class="lb-bar-value">${(e.value > 0 ? '+' : '') + e.value.toFixed(2)}</div>
      `;
      bars.appendChild(row);
    });
    wrap.appendChild(bars);

    // Integration-PR context: when the dev cut any merge-up / integration PRs
    // in the window, explain that their PR / LoC / code-impact contribution is
    // down-weighted in the score above. Display-only; absent (integration_prs
    // unset or 0) when the feature is off, so nothing shows by default.
    const totals = state.dev.totals || {};
    const intg = totals.integration_prs || 0;
    if (intg > 0) {
      const merged = totals.prs_merged || 0;
      const note = document.createElement('div');
      note.className = 'lb-breakdown-note';
      const isOne = intg === 1;
      note.textContent =
        `${formatNumber(intg)} of ${formatNumber(merged)} merged ${merged === 1 ? 'PR was' : 'PRs were'} ` +
        `${isOne ? 'an integration / merge-up PR' : 'integration / merge-up PRs'} ` +
        `(re-shipping already-merged commits) — their PR, LoC, and code-impact contribution is down-weighted in this score.`;
      wrap.appendChild(note);
    }
  }

  // ---- Projects panel (per-dev shares) ----
  function renderProjects() {
    const body = document.getElementById('dev-projects');
    if (!body) return;
    body.innerHTML = '';
    const all = state.dev.projects || [];
    let rows = all.slice();
    if (state.projectsSort === 'surge') {
      rows = rows.filter(p => (p.triggers || []).length > 0);
    }
    rows.sort(projectSorter(state.projectsSort));

    document.getElementById('projects-meta').textContent =
      `${rows.length} shown · ${all.length} total · ${surgeCount(all)} surge-triggered`;

    if (rows.length === 0) {
      const empty = document.createElement('div');
      empty.className = 'share-empty';
      empty.textContent = 'No projects match this filter for the current window.';
      body.appendChild(empty);
      return;
    }
    rows.forEach(p => body.appendChild(shareRow(p)));
  }

  function projectSorter(mode) {
    if (mode === 'absolute') {
      return (a, b) => totalAbs(b) - totalAbs(a);
    }
    if (mode === 'surge') {
      return (a, b) => totalAbs(b) - totalAbs(a);
    }
    // 'share' — sort by max share across metrics, descending.
    return (a, b) => maxShare(b) - maxShare(a);
  }

  function totalAbs(p) { return (p.dev_prs || 0) + (p.dev_commits || 0) + (p.dev_reviews || 0); }
  function maxShare(p) {
    return Math.max(
      ratio(p.dev_prs, p.team_prs),
      ratio(p.dev_commits, p.team_commits),
      ratio(p.dev_reviews, p.team_reviews),
    );
  }
  function ratio(d, t) { return t > 0 ? d / t : 0; }
  function surgeCount(arr) { return arr.filter(p => (p.triggers || []).length > 0).length; }

  function shareRow(p) {
    const row = document.createElement('div');
    row.className = 'share-row';
    if ((p.triggers || []).length > 0) row.classList.add('surge');
    row.innerHTML = `
      <div class="key" role="cell">${escapeHTML(p.epic_key)}</div>
      <div class="summary grow" role="cell">${p.summary ? escapeHTML(p.summary) : '<span class="placeholder">Epic not in cache</span>'}</div>
      <div class="num" role="cell" title="${ratioTitle(p.dev_prs, p.team_prs)}">${shareCell(p.dev_prs, p.team_prs)}</div>
      <div class="num" role="cell" title="${ratioTitle(p.dev_commits, p.team_commits)}">${shareCell(p.dev_commits, p.team_commits)}</div>
      <div class="num" role="cell" title="${ratioTitle(p.dev_reviews, p.team_reviews)}">${shareCell(p.dev_reviews, p.team_reviews)}</div>
      <div class="num" role="cell" title="${ratioTitle(p.dev_code_impact, p.team_code_impact)}">${floatShareCell(p.dev_code_impact, p.team_code_impact)}</div>
      <div class="triggers-cell" role="cell">${triggersHTML(p.triggers || [])}</div>
    `;
    return row;
  }

  // floatShareCell renders code_impact ratios. Same shape as shareCell but
  // formats the dev/team numerator+denominator with one decimal place since
  // code_impact is a derived float, not a discrete count.
  function floatShareCell(dev, team) {
    if (!team || team <= 0) return '<span class="share-dim">—</span>';
    const pct = Math.round((dev / team) * 100);
    return `<span class="share-num">${(dev || 0).toFixed(1)}</span><span class="share-of">/${team.toFixed(1)}</span><span class="share-pct"> · ${pct}%</span>`;
  }

  function shareCell(dev, team) {
    if (!team) return '<span class="share-dim">—</span>';
    const pct = Math.round((dev / team) * 100);
    return `<span class="share-num">${formatNumber(dev)}</span><span class="share-of">/${formatNumber(team)}</span><span class="share-pct"> · ${pct}%</span>`;
  }

  function ratioTitle(dev, team) {
    return team > 0 ? `${dev} of ${team} (${((dev / team) * 100).toFixed(1)}%)` : '0 of 0';
  }

  function triggersHTML(triggers) {
    return triggers.map(t => `<span class="trigger">${escapeHTML(t.replace(/_/g, ' '))}</span>`).join('');
  }

  // ---- Footer ----
  function renderFooter() {
    const s = state.data;
    if (s.generated_at) {
      document.getElementById('footer-generated').textContent =
        `Generated ${new Date(s.generated_at).toLocaleString()}`;
    }
    if (s.meta) {
      document.getElementById('footer-history').textContent =
        `${s.meta.months_loaded} months · ${s.meta.devs_mapped} devs mapped`;
    }
  }

  // ---- SVG line chart ----
  // Metrics whose monthly value isn't an accumulating count, so the shared
  // projection treatment omits (rather than extrapolates) their partial month.
  const NON_CUMULATIVE = new Set(['code_impact']);

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

  // series: [{label, value}], overlay: {label, points: [{label, value}]} | null
  function drawLineChart(svg, tooltip, series, overlay, opts = {}) {
    const vb = { w: 1200, h: opts.compact ? 240 : 320 };
    const pad = { t: 22, r: 22, b: 32, l: 56 };
    svg.setAttribute('viewBox', `0 0 ${vb.w} ${vb.h}`);
    while (svg.firstChild) svg.removeChild(svg.firstChild);
    if (tooltip) tooltip.classList.remove('visible');
    if (series.length === 0) {
      drawEmptyText(svg, vb, 'No data for this metric in the current window.');
      return;
    }

    let minY = Infinity, maxY = -Infinity;
    for (const s of series) { const v = displayValue(s); if (v < minY) minY = v; if (v > maxY) maxY = v; }
    if (overlay) for (const s of overlay.points) { const v = displayValue(s); if (v < minY) minY = v; if (v > maxY) maxY = v; }
    if (opts.baseline !== undefined) {
      if (opts.baseline < minY) minY = opts.baseline;
      if (opts.baseline > maxY) maxY = opts.baseline;
    }
    if (minY === maxY) { maxY = minY + 1; }
    if (minY > 0 && !opts.compact) minY = 0;
    const range = maxY - minY;
    const yMin = minY - range * 0.05;
    const yMax = maxY + range * 0.1;
    const yRange = yMax - yMin;

    const n = series.length;
    const xStep = n > 1 ? (vb.w - pad.l - pad.r) / (n - 1) : 0;
    const x = i => pad.l + i * xStep;
    const y = v => pad.t + (vb.h - pad.t - pad.b) * (1 - (v - yMin) / yRange);

    // Gridlines + y-axis ticks.
    const NS = 'http://www.w3.org/2000/svg';
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
      lbl.textContent = formatNumber(tv);
      svg.appendChild(lbl);
    }

    if (opts.baseline !== undefined && opts.baseline >= yMin && opts.baseline <= yMax) {
      const ref = document.createElementNS(NS, 'line');
      ref.setAttribute('x1', pad.l); ref.setAttribute('x2', vb.w - pad.r);
      ref.setAttribute('y1', y(opts.baseline)); ref.setAttribute('y2', y(opts.baseline));
      ref.setAttribute('class', 'baseline');
      svg.appendChild(ref);
    }

    const addPath = (pts, idxs, cls) => {
      if (idxs.length < 2) return;
      const dd = idxs.map((idx, k) => `${k === 0 ? 'M' : 'L'}${x(idx)},${y(displayValue(pts[idx]))}`).join(' ');
      const p = document.createElementNS(NS, 'path');
      p.setAttribute('d', dd); p.setAttribute('class', cls);
      svg.appendChild(p);
    };
    const seq = (a, b) => { const o = []; for (let i = a; i < b; i++) o.push(i); return o; };

    if (overlay) addPath(overlay.points, seq(0, overlay.points.length), 'trend-line overlay');

    const lastPartial = series.length >= 2 && series[series.length - 1].partial;
    if (lastPartial) {
      addPath(series, seq(0, series.length - 1), 'trend-line current');
      addPath(series, [series.length - 2, series.length - 1], 'trend-line current partial');
    } else {
      addPath(series, seq(0, series.length), 'trend-line current');
    }

    series.forEach((s, i) => {
      const dot = document.createElementNS(NS, 'circle');
      dot.setAttribute('cx', x(i)); dot.setAttribute('cy', y(displayValue(s)));
      dot.setAttribute('r', 2.5);
      dot.setAttribute('class', 'trend-dot current' + (s.partial ? ' partial' : ''));
      svg.appendChild(dot);
    });

    const labelEvery = Math.max(1, Math.ceil(n / 12));
    for (let i = 0; i < n; i += labelEvery) {
      const lbl = document.createElementNS(NS, 'text');
      lbl.setAttribute('x', x(i)); lbl.setAttribute('y', vb.h - pad.b + 16);
      lbl.setAttribute('text-anchor', 'middle');
      lbl.setAttribute('class', 'xtick');
      lbl.textContent = series[i].label;
      svg.appendChild(lbl);
    }

    if (tooltip) wireHover(svg, tooltip, series, overlay, x, y, vb, pad);
  }

  function wireHover(svg, tooltip, series, overlay, x, y, vb, pad) {
    const xStep = series.length > 1 ? (vb.w - pad.l - pad.r) / (series.length - 1) : 0;
    svg.onpointermove = (evt) => {
      const rect = svg.getBoundingClientRect();
      const sx = (evt.clientX - rect.left) * vb.w / rect.width;
      const relX = sx - pad.l;
      const i = Math.max(0, Math.min(series.length - 1, Math.round(relX / (xStep || 1))));
      const cur = series[i];
      const valTxt = (cur.partial && cur.projected != null)
        ? `${formatNumber(cur.value)} so far · ~${formatNumber(Math.round(cur.projected))} proj`
        : formatNumber(cur.value);
      const lines = [
        `<div class="t-label">${cur.label}${cur.partial ? ' (partial)' : ''}</div>`,
        `<div class="t-value">${valTxt}</div>`,
      ];
      if (overlay && overlay.points[i]) {
        lines.push(`<div class="t-value" style="color:var(--overlay)">${overlay.label}: ${formatNumber(overlay.points[i].value)}</div>`);
      }
      tooltip.innerHTML = lines.join('');
      tooltip.classList.add('visible');
      const left = (x(i) * rect.width / vb.w);
      const top  = (y(displayValue(cur)) * rect.height / vb.h) - 40;
      tooltip.style.left = Math.max(0, Math.min(rect.width - 160, left - 50)) + 'px';
      tooltip.style.top  = Math.max(0, top) + 'px';
    };
    svg.onpointerleave = () => { tooltip.classList.remove('visible'); };
  }

  // ---- Helpers ----
  function deltaClass(d) {
    if (d == null) return 'flat';
    return d > 0 ? 'up' : d < 0 ? 'down' : 'flat';
  }

  function formatDelta(d) {
    if (d === null || d === undefined) return '—';
    const sign = d > 0 ? '+' : '';
    return `${sign}${d.toFixed(1)}`;
  }

  function formatScore(score) {
    if (!score) return '—';
    const v = score.total;
    const sign = v > 0 ? '+' : '';
    return `${sign}${v.toFixed(1)}`;
  }

  boot();
})();
