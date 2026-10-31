#!/usr/bin/env bash
# Tests for the Makefile's gate contract (docs/testing.md, "The gate"):
# `make gate`, `make test-slow` and `make test-integration` are the test
# tiers (`make test` only chains them), the gate runs the exec-tax preflight
# (a warning fails nothing, gt-2ycne.1), lint, build and the unit
# tier over every package in that order (no slow tier since gt-ik4a1.9),
# test-slow runs the gate and then the shell tests, neither starts a container
# or takes the container-gate slot, the integration tier runs every
# Docker-backed package listed in internal/testpolicy/docker.txt, and
# test-integration-wall reruns that tier through tierwall for the wall its
# post-merge cadence is judged by (gt-ik4a1.4.5), uncached (-count=1, gt-gq4gl).
#
# The recipe shape is read through `make -n`, which prints recipes without
# running them. That only holds while no gate recipe line references $(MAKE):
# make runs such a line even under -n. The first case checks that too. The
# failure paths are then driven for real, with stub go and golangci-lint on
# PATH and a stub shell-test script (SHELL_TESTS), so every red branch is
# seen to exit non-zero.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PASS=0
FAIL=0

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; [[ $# -gt 1 ]] && printf '%s\n' "$2" | sed 's/^/    /'; FAIL=$((FAIL + 1)); }

dry() { make -C "$ROOT" --no-print-directory -n "$@" 2>&1; }

# line_of prints the first line number of text holding the fixed string.
line_of() { { grep -n -F -- "$2" <<<"$1" || true; } | head -1 | cut -d: -f1; }

echo "gate: dry run"
if out=$(dry gate); then
  pass "make -n gate exits 0"
else
  fail "make -n gate exits 0" "$out"
fi

if grep -q -E '^gate:' "$ROOT/Makefile" && grep -q -E '^test-slow:' "$ROOT/Makefile" && grep -q -E '^test-integration:' "$ROOT/Makefile" && ! awk '/^(gate|test-slow|test-integration|test):/{f=1;next} /^[^\t]/{f=0} f' "$ROOT/Makefile" | grep -q -F '$(MAKE)'; then
  pass "gate, test-slow, test-integration and test recipes do not recurse through \$(MAKE), so -n runs nothing"
else
  fail "gate, test-slow, test-integration and test recipes do not recurse through \$(MAKE), so -n runs nothing"
fi

lint=$(line_of "$out" "golangci-lint run")
build=$(line_of "$out" "go build ./...")
unit=$(line_of "$out" "go test -timeout 20m ./...")
shell=$(line_of "$out" "internal/shelltier/cmd/shelltier")
if [[ -n "$lint" && -n "$build" && -n "$unit" && "$lint" -lt "$build" && "$build" -lt "$unit" ]]; then
  pass "gate runs lint, then go build ./..., then the cached unit tier"
else
  fail "gate runs lint, then go build ./..., then the cached unit tier (lines: lint=$lint build=$build unit=$unit)" "$out"
fi
if [[ -z "$shell" ]]; then
  pass "gate leaves the shell tests (the shelltier runner) to test-slow"
else
  fail "gate leaves the shell tests (the shelltier runner) to test-slow" "$out"
fi
# gt-s1vff: the gate's unit tier is plain go test, so the test result cache
# reruns only the packages a landing can affect; the budget runner's -exec
# wrapper bypassed the cache and reran every test on every landing. Budgets
# stay enforced by make tier-check (the hourly sweep), not the landing path.
if grep -q -F 'go test -timeout 20m ./...' <<<"$out" && ! grep -q -F 'cmd/budget' <<<"$out" && ! grep -q -E -- '-count[= ]1|-exec' <<<"$out" && ! grep -q -E -- '-slow( |=)|slow\.txt' <<<"$out"; then
  pass "gate runs the unit tier over every package through the test cache (no budget -exec, no -count=1)"
else
  fail "gate runs the unit tier over every package through the test cache (no budget -exec, no -count=1)" "$out"
fi
if ! grep -q -F -- '-strict-wall' <<<"$out" && tout=$(dry tier-check) && grep -q -F 'cmd/budget -fast-tier -strict-wall -- -timeout 20m ./...' <<<"$tout" && ! grep -q -E 'golangci-lint|go build|slot +run' <<<"$tout"; then
  pass "gate only warns on wall time; make tier-check runs the unit tier with -strict-wall and no lint or build (gt-z7qtk)"
else
  fail "gate only warns on wall time; make tier-check runs the unit tier with -strict-wall and no lint or build (gt-z7qtk)" "${tout:-}"
fi
if grep -q -E 'gate: PASSED in \$\{wall\}s wall' <<<"$out"; then
  pass "gate prints its wall time"
else
  fail "gate prints its wall time" "$out"
fi

if grep -q -F 'golangci-lint run --timeout=5m --allow-serial-runners' <<<"$out" && lout=$(dry lint) && ! grep -q -F -- '--allow-serial-runners' <<<"$lout" && grep -q -F 'scripts/lint-lock-wait.sh 600 golangci-lint run --timeout=5m' <<<"$lout"; then
  pass "gate's lint waits on the lint lock; plain make lint retries a contended lint for LINT_LOCK_WAIT (gt-uoppq)"
else
  fail "gate's lint waits on the lint lock; plain make lint retries a contended lint for LINT_LOCK_WAIT (gt-uoppq)" "$(grep -F 'golangci-lint run' <<<"$out" ${lout:+<<<"$lout"})"
fi

if grep -q -E 'slot +run' <<<"$out"; then
  fail "gate never takes the container-gate slot" "$(grep -E 'slot +run' <<<"$out")"
else
  pass "gate never takes the container-gate slot"
fi

if grep -q -F 'GT_TEST_DOCKER=0 go test -timeout 20m ./...' <<<"$out"; then
  pass "gate writes the container opt-in off where the unit tier starts"
else
  fail "gate writes the container opt-in off where the unit tier starts" "$out"
fi
if grep -q -E 'GT_TEST_DOCKER=[^0]' <<<"$out"; then
  fail "gate never turns the container opt-in on or defaults it" "$(grep -E 'GT_TEST_DOCKER=[^0]' <<<"$out")"
else
  pass "gate never turns the container opt-in on or defaults it"
fi
# An inherited opt-in must not change what the gate runs. The gate's start
# time, baked into the recipe when make parses it, is masked.
gate_shape() { dry gate | grep -v -E '^(go: |Warning: )' | sed -E 's/date \+%s\) - [0-9]+/date +%s) - START/'; }
if [[ "$(GT_TEST_DOCKER=1 gate_shape)" == "$(gate_shape)" ]]; then
  pass "an inherited GT_TEST_DOCKER=1 does not change the gate"
else
  fail "an inherited GT_TEST_DOCKER=1 does not change the gate"
fi

if [[ "$(grep -v -E '^[[:space:]]*#' <<<"$out" | grep -c -E -- '(^| )-timeout[ =]')" == 1 ]] && grep -q -F -- 'go test -timeout 20m ./...' <<<"$out"; then
  pass "gate carries one -timeout, on the unit tier's go test (the one gate definition)"
else
  fail "gate carries one -timeout, on the unit tier's go test (the one gate definition)" "$(grep -n -E -- '-timeout' <<<"$out")"
fi

echo "deleted entry points"
# Read from the Makefile, never through make -n: a deleted target's old recipe
# recursed through $(MAKE), which make runs even under -n.
for t in test-changed; do
  if grep -q -E "^$t:" "$ROOT/Makefile" || grep -q -E "^\.PHONY:.* $t( |$)" "$ROOT/Makefile"; then
    fail "make $t is gone"
  else
    pass "make $t is gone"
  fi
done
if [[ "$(grep -E '^test:' "$ROOT/Makefile")" == "test: test-slow test-integration" ]] && ! awk '/^test:/{f=1;next} /^[^\t]/{f=0} f' "$ROOT/Makefile" | grep -q .; then
  pass "make test only chains test-slow (the gate, then the shell tests) and test-integration, with no recipe of its own"
else
  fail "make test only chains test-slow (the gate, then the shell tests) and test-integration, with no recipe of its own" "$(grep -A3 -E '^test:' "$ROOT/Makefile")"
fi
if [[ ! -e "$ROOT/internal/testpolicy/slow.txt" ]] && ! grep -q -E 'SLOW_(LIST|PKGS)|slow\.txt' "$ROOT/Makefile"; then
  pass "the slow tier is gone: no slow.txt, and the Makefile names none (gt-ik4a1.9)"
else
  fail "the slow tier is gone: no slow.txt, and the Makefile names none (gt-ik4a1.9)" "$(grep -n -E 'SLOW_(LIST|PKGS)|slow\.txt' "$ROOT/Makefile")"
fi
if grep -q -E '(^|[^A-Za-z_])PKGS *\?=' "$ROOT/Makefile"; then
  fail "no PKGS default for a changed-package gate"
else
  pass "no PKGS default for a changed-package gate"
fi
if grep -q -F 'GT_TEST_DOCKER:-' "$ROOT/Makefile"; then
  fail "no recipe defaults the container opt-in from the environment" "$(grep -n -F 'GT_TEST_DOCKER:-' "$ROOT/Makefile")"
else
  pass "no recipe defaults the container opt-in from the environment"
fi

echo "test-slow"
if sout=$(dry test-slow); then
  pass "make -n test-slow exits 0"
else
  fail "make -n test-slow exits 0" "$sout"
fi
sgate=$(line_of "$sout" "go test -timeout 20m ./...")
sshell=$(line_of "$sout" "GT_TEST_DOCKER=0 go run ./internal/shelltier/cmd/shelltier --all")
if [[ -n "$sgate" && -n "$sshell" && "$sgate" -lt "$sshell" ]] && grep -q -F 'go test -timeout 20m ./...' <<<"$sout" && ! grep -q -E 'GT_TEST_DOCKER=[^0]|slot +run' <<<"$sout"; then
  pass "test-slow runs the gate, then the shell tests, containers off, no slot"
else
  fail "test-slow runs the gate, then the shell tests, containers off, no slot (lines: gate=$sgate shell=$sshell)" "$sout"
fi

# The shell tests have one runner (gt-tqxdd): test-makefile runs the shelltier
# command the same way test-slow does, with no script of its own. A hand-written
# list drifted from the runner once, so the delegation is checked.
echo "test-makefile"
if mout=$(dry test-makefile); then
  pass "make -n test-makefile exits 0"
else
  fail "make -n test-makefile exits 0" "$mout"
fi
mbash=$(grep -o -E 'bash [^[:space:]]+' <<<"$mout" || true)
if [[ -z "$mbash" ]] && grep -q -F 'GT_TEST_DOCKER=0 go run ./internal/shelltier/cmd/shelltier --all' <<<"$mout"; then
  pass "test-makefile runs the shelltier runner alone, as test-slow does (gt-tqxdd)"
else
  fail "test-makefile runs the shelltier runner alone, as test-slow does (gt-tqxdd)" "$mout"
fi

# The shell-test list is the runner's manifest (gt-u560q.6). The runner runs with
# an empty --root, so no script runs: each `bash <path>` finds no file, and before
# each one the runner writes `tier-sweep: shell <path> ...` to stderr. Those lines
# are what it starts, so the three entries the old hand list never ran must be
# among them, and the test-makefile recipe names no hand list.
echo "shell-test list"
lroot="$(mktemp -d)"
(cd "$ROOT" && go run ./internal/shelltier/cmd/shelltier --all --root "$lroot" --logdir "$lroot/logs" >/dev/null 2>"$lroot/stderr") || true
started=$(sed -n 's/^tier-sweep: shell \(.*\) \.\.\.$/\1/p' "$lroot/stderr")
missing=""
for s in .githooks/pre-push_test.sh scripts/post-land-shell_test.sh scripts/town_metrics_test.sh; do
  grep -q -x -F "$s" <<<"$started" || missing="$missing $s"
done
if [[ -z "$missing" ]] && ! grep -q -F 'test-makefile.sh' <<<"$mout"; then
  pass "the shelltier runner starts the scripts the old hand list skipped, and the test-makefile recipe names no hand list"
else
  fail "the shelltier runner starts the scripts the old hand list skipped, and the test-makefile recipe names no hand list" "not started by the runner:$missing (run: go run ./internal/shelltier/cmd/shelltier --all)"
fi
rm -rf "$lroot"

# The OpenShell polecat sandbox targets (claude-q3f.9): an operator runs them
# by hand, and deploy/polecat lands from the crew branch, so the recipes are
# checked here rather than by running them.
echo "polecat sandbox targets"
if pout=$(dry polecat-image); then
  pass "make -n polecat-image exits 0"
else
  fail "make -n polecat-image exits 0" "$pout"
fi
pcross=$(grep -c -F 'GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build' <<<"$pout" || true)
if [[ "$pcross" == 2 ]] && grep -q -F './cmd/gt' <<<"$pout" && grep -q -F './plugins/polecat-image' <<<"$pout" && grep -q -F 'go run ./internal/polecat/bdbuild/cmd/build' <<<"$pout"; then
  pass "polecat-image cross-compiles gt and the cache-trim helper for linux/arm64 and builds bd through the bd build helper"
else
  fail "polecat-image cross-compiles gt and the cache-trim helper for linux/arm64 and builds bd through the bd build helper (builds: $pcross)" "$pout"
fi
pdev=$(grep -c -F 'gastown-polecat:dev' <<<"$pout" || true)
if grep -q -E 'docker build --platform linux/arm64 .*-t gastown-polecat:[^ ]+ -t gastown-polecat:dev -f deploy/polecat/Dockerfile deploy/polecat' <<<"$pout" && [[ "$pdev" == 1 ]]; then
  pass "polecat-image builds deploy/polecat/Dockerfile, tagged both the short sha and :dev"
else
  fail "polecat-image builds deploy/polecat/Dockerfile, tagged both the short sha and :dev" "$pout"
fi
# BD_SRC is the only source of bd: a checkout outside this repo.
if bdsrc=$(dry polecat-image BD_SRC=/somewhere/else) && grep -q -F -- '-src "/somewhere/else"' <<<"$bdsrc"; then
  pass "polecat-image takes BD_SRC pointing at another beads checkout"
else
  fail "polecat-image takes BD_SRC pointing at another beads checkout" "$bdsrc"
fi
# The image records the commit its bd was built from, read from the file the
# helper wrote, so a host bd the checkout predates cannot leave the label
# naming another commit (gt-7b7a5).
if bdlabel=$(dry polecat-image BD_SRC=/somewhere/else BD_COMMIT=3b90ab0) \
  && grep -q -F -- '-commit "3b90ab0"' <<<"$bdlabel" \
  && grep -q -F -- 'bd_commit=$(go run ./internal/polecat/bdbuild/cmd/build' <<<"$bdlabel" \
  && grep -q -F -- 'test -n "$bd_commit"' <<<"$bdlabel" \
  && grep -q -F -- 'bd_commit=$(cat deploy/polecat/bin/bd.commit)' <<<"$bdlabel" \
  && grep -q -F -- '--label org.gastown.bd-commit="$bd_commit"' <<<"$bdlabel"; then
  pass "polecat-image labels the image with the bd commit the helper built"
else
  fail "polecat-image labels the image with the bd commit the helper built" "$bdlabel"
fi
if cout=$(dry polecat-caches RIGS="gastown om") \
  && grep -q -F 'go run ./cmd/polecatcaches' <<<"$cout" \
  && grep -q -F -- '-pins deploy/polecat/seed-pins' <<<"$cout" \
  && grep -q -F -- '-claim gt-cache' <<<"$cout" \
  && grep -q -F 'gastown om' <<<"$cout" \
  && ! grep -q -F 'polecat-caches.sh' <<<"$cout"; then
  pass "make -n polecat-caches runs the Go command with the pins, the claim and RIGS"
else
  fail "make -n polecat-caches runs the Go command with the pins, the claim and RIGS" "$cout"
fi
if oout=$(dry openshell-providers) && grep -q -F 'go run ./cmd/openshell-providers' <<<"$oout"; then
  pass "make -n openshell-providers runs the Go command"
else
  fail "make -n openshell-providers runs the Go command" "$oout"
fi
# The seed stage and the pins (gt-ieuvf, gt-1rz2r), run for real against a
# fixture town: the rig being built is seeded from the build commit, every
# pinned rig from its pin and never from a rig's moving HEAD, and a commit the
# checkout does not hold stops the image build.
SEEDTMP="$(mktemp -d)"
seedtown="$SEEDTMP/seedtown"
# mkrig <name> makes a checkout with two commits and prints "<first> <second>".
mkrig() {
  local dir="$seedtown/$1/mayor/rig"
  mkdir -p "$dir"
  git -C "$dir" init -q -b main
  echo one >"$dir/first.txt"
  git -C "$dir" add first.txt
  git -C "$dir" -c user.name=t -c user.email=t@example.invalid commit -q -m first
  local first
  first=$(git -C "$dir" rev-parse HEAD)
  # A canonical checkout keeps origin/main at its pin; the pin-drift guard reads it.
  git -C "$dir" update-ref refs/remotes/origin/main "$first"
  echo two >"$dir/second.txt"
  git -C "$dir" add second.txt
  git -C "$dir" -c user.name=t -c user.email=t@example.invalid commit -q -m second
  echo "$first $(git -C "$dir" rev-parse HEAD)"
}
read -r gastown_first _ < <(mkrig gastown)
read -r pinned newest < <(mkrig fx)
printf '# comment\nfx %s\n' "$pinned" >"$seedtown/pins"
build=$gastown_first
seedmk() { make -C "$ROOT" --no-print-directory POLE_TOWN="$seedtown" POLE_SEED_PINS="$seedtown/pins" POLE_SEED_DIR="$seedtown/out" POLE_SEED_RIGS=fx POLE_COMMIT="$build" "$@"; }
if sout=$(seedmk polecat-seed-stage 2>&1) \
  && [[ -f "$seedtown/out/gastown/first.txt" && ! -e "$seedtown/out/gastown/second.txt" ]] \
  && grep -q -F "gastown at ${gastown_first:0:12} (the build commit)" <<<"$sout"; then
  pass "polecat-seed-stage stages the build rig at the build commit, not the checkout's HEAD"
else
  fail "polecat-seed-stage stages the build rig at the build commit, not the checkout's HEAD" "$sout"
fi
if [[ -f "$seedtown/out/fx/first.txt" && ! -e "$seedtown/out/fx/second.txt" ]]; then
  pass "polecat-seed-stage stages every other rig at its pin, not its newer HEAD"
else
  fail "polecat-seed-stage stages every other rig at its pin, not its newer HEAD" "$sout"
fi
printf 'fx %s\n' "0000000000000000000000000000000000000000" >"$seedtown/pins"
if sout=$(seedmk polecat-seed-stage 2>&1); then
  fail "polecat-seed-stage stops on a pin the checkout does not hold" "exited 0: $sout"
elif grep -q -F 'rig fx pin 000000000000 is not readable in its canonical checkout' <<<"$sout"; then
  pass "polecat-seed-stage stops on a pin the checkout does not hold, naming the rig and sha"
else
  fail "polecat-seed-stage stops on a pin the checkout does not hold, naming the rig and sha" "$sout"
fi
printf 'fx %s\n' "$pinned" >"$seedtown/pins"
build="0000000000000000000000000000000000000000"
if sout=$(seedmk polecat-seed-stage 2>&1); then
  fail "polecat-seed-stage stops on a build commit the checkout does not hold" "exited 0: $sout"
elif grep -q -F 'cannot stage rig gastown at 0000000000000000000000000000000000000000' <<<"$sout"; then
  pass "polecat-seed-stage stops on a build commit the checkout does not hold, naming the rig and sha"
else
  fail "polecat-seed-stage stops on a build commit the checkout does not hold, naming the rig and sha" "$sout"
fi
build=""
if sout=$(seedmk polecat-seed-stage 2>&1); then
  fail "polecat-seed-stage refuses a build commit it cannot read" "exited 0: $sout"
elif grep -q -F 'cannot read the build commit' <<<"$sout"; then
  pass "polecat-seed-stage refuses a build commit it cannot read, naming the reason"
else
  fail "polecat-seed-stage refuses a build commit it cannot read, naming the reason" "$sout"
fi
build=$gastown_first
: >"$seedtown/pins"
if sout=$(seedmk polecat-seed-stage 2>&1); then
  fail "polecat-seed-stage refuses an empty pins file" "exited 0: $sout"
elif grep -q -F 'missing or empty' <<<"$sout"; then
  pass "polecat-seed-stage refuses an empty pins file"
else
  fail "polecat-seed-stage refuses an empty pins file" "$sout"
fi
if sout=$(seedmk polecat-seed-pins 2>&1) && grep -qx "fx $newest" "$seedtown/pins"; then
  pass "polecat-seed-pins pins each seeded rig to its checkout's HEAD"
else
  fail "polecat-seed-pins pins each seeded rig to its checkout's HEAD" "$sout / $(cat "$seedtown/pins")"
fi
rm -rf "$SEEDTMP"
phony="$(sed -n '1p' "$ROOT/Makefile")"
missing=""
# The pin-drift guard (gt-n3ewt) runs inside the stage, and the stage runs before
# the image build: a stale committed pin stops the image before it is built, and
# nothing is staged.
if od=$(dry polecat-image BD_SRC=/somewhere/else) \
  && stage_line=$(grep -n 'internal/polecat/seed/cmd/stage' <<<"$od" | head -1 | cut -d: -f1) \
  && build_line=$(grep -n 'docker build' <<<"$od" | head -1 | cut -d: -f1) \
  && [[ -n "$stage_line" && -n "$build_line" && "$stage_line" -lt "$build_line" ]]; then
  pass "polecat-image runs the seed stage, with the pin-drift guard, before docker build"
else
  fail "polecat-image runs the seed stage, with the pin-drift guard, before docker build" "$od"
fi
gtown2="$SEEDTMP/behindtown"
mkdir -p "$gtown2/fx/mayor/rig"
git -C "$gtown2/fx/mayor/rig" init -q -b main
printf 'module example.invalid/fx\n\ngo 1.21\n' >"$gtown2/fx/mayor/rig/go.mod"
echo one >"$gtown2/fx/mayor/rig/a.txt"
git -C "$gtown2/fx/mayor/rig" add go.mod a.txt
git -C "$gtown2/fx/mayor/rig" -c user.name=t -c user.email=t@example.invalid commit -q -m first
bpin=$(git -C "$gtown2/fx/mayor/rig" rev-parse HEAD)
# main requires a module the pin lacks: the guard must fail on that gap, not on the lag
printf 'module example.invalid/fx\n\ngo 1.21\n\nrequire github.com/x/missing v1.0.0\n' >"$gtown2/fx/mayor/rig/go.mod"
echo two >"$gtown2/fx/mayor/rig/b.txt"
git -C "$gtown2/fx/mayor/rig" add go.mod b.txt
git -C "$gtown2/fx/mayor/rig" -c user.name=t -c user.email=t@example.invalid commit -q -m second
git -C "$gtown2/fx/mayor/rig" update-ref refs/remotes/origin/main "$(git -C "$gtown2/fx/mayor/rig" rev-parse HEAD)"
mkdir -p "$gtown2/gastown/mayor/rig"
git -C "$gtown2/gastown/mayor/rig" init -q -b main
echo g >"$gtown2/gastown/mayor/rig/g.txt"
git -C "$gtown2/gastown/mayor/rig" add g.txt
git -C "$gtown2/gastown/mayor/rig" -c user.name=t -c user.email=t@example.invalid commit -q -m build
gbuild2=$(git -C "$gtown2/gastown/mayor/rig" rev-parse HEAD)
printf 'fx %s\n' "$bpin" >"$gtown2/pins"
if bout=$(make -C "$ROOT" --no-print-directory polecat-seed-stage POLE_TOWN="$gtown2" POLE_SEED_PINS="$gtown2/pins" POLE_SEED_DIR="$gtown2/out" POLE_SEED_RIGS=fx POLE_COMMIT="$gbuild2" 2>&1); then
  fail "a stale committed pin stops the seed stage before anything is staged" "exited 0: $bout"
elif grep -q -F 'pin-drift guard: rig fx pin' <<<"$bout" && grep -q -F 'lacks modules its main' <<<"$bout" && grep -q -F 'github.com/x/missing' <<<"$bout" && [[ ! -e "$gtown2/out/fx" ]]; then
  pass "a stale committed pin stops the seed stage before anything is staged"
else
  fail "a stale committed pin stops the seed stage before anything is staged" "$bout"
fi

# The node prune (gt-e14j6) is its own target, called by polecat-image after the
# build only when POLE_NODE_PRUNE=1: a build leaves the sandbox nodes alone
# unless asked (gt-06ni7). The gate then runs the target against a fake docker:
# the old image is removed, and a node that cannot list its images keeps
# everything and the failure is named.
#
# The default-off case asks the recipe for its default, so it clears the flag and
# any inherited MAKEFLAGS first: a caller that exported POLE_NODE_PRUNE=1 still
# reads the unset default, not its own opt-in (gt-pkl4q).
if pn=$(unset POLE_NODE_PRUNE; MAKEFLAGS=; dry polecat-image BD_SRC=/somewhere/else) && ! grep -q -F 'polecat-node-prune' <<<"$pn"; then
  pass "polecat-image does not call polecat-node-prune by default (gt-06ni7)"
else
  fail "polecat-image does not call polecat-node-prune by default (gt-06ni7)" "$pn"
fi
if pn=$(dry polecat-image BD_SRC=/somewhere/else POLE_NODE_PRUNE=1) \
  && bline=$(grep -n 'docker build' <<<"$pn" | head -1 | cut -d: -f1) \
  && nline=$(grep -n 'polecat-node-prune' <<<"$pn" | head -1 | cut -d: -f1) \
  && [[ -n "$bline" && -n "$nline" && "$nline" -gt "$bline" ]]; then
  pass "polecat-image calls polecat-node-prune after docker build with POLE_NODE_PRUNE=1"
else
  fail "polecat-image calls polecat-node-prune after docker build with POLE_NODE_PRUNE=1" "$pn"
fi
prunebin="$SEEDTMP/prunebin"
mkdir -p "$prunebin"
cat >"$prunebin/docker" <<'FAKE'
#!/bin/sh
# A fake docker for the node-prune test: docker --context X <command> ...
shift 2
case "$1" in
  ps) echo gt-sbx-worker ;;
  exec)
    shift 2
    shift
    case "$1" in
      images) [ -n "$PRUNE_FAIL_IMAGES" ] && exit 1
        echo '{"images":[{"id":"sha256:old","repoTags":[],"repoDigests":["localhost:5001/gastown-polecat@sha256:f627"]}]}' ;;
      inspecti) echo '{"info":{"imageSpec":{"created":"2026-10-25T16:00:00Z"}}}' ;;
      ps) echo '{"containers":[]}' ;;
      rmi) echo "$2" >>"$PRUNE_RMI_LOG" ;;
    esac ;;
