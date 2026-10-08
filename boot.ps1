# aicrew-agent installer for Windows: installs or upgrades the member's
# aicrew-agent, from any directory:
#
#   powershell -NoProfile -ExecutionPolicy Bypass -Command "irm https://raw.githubusercontent.com/BlackVS/aicrew/v0.4.0/boot.ps1 | iex"
#
# It installs the release this script was fetched from ($release below):
# aicrew-agent-windows-amd64.exe, checked against that release's
# SHA256SUMS, as %LOCALAPPDATA%\aicrew\bin\aicrew-agent.exe, and adds that
# directory to the user's PATH when it is missing. An installed
# aicrew-agent that already reports the release is left as it is; an older
# one is replaced, and both versions are printed. A running aicrew-agent
# keeps its file: the old file is renamed aside (aicrew-agent.exe.old-<UTC
# time>) and removed by a later run once nothing uses it. It installs the
# binary and nothing else: no agent home, no join, no client settings, no
# file outside the bin directory. After a first install, run
# `aicrew-agent join` (or rerun it for an existing home, so its Stop hook
# names this binary).
#
# Optional environment:
#   AICREW_BIN_DIR=...       where aicrew-agent.exe goes
#   AICREW_VERSION=vX.Y.Z    install another release than $release
#   AICREW_REPO=owner/name   install from a fork
$ErrorActionPreference = 'Stop'

# The release this script installs. Bumped together with the CHANGELOG
# when a release is cut (internal/installer checks they agree).
$release = 'v0.4.0'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$repo = if ($env:AICREW_REPO) { $env:AICREW_REPO } else { 'BlackVS/aicrew' }
$tag = if ($env:AICREW_VERSION) { $env:AICREW_VERSION } else { $release }
$dlBase = "https://github.com/$repo/releases/download/$tag"
$binDir = if ($env:AICREW_BIN_DIR) { $env:AICREW_BIN_DIR } else { Join-Path $env:LOCALAPPDATA 'aicrew\bin' }

# BEGIN install-agent
# Install-Agent: install aicrew-agent $Tag from $DlBase as
# $BinDir\aicrew-agent.exe. The download is checked against the release's
# SHA256SUMS in a temporary directory; nothing in the bin directory changes
# unless it passes.
function Get-AgentVersion([string]$Exe) {
  try {
    $line = (& $Exe version 2>$null | Select-Object -First 1)
    if ($LASTEXITCODE -eq 0 -and $line) { return ($line -split '\s+')[1] }
  } catch {}
  return ''
}
# Get-Sha256 hashes through .NET: Get-FileHash lives in a module that a
# powershell.exe started from PowerShell 7 may not find (it inherits
# PSModulePath).
function Get-Sha256([string]$Path) {
  $sha = [Security.Cryptography.SHA256]::Create()
  $f = [IO.File]::OpenRead($Path)
  try { return (($sha.ComputeHash($f) | ForEach-Object { $_.ToString('x2') }) -join '') }
  finally { $f.Dispose(); $sha.Dispose() }
}
function Install-Agent([string]$Tag, [string]$DlBase, [string]$BinDir) {
  $exe = Join-Path $BinDir 'aicrew-agent.exe'
  $asset = 'aicrew-agent-windows-amd64.exe'
  $old = ''
  if (Test-Path $exe) {
    $old = Get-AgentVersion $exe
    if ($old -eq $Tag) {
      Write-Host "aicrew-agent $Tag is current ($exe); nothing changed."
      return
    }
  }
  $tmp = Join-Path ([IO.Path]::GetTempPath()) ('aicrew-agent-' + [Guid]::NewGuid().ToString('N'))
  New-Item -ItemType Directory $tmp | Out-Null
  try {
    Write-Host "fetching aicrew-agent $Tag ($asset)"
    $sums = Join-Path $tmp 'SHA256SUMS'
    $dl = Join-Path $tmp $asset
    try { Invoke-WebRequest "$DlBase/SHA256SUMS" -OutFile $sums -UseBasicParsing }
    catch { throw "cannot fetch the SHA256SUMS of $Tag; refusing an unverified binary. Nothing changed." }
    try { Invoke-WebRequest "$DlBase/$asset" -OutFile $dl -UseBasicParsing }
    catch { throw "cannot fetch $asset of $Tag. Nothing changed." }
    $want = Get-Content $sums | ForEach-Object {
      $f = $_ -split '\s+'
      if ($f.Count -ge 2 -and ($f[1] -eq $asset -or $f[1] -eq "*$asset")) { $f[0].ToLower() }
    } | Select-Object -First 1
    $got = Get-Sha256 $dl
    if (-not $want -or $want -ne $got) {
      $shown = if ($want) { $want } else { 'absent' }
      throw "checksum mismatch for $asset (want $shown, got $got); refusing it. Nothing changed."
    }
    $new = Get-AgentVersion $dl
    New-Item -ItemType Directory -Force $BinDir | Out-Null
    # A running aicrew-agent.exe cannot be overwritten but can be renamed:
    # the old file steps aside, and a later run removes it.
    Get-ChildItem $BinDir -Filter 'aicrew-agent.exe.old-*' -ErrorAction SilentlyContinue |
      ForEach-Object { Remove-Item $_.FullName -Force -ErrorAction SilentlyContinue }
    $aside = ''
    if (Test-Path $exe) {
      $aside = Join-Path $BinDir ('aicrew-agent.exe.old-' + (Get-Date).ToUniversalTime().ToString('yyyyMMddTHHmmssZ'))
      Rename-Item $exe (Split-Path $aside -Leaf)
    }
    try { Copy-Item $dl $exe }
    catch {
      if ($aside) { Rename-Item $aside 'aicrew-agent.exe' }
      throw "cannot write $exe ($($_.Exception.Message)); the previous aicrew-agent is back."
    }
    $shownNew = if ($new) { $new } else { $Tag }
    if ($old) { Write-Host "upgraded aicrew-agent $old -> $shownNew ($exe)" }
    else { Write-Host "installed aicrew-agent $shownNew ($exe)" }
  } finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
  }
}
# END install-agent

Install-Agent $tag $dlBase $binDir
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
$parts = if ($userPath) { $userPath -split ';' } else { @() }
if (-not ($parts | Where-Object { $_.TrimEnd('\') -ieq $binDir.TrimEnd('\') })) {
  $joined = if ($userPath) { $userPath.TrimEnd(';') + ';' + $binDir } else { $binDir }
  [Environment]::SetEnvironmentVariable('Path', $joined, 'User')
  Write-Host "added $binDir to your PATH; open a new terminal to use it."
}
