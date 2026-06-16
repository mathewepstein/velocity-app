/* Velocity scoring — vanilla ES2020 client (story-points engine, Phase 4).
   Review queue for the deterministic band engine: lists persisted score rows
   from /api/scoring/list, flags tickets that need a human/LLM insight pass, and
   opens a per-ticket detail (evidence + band + copy-ready /score-ticket command)
   from /api/scoring/ticket/{key}. The "Run generator" button re-bands the
   post-hoc corpus via /api/scoring/generate.

   Jira write-back (Phase 5): both the bulk select-bar and the per-ticket
   "Approve & post" override block post via /api/scoring/post (dry-run preview →
   confirm → live), gated on /api/scoring/jira-status. */

(() => {
  'use strict';

  const { escapeHTML, fmtDays } = window.VUtil;

  // Page size for the rendered list — the corpus is ~8k tickets, so we fetch the
  // whole set once and filter/paginate client-side: faceted filters narrow it,
  // a "Load more" control paints a page at a time.
  const PAGE_SIZE = 250;

  // Client-side facets over the fetched rows. Defaults reproduce the original
  // review queue (flagged only); everything else starts at "any".
  const state = {
    all:      [],          // full set of ScoreRecords from /api/scoring/list
    shown:    PAGE_SIZE,   // how many filtered rows are currently painted
    facets: {
      flag:       'needs', // any | needs | clean
      confidence: 'any',   // any | high | medium | low
      band:       'any',   // any | 1 | 2 | 3 | 5 | 8 | 13 (matches points)
      posted:     'any',   // any | yes | no
      sp:         'any',   // any | has | none  (existing Jira story points)
      source:     'any',   // any | auto | human
      date:       'all',   // all | 3m | 6m | 12m | ytd  (resolved date window)
    },
    search:       '',            // ticket-key substring filter (lower-cased)
    selected:     null,          // ticket key whose detail is open
    selectedKeys: new Set(),     // tickets checked for a bulk action
    jira:         { configured: false, can_write: false, detail: '' }, // /api/scoring/jira-status
  };

  // filteredRows applies the active facets to the fetched set.
  function filteredRows() {
    const f = state.facets;
    return state.all.filter((r) => {
      if (state.search && !r.ticket.toLowerCase().includes(state.search)) return false;
      if (f.flag === 'needs' && !r.needs_insight) return false;
      if (f.flag === 'clean' && r.needs_insight) return false;
      if (f.confidence !== 'any' && r.confidence !== f.confidence) return false;
      if (f.band !== 'any' && r.points !== Number(f.band)) return false;
      if (f.posted === 'yes' && !r.posted_to_jira) return false;
      if (f.posted === 'no' && r.posted_to_jira) return false;
      if (f.sp === 'has' && !(r.existing_story_points > 0)) return false;
      if (f.sp === 'none' && r.existing_story_points > 0) return false;
      if (f.source !== 'any' && r.source !== f.source) return false;
      if (f.date !== 'all') {
        // Filter on the ticket's resolved date, falling back to created for an
        // open ticket. Undated rows (scored before the date columns existed, or
        // with neither date) are always shown — never silently filtered out.
        const raw = r.resolved_at || r.created_at;
        if (raw) {
          const d = new Date(raw);
          if (!isNaN(d.valueOf()) && d < dateCutoff(f.date)) return false;
        }
      }
      return true;
    });
  }

  // dateCutoff returns the start of the selected window relative to today's
  // client clock (a localhost dashboard — day-level drift is irrelevant for
  // multi-month windows). 'all' is handled by the caller (no cutoff); 'ytd' is
  // Jan 1 of the current year; the rolling windows subtract whole months.
  function dateCutoff(win) {
    const now = new Date();
    if (win === 'ytd') return new Date(now.getFullYear(), 0, 1);
    const months = { '3m': 3, '6m': 6, '12m': 12 }[win] || 0;
    return new Date(now.getFullYear(), now.getMonth() - months, now.getDate());
  }

  async function boot() {
    wireFacetHandlers();
    wireSearch();
    wireGenerator();
    wireDetailClose();
    wireSelection();
    await loadList();
    await loadJiraStatus();
  }

  function wireSearch() {
    document.getElementById('ticket-search').addEventListener('input', (e) => {
      state.search = e.target.value.trim().toLowerCase();
      state.shown = PAGE_SIZE; // reset pagination on a new query
      // Selection is keyed by ticket and survives filtering (unlike a facet
      // change), so a search-as-you-type doesn't wipe an in-progress selection.
      renderTable();
      renderSummary();
      renderSelectBar();
    });
  }

  // loadJiraStatus checks whether the server can write to Jira so the post
  // button reflects reality (no token / missing scope → stays disabled with a
  // reason). Failure is non-fatal — the button just stays disabled.
  async function loadJiraStatus() {
    try {
      const res = await fetch('/api/scoring/jira-status', { cache: 'no-store' });
      if (res.ok) state.jira = await res.json();
    } catch (err) {
      console.error(err);
    }
    updatePostButton();
  }

  async function loadList() {
    document.getElementById('score-summary').textContent = 'Loading…';
    try {
      // Fetch the whole set once (no server filter); facets narrow it
      // client-side, the Load-more control paginates the result.
      const res = await fetch('/api/scoring/list', { cache: 'no-store' });
      if (!res.ok) throw new Error(res.statusText);
      state.all = await res.json();
      state.shown = PAGE_SIZE;
      state.selectedKeys.clear();
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
    renderSelectBar();
    renderFooter();
  }

  function wireFacetHandlers() {
    document.getElementById('score-filters').addEventListener('click', (e) => {
      const btn = e.target.closest('.control-group button');
      if (!btn) return;
      const group = btn.closest('.control-group');
      const facet = group.dataset.facet;
      const val = btn.dataset.val;
      if (state.facets[facet] === val) return;
      state.facets[facet] = val;
      group.querySelectorAll('button').forEach((b) => b.classList.toggle('active', b === btn));
      state.shown = PAGE_SIZE;          // reset pagination on any filter change
      state.selectedKeys.clear();       // selection clears on filter change (plan)
      renderTable();
      renderSummary();
      renderSelectBar();
    });
  }

  // ---- Selection / bulk action bar ----

  function wireSelection() {
    document.getElementById('select-all').addEventListener('change', (e) => {
      const checked = e.target.checked;
      filteredRows().forEach((r) => {
        if (checked) state.selectedKeys.add(r.ticket);
        else state.selectedKeys.delete(r.ticket);
      });
      renderTable();
      renderSelectBar();
    });
    document.getElementById('clear-selection').addEventListener('click', () => {
      state.selectedKeys.clear();
      renderTable();
      renderSelectBar();
    });
    document.getElementById('post-selected').addEventListener('click', previewPost);
  }

  // updatePostButton enables Post-to-Jira only when the server can write and at
  // least one ticket is selected, and surfaces the reason in the tooltip when
  // it can't.
  function updatePostButton() {
    const btn = document.getElementById('post-selected');
    if (!btn) return;
    const n = state.selectedKeys.size;
    const canWrite = state.jira && state.jira.can_write;
    btn.disabled = !canWrite || n === 0;
    if (!canWrite) {
      btn.title = state.jira && state.jira.detail
        ? `Posting disabled: ${state.jira.detail}`
        : 'Posting disabled: no Jira token configured on the server.';
    } else {
      btn.title = n === 0 ? 'Select tickets to post' : `Post ${n} selected ticket(s) to Jira`;
    }
  }

  // ---- Jira write-back (dry-run preview → confirm → live post) ----

  async function postRequest(keys, dryRun) {
    const res = await fetch('/api/scoring/post', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ tickets: keys, dry_run: dryRun }),
    });
    if (!res.ok) throw new Error((await res.text()) || res.statusText);
    return res.json();
  }

  // previewPost runs a dry-run for the selected tickets and renders the exact
  // comments that would be posted, with a confirm button. No write happens here.
  async function previewPost() {
    const keys = [...state.selectedKeys];
    if (!keys.length) return;
    const btn = document.getElementById('post-selected');
    btn.disabled = true;
    try {
      const report = await postRequest(keys, true);
      showPostPreview(keys, report);
    } catch (err) {
      console.error(err);
      window.alert(`Preview failed: ${err.message}`);
    } finally {
      updatePostButton();
    }
  }

  function showPostPreview(keys, report) {
    const panel = document.getElementById('detail-panel');
    const body = document.getElementById('detail-body');
    document.getElementById('detail-key').textContent = 'post preview';
    panel.hidden = false;
    document.getElementById('score-layout').classList.add('split');
    panel.scrollIntoView({ behavior: 'smooth', block: 'nearest' });

    const willPost = report.results.filter((r) => r.action === 'preview');
    const skipped = report.results.filter((r) => r.action !== 'preview');

    const parts = [];
    parts.push(
      `<p class="post-summary"><strong>${willPost.length}</strong> ticket(s) will be written to Jira` +
        (skipped.length ? ` · ${skipped.length} skipped (already posted / no score)` : '') + '.</p>',
    );
    willPost.forEach((r) => {
      parts.push(
        `<div class="post-card"><div class="post-card-head">${escapeHTML(r.ticket)} → ${r.points} pts</div>` +
          `<pre class="post-comment">${escapeHTML((r.comment || []).join('\n'))}</pre></div>`,
      );
    });
    if (!willPost.length) {
      parts.push('<p class="score-empty">Nothing to post — all selected tickets are already posted or have no score.</p>');
    }
    parts.push(
      '<div class="post-actions">' +
        (willPost.length ? '<button type="button" class="run-btn" id="confirm-post">Confirm — post to Jira</button>' : '') +
        '<button type="button" class="copy-btn" id="cancel-post">Cancel</button></div>',
    );
    parts.push('<div class="meta" id="post-result"></div>');
    body.innerHTML = parts.join('');

    const cancel = document.getElementById('cancel-post');
    if (cancel) cancel.addEventListener('click', closeDetail);
    const confirm = document.getElementById('confirm-post');
    if (confirm) confirm.addEventListener('click', () => livePost(keys));
  }

  // livePost performs the real write (dry_run:false). The server is idempotent
  // and skips already-posted rows, so re-posting the same selection is safe.
  async function livePost(keys) {
    const confirm = document.getElementById('confirm-post');
    const result = document.getElementById('post-result');
    if (confirm) confirm.disabled = true;
    if (result) result.textContent = 'Posting…';
    try {
      const report = await postRequest(keys, false);
      if (result) {
        result.textContent =
          `${report.posted} posted · ${report.already_posted} already · ` +
          `${report.no_score} no-score · ${report.errors} error(s).`;
      }
      const errs = report.results.filter((r) => r.action === 'error');
      if (errs.length) console.error('post errors', errs);
      state.selectedKeys.clear();
      await loadList();       // refresh posted_to_jira state on the rows
      renderSelectBar();
    } catch (err) {
      console.error(err);
      if (result) result.textContent = `Failed: ${err.message}`;
    } finally {
      if (confirm) confirm.disabled = false;
    }
  }

  function toggleSelect(key, checked, rowEl) {
    if (checked) state.selectedKeys.add(key);
    else state.selectedKeys.delete(key);
    if (rowEl) rowEl.classList.toggle('checked', checked);
    syncSelectAllState();
    renderSelectBar();
  }

  // syncSelectAllState reflects the header checkbox against the filtered set:
  // checked when all are selected, indeterminate when some are.
  function syncSelectAllState() {
    const rows = filteredRows();
    const sel = rows.reduce((n, r) => n + (state.selectedKeys.has(r.ticket) ? 1 : 0), 0);
    const cb = document.getElementById('select-all');
    cb.checked = rows.length > 0 && sel === rows.length;
    cb.indeterminate = sel > 0 && sel < rows.length;
  }

  function renderSelectBar() {
    const bar = document.getElementById('select-bar');
    const n = state.selectedKeys.size;
    bar.hidden = n === 0;
    document.getElementById('select-count').textContent = `${n} selected`;
    syncSelectAllState();
    updatePostButton();
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
    const rows = filteredRows();
    syncSelectAllState();
    if (!rows.length) {
      const empty = document.createElement('div');
      empty.className = 'score-empty';
      empty.textContent = state.all.length
        ? 'No tickets match the current filters.'
        : 'No scored tickets yet. Run the generator to band the corpus.';
      body.appendChild(empty);
      return;
    }
    rows.slice(0, state.shown).forEach(row => body.appendChild(rowFor(row)));
    if (rows.length > state.shown) {
      const more = document.createElement('button');
      more.type = 'button';
      more.className = 'copy-btn load-more';
      const remaining = rows.length - state.shown;
      more.textContent = `Load ${Math.min(PAGE_SIZE, remaining)} more (${remaining} remaining)`;
      more.addEventListener('click', () => {
        state.shown += PAGE_SIZE;
        renderTable();
        renderSummary();
      });
      body.appendChild(more);
    }
  }

  function rowFor(rec) {
    const row = document.createElement('div');
    row.className = 'score-row';
    row.setAttribute('role', 'row');
    row.tabIndex = 0;
    if (state.selected === rec.ticket) row.classList.add('selected');
    if (state.selectedKeys.has(rec.ticket)) row.classList.add('checked');
    const open = () => openDetail(rec.ticket, row);
    row.addEventListener('click', open);
    row.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') { e.preventDefault(); open(); }
    });

    const check = cell('', 'check-col');
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.checked = state.selectedKeys.has(rec.ticket);
    cb.setAttribute('aria-label', `Select ${rec.ticket}`);
    cb.addEventListener('click', (e) => e.stopPropagation()); // don't open the detail
    cb.addEventListener('change', () => toggleSelect(rec.ticket, cb.checked, row));
    check.appendChild(cb);

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

    [check, ticket, band, points, confidence, flag, jiraSP, posted].forEach(c => row.appendChild(c));
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

    // Why it was flagged, when the engine couldn't confidently band it.
    if (band.needs_insight && band.insight_reason) {
      body.appendChild(field('Why flagged', band.insight_reason));
    }

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

    // Override control — constrained to the configured scale steps.
    body.appendChild(overrideBlock(ev.key, band, persisted, detail.scale));
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

  // Allowed override values: the server-provided configured scale, falling back
  // to the standard Fibonacci ladder for older payloads that predate the field.
  const DEFAULT_SCALE = [1, 2, 3, 5, 8, 13];

  function overrideBlock(key, band, persisted, scale) {
    const steps = (Array.isArray(scale) && scale.length) ? scale : DEFAULT_SCALE;
    const wrap = document.createElement('div');
    wrap.className = 'detail-override';
    const label = document.createElement('div');
    label.className = 'detail-field-label';
    label.textContent = 'Override & post';
    const row = document.createElement('div');
    row.className = 'override-row';
    // A dropdown of the configured scale steps — overrides must land on-scale
    // (matches the engine's snap and what posts to Jira), so no free integers.
    const input = document.createElement('select');
    input.className = 'override-input';
    const current = persisted && persisted.source === 'human'
      ? persisted.points
      : band.points;
    for (const step of steps) {
      const opt = document.createElement('option');
      opt.value = String(step);
      opt.textContent = String(step);
      if (step === current) opt.selected = true;
      input.appendChild(opt);
    }
    const postBtn = document.createElement('button');
    postBtn.type = 'button';
    postBtn.className = 'run-btn';
    postBtn.textContent = 'Approve & post to Jira';
    const note = document.createElement('span');
    note.className = 'run-status';

    const canWrite = state.jira && state.jira.can_write;
    postBtn.disabled = !canWrite;
    if (!canWrite) {
      const why = state.jira && state.jira.detail
        ? state.jira.detail
        : 'no Jira token configured on the server';
      postBtn.title = `Posting disabled: ${why}`;
      note.textContent = `Posting unavailable — ${why}.`;
    } else {
      postBtn.title = 'Persist any override, preview the comment, then post to Jira';
    }

    postBtn.addEventListener('click', async () => {
      const pts = parseInt(input.value, 10);
      if (Number.isNaN(pts) || pts < 0) {
        note.textContent = 'Enter a non-negative integer.';
        return;
      }
      postBtn.disabled = true;
      note.textContent = pts !== band.points ? 'Saving override…' : 'Preparing preview…';
      try {
        // Persist the human override first (ground truth) when the value
        // differs from the deterministic band; the dry-run then reflects it.
        if (pts !== band.points) {
          const r = await fetch('/api/scoring/override', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ ticket: key, points: pts }),
          });
          if (!r.ok) throw new Error((await r.text()) || r.statusText);
        }
        // Dry-run → render the comment with a Confirm button (shared with the
        // bulk flow), scoped to this one ticket.
        const report = await postRequest([key], true);
        showPostPreview([key], report);
      } catch (err) {
        console.error(err);
        note.textContent = `Failed: ${err.message}`;
        postBtn.disabled = false;
      }
    });

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
    const total = state.all.length;
    const rows = filteredRows();
    const n = rows.length;
    let text = `${n} ticket${n === 1 ? '' : 's'}`;
    if (n !== total) text += ` of ${total}`;
    if (n > state.shown) text += ` · showing ${state.shown}`;
    document.getElementById('score-summary').textContent = text;
    document.getElementById('score-meta').textContent = `scorer velocity-band-v1`;
  }

  function renderFooter() {
    // Newest scored_at across the loaded rows = a reasonable "as of" stamp.
    let latest = null;
    state.all.forEach(r => {
      if (r.scored_at && (!latest || r.scored_at > latest)) latest = r.scored_at;
    });
    if (latest) {
      document.getElementById('footer-generated').textContent =
        `Scored as of ${new Date(latest).toLocaleString()}`;
    }
  }

  boot();
})();