esac
FAKE
chmod +x "$prunebin/docker"
rm -f "$SEEDTMP/rmi.log"
if pout=$(PATH="$prunebin:$PATH" PRUNE_RMI_LOG="$SEEDTMP/rmi.log" POLECAT_IMAGE_KEEP=0 make -C "$ROOT" --no-print-directory polecat-node-prune 2>&1) \
  && grep -q -F 'sha256:old' "$SEEDTMP/rmi.log"; then
  pass "polecat-node-prune removes the old polecat image from a node with keep 0"
else
  fail "polecat-node-prune removes the old polecat image from a node with keep 0" "$pout"
fi
rm -f "$SEEDTMP/rmi.log"
if pout=$(PATH="$prunebin:$PATH" PRUNE_FAIL_IMAGES=1 PRUNE_RMI_LOG="$SEEDTMP/rmi.log" POLECAT_IMAGE_KEEP=0 make -C "$ROOT" --no-print-directory polecat-node-prune 2>&1) \
  && [[ ! -e "$SEEDTMP/rmi.log" ]] \
  && grep -q -F 'node prune failed' <<<"$pout"; then
  pass "polecat-node-prune leaves a node it cannot list alone and names the failure"
else
  fail "polecat-node-prune leaves a node it cannot list alone and names the failure" "$pout"
