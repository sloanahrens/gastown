#!/usr/bin/env bash
# Tests for scripts/install-after-merge.sh (claude-7fc): the denylist filter
# decides between a 'skipped' receipt and handing the merge to install-gt.sh.
# install-gt.sh is replaced by a stub that records its arguments, so this file
# tests only the filter.
set -euo pipefail

SRC_DIR="$(cd "$(dirname "$0")" && pwd)"
FAILURES=0
fail() { echo "FAIL: $*"; FAILURES=$((FAILURES + 1)); }
pass() { echo "PASS: $*"; }

# make_world -> dir with a repo (base commit B installed), the script under
# test copied into scripts/ beside a recording install-gt.sh stub, and a gt stub.
make_world() {
  local t; t=$(mktemp -d)
  git init -q -b main "$t/repo"
  git -C "$t/repo" -c user.email=t@t -c user.name=t commit -q --allow-empty -m base
  mkdir -p "$t/scripts/lib" "$t/bin" "$t/daemon"
  cp "$SRC_DIR/install-after-merge.sh" "$t/scripts/"
  cp "$SRC_DIR/lib/install-gt-lib.sh" "$t/scripts/lib/"
  cat > "$t/scripts/install-gt.sh" <<'EOF'
#!/usr/bin/env bash
echo "install-gt $*" >> "$T_WORLD/called.log"
exit "${STUB_RC:-0}"
EOF
  chmod +x "$t/scripts/install-gt.sh"
  set_installed "$t" "$(git -C "$t/repo" rev-parse --short HEAD)"
  echo "$t"
}

# set_installed T COMMIT — the installed gt reports COMMIT ("" = unreadable).
set_installed() {
  cat > "$1/bin/gt" <<EOF
#!/usr/bin/env bash
case "\$1 \${2:-}" in
  "stale --json") printf '{"binary_commit": "%s"}\n' "$2" ;;
  "version "*) echo "gt version dev" ;;
  *) exit 1 ;;
esac
EOF
  chmod +x "$1/bin/gt"
}

# land T FILE... — commit changes to FILEs, echo the new full sha
land() {
  local t="$1"; shift
  for f in "$@"; do mkdir -p "$t/repo/$(dirname "$f")"; echo "x$RANDOM" >> "$t/repo/$f"; done
  git -C "$t/repo" add -A
  git -C "$t/repo" -c user.email=t@t -c user.name=t commit -q -m change
  git -C "$t/repo" rev-parse HEAD
}

run_hook() {
  local t="$1" sha="$2" rc=0
  ( cd "$t/repo" && export T_WORLD="$t" GT_MERGED_SHA="$sha" GT_TOWN_ROOT="$t" \
      INSTALL_GT_BIN_DIR="$t/bin" INSTALL_GT_DAEMON_DIR="$t/daemon" \
      PATH="/usr/bin:/bin:/opt/homebrew/bin"
    bash "$t/scripts/install-after-merge.sh" ) > "$t/run.out" 2>&1 || rc=$?
  echo "$rc"
}

last_event() { tail -1 "$1/daemon/install-receipts.jsonl" 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin)["event"])' 2>/dev/null || echo none; }

echo "=== install-after-merge.sh tests ==="

# --- Only denylisted paths changed -> skipped receipt, install-gt not called ---
T=$(make_world)
SHA=$(land "$T" internal/foo_test.go README.md docs/guide.txt .beads/issues.jsonl)
rc=$(run_hook "$T" "$SHA")
[ "$rc" = "0" ] && [ ! -e "$T/called.log" ] && [ "$(last_event "$T")" = "skipped" ] \
  && pass "denylist only: skipped" || fail "denylist only: rc=$rc called=$(cat "$T/called.log" 2>/dev/null) $(cat "$T/run.out")"

# --- A runtime .go file changed -> install-gt with the merge sha ---
T=$(make_world)
SHA=$(land "$T" internal/foo.go README.md)
rc=$(run_hook "$T" "$SHA")
[ "$rc" = "0" ] && grep -q "install-gt --sha $SHA --source post-merge" "$T/called.log" \
  && pass "runtime .go: install-gt called" || fail "runtime .go: rc=$rc $(cat "$T/called.log" 2>/dev/null)"

# --- An unknown path changed -> install (fail toward installing) ---
T=$(make_world)
SHA=$(land "$T" weird/thing.bin)
rc=$(run_hook "$T" "$SHA")
grep -q "install-gt --sha $SHA" "$T/called.log" 2>/dev/null && pass "unknown path: installs" || fail "unknown path: not installed"

# --- Earlier runtime merge not installed, then a docs-only merge -> still
# installs: the diff starts at the INSTALLED commit, not this merge's parent ---
T=$(make_world)
land "$T" internal/runtime.go >/dev/null
SHA=$(land "$T" docs/only.md)
rc=$(run_hook "$T" "$SHA")
grep -q "install-gt --sha $SHA" "$T/called.log" 2>/dev/null && pass "masked runtime: installs" || fail "masked runtime: skipped a pending runtime change"

# --- Installed commit unreadable -> install ---
T=$(make_world)
set_installed "$T" ""
SHA=$(land "$T" docs/only.md)
rc=$(run_hook "$T" "$SHA")
grep -q "install-gt --sha $SHA" "$T/called.log" 2>/dev/null && pass "unreadable installed: installs" || fail "unreadable installed: skipped"

# --- install-gt's exit code is the hook's exit code (it execs) ---
T=$(make_world)
SHA=$(land "$T" internal/foo.go)
rc=$(STUB_RC=2 run_hook "$T" "$SHA")
[ "$rc" = "2" ] && pass "exit code passes through" || fail "exit code: got $rc"

# --- GT_MERGED_SHA empty (the hook could resolve no merge commit) -> exit 2,
# refused receipt no-merged-sha, nothing installed ---
T=$(make_world)
rc=$(run_hook "$T" "")
if [ "$rc" = "2" ] && [ ! -e "$T/called.log" ] \
  && tail -1 "$T/daemon/install-receipts.jsonl" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r["event"]=="refused" and r["reason"]=="no-merged-sha", r'; then
  pass "empty GT_MERGED_SHA: refused, receipt, exit 2"
else
  fail "empty GT_MERGED_SHA: rc=$rc $(cat "$T/run.out")"
fi

# --- GT_MERGED_SHA unset entirely -> the same refusal ---
T=$(make_world)
rc=0; ( cd "$T/repo" && T_WORLD="$T" GT_TOWN_ROOT="$T" INSTALL_GT_BIN_DIR="$T/bin" INSTALL_GT_DAEMON_DIR="$T/daemon" bash "$T/scripts/install-after-merge.sh" ) >/dev/null 2>&1 || rc=$?
[ "$rc" = "2" ] && [ ! -e "$T/called.log" ] && pass "unset GT_MERGED_SHA: refuses" || fail "unset GT_MERGED_SHA: rc=$rc"

if [ "$FAILURES" -ne 0 ]; then echo "$FAILURES failure(s)"; exit 1; fi
echo "all install-after-merge tests passed"
