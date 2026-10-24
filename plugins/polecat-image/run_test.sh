#!/usr/bin/env bash
# Tests for plugins/polecat-image/run.sh (gt-t4k72): stubs stand in for gt,
# docker, git, make, openshell and pgrep and record every call, so the compare,
# the build, the debounce, the lock and the deferral are exercised without a
# docker daemon, a gateway or a rig repo. Everything lives under one temp
# directory; nothing sleeps. Run directly or via the shell tier:
# scripts/tier-sweep.sh discovers it as a `*_test.sh` script.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
RUN="$SCRIPT_DIR/run.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
PASS=0
FAIL=0

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; FAIL=$((FAIL + 1)); }

export STUB_STATE="$TMP/state"
BIN="$TMP/bin"
mkdir -p "$BIN"

# --- Stubs --------------------------------------------------------------------
# State files under $STUB_STATE: installed (the gt commit), image (the label;
# absent = no image), builds (one line per make run: cwd), sandboxes (what
# `openshell sandbox list` prints), sling-running, unknown-commit, make-fail,
# move-after-build (the commit the installed gt becomes after the first build),
# always-move (every build installs a newer commit).
cat >"$BIN/gt" <<'STUB'
#!/usr/bin/env bash
echo "gt version $(cat "$STUB_STATE/installed") (dev: master@$(cat "$STUB_STATE/installed"))"
STUB
cat >"$BIN/docker" <<'STUB'
#!/usr/bin/env bash
# `info` (is the daemon up) and `image inspect --format … <image>` are used
if [ "${1:-}" = info ]; then [ ! -f "$STUB_STATE/docker-down" ]; exit; fi
# `images --format … <repo>` lists the tags in image-tags (newest first); `rmi`
# is recorded in rmi, and refused for the refs listed in rmi-refuse, with the
# reason in rmi-refuse-message (default: an image a running container uses);
# `image inspect --format {{.Id}} <ref>` answers from ids ("<ref> <id>" lines),
# else "id-of-<ref>".
case "${1:-}" in
  images) cat "$STUB_STATE/image-tags" 2>/dev/null || true; exit 0 ;;
  rmi)
    echo "$*" >>"$STUB_STATE/rmi"
    if grep -qx "$2" "$STUB_STATE/rmi-refuse" 2>/dev/null; then
      cat "$STUB_STATE/rmi-refuse-message" 2>/dev/null || echo "conflict: image is being used by running container"
      exit 1
    fi
    exit 0 ;;
esac
if [ "${1:-}" = image ] && [ "${4:-}" = '{{.Id}}' ]; then
  ref="${5:-}"
  id="$(awk -v r="$ref" '$1 == r { print $2 }' "$STUB_STATE/ids" 2>/dev/null || true)"
  echo "${id:-id-of-$ref}"
  exit 0
fi
[ -f "$STUB_STATE/image" ] || { echo "Error: No such image" >&2; exit 1; }
echo "a warning on stderr that must not read as a commit" >&2
cat "$STUB_STATE/image"
STUB
cat >"$BIN/git" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
[ "$1" = "-C" ] && shift 2
case "$1" in
  rev-parse)
    [ ! -f "$STUB_STATE/unknown-commit" ] || { echo "fatal: bad revision" >&2; exit 128; }
    x="${3%%^*}"
    printf '%s%0*d\n' "$x" $((40 - ${#x})) 0 ;;
  worktree)
    case "$2" in
      add) mkdir -p "$4"; echo "add $4 $5" >>"$STUB_STATE/worktrees" ;;
      remove) rm -rf "$4"; echo "remove $4" >>"$STUB_STATE/worktrees" ;;
    esac ;;
esac
STUB
cat >"$BIN/make" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
echo "$(pwd) $*" >>"$STUB_STATE/builds"
if [ -f "$STUB_STATE/make-fail" ]; then
  for i in $(seq 1 20); do echo "build output line $i"; done
  echo "go: build failed" >&2
  exit 2