fi

for t in polecat-image polecat-seed-stage polecat-seed-pins polecat-caches openshell-providers; do
  grep -q -E "^$t:" "$ROOT/Makefile" && grep -q -E "^\.PHONY:.* $t( |\$)" <<<"$phony" || missing="$missing $t"
done
if [[ -z "$missing" ]]; then
  pass "the polecat sandbox targets are declared and phony"
else
  fail "the polecat sandbox targets are declared and phony" "missing:$missing"
fi

# make polecat-image without BD_SRC stops at its guard, before go or docker runs:
# the guard is the first recipe line, so a run here builds nothing.
bdrc=0
bdout=$(make -C "$ROOT" --no-print-directory polecat-image BD_SRC= 2>&1) || bdrc=$?
if [[ "$bdrc" == 2 ]] && grep -q -F 'BD_SRC is unset' <<<"$bdout" && ! grep -q -E 'go build|docker build' <<<"$bdout"; then
  pass "polecat-image refuses to run without BD_SRC, before building anything"
else
  fail "polecat-image refuses to run without BD_SRC, before building anything (rc=$bdrc)" "$bdout"
fi

# polecat-image compiles gt from the working tree, so its gt-commit label (HEAD)
# is only true of a tree with no changes. A scratch repo holding the Makefile
# and a compilable cmd/gt shows the guard refusing an untracked file before go
# or docker runs, and letting a clean tree on to the go build. The rig go.mod
# guard sits between the two, so the clean tree is given a fixture town: the
# case checks the clean-tree guard, not whether this host's town holds a rig
# checkout (gt-vw2qu).
pdir="$(mktemp -d)"
cp "$ROOT/Makefile" "$pdir/"
mkdir -p "$pdir/cmd/gt"
printf 'module example.invalid/scratch\n\ngo 1.21\n' >"$pdir/go.mod"
printf 'package main\n\nfunc main() {}\n' >"$pdir/cmd/gt/main.go"
git -C "$pdir" init -q
git -C "$pdir" add Makefile go.mod cmd/gt/main.go
git -C "$pdir" -c user.name=test -c user.email=test@example.invalid commit -q -m init
echo 'package main' >"$pdir/scratch.go"
drc=0
dout=$(make -C "$pdir" --no-print-directory polecat-image BD_SRC=/nonexistent 2>&1) || drc=$?
if [[ "$drc" == 2 ]] && grep -q -F 'uncommitted or untracked changes' <<<"$dout" && ! grep -q -E 'go build|docker build' <<<"$dout"; then
  pass "polecat-image refuses a tree with an untracked file, before building anything"
