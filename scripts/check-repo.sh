#!/usr/bin/env bash
# Repository hygiene checks. CI runs this script; run it locally before a push.
# Checks the Git index (what would be committed), not untracked files.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
fail=0

# Whitespace errors (trailing spaces, space before tab, conflict markers)
# across every staged file, compared with the empty tree.
empty_tree=$(git hash-object -t tree /dev/null)
if ! git diff --cached --check "$empty_tree" --; then
  echo "error: whitespace errors in tracked files" >&2
  fail=1
fi

# Tracked files that the ignore rules exclude, e.g. credentials, local
# bindings, handoff files or build output that were added by force.
ignored=$(git ls-files --cached --ignored --exclude-standard)
if [ -n "$ignored" ]; then
  echo "error: tracked files match ignore rules:" >&2
  echo "$ignored" >&2
  fail=1
fi

# UTF-8 byte order marks break shell, JSON and YAML consumers.
bom=$(git grep --cached -lI $'^\xEF\xBB\xBF' -- . || true)
if [ -n "$bom" ]; then
  echo "error: files start with a UTF-8 BOM:" >&2
  echo "$bom" >&2
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "check-repo: ok"
