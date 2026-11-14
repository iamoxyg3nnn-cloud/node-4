# Fleet-wide sweep — 18 nodes via Supabase + trycloudflare tunnels (2026-10-17 ~06:37–06:47 UTC)

Method: roster + `tunnels`/`tunnel_sessions`/`channel_logs`/`upload_journal`/`disk_usage`/`pipeline_states`/`channel_assignments` pulled from Supabase REST (anon key from `.env`), then every `nodes.web_url` probed live (`/api/health`,
`/api/orphans`, `/api/logs?after=0`). Re-runnable sweeps: [`_fleet_sweep_tmp.js`](_fleet_sweep_tmp.js) → [`_sweep.json`](_sweep.json), [`_sweep.log`](_sweep.log).

## Tunnel inventory (from Supabase `nodes.web_url`)

| node | status | load | tunnel URL | health probe |
|---|---|---|---|---|
| node-1 | online | 5 | https://united-january-debug-ash.trycloudflare.com | 200, 1.5s |
| node-2 | online | 0 | https://refer-domestic-romantic-tyler.trycloudflare.com | 200, 2.2s |
| node-3 | online | 0 | https://conflict-findings-losing-podcasts.trycloudflare.com | 200, 1.5s |
| node-4 | online | 193 | https://commonly-amino-females-concentrations.trycloudflare.com | 200, 1.2s |
| node-5 | online | 0 | https://damaged-slot-logan-dealer.trycloudflare.com | 200, 1.5s |
| node-6 | online | 3 | https://closest-lists-roots-ltd.trycloudflare.com | **TUNNEL HANGS** (all endpoints >40s timeout) |
| node-7 | online | 0 | https://remove-sandwich-related-individuals.trycloudflare.com | 200, 2.8s |
| node-8 | online | 166 | https://seasons-ratio-bradley-apr.trycloudflare.com | 200, 1.7s |
| node-9 | online | 4 | https://personals-construct-analytical-responded.trycloudflare.com | 200, 1.8s |
| node-10 | online | 167 | https://shoulder-swaziland-bag-jeremy.trycloudflare.com | 200, 1.6s |
| node-11 | online | 5 | https://exit-market-proceeding-queens.trycloudflare.com | 200, 1.6s |
| node-12 | online | 0 | https://airlines-summaries-collectibles-democratic.trycloudflare.com | 200, 1.8s |
| node-13 | online | 1 | https://variables-basin-maui-latest.trycloudflare.com | 200, 2.4s |
| node-14 | online | 0 | https://agreed-argument-replied-shipment.trycloudflare.com | 200, 1.5s |
| node-15 | online | 167 | https://reaching-memorial-adaptor-with.trycloudflare.com | 200, 1.4s |
| node-16 | online | 166 | https://concern-precisely-similar-spas.trycloudflare.com | 200, 1.7s |
| node-17 | online | 0 | https://tel-autos-yoga-triple.trycloudflare.com | 200, 1.7s |
| node-18 | online | 0 | https://wiring-graduates-employer-whether.trycloudflare.com | 200, 2.8s |

- 18/18 nodes heartbeat-fresh (<5 min), all registered as `online`.
- `tunnels` table holds **one stale row**: `memphis-unto-outsourcing-meanwhile.trycloudflare.com` (from 2026-08-10, instance `default`) — DNS-dead. `tunnel_sessions` table **does not exist** anymore → `scripts/get-tunnel-url.ps1` / `open-ui.ps1` will 404 errors.

## Critical

### C1. node-6 tunnel hangs — node is online but unreachable through its published URL
`/api/health`, `/api/orphans`, `/api/logs` **all time out >40s** in two consecutive runs. Cloudflare still accepts the TCP/TLS side (the connection opens and then nothing), which points at either cloudflared's origin connection being wedged or the node process being starved. Its `web_url` also attracts the stale-tunnel problem from NODE_FLEET_FINDINGS §9: liveness probing that blanks a dead URL only catches DNS failures, not hangs. Needs remote restart (or the keep-alive liveness probe extended to time out and re-mint the tunnel).

### C2. Zero-byte stuck recordings are back — 53 empty files fleet-wide today
Fresh repeat of NODE_FLEET_FINDINGS #1, new cohort (files stamped today 02:00–06:36): node-4 **12**, node-8 **11**, node-16 **8**, node-10 **7**, node-11/15/9/13 5/4/4/1.

