#!/usr/bin/env bash
# Tests of scripts/release.sh's refusals and notes against scratch
# repositories, with a stand-in gh. CI runs it; it needs only git and bash.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
script="$here/release.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
pass=0
failures=0

# A stand-in gh: `release view TAG` succeeds only for tags in $GH_RELEASES.
mkdir "$work/bin"
cat > "$work/bin/gh" <<'EOF'
#!/usr/bin/env bash
if [ "$1 $2" = "release view" ]; then
  case " ${GH_RELEASES:-} " in *" $3 "*) exit 0 ;; esac
  exit 1
fi
echo "stand-in gh: unexpected $*" >&2; exit 2
EOF
chmod +x "$work/bin/gh"
export PATH="$work/bin:$PATH"

# A scratch origin and clone: main has a CHANGELOG with [1.0.0] and tag
# v1.0.0, a tag without a section (v1.1.0), and a tag off main (v2.0.0).
git init -q --bare "$work/origin.git"
git clone -q "$work/origin.git" "$work/repo" 2>/dev/null
cd "$work/repo"
git config user.email t@example.invalid; git config user.name t
git checkout -q -b main
printf 'Required Notice: Copyright (c) example\n\nterms\n' > LICENSE
printf '# Changelog\n\n## [Unreleased]\n\n## [1.0.0] - 2026-10-01\n\n- first release\n' > CHANGELOG.md
git add . && git commit -q -m one && git tag v1.0.0
echo two > two && git add two && git commit -q -m two && git tag v1.1.0
git push -q origin main --tags
git checkout -q -b side && echo side > side && git add side && git commit -q -m side && git tag v2.0.0
git push -q origin side v2.0.0
git checkout -q main

expect() { # expect ok|fail NAME MESSAGE-PART -- COMMAND...
  local want=$1 name=$2 part=$3; shift 4
  local out code=0
  out=$("$@" 2>&1) || code=$?
  if { [ "$want" = ok ] && [ $code -eq 0 ]; } || { [ "$want" = fail ] && [ $code -ne 0 ]; }; then
    if [ -n "$part" ] && ! grep -qF -- "$part" <<<"$out"; then
      echo "FAIL $name: output lacks '$part': $out"; failures=$((failures + 1)); return
    fi
    pass=$((pass + 1))
  else
    echo "FAIL $name: exit $code: $out"; failures=$((failures + 1))
  fi
}

expect ok   "a released commit on main"       "checked (dry run: false)" -- bash "$script" check v1.0.0
expect fail "not vX.Y.Z"                      "is not vMAJOR.MINOR.PATCH" -- bash "$script" check 1.0.0
expect fail "a pre-release"                   "is not vMAJOR.MINOR.PATCH" -- bash "$script" check v1.0.0-rc.1
expect fail "a short version"                 "is not vMAJOR.MINOR.PATCH" -- bash "$script" check v1.0
expect fail "a tag off main"                  "which is not on main"      -- bash "$script" check v2.0.0
expect fail "no CHANGELOG section"            "has no [1.1.0] section"    -- bash "$script" check v1.1.0
expect ok   "no section, dry run"             "a dry run goes on"         -- bash "$script" check v1.1.0 --dry-run
expect fail "a tag that does not exist"       "never creates one"         -- bash "$script" check v3.0.0
expect ok   "a missing tag, dry run"          "a dry run of main's tip"   -- bash "$script" check v3.0.0 --dry-run
expect fail "the release exists"              "already exists"            -- env GH_RELEASES=v1.0.0 bash "$script" check v1.0.0
expect ok   "the release exists, dry run"     ""                          -- env GH_RELEASES=v1.0.0 bash "$script" check v1.0.0 --dry-run
git -c advice.detachedHead=false checkout -q v1.0.0
expect ok   "notes"                           "release notes written"     -- bash "$script" notes v1.0.0
grep -q '^- first release$' release_notes.md && grep -q '^Required Notice: Copyright (c) example$' release_notes.md &&
  grep -q 'polyformproject.org/licenses/noncommercial/1.0.0' release_notes.md && pass=$((pass + 1)) ||
  { echo "FAIL notes content: $(cat release_notes.md)"; failures=$((failures + 1)); }
expect fail "notes without a section"         "has no [1.1.0] section"    -- bash "$script" notes v1.1.0
expect ok   "notes without a section, dry run" ""                         -- bash "$script" notes v1.1.0 --dry-run

# resolve: a pushed tag publishes; a manual run comes from main.
envf=$work/env
expect ok   "resolve a pushed tag"            "" -- env GITHUB_ENV="$envf" GITHUB_EVENT_NAME=push GITHUB_REF_NAME=v1.2.3 bash "$script" resolve
grep -qx 'TAG=v1.2.3' "$envf" && grep -qx 'DRY_RUN=false' "$envf" && pass=$((pass + 1)) || { echo "FAIL resolve output: $(cat "$envf")"; failures=$((failures + 1)); }
: > "$envf"
expect ok   "resolve a manual dry run"        "" -- env GITHUB_ENV="$envf" GITHUB_EVENT_NAME=workflow_dispatch GITHUB_REF=refs/heads/main INPUT_TAG=1.2.3 INPUT_DRY_RUN=true bash "$script" resolve
grep -qx 'TAG=v1.2.3' "$envf" && grep -qx 'DRY_RUN=true' "$envf" && pass=$((pass + 1)) || { echo "FAIL resolve dispatch output: $(cat "$envf")"; failures=$((failures + 1)); }
expect fail "a manual run off main"           "from main" -- env GITHUB_EVENT_NAME=workflow_dispatch GITHUB_REF=refs/heads/side INPUT_TAG=v1.2.3 bash "$script" resolve
expect fail "a bad manual tag"                "is not vMAJOR.MINOR.PATCH" -- env GITHUB_EVENT_NAME=workflow_dispatch GITHUB_REF=refs/heads/main INPUT_TAG='v1.2.3
x' bash "$script" resolve
expect fail "a pushed non-release tag"        "is not vMAJOR.MINOR.PATCH" -- env GITHUB_EVENT_NAME=push GITHUB_REF_NAME=latest bash "$script" resolve

echo "release_test: $pass passed, $failures failed"
[ "$failures" -eq 0 ]