else
  fail "polecat-image refuses a tree with an untracked file, before building anything (rc=$drc)" "$dout"
fi
rm -f "$pdir/scratch.go"
ptown="$(mktemp -d)"
mkdir -p "$ptown/fx/mayor/rig"
printf 'module example.invalid/fx\n\ngo 1.21\n' >"$ptown/fx/mayor/rig/go.mod"
# make echoes a recipe line before running it, so a match on `go build` in the
# output says only that the line was reached; the binary the recipe writes at
# its -o path shows the build ran. BD_SRC=/nonexistent then fails the next
# line, which is why a run that got this far still exits non-zero (gt-t2smk).
crc=0
cout=$(make -C "$pdir" --no-print-directory polecat-image BD_SRC=/nonexistent POLE_TOWN="$ptown" 2>&1) || crc=$?
built=no
if [[ -x "$pdir/deploy/polecat/bin/gt" ]]; then built=yes; fi
if [[ "$crc" == 2 && "$built" == yes ]] && ! grep -q -F 'uncommitted or untracked' <<<"$cout"; then
  pass "polecat-image lets a clean tree past its guard to the build"
else
  fail "polecat-image lets a clean tree past its guard to the build (rc=$crc, binary=$built)" "$cout"
fi
rm -rf "$pdir" "$ptown"

# The image's bd label is the commit the build helper compiled, carried from the
# helper's stdout into docker's --label (gt-7b7a5). A scratch repo runs the whole
# recipe against stub go and docker: the stub helper prints a known sha, and
# docker's recorded arguments show it in the label. The same run drives the
# helper failing and printing nothing, each of which must stop the recipe before
# docker runs. The stub go logs every invocation, so the node prune is checked
# too: absent from a default build (gt-06ni7), present with POLE_NODE_PRUNE=1,
# and stopped before by each refusal.
lrepo="$(mktemp -d)"
lbin="$(mktemp -d)"
cp "$ROOT/Makefile" "$lrepo/"
mkdir -p "$lrepo/cmd/gt" "$lrepo/deploy/polecat"
printf 'module example.invalid/scratch\n\ngo 1.21\n' >"$lrepo/go.mod"
printf 'package main\n\nfunc main() {}\n' >"$lrepo/cmd/gt/main.go"
printf 'fx 92b9e510ac2f9ff8c80b0291f312ac482bf96c38\n' >"$lrepo/deploy/polecat/seed-pins"
printf '#!/bin/sh\necho 1.21\n' >"$lrepo/deploy/polecat/go-floor.sh"
cat >"$lbin/go" <<'FAKE'
#!/bin/sh
# A fake go: every invocation is logged so a test can see which recipe lines
# ran; any -o path a recipe names is created so the recipe runs on, and the bd
# helper's stdout is STUB_BD_COMMIT, or nothing when that is empty.
[ -n "$GO_ARGS_LOG" ] && printf '%s\n' "$*" >>"$GO_ARGS_LOG"
out=""; prev=""
for a in "$@"; do
  [ "$prev" = "-o" ] && out="$a"
  prev="$a"