fi
basename "$(pwd)" >"$STUB_STATE/image"
[ ! -f "$STUB_STATE/sling-after-build" ] || touch "$STUB_STATE/sling-running"
# another run owns the lock by the time this build ends
[ ! -f "$STUB_STATE/steal-lock" ] || echo 99999 >"$POLECAT_IMAGE_TOWN_ROOT/.runtime/polecat-image.lock/pid"
if [ -f "$STUB_STATE/always-move" ]; then
  n=$(wc -l <"$STUB_STATE/builds" | tr -d ' ')
  printf 'f%06d\n' "$n" >"$STUB_STATE/installed"
elif [ -f "$STUB_STATE/move-after-build" ]; then
  mv "$STUB_STATE/move-after-build" "$STUB_STATE/installed"
fi
STUB
cat >"$BIN/openshell" <<'STUB'
#!/usr/bin/env bash
# the real command's two shapes: a NAME header plus rows, or one line
[ ! -f "$STUB_STATE/openshell-fail" ] || { echo "gateway unreachable" >&2; exit 1; }
if [ -f "$STUB_STATE/sandboxes" ]; then
  echo "NAME CREATED PHASE"
  cat "$STUB_STATE/sandboxes"
else
  echo "No sandboxes found."
fi
STUB
cat >"$BIN/pgrep" <<'STUB'
#!/usr/bin/env bash
# pgrep exits 0 on a match, 1 on none, 2 or more when it cannot run
[ ! -f "$STUB_STATE/pgrep-error" ] || exit 2
[ -f "$STUB_STATE/sling-running" ]
STUB
chmod +x "$BIN"/*

# new_case <name> <installed> [image label]: a fresh world for one case.
new_case() {
  CASE="$TMP/$1"
  rm -rf "$CASE"
  mkdir -p "$CASE/state"
  export STUB_STATE="$CASE/state"
  echo "$2" >"$STUB_STATE/installed"
  if [ $# -ge 3 ]; then echo "$3" >"$STUB_STATE/image"; fi
  export POLECAT_IMAGE_TOWN_ROOT="$CASE/town"
  export POLECAT_IMAGE_RIG_REPO="$CASE/rig.git"
  export POLECAT_IMAGE_BD_SRC="$CASE/beads"
  export POLECAT_IMAGE_STALE_MINUTES=20
  export POLECAT_IMAGE_GT="$BIN/gt" POLECAT_IMAGE_DOCKER="$BIN/docker" POLECAT_IMAGE_GIT="$BIN/git"
  export POLECAT_IMAGE_MAKE="$BIN/make" POLECAT_IMAGE_OPENSHELL="$BIN/openshell" POLECAT_IMAGE_PGREP="$BIN/pgrep"
  mkdir -p "$POLECAT_IMAGE_TOWN_ROOT/.runtime"
  LOCK="$POLECAT_IMAGE_TOWN_ROOT/.runtime/polecat-image.lock"
  BUILD_ROOT="$POLECAT_IMAGE_TOWN_ROOT/gastown/.polecat-image-build"
}

# run_it runs the script, keeping its output and exit code.
run_it() { OUT="$("$RUN" 2>&1)" && RC=0 || RC=$?; }

builds() { if [ -f "$STUB_STATE/builds" ]; then wc -l <"$STUB_STATE/builds" | tr -d ' '; else echo 0; fi; }
full() { printf '%s%0*d' "$1" $((40 - ${#1})) 0; }

echo "--- an image at the installed commit is left alone"
new_case current abc1234 "$(full abc1234)"
run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 0 ] && grep -q "current at" <<<"$OUT"; then pass "equal commits (label is the longer form): no build"; else fail "current: rc=$RC builds=$(builds) out=$OUT"; fi

echo "--- a different commit is rebuilt at the installed commit"
new_case drift abc1234 "$(full 1111111)"
run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 1 ]; then pass "one build"; else fail "drift: rc=$RC builds=$(builds) out=$OUT"; fi
if grep -q "polecat-image BD_SRC=$CASE/beads" "$STUB_STATE/builds" && grep -q "^$BUILD_ROOT/$(full abc1234) " "$STUB_STATE/builds"; then pass "built in the detached worktree at the installed commit, with BD_SRC"; else fail "drift build line: $(cat "$STUB_STATE/builds" 2>/dev/null)"; fi
if [ ! -d "$BUILD_ROOT/$(full abc1234)" ] && [ ! -d "$LOCK" ]; then pass "worktree removed and lock released"; else fail "leftovers: $(ls "$BUILD_ROOT" 2>/dev/null) lock=$([ -d "$LOCK" ] && echo held)"; fi
if [ "$(cat "$STUB_STATE/image")" = "$(full abc1234)" ]; then pass "the image now carries the installed commit"; else fail "image label: $(cat "$STUB_STATE/image")"; fi

echo "--- an image without the label is rebuilt"
new_case nolabel abc1234 ""
run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 1 ]; then pass "empty label: build"; else fail "nolabel: rc=$RC builds=$(builds) out=$OUT"; fi
new_case noimage abc1234
run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 1 ]; then pass "no image at all: build"; else fail "noimage: rc=$RC builds=$(builds) out=$OUT"; fi

echo "--- an install that lands during the build is built too"
new_case moved abc1234 "$(full 1111111)"
echo def5678 >"$STUB_STATE/move-after-build"
run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 2 ] && [ "$(cat "$STUB_STATE/image")" = "$(full def5678)" ]; then pass "second build at the newer commit"; else fail "moved: rc=$RC builds=$(builds) image=$(cat "$STUB_STATE/image") out=$OUT"; fi

echo "--- an installed gt that keeps moving stops after 3 builds, loudly"
new_case churn abc1234 "$(full 1111111)"
touch "$STUB_STATE/always-move"
run_it
if [ "$RC" != 0 ] && [ "$(builds)" = 3 ] && grep -q "kept moving" <<<"$OUT"; then pass "3 builds, then an error"; else fail "churn: rc=$RC builds=$(builds) out=$OUT"; fi

echo "--- the lock"
new_case locked abc1234 "$(full 1111111)"
mkdir "$LOCK"
run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 0 ] && grep -q "already running" <<<"$OUT" && [ -d "$LOCK" ]; then pass "a fresh lock: already running, nothing built, lock untouched"; else fail "locked: rc=$RC builds=$(builds) out=$OUT"; fi
new_case stale abc1234 "$(full 1111111)"
mkdir "$LOCK"
touch -t 202001010000 "$LOCK"
run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 1 ] && grep -q "stale lock" <<<"$OUT" && [ ! -d "$LOCK" ]; then pass "a stale lock is taken over and released"; else fail "stale: rc=$RC builds=$(builds) out=$OUT"; fi

echo "--- no rebuild while a sandbox sling could be mid-Prepare"
new_case sling abc1234 "$(full 1111111)"
touch "$STUB_STATE/sling-running"
run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 0 ] && grep -q "deferred: sandbox sling in progress" <<<"$OUT" && [ ! -d "$LOCK" ]; then pass "a running gt sling defers the build"; else fail "sling: rc=$RC builds=$(builds) out=$OUT"; fi
new_case unrecorded abc1234 "$(full 1111111)"
echo "gt-corpus 2026-10-23 Ready" >"$STUB_STATE/sandboxes"
run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 0 ] && grep -q "gt-corpus has no registry record" <<<"$OUT"; then pass "a sandbox with no registry record defers the build"; else fail "unrecorded: rc=$RC builds=$(builds) out=$OUT"; fi
new_case recorded abc1234 "$(full 1111111)"
echo "gt-corpus 2026-10-23 Ready" >"$STUB_STATE/sandboxes"
mkdir -p "$POLECAT_IMAGE_TOWN_ROOT/.runtime/sandboxes"
echo '{}' >"$POLECAT_IMAGE_TOWN_ROOT/.runtime/sandboxes/gt-corpus.json"
run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 1 ]; then pass "a recorded sandbox (a running polecat) does not block the build"; else fail "recorded: rc=$RC builds=$(builds) out=$OUT"; fi

echo "--- a probe that cannot answer defers, it does not read as all clear"
new_case pgreperr abc1234 "$(full 1111111)"
touch "$STUB_STATE/pgrep-error"
run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 0 ] && grep -q "deferred: cannot check for a running gt sling" <<<"$OUT" && [ ! -d "$LOCK" ]; then pass "pgrep failing defers the build"; else fail "pgreperr: rc=$RC builds=$(builds) out=$OUT"; fi
new_case openshellerr abc1234 "$(full 1111111)"
touch "$STUB_STATE/openshell-fail"
run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 0 ] && grep -q "deferred: cannot list sandboxes" <<<"$OUT" && [ ! -d "$LOCK" ]; then pass "openshell failing defers the build"; else fail "openshellerr: rc=$RC builds=$(builds) out=$OUT"; fi

echo "--- a sling that starts during a build holds back the next round"
new_case slingmid abc1234 "$(full 1111111)"
echo def5678 >"$STUB_STATE/move-after-build"
touch "$STUB_STATE/sling-after-build"
run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 1 ] && grep -q "deferred: sandbox sling in progress" <<<"$OUT" && [ ! -d "$LOCK" ]; then pass "one build, then deferred, lock released"; else fail "slingmid: rc=$RC builds=$(builds) out=$OUT"; fi

echo "--- a run releases only a lock it still owns"
new_case stolen abc1234 "$(full 1111111)"
touch "$STUB_STATE/steal-lock"
run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 1 ] && [ -d "$LOCK" ] && [ "$(cat "$LOCK/pid")" = 99999 ]; then pass "another run's lock survives this run's exit"; else fail "stolen: rc=$RC builds=$(builds) pid=$(cat "$LOCK/pid" 2>/dev/null) out=$OUT"; fi
new_case staleaside abc1234 "$(full 1111111)"
mkdir "$LOCK"
touch -t 202001010000 "$LOCK"
run_it
if [ "$RC" = 0 ] && [ ! -e "$LOCK.stale.$$" ] && [ -z "$(ls -d "$LOCK".stale.* 2>/dev/null)" ]; then pass "a takeover leaves no renamed-aside directory behind"; else fail "staleaside leftovers: $(ls -d "$LOCK".stale.* 2>/dev/null)"; fi

echo "--- Docker Desktop stopped is a quiet no-op"
new_case dockerdown abc1234
touch "$STUB_STATE/docker-down"
run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 0 ] && grep -q "deferred: docker unavailable" <<<"$OUT"; then pass "docker down: deferred, exit 0, no build"; else fail "dockerdown: rc=$RC builds=$(builds) out=$OUT"; fi

echo "--- failures are loud and clean up after themselves"
new_case buildfail abc1234 "$(full 1111111)"
touch "$STUB_STATE/make-fail"
run_it
if [ "$RC" != 0 ] && grep -q "make polecat-image failed" <<<"$OUT" && grep -q "build output line 20" <<<"$OUT" && ! grep -q "build output line 1$" <<<"$OUT"; then pass "non-zero, with the tail of the build output"; else fail "buildfail: rc=$RC out=$OUT"; fi
if [ ! -d "$BUILD_ROOT/$(full abc1234)" ] && [ ! -d "$LOCK" ]; then pass "worktree and lock cleaned up after a failed build"; else fail "buildfail leftovers"; fi
new_case unknown abc1234 "$(full 1111111)"
touch "$STUB_STATE/unknown-commit"
run_it
if [ "$RC" != 0 ] && [ "$(builds)" = 0 ] && grep -q "is not in" <<<"$OUT"; then pass "a commit the rig repo does not know is an error, no build"; else fail "unknown: rc=$RC builds=$(builds) out=$OUT"; fi
new_case nogt ""
run_it
if [ "$RC" != 0 ] && grep -q "cannot read the installed gt" <<<"$OUT"; then pass "an unreadable installed commit is an error"; else fail "nogt: rc=$RC out=$OUT"; fi

echo "--- the per-commit images every build leaves behind are untagged, not forced (gt-mexpf)"
# tags_case <name> <installed> [label]: a case whose docker lists these tags,
# newest first. gastown-polecat:dev names the same image as eeeeeee5.
tags_case() {
  new_case "$@"
  printf '%s\n' dev aaaaaaa1 bbbbbbb2 ccccccc3 ddddddd4 eeeeeee5 latest fffffff6 >"$STUB_STATE/image-tags"
  printf '%s\n' "gastown-polecat:dev same-image" "gastown-polecat:eeeeeee5 same-image" >"$STUB_STATE/ids"
}
rmis() { if [ -f "$STUB_STATE/rmi" ]; then sed 's/^rmi //' "$STUB_STATE/rmi" | tr '\n' ' '; fi; }
tags_case prune abc1234 "$(full abc1234)"
run_it
if [ "$RC" = 0 ] && [ "$(rmis)" = "gastown-polecat:ddddddd4 gastown-polecat:fffffff6 " ]; then pass "newest 3 kept; older tags untagged; :dev's image, 'dev' and 'latest' left alone"; else fail "prune: rc=$RC rmi=[$(rmis)] out=$OUT"; fi
if [ -s "$STUB_STATE/rmi" ] && ! grep -q -e ' -f' -e 'force' "$STUB_STATE/rmi"; then pass "docker rmi is called, and never forced"; else fail "no-force: rmi log missing or forced: [$(rmis)]"; fi
tags_case prunekeep abc1234 "$(full abc1234)"
POLECAT_IMAGE_KEEP=1 run_it
if [ "$RC" = 0 ] && [ "$(rmis)" = "gastown-polecat:bbbbbbb2 gastown-polecat:ccccccc3 gastown-polecat:ddddddd4 gastown-polecat:fffffff6 " ]; then pass "POLECAT_IMAGE_KEEP=1 keeps only the newest (and :dev's image)"; else fail "prunekeep: rc=$RC rmi=[$(rmis)] out=$OUT"; fi
tags_case prunerefused abc1234 "$(full abc1234)"
echo "gastown-polecat:ddddddd4" >"$STUB_STATE/rmi-refuse"
run_it
if [ "$RC" = 0 ] && [ "$(rmis)" = "gastown-polecat:ddddddd4 gastown-polecat:fffffff6 " ] && grep -qF "kept gastown-polecat:ddddddd4 (rmi failed: conflict: image is being used by running container)" <<<"$OUT" && grep -q "removed gastown-polecat:fffffff6" <<<"$OUT"; then pass "an image a container uses is kept with docker's refusal, and the others still go"; else fail "prunerefused: rc=$RC rmi=[$(rmis)] out=$OUT"; fi
tags_case prunermierror abc1234 "$(full abc1234)"
echo "gastown-polecat:ddddddd4" >"$STUB_STATE/rmi-refuse"
echo "permission denied while connecting to the Docker daemon" >"$STUB_STATE/rmi-refuse-message"
run_it
if [ "$RC" = 0 ] && grep -qF "kept gastown-polecat:ddddddd4 (rmi failed: permission denied while connecting to the Docker daemon)" <<<"$OUT" && ! grep -q "in use" <<<"$OUT"; then pass "a failed rmi logs docker's own reason, not 'in use'"; else fail "prunermierror: rc=$RC rmi=[$(rmis)] out=$OUT"; fi
tags_case prunebuilt abc1234 "$(full 1111111)"
run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 1 ] && [ "$(rmis)" = "gastown-polecat:ddddddd4 gastown-polecat:fffffff6 " ]; then pass "a run that built also prunes"; else fail "prunebuilt: rc=$RC builds=$(builds) rmi=[$(rmis)] out=$OUT"; fi
tags_case prunedeferred abc1234 "$(full 1111111)"
touch "$STUB_STATE/sling-running"
run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 0 ] && grep -q "deferred: sandbox sling in progress" <<<"$OUT" && [ "$(rmis)" = "gastown-polecat:ddddddd4 gastown-polecat:fffffff6 " ]; then pass "a deferred run still prunes"; else fail "prunedeferred: rc=$RC builds=$(builds) rmi=[$(rmis)] out=$OUT"; fi
tags_case prunedown abc1234
touch "$STUB_STATE/docker-down"
run_it
if [ "$RC" = 0 ] && [ -z "$(rmis)" ]; then pass "docker down: nothing removed, exit 0"; else fail "prunedown: rc=$RC rmi=[$(rmis)] out=$OUT"; fi
new_case prunenone abc1234 "$(full abc1234)"
run_it
if [ "$RC" = 0 ] && [ -z "$(rmis)" ]; then pass "no tags listed: nothing removed"; else fail "prunenone: rc=$RC rmi=[$(rmis)] out=$OUT"; fi
tags_case prunefail abc1234 "$(full 1111111)"
touch "$STUB_STATE/make-fail"
run_it
if [ "$RC" != 0 ] && [ -z "$(rmis)" ]; then pass "a failed build prunes nothing"; else fail "prunefail: rc=$RC rmi=[$(rmis)] out=$OUT"; fi

echo
echo "polecat-image: $PASS passed, $FAIL failed"
[ "$FAIL" = 0 ]