| node | orphans (total) | zero-byte |
|---|---|---|
| node-4 | 14 | 12 |
| node-8 | 12 | 11 |
| node-16 | 13 | 8 |
| node-10 | 9 | 7 |
| node-11 | 5 | 5 |
| node-15 | 5 | 4 |
| node-9 | 5 | 4 |
| node-13 | 1 | 1 |
| others | 0 | 0 |

Sample: `node-11/alicesternley_2026-10-17_06-36-16.mp4` cohort all started within a 60 s window (06:36:16–06:36:17), matching a fleet-wide session-start stampede. Same root causes as documented (cleanupLocked only runs on channel close; CleanupOrphanedFiles skips activeRecs; coordinator skips `status='recording'`; admin matrix skips size 0). No fix landed since the 2026-09-22 report.

### C3. Upload host auth is broken fleet-wide (3 external services)

1. **Streamtape — fix the shared key.** 403 "Authentication failed" on every node that uploads, with the "rejected our credentials — disabling it for the rest of this run" branch firing on node-16/18. Not node-local: the shared Streamtape credential rotating from `.env` is dead/rotated. Node-local guard behavior is otherwise fine (per-run disable works).
2. **AnonMP4 — HTTP 522 (Cloudflare origin-timeout upstream)** on the `anonmp4: node assignment` endpoint across ~10 nodes ("all 4 attempts failed, last: HTTP 522: error code: 522"). AnonMP4's edge is up-and-broken; all 4 retry attempts per file do nothing. Recommend treating 522 as a daily/circumstantial disable class (skip the host on a 3-strike daily counter, not 3 files in a row per-run which resets every cycle).
3. **ImgBB — HTTP 400 "Rate limit reached"** on thumbnails/previews/sprites everywhere (single worst offender: node-7 with 236 ERROR lines in a 5k ring — mostly ImgBB). Multiple keys exist in `.env` (comma-separated) but the rotation is not absorbing this: same key reused until 400, then retried immediately. → previews / platform thumbnails are going unpersisted on a large fraction of rows (WARN "still has no URL 2h after being abandoned" seen on nodes 1/3/5/8/9/18).

### C4. Every node with recordings has channels "recording" while `is_live=false` (worst affected: node-4/8/16)
`channel_assignments` today: node-4 12/12, node-8 11/11, node-16 8/8, node-11 4/5, node-15 4/4, node-9 4/4 — all `status='recording'` with `is_live=false` and **fresh heartbeats**. This is the same stuck-recording signature as finding #1 (recorder went into a broken loop with nothing on disk but slot + heartbeat). Each occupied recording slot blocks pool rebalancing until the node restarts.

## Warnings (per-node)

| node | issues detected this sweep |
|---|---|
| node-1 | ImgBB rate-limit; 55 err /16 warn in ring |
| node-2 | Streamtape 403 + AnonMP4 522 (uploaded-warning class) |
| node-3 | ImgBB rate-limit spam; 87 err /12 warn |
| node-4 | C2 C4 (largest zero-byte cohort) + ImgBB on preview |
| node-5 | AnonMP4 522; ImgBB; thumbnail/preview "abandoned" warns |
| node-6 | **C1 tunnel hangs** + finalize error (`finalize output` in channel_logs) |
| node-7 | 236 err ring — ImgBB flood; streaming OK otherwise |
| node-8 | C2 C4 + audio-seg download failures ("All attempts fail" ×run) |
| node-9 | C2 C4 (0-byte cohort) + ImgBB |
| node-10 | C2 + recorder "forbidden media endpoint" once; channels stall loop (`twins_terror` offline-retry every min) |
| node-11 | C2 C4 + Streamtape/AnonMP4 403/522 |
| node-12 | Streamtape 403 + AnonMP4 522 (worst error-rate node: 126 err /34 warn in ring) |
| node-13 | 1 zero-byte orphan (`lau__1_2026-10-17_06-18-29.mp4`) |
| node-14 | Streamtape 403 + AnonMP4 522 + Supabase schema-cache retries |
| node-15 | C2 C4 + recorder "forbidden media endpoint" |
| node-16 | C2 C4 + Streamtape 403; 21 upload_journal rows in 24 h, 8 failed |
| node-17 | error-rate node: 129 err /27 warn; Streamtape 403 ×many, AnonMP4 522 ×many ("disabling it for rest of this run" ×6) |
| node-18 | 14 err/24h; Streamtape 403 ("rejected credentials"), AnonMP4 522 |

