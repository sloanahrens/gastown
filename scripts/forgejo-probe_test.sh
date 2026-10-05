#!/usr/bin/env bash
# Tests for scripts/forgejo-probe.sh (gt-fn9e6.28). A stub `git` and a stub
# `curl` on PATH stand in for GitHub, the Forgejo git remote and the Forgejo
# API, so the green, red, timeout, refusal and dry-run paths run with no
# network and no live instance. The stub `git` writes the clone's gate workflow
# from a file the case chooses, which is how a copy without gate.yml is
# modelled; the stub `curl` answers status reads from a script of states the
# case sets, which is how pending, success, failure and silence are modelled.
#
# The stub `git` also models the two settings that decide whether a hook runs:
# `worktree add` cuts a worktree of the rig's repository and installs the case's
# hook there, and `push` runs that hook only from such a worktree — what
# core.hooksPath being set in the rig's repository, and nowhere else, means for
# the probe (gt-fn9e6.45).
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROBE="$SCRIPT_DIR/forgejo-probe.sh"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/forgejo-probe-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT
PASS=0
FAIL=0
STUB_ADMIN_TOKEN="stub-admin-token"
API="http://forgejo.test/api/v1"
WEB="http://forgejo.test"
SHA="cafebabecafebabecafebabecafebabecafebabe"
STATE="$TMP/state"
CFG="$TMP/config"
TOWN="$TMP/town"
BARE="$TOWN/rig/.repo.git"

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; FAIL=$((FAIL + 1)); if [ $# -gt 1 ]; then printf '%s\n' "$2" | sed 's/^/    | /'; fi; }
check() { # check DESCRIPTION COMMAND...
  local desc=$1
  shift
  if "$@"; then pass "$desc"; else fail "$desc"; fi
}
contains() { case "$2" in *"$1"*) return 0 ;; *) return 1 ;; esac; }
lacks() { ! contains "$1" "$2"; }
# norm collapses the duplicate slashes a $TMPDIR with a trailing slash leaves
# behind, so a path the stub wrote and the same path git reported as $PWD
# compare equal.
norm() { printf '%s' "$1" | sed 's|//*|/|g'; }
# mode_of prints a file's permission bits in octal; GNU stat and macOS stat
# spell the format differently.
mode_of() { stat -c %a "$1" 2>/dev/null || stat -f %Lp "$1"; }

mkdir -p "$TMP/bin" "$TMP/home" "$TMP/config" "$STATE"
printf 'FORGEJO_ADMIN_TOKEN=%s\n' "$STUB_ADMIN_TOKEN" > "$TMP/admin.env"

# The gate workflow the clone carries, in the shape the design names: name ci,
# one job gate.
cat >"$TMP/gate.yml" <<'YAML'
name: ci
on:
  push:
    branches:
      - 'land/**'
jobs:
  gate:
    runs-on: ci-base
    steps:
      - uses: actions/checkout@v4
      - run: make gate
YAML

# The stub git: clone makes the copy's directory and seeds it with the case's
# hook file, show prints the workflow the case points STUB_GATE_YML at (or fails
# when that file is absent), rev-parse prints STUB_SHA, worktree add creates a
# worktree with that hook installed and worktree remove deletes it, and push
# records its refspec after running the hook a worktree of the rig's repository
# carries. It records a violation when the admin token reaches its argv.
cat >"$TMP/bin/git" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail

printf '%s\n' "$*" >> "$STUB_STATE/git.log"
for a in "$@"; do
  case "$a" in
    *"$STUB_ADMIN_TOKEN"*) echo "admin token in argv: $a" >> "$STUB_STATE/argv-violations" ;;
  esac
done

gitdir=""
if [ "${1:-}" = "-C" ]; then gitdir=$2; shift 2; fi
sub=${1:-}

# seed_hook DIR gives DIR the case's pre-push hook, as a checkout of a tree
# carrying .githooks/ does.
seed_hook() {
  local dir=$1
  [ -n "${STUB_HOOK:-}" ] && [ -f "${STUB_HOOK:-}" ] || return 0
  mkdir -p "$dir/.githooks"
  cp "$STUB_HOOK" "$dir/.githooks/pre-push"
  chmod +x "$dir/.githooks/pre-push"
}

