#!/usr/bin/env bash
# Tests for scripts/tier-sweep.sh's summary line: each tier's line ends with
# the elapsed time of that tier, in the daemon log's rounded style (gt-iqzr0).
# Each case builds a throwaway repo holding the script under test and one stub
# shell test, so no real tier runs. The integration cases add a nested module
# and stub go and gt binaries, so which packages run where, and how a failure
# is named, are the stubs' to report (gt-558i2).
set -uo pipefail

SRC="$(cd "$(dirname "$0")" && pwd)/tier-sweep.sh"
FAILS=0
pass() { echo "PASS: $1"; }
fail() { echo "FAIL: $1"; FAILS=$((FAILS + 1)); }

# new_repo BODY prints a repo whose scripts/x_test.sh runs BODY and whose
# scripts/tier-sweep.sh is the real one. The stub names itself so the sweep's
# own dedupe, which reads each tracked test's path back out of SHELL_TESTS,
# runs it once and not twice.
# tmpbase is the temp dir without a trailing slash. macOS TMPDIR ends in one, so
# "${TMPDIR}/x" holds a "//" that the shell's normalized cwd (what the stub go
# logs) does not, and the cases that compare the two exactly failed on a Mac.
tmpbase() {
  local t=${TMPDIR:-/tmp}
  printf '%s' "${t%/}"
}

new_repo() {
  local body=$1 r
  r=$(mktemp -d "$(tmpbase)/tier-sweep.XXXXXX")
  git -C "$r" init -q -b main
  git -C "$r" config user.email t@example.com
  git -C "$r" config user.name t
  mkdir -p "$r/scripts"
  cp "$SRC" "$r/scripts/tier-sweep.sh"
  printf '#!/usr/bin/env bash\n# scripts/x_test.sh\nexit %s\n' "$body" > "$r/scripts/x_test.sh"
  git -C "$r" add -A
  git -C "$r" commit -q -m base
  echo "$r"
}

# sweep REPO [TIER...] runs the sweep's shell tier in REPO and prints its
# combined output; the exit code lands in $rc.
run_tier() {
  local r=$1
  shift
  out=$(cd "$r" && SHELL_TESTS=scripts/x_test.sh bash scripts/tier-sweep.sh "$@" 2>&1)
  rc=$?
}

# --- A green tier ends its summary line with the elapsed time ---
R=$(new_repo 0)
run_tier "$R" shell
line=$(grep -E '^tier-sweep: shell (GREEN|RED) ' <<<"$out")
if [ "$rc" = 0 ] && grep -Eq '^tier-sweep: shell GREEN passed=1 failed=0 skipped=0 \(logs [^)]*\) in [0-9]+(h[0-9]+m[0-9]+s|m[0-9]+s|s)$' <<<"$line"; then
  pass "green tier: summary line ends with its elapsed time ($line)"
else
  fail "green tier: rc=$rc line=$line out=$out"
fi
rm -rf "$R"

# --- A red tier carries the time too, after the names and the log marker ---
R=$(new_repo 1)
run_tier "$R" shell
line=$(grep -E '^tier-sweep: shell (GREEN|RED) ' <<<"$out")
if [ "$rc" != 0 ] && grep -Eq '^tier-sweep: shell RED passed=0 failed=1 skipped=0 failed: scripts/x_test\.sh \(logs [^)]*\) in [0-9]+(h[0-9]+m[0-9]+s|m[0-9]+s|s)$' <<<"$line"; then
  pass "red tier: summary line ends with its elapsed time ($line)"
else
  fail "red tier: rc=$rc line=$line out=$out"
fi
rm -rf "$R"

# --- A tier whose list is empty is RED, not GREEN (gt-tqxdd) ---
#
# A repo with no integration-tagged file and no docker.txt hands the tier an
# empty package list: nothing runs, and the sweep used to report that GREEN.
# The line keeps the shape the daemon and the landing gate parse, so an empty
# tier is a RED summary with no failed names.
R=$(new_repo 0)
run_tier "$R" integration
line=$(grep -E '^tier-sweep: integration (GREEN|RED) ' <<<"$out")
if [ "$rc" != 0 ] && grep -Eq '^tier-sweep: integration RED passed=0 failed=0 skipped=0 \(logs [^)]*\) in [0-9]+(h[0-9]+m[0-9]+s|m[0-9]+s|s)$' <<<"$line"; then
  pass "empty tier: RED with no names ($line)"
else
  fail "empty tier: rc=$rc line=$line out=$out"
fi
rm -rf "$R"