done
if [ -n "$out" ]; then mkdir -p "$(dirname "$out")" && : >"$out"; fi
if [ "$1" = "run" ] && [ "$2" = "./internal/polecat/bdbuild/cmd/build" ]; then
  [ -n "$STUB_BD_EXIT" ] && exit "$STUB_BD_EXIT"
  printf '%s\n' "$STUB_BD_COMMIT"
fi
exit 0
FAKE
cat >"$lbin/docker" <<'FAKE'
#!/bin/sh
printf '%s\n' "$*" >>"$DOCKER_ARGS_LOG"
FAKE
chmod +x "$lbin/go" "$lbin/docker"
git -C "$lrepo" init -q
git -C "$lrepo" add -A
git -C "$lrepo" -c user.name=test -c user.email=test@example.invalid commit -q -m init
# POLE_TOWN points into the scratch tree: the recipe reads the town root, and
# the gate must not read the live one.
limg=(make -C "$lrepo" --no-print-directory polecat-image BD_SRC=/somewhere/else
  POLE_TOWN="$lrepo/town" POLE_SEED_DIR="$lrepo/seed" POLE_RIG_GO_MODS="$lrepo/go.mod")
stubcommit=3b90ab0c0ffee0000000000000000000000000aa
# A run leaves the binaries it built in the scratch tree, and the recipe refuses
# a tree with untracked files, so each case starts from the committed tree.
lclean() { rm -rf "$lrepo/deploy/polecat/bin" "$lrepo/deploy/polecat/seed"; }
lrc=0
lout=$(PATH="$lbin:$PATH" STUB_BD_COMMIT="$stubcommit" DOCKER_ARGS_LOG="$lbin/docker.args" GO_ARGS_LOG="$lbin/go.args" "${limg[@]}" 2>&1) || lrc=$?
if [[ "$lrc" == 0 ]] \
  && grep -q -F -- "--label org.gastown.bd-commit=$stubcommit" "$lbin/docker.args" 2>/dev/null; then
  pass "polecat-image labels the image with the commit the bd build helper printed"
else
  fail "polecat-image labels the image with the commit the bd build helper printed (rc=$lrc)" "$lout / $(cat "$lbin/docker.args" 2>/dev/null) / $(cat "$lbin/go.args" 2>/dev/null)"
fi
# The node prune is opt-in: a default build changes the image registry and
# leaves the cluster alone, and POLE_NODE_PRUNE=1 still prunes after it (gt-06ni7).
# The default run clears the flag and MAKEFLAGS, as the dry case does, so an
# opted-in caller's shell cannot turn this case into its own opt-in (gt-pkl4q).
lclean
rm -f "$lbin/docker.args" "$lbin/go.args"
lrc=0
lout=$(unset POLE_NODE_PRUNE; MAKEFLAGS=; PATH="$lbin:$PATH" STUB_BD_COMMIT="$stubcommit" DOCKER_ARGS_LOG="$lbin/docker.args" GO_ARGS_LOG="$lbin/go.args" "${limg[@]}" 2>&1) || lrc=$?
if [[ "$lrc" == 0 ]] && [[ -e "$lbin/docker.args" ]] \
  && ! grep -q -F 'run ./cmd/nodeprune' "$lbin/go.args" 2>/dev/null; then
  pass "a default polecat-image leaves the sandbox nodes alone (gt-06ni7)"
else
  fail "a default polecat-image leaves the sandbox nodes alone (gt-06ni7)" "$lout / $(cat "$lbin/go.args" 2>/dev/null)"
fi
lclean
rm -f "$lbin/docker.args" "$lbin/go.args"
lrc=0
lout=$(PATH="$lbin:$PATH" STUB_BD_COMMIT="$stubcommit" DOCKER_ARGS_LOG="$lbin/docker.args" GO_ARGS_LOG="$lbin/go.args" "${limg[@]}" POLE_NODE_PRUNE=1 2>&1) || lrc=$?
if [[ "$lrc" == 0 ]] && grep -q -F 'run ./cmd/nodeprune' "$lbin/go.args" 2>/dev/null; then
  pass "polecat-image prunes the sandbox nodes after the build with POLE_NODE_PRUNE=1 (gt-06ni7)"
else
  fail "polecat-image prunes the sandbox nodes after the build with POLE_NODE_PRUNE=1 (gt-06ni7)" "$lout / $(cat "$lbin/go.args" 2>/dev/null)"
fi
# A failed helper and an empty commit each stop the recipe before docker, so no
# image is built carrying a label for a commit that was not compiled, and before
# the node prune the run asked for.
lclean
rm -f "$lbin/docker.args" "$lbin/go.args"
lrc=0
lout=$(PATH="$lbin:$PATH" STUB_BD_EXIT=1 DOCKER_ARGS_LOG="$lbin/docker.args" GO_ARGS_LOG="$lbin/go.args" "${limg[@]}" POLE_NODE_PRUNE=1 2>&1) || lrc=$?
if [[ "$lrc" != 0 ]] && grep -q -F 'the bd build failed' <<<"$lout" && [[ ! -e "$lbin/docker.args" ]] \
  && ! grep -q -F 'run ./cmd/nodeprune' "$lbin/go.args" 2>/dev/null; then
  pass "polecat-image stops before docker when the bd build fails"
else
  fail "polecat-image stops before docker when the bd build fails (rc=$lrc)" "$lout / $(cat "$lbin/go.args" 2>/dev/null)"
fi
lclean
rm -f "$lbin/docker.args" "$lbin/go.args"
lrc=0
lout=$(PATH="$lbin:$PATH" STUB_BD_COMMIT= DOCKER_ARGS_LOG="$lbin/docker.args" GO_ARGS_LOG="$lbin/go.args" "${limg[@]}" POLE_NODE_PRUNE=1 2>&1) || lrc=$?
if [[ "$lrc" != 0 ]] && grep -q -F 'printed no commit' <<<"$lout" && [[ ! -e "$lbin/docker.args" ]] \
  && ! grep -q -F 'run ./cmd/nodeprune' "$lbin/go.args" 2>/dev/null; then
  pass "polecat-image refuses an empty bd commit before docker builds the image"
else
  fail "polecat-image refuses an empty bd commit before docker builds the image (rc=$lrc)" "$lout / $(cat "$lbin/go.args" 2>/dev/null)"
fi
rm -rf "$lrepo" "$lbin"

# The container suite's test-Dolt init pool must be as wide as the scheduler
# town slot count (gt-ik4a1.4.12). Package beads reads the pool override in
# init(), so TestMain is too late: it has to be in the environment before the
# test binary starts, which is the make target. The count is read from
# schedulerTownSlots, the channel that defines it, so raising that cap without
# the recipes fails here.
town_slots="$(sed -n 's/.*schedulerTownSlots = make(chan struct{}, \([0-9][0-9]*\)).*/\1/p' "$ROOT/internal/cmd/scheduler_integration_test.go")"
init_pool_lines() { grep -c -F -- "GT_TEST_DOLT_INIT_CONCURRENCY=$town_slots" <<<"$1" || true; }

echo "test-integration"
if iout=$(dry test-integration); then
  pass "make -n test-integration exits 0"
else
  fail "make -n test-integration exits 0" "$iout"
fi
if grep -q -F -- '-tags integration' <<<"$iout" && grep -q -F 'GT_TEST_DOCKER=1' <<<"$iout"; then
  pass "test-integration runs the integration tag with containers on"
else
  fail "test-integration runs the integration tag with containers on" "$iout"