case "$sub" in
  clone)
    mkdir -p "${!#}"
    seed_hook "${!#}"
    ;;
  show)
    spec=${!#}
    case "$spec" in
      *:.forgejo/workflows/gate.yml)
        if [ -n "${STUB_GATE_YML:-}" ] && [ -f "${STUB_GATE_YML:-}" ]; then
          cat "$STUB_GATE_YML"
          exit 0
        fi
        exit 1 ;;
    esac
    exit 1 ;;
  rev-parse)
    case "${!#}" in
      *"$STUB_SHA^{commit}"*) [ -z "${STUB_REPO_LACKS_SHA:-}" ] || exit 1 ;;
    esac
    printf '%s\n' "$STUB_SHA"
    ;;
  worktree)
    action=${2:-}
    case "$action" in
      add)
        shift 2
        dir=""
        while [ $# -gt 0 ]; do
          case "$1" in
            -*) ;;
            *) if [ -z "$dir" ]; then dir=$1; else break; fi ;;
          esac
          shift
        done
        [ -n "$dir" ] || exit 1
        mkdir -p "$dir"
        seed_hook "$dir"
        printf '%s\n' "$dir" >> "$STUB_STATE/worktrees"
        ;;
      remove)
        dir=${!#}
        rm -rf "$dir"
        printf '%s\n' "$dir" >> "$STUB_STATE/worktree-removed"
        ;;
    esac
    ;;
  push)
    refspec=${!#}
    # core.hooksPath is set in the rig's repository alone, so the hook runs only
    # when the push comes from a worktree of it and never from the clone.
    if [ -n "$gitdir" ] && grep -qx "$gitdir" "$STUB_STATE/worktrees" 2>/dev/null \
      && [ -x "$gitdir/.githooks/pre-push" ]; then
      cd "$gitdir" || exit 1
      # git reports the refspec's source as local_ref: the SHA for the probe's
      # own refspec, the remote-tracking ref for the clone's.
      if ! printf '%s %s %s %s\n' "${refspec%%:*}" "$STUB_SHA" "${refspec#*:}" \
        0000000000000000000000000000000000000000 \
        | "$gitdir/.githooks/pre-push" origin "$STUB_SHA"; then
        exit 1
      fi
    fi
    printf '%s\n' "$refspec" >> "$STUB_STATE/pushes"
    ;;
esac
STUB
chmod +x "$TMP/bin/git"

# The stub curl: one Forgejo instance in a directory. Status reads walk the
# lines of $STUB_STATE/status-plan (the last line repeats), so a case scripts
# pending-then-success or standing silence. It records a violation when the
# admin token reaches its argv or when a call carries no curl config file.
cat >"$TMP/bin/curl" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail

orig_args=("$@")
method=GET
resp=/dev/null
url=""
config=""
want_status=0
while [ $# -gt 0 ]; do
  case "$1" in
    -sS) ;;
    -o) resp=$2; shift ;;
    -w) want_status=1 ;;
    -X) method=$2; shift ;;
    -K) config=$2; shift ;;
    -H) shift ;;
    -*) ;;
    *) url=$1 ;;
  esac
  shift
done
path=${url#"$STUB_API"}
path=${path%%\?*}

printf '%s %s\n' "$method" "$path" >> "$STUB_STATE/calls.log"
for arg in "${orig_args[@]}"; do
  case "$arg" in
    *"$STUB_ADMIN_TOKEN"*) echo "admin token in argv: $arg" >> "$STUB_STATE/argv-violations" ;;
  esac
done
if [ -z "$config" ]; then
  echo "no curl config file on $method $path" >> "$STUB_STATE/argv-violations"
fi

send() { # send STATUS BODY
  printf '%s' "$2" > "$resp"
  if [ "$want_status" = 1 ]; then printf '%s' "$1"; fi
}