`errors24h` persisted `channel_logs` (top nodes): node-17 30, node-18 14, node-16 13, node-12 8, node-14 7, node-11 5. Nodes 1/2/3/4/5/7/8/13 logged zero ERROR rows in the last 24 h even though their in-memory rings show new errors — those ERRORs are only in ring (not persisted). Node-14 also logged `Supabase schema cache stale — column missing (attempt 3/3)` again (retries-over-restart, no lasting fix) — same class of error flagged in NODE_FLEET_FINDINGS.

## infra inventory (small odds and ends)

- `upload_journal` in 24 h: 78 rows total across fleet, 38 failed (worst: node-14 14/31, node-17 ~12, node-16 8/21).
- `pipeline_states` currently almost unused (≤1 active row per node, only nodes 8/14/15/16/18 have any) — mostly a recording-only run right now.
- disk: all under 16% used — none at risk (fresh rows within 0–9 min, fleet 1–16% used, 126–217 GiB free).

## Fix priority (from today's data)

1. **Rotate Streamtape creds; fix ImgBB key rotation; treat AnonMP4 522 as daily-disable.** Streamtape 403 = dead shared key, single cleanest fix. ImgBB: rotate the comma-separated keys on 400 and add per-key cooldown, not per-file retry. AnonMP4 522: skip on a rolling daily counter instead of a per-run "3 files in a row" that resets every session.
2. **node-6 fresh restart** — the only node whose tunnel hangs >40 s. Also worth wiring timeout-liveness re-mint (probe >10 s → re-mint tunnel + update `web_url`) to catch the hang class, not only DNS failures.
3. **Stuck-recording reclamation** — `channel_assignments` rows `status='recording'` with `is_live=false` and a 0-byte file (nodes 4/8/16 worst) should be reclaimable by the coordinator; today `ReclaimChannels` skips `recording` rows unconditionally, so slots burn until a node restart.
4. **Zero-byte cleanup** — the 4-part recipe from NODE_FLEET_FINDINGS #1 is still unfixed (close-loop delete, skip-list of activeRecs, orphan scan cross-check, admin matrix size>0 filter).
5. **Delete the stale `tunnels` row** (instance `default`, 2026-08-10, DNS-dead) so old-URL fallback never latches onto it, and re-point `scripts/get-tunnel-url.ps1` / `open-ui.ps1` to `tunnels` (or re-create `tunnel_sessions`, which 404s today).

---

# GitHub Actions deep check (added 2026-10-17 ~07:05 UTC)

Source: authenticated `gh` CLI (account `lawdachuss`; `.env` also carries `GITHUB_ACCESS_TOKEN`, unused here). All 18 fleet repos `lawdachuss/node-1 … node-18` swept — runs inventory, every failure's job log, last successful session's log per repo. Artifacts: [_actions_sweep.json](_actions_sweep.json), [_actions_sweep.log](_actions_sweep.log).

## Fleet Actions architecture (verified live)

- One active workflow per repo: **`Secure RDP`** — 5h40m recording session on `windows-latest`, cron `0 */6`, plus `workflow_dispatch` re-mints from two Cloudflare Workers (`workflow-watchman`, `node-watchman` — deployed per commit `a63d8706` "node-watchman: add fleet monitor worker").
- **The CI workflow was deleted on 2026-10-15 09:00 UTC** (commit `7bca11d8` 2026-10-15 09:00 UTC: *"ci: remove CI workflow — nodes build from source at every session start, so per-push CI runs just sit queued behind the 6h RDP sessions and starve the account runner budget"*). Repos have **no test gate since then**; every session builds chaturbate-dvr from whatever is on `main`.
- Run pattern over the last 20 runs/repo: success 7–8, cancelled 8–10, failure 2–4, in_progress 1. The cancels are the *designed* self-cancel (schedule fires while a session is live; Init cancels duplicates + queued runs). **No retry-loop and no stuck-queued runs anywhere** — every repo currently has exactly one session running (started 01:08–06:49 today).

