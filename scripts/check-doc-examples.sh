#!/bin/sh
# Refuses a stale name in the documentation's examples.
#
# An example that names a renamed or removed command, flag, route or
# automation builtin fails for every reader who copies it. TestDocExamples
# in docs/ compares each such name in the operational documents and the
# agent home's managed documents with the code: the cmd/ sources of aicrew,
# aicrew-agent and aicrewd, internal/server/routes.txt and the automation
# host API. docs/DEVELOPMENT.md, "Examples in the documentation", states
# what it reads and what it skips.
set -eu
cd "$(dirname "$0")/.."

go test -count=1 -run '^TestDocExamples' ./docs/
