#!/usr/bin/env bash
# Tests for plugins/polecat-image/run.sh (gt-t4k72): stubs stand in for gt,
# docker, git, make, openshell, crane and pgrep and record every call, so the compare,
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
# always-move (every build installs a newer commit), steal-lock (the build ends
# with another run's pid in the plugin's lock), volumes (what `docker volume ls`
# names), volume-ls-fail, volume-missing (names `docker volume inspect`
# refuses), cache-in-use (names `docker ps --filter volume=` reports a container
# for), ps-fail, cache-run-fail (what the trim helper's run prints on stderr
# before failing), cache-run-say (what a helper that exits 0 prints instead of
# running the trim script).
cat >"$BIN/gt" <<'STUB'
#!/usr/bin/env bash
echo "gt version $(cat "$STUB_STATE/installed") (dev: master@$(cat "$STUB_STATE/installed"))"
STUB
cat >"$BIN/docker" <<'STUB'
#!/usr/bin/env bash
# `info` (is the daemon up) and `image inspect --format … <image>` are used
if [ "${1:-}" = info ]; then [ ! -f "$STUB_STATE/docker-down" ]; exit; fi
# Every call is recorded, so a test can assert what was never run (`volume
# prune`, `volume rm`) rather than only what was.
echo "$*" >>"$STUB_STATE/docker-calls"
# `images --format … <repo>` lists the tags in image-tags (newest first); `rmi`
# is recorded in rmi, and refused for the refs listed in rmi-refuse, with the
# reason in rmi-refuse-message (default: an image a running container uses);
# `image inspect --format {{.Id}} <ref>` answers from ids ("<ref> <id>" lines),
# else "id-of-<ref>".
#
# The cache-volume calls: `volume ls` lists `volumes`, `volume inspect` refuses
# those in volume-missing, `ps --filter volume=<v>` prints a container id for
# those in cache-in-use, and `run` executes the trim script for real against
# $STUB_STATE/vol-<rig> — the plugin's own LRU code, not a canned answer — so
# the deletions themselves are asserted.
case "${1:-}" in
  builder)
    echo "$*" >>"$STUB_STATE/builder"
    if [ -f "$STUB_STATE/builder-fail" ]; then echo "Error response from daemon: prune failed" >&2; exit 1; fi
    echo "Total:	12.3GB"
    exit 0 ;;
  images) cat "$STUB_STATE/image-tags" 2>/dev/null || true; exit 0 ;;
  # `tag <src> <dst>` is recorded in tags; `save -o <file> <ref>` writes the
  # tarball the crane stub then reads, so a push of an empty file is caught.
  # tag-fail and save-fail hold the message of a call that fails.
  tag)
    echo "$*" >>"$STUB_STATE/tags"
    if [ -f "$STUB_STATE/tag-fail" ]; then cat "$STUB_STATE/tag-fail" >&2; exit 1; fi
    exit 0 ;;
  save)
    if [ -f "$STUB_STATE/save-fail" ]; then cat "$STUB_STATE/save-fail" >&2; exit 1; fi
    out=""
    while [ $# -gt 0 ]; do case "$1" in -o) out=$2; shift 2 ;; *) shift ;; esac; done
    [ -n "$out" ] && printf 'tarball\n' >"$out"
    exit 0 ;;
  rmi)
    echo "$*" >>"$STUB_STATE/rmi"
    if grep -qx "$2" "$STUB_STATE/rmi-refuse" 2>/dev/null; then
      cat "$STUB_STATE/rmi-refuse-message" 2>/dev/null || echo "conflict: image is being used by running container"
      exit 1
    fi
    exit 0 ;;
  volume)
    case "${2:-}" in
      ls)
        [ ! -f "$STUB_STATE/volume-ls-fail" ] || { echo "Error response from daemon: volume list failed" >&2; exit 1; }
        cat "$STUB_STATE/volumes" 2>/dev/null || true ;;
      inspect)
        if grep -qxF "${3:-}" "$STUB_STATE/volume-missing" 2>/dev/null; then
          echo "Error response from daemon: get ${3:-}: no such volume" >&2
          exit 1
        fi
        echo '[{"Name":"'"${3:-}"'"}]' ;;
    esac
    exit 0 ;;
  ps)
    [ ! -f "$STUB_STATE/ps-fail" ] || { echo "Cannot connect to the Docker daemon" >&2; exit 1; }
    vol="$(printf '%s\n' "$*" | sed -n 's/.*volume=\([^ ]*\).*/\1/p')"
    grep -qxF "$vol" "$STUB_STATE/cache-in-use" 2>/dev/null && echo "c0ffee123456"
    exit 0 ;;
  run)
    if [ -f "$STUB_STATE/cache-run-fail" ]; then
      cat "$STUB_STATE/cache-run-fail" >&2
      exit 125
    fi
    # A helper that exits 0 but prints what cache-run-say holds, to drive the
    # plugin's reading of a line the real script would not print.
    if [ -f "$STUB_STATE/cache-run-say" ]; then
      cat "$STUB_STATE/cache-run-say"
      exit 0
    fi
    vol="" script=""
    while [ $# -gt 0 ]; do
      case "$1" in
        -v) vol="${2%%:*}"; shift 2 ;;
        -e) export "$2"; shift 2 ;;
        -c) script="$2"; shift 2; break ;;
        *) shift ;;
      esac
    done
    [ -n "$vol" ] && [ -n "$script" ] || exit 1
    # The mount point stands in for $STUB_STATE/vol-<rig>: a real volume is
    # always mounted, empty or not, so the directory exists either way.
    export CACHE_ROOT="$STUB_STATE/vol-${vol#gt-cache-}"
    mkdir -p "$CACHE_ROOT"
    exec sh -c "$script" ;;
esac
if [ "${1:-}" = image ] && [ "${4:-}" = '{{range .RepoTags}}{{println .}}{{end}}' ]; then
  if [ -f "$STUB_STATE/repo-tags-fail" ]; then cat "$STUB_STATE/repo-tags-fail" >&2; exit 1; fi
  cat "$STUB_STATE/repo-tags" 2>/dev/null || true
  exit 0