status_body() { # status_body STATE[,STATE...]
  case "$1" in
    absent) printf '{"state":"pending","sha":"%s","total_count":0,"statuses":[]}' "$STUB_SHA" ;;
    *)
      # A comma-separated list models the repeated probe of an unchanged main:
      # the same context carries more than one status, the later one with the
      # higher id.
      local rest=$1 st id=3 items="" first=1
      while [ -n "$rest" ]; do
        st=${rest%%,*}
        case "$rest" in *,*) rest=${rest#*,} ;; *) rest="" ;; esac
        [ "$first" = 1 ] || items="$items,"
        first=0
        items="$items{\"id\":$id,\"status\":\"$st\",\"context\":\"$STUB_CONTEXT\",\"description\":\"\",\"target_url\":\"$STUB_WEB/acme/rig/actions/runs/12\"}"
        id=$((id + 6))
      done
      printf '{"state":"%s","sha":"%s","total_count":1,"statuses":[%s]}' "$st" "$STUB_SHA" "$items"
      ;;
  esac
}

case "$method $path" in
  "GET /repos/acme/rig")
    send 200 '{"id":1,"full_name":"acme/rig","default_branch":"main"}'
    ;;
  "GET /repos/acme/rig/commits/$STUB_SHA/status")
    n=$(cat "$STUB_STATE/status-calls" 2>/dev/null || echo 0)
    n=$((n + 1))
    printf '%s' "$n" > "$STUB_STATE/status-calls"
    line=$(sed -n "${n}p" "$STUB_STATE/status-plan")
    [ -n "$line" ] || line=$(tail -1 "$STUB_STATE/status-plan")
    send 200 "$(status_body "$line")"
    ;;
  "GET /repos/acme/rig/actions/runs")
    if [ -n "${STUB_NO_RUN:-}" ]; then
      send 200 '{"total_count":0,"workflow_runs":[]}'
    else
      send 200 "{\"total_count\":1,\"workflow_runs\":[{\"id\":12,\"index_in_repo\":1,\"title\":\"probe\",\"status\":\"failure\",\"event\":\"push\",\"commit_sha\":\"$STUB_SHA\",\"prettyref\":\"land/probe-rig\",\"workflow_id\":\"ci.yml\",\"html_url\":\"$STUB_WEB/acme/rig/actions/runs/12\"}]}"
    fi
    ;;
  "GET /repos/acme/rig/actions/runs/12/jobs")
    if [ -n "${STUB_JOB_RENAMED:-}" ]; then
      send 200 '[{"id":311,"run_id":12,"name":"renamed","status":"failure","needs":[]}]'
    else
      send 200 '[{"id":311,"run_id":12,"name":"gate","status":"failure","needs":[]}]'
    fi
    ;;
  "GET /repos/acme/rig/actions/jobs/311/logs")
    send 200 "$(cat "$STUB_STATE/job-log")"
    ;;
  "DELETE /repos/acme/rig/branches/land%2Fprobe-rig")
    printf '%s\n' "land/probe-rig" >> "$STUB_STATE/deleted"
    send 204 ''
    ;;
  *)
    send 404 '{"message":"unhandled stub route"}'
    ;;
esac
STUB
chmod +x "$TMP/bin/curl"

printf 'the failing package is named here\nFAIL\tTestGate\t0.01s\n' > "$STATE/job-log"

