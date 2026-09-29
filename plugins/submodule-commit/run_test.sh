#!/usr/bin/env bash
# Tests for submodule-commit/run.sh's opt-in read (gt-fcxe9.5). A rig with
# submodules whose settings carry no plugins key must be reported loudly, in
# the log and in the skip receipt, not read as a deliberate "off".
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$SCRIPT_DIR/run.sh"
ORIGINAL_PATH="$PATH"
PASS=0
FAIL=0
CLEANUP_DIRS=()

cleanup() {
  local d
  for d in "${CLEANUP_DIRS[@]+"${CLEANUP_DIRS[@]}"}"; do rm -rf "$d"; done
}
trap cleanup EXIT

record_pass() { PASS=$((PASS + 1)); printf 'PASS: %s\n' "$1"; }
record_fail() { FAIL=$((FAIL + 1)); printf 'FAIL: %s\n' "$1"; }

assert_contains() {
  if grep -qF -- "$2" "$1"; then record_pass "$3"; else record_fail "$3 (missing '$2' in $1: $(cat "$1"))"; fi
}
assert_not_contains() {
  if grep -qF -- "$2" "$1"; then record_fail "$3 (unexpected '$2' in $1)"; else record_pass "$3"; fi
}

# setup_case <settings-json> — a town with one rig "myrig" whose repo has a
# .gitmodules file, and a fake gt whose rig settings show prints the argument
# (empty = the rig is unknown: print nothing and fail).
setup_case() {
  CASE_DIR=$(mktemp -d)
  CLEANUP_DIRS+=("$CASE_DIR")
  mkdir -p "$CASE_DIR/bin" "$CASE_DIR/myrig"
  : > "$CASE_DIR/myrig/.gitmodules"
  printf '%s' "$1" > "$CASE_DIR/settings.json"
  : > "$CASE_DIR/record.log"
  : > "$CASE_DIR/escalate.log"
  cat > "$CASE_DIR/bin/gt" <<SH
#!/usr/bin/env bash
case "\$1 \$2" in
  "rig list") printf '[{"name":"myrig","repo_path":"$CASE_DIR/myrig"}]\n' ;;
  "rig settings")
    [ -s "$CASE_DIR/settings.json" ] || exit 1
    cat "$CASE_DIR/settings.json" ;;
  "plugin record-run") printf '%s\n' "\$*" >> "$CASE_DIR/record.log" ;;
  escalate*) printf '%s\n' "\$*" >> "$CASE_DIR/escalate.log" ;;
  *) echo "fake gt: unexpected: \$*" >&2; exit 2 ;;
esac
SH
  chmod +x "$CASE_DIR/bin/gt"
}

run_plugin() {
  PLUGIN_RC=0
  PATH="$CASE_DIR/bin:$ORIGINAL_PATH" bash "$SCRIPT" > "$CASE_DIR/out.log" 2>&1 || PLUGIN_RC=$?
}

# No plugins key in the rig's settings: warn, skip, name the rig in the receipt.
setup_case '{"type":"rig-settings","version":1}'
run_plugin
if [ "$PLUGIN_RC" -eq 0 ]; then record_pass "no plugins key: exits 0 (skip)"; else record_fail "no plugins key: exit $PLUGIN_RC: $(cat "$CASE_DIR/out.log")"; fi
assert_contains "$CASE_DIR/out.log" "no readable submodule-commit opt-in" "no plugins key: warns"
assert_contains "$CASE_DIR/record.log" "no readable opt-in: myrig" "no plugins key: receipt names the rig"
assert_contains "$CASE_DIR/escalate.log" "--fingerprint submodule-commit:opt-in-unreadable" "no plugins key: escalates under one folding key"
assert_contains "$CASE_DIR/escalate.log" "myrig" "no plugins key: escalation names the rig"

# An explicit opt-out is a deliberate off: no warning.
setup_case '{"type":"rig-settings","version":1,"plugins":{"submodule-commit":{"enabled":false}}}'
run_plugin
if [ "$PLUGIN_RC" -eq 0 ]; then record_pass "explicit off: exits 0 (skip)"; else record_fail "explicit off: exit $PLUGIN_RC"; fi
assert_not_contains "$CASE_DIR/out.log" "no readable submodule-commit opt-in" "explicit off: no warning"
if [ -s "$CASE_DIR/escalate.log" ]; then record_fail "explicit off: escalated: $(cat "$CASE_DIR/escalate.log")"; else record_pass "explicit off: no escalation"; fi

# gt rig settings show fails (unknown rig): also unreadable, also loud.
setup_case ''
run_plugin
if [ "$PLUGIN_RC" -eq 0 ]; then record_pass "settings unavailable: exits 0 (skip)"; else record_fail "settings unavailable: exit $PLUGIN_RC"; fi
assert_contains "$CASE_DIR/out.log" "no readable submodule-commit opt-in" "settings unavailable: warns"

printf '\n%s passed, %s failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