fi
if [ "${1:-}" = image ] && [ "${4:-}" = '{{.Id}}' ]; then
  ref="${5:-}"
  # missing-refs: refs docker has no image for.
  if grep -qxF "$ref" "$STUB_STATE/missing-refs" 2>/dev/null; then echo "Error: No such image: $ref" >&2; exit 1; fi
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
base="$(basename "$(pwd)")"
echo "$base" >"$STUB_STATE/image"
# The tags a real build makes (`:dev` and the short commit), plus the registry
# alias a previous push left on the same image, which `image inspect` reports
# in RepoTags and the plugin must not prefix a second time.
printf 'gastown-polecat:dev\ngastown-polecat:%s\n' "${base:0:7}" >"$STUB_STATE/repo-tags"
[ -z "${POLECAT_IMAGE_REGISTRY:-}" ] || echo "$POLECAT_IMAGE_REGISTRY/gastown-polecat:dev" >>"$STUB_STATE/repo-tags"
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
cat >"$BIN/crane" <<'STUB'
#!/usr/bin/env bash
# `crane push --insecure <tarball> <ref>` and `crane tag --insecure <ref> <tag>`:
# each call is recorded in crane, with a missing-tarball line first when the
# docker save stub wrote nothing (or the plugin removed it too early) for a push.
# crane-fail holds the message of a call that fails.
case "${1:-}" in
  push) [ -s "${3:-}" ] || echo "missing-tarball ${4:-}" >>"$STUB_STATE/crane" ;;
  tag) ;;
  *) echo "unexpected crane call: $*" >&2; exit 2 ;;
esac
echo "$*" >>"$STUB_STATE/crane"
# crane-tag-fail holds the message of a crane tag call that fails; a push still works.
if [ "${1:-}" = tag ] && [ -f "$STUB_STATE/crane-tag-fail" ]; then
  cat "$STUB_STATE/crane-tag-fail" >&2
  exit 1
fi
if [ -f "$STUB_STATE/crane-fail" ]; then
  cat "$STUB_STATE/crane-fail" >&2
  exit 1
fi
exit 0
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
  unset POLECAT_IMAGE_REGISTRY POLECAT_IMAGE_FAIL_CLOSED
  export POLECAT_IMAGE_CRANE="$BIN/crane"
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

echo "--- the build cache is trimmed LRU under the lock, nothing else (gt-atxdz)"
builders() { if [ -f "$STUB_STATE/builder" ]; then tr '\n' '|' <"$STUB_STATE/builder"; fi; }
new_case bcache abc1234 "$(full abc1234)"
run_it
if [ "$RC" = 0 ] && [ "$(builders)" = "builder prune --keep-storage 8GB -f|" ] && grep -q "build cache trimmed to the newest 8GB" <<<"$OUT"; then pass "one 'docker builder prune --keep-storage 8GB -f' per run, logged"; else fail "bcache: rc=$RC builder=[$(builders)] out=$OUT"; fi
if [ ! -d "$LOCK" ]; then pass "the lock the trim took is released"; else fail "bcache: lock left behind"; fi
if ! grep -q -e 'system' -e 'volume' -e 'image prune' -e '--all' "$STUB_STATE/builder"; then pass "no system, volume, image or --all prune"; else fail "bcache ran a wider prune: $(cat "$STUB_STATE/builder")"; fi
new_case bcachekeep abc1234 "$(full abc1234)"
POLECAT_IMAGE_BUILDER_KEEP=3GB run_it
if [ "$RC" = 0 ] && [ "$(builders)" = "builder prune --keep-storage 3GB -f|" ]; then pass "POLECAT_IMAGE_BUILDER_KEEP sets the floor"; else fail "bcachekeep: rc=$RC builder=[$(builders)] out=$OUT"; fi
new_case bcachelock abc1234 "$(full abc1234)"
mkdir "$LOCK"
run_it
if [ "$RC" = 0 ] && [ -z "$(builders)" ] && grep -q "build cache: skipped, another run holds the lock" <<<"$OUT" && [ -d "$LOCK" ]; then pass "a fresh lock held by another run: no trim, lock untouched"; else fail "bcachelock: rc=$RC builder=[$(builders)] out=$OUT"; fi
new_case bcachestale abc1234 "$(full abc1234)"
mkdir "$LOCK"
touch -t 202001010000 "$LOCK"
run_it
if [ "$RC" = 0 ] && [ "$(builders)" = "builder prune --keep-storage 8GB -f|" ] && [ ! -d "$LOCK" ]; then pass "a stale lock is taken over for the trim and released"; else fail "bcachestale: rc=$RC builder=[$(builders)] out=$OUT"; fi
new_case bcachebuilt abc1234 "$(full 1111111)"
run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 1 ] && [ "$(builders)" = "builder prune --keep-storage 8GB -f|" ] && [ ! -d "$LOCK" ]; then pass "a run that built trims once under its own lock and releases it"; else fail "bcachebuilt: rc=$RC builds=$(builds) builder=[$(builders)] out=$OUT"; fi
new_case bcachestolen abc1234 "$(full 1111111)"
# A build long enough for another run to take the lock before it returns.
touch "$STUB_STATE/steal-lock"
run_it
if [ "$RC" = 0 ] && [ -z "$(builders)" ] && grep -q "the lock is another run's now" <<<"$OUT"; then pass "a lock another run took during the build: no trim"; else fail "bcachestolen: rc=$RC builder=[$(builders)] out=$OUT"; fi
new_case bcachedown abc1234
touch "$STUB_STATE/docker-down"
run_it
if [ "$RC" = 0 ] && [ -z "$(builders)" ]; then pass "docker down: no trim, exit 0"; else fail "bcachedown: rc=$RC builder=[$(builders)] out=$OUT"; fi
new_case bcachefail abc1234 "$(full abc1234)"
touch "$STUB_STATE/builder-fail"
run_it
if [ "$RC" = 0 ] && grep -q "build cache not trimmed: Error response from daemon: prune failed" <<<"$OUT" && [ ! -d "$LOCK" ]; then pass "a failing prune is logged, exit 0, lock released"; else fail "bcachefail: rc=$RC out=$OUT"; fi
new_case bcachebuildfail abc1234 "$(full 1111111)"
touch "$STUB_STATE/make-fail"
run_it
if [ "$RC" != 0 ] && [ -z "$(builders)" ] && [ ! -d "$LOCK" ]; then pass "a failed build trims nothing and releases the lock"; else fail "bcachebuildfail: rc=$RC builder=[$(builders)] lock=$([ -d "$LOCK" ] && echo held || echo free) out=$OUT"; fi
# The failure must not cost the next run its trim: that run takes the lock and
# trims, which is the code the failed run never reached.
rm -f "$STUB_STATE/make-fail"
run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 2 ] && [ "$(builders)" = "builder prune --keep-storage 8GB -f|" ]; then pass "the run after a failed build builds and trims"; else fail "bcachebuildfail next: rc=$RC builds=$(builds) builder=[$(builders)] out=$OUT"; fi

