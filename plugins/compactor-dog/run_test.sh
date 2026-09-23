#!/usr/bin/env bash
# Tests for compactor-dog/run.sh helper functions.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
FAILURES=0

# Source just the helper functions from run.sh by extracting them.
# We can't source the whole script (it runs immediately), so redefine here.
log() { echo "[test] $*"; }

# --- Copy validate_hash from run.sh (must stay in sync) ---
validate_hash() {
  local hash="$1"
  local context="$2"
  if [[ ! "$hash" =~ ^[a-v0-9]+$ ]]; then
    log "ERROR: Unsafe $context hash rejected: '$hash'"
    return 1
  fi
  return 0
}

# Verify our copy matches run.sh (guard against drift).
# Extract the regex from each file's validate_hash function with POSIX sed:
# grep -oP is GNU-only and fails on macOS/BSD grep, which is where the dog runs.
extract_hash_regex() {
  sed -n '/^validate_hash/,/^}/p' "$1" | sed -n 's/.*=~ \(.*\) \]\];.*/\1/p'
}
RUN_SH_REGEX=$(extract_hash_regex "$SCRIPT_DIR/run.sh")
TEST_REGEX=$(extract_hash_regex "$0")
if [[ -z "$RUN_SH_REGEX" || -z "$TEST_REGEX" ]]; then
  echo "FAIL: could not extract validate_hash regex (run.sh='$RUN_SH_REGEX' test='$TEST_REGEX')"
  echo "      Did the 'if [[ ! \"\$hash\" =~ ... ]]' line change shape?"
  exit 1
fi
if [[ "$RUN_SH_REGEX" != "$TEST_REGEX" ]]; then
  echo "FAIL: validate_hash regex in test ($TEST_REGEX) doesn't match run.sh ($RUN_SH_REGEX)"
  echo "      Update the test to match run.sh"
  exit 1
fi

# Verify compaction is not the default (gt-e14c). The destructive path
# (flatten + force-push) must require an explicit --compact flag; a plain
# `bash run.sh` has to stay monitor-only.
if ! grep -q '^CHECK_ONLY=true' "$SCRIPT_DIR/run.sh"; then
  echo "FAIL: run.sh no longer defaults CHECK_ONLY=true — destructive compaction is the default"
  FAILURES=$((FAILURES + 1))
fi
FALSE_SETTERS=$(grep -c 'CHECK_ONLY=false' "$SCRIPT_DIR/run.sh" || true)
COMPACT_FLAG=$(grep -c -- '--compact).*CHECK_ONLY=false' "$SCRIPT_DIR/run.sh" || true)
if [[ "$FALSE_SETTERS" != "$COMPACT_FLAG" ]]; then
  echo "FAIL: run.sh clears CHECK_ONLY on $FALSE_SETTERS line(s) but only $COMPACT_FLAG is the --compact flag"
  FAILURES=$((FAILURES + 1))
fi

assert_valid() {
  local hash="$1"
  if ! validate_hash "$hash" "test" >/dev/null 2>&1; then
    echo "FAIL: expected valid hash: '$hash'"
    FAILURES=$((FAILURES + 1))
  fi
}

assert_invalid() {
  local hash="$1"
  if validate_hash "$hash" "test" >/dev/null 2>&1; then
    echo "FAIL: expected invalid hash: '$hash'"
    FAILURES=$((FAILURES + 1))
  fi
}

# --- Tests ---

echo "=== validate_hash tests ==="

# Dolt base32 hashes (real examples)
assert_valid "aecqtmbdbabpalqnamq8atfv86ehjf7r"
assert_valid "0123456789abcdefghijklmnopqrstuv"
assert_valid "abc123"
assert_valid "00000000"

# Hex-only hashes should still pass (subset of base32)
assert_valid "deadbeef"
assert_valid "abcdef0123456789"

# Invalid: characters outside base32 range
assert_invalid "xyz"
assert_invalid "ABCDEF"
assert_invalid "hash-with-dashes"
assert_invalid "hash_with_underscores"
assert_invalid "hash with spaces"
assert_invalid ""
assert_invalid "../../../etc/passwd"
assert_invalid "'; DROP TABLE issues; --"

