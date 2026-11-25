# Sets up a local OpenSSH server on a Windows CI runner for step 8's
# carrying line (docs/BRING-UP.md, "The team's architect"): the server
# stands in for aicrewd's host, the runner's user is the login, and its
# shell is Windows PowerShell, whose `rm` the line's second command uses
# as a Linux host's would. Run in PowerShell 7 (pwsh), as the runner's
# administrator. It writes the target to $GITHUB_ENV as
# AICREW_CI_SSH_HOST, which TestWindowsArchitectCarry (e2e/docsteps) reads.
#
# The runner is a throwaway machine: this installs the OpenSSH server
# capability, authorizes a key made here for the runner's user, and
# writes the machine's ssh_config for localhost. Nothing else is changed.
$ErrorActionPreference = 'Stop'

$cap = Get-WindowsCapability -Online -Name 'OpenSSH.Server*' | Select-Object -First 1
if (-not $cap) { throw 'no OpenSSH.Server capability on this runner' }
Write-Output "ci-windows-sshd: $($cap.Name) is $($cap.State)"
if ($cap.State -ne 'Installed') {
  Add-WindowsCapability -Online -Name $cap.Name | Out-Null
}

$sshdir = Join-Path $env:ProgramData 'ssh'
$key = Join-Path $env:RUNNER_TEMP 'ci-architect-carry'
ssh-keygen -q -t ed25519 -N '' -f $key
if ($LASTEXITCODE -ne 0) { throw 'ssh-keygen failed' }

# The runner's user is an administrator, whose keys sshd reads from
# administrators_authorized_keys, readable by Administrators and SYSTEM only.
New-Item -ItemType Directory -Force $sshdir | Out-Null
$auth = Join-Path $sshdir 'administrators_authorized_keys'
Copy-Item "$key.pub" $auth -Force
icacls $auth /inheritance:r /grant '*S-1-5-32-544:F' /grant '*S-1-5-18:F' | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'icacls on administrators_authorized_keys failed' }

New-Item -Path 'HKLM:\SOFTWARE\OpenSSH' -Force | Out-Null
New-ItemProperty -Path 'HKLM:\SOFTWARE\OpenSSH' -Name DefaultShell -PropertyType String -Force `
  -Value (Join-Path $env:WINDIR 'System32\WindowsPowerShell\v1.0\powershell.exe') | Out-Null
Start-Service sshd

# The client side, for every user of the machine: the key, and no prompt
# for the new host key.
@"
Host localhost
  IdentityFile $key
  StrictHostKeyChecking no
  UserKnownHostsFile NUL
  BatchMode yes
"@ | Set-Content -Encoding ascii (Join-Path $sshdir 'ssh_config')

$target = "$env:USERNAME@localhost"
$probe = ssh $target 'Write-Output carry-ok'
if ($LASTEXITCODE -ne 0 -or "$probe".Trim() -ne 'carry-ok') { throw "ssh $target answered: $probe" }
Write-Output "ci-windows-sshd: $target answers"
"AICREW_CI_SSH_HOST=$target" | Out-File -Append -Encoding utf8 $env:GITHUB_ENV