fi
# The tier is every integration-tagged test, not the ones whose name a filter
# admits (gt-ik4a1.2): a name filter is how ~120 tests ran nowhere.
no_run_filter() { ! grep -q -E -- '(^|[[:space:]])-(test\.)?run[ =]' <<<"$1"; }
if no_run_filter "$iout"; then
  pass "test-integration passes no -run name filter"
else
  fail "test-integration passes no -run name filter" "$iout"
fi
# The check itself is exercised on the shape it exists to catch: the recipe
# this target carried before gt-ik4a1.2, whose -run ^TestIntegration admitted
# only the tests named TestIntegration* and left every other integration-tagged
# test running nowhere.
if no_run_filter "GT_TEST_DOCKER=1 go test -tags integration -run '^TestIntegration' -timeout 20m ./..."; then
  fail "the no-filter check rejects the pre-gt-ik4a1.2 recipe that passed -run"
else
  pass "the no-filter check rejects the pre-gt-ik4a1.2 recipe that passed -run"
fi
missing=""
while IFS= read -r pkg; do
  pkg="${pkg%%#*}"
  pkg="$(echo "$pkg" | tr -d '[:space:]')"
  [[ -z "$pkg" ]] && continue
  grep -q -F "./$pkg" <<<"$iout" || missing="$missing $pkg"
done <"$ROOT/internal/testpolicy/docker.txt"
if [[ -z "$missing" ]]; then
  pass "test-integration runs every package in internal/testpolicy/docker.txt"
else
  fail "test-integration runs every package in internal/testpolicy/docker.txt; missing:$missing" "$iout"
fi
# The tracker module's integration tier (a real postgres) runs in the same
# target, after the root packages, and its failure fails the target.
if grep -q -F 'for m in tracker; do' <<<"$iout" && grep -q -F 'go vet -tags integration ./... && go test -tags integration -timeout 20m ./...' <<<"$iout"; then
  pass "test-integration also vets and tests the tracker module with the integration tag"
else
  fail "test-integration also vets and tests the tracker module with the integration tag" "$iout"
fi
if [[ -n "$town_slots" && "$(init_pool_lines "$iout")" == 2 ]]; then
  pass "test-integration sets the test-Dolt init pool to the town slot count ($town_slots) on both commands"
else
  fail "test-integration sets the test-Dolt init pool to the town slot count (read '$town_slots' from schedulerTownSlots)" "$iout"
fi

echo "test-integration-wall"
if wout=$(dry test-integration-wall); then
  pass "make -n test-integration-wall exits 0"
else
  fail "make -n test-integration-wall exits 0" "$wout"
fi
# The wall report is only worth reading if it measures the tier the cadence
# runs (gt-ik4a1.4.5), so each of test-integration's two suite commands must
# reappear verbatim but for -json and -count=1. A suite command carries the
# container opt-in and the test-Dolt init pool ahead of go test (gt-ik4a1.4.12),
# so match on the opt-in and `go test` rather than a fixed prefix.
wall_missing=""
suite_seen=0
while IFS= read -r line; do
  [[ "$line" != *'GT_TEST_DOCKER=1'* || "$line" != *'go test '* ]] && continue
  suite_seen=$((suite_seen + 1))
  want="${line/'go test '/'go test -json -count=1 '}"
  grep -q -F -- "$want" <<<"$wout" || wall_missing="$wall_missing [$want]"
done <<<"$iout"
if [[ "$suite_seen" == 2 && -z "$wall_missing" ]]; then
  pass "test-integration-wall runs test-integration's two go test commands, each -json -count=1"
else
  fail "test-integration-wall runs test-integration's two go test commands, each -json -count=1 (suite lines: $suite_seen); missing:$wall_missing" "$wout"
fi
# -count=1 keeps the wall an uncached run (gt-gq4gl): the number decides the
# cadence switch (gt-ik4a1.4), and without it a second run of unchanged source
# is served from the Go test cache, whose wall can sit far under a cold run's
# (6.6s cached against 209s cold on 2026-10-03). Both commands, exactly.
wall_uncached() { [[ "$(grep -c -E -- 'go test -json -count=1' <<<"$1")" == 2 ]]; }
if wall_uncached "$wout"; then
  pass "test-integration-wall is an uncached run: both commands carry -count=1 (gt-gq4gl)"
else
  fail "test-integration-wall is an uncached run: both commands carry -count=1 (gt-gq4gl)" "$wout"
fi
# The check is exercised on the shape it exists to catch: the pre-gt-gq4gl
# recipe, whose two commands took -json and no -count.
if wall_uncached "GT_TEST_DOCKER=1 go test -json -tags integration -timeout 20m ./..."; then
  fail "the -count=1 check rejects the pre-gt-gq4gl wall recipe"
else
  pass "the -count=1 check rejects the pre-gt-gq4gl wall recipe"
fi
# The flag rides after $(INTEGRATION_GO_TEST), not inside the default, or a CI
# runner swap would drop it and silently restore the cached measurement.
if wswap=$(dry test-integration-wall INTEGRATION_GO_TEST="go test -x") && [[ "$(grep -c -F -- 'go test -x -json -count=1' <<<"$wswap")" == 2 ]]; then
  pass "INTEGRATION_GO_TEST still swaps the wall's runner, and -count=1 rides after it (gt-gq4gl)"
else
  fail "INTEGRATION_GO_TEST still swaps the wall's runner, and -count=1 rides after it (gt-gq4gl)" "$wswap"
fi
if grep -q -F -- '-count=1' <<<"$(grep -E '^INTEGRATION_GO_TEST' "$ROOT/Makefile" || true)"; then
  fail "the runner default (INTEGRATION_GO_TEST) carries no -count=1; the wall's recipe does"
else
  pass "the runner default (INTEGRATION_GO_TEST) carries no -count=1; the wall's recipe does"
fi
wall_runs=$(grep -c -E -- 'GT_TEST_DOCKER=1.*go test -json' <<<"$wout" || true)
wall_tool=$(grep -c -F 'cmd/tierwall' <<<"$wout" || true)
# set -o pipefail is what keeps a go test that died before emitting a FAIL
# event red; without it the pipeline's exit status is tierwall's alone.
if [[ "$wall_runs" == 2 && "$wall_tool" == 1 ]] && grep -q -F 'set -o pipefail' <<<"$wout" && grep -q -F 'test-integration-wall: internal/testpolicy/docker.txt lists no package' <<<"$wout" && no_run_filter "$wout"; then
  pass "test-integration-wall pipes both commands through one tierwall run under pipefail, containers on, no -run filter"
else
  fail "test-integration-wall pipes both commands through one tierwall run under pipefail, containers on, no -run filter (lines: runs=$wall_runs tierwall=$wall_tool)" "$wout"
fi
if [[ -n "$town_slots" && "$(init_pool_lines "$wout")" == 2 ]]; then
  pass "test-integration-wall sets the test-Dolt init pool to the town slot count ($town_slots) on both commands"
else
  fail "test-integration-wall sets the test-Dolt init pool to the town slot count (read '$town_slots' from schedulerTownSlots)" "$wout"
fi

echo "gate: failure paths, driven with stubs"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"
# The go stub logs each call with the container opt-in it saw, and fails the
# subcommand named in STUB_GO_FAIL (build or test).
cat >"$TMP/bin/go" <<'STUB'
#!/usr/bin/env bash
echo "go $1 GT_TEST_DOCKER=${GT_TEST_DOCKER:-unset}" >>"$STUB_LOG"
[[ "${PWD##*/}" == tracker ]] && echo "go $1 in tracker" >>"$STUB_LOG"
if [[ "$1" == test && -n "${STUB_GO_INTERRUPT:-}" ]]; then
  # Signal the recipe shell (the parent), then wait to be killed.
  kill -TERM "$PPID"
  sleep 20
  echo "go test survived the interrupt" >>"$STUB_LOG"
fi
# STUB_GO_FAIL_DIR limits the failure to a run inside that directory name, or
# (with a leading !) to a run anywhere else.
dir_ok=1
if [[ -n "${STUB_GO_FAIL_DIR:-}" ]]; then
  if [[ "$STUB_GO_FAIL_DIR" == '!'* ]]; then
    [[ "${PWD##*/}" != "${STUB_GO_FAIL_DIR#!}" ]] || dir_ok=0
  else
    [[ "${PWD##*/}" == "$STUB_GO_FAIL_DIR" ]] || dir_ok=0
  fi