echo "--- each gt-cache-<rig> volume is bounded, least recently accessed first (gt-3g7p2)"
# The docker stub runs the plugin's own trim script against $STUB_STATE/vol-<rig>,
# so these cases assert the files that survive, not a canned helper answer.
# cache_case <name> <installed> [label]: a town with two cache volumes.
cache_case() {
  new_case "$@"
  printf '%s\n' gt-cache-gastown gt-cache-devops >"$STUB_STATE/volumes"
}
cache_dir() { echo "$STUB_STATE/vol-$1"; }
# cache_fill <rig> <count>: COUNT files of 4096 bytes under go-build, one per
# day from oldest, plus one module file under go/pkg/mod. 4096 bytes is the
# usual block, so du's KiB figure moves by a whole 4 per file on any host.
cache_fill() {
  local dir i
  dir="$(cache_dir "$1")"
  mkdir -p "$dir/go-build/aa" "$dir/go/pkg/mod/example.com"
  for i in $(seq 1 "$2"); do
    head -c 4096 /dev/zero >"$dir/go-build/aa/f$i"
    touch -t "$(printf '202601%02d0000' $((10 + i)))" "$dir/go-build/aa/f$i"
  done
  head -c 100 /dev/zero >"$dir/go/pkg/mod/example.com/mod.zip"
}
# cache_kib is the same figure the trim measures: du's KiB, not du -sb, which
# does not exist on macOS where this suite also runs.
cache_kib() { du -sk "$(cache_dir "$1")" | cut -f1; }
cache_has() { [ -f "$(cache_dir "$1")/go-build/aa/f$2" ]; }
# cache_cap <rig> <files to free>: a byte cap that leaves exactly that many
# 4096-byte files to delete, the trim's own arithmetic counted in bytes.
cache_cap() { echo $(( $(cache_kib "$1") * 1024 - $2 * 4096 )); }

cache_case cacheunder abc1234 "$(full abc1234)"
cache_fill gastown 3
POLECAT_CACHE_CAP=1M run_it
if [ "$RC" = 0 ] && grep -q "cache: gt-cache-gastown: .* under the cap" <<<"$OUT" && grep -q "cache: gt-cache-devops: .* under the cap" <<<"$OUT" && cache_has gastown 1 && cache_has gastown 3; then pass "under the cap: measured, nothing deleted, every volume checked"; else fail "cacheunder: rc=$RC out=$OUT"; fi
if ! grep -q 'cache: gt-cache-.*: trimmed' <<<"$OUT" && cache_has gastown 1 && cache_has gastown 3; then pass "under the cap: the helper measures and reports, and deletes nothing"; else fail "cacheunder: deleted below the cap, out=$OUT"; fi

cache_case cachetrim abc1234 "$(full abc1234)"
cache_fill gastown 10
CAP="$(cache_cap gastown 6)"
POLECAT_CACHE_CAP="$CAP" run_it
AFTER_KIB="$(cache_kib gastown)"
if [ "$RC" = 0 ] && [ "$((AFTER_KIB * 1024))" -le "$CAP" ] && grep -q "cache: gt-cache-gastown: trimmed .* (cap $CAP)" <<<"$OUT"; then pass "over the cap and idle: trimmed back under it"; else fail "cachetrim: rc=$RC after=${AFTER_KIB}KiB cap=$CAP out=$OUT"; fi
if ! cache_has gastown 1 && ! cache_has gastown 6 && cache_has gastown 7 && cache_has gastown 10; then pass "the six least recently accessed files went, the four newest stayed"; else fail "cachetrim LRU: $(ls "$(cache_dir gastown)/go-build/aa" | tr '\n' ' ')"; fi
if [ -f "$(cache_dir gastown)/go/pkg/mod/example.com/mod.zip" ]; then pass "go/pkg/mod is left alone while the build cache can reach the cap"; else fail "cachetrim deleted mod.zip"; fi
if [ ! -d "$LOCK" ] && [ -d "$(cache_dir gastown)" ] && ! grep -qE 'volume (prune|rm)' "$STUB_STATE/docker-calls"; then pass "lock released, the volume itself never removed, nothing pruned"; else fail "cachetrim: lock=$([ -d "$LOCK" ] && echo held) calls=$(cat "$STUB_STATE/docker-calls")"; fi
if grep -q '^run --rm --read-only .*-v gt-cache-gastown:' "$STUB_STATE/docker-calls"; then pass "the helper runs from the polecat image, read-only apart from the volume"; else fail "cachetrim run line: $(grep '^run ' "$STUB_STATE/docker-calls")"; fi

cache_case cacheinuse abc1234 "$(full abc1234)"
cache_fill gastown 10
echo gt-cache-gastown >"$STUB_STATE/cache-in-use"
POLECAT_CACHE_CAP="$(cache_cap gastown 6)" run_it
if [ "$RC" = 0 ] && grep -q "cache: gt-cache-gastown: in use by 1 container(s); skipped" <<<"$OUT" && cache_has gastown 1 && cache_has gastown 10; then pass "a volume a container has attached: skipped, nothing deleted"; else fail "cacheinuse: rc=$RC out=$OUT"; fi
if ! grep -q -- '-v gt-cache-gastown:' "$STUB_STATE/docker-calls"; then pass "no helper container is started against a volume in use"; else fail "cacheinuse: the helper ran on gt-cache-gastown"; fi

cache_case cachepsfail abc1234 "$(full abc1234)"
cache_fill gastown 10
touch "$STUB_STATE/ps-fail"
POLECAT_CACHE_CAP="$(cache_cap gastown 6)" run_it
if [ "$RC" = 0 ] && grep -q "cache: gt-cache-gastown: cannot list the containers using it (Cannot connect to the Docker daemon); nothing trimmed" <<<"$OUT" && cache_has gastown 1; then pass "a container query that fails: nothing trimmed, the reason logged"; else fail "cachepsfail: rc=$RC out=$OUT"; fi

