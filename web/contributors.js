/* Velocity contributors — vanilla ES2020 client.
   Renders a sortable table of every mapped dev. The Window picker (P2.3)
   re-ranks the cohort for an arbitrary scoring window via the on-demand
   /api/contributors endpoint; metrics.json is still read once for page
   context (current month, full-history month math, generated_at). Full
   per-dev breakdown lives on /dev/<login>. */

(() => {
  'use strict';

  const { formatNumber, devLogin, setActive, shiftMonth, clampMonth } = window.VUtil;

  // Column → value accessor on a dev entry. Numeric accessors return 0 (not
  // null) so the comparator stays stable for devs missing a rating/score.
  const ACCESSORS = {
    name:          d => (d.dev.display_name || '').toLowerCase(),
    rating:        d => d.rating?.current ?? 0,
    composite:     d => d.score?.total ?? 0,
    code_impact:   d => d.totals?.code_impact ?? 0,
    prs_merged:    d => d.totals?.prs_merged ?? 0,
    prs_reviewed:  d => d.totals?.prs_reviewed ?? 0,
    jira_resolved: d => d.totals?.jira_issues_resolved ?? 0,
    active_weeks:  d => d.totals?.active_weeks ?? 0,
  };

  // Default sort direction per column. Name is alpha-asc; everything else
  // ranks high → low so the strongest contributors land at the top.
  const DEFAULT_DIR = {
    name: 'asc', rating: 'desc', composite: 'desc', code_impact: 'desc',
    prs_merged: 'desc', prs_reviewed: 'desc', jira_resolved: 'desc', active_weeks: 'desc',
  };

  const state = {
    data:    null,    // metrics.json — page context (window math, generated_at)
    devs:    [],      // current windowed cohort, from /api/contributors
    range:   '3m',    // selected scoring window; default mirrors the current window
    window:  null,    // {start, end, label} resolved from range
    sortKey: 'rating',
    sortDir: 'desc',
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
    document.getElementById('loading').hidden = true;
    wireSortHandlers();
    wireRangeHandlers();
    await loadWindow();
  }

  // Resolve the selected range to a [start, end] month span, anchored at the
  // latest cached month — mirrors leaderboard.js teamWindow() so the two pages
  // agree on what "3M"/"YTD"/etc. mean.
  function contribWindow() {
    const s = state.data;
    const history = (s.full_history && s.full_history.monthly) || [];
    const current = s.current_month;
    const firstMonth = (history[0] && history[0].month) || current;
    const back = (n) => clampMonth(shiftMonth(current, -n), firstMonth);
    switch (state.range) {
      case '3m':  return { start: back(2),  end: current, label: 'Last 3 months' };
      case '6m':  return { start: back(5),  end: current, label: 'Last 6 months' };
      case '12m': return { start: back(11), end: current, label: 'Last 12 months' };
      case 'ytd': return { start: `${current.slice(0, 4)}-01`, end: current, label: `YTD ${current.slice(0, 4)}` };
      case 'all':
      default:    return { start: firstMonth, end: current, label: 'Full history' };
    }
  }

  // loadWindow recomputes the cohort for the selected window server-side and
  // re-renders. The leaderboard/contributors composite is window-relative
  // (z-scored across the cohort), so a different window genuinely re-ranks —
  // this is the work the precomputed metrics.json blob couldn't do.
  async function loadWindow() {
    const win = contribWindow();
    state.window = win;
    document.getElementById('contrib-summary').textContent = 'Loading…';
    try {
      const res = await fetch(`/api/contributors?from=${win.start}&to=${win.end}`, { cache: 'no-store' });
      if (!res.ok) throw new Error(res.statusText);
      state.devs = await res.json();
    } catch (err) {
      console.error(err);
      document.getElementById('contrib-summary').textContent = 'Could not load window';
      return;
    }
    renderAll();
  }

  function wireRangeHandlers() {
    document.getElementById('contrib-range').addEventListener('click', (e) => {
      const btn = e.target.closest('button');
      if (!btn || btn.dataset.range === state.range) return;
      state.range = btn.dataset.range;
      setActive('contrib-range', btn);
      loadWindow();
    });
  }

  function wireSortHandlers() {
    document.querySelectorAll('.contrib-head [data-sort]').forEach(h => {
      h.addEventListener('click', () => {
        const key = h.dataset.sort;
        if (state.sortKey === key) {
          state.sortDir = state.sortDir === 'asc' ? 'desc' : 'asc';
        } else {
          state.sortKey = key;
          state.sortDir = DEFAULT_DIR[key] || 'desc';
        }
        renderAll();
      });
    });
  }

  function renderAll() {
    renderHeader();
    renderTable();
    renderFooter();
    decorateSortHeaders();
  }

  function devs() {
    return (state.devs || []).filter(d => d.dev && d.dev.display_name !== 'unknown');
  }

  function renderHeader() {
    const all = devs();
    const win = state.window;
    document.getElementById('contrib-summary').textContent =
      `${all.length} mapped devs · ${win.label} (${win.start} → ${win.end})`;
    document.getElementById('contrib-meta').textContent =
      `sorted by ${state.sortKey.replace(/_/g, ' ')} (${state.sortDir})`;
  }

  function renderTable() {
    const body = document.getElementById('contrib-body');
    body.innerHTML = '';
    const accessor = ACCESSORS[state.sortKey] || (() => 0);
    const dir = state.sortDir === 'asc' ? 1 : -1;
    const rows = devs().slice().sort((a, b) => {
      const av = accessor(a), bv = accessor(b);
      if (av < bv) return -1 * dir;
      if (av > bv) return  1 * dir;
      // Tie-break alphabetical by display name so re-renders are stable.
      const aN = a.dev.display_name.toLowerCase();
      const bN = b.dev.display_name.toLowerCase();
      return aN < bN ? -1 : aN > bN ? 1 : 0;
    });
    rows.forEach(d => body.appendChild(rowFor(d)));
  }

  function rowFor(d) {
    const row = document.createElement('a');
    row.className = 'contrib-row';
    row.href = '/dev/' + (devLogin(d) || '');
    row.setAttribute('role', 'row');
    if (d.rating?.provisional) row.classList.add('provisional');

    const name = cell('', 'name');
    name.textContent = d.dev.display_name;
    if (d.rating?.provisional) {
      const badge = document.createElement('span');
      badge.className = 'provisional-badge';
      badge.textContent = 'Provisional';
      badge.title = `Fewer than the configured threshold of established periods played (${d.rating.periods_played}). Rating is still settling.`;
      name.appendChild(badge);
    }

    const rating = numCell(d.rating ? Math.round(d.rating.current) : null);
    const composite = numCell(d.score ? formatSignedFloat(d.score.total) : null);
    const codeImpact = numCell(
      d.totals?.code_impact != null ? Math.round(d.totals.code_impact * 10) / 10 : null,
    );
    const prsMerged = numCell(d.totals?.prs_merged ?? null);
    const reviews = numCell(d.totals?.prs_reviewed ?? null);
    const jiraResolved = numCell(d.totals?.jira_issues_resolved ?? null);
    const activeWeeks = numCell(d.totals?.active_weeks ?? null);

    [name, rating, composite, codeImpact, prsMerged, reviews, jiraResolved, activeWeeks].forEach(c => row.appendChild(c));
    return row;
  }

  function cell(text, cls) {
    const c = document.createElement('div');
    c.setAttribute('role', 'cell');
    if (cls) c.className = cls;
    if (text) c.textContent = text;
    return c;
  }

  function numCell(value) {
    const c = cell('', 'num');
    c.textContent = value == null ? '—' : (typeof value === 'string' ? value : formatNumber(value));
    return c;
  }

  function decorateSortHeaders() {
    document.querySelectorAll('.contrib-head [data-sort]').forEach(h => {
      h.classList.remove('sort-asc', 'sort-desc');
      if (h.dataset.sort === state.sortKey) {
        h.classList.add(state.sortDir === 'asc' ? 'sort-asc' : 'sort-desc');
      }
    });
  }

  function renderFooter() {
    const s = state.data;
    if (s.generated_at) {
      document.getElementById('footer-generated').textContent =
        `Generated ${new Date(s.generated_at).toLocaleString()}`;
    }
  }

  function formatSignedFloat(v) {
    if (v === null || v === undefined) return '—';
    const sign = v > 0 ? '+' : '';
    return `${sign}${v.toFixed(1)}`;
  }

  boot();
})();