# The rig hooks a case installs. hook-refuse is a pre-push that refuses the
# probe's branch, as a rig's own hook may; hook-allow records that it ran.
cat >"$TMP/hook-refuse" <<'HOOK'
#!/bin/bash
while read -r _local_ref _local_sha remote_ref _remote_sha; do
  case "$remote_ref" in
    refs/heads/land/*)
      echo "rig pre-push: refusing $remote_ref; land branches are pushed by the lander, not from here" >&2
      exit 1 ;;
  esac
done
exit 0
HOOK
chmod +x "$TMP/hook-refuse"

cat >"$TMP/hook-allow" <<'HOOK'
#!/bin/bash
printf 'ran in %s\n' "$PWD" >> "$STUB_STATE/hook-ran"
exit 0
HOOK
chmod +x "$TMP/hook-allow"

# run_probe [ARGS...] runs the probe against the stubs, prints its combined
# output, and returns its exit code (the callers read it from $?).
run_probe() {
  local out rc
  out=$(cd "$SCRIPT_DIR/.." && env -u FORGEJO_ADMIN_TOKEN -u FORGEJO_API_URL -u FORGEJO_URL \
    HOME="$TMP/home" XDG_CONFIG_HOME="$CFG" PATH="$TMP/bin:$PATH" \
    STUB_STATE="$STATE" STUB_API="$API" STUB_WEB="$WEB" STUB_SHA="$SHA" \
    STUB_ADMIN_TOKEN="$STUB_ADMIN_TOKEN" STUB_CONTEXT="${STUB_CONTEXT:-ci / gate (push)}" \
    STUB_GATE_YML="${STUB_GATE_YML:-$TMP/gate.yml}" \
    STUB_HOOK="${STUB_HOOK:-}" STUB_REPO_LACKS_SHA="${STUB_REPO_LACKS_SHA:-}" \
    bash "$PROBE" --api-url "$API" --web-url "$WEB" --admin-token-file "$TMP/admin.env" \
    --town-root "${STUB_TOWN_ROOT:-$TOWN}" "$@" 2>&1)
  rc=$?
  printf '%s' "$out"
  return "$rc"
}

# fresh empties the stub's state and gives the case a starting status script and
# a job log. The rig's repository exists, as it does on a real host, and the
# config dir is emptied so a case's "no record" assertions see only its run.
fresh() {
  rm -rf "$STATE" "$CFG"
  mkdir -p "$STATE" "$BARE" "$CFG"
  : > "$STATE/calls.log"
  printf 'success\n' > "$STATE/status-plan"
  printf 'the failing package is named here\nFAIL\tTestGate\t0.01s\n' > "$STATE/job-log"
}

calls() { cat "$STATE/calls.log" 2>/dev/null || true; }
count_calls() { # count_calls METHOD [PATH_CONTAINS]
  local n=0 line
  while IFS= read -r line; do
    case "$line" in
      "$1 "*) case "$line" in *"${2:-}"*) n=$((n + 1)) ;; esac ;;
    esac
  done <<<"$(calls)"
  printf '%s' "$n"
}
# count_calls_exact METHOD PATH — calls whose path is exactly PATH, so the run
# list is not confused with a run's jobs.
count_calls_exact() { # count_calls_exact METHOD PATH
  local n=0 line
  while IFS= read -r line; do
    [ "$line" = "$1 $2" ] && n=$((n + 1))
  done <<<"$(calls)"
  printf '%s' "$n"
}
pushes() { cat "$STATE/pushes" 2>/dev/null || true; }
deleted() { cat "$STATE/deleted" 2>/dev/null || true; }
worktrees_added() { cat "$STATE/worktrees" 2>/dev/null || true; }
worktrees_removed() { cat "$STATE/worktree-removed" 2>/dev/null || true; }
hook_ran() { cat "$STATE/hook-ran" 2>/dev/null || true; }
# record prints the green record for the default rig (empty when none was
# written); record_value reads one key's value out of it.
record() { cat "$CFG/gt/probe-rig.record" 2>/dev/null || true; }
record_value() { sed -n "s/^$1=//p" "$CFG/gt/probe-rig.record" 2>/dev/null || true; }

echo "=== a green gate passes ==="
fresh
out=$(run_probe rig --repo acme/rig --timeout 20 --poll-interval 1); rc=$?
if [ "$rc" = 0 ]; then pass "a green probe exits 0"; else fail "a green probe exits 0 (rc=$rc)" "$out"; fi
check "the copy is cloned in full" contains "clone" "$(cat "$STATE/git.log")"
check "no shallow flag is used" lacks "--depth" "$(cat "$STATE/git.log")"
check "main is pushed as the probe branch" contains "$SHA:refs/heads/land/probe-rig" "$(pushes)"
check "the candidate is pushed from the rig's own repository" contains "$BARE" "$(cat "$STATE/git.log")"
check "the probe branch is deleted" test "$(deleted)" = "land/probe-rig"
check "the deletion names the encoded branch" grep -q "DELETE /repos/acme/rig/branches/land%2Fprobe-rig" "$STATE/calls.log"
check "the run reports the gate green" contains "green" "$out"
check "the run names the context it waited on" contains "ci / gate (push)" "$out"
check "the worktree is unregistered after the push" test "$(worktrees_added)" = "$(worktrees_removed)"
check "the worktree directory is gone" test ! -d "$(worktrees_added)"
check "the unregistration is worktree remove, not a prune of the shared repo" test ! -e "$STATE/worktree-pruned"
check "the throwaway worktree sits in the probe's temp dir" contains "forgejo-probe." "$(worktrees_added)"
check "the admin token never reaches the stub's argv" test ! -e "$STATE/argv-violations"
check "the admin token never reaches the output" lacks "$STUB_ADMIN_TOKEN" "$out"
check "a green run writes the record" test -f "$CFG/gt/probe-rig.record"
check "the record names the rig" test "$(record_value rig)" = "rig"
check "the record names the repository" test "$(record_value repo)" = "acme/rig"
check "the record names the probed commit" test "$(record_value commit)" = "$SHA"
check "the record names the gate context" test "$(record_value context)" = "ci / gate (push)"
check "the record names the green verdict" test "$(record_value verdict)" = "green"
check "the record names the time" grep -Eq '^time=[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$' "$CFG/gt/probe-rig.record"
check "the record carries an epoch to age it with" grep -Eq '^epoch=[0-9]+$' "$CFG/gt/probe-rig.record"
check "the record is mode 600" test "$(mode_of "$CFG/gt/probe-rig.record")" = "600"
check "the record holds no token" lacks "$STUB_ADMIN_TOKEN" "$(record)"

echo "=== a rig hook that refuses land/* fails the probe ==="
fresh
out=$(STUB_HOOK="$TMP/hook-refuse" run_probe rig --repo acme/rig --timeout 20 --poll-interval 1); rc=$?
if [ "$rc" != 0 ]; then pass "a hook that refuses the candidate fails the probe"; else fail "a hook that refuses the candidate fails the probe (rc=$rc)" "$out"; fi
check "the refusal is the hook's own message" contains "rig pre-push: refusing refs/heads/land/probe-rig" "$out"
check "the refused push never happens" test ! -e "$STATE/pushes"
check "no branch is deleted for a refused push" test -z "$(deleted)"
check "the status is never read" test "$(count_calls GET /commits/)" = 0
check "the refused push cut a worktree" test -n "$(worktrees_added)"
check "the refused worktree is unregistered" test "$(worktrees_added)" = "$(worktrees_removed)"
check "a refused probe writes no record" test ! -e "$CFG/gt/probe-rig.record"

echo "=== a rig hook that allows land/* runs in the push's worktree ==="
fresh
out=$(STUB_HOOK="$TMP/hook-allow" run_probe rig --repo acme/rig --timeout 20 --poll-interval 1); rc=$?
if [ "$rc" = 0 ]; then pass "a hook that allows the candidate leaves the probe green"; else fail "a hook that allows the candidate leaves the probe green (rc=$rc)" "$out"; fi
check "the rig's hook ran" contains "ran in $(norm "$(worktrees_added)")" "$(norm "$(hook_ran)")"
check "the hook ran where the push did" contains "$SHA:refs/heads/land/probe-rig" "$(pushes)"
check "the allowed push deletes the branch" test "$(deleted)" = "land/probe-rig"
check "the allowed push cut a worktree" test -n "$(worktrees_added)"
check "the allowed worktree is unregistered" test "$(worktrees_added)" = "$(worktrees_removed)"

echo "=== without the rig's repository the probe warns hooks were not exercised ==="
fresh
out=$(STUB_TOWN_ROOT="$TMP/no-town" run_probe rig --repo acme/rig --timeout 20 --poll-interval 1); rc=$?
if [ "$rc" = 0 ]; then pass "a rig with no repository still probes"; else fail "a rig with no repository still probes (rc=$rc)" "$out"; fi
check "the warning names the missing repository" contains "no rig repository at $TMP/no-town/rig/.repo.git" "$out"
check "the warning says the hooks were not exercised" contains "hooks were not exercised" "$out"
check "the push falls back to the clone" contains "refs/remotes/origin/main:refs/heads/land/probe-rig" "$(pushes)"
check "no worktree is cut without a repository" test ! -e "$STATE/worktrees"
check "no hook runs without a repository" test ! -e "$STATE/hook-ran"
check "the fallback push still deletes the branch" test "$(deleted)" = "land/probe-rig"

echo "=== a rig repository without the probed commit says so ==="
fresh
out=$(STUB_REPO_LACKS_SHA=1 run_probe rig --repo acme/rig --timeout 5); rc=$?
if [ "$rc" != 0 ]; then pass "a repository behind the probed commit exits non-zero"; else fail "a repository behind the probed commit exits non-zero (rc=$rc)" "$out"; fi
check "the refusal names the repository and the remedy" contains "fetch main in $BARE" "$out"
check "nothing is pushed for an unheld commit" test ! -e "$STATE/pushes"
check "no worktree is cut for an unheld commit" test ! -e "$STATE/worktrees"

echo "=== a pending gate is polled until it reports ==="
fresh
printf 'pending\npending\nsuccess\n' > "$STATE/status-plan"
out=$(run_probe rig --repo acme/rig --timeout 20 --poll-interval 1); rc=$?
if [ "$rc" = 0 ]; then pass "a pending-then-green gate exits 0"; else fail "a pending-then-green gate exits 0 (rc=$rc)" "$out"; fi
check "the status was read three times" test "$(count_calls GET /commits/)" = 3
check "the probe branch is still deleted" test "$(deleted)" = "land/probe-rig"

echo "=== a red gate fails and prints the job log tail ==="
fresh
printf 'failure\n' > "$STATE/status-plan"
out=$(run_probe rig --repo acme/rig --timeout 20 --poll-interval 1); rc=$?
if [ "$rc" != 0 ]; then pass "a red probe exits non-zero"; else fail "a red probe exits non-zero (rc=$rc)" "$out"; fi
check "the run reports the failing state" contains "ci / gate (push) is failure" "$out"
check "the failing job's log tail is printed" contains "the failing package is named here" "$out"
check "the run list is read once" test "$(count_calls_exact GET /repos/acme/rig/actions/runs)" = 1
check "the jobs of the run are read" test "$(count_calls GET /actions/runs/12/jobs)" = 1
check "the job's log is read" test "$(count_calls GET /actions/jobs/311/logs)" = 1
check "a red verdict stops the poll" test "$(count_calls GET /commits/)" = 1
check "the probe branch is deleted after a red" test "$(deleted)" = "land/probe-rig"
check "a red probe writes no record" test ! -e "$CFG/gt/probe-rig.record"

echo "=== a red gate prints only the log tail ==="
fresh
printf 'failure\n' > "$STATE/status-plan"
{ printf 'HEAD-MARKER\n'; head -c 40000 /dev/zero | tr '\0' 'x'; printf '\nTAIL-MARKER\n'; } > "$STATE/job-log"
out=$(run_probe rig --repo acme/rig --timeout 20 --poll-interval 1); rc=$?
if [ "$rc" != 0 ]; then pass "a long red log still fails the probe"; else fail "a long red log still fails the probe (rc=$rc)" "$out"; fi
check "the tail of a long log is printed" contains "TAIL-MARKER" "$out"
check "the head of a long log is dropped" lacks "HEAD-MARKER" "$out"
check "the printed tail is bounded" test "${#out}" -lt 20000

echo "=== a red gate with a renamed job reads the failed job ==="
fresh
printf 'failure\n' > "$STATE/status-plan"
out=$(STUB_JOB_RENAMED=1 run_probe rig --repo acme/rig --timeout 20 --poll-interval 1); rc=$?
if [ "$rc" != 0 ]; then pass "a renamed job still fails the probe"; else fail "a renamed job still fails the probe (rc=$rc)" "$out"; fi
check "the failed job is read when the named job is absent" test "$(count_calls GET /actions/jobs/311/logs)" = 1

echo "=== a gate that never reports times out ==="
fresh
printf 'pending\n' > "$STATE/status-plan"
out=$(STUB_NO_RUN=1 run_probe rig --repo acme/rig --timeout 2 --poll-interval 1); rc=$?
if [ "$rc" != 0 ]; then pass "a silent gate exits non-zero"; else fail "a silent gate exits non-zero (rc=$rc)" "$out"; fi
check "the timeout is reported" contains "reported nothing" "$out"
check "a silent gate reports there is no run to read" contains "no workflow run tested" "$out"
check "the wait is bounded by the timeout" test "$(count_calls GET /commits/)" -le 4
check "the probe branch is deleted after a timeout" test "$(deleted)" = "land/probe-rig"
check "a timed-out probe writes no record" test ! -e "$CFG/gt/probe-rig.record"
check "a silent gate reads no job log" test "$(count_calls GET /actions/jobs)" = 0

echo "=== a stuck run's log is printed when the gate goes silent ==="
fresh
printf 'pending\n' > "$STATE/status-plan"
out=$(run_probe rig --repo acme/rig --timeout 2 --poll-interval 1); rc=$?
if [ "$rc" != 0 ]; then pass "a stuck run exits non-zero"; else fail "a stuck run exits non-zero (rc=$rc)" "$out"; fi
check "the stuck run's job log is printed" contains "the failing package is named here" "$out"
check "the probe branch is deleted after a stuck run" test "$(deleted)" = "land/probe-rig"

echo "=== a status that has not appeared is the same as pending ==="
fresh
printf 'absent\nsuccess\n' > "$STATE/status-plan"
out=$(run_probe rig --repo acme/rig --timeout 20 --poll-interval 1); rc=$?
if [ "$rc" = 0 ]; then pass "an unreported context then a green one exits 0"; else fail "an unreported context then a green one exits 0 (rc=$rc)" "$out"; fi
check "the absent status was polled through" test "$(count_calls GET /commits/)" = 2

echo "=== a copy without the gate workflow refuses before any push ==="
fresh
out=$(STUB_GATE_YML="$TMP/absent.yml" run_probe rig --repo acme/rig --timeout 5 --poll-interval 1); rc=$?
if [ "$rc" != 0 ]; then pass "a copy without gate.yml exits non-zero"; else fail "a copy without gate.yml exits non-zero (rc=$rc)" "$out"; fi
check "the refusal is named" contains "no .forgejo/workflows/gate.yml" "$out"
check "the refusal happens before the push" test ! -e "$STATE/pushes"
check "no branch is deleted after a refusal" test -z "$(deleted)"
check "the status is never read" test "$(count_calls GET /commits/)" = 0
check "a refusal before the push writes no record" test ! -e "$CFG/gt/probe-rig.record"

echo "=== a workflow with more than one job refuses ==="
fresh
cat >"$TMP/two-jobs.yml" <<'YAML'
name: ci
jobs:
  lint:
    runs-on: ci-base
  gate:
    runs-on: ci-base
YAML
out=$(STUB_GATE_YML="$TMP/two-jobs.yml" run_probe rig --repo acme/rig --timeout 5 --poll-interval 1); rc=$?
if [ "$rc" != 0 ]; then pass "a two-job workflow exits non-zero"; else fail "a two-job workflow exits non-zero (rc=$rc)" "$out"; fi
check "the refusal names the job count" contains "names 2 jobs under jobs:" "$out"
check "nothing is pushed for an ambiguous context" test ! -e "$STATE/pushes"
check "no status is read for an ambiguous context" test "$(count_calls GET /commits/)" = 0

echo "=== the newest status carrying the context wins ==="
fresh
# The repeated probe of an unchanged main: the commit already carries the
# previous run's success, and the push just made has reported red.
printf 'success,failure\n' > "$STATE/status-plan"
out=$(run_probe rig --repo acme/rig --timeout 20 --poll-interval 1); rc=$?
if [ "$rc" != 0 ]; then pass "a stale success does not hide the new red"; else fail "a stale success does not hide the new red (rc=$rc)" "$out"; fi
check "the failing state is reported" contains "ci / gate (push) is failure" "$out"
check "the probe branch is deleted" test "$(deleted)" = "land/probe-rig"

echo "=== the context is derived from the workflow's name and job ==="
fresh
cat >"$TMP/other.yml" <<'YAML'
name: build
jobs:
  check:
    runs-on: ci-base
YAML
out=$(STUB_GATE_YML="$TMP/other.yml" STUB_CONTEXT="build / check (push)" \
  run_probe rig --repo acme/rig --timeout 20 --poll-interval 1); rc=$?
if [ "$rc" = 0 ]; then pass "a renamed workflow is probed under its own context"; else fail "a renamed workflow is probed under its own context (rc=$rc)" "$out"; fi
check "the derived context is reported" contains "build / check (push)" "$out"

echo "=== dry run writes nothing ==="
fresh
printf 'success\n' > "$STATE/status-plan"
out=$(run_probe rig --repo acme/rig --dry-run); rc=$?
if [ "$rc" = 0 ]; then pass "a dry run exits 0"; else fail "a dry run exits 0 (rc=$rc)" "$out"; fi
check "dry run sends no push" test ! -e "$STATE/pushes"
check "dry run sends no delete" test ! -e "$STATE/deleted"
check "dry run sends no status read" test "$(count_calls GET /commits/)" = 0
check "dry run reads the repository" test "$(count_calls GET /repos/acme/rig)" = 1
check "dry run says it would push" contains "would push" "$out"
check "dry run names the worktree the push would ride" contains "would push from a throwaway worktree of $BARE" "$out"
check "dry run cuts no worktree" test ! -e "$STATE/worktrees"
check "dry run says it would delete the branch" contains "would delete branch land/probe-rig" "$out"
check "dry run reports no write was sent" contains "no push and no deletion was sent" "$out"
check "dry run writes no record" test ! -e "$CFG/gt/probe-rig.record"

echo "=== a repository the instance does not hold fails loudly ==="
fresh
out=$(run_probe rig --repo acme/missing --timeout 5); rc=$?
if [ "$rc" != 0 ]; then pass "an unheld repository exits non-zero"; else fail "an unheld repository exits non-zero (rc=$rc)" "$out"; fi
check "the failure names the repository" contains "repos/acme/missing" "$out"
check "nothing is cloned for a missing repository" test ! -e "$STATE/git.log"

echo "=== argument errors ==="
free_home=$(mktemp -d "$TMP/free.XXXXXX")
out=$(env PATH="$TMP/bin:$PATH" STUB_STATE="$STATE" STUB_API="$API" STUB_WEB="$WEB" STUB_SHA="$SHA" \
  HOME="$free_home" bash "$PROBE" --repo acme/rig 2>&1); rc=$?
if [ "$rc" = 2 ]; then pass "a missing rig exits 2"; else fail "a missing rig exits 2 (rc=$rc)" "$out"; fi
out=$(run_probe rig); rc=$?
if [ "$rc" = 2 ]; then pass "a missing --repo exits 2"; else fail "a missing --repo exits 2 (rc=$rc)" "$out"; fi
out=$(run_probe rig --repo not-a-repo); rc=$?
if [ "$rc" = 2 ]; then pass "--repo without OWNER/NAME exits 2"; else fail "--repo without OWNER/NAME exits 2 (rc=$rc)" "$out"; fi
out=$(run_probe 'bad/rig' --repo acme/rig); rc=$?
if [ "$rc" = 2 ]; then pass "a rig name with a slash exits 2"; else fail "a rig name with a slash exits 2 (rc=$rc)" "$out"; fi
out=$(run_probe rig --repo acme/rig --timeout 0); rc=$?
if [ "$rc" = 2 ]; then pass "a zero timeout exits 2"; else fail "a zero timeout exits 2 (rc=$rc)" "$out"; fi
out=$(run_probe rig --repo acme/rig --town-root); rc=$?
if [ "$rc" = 2 ]; then pass "--town-root without a value exits 2"; else fail "--town-root without a value exits 2 (rc=$rc)" "$out"; fi
out=$(run_probe rig --repo acme/rig --timeout soon); rc=$?
if [ "$rc" = 2 ]; then pass "a non-numeric timeout exits 2"; else fail "a non-numeric timeout exits 2 (rc=$rc)" "$out"; fi
out=$(run_probe rig --repo acme/rig --nope); rc=$?
if [ "$rc" = 2 ]; then pass "an unknown option exits 2"; else fail "an unknown option exits 2 (rc=$rc)" "$out"; fi

echo "=== a missing admin token fails loudly ==="
fresh
out=$(env -u FORGEJO_ADMIN_TOKEN HOME="$TMP/home" PATH="$TMP/bin:$PATH" \
  STUB_STATE="$STATE" STUB_API="$API" STUB_WEB="$WEB" STUB_SHA="$SHA" \
  bash "$PROBE" rig --api-url "$API" --repo acme/rig --admin-token-file "$TMP/absent.env" 2>&1); rc=$?
if [ "$rc" = 1 ]; then pass "a missing admin token exits 1"; else fail "a missing admin token exits 1 (rc=$rc)" "$out"; fi
check "the missing token is named in the error" contains "no admin token" "$out"

echo
if [ "$FAIL" = 0 ]; then
  echo "forgejo-probe_test.sh: all $PASS cases passed"
else
  echo "forgejo-probe_test.sh: $FAIL of $((PASS + FAIL)) cases failed"
  exit 1
fi
