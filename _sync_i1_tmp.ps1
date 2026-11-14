# I1 sync pass — pushes the admin key-store fix (Workstream I) to the 18 repos.
# Created 2026-10-20 after explicit push authorization. Two phases:
#   default     : clone (depth 1) -> base assert -> copy changed files ->
#                 commit fix(admin) + docs(plan) -> push
#   -DocsPass   : after local doc update, copy plan doc only -> commit ->
#                 push (the verified-status follow-up, mirroring G1's docs pass)
# PS 5.1: invoke in-session as  & .\_sync_i1_tmp.ps1 -Nodes (1..18)  (arrays
# do not parse via powershell -File).
param(
    [int[]]$Nodes = @(1),
    [switch]$DocsPass,
    [switch]$Combined
)

$ErrorActionPreference = 'Continue'
$env:GCM_INTERACTIVE = 'never'

$src = 'C:\Users\basud\OneDrive\Desktop\node-1-main'
$tmp = Join-Path $env:TEMP 'opencode\i1_sync'
New-Item -ItemType Directory -Force -Path $tmp | Out-Null
$logPath = Join-Path $src '_sync_i1_tmp.log'

$codeFiles = @(
    'uploadkeys/uploadkeys.go',
    'uploadkeys/prober.go',
    'uploadkeys/uploadkeys_test.go',
    'router/router_handler.go',
    'router/view/templates/admin.html'
)
$docFile = 'docs/superpowers/plans/2026-10-17-fleet-remediation.md'
# -Combined second commit: the checkout drift the workstream syncs missed
# (found by the normalized tree audit; without it the tree does not compile
# against the fix(admin) prober). 13 files, node-1 already carries them.
$restFiles = @(
    'coordinator/assignment.go',
    'coordinator/coordinator.go',
    'coordinator/shuffle.go',
    'coordinator/dead_run_release_test.go',
    'main.go',
    'router/view/templates/index.html',
    'router/view/templates/scripts/app.js',
    'uploader/probe.go',
    'uploader/probe_test.go',
    'uploader/probe_upload.go',
    'uploader/probe_upload_test.go',
    'uploader/probe_video.go',
    'supabase/migrations/20261017000000_prune_stale_tunnels.sql'
)
$restMsg = @'
fix(sync): carry remaining checkout drift (B2 ReleaseHook, quota-aware Verify probes)

Per-workstream syncs only patched their own files, so this drift never
reached the repos - and the fix(admin) commit above references the new
Verify*/Quota probe API, which left the tree uncompilable without it.

- main.go: B2 ReleaseHook wiring (slot reclaim when the stuck-recording
  watchdog clears a dead run - without it reclaim cannot touch
  recording-status assignment rows and slots stay ghosted)
- uploader/probe.go + probe_test.go: quota-aware ProbeResult (Quota field;
  storage-full / daily-cap no longer reported as healthy)
- uploader/probe_upload.go + probe_video.go + probe_upload_test.go:
  genuine upload-round-trip Verify* probes (one tiny file, deleted again)
- coordinator/{assignment,coordinator,shuffle}.go + dead_run_release_test.go
- router/view/templates/index.html + scripts/app.js: settings-save verdict UI
- supabase/migrations/20261017000000_prune_stale_tunnels.sql (D2 file)

go build/vet ./... clean, uploadkeys/uploader/coordinator/router tests green
against these exact bytes.
'@
$expectedBase = 'docs(plan): fleet remediation verified status + Oct-20 sweep evidence'

$codeMsg = @'
fix(admin): boot-time key-store apply, replace-on-add, dead-key auto-reap

- Start() now calls applyLoaded(): the loaded central upload-key store
  overlays config+env at boot, so a node that boots AFTER an admin edit no
  longer spends its whole 6h session on stale env-derived keys (refresh()
  only applied on a detected change, so post-edit boots never converged; the
  startup SaveSettings() then re-persisted those stale keys too). A Managed
  empty store clears env as well - a reboot can no longer resurrect
  deliberately deleted keys.
- uploadkeys.Add REPLACES the host's credential list with the new key
  (rotation semantics): replaced status rows are dropped, the new key is
  re-verified in-request, and config+env are applied fleet-wide. Adding a
  host's only unchanged key stays a duplicate error. Admin panel note and
  add-button tooltip updated.
- reapDead(): credentials the host explicitly rejected (StatusDead) are
  removed fleet-wide every prober cycle, behind the probe lease and strictly
  AFTER probeStale() so the verdict survived a fresh re-probe in the same
  cycle before anything is deleted. quota/unknown/healthy never reap;
  removal is idempotent per fingerprint.
- New tests: TestApplyLoadedAppliesCentralSetAtBoot,
  TestAddReplacesOldKeysAndDropsStatuses, TestReapDeadRemovesOnlyDeadKeys.
  Full suite green incl. canary TestTrackLateAssetStillMissingIsTheOnlyRealFailure.
'@

$docsMsg = 'docs(plan): record workstream I - admin key-store authority (I1)'
$docsPassMsg = 'docs(plan): mark I1 synced 2026-10-20'

function Log([string]$m) {
    $line = "[{0}] {1}" -f (Get-Date -Format 'HH:mm:ss'), $m
    Write-Output $line
    Add-Content -Path $logPath -Value $line
}

$failed = @()

