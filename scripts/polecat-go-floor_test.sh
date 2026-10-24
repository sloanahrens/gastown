#!/usr/bin/env bash
# Tests for deploy/polecat/go-floor.sh, the Go floor of the polecat image
# (gt-y06gw). The rig mode turns the rigs' go.mod files into the GO_FLOOR build
# argument; the check mode is the image's RUN step, which refuses an image whose
# Go is below that floor. Each case reads the script's exit status, stdout and
# stderr. The check mode runs against stub go binaries, so no toolchain is needed.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FLOOR_SH="$ROOT/deploy/polecat/go-floor.sh"
PASS=0
FAIL=0
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() {
  echo "  FAIL: $1"
  if [[ $# -gt 1 ]]; then printf '%s\n' "$2" | sed 's/^/    /'; fi
  FAIL=$((FAIL + 1))
}

# case_run NAME WANT_STATUS WANT_STDOUT WANT_STDERR_SUBSTRING CMD...
# An empty WANT_STDERR_SUBSTRING means stderr is not checked.
case_run() {
  local name="$1" want_status="$2" want_out="$3" want_err="$4"
  shift 4
  local out status err
  set +e
  out=$("$@" 2>"$TMP/stderr")
  status=$?
  set -e
  err=$(cat "$TMP/stderr")
  if [[ "$status" -ne "$want_status" || "$out" != "$want_out" ]]; then
    fail "$name" "want status $want_status stdout '$want_out', got status $status stdout '$out' stderr: $err"
    return
  fi
  if [[ -n "$want_err" && "$err" != *"$want_err"* ]]; then
    fail "$name" "stderr lacks '$want_err': $err"
    return
  fi
  pass "$name"
}

echo "rig: floor from go.mod files"
mkdir -p "$TMP/rigs"
printf 'module a\n\ngo 1.9\n' > "$TMP/rigs/a.mod"
printf 'module b\n\ngo 1.27\n' > "$TMP/rigs/b.mod"
printf 'module c\n\ngo 1.26.2\n' > "$TMP/rigs/c.mod"
printf 'module d\n' > "$TMP/rigs/nogo.mod"

case_run "rig picks the highest line by version order, not text order" \
  0 "1.27" "" bash "$FLOOR_SH" rig "$TMP/rigs/a.mod" "$TMP/rigs/b.mod"
case_run "rig takes the highest of three files" \
  0 "1.27" "" bash "$FLOOR_SH" rig "$TMP/rigs/a.mod" "$TMP/rigs/b.mod" "$TMP/rigs/c.mod"
case_run "rig reads a single file" \
  0 "1.26.2" "" bash "$FLOOR_SH" rig "$TMP/rigs/c.mod"
case_run "rig refuses files that name no go line" \
  2 "" "no go line" bash "$FLOOR_SH" rig "$TMP/rigs/nogo.mod"
case_run "rig refuses a missing file" \
  2 "" "" bash "$FLOOR_SH" rig "$TMP/rigs/absent.mod"
case_run "rig needs at least one file" \
  2 "" "needs at least one go.mod" bash "$FLOOR_SH" rig

echo "check: image Go against the floor"
printf '#!/usr/bin/env bash\necho go1.26.2\n' > "$TMP/go-1.26.2"
printf '#!/usr/bin/env bash\necho go1.27\n' > "$TMP/go-1.27"
printf '#!/usr/bin/env bash\necho go1.27.1\n' > "$TMP/go-1.27.1"
chmod +x "$TMP/go-1.26.2" "$TMP/go-1.27" "$TMP/go-1.27.1"

case_run "check refuses an image Go below the floor" \
  1 "" "image Go 1.26.2 is below GO_FLOOR 1.27" env GO_BIN="$TMP/go-1.26.2" bash "$FLOOR_SH" check 1.27
case_run "check accepts an image Go above the floor" \
  0 "" "" env GO_BIN="$TMP/go-1.27.1" bash "$FLOOR_SH" check 1.27
case_run "check accepts an image Go equal to the floor" \
  0 "" "" env GO_BIN="$TMP/go-1.27" bash "$FLOOR_SH" check 1.27
case_run "check refuses a floor patch release the image Go lacks" \
  1 "" "below GO_FLOOR 1.27.1" env GO_BIN="$TMP/go-1.27" bash "$FLOOR_SH" check 1.27.1
case_run "check compares versions numerically (1.9 is below 1.27)" \
  0 "" "" env GO_BIN="$TMP/go-1.27.1" bash "$FLOOR_SH" check 1.9
case_run "check refuses an empty floor" \
  2 "" "GO_FLOOR is empty" env GO_BIN="$TMP/go-1.27.1" bash "$FLOOR_SH" check ""
case_run "check needs a floor argument" \
  2 "" "GO_FLOOR is empty" env GO_BIN="$TMP/go-1.27.1" bash "$FLOOR_SH" check

echo "usage"
case_run "no mode prints usage and exits 2" \
  2 "" "usage: go-floor.sh" bash "$FLOOR_SH"

echo
echo "go-floor: $PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]]