cache_case cachedown abc1234
cache_fill gastown 10
touch "$STUB_STATE/docker-down"
run_it
if [ "$RC" = 0 ] && ! grep -q "cache: gt-cache" <<<"$OUT" && cache_has gastown 1; then pass "docker down: nothing trimmed, nothing claimed"; else fail "cachedown: rc=$RC out=$OUT"; fi

cache_case cacherunfail abc1234 "$(full abc1234)"
cache_fill gastown 10
echo "du: cannot read directory '$STUB_STATE/vol-gastown/go-build/aa': Permission denied" >"$STUB_STATE/cache-run-fail"
POLECAT_CACHE_CAP="$(cache_cap gastown 6)" run_it
if [ "$RC" = 0 ] && grep -q "cache: gt-cache-gastown: not trimmed (du: cannot read directory .*Permission denied)" <<<"$OUT" && cache_has gastown 1 && cache_has gastown 10; then pass "an unreadable size: not trimmed, the helper's reason logged"; else fail "cacherunfail: rc=$RC out=$OUT"; fi

cache_case cachecap abc1234 "$(full abc1234)"
cache_fill gastown 10
POLECAT_CACHE_CAP=10XB run_it
if [ "$RC" = 0 ] && grep -q "cache: POLECAT_CACHE_CAP=10XB is not a size such as 10GB; nothing trimmed" <<<"$OUT" && cache_has gastown 1; then pass "a cap that is not a size: nothing trimmed, the typo named"; else fail "cachecap: rc=$RC out=$OUT"; fi

cache_case cachemissing abc1234 "$(full abc1234)"
cache_fill gastown 10
echo gt-cache-gastown >"$STUB_STATE/volume-missing"
POLECAT_CACHE_CAP="$(cache_cap gastown 6)" run_it
if [ "$RC" = 0 ] && grep -q "cache: gt-cache-gastown: cannot inspect the volume (Error response from daemon: get gt-cache-gastown: no such volume); nothing trimmed" <<<"$OUT" && cache_has gastown 1; then pass "a volume that is gone: nothing trimmed, the reason logged"; else fail "cachemissing: rc=$RC out=$OUT"; fi

cache_case cachemod abc1234 "$(full abc1234)"
cache_fill gastown 10
POLECAT_CACHE_CAP=1 run_it
if [ "$RC" = 0 ] && ! cache_has gastown 1 && ! cache_has gastown 10 && [ ! -f "$(cache_dir gastown)/go/pkg/mod/example.com/mod.zip" ] && grep -q "cache: gt-cache-gastown: trimmed .* (cap 1)" <<<"$OUT"; then pass "build cache alone cannot reach the cap: go/pkg/mod goes too"; else fail "cachemod: rc=$RC out=$OUT"; fi

cache_case cachelistfail abc1234 "$(full abc1234)"
cache_fill gastown 10
touch "$STUB_STATE/volume-ls-fail"
POLECAT_CACHE_CAP="$(cache_cap gastown 6)" run_it
if [ "$RC" = 0 ] && grep -q "cache: cannot list the volumes (Error response from daemon: volume list failed); nothing trimmed" <<<"$OUT" && cache_has gastown 1; then pass "a volume listing that fails: nothing trimmed, the reason logged"; else fail "cachelistfail: rc=$RC out=$OUT"; fi

cache_case cachelock abc1234 "$(full abc1234)"
cache_fill gastown 10
mkdir "$LOCK"
POLECAT_CACHE_CAP="$(cache_cap gastown 6)" run_it
if [ "$RC" = 0 ] && grep -q "cache: skipped, another run holds the lock" <<<"$OUT" && cache_has gastown 1 && [ -d "$LOCK" ]; then pass "another run's lock: no trim, lock untouched"; else fail "cachelock: rc=$RC out=$OUT"; fi

cache_case cachesay abc1234 "$(full abc1234)"
cache_fill gastown 10
echo "all done" >"$STUB_STATE/cache-run-say"
POLECAT_CACHE_CAP="$(cache_cap gastown 6)" run_it
if [ "$RC" = 0 ] && grep -q "cache: gt-cache-gastown: not trimmed, the helper printed 'all done'" <<<"$OUT" && cache_has gastown 1; then pass "a helper line the plugin cannot read: logged, never claimed as a trim"; else fail "cachesay: rc=$RC out=$OUT"; fi

cache_case cachestill abc1234 "$(full abc1234)"
cache_fill gastown 10
echo "trimmed 5242880 4194304" >"$STUB_STATE/cache-run-say"
POLECAT_CACHE_CAP=1MB run_it
if [ "$RC" = 0 ] && grep -q "cache: gt-cache-gastown: trimmed 5.0MB to 4.0MB (cap 1MB)" <<<"$OUT" && grep -q "cache: gt-cache-gastown: still over the cap with nothing left to trim" <<<"$OUT"; then pass "a trim that ends over the cap: the shortfall is logged"; else fail "cachestill: rc=$RC out=$OUT"; fi

# GNU stat is the image's stat. This shim answers like it: -c takes the format,
# -f means "filesystem" and succeeds with fields that are not file times. Real
# stat does the work, translated when the host's is BSD.
GNU_BIN="$TMP/gnu-bin"
mkdir -p "$GNU_BIN"
cat >"$GNU_BIN/stat" <<'STUB'
#!/bin/sh
REAL=/usr/bin/stat
case "${1:-}" in
  -c)
    if "$REAL" -c '%X' / >/dev/null 2>&1; then exec "$REAL" "$@"; fi
    fmt=$(printf '%s' "$2" | sed 's/%X/%a/g; s/%Y/%m/g; s/%s/%z/g; s/%n/%N/g')
    shift 2
    exec "$REAL" -f "$fmt" "$@" ;;
  -f) echo "999 ?"; exit 0 ;;
esac
exit 1
STUB
chmod +x "$GNU_BIN/stat"
cache_case cachegnu abc1234 "$(full abc1234)"
cache_fill gastown 10
CAP="$(cache_cap gastown 6)"
PATH="$GNU_BIN:$PATH" POLECAT_CACHE_CAP="$CAP" run_it
if [ "$RC" = 0 ] && [ "$(( $(cache_kib gastown) * 1024 ))" -le "$CAP" ] && ! cache_has gastown 1 && ! cache_has gastown 6 && cache_has gastown 7 && cache_has gastown 10; then pass "where stat reads like GNU's: the same oldest files go"; else fail "cachegnu: rc=$RC out=$OUT"; fi

