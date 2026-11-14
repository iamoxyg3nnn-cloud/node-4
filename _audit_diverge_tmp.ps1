# Audit: hash-compare the local source checkout against a node-1 clone tree.
# Reports files that differ, exist only locally, or exist only in the clone.
param(
    [string]$Local = 'C:\Users\basud\OneDrive\Desktop\node-1-main',
    [string]$Clone = (Join-Path $env:TEMP 'opencode\i1_sync\node-1')
)
$ErrorActionPreference = 'Continue'

$skipExact = @('.env', '_sync_i1_tmp.ps1', '_sync_i1_tmp.log', '_fleet_sweep_tmp.js',
    '_actions_sweep_tmp.js', '_ci_fail_tmp.txt', '.gitignore')
$skipDirs  = @('.git', 'node_modules')

function Get-Tree([string]$root) {
    $map = @{}
    Get-ChildItem -Path $root -Recurse -File -Force | ForEach-Object {
        $rel = $_.FullName.Substring($root.Length + 1)
        $parts = $rel -split '\\'
        foreach ($d in $skipDirs) { if ($parts -contains $d) { return } }
        if ($skipExact -contains $parts[-1]) { return }
        if ($parts[-1] -like '*_tmp*') { return }
        $map[$rel] = (Get-FileHash -Algorithm SHA256 -Path $_.FullName).Hash
    }
    return $map
}

$a = Get-Tree $Local
$b = Get-Tree $Clone

$onlyLocal = @($a.Keys | Where-Object { -not $b.ContainsKey($_) } | Sort-Object)
$onlyClone = @($b.Keys | Where-Object { -not $a.ContainsKey($_) } | Sort-Object)
$differs   = @($a.Keys | Where-Object { $b.ContainsKey($_) -and $a[$_] -ne $b[$_] } | Sort-Object)

Write-Output "== DIFFERING ($($differs.Count)) =="
$differs | ForEach-Object { Write-Output "  $_" }
Write-Output "== ONLY IN LOCAL ($($onlyLocal.Count)) =="
$onlyLocal | ForEach-Object { Write-Output "  $_" }
Write-Output "== ONLY IN CLONE ($($onlyClone.Count)) =="
$onlyClone | ForEach-Object { Write-Output "  $_" }
