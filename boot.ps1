# aicrew-agent installer for Windows: installs or upgrades the member's
# aicrew-agent, and the operator CLI aicrew beside it, from any directory:
#
#   powershell -NoProfile -ExecutionPolicy Bypass -Command "irm https://raw.githubusercontent.com/BlackVS/aicrew/v0.6.0/boot.ps1 | iex"
#
# It installs the release this script was fetched from ($release below):
# aicrew-agent-windows-amd64.exe and aicrew-windows-amd64.exe, each checked
# against that release's SHA256SUMS, as aicrew-agent.exe and aicrew.exe in
# %LOCALAPPDATA%\aicrew\bin, and adds that directory to the user's PATH
# when it is missing. A binary that already reports the release is left as
# it is; an older one is replaced, and both versions are printed. A
# checksum mismatch installs neither. A running binary keeps its file: the
# old file is renamed aside (<name>.exe.old-<UTC time>) and removed by a
# later run once nothing uses it. It installs the binaries and nothing else: no agent home, no join, no client settings, no
# file outside the bin directory. After a first install, run
# `aicrew-agent join` (or rerun it for an existing home, so its Stop hook
# names this binary). A member home in turns mode runs as a service
# (aicrew-agent service): an upgrade stops every home's service before the
# swap and starts it again after.
#
# Optional environment:
#   AICREW_BIN_DIR=...       where aicrew-agent.exe and aicrew.exe go
#   AICREW_VERSION=vX.Y.Z    install another release than $release
#   AICREW_REPO=owner/name   install from a fork
#   AICREW_NO_SERVICE=1      leave the members' services alone
$ErrorActionPreference = 'Stop'

# The release this script installs. Bumped together with the CHANGELOG
# when a release is cut (internal/installer checks they agree).
$release = 'v0.6.0'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$repo = if ($env:AICREW_REPO) { $env:AICREW_REPO } else { 'BlackVS/aicrew' }
$tag = if ($env:AICREW_VERSION) { $env:AICREW_VERSION } else { $release }
$dlBase = "https://github.com/$repo/releases/download/$tag"
$binDir = if ($env:AICREW_BIN_DIR) { $env:AICREW_BIN_DIR } else { Join-Path $env:LOCALAPPDATA 'aicrew\bin' }

# BEGIN install-agent
# Install-Agent: install aicrew-agent and the operator CLI aicrew, both
# $Tag, from $DlBase into $BinDir. A binary that already reports $Tag is
# left alone. Every download is checked against the release's SHA256SUMS in
# a temporary directory, all of them before any is installed: nothing in the
# bin directory changes unless every one passes.
# Get-AgentVersion lets `version` run to its end before reading its first
# line: cutting the pipeline short (Select-Object -First) abandons a process
# that has printed its line but not yet exited, and its exit code reads -1.
function Get-AgentVersion([string]$Exe) {
  try {
    $out = @(& $Exe version 2>$null)
    if ($LASTEXITCODE -eq 0 -and $out.Count -gt 0 -and $out[0]) { return ("$($out[0])" -split '\s+')[1] }
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
# The members' services (aicrew-agent service): stop, swap, start. The new
# aicrew-agent, still in the temporary directory, stops every registered
# home's service before the swap; the installed one starts them again after
# it, rewriting each definition with its own path. A home with no service,
# or AICREW_NO_SERVICE=1, is left alone. Stop-MemberServices answers whether
# any service was stopped, and so must be started again.
function Stop-MemberServices([string]$NewExe) {
  if ($env:AICREW_NO_SERVICE -eq '1') { return $false }
  $out = @(& $NewExe service stop --all)
  if ($LASTEXITCODE -ne 0) {
    Write-Warning "not every member's service stopped; the swap goes on, and each is started again after it."
  }
  $lines = @($out | Where-Object { $_ })
  if ($lines.Count -eq 0) { return $false }
  Write-Host 'stopped for the upgrade:'
  $lines | ForEach-Object { Write-Host $_ }
  return $true
}
function Start-MemberServices([string]$BinDir) {
  & (Join-Path $BinDir 'aicrew-agent.exe') service start --all | ForEach-Object { Write-Host $_ }
  if ($LASTEXITCODE -ne 0) {
    Write-Warning "not every member's service started; see: aicrew-agent service status --all"
  }
}
function Install-Agent([string]$Tag, [string]$DlBase, [string]$BinDir) {
  $todo = @()
  foreach ($name in @('aicrew-agent', 'aicrew')) {
    $exe = Join-Path $BinDir "$name.exe"
    if ((Test-Path $exe) -and ((Get-AgentVersion $exe) -eq $Tag)) {
      Write-Host "$name $Tag is current ($exe); nothing changed."
    } else {
      $todo += $name
    }
  }
  if ($todo.Count -eq 0) { return }
  $tmp = Join-Path ([IO.Path]::GetTempPath()) ('aicrew-agent-' + [Guid]::NewGuid().ToString('N'))
  New-Item -ItemType Directory $tmp | Out-Null
  try {
    $sums = Join-Path $tmp 'SHA256SUMS'
    try { Invoke-WebRequest "$DlBase/SHA256SUMS" -OutFile $sums -UseBasicParsing }
    catch { throw "cannot fetch the SHA256SUMS of $Tag; refusing an unverified binary. Nothing changed." }
    foreach ($name in $todo) {
      $asset = "$name-windows-amd64.exe"
      Write-Host "fetching $name $Tag ($asset)"
      $dl = Join-Path $tmp $asset
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
    }
    New-Item -ItemType Directory -Force $BinDir | Out-Null
    $stopped = $false
    if ($todo -contains 'aicrew-agent') { $stopped = Stop-MemberServices (Join-Path $tmp 'aicrew-agent-windows-amd64.exe') }
    foreach ($name in $todo) {
      $exe = Join-Path $BinDir "$name.exe"
      $dl = Join-Path $tmp "$name-windows-amd64.exe"
      $old = if (Test-Path $exe) { Get-AgentVersion $exe } else { '' }
      $new = Get-AgentVersion $dl
      # A running binary cannot be overwritten but can be renamed: the old
      # file steps aside, and a later run removes it.
      Get-ChildItem $BinDir -Filter "$name.exe.old-*" -ErrorAction SilentlyContinue |
        ForEach-Object { Remove-Item $_.FullName -Force -ErrorAction SilentlyContinue }
      $aside = ''
      if (Test-Path $exe) {
        $aside = Join-Path $BinDir ("$name.exe.old-" + (Get-Date).ToUniversalTime().ToString('yyyyMMddTHHmmssZ'))
        Rename-Item $exe (Split-Path $aside -Leaf)
      }
      try { Copy-Item $dl $exe }
      catch {
        if ($aside) { Rename-Item $aside "$name.exe" }
        if ($stopped) { Start-MemberServices $BinDir }
        throw "cannot write $exe ($($_.Exception.Message)); the previous $name is back."
      }
      $shownNew = if ($new) { $new } else { $Tag }
      if ($old) { Write-Host "upgraded $name $old -> $shownNew ($exe)" }
      else { Write-Host "installed $name $shownNew ($exe)" }
    }
    if ($stopped) { Start-MemberServices $BinDir }
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
