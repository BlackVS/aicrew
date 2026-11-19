# aicrew-agent installer for Windows: installs or upgrades the member's
# aicrew-agent, and the operator CLI aicrew beside it, from any directory:
#
#   powershell -NoProfile -ExecutionPolicy Bypass -Command "irm https://raw.githubusercontent.com/BlackVS/aicrew/v0.11.1/boot.ps1 | iex"
#
# In a closed network, from an aiforge export (README.txt in the export has
# the commands with its tags filled in). From a directory or a share, copy
# boot.ps1 to a local folder and run the copy: Windows PowerShell refuses
# an unsigned script read straight from a share named by a dotted host name
# or an address under RemoteSigned. The copy runs inside the try, so a
# failed copy never runs one an earlier run left behind:
#
#   $env:AICREW_BASE = '\\files\aiforge-export'
#   $boot = Join-Path $env:TEMP 'aicrew-boot.ps1'
#   try {
#     Copy-Item -LiteralPath "$env:AICREW_BASE\aicrew\<tag>\boot.ps1" -Destination $boot -ErrorAction Stop
#     powershell -NoProfile -ExecutionPolicy RemoteSigned -File $boot
#   } finally { Remove-Item Env:AICREW_BASE; Remove-Item $boot -ErrorAction SilentlyContinue }
#
# From an internal https server:
#
#   $env:AICREW_BASE = 'https://files.example.com/aiforge'
#   irm https://files.example.com/aiforge/aicrew/<tag>/boot.ps1 | iex
#   Remove-Item Env:AICREW_BASE
#
# It installs the release this script was fetched from ($release below), or
# the one the export's MANIFEST names: aicrew-agent-windows-amd64.exe and
# aicrew-windows-amd64.exe, each checked against that release's SHA256SUMS,
# as aicrew-agent.exe and aicrew.exe in %LOCALAPPDATA%\aicrew\bin, and adds
# that directory to the user's PATH when it is missing. A binary that
# already reports the release is left as it is; an older one is replaced,
# and both versions are printed. A checksum mismatch installs neither. A
# running binary keeps its file: the old file is renamed aside
# (<name>.exe.old-<UTC time>) and removed by a later run once nothing uses
# it. It installs the binaries and nothing else: no agent home, no join, no
# client settings, no file outside the bin directory. After a first
# install, run `aicrew-agent join` (or rerun it for an existing home, so
# its Stop hook names this binary). A member home in turns mode runs as a
# service (aicrew-agent service): an upgrade stops every home's service
# before the swap and starts it again after.
#
# Optional environment:
#   AICREW_BIN_DIR=...       where aicrew-agent.exe and aicrew.exe go
#   AICREW_BASE=X:\dir|\\host\share|https://...
#                            install from an aiforge export instead of
#                            github.com: <base>\MANIFEST names the release,
#                            <base>\aicrew\<tag>\ holds its files; no other
#                            host is contacted
#   AICREW_VERSION=vX.Y.Z    install another release than $release (or than
#                            the export's MANIFEST names)
#   AICREW_REPO=owner/name   install from a fork (not read with AICREW_BASE)
#   AICREW_NO_SERVICE=1      leave the members' services alone
#
# The whole script is one script block, closed on its last line, so a
# download cut short does not parse and runs nothing.
& {
$ErrorActionPreference = 'Stop'

# The release this script installs. Bumped together with the CHANGELOG
# when a release is cut (internal/installer checks they agree).
$release = 'v0.11.1'

# BEGIN release-source
# Where the release's files come from, and their verification; the same
# rules as boot.sh's release-source block. Connected: the release download
# URL on github.com. With AICREW_BASE: an aiforge export, format 1 -
# <base>\MANIFEST is `aiforge-export 1` and then `<product> <tag>` lines,
# <base>\aicrew\<tag>\ holds the release's assets under their published
# names with its SHA256SUMS. An explicit AICREW_VERSION wins over MANIFEST.
# Every file is checked against SHA256SUMS before it is used; any failure
# stops with the path and changes nothing.
function Stop-Install($msg) { throw "$msg. Nothing changed." }
# Get-Sha256 hashes through .NET: Get-FileHash lives in a module that a
# powershell.exe started from PowerShell 7 may not find (it inherits
# PSModulePath).
function Get-Sha256([string]$Path) {
  $sha = [Security.Cryptography.SHA256]::Create()
  $f = [IO.File]::OpenRead($Path)
  try { return (($sha.ComputeHash($f) | ForEach-Object { $_.ToString('x2') }) -join '') }
  finally { $f.Dispose(); $sha.Dispose() }
}
# Windows PowerShell 5.1 follows a redirect from https to http and
# downloads over http; PowerShell 7 refuses one (observed by aimem, see
# its custom-codereview-guide). A release download is itself redirected
# by the forge, so redirects are followed here, one hop at a time, and
# only to https. Under 5.1 -MaximumRedirection 0 returns the 3xx only
# with -ErrorAction SilentlyContinue; under 7 only with -SkipHttpErrorCheck.
function Get-HttpsFile($url, $dest) {
  $extra = @{}
  if ($PSVersionTable.PSEdition -eq 'Core') { $extra.SkipHttpErrorCheck = $true }
  for ($hop = 0; $hop -le 10; $hop++) {
    if ($url -notlike 'https://*') { Stop-Install "refusing a redirect to $url, which is not https" }
    $r = Invoke-WebRequest $url -OutFile $dest -PassThru -UseBasicParsing -MaximumRedirection 0 -ErrorAction SilentlyContinue -ErrorVariable failed @extra
    if ($null -eq $r) { throw "$failed" }
    $code = [int]$r.StatusCode
    if ($code -ge 300 -and $code -lt 400) {
      $url = [Uri]::new([Uri]$url, "$($r.Headers['Location'])").AbsoluteUri
      continue
    }
    if ($code -ne 200) { throw "HTTP $code from $url" }
    return
  }
  throw "more than 10 redirects from $url"
}
# Get-SourceFile: an https download, or a copy of a local file; $false when
# the file cannot be read. A refused redirect stops the install.
function Get-SourceFile($src, $dest) {
  if ($src -like 'https://*') {
    try { Get-HttpsFile $src $dest } catch {
      if ("$_" -like 'refusing a redirect*') { throw }
      Write-Warning "$src : $_"; return $false
    }
    return $true
  }
  if (-not (Test-Path -LiteralPath $src -PathType Leaf)) { return $false }
  Copy-Item -LiteralPath $src -Destination $dest
  return $true
}
function Test-Tag($t) { return ($t -cmatch '^v[0-9][A-Za-z0-9._+-]*$') -and ($t -notlike '*..*') }
# Resolve-Source: the release's tag and the directory or URL holding its
# files, from AICREW_BASE, AICREW_VERSION, AICREW_REPO and $Release.
function Resolve-Source([string]$Work, [string]$Release) {
  if (-not $env:AICREW_BASE) {
    $t = if ($env:AICREW_VERSION) { $env:AICREW_VERSION } else { $Release }
    if (-not (Test-Tag $t)) { Stop-Install "'$t' is not a release tag" }
    $repo = if ($env:AICREW_REPO) { $env:AICREW_REPO } else { 'BlackVS/aicrew' }
    return @{ Tag = $t; Src = "https://github.com/$repo/releases/download/$t" }
  }
  $b = $env:AICREW_BASE
  if (-not ($b -like 'https://*' -or $b -match '^[A-Za-z]:[\\/]' -or $b -match '^\\\\[^\\]')) {
    Stop-Install "AICREW_BASE must be an absolute directory, a \\host\share path or an https:// URL, not '$b'"
  }
  $b = $b.TrimEnd('/', '\')
  if ($env:AICREW_VERSION) {
    $t = $env:AICREW_VERSION
  } else {
    $m = Join-Path $Work 'MANIFEST'
    if (-not (Get-SourceFile "$b/MANIFEST" $m)) {
      Stop-Install "$b/MANIFEST is missing (or set AICREW_VERSION to name the release)"
    }
    $lines = @(Get-Content -LiteralPath $m)
    $format = if ($lines.Count) { $lines[0].Trim() } else { '' }
    if ($format -cne 'aiforge-export 1') {
      Stop-Install "$b/MANIFEST is '$format', not a format this installer reads (aiforge-export 1)"
    }
    $t = $lines | Select-Object -Skip 1 | ForEach-Object { , (-split $_) } |
      Where-Object { $_.Count -ge 2 -and $_[0] -ceq 'aicrew' } |
      ForEach-Object { $_[1] } | Select-Object -First 1
    if (-not $t) { Stop-Install "$b/MANIFEST names no aicrew release" }
  }
  if (-not (Test-Tag $t)) { Stop-Install "'$t' is not a release tag" }
  return @{ Tag = $t; Src = "$b/aicrew/$t" }
}
# Stop-Missing: the refusal for a file that could not be read.
function Stop-Missing([string]$Path, [string]$Tag) {
  if ($env:AICREW_BASE) { Stop-Install "$Path is missing: the export holds an incomplete copy of aicrew $Tag" }
  Stop-Install "cannot fetch $Path"
}
# Get-Verified: the asset from $Src into $Work, checked against
# $Work\SHA256SUMS on its whole name; its path.
function Get-Verified([string]$Work, [string]$Src, [string]$Asset, [string]$Tag) {
  $out = Join-Path $Work $Asset
  if (-not (Get-SourceFile "$Src/$Asset" $out)) { Stop-Missing "$Src/$Asset" $Tag }
  $want = Get-Content -LiteralPath (Join-Path $Work 'SHA256SUMS') | ForEach-Object { , (-split $_) } |
    Where-Object { $_.Count -ge 2 -and ($_[1] -ceq $Asset -or $_[1] -ceq "*$Asset") } |
    ForEach-Object { $_[0].ToLower() } | Select-Object -First 1
  $got = Get-Sha256 $out
  if (-not $want -or $want -ne $got) {
    $shown = if ($want) { $want } else { 'absent' }
    Stop-Install "checksum mismatch for $Src/$Asset (want $shown, got $got); refusing it"
  }
  Write-Host "checksum OK: $Asset"
  return $out
}
# END release-source

# BEGIN install-agent
# Install-Agent: install aicrew-agent and the operator CLI aicrew, both
# $Tag, from $Src (release-source) into $BinDir. A binary that already
# reports $Tag is left alone. Every download is checked against the
# release's SHA256SUMS in a temporary directory, all of them before any is
# installed: nothing in the bin directory changes unless every one passes.
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
function Install-Agent([string]$Tag, [string]$Src, [string]$BinDir) {
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
    if (-not (Get-SourceFile "$Src/SHA256SUMS" (Join-Path $tmp 'SHA256SUMS'))) {
      Stop-Missing "$Src/SHA256SUMS" $Tag
    }
    foreach ($name in $todo) {
      $asset = "$name-windows-amd64.exe"
      Write-Host "fetching $name $Tag ($asset)"
      Get-Verified $tmp $Src $asset $Tag | Out-Null
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

$binDir = if ($env:AICREW_BIN_DIR) { $env:AICREW_BIN_DIR } else { Join-Path $env:LOCALAPPDATA 'aicrew\bin' }
$work = Join-Path ([IO.Path]::GetTempPath()) ('aicrew-boot-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory $work | Out-Null
$protocol = [Net.ServicePointManager]::SecurityProtocol
try {
  # Process-wide, so the finally puts it back too: run as `irm | iex` this
  # runs in the caller's window.
  [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
  $source = Resolve-Source $work $release
  if ($env:AICREW_BASE) { Write-Host "installing aicrew $($source.Tag) from $($source.Src)" }
  Install-Agent $source.Tag $source.Src $binDir
} finally {
  [Net.ServicePointManager]::SecurityProtocol = $protocol
  Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
}
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
$parts = if ($userPath) { $userPath -split ';' } else { @() }
if (-not ($parts | Where-Object { $_.TrimEnd('\') -ieq $binDir.TrimEnd('\') })) {
  $joined = if ($userPath) { $userPath.TrimEnd(';') + ';' + $binDir } else { $binDir }
  [Environment]::SetEnvironmentVariable('Path', $joined, 'User')
  Write-Host "added $binDir to your PATH; open a new terminal to use it."
}
}
