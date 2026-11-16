/**
 * Chaturbate & Stripchat top-channels autopilot — Cloudflare Worker
 * ------------------------------------------------------------------
 * Every 15 minutes (Cron Trigger) this worker wakes up, but it only performs a
 * real scan every `scan_interval_minutes` (default 45) — see the cadence gate
 * below. Each scan inspects:
 *   1. Chaturbate's public "female" and "couple" room listings
 *   2. Stripchat's public "couple" room listings (sorted by top viewers)
 *
 * Keeps a healthy BUFFER of the most-viewed PUBLIC rooms ready in the shared
 * Supabase `channel_assignments` pool (status = "unassigned").
 * Go DVR nodes claim "unassigned" rows when CHANNEL_POOL_MODE = pooled.
 *
 * Multi-Site Behaviour:
 *   - Chaturbate: female & couple rooms (filtered by min_viewers, default 20k)
 *   - Stripchat: couple category rooms (filtered by min_viewers, default 200)
 *   - Only PUBLIC and live streams qualify.
 *   - Automatic site tagging: `site = 'chaturbate'` or `site = 'stripchat'`.
 *   - Direct fetch is attempted first (free, 0 browser seconds).
 *   - Headless Chromium (Browser Rendering) is used only as fallback if direct
 *     fetch is challenged by Cloudflare anti-bot.
 *   - Strict daily browser budget accounting to never exceed Cloudflare limits.
 *
 * Config (Supabase `app_settings` key `autopilot_config`):
 *   { min_viewers, target_buffer, categories:[{key,label,site,target_buffer,min_viewers}],
 *     stale_minutes, scan_interval_minutes, browser_daily_seconds, browser_min_spacing_seconds }
 */

import puppeteer from '@cloudflare/puppeteer';

// ---- Defaults (used until autopilot_config exists in app_settings) ----------
const DEFAULT_CONFIG = {
  min_viewers: 20000,
  target_buffer: 10,
  categories: [
    { key: 'f', label: 'female', site: 'chaturbate', target_buffer: 1, min_viewers: 20000 },
    { key: 'c', label: 'couple', site: 'chaturbate', target_buffer: 2, min_viewers: 20000 },
    { key: 'sc_c', label: 'couple', site: 'stripchat', target_buffer: 2, min_viewers: 200 },
  ],
  stale_minutes: 70,
  scan_interval_minutes: 45,
  browser_daily_seconds: 480,
  browser_min_spacing_seconds: 90,
};
const DEFAULT_UA =
  'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 ' +
  '(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36';
const SESSION_TTL = 2_592_000; // 30 days
const RATE_LIMIT_KEY = 'rate_limited_until';
const RATE_LIMIT_STREAK_KEY = 'rate_limit_streak';
const RL_BASE_MS = 5 * 60 * 1000;
const RL_MAX_MS = 60 * 60 * 1000;
const SCAN_LOCK_KEY = 'scan_lock';
const SCAN_LOCK_TTL_SEC = 180;
const SCAN_INTERVAL_KEY = 'last_scan_at';
const DEFAULT_SCAN_INTERVAL_MIN = 45;

const BROWSER_BUDGET_DAY_KEY = 'browser_budget_day';
const BROWSER_USED_KEY = 'browser_seconds_used';
const BROWSER_LAST_LAUNCH_KEY = 'browser_last_launch_at';
const BROWSER_LAUNCH_ESTIMATE_SEC = 20;
const ROOMLIST_MAX_ATTEMPTS = 6;
const ROOMLIST_RETRY_MS = 1500;
const STALE_ALERT_KEY = 'stale_alert_at';
const STALE_ALERT_INTERVAL_MS = 6 * 60 * 60 * 1000;
const CONFIG_KEY = 'autopilot_config';
const SCANS_KEY = 'autopilot_scans';
const HEARTBEAT_KEY = 'autopilot_heartbeat';

