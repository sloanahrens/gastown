#!/usr/bin/env bash
# Tests for scripts/install-om.sh (gt-3imih). Offline: a real git clone stands in
# for <town>/.runtime/om-src, and a fake `go` on PATH "builds" an om script that
# prints "om version <sha>" for the sha it was handed through -ldflags. Every
# install lands in a mktemp -d bin dir, never in ~/go/bin or ~/.local/bin.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
INSTALLER="$SCRIPT_DIR/install-om.sh"
FAILURES=0
fail() { echo "FAIL: $*"; FAILURES=$((FAILURES + 1)); }
pass() { echo "PASS: $*"; }

# make_world -> dir with: src (a clone of origin, at c2), bin/ (empty), daemon/,
# stubs/go. c1 and c2 hold the two full shas.
#
# The fake go reads the version from -X main.version=<sha> and the output from
# -o. Marker files in $T_WORLD steer it: fail_build (exit 2), wrong_sha (the om
# it builds reports a different sha), no_version_line (it prints junk), and
# hold_build (wait for the file release_build to appear, so a second run can be
# started while this one holds the lock).
make_world() {
  local t; t=$(mktemp -d)
  git init -q --bare -b main "$t/origin.git"
  git clone -q "$t/origin.git" "$t/src" 2>/dev/null
  git -C "$t/src" -c user.email=t@t -c user.name=t commit -q --allow-empty -m c1
  git -C "$t/src" rev-parse HEAD > "$t/c1"
  git -C "$t/src" -c user.email=t@t -c user.name=t commit -q --allow-empty -m c2
  git -C "$t/src" rev-parse HEAD > "$t/c2"
  git -C "$t/src" push -q origin main
  mkdir -p "$t/stubs" "$t/bin" "$t/daemon"
  cat > "$t/stubs/go" <<'GO'
#!/usr/bin/env bash
echo "go $*" >> "$T_WORLD/go.log"
echo "cwd $PWD" >> "$T_WORLD/go.log"
[ "$1" = "build" ] || { echo "stub go: unhandled: $*" >&2; exit 2; }
shift
ver="" out=""
while [ $# -gt 0 ]; do
  case "$1" in
    -ldflags) ver="${2#-X main.version=}"; shift 2 ;;
    -o) out="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ -e "$T_WORLD/hold_build" ]; then
  : > "$T_WORLD/build_started"
  while [ ! -e "$T_WORLD/release_build" ]; do sleep 0.1; done
fi
[ -e "$T_WORLD/fail_build" ] && { echo "build failed" >&2; exit 2; }
[ -e "$T_WORLD/wrong_sha" ] && ver="deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
if [ -e "$T_WORLD/no_version_line" ]; then
  printf '#!/usr/bin/env bash\necho "not a version"\n' > "$out"
else
  printf '#!/usr/bin/env bash\necho "om version %s"\n' "$ver" > "$out"
fi
chmod +x "$out"
GO
  # The security and codesign the signing cases look for; they only log.
  cat > "$t/stubs/security" <<'SEC'
#!/usr/bin/env bash
echo "security $1" >> "$T_WORLD/sign.log"
[ -e "$T_WORLD/unlock_fails" ] && exit 1
exit 0
SEC
  cat > "$t/stubs/codesign" <<'CS'
#!/usr/bin/env bash
echo "codesign $*" >> "$T_WORLD/sign.log"
[ -e "$T_WORLD/codesign_fails" ] && { echo "no identity found" >&2; exit 1; }
# Mark the file so a case can see that the signed build is the one swapped in.
printf '# signed\n' >> "${@: -1}"
exit 0
CS
  chmod +x "$t/stubs/go" "$t/stubs/security" "$t/stubs/codesign"
  echo "$t"
}

# put_om T SHA — an already-installed om reporting SHA, at $T/bin/om.
put_om() {
  printf '#!/usr/bin/env bash\necho "om version %s"\n' "$2" > "$1/bin/om"
  chmod +x "$1/bin/om"
}

# run_install T ARGS... -> exit code; output in $RUN_OUT (default $T/run.out).
run_install() {
  local t="$1" rc=0; shift
  ( export T_WORLD="$t" GT_TOWN_ROOT="$t/town" INSTALL_OM_SRC_DIR="$t/src" \
      INSTALL_OM_DAEMON_DIR="$t/daemon" INSTALL_OM_LOCK_WAIT="${LOCK_WAIT:-5}" \
      INSTALL_OM_SIGN_DIR="$t/signing" INSTALL_OM_PATH="$t/bin/om" \
      PATH="$t/stubs:/usr/bin:/bin:/opt/homebrew/bin:/usr/sbin:/sbin"
    bash "$INSTALLER" "$@" ) > "${RUN_OUT:-$t/run.out}" 2>&1 || rc=$?
  echo "$rc"
}