# --- Last-run check finds existing receipts (gt-idwq) ---
#
# Receipts are ephemeral wisps (`gt plugin record-run` -> bd create
# --ephemeral). A real `bd list` hides ephemeral beads, so the query in
# plugin.md returns [] and reports "never" unless it passes --include-infra.
# The fake bd below reproduces that filtering, with a receipt on hand: the
# query has to find it.
echo ""
echo "=== last-run query tests ==="

PLUGIN_MD="$SCRIPT_DIR/plugin.md"
QUERY_OK=true

# Extract the shipped query instead of copying it, so plugin.md stays the
# single source of truth. Breaking its shape fails the test rather than
# silently testing a stale copy.
LAST_RUN_QUERY=$(sed -n '/^RECENT_RUNS=/,/^  | jq/p' "$PLUGIN_MD" || true)
QUERY_LINES=$(printf '%s\n' "$LAST_RUN_QUERY" | grep -c . || true)
if [[ "$QUERY_LINES" != "2" || "$LAST_RUN_QUERY" != *"jq"* ]]; then
  echo "FAIL: expected a 2-line 'RECENT_RUNS=(bd list ... | jq' query in $PLUGIN_MD,"
  echo "      got $QUERY_LINES line(s). Did the last-run check change shape?"
  echo "      (A sed range that misses its end pattern runs to EOF, so the shape"
  echo "      check also keeps this test from eval'ing the rest of the file.)"
  QUERY_OK=false
  FAILURES=$((FAILURES + 1))
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "SKIP: jq not installed (the query pipes through jq)"
elif $QUERY_OK; then
  FAKE_DIR=$(mktemp -d)
  trap 'rm -rf "${FAKE_DIR:-}"' EXIT

  # Run the extracted query under whichever fake bd is installed in FAKE_DIR.
  # The subshell keeps PATH changes from leaking into later tests.
  run_last_run_query() {
    (
      export PATH="$FAKE_DIR:$PATH"
      export FAKE_RECEIPT="$1"
      RECENT_RUNS=""
      eval "$LAST_RUN_QUERY"
      printf '%s' "$RECENT_RUNS"
    )
  }

  write_fake_bd() {
    cat > "$FAKE_DIR/bd"
    chmod +x "$FAKE_DIR/bd"
  }

  # Receipt visible only when --include-infra is passed, as real bd behaves.
  write_fake_bd <<'FAKE_BD'
#!/usr/bin/env bash
for arg in "$@"; do
  if [[ "$arg" == "--include-infra" ]]; then
    printf '%s\n' "$FAKE_RECEIPT"
    exit 0
  fi
done
printf '[]\n'
FAKE_BD

  RECEIPT='[{"id":"hq-wisp-test1","created_at":"2026-09-22T10:00:00Z","labels":["type:plugin-run","plugin:compactor-dog","result:success"]}]'
  FOUND=$(run_last_run_query "$RECEIPT")
  if [[ "$FOUND" != "2026-09-22T10:00:00Z" ]]; then
    echo "FAIL: last-run query missed an existing receipt (got '$FOUND', want '2026-09-22T10:00:00Z')"
    echo "      The query must pass --include-infra; receipts are ephemeral wisps."
    FAILURES=$((FAILURES + 1))
  fi

  # Negative control: against a bd that returns [] no matter what, the same
  # query must report "never". Without this, a query that never ran (empty
  # extraction, missing jq) would pass as "found nothing" above.
  write_fake_bd <<'FAKE_BD_EMPTY'
#!/usr/bin/env bash
printf '[]\n'
FAKE_BD_EMPTY

  EMPTY=$(run_last_run_query "$RECEIPT")
  if [[ "$EMPTY" != "never" ]]; then
    echo "FAIL: negative control expected 'never' from an always-empty bd, got '$EMPTY'"
    echo "      The harness is not exercising the query in plugin.md."
    FAILURES=$((FAILURES + 1))
  fi
fi

echo ""
if [[ $FAILURES -gt 0 ]]; then
  echo "FAILED: $FAILURES test(s) failed"
  exit 1
else
  echo "PASSED: all tests passed"
fi