function jsonResponse(obj, status = 200) {
  return new Response(JSON.stringify(obj, null, 2), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

// ---- Supabase helpers -------------------------------------------------------
function sbHeaders(env, extra = {}) {
  return {
    apikey: env.SUPABASE_SERVICE_ROLE_KEY,
    Authorization: `Bearer ${env.SUPABASE_SERVICE_ROLE_KEY}`,
    'Content-Type': 'application/json',
    ...extra,
  };
}

async function sbGet(env, path) {
  const resp = await fetch(`${env.SUPABASE_URL}/rest/v1/${path}`, {
    headers: sbHeaders(env),
  });
  if (!resp.ok) throw new Error(`GET ${path} -> HTTP ${resp.status} ${await resp.text()}`);
  return resp.json();
}

async function sbPost(env, path, body, extra = {}) {
  const resp = await fetch(`${env.SUPABASE_URL}/rest/v1/${path}`, {
    method: 'POST',
    headers: sbHeaders(env, extra),
    body: JSON.stringify(body),
  });
  if (!resp.ok) throw new Error(`POST ${path} -> HTTP ${resp.status} ${await resp.text()}`);
  return resp;
}

async function sbPostMany(env, path, rows, extra = {}) {
  if (!rows || rows.length === 0) return;
  await sbPost(env, path, rows, extra);
}

async function getSetting(env, key) {
  const rows = await sbGet(env, `app_settings?key=eq.${encodeURIComponent(key)}&select=value&limit=1`);
  if (Array.isArray(rows) && rows[0]) return rows[0].value;
  return null;
}

async function setSetting(env, key, value) {
  await sbPost(
    env,
    'app_settings?on_conflict=key',
    { key, value },
    { Prefer: 'resolution=merge-duplicates' }
  );
}

function normalizeConfig(raw) {
  const cfg = raw && typeof raw === 'object' ? raw : {};
  cfg.min_viewers = Number(cfg.min_viewers) || DEFAULT_CONFIG.min_viewers;
  cfg.target_buffer = Number(cfg.target_buffer) || DEFAULT_CONFIG.target_buffer;
  cfg.stale_minutes = Number(cfg.stale_minutes) || DEFAULT_CONFIG.stale_minutes;
  cfg.scan_interval_minutes =
    Number(cfg.scan_interval_minutes) >= 0
      ? Number(cfg.scan_interval_minutes)
      : DEFAULT_CONFIG.scan_interval_minutes;
  cfg.browser_daily_seconds =
    Number(cfg.browser_daily_seconds) > 0
      ? Number(cfg.browser_daily_seconds)
      : DEFAULT_CONFIG.browser_daily_seconds;
  cfg.browser_min_spacing_seconds =
    Number(cfg.browser_min_spacing_seconds) > 0
      ? Number(cfg.browser_min_spacing_seconds)
      : DEFAULT_CONFIG.browser_min_spacing_seconds;

  if (!Array.isArray(cfg.categories) || cfg.categories.length === 0) {
    cfg.categories = JSON.parse(JSON.stringify(DEFAULT_CONFIG.categories));
  } else {
    // If stripchat couple is missing from categories, add it automatically
    const hasStripchat = cfg.categories.some((c) => (c.site || '') === 'stripchat');
    if (!hasStripchat) {
      cfg.categories.push({
        key: 'sc_c',
        label: 'couple',
        site: 'stripchat',
        target_buffer: 2,
        min_viewers: 200,
      });
    }
  }

  for (const cat of cfg.categories) {
    cat.site = cat.site || 'chaturbate';
    cat.target_buffer = Number(cat.target_buffer) || cfg.target_buffer || 2;
    if (cat.site === 'stripchat') {
      cat.min_viewers = Number(cat.min_viewers) || 200;
    } else {
      cat.min_viewers = Number(cat.min_viewers) || cfg.min_viewers || 20000;
    }
  }

  return cfg;
}

async function loadConfig(env) {
  let cfg = null;
  try {
    cfg = await getSetting(env, CONFIG_KEY);
  } catch (e) {
    console.error('loadConfig failed, using defaults:', e.message);
  }

  const normalized = normalizeConfig(cfg);
  // If categories was upgraded (e.g. added stripchat), save back to app_settings
  if (!cfg || !cfg.categories || !cfg.categories.some((c) => c.site === 'stripchat')) {
    try {
      await setSetting(env, CONFIG_KEY, normalized);
    } catch (_) {}
  }
  return normalized;
}

// ---- Batched Supabase pool helpers ------------------------------------------
function inList(usernames) {
  return 'in.(' + usernames.map((u) => encodeURIComponent(u)).join(',') + ')';
}

async function batchInPool(env, items) {
  if (!items || items.length === 0) return new Set();
  const bySite = {};
  for (const it of items) {
    const s = it.site || 'chaturbate';
    bySite[s] = bySite[s] || [];
    bySite[s].push(it.username);
  }
  const poolSet = new Set();
  for (const [site, usernames] of Object.entries(bySite)) {
    try {
      const rows = await sbGet(
        env,
        `channel_assignments?username=${inList(usernames)}&site=eq.${site}&select=username,site`
      );
      for (const r of rows || []) {
        poolSet.add(`${r.site || 'chaturbate'}:${r.username.toLowerCase()}`);
      }
    } catch (e) {
      console.error(`batchInPool error for site ${site}:`, e.message);
    }
  }
  return poolSet;
}

async function batchAutoAdded(env, usernames) {
  if (usernames.length === 0) return new Set();
  const rows = await sbGet(env, `pool_autopilot?username=${inList(usernames)}&select=username`);
  return new Set((rows || []).map((r) => r.username.toLowerCase()));
}

async function batchInChannels(env, usernames) {
  if (usernames.length === 0) return new Set();
  const rows = await sbGet(env, `channels?username=${inList(usernames)}&select=username`);
  return new Set((rows || []).map((r) => r.username.toLowerCase()));
}

/**
 * Count unassigned channels in channel_assignments per site & category.
 * Chaturbate categories (female, couple) are resolved via pool_autopilot.
 * Stripchat couples are tagged site=stripchat.
 */
async function countAutoUnassigned(env) {
  const unassigned = await sbGet(
    env,
    `channel_assignments?status=eq.unassigned&select=username,site`
  );
  const autopilot = await sbGet(env, `pool_autopilot?select=username,gender`);
  const genderByUser = {};
  for (const a of autopilot || []) {
    if (a.username && a.gender) genderByUser[a.username.toLowerCase()] = a.gender;
  }

  const autoByCat = {};
  for (const row of unassigned || []) {
    const site = row.site || 'chaturbate';
    const u = (row.username || '').toLowerCase();
    let g = genderByUser[u];
    if (!g) {
      g = site === 'stripchat' ? 'couple' : 'unknown';
    } else if (g === 'stripchat_couple') {
      g = 'couple';
    }
    const catKey = `${site}:${g}`;
    autoByCat[catKey] = (autoByCat[catKey] || 0) + 1;
  }
  return autoByCat;
}

// ---- Lightweight logging (Cloudflare KV ring buffer) ------------------------
const CF_LOG_KEY = 'autopilot_log';
const CF_LOG_MAX = 200;

async function logMany(env, entries) {
  if (!entries || !entries.length) return;
  for (const e of entries) {
    const lvl = (e.log_level || 'info').toUpperCase();
    const who = e.username && e.username !== '__autopilot__' ? ` [${e.username}]` : '';
    console.log(`[autopilot:${lvl}]${who} ${e.message}`);
  }
  try {
    const prev = (await env.AUTOPILOT?.get(CF_LOG_KEY, { type: 'json' })) || [];
    const arr = Array.isArray(prev) ? prev : [];
    for (const e of entries) {
      arr.unshift({
        t: new Date().toISOString(),
        level: e.log_level || 'info',
        user: e.username || null,
        msg: e.message,
      });
    }
    while (arr.length > CF_LOG_MAX) arr.pop();
    await env.AUTOPILOT?.put(CF_LOG_KEY, JSON.stringify(arr));
  } catch (e) {
    console.error('cf log buffer write failed:', e.message);
  }
}

async function trimChannelLogs(env) {
  try {
    const prev = await env.AUTOPILOT?.get(CF_LOG_KEY, { type: 'json' });
    const arr = Array.isArray(prev) ? prev : [];
    if (arr.length > CF_LOG_MAX) {
      arr.length = CF_LOG_MAX;
      await env.AUTOPILOT?.put(CF_LOG_KEY, JSON.stringify(arr));
    }
  } catch (e) {
    console.error('trimChannelLogs failed:', e.message);
  }
}

// ---- Chaturbate fetch -------------------------------------------------------
function topRooms(data, minViewers) {
  const rooms = Array.isArray(data?.rooms) ? data.rooms : [];
  return rooms
    .filter((r) => (r.current_show || r.room_status) === 'public' && (r.num_users ?? 0) >= minViewers)
    .sort((a, b) => (b.num_users ?? 0) - (a.num_users ?? 0));
}

function roomListUrl(genderKey, env) {
  return (
    `${env.CB_DOMAIN}/api/ts/roomlist/room-list/` +
    `?enable_recommendations=false&genders=${genderKey}&limit=100`
  );
}

async function fetchRoomList(page, genderKey, env) {
  const apiUrl = roomListUrl(genderKey, env);
  return await page.evaluate(
    async ({ url, attempts, retryMs }) => {
      for (let attempt = 0; attempt < attempts; attempt++) {
        try {
          const r = await fetch(url, {
            headers: {
              Accept: 'application/json, text/javascript, */*; q=0.01',
              'X-Requested-With': 'XMLHttpRequest',
            },
          });
          const text = await r.text();
          if (r.ok && text.trim().startsWith('{')) return JSON.parse(text);
        } catch (_) {}
        await new Promise((res) => setTimeout(res, retryMs));
      }
      throw new Error('challenge not cleared / no JSON after retries');
    },
    { url: apiUrl, attempts: ROOMLIST_MAX_ATTEMPTS, retryMs: ROOMLIST_RETRY_MS }
  );
}

async function fetchRoomListDirect(env, genderKey, cached) {
  try {
    const cookieHeader = (cached?.cookies || [])
      .filter((c) => (c.domain || '').includes('chaturbate'))
      .map((c) => `${c.name}=${c.value}`)
      .join('; ');
    const r = await fetch(roomListUrl(genderKey, env), {
      headers: {
        Accept: 'application/json, text/javascript, */*; q=0.01',
        'X-Requested-With': 'XMLHttpRequest',
        'User-Agent': env.CB_USER_AGENT || DEFAULT_UA,
        ...(cookieHeader ? { Cookie: cookieHeader } : {}),
      },
    });
    if (!r.ok) return null;
    const text = await r.text();
    if (!text.trim().startsWith('{')) return null;
    const data = JSON.parse(text);
    return Array.isArray(data?.rooms) ? data : null;
  } catch (_) {
    return null;
  }
}

// ---- Stripchat fetch --------------------------------------------------------
/**
 * Fetches Stripchat couple rooms directly via its front models API.
 * Focuses on top viewers in the couples category with sortBy=stripRanking.
 * Costs 0 browser seconds when direct fetch succeeds.
 */
async function fetchStripchatCouplesDirect(env, minViewers = 200) {
  try {
    const url = 'https://stripchat.com/api/front/models?limit=60&offset=0&primaryTag=couples&sortBy=stripRanking';
    const r = await fetch(url, {
      headers: {
        Accept: 'application/json, text/plain, */*',
        'User-Agent': env.CB_USER_AGENT || DEFAULT_UA,
        Referer: 'https://stripchat.com/couples',
      },
    });
    if (!r.ok) {
      console.warn(`[stripchat] HTTP ${r.status} on direct fetch`);
      return null;
    }
    const data = await r.json();
    const models = Array.isArray(data?.models) ? data.models : [];
    const qualified = models
      .filter((m) => m.isLive === true && m.status === 'public' && (m.viewersCount || 0) >= minViewers)
      .sort((a, b) => (b.viewersCount || 0) - (a.viewersCount || 0))
      .map((m) => ({
        username: m.username,
        num_users: m.viewersCount || 0,
        site: 'stripchat',
        category: 'couple',
      }));
    return qualified;
  } catch (err) {
    console.error('[stripchat] direct fetch error:', err.message);
    return null;
  }
}

/** Fallback to browser execution if Stripchat challenges direct fetch */
async function fetchStripchatCouplesBrowser(page, minViewers = 200) {
  const apiUrl = 'https://stripchat.com/api/front/models?limit=60&offset=0&primaryTag=couples&sortBy=stripRanking';
  return await page.evaluate(
    async ({ url, minViewers }) => {
      const r = await fetch(url, {
        headers: {
          Accept: 'application/json, text/plain, */*',
          Referer: 'https://stripchat.com/couples',
        },
      });
      const data = await r.json();
      const models = Array.isArray(data?.models) ? data.models : [];
      return models
        .filter((m) => m.isLive === true && m.status === 'public' && (m.viewersCount || 0) >= minViewers)
        .sort((a, b) => (b.viewersCount || 0) - (a.viewersCount || 0))
        .map((m) => ({
          username: m.username,
          num_users: m.viewersCount || 0,
          site: 'stripchat',
          category: 'couple',
        }));
    },
    { url: apiUrl, minViewers }
  );
}

// ---- KV session cache -------------------------------------------------------
async function loadCachedSession(env) {
  try {
    if (!env.AUTOPILOT) return null;
    return await env.AUTOPILOT.get('cb_session', { type: 'json' });
  } catch (_) {
    return null;
  }
}

async function saveCachedSession(env, cookies) {
  try {
    if (!env.AUTOPILOT || !cookies) return;
    await env.AUTOPILOT.put('cb_session', JSON.stringify({ cookies, saved_at: Date.now() }), {
      expirationTtl: SESSION_TTL,
    });
  } catch (e) {
    console.error('saveCachedSession failed:', e.message);
  }
}

// ---- Rate-limit backoff (429) -----------------------------------------------
async function isRateLimited(env) {
  try {
    if (!env.AUTOPILOT) return false;
    const until = await env.AUTOPILOT.get(RATE_LIMIT_KEY);
    if (!until) return false;
    return Date.now() < Number(until);
  } catch (_) {
    return false;
  }
}

function nextUtcMidnightMs(nowMs = Date.now()) {
  const d = new Date(nowMs);
  return Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate() + 1, 0, 0, 0);
}

function retryAfterSeconds(e) {
  try {
    const h = e && e.headers;
    if (h && typeof h.get === 'function') {
      const v = Number(h.get('retry-after') ?? h.get('Retry-After'));
      if (v > 0) return v;
    }
    for (const k of ['retryAfter', 'retry_after']) {
      if (e && Number(e[k]) > 0) return Number(e[k]);
    }
    const m = String((e && e.message) || '').match(/retry[- ]?after\D{0,10}(\d+)/i);
    if (m) return Number(m[1]);
  } catch (_) {}
  return 0;
}

async function setRateLimited(env, { dailyLimit = false, retryAfterSec = 0 } = {}) {
  try {
    if (!env.AUTOPILOT) return;
    const now = Date.now();
    let untilMs;
    let streak = 0;

    if (dailyLimit) {
      untilMs = nextUtcMidnightMs(now);
    } else {
      try {
        streak = Number(await env.AUTOPILOT.get(RATE_LIMIT_STREAK_KEY)) || 0;
      } catch (_) {}
      streak += 1;
      const backoffMs = Math.min(RL_MAX_MS, RL_BASE_MS * 2 ** (streak - 1));
      const retryMs = retryAfterSec > 0 ? retryAfterSec * 1000 : 0;
      untilMs = now + Math.max(backoffMs, retryMs);
      try {
        await env.AUTOPILOT.put(RATE_LIMIT_STREAK_KEY, String(streak), { expirationTtl: 86400 });
      } catch (_) {}
    }

    const ttlSeconds = Math.max(60, Math.ceil((untilMs - now) / 1000));
    await env.AUTOPILOT.put(RATE_LIMIT_KEY, String(untilMs), { expirationTtl: ttlSeconds });
    console.log(
      `rate-limit backoff set until ${new Date(untilMs).toISOString()} ` +
        `(ttl=${ttlSeconds}s, daily=${dailyLimit}, streak=${streak}, retryAfter=${retryAfterSec}s)`
    );
  } catch (e) {
    console.error('setRateLimited failed:', e.message);
  }
}

async function backoffFromLaunchError(env, e) {
  const msg = String((e && e.message) || '').toLowerCase();
  const dailyLimit = msg.includes('time limit exceeded');
  const transient = msg.includes('429') || msg.includes('rate limit') || msg.includes('too many');
  const retryAfterSec = retryAfterSeconds(e);
  if (!dailyLimit && !transient && !retryAfterSec) return;
  await setRateLimited(env, { dailyLimit, retryAfterSec });
}

async function clearRateLimited(env) {
  try {
    if (!env.AUTOPILOT) return;
    await env.AUTOPILOT.delete(RATE_LIMIT_KEY);
    await env.AUTOPILOT.delete(RATE_LIMIT_STREAK_KEY);
    console.log('rate-limit backoff cleared (browser launch succeeded)');
  } catch (_) {}
}

// ---- Single-flight scan lock -------------------------------------------------
async function acquireScanLock(env) {
  try {
    if (!env.AUTOPILOT) return true;
    if (await env.AUTOPILOT.get(SCAN_LOCK_KEY)) return false;
    await env.AUTOPILOT.put(SCAN_LOCK_KEY, String(Date.now()), {
      expirationTtl: SCAN_LOCK_TTL_SEC,
    });
    return true;
  } catch (_) {
    return true;
  }
}

async function releaseScanLock(env) {
  try {
    await env.AUTOPILOT?.delete(SCAN_LOCK_KEY);
  } catch (_) {}
}

// ---- Scan cadence (45 min) ---------------------------------------------------
async function getLastScanAt(env) {
  try {
    if (!env.AUTOPILOT) return 0;
    const v = await env.AUTOPILOT.get(SCAN_INTERVAL_KEY);
    return v ? Number(v) || 0 : 0;
  } catch (_) {
    return 0;
  }
}

async function markScanStarted(env) {
  try {
    if (!env.AUTOPILOT) return;
    await env.AUTOPILOT.put(SCAN_INTERVAL_KEY, String(Date.now()), {
      expirationTtl: 86400,
    });
  } catch (e) {
    console.error('markScanStarted failed:', e.message);
  }
}

// ---- Browser budget accountant ----------------------------------------------
function todayUtcKey() {
  const d = new Date();
  return `${d.getUTCFullYear()}-${String(d.getUTCMonth() + 1).padStart(2, '0')}-${String(
    d.getUTCDate()
  ).padStart(2, '0')}`;
}

async function getBrowserSecondsUsed(env) {
  try {
    if (!env.AUTOPILOT) return 0;
    const today = todayUtcKey();
    const recordedDay = await env.AUTOPILOT.get(BROWSER_BUDGET_DAY_KEY);
    if (recordedDay !== today) return 0;
    const v = await env.AUTOPILOT.get(BROWSER_USED_KEY);
    return v ? Number(v) || 0 : 0;
  } catch (_) {
    return 0;
  }
}

async function addBrowserSeconds(env, seconds) {
  if (seconds <= 0) return;
  try {
    if (!env.AUTOPILOT) return;
    const today = todayUtcKey();
    const recordedDay = await env.AUTOPILOT.get(BROWSER_BUDGET_DAY_KEY);
    let total = seconds;
    if (recordedDay === today) {
      const v = await env.AUTOPILOT.get(BROWSER_USED_KEY);
      total += v ? Number(v) || 0 : 0;
    }
    await env.AUTOPILOT.put(BROWSER_BUDGET_DAY_KEY, today, { expirationTtl: 172800 });
    await env.AUTOPILOT.put(BROWSER_USED_KEY, String(total), { expirationTtl: 172800 });
    console.log(`browser time charged: +${seconds.toFixed(1)}s (today: ${total.toFixed(1)}s)`);
  } catch (e) {
    console.error('addBrowserSeconds failed:', e.message);
  }
}

async function getLastLaunchAt(env) {
  try {
    if (!env.AUTOPILOT) return 0;
    const v = await env.AUTOPILOT.get(BROWSER_LAST_LAUNCH_KEY);
    return v ? Number(v) || 0 : 0;
  } catch (_) {
    return 0;
  }
}

async function markBrowserLaunched(env) {
  try {
    if (!env.AUTOPILOT) return;
    await env.AUTOPILOT.put(BROWSER_LAST_LAUNCH_KEY, String(Date.now()), { expirationTtl: 86400 });
  } catch (e) {
    console.error('markBrowserLaunched failed:', e.message);
  }
}

// ---- Heartbeat & alerts -----------------------------------------------------
async function writeHeartbeat(env, method) {
  try {
    await setSetting(env, HEARTBEAT_KEY, {
      last_success_at: new Date().toISOString(),
      method,
    });
  } catch (e) {
    console.error('writeHeartbeat failed:', e.message);
  }
}

async function checkStale(env, cfg) {
  try {
    const hb = await getSetting(env, HEARTBEAT_KEY);
    if (!hb || !hb.last_success_at) return;
    const ageMinutes = (Date.now() - new Date(hb.last_success_at).getTime()) / 60000;
    if (ageMinutes <= cfg.stale_minutes) return;

    if (env.AUTOPILOT) {
      const lastAlert = await env.AUTOPILOT.get(STALE_ALERT_KEY);
      if (lastAlert && Date.now() - Number(lastAlert) < STALE_ALERT_INTERVAL_MS) return;
      await env.AUTOPILOT.put(STALE_ALERT_KEY, String(Date.now()), {
        expirationTtl: Math.ceil(STALE_ALERT_INTERVAL_MS / 1000),
      });
    }

    const msg =
      `autopilot stale: last successful scan was ${Math.round(ageMinutes)}m ago ` +
      `(threshold ${cfg.stale_minutes}m, last method: ${hb.method || 'unknown'})`;
    console.error(msg);
    await logMany(env, [{ username: '__autopilot__', log_level: 'error', message: msg }]);
    await alert(env, msg);
  } catch (e) {
    console.error('checkStale failed:', e.message);
  }
}

async function recordScan(env, scanRow) {
  try {
    const prev = (await getSetting(env, SCANS_KEY)) || [];
    const arr = Array.isArray(prev) ? prev : [];
    arr.unshift(scanRow);
    if (arr.length > 50) arr.length = 50;
    await setSetting(env, SCANS_KEY, arr);
  } catch (e) {
    console.error('recordScan failed:', e.message);
  }
}

async function alert(env, message) {
  if (!env.CB_ALERT_WEBHOOK) return;
  try {
    await fetch(env.CB_ALERT_WEBHOOK, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ text: message, level: 'error', source: 'chaturbate-autopilot' }),
    });
  } catch (e) {
    console.error('alert webhook failed:', e.message);
  }
}