# A noatime mount leaves atime stale; Go touches the mtime of an entry it uses.
# The oldest-named file was just used, so it outlasts files with a newer atime.
cache_case cachemtime abc1234 "$(full abc1234)"
cache_fill gastown 10
touch -m -t 202603010000 "$(cache_dir gastown)/go-build/aa/f1"
POLECAT_CACHE_CAP="$(cache_cap gastown 6)" run_it
if [ "$RC" = 0 ] && cache_has gastown 1 && ! cache_has gastown 2 && ! cache_has gastown 7 && cache_has gastown 8; then pass "a file with a recent mtime outlasts older ones despite an old atime"; else fail "cachemtime: $(ls "$(cache_dir gastown)/go-build/aa" | tr '\n' ' ') out=$OUT"; fi

echo "--- POLECAT_IMAGE_REGISTRY publishes the image it builds (gt-mm7q6)"
# tags_made lists `docker tag <src> <dst>` calls, pushes the refs crane was
# given; the tarball path varies, so only the destination is compared.
tags_made() { if [ -f "$STUB_STATE/tags" ]; then sed 's/^tag //' "$STUB_STATE/tags" | tr '\n' ' '; fi; }
pushes() { if [ -f "$STUB_STATE/crane" ]; then grep '^push ' "$STUB_STATE/crane" | sed 's/^push --insecure [^ ]* //' | tr '\n' ' '; fi; }
# retags lists the registry-side tags crane tag made: "<pushed ref> <new tag>".
retags() { if [ -f "$STUB_STATE/crane" ]; then grep '^tag ' "$STUB_STATE/crane" | sed 's/^tag --insecure //' | tr '\n' ',' ; fi; }

new_case regunset abc1234 "$(full 1111111)"
run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 1 ] && [ ! -f "$STUB_STATE/tags" ] && [ ! -f "$STUB_STATE/crane" ] && ! grep -q "registry:" <<<"$OUT"; then pass "unset: the build as before, nothing tagged or pushed"; else fail "regunset: rc=$RC builds=$(builds) tags=[$(tags_made)] out=$OUT"; fi

new_case regpush abc1234 "$(full 1111111)"
POLECAT_IMAGE_REGISTRY=localhost:5001 run_it
if [ "$RC" = 0 ] && [ "$(tags_made)" = "gastown-polecat:dev localhost:5001/gastown-polecat:dev gastown-polecat:abc1234 localhost:5001/gastown-polecat:abc1234 " ]; then pass ":dev and the commit tag each get the registry reference locally"; else fail "regpush tags: rc=$RC tags=[$(tags_made)] out=$OUT"; fi
if [ "$(pushes)" = "localhost:5001/gastown-polecat:dev " ] && [ "$(retags)" = "localhost:5001/gastown-polecat:dev abc1234," ] && grep -q "registry: pushed localhost:5001/gastown-polecat:dev" <<<"$OUT" && grep -q "registry: tagged localhost:5001/gastown-polecat:abc1234" <<<"$OUT"; then pass "both references are pushed, and the pushes are logged"; else fail "regpush pushes=[$(pushes)] out=$OUT"; fi
if [ -f "$STUB_STATE/crane" ] && ! grep -q missing-tarball "$STUB_STATE/crane"; then pass "crane is handed a tarball docker save wrote"; else fail "regpush tarball: $(cat "$STUB_STATE/crane" 2>/dev/null)"; fi
if ! grep -q "localhost:5001/localhost:5001" <<<"$(tags_made)$(pushes)"; then pass "the alias a previous push left is not prefixed a second time"; else fail "regpush double prefix: tags=[$(tags_made)] pushes=[$(pushes)]"; fi

new_case regcurrent abc1234 "$(full abc1234)"
printf 'gastown-polecat:dev\ngastown-polecat:abc1234\n' >"$STUB_STATE/repo-tags"
POLECAT_IMAGE_REGISTRY=localhost:5001 run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 0 ] && [ "$(pushes)" = "localhost:5001/gastown-polecat:dev " ] && [ "$(retags)" = "localhost:5001/gastown-polecat:dev abc1234," ] && grep -q "current at" <<<"$OUT"; then pass "a current image never published is pushed without a build"; else fail "regcurrent: rc=$RC builds=$(builds) pushes=[$(pushes)] out=$OUT"; fi
POLECAT_IMAGE_REGISTRY=localhost:5001 run_it
if [ "$RC" = 0 ] && [ "$(grep -c '^push ' "$STUB_STATE/crane")" = 1 ] && [ ! -d "$LOCK" ]; then pass "once pushed, a current image is not pushed again, and the lock is released"; else fail "regstamp: rc=$RC pushes=[$(pushes)] out=$OUT"; fi
POLECAT_IMAGE_REGISTRY=localhost:6000 run_it
if [ "$RC" = 0 ] && [ "$(pushes)" = "localhost:5001/gastown-polecat:dev localhost:6000/gastown-polecat:dev " ]; then pass "a different registry is pushed to even though the image is current"; else fail "regother: rc=$RC pushes=[$(pushes)] out=$OUT"; fi

new_case regretry abc1234 "$(full 1111111)"
echo "UNAUTHORIZED: authentication required" >"$STUB_STATE/crane-fail"
POLECAT_IMAGE_REGISTRY=localhost:5001 run_it
rm "$STUB_STATE/crane-fail"
POLECAT_IMAGE_REGISTRY=localhost:5001 run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 1 ] && [ "$(pushes)" = "localhost:5001/gastown-polecat:dev localhost:5001/gastown-polecat:dev " ] && [ "$(retags)" = "localhost:5001/gastown-polecat:dev abc1234," ]; then pass "a failed push is retried by the next run while the image stays current"; else fail "regretry: rc=$RC builds=$(builds) pushes=[$(pushes)] out=$OUT"; fi

