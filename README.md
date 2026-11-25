# aicrew
AI Crew team management

## Installing

Each release's one-liners install it and upgrade to it; docs/DEVELOPMENT.md
describes what they do and their knobs.

- A member's or the operator's machine, Linux and macOS:

  ```sh
  curl -fsSL https://raw.githubusercontent.com/BlackVS/aicrew/v0.11.5/boot.sh | bash
  ```

- Windows, in a PowerShell window that is already open:

  ```powershell
  irm https://raw.githubusercontent.com/BlackVS/aicrew/v0.11.5/boot.ps1 | iex
  ```

  From cmd, or from the Run dialog (Win+R):

  ```bat
  powershell -NoProfile -ExecutionPolicy Bypass -Command "irm https://raw.githubusercontent.com/BlackVS/aicrew/v0.11.5/boot.ps1 | iex"
  ```

- The aicrewd service on a Debian or Ubuntu hub host, as root:

  ```sh
  curl -fsSL https://raw.githubusercontent.com/BlackVS/aicrew/v0.11.5/install-aicrewd.sh | bash
  ```

### In a closed network

A closed network installs from an aiforge export, copied in or served from
an internal https server, which names no other host. Its `README.txt` has
these commands with its tags filled in, and docs/DEVELOPMENT.md, "Installing
in a closed network", the details.

- From a directory, on Linux and macOS (the hub host's `install-aicrewd.sh`
  likewise, as root):

  ```sh
  AICREW_BASE=/media/aiforge-export bash /media/aiforge-export/aicrew/<tag>/boot.sh
  ```

- From an https server:

  ```sh
  curl -fsSL https://files.example.com/aiforge/aicrew/<tag>/boot.sh | AICREW_BASE=https://files.example.com/aiforge bash
  ```

- Windows, from a directory or a share: copy `boot.ps1` to a local folder
  and run the copy.

  ```powershell
  $env:AICREW_BASE = '\\files\aiforge-export'
  $boot = Join-Path $env:TEMP 'aicrew-boot.ps1'
  try {
    Copy-Item -LiteralPath "$env:AICREW_BASE\aicrew\<tag>\boot.ps1" -Destination $boot -ErrorAction Stop
    powershell -NoProfile -ExecutionPolicy RemoteSigned -File $boot
  } finally { Remove-Item Env:AICREW_BASE; Remove-Item $boot -ErrorAction SilentlyContinue }
  ```

- Windows, from an https server, in an open PowerShell window:

  ```powershell
  $env:AICREW_BASE = 'https://files.example.com/aiforge'
  irm https://files.example.com/aiforge/aicrew/<tag>/boot.ps1 | iex
  Remove-Item Env:AICREW_BASE
  ```

The forms above set `AICREW_BASE` for the installer only. Join each member
home with it set to the same export, so that the join records it and
`aicrew-agent check` names the export, not github.com, in what it tells
a member to install (aimem's boot script; skills always come from the
hub):

```sh
AICREW_BASE=/media/aiforge-export aicrew-agent join --label builder --url https://aicrew.example:8443 ...
```

On Windows, set `$env:AICREW_BASE` before `aicrew-agent join` and remove
it afterwards, as in the forms above.