foreach ($n in $Nodes) {
    $name = "node-$n"
    $dir = Join-Path $tmp $name

    if ($DocsPass) {
        if (-not (Test-Path $dir)) {
            Log "$name : MISSING clone (pass 1 must run first)"
            $failed += $name
            continue
        }
        Push-Location $dir
        Copy-Item (Join-Path $src $docFile) (Join-Path $dir $docFile) -Force
        git add -- $docFile 2>$null
        $staged = @(git diff --cached --name-only 2>$null)
        if ($staged.Count -eq 0) {
            Log "$name : doc unchanged - nothing to push"
            Pop-Location
            continue
        }
        git commit -q -m $docsPassMsg 2>$null
        if ($LASTEXITCODE -ne 0) {
            Log "$name : DOCS COMMIT FAILED"
            $failed += $name
            Pop-Location
            continue
        }
        $branch = (git rev-parse --abbrev-ref HEAD).Trim()
        git push origin "HEAD:$branch" 2>$null
        if ($LASTEXITCODE -ne 0) {
            Log "$name : DOCS PUSH FAILED"
            $failed += $name
            Pop-Location
            continue
        }
        Log "$name : docs-pass pushed $((git rev-parse --short HEAD).Trim())"
        Pop-Location
        continue
    }

    # ---- pass 1: full clone + code/docs commits + push ----
    if (Test-Path $dir) { Remove-Item -Recurse -Force $dir }
    git clone --depth 1 --single-branch "https://github.com/lawdachuss/$name.git" $dir 2>$null
    if ($LASTEXITCODE -ne 0) {
        Log "$name : CLONE FAILED"
        $failed += $name
        continue
    }
    Push-Location $dir
    $base = (git log -1 --format=%s 2>$null)
    if ($null -ne $base) { $base = $base.Trim() }
    if ($base -ne $expectedBase) {
        Log "$name : BASE MISMATCH - got '$base'"
        $failed += $name
        Pop-Location
        continue
    }
    git config user.name  'lawdachuss'
    git config user.email 'lawdachuss@users.noreply.github.com'

    foreach ($f in ($codeFiles + $docFile)) {
        $dst = Join-Path $dir ($f -replace '/', '\')
        $dstDir = Split-Path $dst -Parent
        New-Item -ItemType Directory -Force -Path $dstDir | Out-Null
        Copy-Item (Join-Path $src ($f -replace '/', '\')) $dst -Force
    }

    git add -- $codeFiles 2>$null
    $staged = @(git diff --cached --name-only 2>$null)
    if ($staged.Count -ne $codeFiles.Count) {
        Log "$name : STAGED $($staged.Count) of $($codeFiles.Count) code files - aborting node (drift?)"
        $failed += $name
        Pop-Location
        continue
    }
    if ($n -eq 1) {
        Log "node-1 staged stat: $((git diff --cached --stat 2>$null) -join ' | ')"
    }
    git commit -q -m $codeMsg 2>$null
    if ($LASTEXITCODE -ne 0) {
        Log "$name : CODE COMMIT FAILED"
        $failed += $name
        Pop-Location
        continue
    }
    $sha1 = (git rev-parse --short HEAD).Trim()

    git add -- $docFile 2>$null
    $stagedDoc = @(git diff --cached --name-only 2>$null)
    if ($stagedDoc.Count -ne 1) {
        Log "$name : DOC NOT STAGED - aborting node"
        $failed += $name
        Pop-Location
        continue
    }
    git commit -q -m $docsMsg 2>$null
    if ($LASTEXITCODE -ne 0) {
        Log "$name : DOCS COMMIT FAILED"
        $failed += $name
        Pop-Location
        continue
    }
    $sha2 = (git rev-parse --short HEAD).Trim()

    if ($Combined) {
        foreach ($f in $restFiles) {
            $dst = Join-Path $dir ($f -replace '/', '\')
            $dstDir = Split-Path $dst -Parent
            New-Item -ItemType Directory -Force -Path $dstDir | Out-Null
            Copy-Item (Join-Path $src ($f -replace '/', '\')) $dst -Force
        }
        git add -- $restFiles 2>$null
        $stagedRest = @(git diff --cached --name-only 2>$null)
        if ($stagedRest.Count -ne $restFiles.Count) {
            Log "$name : STAGED $($stagedRest.Count) of $($restFiles.Count) drift files - aborting node"
            $failed += $name
            Pop-Location
            continue
        }
        git commit -q -m $restMsg 2>$null
        if ($LASTEXITCODE -ne 0) {
            Log "$name : DRIFT COMMIT FAILED"
            $failed += $name
            Pop-Location
            continue
        }
        $sha3 = (git rev-parse --short HEAD).Trim()
        Log "$name : drift commit $sha3"
    }

    $branch = (git rev-parse --abbrev-ref HEAD).Trim()
    git push origin "HEAD:$branch" 2>$null
    if ($LASTEXITCODE -ne 0) {
        Log "$name : PUSH FAILED"
        $failed += $name
        Pop-Location
        continue
    }
    Log "$name : pushed $sha1 (code) + $sha2 (docs) -> $branch"
    Pop-Location
}

if ($failed.Count -gt 0) {
    Log "FAILED nodes: $($failed -join ', ')"
    exit 1
}
Log "PASS complete for: $($Nodes -join ', ')"
exit 0
