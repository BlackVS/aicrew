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
