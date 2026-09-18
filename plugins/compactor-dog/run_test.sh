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

echo ""
if [[ $FAILURES -gt 0 ]]; then
  echo "FAILED: $FAILURES test(s) failed"
  exit 1
else
  echo "PASSED: all tests passed"
fi