async function successAlert(env, added) {
  if (!env.CB_SUCCESS_WEBHOOK || !added || added.length === 0) return;
  try {
    await fetch(env.CB_SUCCESS_WEBHOOK, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ text: 'autopilot added channels', added, source: 'chaturbate-autopilot' }),
    });
  } catch (e) {
    console.error('success webhook failed:', e.message);
  }
}

// ---- Main scan loop ---------------------------------------------------------
async function runScan(env, dryrun = false, trigger = 'cron', force = false) {
  const cfg = await loadConfig(env);
  const result = {
    now: new Date().toISOString(),
    trigger,
    dryrun: !!dryrun,
    config: {
      min_viewers: cfg.min_viewers,
      target_buffer: cfg.target_buffer,
      scan_interval_minutes: cfg.scan_interval_minutes,
      categories: cfg.categories,
    },
    method: 'direct',
    female: [],
    couple: [],
    stripchat_couple: [],
    added: [],
    skipped: [],
    errors: [],
  };

  const startedAt = Date.now();
  let browser = null;
  let launchedAt = 0;

  if (force) {
    await clearRateLimited(env);
    await releaseScanLock(env);
  }

  // ---- Cadence gate ---------------------------------------------------------
  if (!dryrun && !force && String(trigger).startsWith('cron:')) {
    const intervalMs = (Number(cfg.scan_interval_minutes) || 0) * 60000;
    const lastScanAt = await getLastScanAt(env);
    if (intervalMs > 0 && lastScanAt && Date.now() - lastScanAt < intervalMs) {
      result.method = 'throttled';
      result.next_scan_in_s = Math.round((lastScanAt + intervalMs - Date.now()) / 1000);
      console.log(`autopilot: throttled — next scan in ${result.next_scan_in_s}s`);
      result.duration_ms = Date.now() - startedAt;
      return result;
    }
  }

  // Single-flight lock
  const locked = dryrun || (await acquireScanLock(env));
  if (!locked) {
    result.method = 'skipped';
    result.errors.push('another scan is already in flight — skipping');
    result.duration_ms = Date.now() - startedAt;
    return result;
  }

  try {
    await markScanStarted(env);
    await checkStale(env, cfg);

    const autoByCat = await countAutoUnassigned(env).catch((e) => {
      result.errors.push(`countAutoUnassigned failed: ${e.message}`);
      return {};
    });

    // Calculate deficits per category
    const deficits = {};
    let needScan = false;
    for (const cat of cfg.categories) {
      const site = cat.site || 'chaturbate';
      const catKey = `${site}:${cat.label}`;
      const current = autoByCat[catKey] || 0;
      const d = Math.max(0, cat.target_buffer - current);
      deficits[catKey] = d;
      if (d > 0) needScan = true;
    }

    const bufferSummary = cfg.categories
      .map((cat) => `${cat.site || 'chaturbate'}:${cat.label}=${autoByCat[`${cat.site || 'chaturbate'}:${cat.label}`] || 0}/${cat.target_buffer}`)
      .join(', ');
    console.log(`autopilot: buffer status (${bufferSummary})`);

    const candidates = [];
    let page = null;
    const cached = await loadCachedSession(env);
    const topsByCategory = {};

    if (!force && (await isRateLimited(env))) {
      result.method = 'rate_limited';
      const msg = 'rate limited — skipping scan until backoff expires';
      result.errors.push(msg);
      await logMany(env, [{ username: '__autopilot__', log_level: 'warn', message: `autopilot: ${msg}` }]);
    } else if (!needScan) {
      result.method = 'buffer_full';
      console.log(`autopilot: all buffers full (${bufferSummary}) — no roomlist fetch needed`);
    } else {
      const wanted = cfg.categories.filter((cat) => {
        const catKey = `${cat.site || 'chaturbate'}:${cat.label}`;
        return (deficits[catKey] || 0) > 0;
      });

      const cbCategories = wanted.filter((c) => (c.site || 'chaturbate') === 'chaturbate');
      const scCategories = wanted.filter((c) => c.site === 'stripchat');
      const needingBrowser = [];

      // 1. Fetch Stripchat categories directly (free, 0 browser seconds)
      for (const cat of scCategories) {
        const catKey = `stripchat:${cat.label}`;
        const minV = cat.min_viewers || 200;
        const scRooms = await fetchStripchatCouplesDirect(env, minV);
        if (scRooms && scRooms.length > 0) {
          topsByCategory[catKey] = scRooms;
          console.log(`autopilot: Stripchat couples fetched directly (${scRooms.length} qualified >= ${minV} viewers)`);
        } else {
          needingBrowser.push(cat);
        }
      }

      // 2. Fetch Chaturbate categories directly using cached cookies if present
      if (cached?.cookies?.length) {
        for (const cat of cbCategories) {
          const catKey = `chaturbate:${cat.label}`;
          const data = await fetchRoomListDirect(env, cat.key, cached);
          if (data) {
            topsByCategory[catKey] = topRooms(data, cat.min_viewers || cfg.min_viewers);
          } else {
            needingBrowser.push(cat);
          }
        }
        if (Object.keys(topsByCategory).length > 0) result.method = 'direct';
      } else {
        needingBrowser.push(...cbCategories);
      }

      // 3. Fallback to browser ONLY for categories that could not be fetched directly
      if (needingBrowser.length === 0) {
        console.log('autopilot: all roomlists served directly without browser');
      } else if (!env.BROWSER) {
        result.errors.push('BROWSER binding not configured for browser-needing categories');
      } else {
        const usedSeconds = await getBrowserSecondsUsed(env);
        const budgetLeft = cfg.browser_daily_seconds - usedSeconds;
        const sinceLastLaunchMs = Date.now() - (await getLastLaunchAt(env));
        const minSpacingMs = cfg.browser_min_spacing_seconds * 1000;
        result.browser_seconds_used_today = Math.round(usedSeconds * 10) / 10;

        if (budgetLeft < BROWSER_LAUNCH_ESTIMATE_SEC) {
          result.method = 'budget_exhausted';
          const msg =
            `daily browser budget spent (${usedSeconds.toFixed(1)}s of ` +
            `${cfg.browser_daily_seconds}s) — pausing browser scans until next UTC day`;
          result.errors.push(msg);
          await logMany(env, [{ username: '__autopilot__', log_level: 'warn', message: `autopilot: ${msg}` }]);
          await setRateLimited(env, { dailyLimit: true });
        } else if (sinceLastLaunchMs < minSpacingMs) {
          result.method = 'instance_spacing';
          const waitS = Math.ceil((minSpacingMs - sinceLastLaunchMs) / 1000);
          const msg = `browser launched ${Math.round(
            sinceLastLaunchMs / 1000
          )}s ago — deferring ${waitS}s to respect instance rate limit`;
          result.errors.push(msg);
          await logMany(env, [{ username: '__autopilot__', log_level: 'warn', message: `autopilot: ${msg}` }]);
        } else {
          await markBrowserLaunched(env);
          launchedAt = Date.now();
          try {
            browser = await puppeteer.launch(env.BROWSER);
            await clearRateLimited(env);
          } catch (e) {
            const msg = `browser launch failed: ${e.message}`;
            result.errors.push(msg);
            await logMany(env, [{ username: '__autopilot__', log_level: 'error', message: msg }]);
            await backoffFromLaunchError(env, e);
          }
        }

        if (browser) {
          result.method = result.method === 'direct' ? 'direct+browser' : 'browser';
          page = await browser.newPage();
          await page.setUserAgent(env.CB_USER_AGENT || DEFAULT_UA);

          await page.setRequestInterception(true);
          page.on('request', (req) => {
            const t = req.resourceType();
            if (t === 'document' || t === 'script' || t === 'xhr' || t === 'fetch') {
              req.continue();
            } else {
              req.abort().catch(() => {});
            }
          });

          if (cached?.cookies?.length) {
            try {
              await page.setCookie(...cached.cookies);
            } catch (_) {}
          }

          // If any Chaturbate categories need browser, load Chaturbate homepage to resolve challenge
          const needsCb = needingBrowser.some((c) => (c.site || 'chaturbate') === 'chaturbate');
          if (needsCb) {
            await page
              .goto(env.CB_DOMAIN + '/', { waitUntil: 'domcontentloaded', timeout: 20000 })
              .catch(() => {});
          }

          for (const cat of needingBrowser) {
            const site = cat.site || 'chaturbate';
            const catKey = `${site}:${cat.label}`;
            try {
              if (site === 'stripchat') {
                const scRooms = await fetchStripchatCouplesBrowser(page, cat.min_viewers || 200);
                topsByCategory[catKey] = scRooms;
              } else {
                const data = await fetchRoomList(page, cat.key, env);
                topsByCategory[catKey] = topRooms(data, cat.min_viewers || cfg.min_viewers);
              }
            } catch (e) {
              const msg = `browser scan failed for [${site}] ${cat.label}: ${e.message}`;
              result.errors.push(msg);
              await logMany(env, [{ username: '__autopilot__', log_level: 'error', message: msg }]);
              await alert(env, msg);
            }
          }
        }
      }
    }

    // ---- Phase 3: Build candidates from topsByCategory -----------------------
    for (const cat of cfg.categories) {
      const site = cat.site || 'chaturbate';
      const catKey = `${site}:${cat.label}`;
      const top = topsByCategory[catKey];
      if (!top || !Array.isArray(top)) continue;

      const deficit = deficits[catKey] ?? 0;
      const capped = top.slice(0, deficit);
      const resKey = site === 'stripchat' ? 'stripchat_couple' : cat.label;
      result[resKey] = capped.map((r) => ({ username: r.username, num_users: r.num_users, site }));
      for (const r of capped) {
        candidates.push({
          category: cat.label,
          site,
          username: r.username,
          num_users: r.num_users,
        });
      }
    }

    // ---- Phase 4: Dedup against pool & catalogue, then insert ----------------
    const usernames = candidates.map((c) => c.username);
    const [inPoolSet, autoAddedSet, channelsSet] = await Promise.all([
      batchInPool(env, candidates),
      batchAutoAdded(env, usernames),
      batchInChannels(env, usernames),
    ]).catch((e) => {
      result.errors.push(`batch dedup failed: ${e.message}`);
      return [new Set(), new Set(), new Set()];
    });

    const addRows = [];
    const channelRows = [];
    const autoRows = [];
    const logRows = [];

    for (const c of candidates) {
      const poolKey = `${c.site || 'chaturbate'}:${c.username.toLowerCase()}`;
      const uLower = c.username.toLowerCase();
      let reason = null;

      if (inPoolSet.has(poolKey)) {
        reason = 'already in pool / claimed by a node';
      } else if (autoAddedSet.has(uLower) && channelsSet.has(uLower)) {
        reason = 'previously auto-added then removed (blocklist)';
      }

      if (reason) {
        result.skipped.push({ site: c.site, category: c.category, username: c.username, reason });
        continue;
      }

      if (dryrun) {
        result.added.push({ site: c.site, category: c.category, username: c.username, num_users: c.num_users, dryrun: true });
        continue;
      }

      addRows.push({
        username: c.username,
        site: c.site || 'chaturbate',
        status: 'unassigned',
        resolution: 2160,
        framerate: 60,
      });

      channelRows.push({
        username: c.username,
        created_at: Math.floor(Date.now() / 1000),
      });

      autoRows.push({
        username: c.username,
        gender: c.site === 'stripchat' ? 'stripchat_couple' : c.category,
        viewers: c.num_users,
        updated_at: new Date().toISOString(),
      });

      logRows.push({
        username: c.username,
        log_level: 'info',
        message: `autopilot: added [${c.site}] ${c.category} ${c.username} (${c.num_users} viewers)`,
      });

      result.added.push({ site: c.site, category: c.category, username: c.username, num_users: c.num_users });
    }

    if (!dryrun && addRows.length > 0) {
      try {
        await sbPostMany(
          env,
          'channel_assignments?on_conflict=username,site',
          addRows,
          { Prefer: 'resolution=ignore-duplicates' }
        );
        await sbPostMany(
          env,
          'pool_autopilot?on_conflict=username',
          autoRows,
          { Prefer: 'resolution=merge-duplicates' }
        );
        try {
          await sbPostMany(
            env,
            'channels?on_conflict=username',
            channelRows,
            { Prefer: 'resolution=ignore-duplicates' }
          );
        } catch (e) {
          result.errors.push(`catalogue register failed: ${e.message}`);
        }
        await logMany(env, logRows);
      } catch (e) {
        result.errors.push(`batch write failed: ${e.message}`);
      }
    }

    // Cache fresh Chaturbate cookies from browser run
    if (page) {
      try {
        const cookies = await page.cookies();
        const cb = cookies.filter((c) => (c.domain || '').includes('chaturbate'));
        if (cb.length) await saveCachedSession(env, cb);
      } catch (e) {
        console.error('cookie extract failed:', e.message);
      }
    }

    const success = result.errors.length === 0;
    if (success && !dryrun) {
      await writeHeartbeat(env, result.method);
      await successAlert(env, result.added);
    }
  } finally {
    if (browser) {
      try {
        await browser.close();
      } catch (_) {}
      const usedSec = launchedAt ? (Date.now() - launchedAt) / 1000 : 0;
      if (usedSec > 0 && !dryrun) {
        await addBrowserSeconds(env, usedSec);
        result.browser_seconds = Math.round(usedSec * 10) / 10;
      }
    }
    if (!dryrun) await releaseScanLock(env);
  }

  result.duration_ms = Date.now() - startedAt;
  await recordScan(env, {
    t: result.now,
    trigger,
    method: result.method,
    duration_ms: result.duration_ms,
    added: result.added.length,
    skipped: result.skipped.length,
    errors: result.errors,
  });
  await trimChannelLogs(env);
  return result;
}