## Real failures found in logs

1. **The last CI ever run is red on all 18 repos — one failing Go test, and it's the stuck-recording class.** Pushes at 06:41/07:07/07:16 on Oct 15 left `build-test` failed in every repo (45+ runs), always in the same place:
   ```
   --- FAIL: TestTrackLateAssetStillMissingIsTheOnlyRealFailure (0.01s)
       channel/asset_late_test.go:172: an asset that never finishes is a real
       failure and must be reported once, got: []
   FAIL  github.com/teacat/chaturbate-dvr/channel  5.464s
   ```
   All other packages passed. Instead of fixing it, the same day CI was deleted (09:00 commit) — **before a green CI ever shipped** — and the 09:00 bulk-sync commit never ran any check. Consequence: every live node session since Oct 15 09:00 has been building a binary that the repo's own late-asset test flags as broken. This is the strongest root-cause candidate on record for **C2/C4 (zero-byte stuck recordings + `recording` rows with `is_live=false`)** — the test failing is *literally the assertion that a never-finishing asset must be surfaced as a real failure*. The regression shipped silently and the sentinel that would have caught it was removed.
2. **node-4's 01:08 session died at 03:00 into the run with no logs.** Run `38989576547` (workflow_dispatch): job `rdp` failure 01:08:50 → 04:08:01, **zero failed steps reported and its job log is unrecoverable** (`log not found`), so the death cause is lost. `node-watchman` correctly re-minted a session 2 minutes later (04:10, currently running). node-4 is also the node with today's worst stuck cohort (12 zero-byte files, 12 ghost slots) — repeated dead/logless sessions fit that pattern. Only node-4 shows this; worth wiring `upload-artifact` of the DVR log at failure (the current artifact path only publishes `tunnel-url`).
3. Everything else failing = the designed cancels. No workflow-level retry-loop: repo failure counts are dominated by the three Oct-15 CI pushes (2–4 "failures" per repo = the CI trio) and single `Secure RDP` job deaths like node-4's.

## Actions ↔ fleet-finding correlation

| Fleet finding | Actions evidence |
|---|---|
| **C2 zero-byte stuck recordings / C4 ghost slots** | **Direct hit.** `TestTrackLateAssetStillMissingIsTheOnlyRealFailure` (the exact invariant "a never-finishing asset is a failure") was red on every repo when CI was deleted; all today's sessions build from that un-fixed `main`. node-4, the logless-death node, owns the biggest cohort (12). Node-11's 05:xx–06:36 zero-byte files landed mid-session (01:08 dispatch cohort). |
| **C1 node-6 tunnel hang** | Actions shows node-6's runner **alive** (session in_progress since 01:41). So the wedge is inside the runner's `cloudflared`/DVR process, not the Actions layer; and nothing in the workflow catches a hung tunnel — `/api/health`-style probes are not part of the job, the green run keeps waving. |
| **C3 broken upload hosts** | Confirmed in Actions logs identically to node logs (Streamtape 403 "Authentication failed", AnonMP4 522, ImgBB "Rate limit reached"). Even the Oct-15 CI run logged the same Streamtape 403 from a *test* (`TestPipelineQueueDedup` → "Streamtape rejected our credentials") — i.e. the dead Streamtape credential predates and outlives the workflow layer. |
| **Tunnel rotation (§9 stale URLs)** | Each session's log shows its own fresh trycloudflare URL (e.g. node-1's previous session `resolutions‑internet‑trends‑ray`, current `united-january-debug-ash` in Supabase). Re-registration works for currently-live sessions; the failures are stale-row hygiene (old `tunnels` row, `tunnel_sessions` 404), not the session dispatch itself. |

## Green-but-doing-nothing check

No silent-green sessions found: each repo's last success shows a minted tunnel + session env dump, and Supabase independently corroborates activity (`disk_usage` rows <9 min old, `upload_journal` rows in 24h, upload/persistence lines). The real no-op hazards to note:
- `Prepare cookies (parallel)` step runs with `continue-on-error: true` → a dead cookie prep is invisible to the run status.
- `Upload Tunnel URL` artifact uses `if-no-files-found: ignore` → a session that never minted a tunnel still reads green.
- The heavy `cancelled` count (8–10 per repo in 20 runs) looks like mass failure on the dashboard but is by-design duplicate suppression.