last_receipt() {
  tail -1 "$1/daemon/install-om-receipts.jsonl" 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get(sys.argv[1]))' "$2" 2>/dev/null || echo "NO-RECEIPT"
}
result_line() { tail -1 "$1/run.out"; }
reported() { "$1/bin/om" version; }
# no_stray T -> true when the bin dir holds nothing but om and om.prev
no_stray() { [ -z "$(ls -A "$1/bin" | grep -v -x -e om -e om.prev || true)" ]; }

echo "=== install-om.sh tests ==="

# --- Case 1: first install from a clone with no om: built sha in force, no om.prev ---
T=$(make_world)
C2=$(cat "$T/c2")
rc=$(run_install "$T")
[ "$rc" = "0" ] && pass "first install: exit 0" || fail "first install: exit $rc: $(cat "$T/run.out")"
[ "$(reported "$T")" = "om version $C2" ] && pass "first install: om reports the built sha" || fail "first install: om reports $(reported "$T")"
[ ! -e "$T/bin/om.prev" ] && pass "first install: no om.prev (nothing to keep)" || fail "first install: om.prev appeared"
no_stray "$T" && pass "first install: no temp files left in the bin dir" || fail "first install: stray files: $(ls -A "$T/bin")"
grep -q "go build -ldflags -X main.version=$C2 -o .* ./cmd/om" "$T/go.log" && pass "first install: go build stamps main.version with the full sha" || fail "first install: go.log $(cat "$T/go.log")"
grep -q -x -F -e "cwd $T/src" -e "cwd $(cd "$T/src" && pwd -P)" "$T/go.log" && pass "first install: built inside the job's clone" || fail "first install: build cwd: $(grep ^cwd "$T/go.log")"
[ "$(result_line "$T")" = "install-om: RESULT installed $C2 - -" ] && pass "first install: RESULT line" || fail "first install: RESULT line: $(result_line "$T")"
[ "$(last_receipt "$T" event)" = "installed" ] && [ "$(last_receipt "$T" commit)" = "$C2" ] && pass "first install: installed receipt" || fail "first install: receipt $(tail -1 "$T/daemon/install-om-receipts.jsonl" 2>/dev/null)"

# --- Case 2: upgrade c1 -> c2: new sha in force, om.prev is the c1 binary ---
T=$(make_world)
C1=$(cat "$T/c1"); C2=$(cat "$T/c2")
put_om "$T" "$C1"
rc=$(run_install "$T" --source rebuild-om)
[ "$rc" = "0" ] && [ "$(reported "$T")" = "om version $C2" ] && pass "upgrade: c2 in force" || fail "upgrade: rc=$rc reported $(reported "$T")"
[ "$("$T/bin/om.prev" version)" = "om version $C1" ] && pass "upgrade: om.prev is the c1 binary" || fail "upgrade: om.prev missing or wrong"
no_stray "$T" && pass "upgrade: no temp files left" || fail "upgrade: stray files: $(ls -A "$T/bin")"
[ "$(result_line "$T")" = "install-om: RESULT installed $C2 $C1 -" ] && pass "upgrade: RESULT line carries commit and prev" || fail "upgrade: RESULT line: $(result_line "$T")"
[ "$(last_receipt "$T" source)" = "rebuild-om" ] && [ "$(last_receipt "$T" prev_commit)" = "$C1" ] && pass "upgrade: receipt carries source and prev" || fail "upgrade: receipt $(tail -1 "$T/daemon/install-om-receipts.jsonl")"

# --- Case 3: the installed om already is HEAD -> noop, no build ---
T=$(make_world)
C2=$(cat "$T/c2")
put_om "$T" "$C2"
rc=$(run_install "$T")
[ "$rc" = "0" ] && [ ! -e "$T/go.log" ] && pass "noop: exit 0, no build" || fail "noop: rc=$rc $(cat "$T/run.out")"
[ "$(result_line "$T")" = "install-om: RESULT noop $C2 $C2 already-installed" ] && pass "noop: RESULT line" || fail "noop: RESULT line: $(result_line "$T")"
[ ! -e "$T/bin/om.prev" ] && pass "noop: om.prev untouched" || fail "noop: om.prev appeared"