fi
[[ "$1" == "${STUB_GO_FAIL:-}" && $dir_ok == 1 ]] && exit 1
exit 0
STUB
cat >"$TMP/bin/golangci-lint" <<'STUB'
#!/usr/bin/env bash
echo "golangci-lint $*" >>"$STUB_LOG"
[[ "${PWD##*/}" == tracker ]] && echo "golangci-lint in tracker" >>"$STUB_LOG"
[[ "$1" == run && -n "${STUB_LINT_FAIL:-}" ]] && exit 1
[[ "$1" == run && "${PWD##*/}" == "${STUB_LINT_FAIL_DIR:-}" ]] && exit 1
exit 0
STUB
cat >"$TMP/shell-tests.sh" <<'STUB'
#!/usr/bin/env bash
echo "shell-tests" >>"$STUB_LOG"
[[ -n "${STUB_SHELL_FAIL:-}" ]] && exit 1
exit 0
STUB
# The exec-tax preflight, which the real gate reaches through `go run`: the
# go stub would swallow that call, so every case below points
# EXEC_TAX_PREFLIGHT at this stub. It warns only when EXEC_TAX_WARN is set,
# and exits 0 either way.
cat >"$TMP/preflight.sh" <<'STUB'
#!/usr/bin/env bash
if [[ -n "${EXEC_TAX_WARN:-}" ]]; then
  echo "gate: WARNING exec tax 180 ms/exec in this process tree: a fresh executable here waits on a macOS scan or a throttled process, and the unit tier builds one per package (see gt-2ycne.1)" >&2
fi
exit 0
STUB
chmod +x "$TMP/bin/go" "$TMP/bin/golangci-lint" "$TMP/preflight.sh" "$TMP/shell-tests.sh"

