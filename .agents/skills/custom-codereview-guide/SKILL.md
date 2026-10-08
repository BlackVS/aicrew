---
name: custom-codereview-guide
description: aicrew's repository-specific review notes. The reviewer reads them before every review of this repository; they record runtime contracts observed at the real boundary, so a later review neither re-raises a settled hypothesis nor misses a known trap.
---

# aicrew review guide

Notes the `oh-code-review` skill reads before reviewing this repository.
Each entry is a contract observed at the real boundary: what was run, where,
and what it showed. A finding that rests on one of these facts cites the
entry instead of re-deriving it.

## Observed runtime contracts

### Syncing a pipe (observed 2026-10-04, PR #87)

`(*os.File).Sync` is for regular files only.

- **Linux** (Ubuntu 24.04 under WSL, Go 1.26.4): `Sync` on the write end of a
  pipe, including standard output when it is a pipe, returns `invalid
  argument` (EINVAL) after the write itself succeeded.
- **Windows 11** (Go 1.26.4): `Sync` on a pipe succeeds once the reader has
  drained it, and blocks indefinitely while nobody reads.

Probe: a standalone program creates `os.Pipe()`, writes one dummy line,
calls `Sync` with a 5 s guard, and prints both errors; no secret and no
service.

Consequence for review: code that writes a secret or any other value and
then treats a failed `Sync` as a failed delivery must sync only after
checking `Stat().Mode().IsRegular()`. Otherwise a value delivered through
a pipe is reported as lost, and whatever the failure path does (revoking a
just-issued credential, for example) runs on a delivery that succeeded.
`cmd/aicrew/secretout.go` (`syncRegular`) and its tests
`TestCredentialOutputRealPipe` and `TestCredentialOutputPipedProcess` hold
this rule.

### A failed Windows copy leaves no destination (observed 2026-10-08, PR #107)

`Copy-Item` (Windows PowerShell 5.1, Windows 11) that fails mid-way leaves
no destination file: the copy preallocates the destination at the full
length, and on the failure Windows removes it.

Probe (task 01a11bf2-5df1, a disposable temporary directory, no installed
agent, PATH or shared storage):
1. A 64 MiB source file. A second handle holds a byte-range lock on its
   second half, so the copy's reads there fail.
2. `aicrew-agent.exe` is renamed aside first, then `Copy-Item` copies the
   source to it, as `boot.ps1` swaps a running binary.
3. Another runspace polls the destination's size during the copy.

What it showed:
- the copy failed with `IOException` ("another process has locked a
  portion of the file");
- the destination had existed at its full 64 MiB during the copy;
- after the failure it did not exist;
- the rollback's `Rename-Item` of the old file back to
  `aicrew-agent.exe` succeeded, and the old content was in place.

Consequence for review: the hypothesis that a failed copy in `boot.ps1`
leaves a partial `aicrew-agent.exe` that blocks the rename back
(`boot.ps1`, the `catch` after `Copy-Item`) is REJECTED-RUNTIME. Do not add
a removal of the destination before the rename for it. A finding about
another failure kind must bring its own probe.