new_case reglocked abc1234 "$(full abc1234)"
mkdir "$LOCK"
POLECAT_IMAGE_REGISTRY=localhost:5001 run_it
if [ "$RC" = 0 ] && [ ! -f "$STUB_STATE/crane" ] && [ -d "$LOCK" ] && grep -q "registry: already running" <<<"$OUT"; then pass "a current image with a held lock: the publish waits, lock untouched"; else fail "reglocked: rc=$RC out=$OUT"; fi

new_case regpath abc1234 "$(full 1111111)"
POLECAT_IMAGE_REGISTRY=/Users/x/gt/.runtime/sandboxes run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 1 ] && [ ! -f "$STUB_STATE/tags" ] && [ ! -f "$STUB_STATE/crane" ] && grep -q "is a path, not a host:port (the sandbox records directory is POLECAT_IMAGE_SANDBOX_RECORDS now)" <<<"$OUT"; then pass "an old sandbox-records path in POLECAT_IMAGE_REGISTRY is refused with the new name"; else fail "regpath: rc=$RC tags=[$(tags_made)] out=$OUT"; fi

new_case recordsdir abc1234 "$(full 1111111)"
echo "gt-corpus 2026-10-23 Ready" >"$STUB_STATE/sandboxes"
mkdir -p "$CASE/records"
echo '{}' >"$CASE/records/gt-corpus.json"
POLECAT_IMAGE_SANDBOX_RECORDS="$CASE/records" run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 1 ]; then pass "POLECAT_IMAGE_SANDBOX_RECORDS names the sandbox records directory"; else fail "recordsdir: rc=$RC builds=$(builds) out=$OUT"; fi

new_case regnocrane abc1234 "$(full 1111111)"
POLECAT_IMAGE_REGISTRY=localhost:5001 POLECAT_IMAGE_CRANE="$CASE/no-crane" run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 1 ] && grep -qF "registry: POLECAT_IMAGE_REGISTRY=localhost:5001 is set but $CASE/no-crane is not installed; the image is not published" <<<"$OUT" && [ ! -f "$STUB_STATE/tags" ]; then pass "a missing crane: logged, exit 0, nothing tagged"; else fail "regnocrane: rc=$RC builds=$(builds) tags=[$(tags_made)] out=$OUT"; fi

new_case regfail abc1234 "$(full 1111111)"
echo "UNAUTHORIZED: authentication required" >"$STUB_STATE/crane-fail"
POLECAT_IMAGE_REGISTRY=localhost:5001 run_it
if [ "$RC" = 0 ] && grep -qF "registry: pushing localhost:5001/gastown-polecat:dev failed (UNAUTHORIZED: authentication required); the image is not published" <<<"$OUT" && [ "$(grep -c '^push ' "$STUB_STATE/crane")" = 1 ]; then pass "a failed push: crane's reason logged, exit 0, one attempt"; else fail "regfail: rc=$RC crane=[$(cat "$STUB_STATE/crane" 2>/dev/null)] out=$OUT"; fi
if [ -z "$(rmis)" ] && grep -qF "tag gastown-polecat:dev localhost:5001/gastown-polecat:dev" "$STUB_STATE/tags"; then pass "a failed push removes nothing and keeps the local registry tag"; else fail "regfail removal: rmi=[$(rmis)] tags=[$(tags_made)]"; fi

# Each failing step stops the publish: the later steps never run, the run exits
# 0, nothing is removed, and the next run tries again (no stamp is written).
new_case regtagfail abc1234 "$(full 1111111)"
echo "Error response from daemon: no such image" >"$STUB_STATE/tag-fail"
POLECAT_IMAGE_REGISTRY=localhost:5001 run_it
if [ "$RC" = 0 ] && grep -qF "registry: cannot tag localhost:5001/gastown-polecat:dev (Error response from daemon: no such image); the image is not published" <<<"$OUT" && [ ! -f "$STUB_STATE/crane" ] && ! grep -q '^save' "$STUB_STATE/docker-calls" && [ -z "$(rmis)" ] && [ ! -f "$POLECAT_IMAGE_TOWN_ROOT/.runtime/polecat-image.published" ]; then pass "a failed docker tag: logged, exit 0, no save or push, nothing removed"; else fail "regtagfail: rc=$RC out=$OUT"; fi

new_case regsavefail abc1234 "$(full 1111111)"
echo "write /tmp/image.tar: no space left on device" >"$STUB_STATE/save-fail"
POLECAT_IMAGE_REGISTRY=localhost:5001 run_it
if [ "$RC" = 0 ] && grep -qF "registry: cannot save localhost:5001/gastown-polecat:dev to a tarball (write /tmp/image.tar: no space left on device); the image is not published" <<<"$OUT" && [ ! -f "$STUB_STATE/crane" ] && [ -z "$(rmis)" ] && [ ! -f "$POLECAT_IMAGE_TOWN_ROOT/.runtime/polecat-image.published" ]; then pass "a failed docker save: logged, exit 0, no push, nothing removed"; else fail "regsavefail: rc=$RC out=$OUT"; fi

new_case reginspectfail abc1234 "$(full 1111111)"
echo "Error: No such image: gastown-polecat:dev" >"$STUB_STATE/repo-tags-fail"
POLECAT_IMAGE_REGISTRY=localhost:5001 run_it
if [ "$RC" = 0 ] && grep -qF "registry: cannot read the tags of gastown-polecat:dev (Error: No such image: gastown-polecat:dev); the image is not published" <<<"$OUT" && [ ! -f "$STUB_STATE/tags" ] && [ ! -f "$STUB_STATE/crane" ]; then pass "a failed tag listing: logged, exit 0, nothing tagged or pushed"; else fail "reginspectfail: rc=$RC out=$OUT"; fi

