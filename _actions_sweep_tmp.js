// Deep GitHub Actions sweep across the 18 node repos of the fleet.
// Uses authenticated `gh` CLI. No secrets printed.
// Output: _actions_sweep.json + readable stdout.
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { writeFileSync } from 'node:fs';
const exec = promisify(execFile);

const ORG = 'lawdachuss';
const REPOS = Array.from({ length: 18 }, (_, i) => `node-${i + 1}`);
const START = '2026-10-14T00:00:00Z';

async function gh(args, opts = {}) {
  const { stdout } = await exec('gh', args, { maxBuffer: 512 * 1024 * 1024, timeout: opts.timeout ?? 120000 });
  return stdout;
}
const ghJson = async (args) => JSON.parse(await gh(args));

let out = [];
const log = (s = '') => { console.log(s); out.push(s); };

log(`ACTIONS DEEP SWEEP ${new Date().toISOString()}`);
log('='.repeat(78));

// ── 1. Runs inventory per repo ──────────────────────────────────────────────
const store = {};
for (const repo of REPOS) {
  try {
    const runs = await ghJson(['api', `repos/${ORG}/${repo}/actions/runs?per_page=20`, '--jq',
      '[.workflow_runs[] | {id,name,event,status,conclusion,created_at,attempt}]']);
    const recent = runs.filter(r => r.created_at >= START);
    store[repo] = { runs: recent };
    const byC = {};
    for (const r of recent) byC[r.conclusion || r.status] = (byC[r.conclusion || r.status] || 0) + 1;
    const names = [...new Set(recent.map(r => r.name))];
    log(`${repo.padEnd(9)} last48h runs=${recent.length} [${names.join('|') || '-'}] ` +
      Object.entries(byC).map(([k, v]) => `${k}=${v}`).join(' '));
  } catch (e) {
    store[repo] = { error: e.message.slice(0, 150) };
    log(`${repo.padEnd(9)} ERROR ${e.message.slice(0, 120)}`);
  }
}

// ── 2. Failure analysis: failed + cancelled-anywhere runs whose logs matter ──
// note: 'cancelled' is the designed self-cancel of schedule-vs-session, likely benign;
// but we still open failure logs for conclusion=failure runs and last-success-green checks.
log('\n=== FAILING RUNS: failed jobs + first error line ===');
const failuresDone = new Set();
for (const repo of REPOS) {
  const st = store[repo];
  if (!st.runs) continue;
  for (const r of st.runs.filter(x => x.conclusion === 'failure')) {
    if (failuresDone.has(`${repo}:${r.id}`)) continue;
    failuresDone.add(`${repo}:${r.id}`);
    try {
      const jobs = await ghJson(['api', `repos/${ORG}/${repo}/actions/runs/${r.id}/jobs?per_page=30`, '--jq',
        `[.jobs[] | {id,name,conclusion,started_at,completed_at,steps:(.steps|map({name,conclusion}))}]`]);
      const bad = jobs.filter(j => j.conclusion === 'failure');
      for (const j of bad) {
        const failedSteps = (j.steps || []).filter(s => s.conclusion === 'failure').map(s => s.name).join(', ');
        log(`${repo} run ${r.id} (${r.created_at.slice(0, 16)}, ${r.event}) job "${j.name}" FAILED steps: ${failedSteps || '(none reported)'}`);
        // first error lines from the failed-job log (gh view --log-failed keeps only failed steps)
        try {
          const tail = await gh(['run', 'view', String(r.id), '--repo', `${ORG}/${repo}`, '--log-failed']);
          const lines = tail.split(/\r?\n/).filter(Boolean);
          const errLine = lines.find(l => /error|fail|exception|cannot|unable|not found|denied/i.test(l)) || lines[lines.length - 1] || '';
          log(`    first-err: ${errLine.slice(0, 220)}`);
          log(`    log lines: ${lines.length}`);
          const last5 = lines.slice(-5).map(l => '      | ' + l.slice(0, 200));
          log(last5.join('\n'));
        } catch (e) { log(`    log fetch failed: ${e.message.slice(0, 100)}`); }
      }
      // cancelled duplicate jobs in a failed run
      const other = jobs.filter(j => j.conclusion !== 'failure');
      if (other.length) log(`  (other jobs in run: ${other.map(j => `${j.name}=${j.conclusion}`).join(', ')})`);
    } catch (e) { log(`${repo} run ${r.id} jobs fetch failed: ${e.message.slice(0, 100)}`); }
  }
}