// ---- Health check -----------------------------------------------------------
async function healthCheck(env) {
  const cfg = await loadConfig(env).catch(() => DEFAULT_CONFIG);
  await checkStale(env, cfg);
}

// ---- Worker handlers --------------------------------------------------------
export default {
  async scheduled(event, env, ctx) {
    if (event.cron === '*/20 * * * *') {
      ctx.waitUntil(healthCheck(env).catch((e) => console.error('healthCheck error:', e)));
      return;
    }
    const trigger = `cron:${event.cron}`;
    ctx.waitUntil(
      runScan(env, false, trigger)
        .then((r) => console.log('autopilot scan complete', JSON.stringify(r)))
        .catch((e) => console.error('cron scan error:', e))
    );
  },

  async fetch(request, env, _ctx) {
    const url = new URL(request.url);
    const dryrun =
      url.searchParams.get('dryrun') === '1' || url.searchParams.get('dryrun') === 'true';
    const force = url.searchParams.get('force') === '1';

    if (url.pathname === '/health') {
      const hb = await getSetting(env, HEARTBEAT_KEY).catch(() => null);
      const ageMin = hb?.last_success_at
        ? Math.round((Date.now() - new Date(hb.last_success_at).getTime()) / 60000)
        : null;
      const cfg = await loadConfig(env).catch(() => DEFAULT_CONFIG);
      const usedSeconds = await getBrowserSecondsUsed(env);
      const rateLimitedUntil = env.AUTOPILOT
        ? Number(await env.AUTOPILOT.get(RATE_LIMIT_KEY).catch(() => 0)) || 0
        : 0;
      const lastScanAt = await getLastScanAt(env);
      return jsonResponse({
        ok: !!hb,
        last_success_at: hb?.last_success_at || null,
        age_min: ageMin,
        method: hb?.method || null,
        stale_minutes: cfg.stale_minutes,
        scan_interval_minutes: cfg.scan_interval_minutes,
        categories: cfg.categories,
        next_scan_due_in_s: Math.max(
          0,
          Math.round((lastScanAt + cfg.scan_interval_minutes * 60000 - Date.now()) / 1000)
        ),
        browser_budget: {
          used_seconds_today: Math.round(usedSeconds * 10) / 10,
          limit_seconds: cfg.browser_daily_seconds,
          remaining_seconds: Math.max(0, Math.round((cfg.browser_daily_seconds - usedSeconds) * 10) / 10),
        },
        rate_limited_until:
          rateLimitedUntil > Date.now() ? new Date(rateLimitedUntil).toISOString() : null,
      });
    }

    const wantsScan =
      request.method !== 'GET' ||
      url.searchParams.get('scan') === '1' ||
      url.searchParams.get('run') === '1' ||
      force ||
      dryrun;

    if (!wantsScan) {
      const hb = await getSetting(env, HEARTBEAT_KEY).catch(() => null);
      return jsonResponse({
        ok: true,
        message:
          'chaturbate & stripchat autopilot is up. Scans run on cron; trigger manually with ' +
          'POST / (or GET /?scan=1, add &dryrun=1 to preview without writing).',
        last_success_at: hb?.last_success_at || null,
      });
    }

    try {
      const trigger = `http:${request.method} ${url.pathname}${url.search}`;
      const result = await runScan(env, dryrun, trigger, force);
      return jsonResponse(result);
    } catch (e) {
      console.error('runScan error:', e);
      return jsonResponse({ error: e.message }, 500);
    }
  },
};
