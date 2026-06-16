/* Velocity shared utilities — pure data/format/DOM helpers used across every
   page script. Loaded (deferred) before charts.js and the per-page script;
   exposes window.VUtil. No build step — plain global by design. */
(() => {
  'use strict';

  // Compact number formatting: 1.2M / 12.3k / raw. En-dash for nullish.
  function formatNumber(n) {
    if (n === null || n === undefined) return '–';
    if (Math.abs(n) >= 1_000_000) return (n / 1_000_000).toFixed(1).replace(/\.0$/, '') + 'M';
    if (Math.abs(n) >= 10_000)    return (n / 1000).toFixed(1).replace(/\.0$/, '') + 'k';
    return String(n);
  }

  // Hours → "N.Nd"; en-dash for missing/zero.
  function fmtDays(hours) {
    if (!hours || hours <= 0) return '–';
    return `${(hours / 24).toFixed(1)}d`;
  }

  function escapeHTML(s) {
    return String(s).replace(/[&<>"']/g, c => ({
      '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'
    }[c]));
  }

  // ---- Month math (YYYY-MM strings) ----
  function shiftMonth(s, offset) {
    const [y, m] = s.split('-').map(Number);
    const d = new Date(Date.UTC(y, m - 1 + offset, 1));
    return `${d.getUTCFullYear()}-${String(d.getUTCMonth() + 1).padStart(2, '0')}`;
  }
  function monthDelta(a, b) {
    const [ay, am] = a.split('-').map(Number);
    const [by, bm] = b.split('-').map(Number);
    return (by - ay) * 12 + (bm - am);
  }
  function clampMonth(m, floor) { return m < floor ? floor : m; }

  // Coarse ISO-week (YYYY-Wnn) → YYYY-MM via the week midpoint.
  function isoWeekToApproxMonth(weekStr) {
    if (!weekStr) return null;
    const match = /^(\d{4})-W(\d{2})$/.exec(weekStr);
    if (!match) return null;
    const y = Number(match[1]);
    const w = Number(match[2]);
    const d = new Date(Date.UTC(y, 0, 4 + (w - 1) * 7));
    return `${d.getUTCFullYear()}-${String(d.getUTCMonth() + 1).padStart(2, '0')}`;
  }

  // Jira base URL for {base}/browse/{KEY} deep links, read from the server-
  // rendered <meta name="jira-base"> (empty when no Jira base is configured).
  function jiraBase() {
    const el = document.querySelector('meta[name="jira-base"]');
    return (el && el.content || '').replace(/\/+$/, '');
  }

  // Build a Jira browse URL, or '' when no base is configured / no key — so
  // callers render plain text rather than a broken link. Mirrors the server's
  // jiraBrowseURL.
  function jiraBrowseURL(key) {
    const base = jiraBase();
    if (!base || !key) return '';
    return `${base}/browse/${encodeURIComponent(key)}`;
  }

  // Show/hide the per-chart loading overlay for the chart whose <svg> has the
  // given id. The overlay is a `.chart-loading` element inside the same
  // `.chart-wrap`; no-op when the markup isn't present. Used while an async
  // fetch backing that chart is in flight.
  function chartLoading(svgId, on) {
    const svg = document.getElementById(svgId);
    const wrap = svg && svg.closest('.chart-wrap');
    const el = wrap && wrap.querySelector('.chart-loading');
    if (el) el.hidden = !on;
  }

  // Toggle the .active class within a button group to the clicked button.
  function setActive(groupId, btn) {
    document.querySelectorAll(`#${groupId} button`).forEach(b => b.classList.remove('active'));
    btn.classList.add('active');
  }

  // Canonical handle for a dev's profile URL. Prefers the analyzer-resolved
  // primary_login (activity-weighted across the full cache); falls back to the
  // first declared github_logins entry for older metrics.json payloads that
  // pre-date the field.
  function devLogin(d) {
    if (d.primary_login) return d.primary_login;
    const logins = d.dev && d.dev.github_logins;
    return (logins && logins.length) ? logins[0] : '';
  }

  window.VUtil = {
    formatNumber, fmtDays, escapeHTML,
    shiftMonth, monthDelta, clampMonth, isoWeekToApproxMonth,
    setActive, devLogin, jiraBase, jiraBrowseURL, chartLoading,
  };
})();
