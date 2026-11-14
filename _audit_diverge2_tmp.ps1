# Audit v2: compare local checkout vs clone with LINE ENDINGS NORMALIZED,
# so a CRLF/LF checkout artifact cannot masquerade as content drift.
# Real drift = differs after normalizing, or exists on one side only.
param(
    [string]$Local = 'C:\Users\basud\OneDrive\Desktop\node-1-main',
    [string]$Clone = (Join-Path $env:TEMP 'opencode\i1_sync\node-1')
)
$ErrorActionPreference = 'Continue'

$skipExact = @('.env', '.gitignore', '_sync_i1_tmp.ps1', '_sync_i1_tmp.log',
    '_audit_diverge_tmp.ps1', '_audit_diverge2_tmp.ps1',
    '_fleet_sweep_tmp.js', '_actions_sweep_tmp.js', '_ci_fail_tmp.txt')
$skipDirs  = @('.git', 'node_modules')

function Get-NormHash([string]$path) {
    $bytes = [System.IO.File]::ReadAllBytes($path)
    # Normalize CRLF -> LF for comparison only (hashes stay byte-exact for
    # the summary line, see Get-RawHash usage below if needed).
    $text = [System.Text.Encoding]::UTF8.GetString($bytes)
    $text = $text -replace "`r`n", "`n"
    $sha  = [System.Security.Cryptography.SHA256]::Create()
    $hash = $sha.ComputeHash([System.Text.Encoding]::UTF8.GetBytes($text))
    return ($hash | ForEach-Object { $_.ToString('x2') }) -join ''
}

function Get-Tree([string]$root) {
    $map = @{}
    Get-ChildItem -Path $root -Recurse -File -Force | ForEach-Object {
        $rel = $_.FullName.Substring($root.Length + 1)
        $parts = $rel -split '\\'
        foreach ($d in $skipDirs) { if ($parts -contains $d) { return } }
        if ($skipExact -contains $parts[-1]) { return }
        if ($parts[-1] -like '*_tmp*') { return }
        $map[$rel] = Get-NormHash $_.FullName
    }
    return $map
}

$a = Get-Tree $Local
$b = Get-Tree $Clone

$onlyLocal = @($a.Keys | Where-Object { -not $b.ContainsKey($_) } | Sort-Object)
$onlyClone = @($b.Keys | Where-Object { -not $a.ContainsKey($_) } | Sort-Object)
$differs   = @($a.Keys | Where-Object { $b.ContainsKey($_) -and $a[$_] -ne $b[$_] } | Sort-Object)

Write-Output "DIFFERING(normalized)=$($differs.Count) ONLYLOCAL=$($onlyLocal.Count) ONLYCLONE=$($onlyClone.Count)"
Write-Output "== DIFFERING =="
$differs | ForEach-Object { Write-Output "  $_" }
Write-Output "== ONLY IN LOCAL =="
$onlyLocal | ForEach-Object { Write-Output "  $_" }
Write-Output "== ONLY IN CLONE =="
$onlyClone | ForEach-Object { Write-Output "  $_" }