# The tarball directory is made under the build root and removed after every
# publish, whatever its outcome and however many rounds ran.
pushdirs() { find "$BUILD_ROOT" -maxdepth 1 -name 'push.*' 2>/dev/null | wc -l | tr -d ' '; }
new_case regrounds abc1234 "$(full 1111111)"
echo def5678 >"$STUB_STATE/move-after-build"
POLECAT_IMAGE_REGISTRY=localhost:5001 run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 2 ] && [ "$(grep -c '^push ' "$STUB_STATE/crane")" = 2 ] && [ "$(pushdirs)" = 0 ]; then pass "two build rounds publish twice and leave no tarball directory"; else fail "regrounds: rc=$RC builds=$(builds) dirs=$(pushdirs) out=$OUT"; fi
new_case regtagfail abc1234 "$(full 1111111)"
echo "MANIFEST_UNKNOWN: manifest unknown" >"$STUB_STATE/crane-tag-fail"
POLECAT_IMAGE_REGISTRY=localhost:5001 run_it
if [ "$RC" = 0 ] && [ "$(pushes)" = "localhost:5001/gastown-polecat:dev " ] \
  && grep -qF "registry: tagging localhost:5001/gastown-polecat:abc1234 failed (MANIFEST_UNKNOWN: manifest unknown); the image is only partly published" <<<"$OUT" \
  && [ "$(pushdirs)" = 0 ]; then pass "a failed crane tag: the reason is logged, exit 0, and the tarball directory is gone"; else fail "regtagfail: rc=$RC pushes=[$(pushes)] dirs=$(pushdirs) out=$OUT"; fi

# The first tag was pushed before the crane tag failed and no stamp was written,
# so the next run retries the whole publish. The stub's crane log spans both
# runs, so the two pushes and two retags asserted below are one per run.
new_case regretagfail abc1234 "$(full 1111111)"
echo "MANIFEST_UNKNOWN: manifest unknown" >"$STUB_STATE/crane-tag-fail"
POLECAT_IMAGE_REGISTRY=localhost:5001 run_it
FIRST=$OUT
rm "$STUB_STATE/crane-tag-fail"
POLECAT_IMAGE_REGISTRY=localhost:5001 run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 1 ] && grep -qF "the image is only partly published" <<<"$FIRST" \
  && [ "$(pushes)" = "localhost:5001/gastown-polecat:dev localhost:5001/gastown-polecat:dev " ] \
  && [ "$(retags)" = "localhost:5001/gastown-polecat:dev abc1234,localhost:5001/gastown-polecat:dev abc1234," ] \
  && [ -f "$POLECAT_IMAGE_TOWN_ROOT/.runtime/polecat-image.published" ]; then pass "a failed crane tag is retried by the next run, which pushes and tags without a build"; else fail "regretag: rc=$RC builds=$(builds) pushes=[$(pushes)] retags=[$(retags)] out=$OUT"; fi
new_case regleak abc1234 "$(full 1111111)"
echo "UNAUTHORIZED: authentication required" >"$STUB_STATE/crane-fail"
POLECAT_IMAGE_REGISTRY=localhost:5001 run_it
if [ "$RC" = 0 ] && [ "$(pushdirs)" = 0 ]; then pass "a failed push leaves no tarball directory"; else fail "regleak: rc=$RC dirs=$(pushdirs) out=$OUT"; fi

# POLECAT_IMAGE_FAIL_CLOSED=1 (gt-efupr) makes a run that leaves the image
# unbuilt or unpublished fail. install-gt.sh sets it for the build before the
# binary swap, so an install never succeeds with an image that is not in place.
# A deferral leaves the image unbuilt just the same, so under the switch it
# fails too.
echo "--- POLECAT_IMAGE_FAIL_CLOSED=1: an image that is not built and published fails the run"
CLOSED_ERR="ERROR: the image is not published to localhost:5001, so this run fails (POLECAT_IMAGE_FAIL_CLOSED=1)"
CLOSED_PARTLY="ERROR: the image is only partly published to localhost:5001, so this run fails (POLECAT_IMAGE_FAIL_CLOSED=1)"
CLOSED_DEFER="ERROR: the image was not built and published at the installed commit, so this run fails (POLECAT_IMAGE_FAIL_CLOSED=1)"
new_case regclosedpush abc1234 "$(full 1111111)"
echo "UNAUTHORIZED: authentication required" >"$STUB_STATE/crane-fail"
POLECAT_IMAGE_REGISTRY=localhost:5001 POLECAT_IMAGE_FAIL_CLOSED=1 run_it
if [ "$RC" != 0 ] && grep -qF "registry: pushing localhost:5001/gastown-polecat:dev failed (UNAUTHORIZED: authentication required); the image is not published" <<<"$OUT" \
  && grep -qF "$CLOSED_ERR" <<<"$OUT" && [ ! -f "$POLECAT_IMAGE_TOWN_ROOT/.runtime/polecat-image.published" ] \
  && [ "$(pushdirs)" = 0 ] && [ ! -d "$LOCK" ]; then pass "a failed push: the reason and the switch are logged, non-zero, no stamp or tarball left, lock released"; else fail "regclosedpush: rc=$RC dirs=$(pushdirs) lock=$([ -d "$LOCK" ] && echo held) out=$OUT"; fi
new_case regclosedtag abc1234 "$(full 1111111)"
echo "MANIFEST_UNKNOWN: manifest unknown" >"$STUB_STATE/crane-tag-fail"
POLECAT_IMAGE_REGISTRY=localhost:5001 POLECAT_IMAGE_FAIL_CLOSED=1 run_it
if [ "$RC" != 0 ] && grep -qF "registry: tagging localhost:5001/gastown-polecat:abc1234 failed (MANIFEST_UNKNOWN: manifest unknown); the image is only partly published" <<<"$OUT" \
  && grep -qF "$CLOSED_PARTLY" <<<"$OUT" && ! grep -qF "$CLOSED_ERR" <<<"$OUT" && [ "$(pushdirs)" = 0 ]; then pass "a failed crane tag fails the run too, and the error says partly published like the log"; else fail "regclosedtag: rc=$RC dirs=$(pushdirs) out=$OUT"; fi
new_case regclosedcrane abc1234 "$(full 1111111)"
POLECAT_IMAGE_REGISTRY=localhost:5001 POLECAT_IMAGE_CRANE="$CASE/no-crane" POLECAT_IMAGE_FAIL_CLOSED=1 run_it
if [ "$RC" != 0 ] && grep -qF "registry: POLECAT_IMAGE_REGISTRY=localhost:5001 is set but $CASE/no-crane is not installed; the image is not published" <<<"$OUT" \
  && grep -qF "$CLOSED_ERR" <<<"$OUT" && [ ! -f "$STUB_STATE/tags" ]; then pass "a missing crane fails the run instead of exiting 0"; else fail "regclosedcrane: rc=$RC tags=[$(tags_made)] out=$OUT"; fi