# run_gate [make flags] -- VAR=value... runs the real gate recipe against the
# stubs, with an inherited GT_TEST_DOCKER=1 the recipe must override, and
# prints its exit code; the log of stub calls is in $TMP/calls, stderr in
# $TMP/err. Cases past lint pass `-o lint` (make treats the target as up to
# date), and the lint case `-o docs-lint`, so the real docs-lint (~20 s) runs
# once, in the green case.
run_gate() {
  local flags=()
  while [[ $# -gt 0 && "$1" != -- ]]; do flags+=("$1"); shift; done
  [[ $# -gt 0 ]] && shift
  : >"$TMP/calls"
  local rc=0
  env "$@" STUB_LOG="$TMP/calls" GT_TEST_DOCKER=1 PATH="$TMP/bin:$PATH" \
    make -C "$ROOT" --no-print-directory "${flags[@]}" "${TARGET:-gate}" SHELL_TESTS="$TMP/shell-tests.sh" EXEC_TAX_PREFLIGHT="$TMP/preflight.sh" >"$TMP/out" 2>"$TMP/err" || rc=$?
  echo "$rc"
}

rc=$(run_gate)
if [[ "$rc" == 0 ]] && grep -q -E 'gate: PASSED in [0-9]+s wall' "$TMP/err" && grep -q -x 'go test GT_TEST_DOCKER=0' "$TMP/calls" && ! grep -q -x 'shell-tests' "$TMP/calls" && grep -q -F 'golangci-lint run --timeout=5m --allow-serial-runners' "$TMP/calls"; then
  pass "green stubs: exit 0 with the wall printed, lint waited on the lock, only the Go suite ran, and it saw GT_TEST_DOCKER=0 despite an inherited 1"
else
  fail "green stubs: exit 0 with the wall printed, lint waited on the lock, only the Go suite ran, and it saw GT_TEST_DOCKER=0 despite an inherited 1 (rc=$rc)" "$(cat "$TMP/calls" "$TMP/err")"
fi

# The preflight's warning path: a taxed host says so, and the gate still
# passes. -o lint so the real docs-lint runs once, in the green case above.
rc=$(run_gate -o lint -- EXEC_TAX_WARN=1)
if [[ "$rc" == 0 ]] && grep -q -F 'gate: WARNING exec tax 180 ms/exec' "$TMP/err" && grep -q -E 'gate: PASSED in [0-9]+s wall' "$TMP/err"; then
  pass "a taxed host: the gate warns on the exec tax and still passes (gt-2ycne.1)"
else
  fail "a taxed host: the gate warns on the exec tax and still passes (gt-2ycne.1) (rc=$rc)" "$(cat "$TMP/err")"
fi

rc=$(run_gate -o docs-lint -- STUB_LINT_FAIL=1)
if [[ "$rc" != 0 ]] && grep -q -E '^golangci-lint run' "$TMP/calls" && ! grep -q -E '^go (build|test)' "$TMP/calls"; then
  pass "lint fails: non-zero, nothing built or tested"
else
  fail "lint fails: non-zero, nothing built or tested (rc=$rc)" "$(cat "$TMP/calls")"
fi

rc=$(run_gate -o lint -- STUB_GO_FAIL=build)
if [[ "$rc" != 0 ]] && grep -q -F 'gate: FAILED at build' "$TMP/err" && ! grep -q -E '^go test' "$TMP/calls"; then
  pass "build fails: non-zero, names the build stage, unit tier never starts"
else
  fail "build fails: non-zero, names the build stage, unit tier never starts (rc=$rc)" "$(cat "$TMP/calls" "$TMP/err")"
fi

rc=$(run_gate -o lint -- STUB_GO_FAIL=test)
if [[ "$rc" != 0 ]] && grep -q -F 'gate: FAILED at unit tier (Go suite' "$TMP/err" && ! grep -q -F 'gate: PASSED' "$TMP/err"; then
  pass "Go suite fails: non-zero, names the Go half"
else
  fail "Go suite fails: non-zero, names the Go half (rc=$rc)" "$(cat "$TMP/err")"
fi

# An interrupted gate stops the Go suite and exits red. The stub go run
# signals the recipe shell (its parent) and then waits to be killed; a trap
# that did not fire would let it finish and the gate would pass.
# The tracker module is vetted and tested after the root suite, and its failure
# is the Go half's failure: the same wording, a non-zero exit, no PASSED line.
rc=$(run_gate -o lint)
if [[ "$rc" == 0 ]] && grep -q -x 'go vet in tracker' "$TMP/calls" && grep -q -x 'go test in tracker' "$TMP/calls"; then
  pass "green stubs: the tracker module is vetted and tested after the root suite"
else
  fail "green stubs: the tracker module is vetted and tested after the root suite (rc=$rc)" "$(cat "$TMP/calls" "$TMP/err")"
fi

rc=$(run_gate -o lint -- STUB_GO_FAIL=test STUB_GO_FAIL_DIR=tracker)
if [[ "$rc" != 0 ]] && grep -q -F 'gate: FAILED at unit tier (Go suite' "$TMP/err" && ! grep -q -F 'gate: PASSED' "$TMP/err" && grep -q -x 'go test in tracker' "$TMP/calls"; then
  pass "tracker tests fail: non-zero, names the Go half, no PASSED line"
else
  fail "tracker tests fail: non-zero, names the Go half, no PASSED line (rc=$rc)" "$(cat "$TMP/calls" "$TMP/err")"
fi

# A failing root suite stops the gate before the tracker module runs.
rc=$(run_gate -o lint -- STUB_GO_FAIL=test STUB_GO_FAIL_DIR='!tracker')
if [[ "$rc" != 0 ]] && ! grep -q -x 'go vet in tracker' "$TMP/calls"; then
  pass "root suite fails: the tracker module does not run"
else
  fail "root suite fails: the tracker module does not run (rc=$rc)" "$(cat "$TMP/calls" "$TMP/err")"
fi

# make lint also lints the tracker module with the root config; a finding in
# it fails lint, and says which module.
rc=$(run_gate -o docs-lint)
if [[ "$rc" == 0 ]] && grep -q -x 'golangci-lint in tracker' "$TMP/calls" && grep -q -F -- '--config ../.golangci.yml' "$TMP/calls"; then
  pass "lint runs golangci-lint in the tracker module with the root config"
else
  fail "lint runs golangci-lint in the tracker module with the root config (rc=$rc)" "$(cat "$TMP/calls" "$TMP/err")"
fi

rc=$(run_gate -o docs-lint -- STUB_LINT_FAIL_DIR=tracker)
if [[ "$rc" != 0 ]] && grep -q -F 'lint failed in tracker' "$TMP/out" "$TMP/err" && ! grep -q -F 'gate: PASSED' "$TMP/err"; then
  pass "a lint finding in the tracker module fails the gate and names the module"
else
  fail "a lint finding in the tracker module fails the gate and names the module (rc=$rc)" "$(cat "$TMP/calls" "$TMP/out" "$TMP/err")"
fi

# presubmit tests the tracker module only when the branch changed it, and a
# failure there fails presubmit. PRESUBMIT_BASE=HEAD means "nothing changed";
# the repo's first commit stands for a base the module differs from.
BASE_FIRST=$(git -C "$ROOT" rev-list --max-parents=0 HEAD | tail -1)
rc=$(TARGET=presubmit run_gate -o lint PRESUBMIT_BASE="$BASE_FIRST" -- STUB_GO_FAIL=test STUB_GO_FAIL_DIR=tracker)
if [[ "$rc" != 0 ]] && grep -q -F 'presubmit: FAILED at tests (tracker)' "$TMP/err" && grep -q -x 'go test in tracker' "$TMP/calls"; then
  pass "presubmit: a changed tracker module whose tests fail fails presubmit"
else
  fail "presubmit: a changed tracker module whose tests fail fails presubmit (rc=$rc)" "$(cat "$TMP/calls" "$TMP/err")"
fi

rc=$(TARGET=presubmit run_gate -o lint PRESUBMIT_BASE=HEAD -- STUB_GO_FAIL=test STUB_GO_FAIL_DIR=tracker)
if [[ "$rc" == 0 ]] && ! grep -q -x 'go test in tracker' "$TMP/calls"; then
  pass "presubmit: an unchanged tracker module is not tested"
else
  fail "presubmit: an unchanged tracker module is not tested (rc=$rc)" "$(cat "$TMP/calls" "$TMP/err")"
fi

rc=$(TARGET=test-integration run_gate -o lint -- STUB_GO_FAIL=test STUB_GO_FAIL_DIR=tracker)
if [[ "$rc" != 0 ]] && grep -q -F 'test-integration: FAILED in tracker' "$TMP/err" && grep -q -x 'go test in tracker' "$TMP/calls"; then
  pass "test-integration: a failing tracker integration test fails the target and names the module"
else
  fail "test-integration: a failing tracker integration test fails the target and names the module (rc=$rc)" "$(cat "$TMP/calls" "$TMP/err")"
fi

rc=$(run_gate -o lint -- STUB_GO_INTERRUPT=1)
sleep 1
if [[ "$rc" != 0 ]] && ! grep -q -F 'gate: PASSED' "$TMP/err" && ! grep -q -F 'survived' "$TMP/calls"; then
  pass "interrupted gate: non-zero, and the trap stopped the running suite"
else
  fail "interrupted gate: non-zero, and the trap stopped the running suite (rc=$rc)" "$(cat "$TMP/calls" "$TMP/err")"
fi

rc=$(TARGET=test-slow run_gate -o lint)
if [[ "$rc" == 0 ]] && grep -q -x 'go test GT_TEST_DOCKER=0' "$TMP/calls" && grep -q -x 'shell-tests' "$TMP/calls" && grep -q -E 'gate: PASSED in [0-9]+s wall' "$TMP/err" && grep -q -F 'test-slow: PASSED' "$TMP/err"; then
  pass "test-slow green stubs: exit 0, the Go suite and the shell tests ran with GT_TEST_DOCKER=0"
else
  fail "test-slow green stubs: exit 0, the Go suite and the shell tests ran with GT_TEST_DOCKER=0 (rc=$rc)" "$(cat "$TMP/calls" "$TMP/err")"
fi

rc=$(TARGET=test-slow run_gate -o lint -- STUB_GO_FAIL=test)
if [[ "$rc" != 0 ]] && grep -q -F 'gate: FAILED at unit tier (Go suite' "$TMP/err" && ! grep -q -F 'test-slow: PASSED' "$TMP/err"; then
  pass "test-slow Go suite fails: non-zero, the gate names the Go half"
else
  fail "test-slow Go suite fails: non-zero, the gate names the Go half (rc=$rc)" "$(cat "$TMP/calls" "$TMP/err")"
fi

rc=$(TARGET=test-slow run_gate -o lint -- STUB_SHELL_FAIL=1)
if [[ "$rc" != 0 ]] && grep -q -F 'test-slow: FAILED at shell tests' "$TMP/err"; then
  pass "test-slow shell tests fail: non-zero, names the shell tests"
else
  fail "test-slow shell tests fail: non-zero, names the shell tests (rc=$rc)" "$(cat "$TMP/err")"
fi

# scripts/lint-lock-wait.sh against a lint stub that reports the module lock
# held for its first STUB_CONTEND calls (gt-uoppq).
cat >"$TMP/contended-lint" <<'STUB'
#!/usr/bin/env bash
n=$(( $(wc -l <"$STUB_LOG") ))
echo "lint" >>"$STUB_LOG"
if (( n < STUB_CONTEND )); then
  echo "Error: parallel golangci-lint is running"
  exit 3
fi
[[ -n "${STUB_FINDING:-}" ]] && { echo "x.go:1: finding"; exit 1; }
exit 0
STUB
chmod +x "$TMP/contended-lint"
lock_wait() {
  : >"$TMP/calls"
  local rc=0
  env "${@:2}" STUB_LOG="$TMP/calls" LINT_LOCK_POLL=0 bash "$ROOT/scripts/lint-lock-wait.sh" "$1" "$TMP/contended-lint" >"$TMP/out" 2>"$TMP/err" || rc=$?
  echo "$rc"
}

rc=$(lock_wait 5 STUB_CONTEND=2)
if [[ "$rc" == 0 ]] && [[ $(wc -l <"$TMP/calls") -eq 3 ]] && grep -q -F 'retrying in 0s' "$TMP/err"; then
  pass "lint-lock-wait: a lint contended twice is retried and passes"
else
  fail "lint-lock-wait: a lint contended twice is retried and passes (rc=$rc)" "$(cat "$TMP/calls" "$TMP/out" "$TMP/err")"
fi

rc=$(lock_wait 0 STUB_CONTEND=99)
if [[ "$rc" == 3 ]] && [[ $(wc -l <"$TMP/calls") -eq 1 ]] && grep -q -F 'parallel golangci-lint is running' "$TMP/out" && grep -q -F 'nothing was linted' "$TMP/err"; then
  pass "lint-lock-wait: still contended at the wait bound exits with the lint's code and keeps the lock line"
else
  fail "lint-lock-wait: still contended at the wait bound exits with the lint's code and keeps the lock line (rc=$rc)" "$(cat "$TMP/calls" "$TMP/out" "$TMP/err")"
fi

rc=$(lock_wait 5 STUB_CONTEND=0 STUB_FINDING=1)
if [[ "$rc" == 1 ]] && [[ $(wc -l <"$TMP/calls") -eq 1 ]] && grep -q -F 'finding' "$TMP/out"; then
  pass "lint-lock-wait: a lint with findings is never retried"
else
  fail "lint-lock-wait: a lint with findings is never retried (rc=$rc)" "$(cat "$TMP/calls" "$TMP/out" "$TMP/err")"
fi

rc=0
env PATH="$TMP/bin:$PATH" STUB_LOG="$TMP/calls" make -C "$ROOT" --no-print-directory test-integration DOCKER_PKGS= >"$TMP/out" 2>"$TMP/err" || rc=$?
if [[ "$rc" != 0 ]] && grep -q -F 'lists no package' "$TMP/err"; then
  pass "test-integration refuses an empty docker.txt instead of testing nothing"
else
  fail "test-integration refuses an empty docker.txt instead of testing nothing (rc=$rc)" "$(cat "$TMP/err")"
fi

# The Docker volume script is gone with the Docker cache path (gt-s9r1s). The
# Go command that replaced it is pinned above and in its unit tests.
if [[ ! -e "$ROOT/scripts/polecat-caches.sh" ]]; then
  pass "the Docker volume script scripts/polecat-caches.sh is gone"
else
  fail "the Docker volume script scripts/polecat-caches.sh is gone" "$ROOT/scripts/polecat-caches.sh still exists"
fi

echo ""
echo "makefile-gate_test: $PASS passed, $FAIL failed"
[[ $FAIL -eq 0 ]]
