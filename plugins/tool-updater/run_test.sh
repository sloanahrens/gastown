#!/usr/bin/env bash
# Tests for plugins/tool-updater/run.sh (gt-th5it): a stub brew on PATH serves
# canned `outdated --json=v2` output and records any upgrade or install call, so
# the outdated-report branch, both fail-closed branches and the stale-index
# branch are exercised without touching the real Homebrew or the town database
# (a stub gt swallows the run receipt). Run directly or via the shell tier:
# scripts/tier-sweep.sh discovers it as a `*_test.sh` script (gt-6ox58).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
RUN="$SCRIPT_DIR/run.sh"
BASH_BIN="$(command -v bash)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
PASS=0
FAIL=0

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; FAIL=$((FAIL + 1)); }

# --- Stub brew and gt ---------------------------------------------------------
# brew serves BREW_OUTDATED_JSON for `outdated --json=v2` and BREW_UPDATE_RC for
# `update`; an upgrade or install is a test failure, so it is recorded and exits
# 99 (run.sh has no such call today, and the log is the guard that it never
# gains one).
mkdir -p "$TMP/bin"
cat >"$TMP/bin/brew" <<'STUB'
#!/usr/bin/env bash
case "${1:-}" in
  update) exit "${BREW_UPDATE_RC:-0}" ;;
  outdated)
    [[ "${2:-}" == "--json=v2" ]] || exit 2
    printf '%s' "${BREW_OUTDATED_JSON:-}"
    exit "${BREW_OUTDATED_RC:-0}" ;;
  upgrade|install)
    printf '%s\n' "$*" >>"${BREW_MUTATION_LOG:?}"
    exit 99 ;;
  *) exit 2 ;;
esac
STUB
chmod +x "$TMP/bin/brew"
# The real gt would write a plugin-run receipt to the town database; the stub
# keeps the test hermetic.
cat >"$TMP/bin/gt" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "$TMP/bin/gt"

MUTATIONS="$TMP/mutations.log"

# run_script JSON OUT runs run.sh with the stub on PATH; rc holds its exit code.
run_script() {
  local json="$1" out="$2"
  rc=0
  PATH="$TMP/bin:$PATH" \
  BREW_MUTATION_LOG="$MUTATIONS" \
  BREW_OUTDATED_JSON="$json" \
  BREW_OUTDATED_RC="${BREW_OUTDATED_RC:-0}" \
  BREW_UPDATE_RC="${BREW_UPDATE_RC:-0}" \
    "$BASH_BIN" "$RUN" >"$out" 2>&1 || rc=$?
}
# Check no upgrade or install reached the stub.
no_mutation() {
  if [[ -s "$MUTATIONS" ]]; then
    fail "$1 (mutations: $(cat "$MUTATIONS"))"
  else
    pass "$1"
  fi
}

OUTDATED_BOTH='{"formulae":[
  {"name":"dolt","installed_versions":["2.3.2"],"current_version":"2.4.1"},
  {"name":"beads","installed_versions":["1.2.2"],"current_version":"1.3.1"}]}'
OUTDATED_BEADS='{"formulae":[
  {"name":"beads","installed_versions":["1.2.2"],"current_version":"1.3.1"}]}'
CURRENT='{"formulae":[]}'
NOT_A_LIST='{"formulae":{}}'

echo "tool-updater: outdated branch reports each tool and mutates nothing"
BREW_OUTDATED_RC=0 BREW_UPDATE_RC=0 run_script "$OUTDATED_BOTH" "$TMP/out"
if grep -qF "dolt: update available (installed: 2.3.2, available: 2.4.1)" "$TMP/out" \
   && grep -qF "beads: update available (installed: 1.2.2, available: 1.3.1)" "$TMP/out"; then
  pass "one line per outdated tool with installed and available"
else
  fail "one line per outdated tool with installed and available ($(cat "$TMP/out"))"
fi
if [[ "$rc" == 0 ]] && grep -qF "outdated=2 (report-only)" "$TMP/out"; then
  pass "exit 0 and a summary naming both tools"
else
  fail "exit 0 and a summary naming both tools (rc=$rc)"
fi
no_mutation "no brew upgrade or install called"

echo "tool-updater: up-to-date branch"
BREW_OUTDATED_RC=0 BREW_UPDATE_RC=0 run_script "$CURRENT" "$TMP/out"
if [[ "$rc" == 0 ]] && grep -qF "dolt: up to date" "$TMP/out" \
   && grep -qF "beads: up to date" "$TMP/out" \
   && grep -qF "all tools current" "$TMP/out"; then
  pass "exit 0 and all tools current"
else
  fail "exit 0 and all tools current (rc=$rc, $(cat "$TMP/out"))"
fi

echo "tool-updater: a non-actionable tool still blocks the all-clear (gt-th5it)"
BREW_OUTDATED_RC=0 BREW_UPDATE_RC=0 run_script "$OUTDATED_BEADS" "$TMP/out"
if [[ "$rc" == 0 ]] && grep -qF "outdated=1 (report-only)" "$TMP/out" \
   && ! grep -qF "all tools current" "$TMP/out"; then
  pass "beads outdated: summary says outdated=1, never all current"
else
  fail "beads outdated: summary says outdated=1, never all current (rc=$rc, $(cat "$TMP/out"))"
fi

echo "tool-updater: a failed probe fails closed"
BREW_OUTDATED_RC=1 BREW_UPDATE_RC=0 run_script "$CURRENT" "$TMP/out"
if [[ "$rc" == 1 ]] && grep -qF "ERROR" "$TMP/out" \
   && ! grep -qF "all tools current" "$TMP/out"; then
  pass "brew outdated failure: exit 1, error, no all-clear"
else
  fail "brew outdated failure: exit 1, error, no all-clear (rc=$rc, $(cat "$TMP/out"))"
fi
for shape in "$NOT_A_LIST" 'not json'; do
  BREW_OUTDATED_RC=0 BREW_UPDATE_RC=0 run_script "$shape" "$TMP/out"
  if [[ "$rc" == 1 ]] && grep -qF "ERROR" "$TMP/out" \
     && ! grep -qF "all tools current" "$TMP/out"; then
    pass "unreadable probe output ($shape): exit 1, error, no all-clear"
  else
    fail "unreadable probe output ($shape): exit 1, error, no all-clear (rc=$rc, $(cat "$TMP/out"))"
  fi
done

echo "tool-updater: brew missing fails closed"
mkdir -p "$TMP/nobrew"
rc=0
PATH="$TMP/nobrew" BREW_MUTATION_LOG="$MUTATIONS" \
  "$BASH_BIN" "$RUN" >"$TMP/out" 2>&1 || rc=$?
if [[ "$rc" == 1 ]] && grep -qF "ERROR" "$TMP/out"; then
  pass "no brew on PATH: exit 1 and an error"
else
  fail "no brew on PATH: exit 1 and an error (rc=$rc, $(cat "$TMP/out"))"
fi

echo "tool-updater: a failed index refresh is surfaced, not swallowed (gt-th5it)"
BREW_OUTDATED_RC=0 BREW_UPDATE_RC=1 run_script "$CURRENT" "$TMP/out"
if [[ "$rc" == 0 ]] && grep -qF "WARNING: brew update failed" "$TMP/out" \
   && grep -qF "formula index stale" "$TMP/out"; then
  pass "brew update failure warns and marks the summary stale"
else
  fail "brew update failure warns and marks the summary stale (rc=$rc, $(cat "$TMP/out"))"
fi

echo "tool-updater: $PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]]