# --- Case 4: smoke check fails (the build reports another sha) -> om.prev restored ---
T=$(make_world)
C1=$(cat "$T/c1"); C2=$(cat "$T/c2")
put_om "$T" "$C1"
: > "$T/wrong_sha"
rc=$(run_install "$T")
[ "$rc" = "1" ] && pass "smoke fails: exit 1" || fail "smoke fails: exit $rc"
[ "$(reported "$T")" = "om version $C1" ] && pass "smoke fails: the c1 om is back in force" || fail "smoke fails: om reports $(reported "$T")"
[ "$(result_line "$T")" = "install-om: RESULT rolled_back $C2 $C1 smoke-failed" ] && pass "smoke fails: rolled_back RESULT line" || fail "smoke fails: RESULT line: $(result_line "$T")"
[ "$(last_receipt "$T" event)" = "rolled_back" ] && pass "smoke fails: rolled_back receipt" || fail "smoke fails: receipt $(last_receipt "$T" event)"
no_stray "$T" && pass "smoke fails: no temp files left" || fail "smoke fails: stray files: $(ls -A "$T/bin")"

# --- Case 4b: the new om prints no version line at all -> also rolled back ---
T=$(make_world)
C1=$(cat "$T/c1")
put_om "$T" "$C1"
: > "$T/no_version_line"
rc=$(run_install "$T")
[ "$rc" = "1" ] && [ "$(reported "$T")" = "om version $C1" ] && pass "smoke (no version line): rolled back" || fail "smoke (no version line): rc=$rc reported $(reported "$T")"

# --- Case 4c: first install whose smoke check fails leaves no om behind ---
T=$(make_world)
: > "$T/wrong_sha"
rc=$(run_install "$T")
[ "$rc" = "1" ] && [ ! -e "$T/bin/om" ] && pass "smoke fails, first install: no broken om left in force" || fail "smoke fails, first install: rc=$rc, om exists: $(ls "$T/bin")"
case "$(result_line "$T")" in "install-om: RESULT rolled_back "*" - smoke-failed") pass "smoke fails, first install: RESULT line" ;; *) fail "smoke fails, first install: $(result_line "$T")" ;; esac

# --- Case 5: build fails -> previous om untouched, failed RESULT ---
T=$(make_world)
C1=$(cat "$T/c1"); C2=$(cat "$T/c2")
put_om "$T" "$C1"
: > "$T/fail_build"
rc=$(run_install "$T")
[ "$rc" = "1" ] && [ "$(reported "$T")" = "om version $C1" ] && pass "build fails: exit 1, previous om in place" || fail "build fails: rc=$rc reported $(reported "$T")"
[ ! -e "$T/bin/om.prev" ] && no_stray "$T" && pass "build fails: nothing else written to the bin dir" || fail "build fails: bin dir has $(ls -A "$T/bin")"
[ "$(result_line "$T")" = "install-om: RESULT failed $C2 $C1 build-failed" ] && pass "build fails: RESULT line" || fail "build fails: RESULT line: $(result_line "$T")"

# --- Case 6: refusals leave om alone: no clone, dirty clone ---
T=$(make_world)
C1=$(cat "$T/c1")
put_om "$T" "$C1"
rm -rf "$T/src"
rc=$(run_install "$T")
[ "$rc" = "2" ] && [ "$(reported "$T")" = "om version $C1" ] && pass "no clone: exit 2, om untouched" || fail "no clone: rc=$rc"
[ "$(result_line "$T")" = "install-om: RESULT refused - - no-source" ] && pass "no clone: RESULT line" || fail "no clone: RESULT line: $(result_line "$T")"

T=$(make_world)
put_om "$T" "$(cat "$T/c1")"
echo tracked > "$T/src/f.txt"; git -C "$T/src" add f.txt
rc=$(run_install "$T")
[ "$rc" = "2" ] && [ ! -e "$T/go.log" ] && pass "dirty clone: exit 2, no build" || fail "dirty clone: rc=$rc"
case "$(result_line "$T")" in *" dirty") pass "dirty clone: RESULT reason" ;; *) fail "dirty clone: $(result_line "$T")" ;; esac