# --- An unknown tier runs nothing, so it prints no summary line at all ---
R=$(new_repo 0)
run_tier "$R" bogus
if [ "$rc" = 2 ] && ! grep -qE '^tier-sweep: bogus ' <<<"$out" && grep -q 'unknown tier bogus' <<<"$out"; then
  pass "unknown tier: no summary line, exit 2"
else
  fail "unknown tier: rc=$rc out=$out"
fi
rm -rf "$R"

# new_integration_repo NESTED_PKGS [MODULE_ROOT_TAGGED] [STRAY_GO_MOD]
# [DOCKER_PKG] prints a throwaway repo holding the real sweep, a root module
# with one integration-tagged package (./internal/root) and a nested module
# tracker/ whose NESTED_PKGS (space-separated, module relative such as
# store/pgtest) are integration tagged — with MODULE_ROOT_TAGGED set, a tagged
# file at tracker/ itself too. STRAY_GO_MOD adds that file and a tagged test
# beside it, a directory that merely contains a file ending in go.mod.
# DOCKER_PKG lists that package in docker.txt, the untagged run. Its stub/
# holds the go and gt the sweep calls:
#
#   go   appends "cwd|pkg" to $STUB_LOG, refuses a package inside a nested
#        module when run from the root (the real toolchain's "main module ...
#        does not contain package"), and fails each package $STUB_FAIL names.
#   gt   runs the argv after `slot run --`.
new_integration_repo() {
  local pkgs=${1:-} root_tagged=${2:-} stray=${3:-} docker=${4:-} pkg r
  r=$(mktemp -d "$(tmpbase)/tier-sweep.XXXXXX")
  [ -d "$r" ] || { echo "new_integration_repo: mktemp failed" >&2; exit 1; }
  git -C "$r" init -q -b main
  git -C "$r" config user.email t@example.com
  git -C "$r" config user.name t
  mkdir -p "$r/scripts" "$r/stub" "$r/internal/root" "$r/internal/testpolicy" "$r/tracker"
  cp "$SRC" "$r/scripts/tier-sweep.sh"
  printf 'module example.com/root\n\ngo 1.21\n' >"$r/go.mod"
  printf '//go:build integration\n\npackage root\n' >"$r/internal/root/integration_test.go"
  printf '# no docker-backed package\n' >"$r/internal/testpolicy/docker.txt"
  printf 'module example.com/root/tracker\n\ngo 1.21\n' >"$r/tracker/go.mod"
  [ -n "$root_tagged" ] && printf '//go:build integration\n\npackage fixture\n' >"$r/tracker/integration_test.go"
  for pkg in $pkgs; do
    mkdir -p "$r/tracker/$pkg"
    printf '//go:build integration\n\npackage fixture\n' >"$r/tracker/$pkg/integration_test.go"
  done
  if [ -n "$stray" ]; then
    mkdir -p "$r/$(dirname "$stray")"
    printf 'not a module: a file whose name merely ends in go.mod\n' >"$r/$stray"
    printf '//go:build integration\n\npackage fixture\n' >"$r/$(dirname "$stray")/integration_test.go"
  fi
  if [ -n "$docker" ]; then
    printf '%s\n' $docker >"$r/internal/testpolicy/docker.txt"
  fi
  cat >"$r/stub/go" <<'STUB'
#!/usr/bin/env bash
pkg=""
for a in "$@"; do pkg=$a; done
echo "$PWD|$pkg" >>"$STUB_LOG"
mod=$PWD/${pkg#./}
mod=${mod%/}
while [ ! -f "$mod/go.mod" ] && [ "$mod" != / ]; do mod=$(dirname "$mod"); done
if [ "$mod" != "$PWD" ]; then
  echo "main module example.com/root does not contain package $pkg" >&2
  echo "FAIL $pkg [setup failed]" >&2
  exit 1
fi
case " ${STUB_FAIL:-} " in
*" $pkg "*) echo "--- FAIL: TestFixture" >&2; exit 1 ;;
esac
exit 0
STUB
  cat >"$r/stub/gt" <<'STUB'
#!/usr/bin/env bash
while [ $# -gt 0 ] && [ "$1" != -- ]; do shift; done
[ $# -gt 0 ] && shift
exec "$@"
STUB
  chmod +x "$r/stub/go" "$r/stub/gt"
  git -C "$r" add -A
  git -C "$r" commit -q -m base
  echo "$r"
}

# --- A nested module's package runs in that module, not from the root ---
#
# The sweep built its integration list from the root module's git ls-files, so
# ./tracker/store reached `go test` at the repo root, where the toolchain
# refuses it: "main module ... does not contain package", the integration RED
# of the first sweep after tracker/ grew its tests (gt-558i2). The stub go
# reports exactly that from the root only, so the old script is RED here.
R=$(new_integration_repo store)
: >"$R/stub.log"
out=$(cd "$R" && PATH="$R/stub:$PATH" STUB_LOG="$R/stub.log" bash scripts/tier-sweep.sh integration 2>&1); rc=$?
line=$(grep -E '^tier-sweep: integration (GREEN|RED) ' <<<"$out")
if [ "$rc" = 0 ] \
  && grep -Eq '^tier-sweep: integration GREEN passed=2 failed=0 skipped=0 \(logs [^)]*\) in ' <<<"$line" \
  && grep -Eq '/tracker\|\./store$' "$R/stub.log" \
  && ! grep -Eq '\|\./tracker/' "$R/stub.log"; then
  pass "nested module: tracker/store runs inside tracker, the root list omits it ($line)"
else
  fail "nested module: rc=$rc line=$line ran=$(tr '\n' ';' <"$R/stub.log") out=$out"
fi
rm -rf "$R"

# --- A nested package's failure names its module and module-relative path ---
#
# One tracker package fails, one passes: the failure lands in failed: as
# ./tracker/store/pgtest, the module then the path inside it, and the passing
# one still counts, whichever module it ran in.
R=$(new_integration_repo "store store/pgtest")
: >"$R/stub.log"
out=$(cd "$R" && PATH="$R/stub:$PATH" STUB_LOG="$R/stub.log" STUB_FAIL="./store/pgtest" bash scripts/tier-sweep.sh integration 2>&1); rc=$?
line=$(grep -E '^tier-sweep: integration (GREEN|RED) ' <<<"$out")
if [ "$rc" != 0 ] \
  && grep -Eq '^tier-sweep: integration RED passed=2 failed=1 skipped=0 failed: \./tracker/store/pgtest \(logs [^)]*\) in ' <<<"$line"; then
  pass "nested module: a failing ./tracker/store/pgtest is named in failed: ($line)"
else
  fail "nested module failure: rc=$rc line=$line out=$out"
fi
rm -rf "$R"

# --- A nested package takes the same skip entry the root list's do ---
#
# The skip list spells a package ./path, so a nested one has to read
# ./tracker/store and not tracker/store: an entry that never matches is a
# quarantine that silently does nothing, and the tier stays RED.
R=$(new_integration_repo store)
: >"$R/stub.log"
out=$(cd "$R" && PATH="$R/stub:$PATH" STUB_LOG="$R/stub.log" STUB_FAIL="./store" TIER_SWEEP_SKIP=integration:./tracker/store bash scripts/tier-sweep.sh integration 2>&1); rc=$?
line=$(grep -E '^tier-sweep: integration (GREEN|RED) ' <<<"$out")
if [ "$rc" = 0 ] \
  && grep -Eq '^tier-sweep: integration GREEN passed=1 failed=0 skipped=1 \(logs [^)]*\) in ' <<<"$line" \
  && ! grep -Eq '\|\./store$' "$R/stub.log"; then
  pass "nested module: integration:./tracker/store skips the package ($line)"
else
  fail "nested module skip: rc=$rc line=$line ran=$(tr '\n' ';' <"$R/stub.log") out=$out"
fi
rm -rf "$R"

# --- A module with no tagged package runs nothing and says nothing ---
#
# The list is empty for tracker/, and an empty list must not reach dirname: it
# would print "dirname: missing operand" into the log of every sweep.
R=$(new_integration_repo "")
: >"$R/stub.log"
out=$(cd "$R" && PATH="$R/stub:$PATH" STUB_LOG="$R/stub.log" bash scripts/tier-sweep.sh integration 2>&1); rc=$?
line=$(grep -E '^tier-sweep: integration (GREEN|RED) ' <<<"$out")
if [ "$rc" = 0 ] \
  && grep -Eq '^tier-sweep: integration GREEN passed=1 failed=0 skipped=0 \(logs [^)]*\) in ' <<<"$line" \
  && ! grep -q 'missing operand' <<<"$out"; then
  pass "nested module: no tagged package there runs nothing and logs no dirname error"
else
  fail "nested module empty: rc=$rc line=$line out=$out"
fi
rm -rf "$R"

# --- A tagged file at the module root runs in that module, not from the root ---
#
# The root list must drop ./tracker itself, not only what lies below it, or the
# module's own package fails to build from the root and the nested pass names
# it a second time.
R=$(new_integration_repo "" 1)
: >"$R/stub.log"
out=$(cd "$R" && PATH="$R/stub:$PATH" STUB_LOG="$R/stub.log" bash scripts/tier-sweep.sh integration 2>&1); rc=$?
line=$(grep -E '^tier-sweep: integration (GREEN|RED) ' <<<"$out")
if [ "$rc" = 0 ] \
  && grep -Eq '^tier-sweep: integration GREEN passed=2 failed=0 skipped=0 \(logs [^)]*\) in ' <<<"$line" \
  && grep -Eq '/tracker\|\./$' "$R/stub.log" \
  && ! grep -Eq '\|\./tracker$' "$R/stub.log"; then
  pass "nested module: a module-root package runs inside tracker ($line)"
else
  fail "nested module root: rc=$rc line=$line ran=$(tr '\n' ';' <"$R/stub.log") out=$out"
fi
rm -rf "$R"

# --- A root failure survives a nested module that reports nothing ---
#
# The failed names accumulate across the root run, each module and the
# untagged run. A module with no failure must add nothing: appending its empty
# name unguarded reset the list, so this RED line named no failure at all and
# the operator saw counts with nothing to chase.
R=$(new_integration_repo store)
: >"$R/stub.log"
out=$(cd "$R" && PATH="$R/stub:$PATH" STUB_LOG="$R/stub.log" STUB_FAIL="./internal/root" bash scripts/tier-sweep.sh integration 2>&1); rc=$?
line=$(grep -E '^tier-sweep: integration (GREEN|RED) ' <<<"$out")
if [ "$rc" != 0 ] \
  && grep -Eq '^tier-sweep: integration RED passed=1 failed=1 skipped=0 failed: \./internal/root \(logs [^)]*\) in ' <<<"$line"; then
  pass "failed names: a passing nested module keeps ./internal/root in failed: ($line)"
else
  fail "failed names across a passing module: rc=$rc line=$line out=$out"
fi
rm -rf "$R"

# --- A nested failure precedes the untagged: group ---
#
# "untagged:" labels the names that follow it, so a nested failure appended
# after the marker reads as one of the untagged packages. Both failures here:
# the nested one first, then untagged:./internal/docker.
R=$(new_integration_repo "store store/pgtest" "" "" "internal/docker")
: >"$R/stub.log"
out=$(cd "$R" && PATH="$R/stub:$PATH" STUB_LOG="$R/stub.log" STUB_FAIL="./store/pgtest ./internal/docker" bash scripts/tier-sweep.sh integration 2>&1); rc=$?
line=$(grep -E '^tier-sweep: integration (GREEN|RED) ' <<<"$out")
if [ "$rc" != 0 ] \
  && grep -Eq '^tier-sweep: integration RED passed=2 failed=2 skipped=0 failed: \./tracker/store/pgtest untagged:\./internal/docker \(logs [^)]*\) in ' <<<"$line"; then
  pass "failed names: the nested failure comes before untagged: ($line)"
else
  fail "failed names order: rc=$rc line=$line out=$out"
fi
rm -rf "$R"

# --- A file whose name merely ends in go.mod is not a module ---
#
# nested_modules' pathspec names go.mod itself. '*go.mod' also matched
# extra/notgo.mod, so extra/ left the root list and ran as if it were a
# module: here it is a root package and must run from the repo root.
R=$(new_integration_repo store "" "extra/notgo.mod")
: >"$R/stub.log"
out=$(cd "$R" && PATH="$R/stub:$PATH" STUB_LOG="$R/stub.log" bash scripts/tier-sweep.sh integration 2>&1); rc=$?
line=$(grep -E '^tier-sweep: integration (GREEN|RED) ' <<<"$out")
if [ "$rc" = 0 ] \
  && grep -Eq '^tier-sweep: integration GREEN passed=3 failed=0 skipped=0 \(logs [^)]*\) in ' <<<"$line" \
  && grep -Fxq "$R|./extra" "$R/stub.log" \
  && ! grep -Fq "$R/extra|" "$R/stub.log"; then
  pass "not a module: extra/notgo.mod leaves extra/ in the root list ($line)"
else
  fail "not a module: rc=$rc line=$line ran=$(tr '\n' ';' <"$R/stub.log") out=$out"
fi
rm -rf "$R"

[ "$FAILS" = 0 ] || { echo "$FAILS failure(s)"; exit 1; }
echo "tier-sweep_test.sh: all cases passed"