## Checks that could not run / limitations

- ~~No local Go toolchain → could not re-run the failing test at HEAD~~ **Resolved in the follow-up section below**: Go 1.26.9 installed, test re-run at HEAD (local file verified byte-identical to repo `main`), **still FAILS** — see "Follow-up verification" §1.
- node-4 run `38989576547`'s job log is gone (platform-side `log not found`), so the cause of that mid-session death is not recoverable.
- `gh run list --json id` isn't a valid field in this gh build (minor; used `databaseId`).

## Action-layer fix priorities

1. **Reinstate a test gate** (re-enable CI or move the test into `parallel-setup.ps1` pre-flight): the fleet's stuck-recording regression is *the* known failing test — fix or gate before the next session build.
2. **Correct `TestTrackLateAssetStillMissingIsTheOnlyRealFailure`** and the late-asset tracking that underpins it (same family as the C2/C4 zero-byte bugs) — this is the fix-delivery path for the whole zero-byte cohort.
3. **Auto-artifact the DVR log on job death** (continue-on-error step with `if: failure() || cancelled()`) so a logless node-4-class death is diagnosable next time.
4. **Probe cloudflared health inside the session** (every 5 min in keep-alive, as already specced for node-6 in C1's remediation): Actions-green no longer masks a wedged tunnel.

## Follow-up verification (added 2026-10-17 ~07:35 UTC)

### 1. The headline claim is now PROVEN, not inferred
A Go 1.26.9 toolchain was installed locally (official zip, no admin) and the exact command from the repo's CI was run against this checkout — whose `channel/asset_late_test.go` is **byte-identical** to `main` on `lawdachuss/node-3` (verified both 6889 bytes and Base64 content equal against `gh api repos/lawdachuss/node-3/contents/...`, i.e. the state after the CI-deletion commit `7bca11d8`):

```
$ go test ./channel -run TestTrackLateAssetStillMissingIsTheOnlyRealFailure -count=1
TEST-EXIT=1
--- FAIL: TestTrackLateAssetStillMissingIsTheOnlyRealFailure (0.01s)
    asset_late_test.go:172: an asset that never finishes is a real failure and
    must be reported once, got: []
FAIL    github.com/teacat/chaturbate-dvr/channel    5.024s
```

The regression that the deleted CI was flagging **still reproduces on the exact code every node session builds from today**. The stuck-recording root-cause story (C2/C4 via the shipped-without-gate late-asset regression) stands on direct evidence. Output archived in [_go_test_lateasset.log](_go_test_lateasset.log).

### 2. `GET /api/uploads` — response shape known, per-node queue depth measured
Prior sweep's parser hole is closed. Response (live-verified on node-1, matches `entity/entity.go:146-183` — `UploadsResponse`):

```json
{ "active":    [UploadEntry]   // channel, filename, status, progress(0-100),
                               // host_count, host_total, bytes_current,
                               // bytes_total, speed, hosts[] per-host progress
  "pending":   [PendingEntry],// channel, filename, stage, failed, error, size
  "history":   [PendingEntry],
  "processing": true|false    // node is in the post-session drain
}
```

Fleet-wide measurement (18/18 nodes, taken ~07:20 UTC after the ~07:1x session rollover): **every node reports `active=0 pending=0 history=0`** — upload queues are drained; the only non-idle node is **node-6 (`processing=true`)**, i.e. it is actively draining post-session uploads. Implication: today's earlier upload failures (Streamtape/AnonMP4/ImgBB) are not accumulating a backlog; files that failed their host attempts were either final-attempt-persisted or dropped, which makes the `upload_journal` failure counts the accurate loss metric, not queue depth.

### 3. node-6 tunnel recovered on session rollover
node-6's wedged tunnel (`closest-lists-roots-ltd`, timed-out >40s all endpoints at ~06:35–06:45) is gone with that session; post-rollover (fresh `web_url`, ~07:2x) node-6 answers `/api/uploads` normally and is the one node with a live upload drain. C1's wedge was session-scoped; the follow-up recommendation stands (5-min in-session tunnel probe + re-mint) so the *next* hang is caught inside the session instead of self-resolving at a boundary that may cost hours of unreachable time.