new_case regclosedpath abc1234 "$(full 1111111)"
POLECAT_IMAGE_REGISTRY=/Users/x/gt/.runtime/sandboxes POLECAT_IMAGE_FAIL_CLOSED=1 run_it
if [ "$RC" != 0 ] && grep -qF "is a path, not a host:port" <<<"$OUT" && grep -qF "ERROR: the image is not published to /Users/x/gt/.runtime/sandboxes, so this run fails (POLECAT_IMAGE_FAIL_CLOSED=1)" <<<"$OUT"; then pass "a refused registry fails the run"; else fail "regclosedpath: rc=$RC out=$OUT"; fi
new_case regclosedcurrent abc1234 "$(full abc1234)"
printf 'gastown-polecat:dev\ngastown-polecat:abc1234\n' >"$STUB_STATE/repo-tags"
echo "UNAUTHORIZED: authentication required" >"$STUB_STATE/crane-fail"
POLECAT_IMAGE_REGISTRY=localhost:5001 POLECAT_IMAGE_FAIL_CLOSED=1 run_it
if [ "$RC" != 0 ] && [ "$(builds)" = 0 ] && grep -qF "$CLOSED_ERR" <<<"$OUT"; then pass "a current image whose retried publish fails fails the run, with no build"; else fail "regclosedcurrent: rc=$RC builds=$(builds) out=$OUT"; fi
new_case regclosednone abc1234 "$(full 1111111)"
POLECAT_IMAGE_FAIL_CLOSED=1 run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 1 ] && [ ! -f "$STUB_STATE/crane" ] && ! grep -q "registry:" <<<"$OUT"; then pass "with no POLECAT_IMAGE_REGISTRY there is nothing to publish, so the switch changes nothing"; else fail "regclosednone: rc=$RC builds=$(builds) out=$OUT"; fi
new_case regclosedlock abc1234 "$(full abc1234)"
mkdir "$LOCK"
POLECAT_IMAGE_REGISTRY=localhost:5001 POLECAT_IMAGE_FAIL_CLOSED=1 run_it
if [ "$RC" != 0 ] && [ ! -f "$STUB_STATE/crane" ] && [ -d "$LOCK" ] && grep -q "registry: already running" <<<"$OUT" \
  && grep -qF "$CLOSED_DEFER" <<<"$OUT"; then pass "a lock another run holds is a deferral: non-zero, lock untouched, the publish is left to that run"; else fail "regclosedlock: rc=$RC out=$OUT"; fi
new_case regcloseddown abc1234 "$(full 1111111)"
touch "$STUB_STATE/docker-down"
POLECAT_IMAGE_REGISTRY=localhost:5001 POLECAT_IMAGE_FAIL_CLOSED=1 run_it
if [ "$RC" != 0 ] && grep -q "deferred: docker unavailable" <<<"$OUT" && grep -qF "$CLOSED_DEFER" <<<"$OUT"; then pass "docker down fails the run under the switch"; else fail "regcloseddown: rc=$RC out=$OUT"; fi
new_case regclosedsling abc1234 "$(full 1111111)"
touch "$STUB_STATE/sling-running"
POLECAT_IMAGE_REGISTRY=localhost:5001 POLECAT_IMAGE_FAIL_CLOSED=1 run_it
if [ "$RC" != 0 ] && grep -q "deferred: sandbox sling in progress" <<<"$OUT" && grep -qF "$CLOSED_DEFER" <<<"$OUT"; then pass "a running sling fails the run under the switch"; else fail "regclosedsling: rc=$RC out=$OUT"; fi
new_case regclosednotone abc1234 "$(full 1111111)"
echo "UNAUTHORIZED: authentication required" >"$STUB_STATE/crane-fail"
POLECAT_IMAGE_REGISTRY=localhost:5001 POLECAT_IMAGE_FAIL_CLOSED=0 run_it
if [ "$RC" = 0 ] && [ "$(builds)" = 1 ] && grep -q "registry: pushing .* failed" <<<"$OUT" && ! grep -q "POLECAT_IMAGE_FAIL_CLOSED=1" <<<"$OUT"; then pass "only 1 turns the switch on: a failed push stays best effort"; else fail "regclosednotone: rc=$RC builds=$(builds) out=$OUT"; fi

# The registry alias of a per-commit tag goes with the tag: the image stays on
# disk while any tag names it.
tags_case regprune abc1234 "$(full abc1234)"
echo "localhost:5001/gastown-polecat:fffffff6" >"$STUB_STATE/missing-refs"
POLECAT_IMAGE_REGISTRY=localhost:5001 run_it
if [ "$RC" = 0 ] && [ "$(rmis)" = "localhost:5001/gastown-polecat:ddddddd4 gastown-polecat:ddddddd4 gastown-polecat:fffffff6 " ]; then pass "an old tag's registry alias is untagged first; a tag with no alias is untagged alone; :dev's alias stays"; else fail "regprune: rc=$RC rmi=[$(rmis)] out=$OUT"; fi
tags_case regprunekeep abc1234 "$(full abc1234)"
echo "localhost:5001/gastown-polecat:ddddddd4" >"$STUB_STATE/rmi-refuse"
POLECAT_IMAGE_REGISTRY=localhost:5001 run_it
if [ "$RC" = 0 ] && [ "$(rmis)" = "localhost:5001/gastown-polecat:ddddddd4 localhost:5001/gastown-polecat:fffffff6 gastown-polecat:fffffff6 " ] && grep -qF "kept gastown-polecat:ddddddd4 (localhost:5001/gastown-polecat:ddddddd4: rmi failed: conflict: image is being used by running container)" <<<"$OUT"; then pass "an alias docker refuses to untag keeps its tag, and the others still go"; else fail "regprunekeep: rc=$RC rmi=[$(rmis)] out=$OUT"; fi

new_case regbuildfail abc1234 "$(full 1111111)"
touch "$STUB_STATE/make-fail"
POLECAT_IMAGE_REGISTRY=localhost:5001 run_it
if [ "$RC" != 0 ] && [ ! -f "$STUB_STATE/tags" ] && [ ! -f "$STUB_STATE/crane" ]; then pass "a failed build publishes nothing"; else fail "regbuildfail: rc=$RC tags=[$(tags_made)] crane=[$(cat "$STUB_STATE/crane" 2>/dev/null)]"; fi

echo
echo "polecat-image: $PASS passed, $FAIL failed"
[ "$FAIL" = 0 ]
