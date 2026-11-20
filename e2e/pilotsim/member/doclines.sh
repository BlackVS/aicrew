#!/bin/sh
# doclines.sh IN RAN ID...: runs each docs/BRING-UP.md line the runner wrote
# as IN/ID.line, in order, with sh and no terminal, and records ID in RAN
# after its line succeeded. A missing or empty line file is the runner's
# fault, never a step run: it stops before anything is recorded for it, as
# a failed line does (e2e/docsteps, docstep verify).
set -eu
in=$1 ran=$2
shift 2
for id do
  line=$(cat "$in/$id.line") && [ -n "$line" ] ||
    { echo "doclines.sh: no command line for step $id in $in" >&2; exit 1; }
  sh -c "$line" < /dev/null
  echo "$id" >> "$ran"
done