# --- Case 7: two concurrent runs serialize on the lock; the second reports busy ---
T=$(make_world)
C2=$(cat "$T/c2")
: > "$T/hold_build"
( RUN_OUT="$T/first.out" run_install "$T" > "$T/first.rc" ) &
FIRST=$!
for _ in $(seq 1 100); do [ -e "$T/build_started" ] && break; sleep 0.1; done
[ -e "$T/build_started" ] || fail "lock: the first run never started building"
rc=$(LOCK_WAIT=1 run_install "$T")
[ "$rc" = "3" ] && pass "lock: second run exits 3 while the first builds" || fail "lock: second exit $rc: $(cat "$T/run.out")"
[ "$(result_line "$T")" = "install-om: RESULT refused - - lock-busy" ] && pass "lock: second run's RESULT line is refused lock-busy" || fail "lock: second RESULT line: $(result_line "$T")"
[ "$(grep -c '^go build' "$T/go.log")" = "1" ] && pass "lock: only one build ran" || fail "lock: go.log $(cat "$T/go.log")"
: > "$T/release_build"
wait "$FIRST"
[ "$(cat "$T/first.rc")" = "0" ] && [ "$(reported "$T")" = "om version $C2" ] && pass "lock: the first run finished and installed" || fail "lock: first rc=$(cat "$T/first.rc") reported $(reported "$T")"
# With the lock free again a later run goes straight through (noop: already c2).
rc=$(run_install "$T")
[ "$rc" = "0" ] && pass "lock: released after the first run" || fail "lock: third run exit $rc"

# --- Case 8: signing. Material present -> the build is signed before the swap ---
# The fake keychain password every case below writes. It is long and distinctive
# so the leak check in this case can match only a real leak: a two-letter
# password matched the random mktemp names in the output (gt-j4b9v).
KC_CANARY='keychain-canary-9f3c1d7e'
T=$(make_world)
mkdir -p "$T/signing"
printf 'kc\n' > "$T/signing/gastown-signing.keychain-db"
printf '%s' "$KC_CANARY" > "$T/signing/keychain.pass"
rc=$(run_install "$T")
[ "$rc" = "0" ] && pass "signing: exit 0" || fail "signing: exit $rc: $(cat "$T/run.out")"
grep -q "codesign --force --keychain $T/signing/gastown-signing.keychain-db -s gastown-dev-unattended -i com.gastown.om" "$T/sign.log" && pass "signing: codesign ran with the town identity" || fail "signing: sign.log $(cat "$T/sign.log" 2>/dev/null)"
grep -q "# signed" "$T/bin/om" && pass "signing: the signed build is what was swapped in" || fail "signing: installed om is not the signed one"
grep -qF -- "$KC_CANARY" "$T/run.out" && fail "signing: the keychain password leaked into the output" || pass "signing: password not in the output"

# --- Case 9: signing falls back to ad-hoc with one WARNING and still installs ---
T=$(make_world)
rc=$(run_install "$T")
[ "$rc" = "0" ] && grep -q "WARNING: installing ad-hoc: no keychain at $T/signing/gastown-signing.keychain-db" "$T/run.out" \
  && pass "no signing material: ad-hoc install with a WARNING" || fail "no signing material: rc=$rc $(grep WARNING "$T/run.out")"

T=$(make_world)
mkdir -p "$T/signing"
printf 'kc\n' > "$T/signing/gastown-signing.keychain-db"
printf '%s' "$KC_CANARY" > "$T/signing/keychain.pass"
: > "$T/codesign_fails"
rc=$(run_install "$T")
[ "$rc" = "0" ] && [ "$(reported "$T")" = "om version $(cat "$T/c2")" ] && grep -q "WARNING: installing ad-hoc: codesign failed: no identity found" "$T/run.out" \
  && pass "codesign fails: ad-hoc install, not a failure" || fail "codesign fails: rc=$rc $(grep WARNING "$T/run.out")"

T=$(make_world)
mkdir -p "$T/signing"
printf 'kc\n' > "$T/signing/gastown-signing.keychain-db"
printf '%s' "$KC_CANARY" > "$T/signing/keychain.pass"
: > "$T/unlock_fails"
rc=$(run_install "$T")
[ "$rc" = "0" ] && grep -q "WARNING: installing ad-hoc: cannot unlock" "$T/run.out" \
  && pass "unlock fails: ad-hoc install with a WARNING" || fail "unlock fails: rc=$rc $(grep WARNING "$T/run.out")"

# --- Case 10: a bad flag still ends on a RESULT line (the EXIT trap's contract) ---
T=$(make_world)
rc=$(run_install "$T" --bogus)
[ "$rc" = "1" ] && [ "$(result_line "$T")" = "install-om: RESULT failed - - unexpected" ] && pass "bad flag: exit 1 with a failed RESULT line" || fail "bad flag: exit $rc: $(result_line "$T")"

if [ "$FAILURES" -ne 0 ]; then echo "$FAILURES failure(s)"; exit 1; fi
echo "all install-om tests passed"