// ── 3. Green-but-doing-nothing check on last SUCCESS run per repo ───────────
log('\n=== LAST SUCCESS per repo: verify the session actually recorded ===');
for (const repo of REPOS) {
  const st = store[repo];
  if (!st.runs) continue;
  // pick last success, prefer entries with keep-alive session
  const ok = st.runs.filter(r => r.conclusion === 'success' && r.name === 'Secure RDP').sort((a, b) => b.created_at.localeCompare(a.created_at))[0];
  if (!ok) { log(`${repo} no successful Secure RDP run in window`); continue; }
  store[repo].lastSuccess = ok;
  try {
    // get keep-alive job id
    const jobs = await ghJson(['api', `repos/${ORG}/${repo}/actions/runs/${ok.id}/jobs?per_page=10`, '--jq',
      '[.jobs[] | {id,name,conclusion}]']);
    const ja = jobs.find(j => /keep|alive/i.test(j.name)) || jobs[0];
    const logs = await gh(['run', 'view', String(ok.id), '--repo', `${ORG}/${repo}`, '--job', String(ja.id), '--log'], { timeout: 240000 });
    const lines = logs.split(/\r?\n/);
    const markers = {
      tunnel: lines.filter(l => /trycloudflare\.com/.test(l)).length,
      tunnelLine: (lines.find(l => /trycloudflare\.com/.test(l)) || '').trim().slice(0, 140),
      channels: lines.filter(l => /\brecording\b/i.test(l) && nep(l)).length,
      uploaded: lines.filter(l => /uploaded|upload complete|persisting partial/i.test(l)).length,
      zeroByte: lines.filter(l => /0-byte|zero-byte|0 B\b/i.test(l)).length,
      apiErr: lines.filter(l => /ERROR/i.test(l)).length,
      cancelledDup: lines.filter(l => /Cancelling queued run|cancel .*run/i.test(l)).length,
      lines: lines.length,
    };
    function nep(l) { return !/api\/logs|log buffer/i.test(l); }
    log(`${repo} success-run ${ok.created_at.slice(0, 16)} job=${ja.name}:`);
    log(`    tunnelURL: ${markers.tunnelLine || '(none found!)'}`);
    log(`    recordingLines=${markers.channels} uploadedLines=${markers.uploaded} ERRORLines=${markers.apiErr} zeroByteLines=${markers.zeroByte} cancelledDup=${markers.cancelledDup} (total log lines ${markers.lines})`);
    // show key session lifecycle lines
    const life = lines.filter(l => /session|rebalanc|stop.*upload|upload-complete|deadline/i.test(l)).slice(-4);
    for (const l of life) log(`      | ${l.trim().slice(0, 190)}`);
  } catch (e) { log(`${repo} success-log check failed: ${e.message.slice(0, 120)}`); }
}

// ── 4. Currently in_progress / queued runs + retry-loop detection ───────────
log('\n=== 4. LIVE state + retry loops ===');
const live = await RESULTS_now();
function RESULTS_now() {
  return (async () => {
    const rows = [];
    for (const repo of REPOS) {
      const runs = await ghJson(['api', `repos/${ORG}/${repo}/actions/runs?per_page=10`, '--jq',
        '[.workflow_runs[] | {id,status,conclusion,name,created_at,run_attempt}]']);
      const ip = runs.filter(r => r.status === 'in_progress');
      const q = runs.filter(r => r.status === 'queued');
      for (const r of ip) rows.push(`${repo} IN_PROGRESS run ${r.id} (${r.name}) since ${r.created_at.slice(0, 16)} attempt=${r.run_attempt}`);
      for (const r of q) rows.push(`${repo} QUEUED run ${r.id} since ${r.created_at.slice(0, 16)}`);
      // retry loop: several consecutive runs of same event within 45 min each, all cancelled
      const cnt = {};
      const runs2 = await ghJson(['api', `repos/${ORG}/${repo}/actions/runs?per_page=20`, '--jq',
        '[.workflow_runs[] | {id,event,conclusion,created_at}]']);
      for (const r of runs2) cnt[`${r.conclusion || r.status}`] = (cnt[`${r.conclusion || r.status}`] || 0) + 1;
      if (cnt.cancelled >= 5) rows.push(`${repo} cancelled>=5 in last 20 runs (${cnt.cancelled}) — check same-event loop`);
    }
    return rows;
  })();
}
for (const r of live) log('  ' + r);

writeFileSync('_actions_sweep.json', JSON.stringify(store, null, 2));
writeFileSync('_actions_sweep.log', out.join('\n'));
log('\nWrote _actions_sweep.json + _actions_sweep.log');
