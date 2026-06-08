/* Velocity scoring — vanilla ES2020 client (story-points engine, Phase 4).
   Review queue for the deterministic band engine: lists persisted score rows
   from /api/scoring/list, flags tickets that need a human/LLM insight pass, and
   opens a per-ticket detail (evidence + band + copy-ready /score-ticket command)
   from /api/scoring/ticket/{key}. The "Run generator" button re-bands the
   post-hoc corpus via /api/scoring/generate.

   Jira write-back (approve & post) is Phase 5 — the override input is laid out
   here but its post path is wired in the next phase. */

(() => {
  'use strict';

  const { escapeHTML, fmtDays, setActive } = window.VUtil;

  // Page size for the rendered list — the corpus is ~8k tickets, so we fetch the
  // whole filtered set once (true count) but only paint a page at a time, with a
  // "Load more" control. needs_insight rows sort first server-side.
  const PAGE_SIZE = 250;

  const state = {
    rows:     [],               // full filtered set of ScoreRecords from /api/scoring/list
    shown:    PAGE_SIZE,        // how many rows are currently painted
    filter:   'needs_insight',  // needs_insight | all
    selected: null,             // ticket key whose detail is open
  };

  async function boot() {
    wireFilterHandlers();
    wireGenerator();
    wireDetailClose();
    await loadList();
  }

  async function loadList() {
    document.getElementById('score-summary').textContent = 'Loading…';
    try {
      // Fetch the whole filtered set (no limit) so the count is honest; paint a
      // page at a time client-side via the Load-more control.
      const res = await fetch(
        `/api/scoring/list?filter=${state.filter}`,
        { cache: 'no-store' },
      );
      if (!res.ok) throw new Error(res.statusText);
      state.rows = await res.json();
      state.shown = PAGE_SIZE;
    } catch (err) {
      console.error(err);
      document.getElementById('loading').hidden = true;
      document.getElementById('error').hidden = false;
      return;
    }
    document.getElementById('loading').hidden = true;
    document.getElementById('error').hidden = true;
    renderTable();
    renderSummary();
    renderFooter();
  }

  function wireFilterHandlers() {
    document.getElementById('score-filter').addEventListener('click', (e) => {
      const btn = e.target.closest('button');
      if (!btn || btn.dataset.filter === state.filter) return;
      state.filter = btn.dataset.filter;
      setActive('score-filter', btn);
      loadList();
    });
  }

  function wireGenerator() {
    const btn = document.getElementById('run-generator');
    btn.addEventListener('click', async () => {
      if (!window.confirm(
        'Re-band every post-hoc ticket (those with a merged PR) and persist the results?\n\n' +
        'Human overrides and posted state are preserved. This can take a minute on a large corpus.',
      )) return;
      const status = document.getElementById('run-status');
      btn.disabled = true;
      status.textContent = 'Running…';
      try {
        const res = await fetch('/api/scoring/generate', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: '{}',
        });
        if (!res.ok) throw new Error(await res.text() || res.statusText);
        const r = await res.json();
        status.textContent =
          `Scored ${r.scored} · ${r.flagged} flagged · ` +
          `${r.inserted} new / ${r.updated} updated / ${r.skipped} unchanged / ${r.preserved} preserved`;
        await loadList();
      } catch (err) {
        console.error(err);
        status.textContent = `Failed: ${err.message}`;
      } finally {
        btn.disabled = false;
      }
    });
  }

  // ---- Table ----

  function renderTable() {
    const body = document.getElementById('score-body');
    body.innerHTML = '';
    if (!state.rows.length) {
      const empty = document.createElement('div');
      empty.className = 'score-empty';
      empty.textContent = state.filter === 'needs_insight'
        ? 'No flagged tickets — nothing needs an insight pass.'
        : 'No scored tickets yet. Run the generator to band the corpus.';
      body.appendChild(empty);
      return;
    }
    state.rows.slice(0, state.shown).forEach(row => body.appendChild(rowFor(row)));
    if (state.rows.length > state.shown) {
      const more = document.createElement('button');
      more.type = 'button';
      more.className = 'copy-btn load-more';
      const remaining = state.rows.length - state.shown;
      more.textContent = `Load ${Math.min(PAGE_SIZE, remaining)} more (${remaining} remaining)`;
      more.addEventListener('click', () => {
        state.shown += PAGE_SIZE;
        renderTable();
      });
      body.appendChild(more);
    }
  }

  function rowFor(rec) {
    const row = document.createElement('button');
    row.type = 'button';
    row.className = 'score-row';
    row.setAttribute('role', 'row');
    if (state.selected === rec.ticket) row.classList.add('selected');
    row.addEventListener('click', () => openDetail(rec.ticket, row));

    const ticket = cell(rec.ticket, 'ticket');
    const band = numCell(rec.band || String(rec.points));
    const points = numCell(rec.points);
    const confidence = cell('');
    confidence.appendChild(confBadge(rec.confidence));
    const flag = cell('');
    if (rec.needs_insight) flag.appendChild(badge('Needs insight', 'flag-badge'));
    const jiraSP = numCell(rec.existing_story_points > 0 ? rec.existing_story_points : null);
    const posted = cell('');
    if (rec.posted_to_jira) posted.appendChild(badge('Posted', 'posted-badge'));
    if (rec.source === 'human') posted.appendChild(badge('Override', 'override-badge'));

    [ticket, band, points, confidence, flag, jiraSP, posted].forEach(c => row.appendChild(c));
    return row;
  }

  // ---- Detail ----

  function wireDetailClose() {
    document.getElementById('detail-close').addEventListener('click', closeDetail);
  }

  // closeDetail collapses the split back to a full-width list.
  function closeDetail() {
    state.selected = null;
    document.querySelectorAll('.score-row.selected').forEach(r => r.classList.remove('selected'));
    document.getElementById('detail-panel').hidden = true;
    document.getElementById('score-layout').classList.remove('split');
  }

  async function openDetail(key, rowEl) {
    state.selected = key;
    document.querySelectorAll('.score-row.selected').forEach(r => r.classList.remove('selected'));
    if (rowEl) rowEl.classList.add('selected');

    const panel = document.getElementById('detail-panel');
    const body = document.getElementById('detail-body');
    document.getElementById('detail-key').textContent = key;
    panel.hidden = false;
    document.getElementById('score-layout').classList.add('split');
    body.innerHTML = '<div class="score-empty">Loading…</div>';
    panel.scrollIntoView({ behavior: 'smooth', block: 'nearest' });

    let detail;
    try {
      const res = await fetch(`/api/scoring/ticket/${encodeURIComponent(key)}`, { cache: 'no-store' });
      if (!res.ok) throw new Error(await res.text() || res.statusText);
      detail = await res.json();
    } catch (err) {
      console.error(err);
      body.innerHTML = `<div class="score-empty">Could not load detail: ${escapeHTML(err.message)}</div>`;
      return;
    }
    renderDetail(body, detail);
  }

  function renderDetail(body, detail) {
    const { evidence: ev, band, persisted } = detail;
    body.innerHTML = '';

    // Summary line.
    const summary = document.createElement('p');
    summary.className = 'detail-summary';
    summary.textContent = ev.summary || '(no summary)';
    body.appendChild(summary);

    // Deep link to the Jira ticket (server builds it from the configured base
    // URL; absent when unconfigured, so we render nothing rather than a stub).
    if (detail.jira_url) {
      const link = document.createElement('a');
      link.className = 'detail-jira-link';
      link.href = detail.jira_url;
      link.target = '_blank';
      link.rel = 'noopener noreferrer';
      link.textContent = `View ${ev.key} in Jira`;
      body.appendChild(link);
    }

    // Band headline: points + band range + confidence + flag.
    const head = document.createElement('div');
    head.className = 'detail-head';
    head.appendChild(stat('Points', String(band.points)));
    head.appendChild(stat('Band', band.band));
    const conf = stat('Confidence', '');
    conf.querySelector('.detail-stat-val').appendChild(confBadge(band.confidence));
    head.appendChild(conf);
    if (band.needs_insight) {
      const flag = stat('Flag', '');
      flag.querySelector('.detail-stat-val').appendChild(badge('Needs insight', 'flag-badge'));
      head.appendChild(flag);
    }
    body.appendChild(head);

    // Quadrant + drivers + signal summary.
    body.appendChild(field('Quadrant', `${band.quadrant_cell} · prior band ${band.quadrant_band}`));
    if (band.drivers && band.drivers.length) {
      const list = document.createElement('ul');
      list.className = 'detail-drivers';
      band.drivers.forEach(d => {
        const li = document.createElement('li');
        li.textContent = d;
        list.appendChild(li);
      });
      body.appendChild(labeled('Drivers', list));
    }
    if (band.signal_summary) body.appendChild(field('Signal summary', band.signal_summary));
    if (band.hardest_aspect_hint) body.appendChild(field('Hardest aspect (hint)', band.hardest_aspect_hint));

    // Evidence facts.
    body.appendChild(labeled('Evidence', evidenceFacts(ev)));

    // Copy-ready /score-ticket command for the human/LLM insight pass.
    body.appendChild(copyBlock(detail.score_ticket_command));

    // Override input — Phase 4 lays out the control; the post path is Phase 5.
    body.appendChild(overrideBlock(ev.key, band, persisted));
  }

  function evidenceFacts(ev) {
    const dl = document.createElement('dl');
    dl.className = 'detail-facts';
    const add = (k, v) => {
      if (v == null || v === '' || v === '–') return;
      const dt = document.createElement('dt'); dt.textContent = k;
      const dd = document.createElement('dd'); dd.textContent = v;
      dl.append(dt, dd);
    };
    add('Type', ev.issue_type);
    add('Status', ev.status);
    add('PRs', ev.prs ? ev.prs.length : 0);
    add('Net LOC', ev.net_loc != null ? ev.net_loc.toLocaleString() : null);
    add('Files', ev.file_count || null);
    add('Dir spread', ev.dir_spread || null);
    add('Test files', ev.test_files_touched || null);
    add('Active cycle', fmtDays(ev.active_cycle_hours));
    add('Cycle (raw)', fmtDays(ev.cycle_hours));
    add('Rework bounces', ev.rework_count || null);
    add('Review rounds', ev.review_rounds || null);
    add('Reviewers', ev.distinct_reviewers || null);
    add('Deep threads', ev.deep_threads || null);
    add('Touched-area risk', ev.touched_area_risk);
    add('Repos', ev.repos ? ev.repos.join(', ') : null);
    if (ev.hot_files && ev.hot_files.length) add('Hot files', ev.hot_files.join(', '));
    return dl;
  }

  function copyBlock(command) {
    const wrap = document.createElement('div');
    wrap.className = 'detail-copy';
    const label = document.createElement('div');
    label.className = 'detail-field-label';
    label.textContent = 'Insight pass (run in Claude Code)';
    const row = document.createElement('div');
    row.className = 'copy-row';
    const code = document.createElement('code');
    code.textContent = command;
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'copy-btn';
    btn.textContent = 'Copy';
    btn.addEventListener('click', async () => {
      try {
        await navigator.clipboard.writeText(command);
        btn.textContent = 'Copied';
        setTimeout(() => { btn.textContent = 'Copy'; }, 1500);
      } catch {
        btn.textContent = 'Copy failed';
      }
    });
    row.append(code, btn);
    wrap.append(label, row);
    return wrap;
  }

  function overrideBlock(key, band, persisted) {
    const wrap = document.createElement('div');
    wrap.className = 'detail-override';
    const label = document.createElement('div');
    label.className = 'detail-field-label';
    label.textContent = 'Override & post';
    const row = document.createElement('div');
    row.className = 'override-row';
    const input = document.createElement('input');
    input.type = 'number';
    input.min = '0';
    input.step = '1';
    input.className = 'override-input';
    input.placeholder = 'points';
    input.value = persisted && persisted.source === 'human'
      ? String(persisted.points)
      : String(band.points);
    const postBtn = document.createElement('button');
    postBtn.type = 'button';
    postBtn.className = 'run-btn';
    postBtn.textContent = 'Approve & post to Jira';
    postBtn.disabled = true;
    postBtn.title = 'Jira write-back lands in Phase 5.';
    const note = document.createElement('span');
    note.className = 'run-status';
    note.textContent = 'Posting to Jira is wired in Phase 5.';
    row.append(input, postBtn, note);
    wrap.append(label, row);
    return wrap;
  }

  // ---- Small DOM helpers ----

  function cell(text, cls) {
    const c = document.createElement('div');
    c.setAttribute('role', 'cell');
    if (cls) c.className = cls;
    if (text) c.textContent = text;
    return c;
  }

  function numCell(value) {
    const c = cell('', 'num');
    c.textContent = value == null ? '—' : String(value);
    return c;
  }

  function badge(text, cls) {
    const b = document.createElement('span');
    b.className = `score-badge ${cls}`;
    b.textContent = text;
    return b;
  }

  function confBadge(confidence) {
    const map = { high: 'conf-high', medium: 'conf-med', low: 'conf-low' };
    return badge(confidence || 'low', `conf-badge ${map[confidence] || 'conf-low'}`);
  }

  function stat(label, value) {
    const s = document.createElement('div');
    s.className = 'detail-stat';
    const l = document.createElement('div'); l.className = 'detail-stat-label'; l.textContent = label;
    const v = document.createElement('div'); v.className = 'detail-stat-val'; if (value) v.textContent = value;
    s.append(l, v);
    return s;
  }

  function field(label, value) {
    const p = document.createElement('div');
    p.className = 'detail-field';
    const l = document.createElement('div'); l.className = 'detail-field-label'; l.textContent = label;
    const v = document.createElement('div'); v.className = 'detail-field-val'; v.textContent = value;
    p.append(l, v);
    return p;
  }

  function labeled(label, node) {
    const p = document.createElement('div');
    p.className = 'detail-field';
    const l = document.createElement('div'); l.className = 'detail-field-label'; l.textContent = label;
    p.append(l, node);
    return p;
  }

  function renderSummary() {
    const n = state.rows.length;
    const flagged = state.rows.filter(r => r.needs_insight).length;
    const label = state.filter === 'needs_insight' ? 'flagged tickets' : 'scored tickets';
    let text = `${n} ${label}`;
    if (state.filter === 'all' && flagged) text += ` · ${flagged} need insight`;
    if (n > state.shown) text += ` (showing ${state.shown})`;
    document.getElementById('score-summary').textContent = text;
    document.getElementById('score-meta').textContent = `scorer velocity-band-v1`;
  }

  function renderFooter() {
    // Newest scored_at across the loaded rows = a reasonable "as of" stamp.
    let latest = null;
    state.rows.forEach(r => {
      if (r.scored_at && (!latest || r.scored_at > latest)) latest = r.scored_at;
    });
    if (latest) {
      document.getElementById('footer-generated').textContent =
        `Scored as of ${new Date(latest).toLocaleString()}`;
    }
  }

  boot();
})();
