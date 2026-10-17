#!/usr/bin/env bash
# Tests for plugins/app-promote/run.sh (gt-5xrmp, gt-vdhwk): a curl stub serves
# canned Forgejo Actions JSON and records every call, and a gt stub records the
# promotes, so the selection, the idempotence and the failure paths are
# exercised without a Forgejo, a town database or a GitHub target. Run directly
# or via the shell tier: scripts/tier-sweep.sh discovers it as a `*_test.sh`
# script.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
RUN="$SCRIPT_DIR/run.sh"
BASH_BIN="$(command -v bash)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
PASS=0
FAIL=0

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; FAIL=$((FAIL + 1)); }

VIEWER_TOKEN="viewer-token-not-to-be-printed"

# --- Stubs --------------------------------------------------------------------
# curl serves the fixture the current case named and records the URL and whether
# the token travelled in the mode-600 config file rather than an argv. The real
# Forgejo is never reached: FORGEJO_API_URL points run.sh at a name that does
# not resolve. A compare against main is the "is it on main" read and has its
# own fixture, apart from the compare against last_promoted.
mkdir -p "$TMP/bin"
cat >"$TMP/bin/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
argv=("$@")
out="" url="" conf="" headers=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift 2 ;;
    -w) shift 2 ;;
    -K) conf=$2; shift 2 ;;
    -H) headers="$headers $2"; shift 2 ;;
    -sS) shift ;;
    *) url=$1; shift ;;
  esac
done
printf '%s\n' "$url" >>"$STUB_STATE/calls"
# A token in argv would show in `ps`; the config file is the only place it may
# travel, and every call must carry one.
for a in "${argv[@]}"; do
  case "$a" in *"$STUB_TOKEN"*) echo "token in argv: $a" >>"$STUB_STATE/violations" ;; esac
done
if [ -z "$conf" ] || ! grep -qF "Authorization: token $STUB_TOKEN" "$conf"; then
  echo "no token in the curl config for $url" >>"$STUB_STATE/violations"
fi
[ -z "${STUB_CURL_FAIL:-}" ] || exit 7
shape=runs
case "$url" in
  */actions/runs/*/jobs) shape=jobs ;;
  */actions/runs\?*page=2*) shape=runs-page2 ;;
  */compare/main...*) shape=compare-main ;;
  */compare/*) shape=compare ;;
  */tags/*) shape="tag-${url##*/}" ;;
esac
if [ -n "${STUB_HTTP_STATUS:-}" ] && [ "$STUB_HTTP_STATUS" != 200 ]; then
  printf '{"message":"stub says %s"}' "$STUB_HTTP_STATUS" >"$out"
  printf '%s' "$STUB_HTTP_STATUS"
  exit 0
fi
# A tag with no fixture is a ref that is not a tag: the API answers 404.
if [ ! -f "$STUB_STATE/$shape.json" ]; then
  printf '{"message":"not found"}' >"$out"
  printf '404'
  exit 0
fi
cp "$STUB_STATE/$shape.json" "$out"
printf '200'
STUB
chmod +x "$TMP/bin/curl"

# gt records the promote argv and answers with STUB_GT_OUT and STUB_GT_RC.
cat >"$TMP/bin/gt" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_STATE/promotes"
printf '%s' "${STUB_GT_OUT:-}"
exit "${STUB_GT_RC:-0}"
STUB
chmod +x "$TMP/bin/gt"

STATE="$TMP/state"
mkdir -p "$STATE"

# --- Fixtures -----------------------------------------------------------------
GREEN="aaaa111122223333444455556666777788889999"
OLDER="bbbb111122223333444455556666777788889999"
STAGING_ONLY="cccc111122223333444455556666777788889999"
TOWN="$TMP/town"
mkdir -p "$TOWN/.runtime/red-main" "$TMP/config/gt"
printf 'FORGEJO_TOKEN=%s\n' "$VIEWER_TOKEN" >"$TMP/config/gt/forgejo-viewer.env"
chmod 600 "$TMP/config/gt/forgejo-viewer.env"

# A run list whose newest successful deploy run is the tag v0.2.0 at GREEN, with
# an older tag run that also succeeded, a newer tag run still running, a newer
# successful staging run on main that must prove nothing, and a newer successful
# deploy run at a branch. Nothing but the created stamp orders them.
write_runs() {
  cat >"$STATE/runs.json" <<JSON
{"total_count":6,"workflow_runs":[
 {"id":9,"workflow_id":"deploy.yml","status":"success","event":"push","prettyref":"v0.2.0","commit_sha":"$GREEN","created":"2026-10-09T03:14:42Z"},
 {"id":8,"workflow_id":".forgejo/workflows/staging.yml","status":"success","event":"push","prettyref":"main","commit_sha":"$STAGING_ONLY","created":"2026-10-09T03:40:00Z"},
 {"id":7,"workflow_id":"deploy.yml","status":"success","event":"push","prettyref":"v0.1.0","commit_sha":"$OLDER","created":"2026-10-09T02:59:20Z"},
 {"id":6,"workflow_id":"deploy.yml","status":"running","event":"push","prettyref":"v0.3.0","commit_sha":"$STAGING_ONLY","created":"2026-10-09T03:20:40Z"},
 {"id":5,"workflow_id":"gate.yml","status":"success","event":"push","prettyref":"land/fr-y","commit_sha":"$OLDER","created":"2026-10-09T03:19:00Z"}
]}
JSON
}
write_jobs() {
  cat >"$STATE/jobs.json" <<JSON
[{"id":1,"name":"build","status":"success"},
 {"id":2,"name":"prod","status":"success","needs":["build"]},
 {"id":3,"name":"prod-integration","status":"$1","needs":["prod"]}]
JSON
}
# write_tag NAME SHA makes NAME a tag of the repo naming SHA; the stub answers 404
# for a tag it has no file for.
write_tag() {
  printf '{"name":"%s","commit":{"sha":"%s"}}' "$1" "$2" >"$STATE/tag-$1.json"
}
write_runs
write_jobs success
write_tag v0.2.0 "$GREEN"
write_tag v0.1.0 "$OLDER"
# The commit is ahead of last_promoted and on main: the compare against last
# holds commits, the compare against main holds none.
cat >"$STATE/compare.json" <<'JSON'
{"total_commits":2,"commits":[{},{}]}
JSON
cat >"$STATE/compare-main.json" <<'JSON'
{"total_commits":0,"commits":[]}
JSON

# run_runs OUT [GT_RC GT_OUT CURL_FAIL HTTP_STATUS] runs run.sh against the
# stubs; rc holds the exit code and the whole output (stdout and stderr
# together) lands in OUT. The viewer token reaches run.sh through the file it
# reads and the config file curl is handed, never through an argument.
run_runs() {
  local out=$1 gt_rc=${2:-0} gt_out=${3:-} curl_fail=${4:-} http_status=${5:-200}
  : >"$STATE/calls"
  : >"$STATE/promotes"
  : >"$STATE/violations"
  rc=0
  PATH="$TMP/bin:$PATH" \
    XDG_CONFIG_HOME="$TMP/config" \
    GT_TOWN_ROOT="$TOWN" \
    FORGEJO_API_URL="http://forgejo.invalid/api/v1" \
    STUB_STATE="$STATE" STUB_TOKEN="$VIEWER_TOKEN" \
    STUB_HTTP_STATUS="$http_status" \
    STUB_CURL_FAIL="$curl_fail" \
    STUB_GT_OUT="$gt_out" \
    STUB_GT_RC="$gt_rc" \
    "$BASH_BIN" "$RUN" >"$out" 2>&1 || rc=$?
}

# no_token_printed NAME checks the run's output and every stub call for the
# token.
no_token_printed() {
  if grep -qF "$VIEWER_TOKEN" "$TMP/out"; then
    fail "$1 (the token reached the output)"
  elif [ -s "$STATE/violations" ]; then
    fail "$1 ($(tr '\n' ' ' <"$STATE/violations"))"
  else
    pass "$1"
  fi
}

# write_state RIG [LAST] writes the rig's red-main record, or removes it when
# LAST is empty.
write_state() {
  local file="$TOWN/.runtime/red-main/$1.json"
  if [ -z "${2:-}" ]; then
    rm -f "$file"
  else
    printf '{"last_green":"%s","last_promoted":"%s"}\n' "$GREEN" "$2" >"$file"
  fi
}

# expect_nothing NAME PATTERN checks that the run exited 0, logged PATTERN,
# promoted nothing and recorded itself as skipped.
expect_nothing() {
  if [ "$rc" == 0 ] && [ ! -s "$STATE/promotes" ] && grep -qF "$2" "$TMP/out" \
    && grep -qF "[plugin-result skipped]" "$TMP/out"; then
    pass "$1"
  else
    fail "$1 (rc=$rc, promotes=$(tr '\n' '|' <"$STATE/promotes"), $(cat "$TMP/out"))"
  fi
}

echo "app-promote: promotes the commit the newest prod-green release tag names"
write_state fractals "$OLDER"
run_runs "$TMP/out"
if [ "$rc" == 0 ] && grep -qF "rig fractals: promoted ${GREEN:0:8} (v0.2.0)" "$TMP/out"; then
  pass "prod-integration green and ahead: promoted to the tag's commit"
else
  fail "prod-integration green and ahead: promoted (rc=$rc, $(cat "$TMP/out"))"
fi
if [ "$(cat "$STATE/promotes")" == "promote --rig fractals --sha $GREEN" ]; then
  pass "gt promote is called with the rig and the tag's commit"
else
  fail "gt promote is called with the rig and the tag's commit ($(tr '\n' '|' <"$STATE/promotes"))"
fi
if grep -qF "/repos/sloan/fractals-nextjs/actions/runs/9/jobs" "$STATE/calls"; then
  pass "the jobs read are the newest successful tag run's, not the newer staging or branch runs"
else
  fail "the jobs read are the newest successful tag run's ($(tr '\n' ' ' <"$STATE/calls"))"
fi
if grep -qF "/repos/sloan/fractals-nextjs/compare/main...$GREEN" "$STATE/calls"; then
  pass "the commit is checked against the rig's Forgejo main"
else
  fail "the commit is checked against the rig's Forgejo main ($(tr '\n' ' ' <"$STATE/calls"))"
fi
no_token_printed "the viewer token reaches no output and no argv"

echo "app-promote: a first promotion has no last_promoted to compare with"
write_state fractals ""
run_runs "$TMP/out"
if [ "$rc" == 0 ] && [ "$(cat "$STATE/promotes")" == "promote --rig fractals --sha $GREEN" ] \
  && [ "$(grep -cF '/compare/' "$STATE/calls")" == 1 ]; then
  pass "no red-main record: promoted after the one compare against main"
else
  fail "no red-main record: promoted (rc=$rc, $(cat "$TMP/out"); $(tr '\n' ' ' <"$STATE/calls"))"
fi

echo "app-promote: a failed prod-integration job proves nothing"
write_jobs failure
write_state fractals "$OLDER"
run_runs "$TMP/out"
expect_nothing "a run whose prod-integration job failed is not promoted" "has no successful 'prod-integration' job"

echo "app-promote: a missing prod-integration job proves nothing"
cat >"$STATE/jobs.json" <<'JSON'
[{"id":1,"name":"build","status":"success"},
 {"id":2,"name":"prod","status":"success","needs":["build"]}]
JSON
run_runs "$TMP/out"
expect_nothing "a release run that predates the job is not promoted" "has no successful 'prod-integration' job"
write_jobs success

echo "app-promote: staging alone never promotes"
cat >"$STATE/runs.json" <<JSON
{"total_count":1,"workflow_runs":[
 {"id":8,"workflow_id":".forgejo/workflows/staging.yml","status":"success","event":"push","prettyref":"main","commit_sha":"$STAGING_ONLY","created":"2026-10-09T03:40:00Z"}
]}
JSON
run_runs "$TMP/out"
expect_nothing "a green staging run and its jobs with no release run: nothing" "no successful deploy.yml run on a v* tag"
if ! grep -qF "/actions/runs/8/jobs" "$STATE/calls"; then
  pass "the staging run's jobs are never read"
else
  fail "the staging run's jobs are never read ($(tr '\n' ' ' <"$STATE/calls"))"
fi

echo "app-promote: a deploy run for a branch is not a release"
cat >"$STATE/runs.json" <<JSON
{"total_count":2,"workflow_runs":[
 {"id":10,"workflow_id":"deploy.yml","status":"success","event":"workflow_dispatch","prettyref":"main","commit_sha":"$STAGING_ONLY","created":"2026-10-09T04:00:00Z"},
 {"id":11,"workflow_id":"deploy.yml","status":"running","event":"push","prettyref":"v0.3.0","commit_sha":"$STAGING_ONLY","created":"2026-10-09T04:10:00Z"}
]}
JSON
run_runs "$TMP/out"
expect_nothing "a successful deploy.yml run at main, and a tag run still going: nothing" "no successful deploy.yml run on a v* tag"
write_runs

echo "app-promote: a commit equal to last_promoted is already on GitHub"
write_state fractals "$GREEN"
run_runs "$TMP/out"
expect_nothing "the tag's commit is last_promoted: no gt promote and the skip marker" "is already promoted"

echo "app-promote: a commit behind last_promoted is left alone"
write_state fractals "$STAGING_ONLY"
cat >"$STATE/compare.json" <<'JSON'
{"total_commits":0,"commits":[]}
JSON
run_runs "$TMP/out"
expect_nothing "a sha behind last_promoted is reported and not promoted" "is not ahead of last_promoted"
if [ "$(grep -cF "/repos/sloan/fractals-nextjs/compare/$STAGING_ONLY...$GREEN" "$STATE/calls")" == 1 ]; then
  pass "the comparison against last_promoted is one compare read"
else
  fail "the comparison against last_promoted is one compare read ($(tr '\n' ' ' <"$STATE/calls"))"
fi
cat >"$STATE/compare.json" <<'JSON'
{"total_commits":2,"commits":[{},{}]}
JSON

echo "app-promote: a release commit that is not on Forgejo main is refused"
write_state fractals "$OLDER"
cat >"$STATE/compare-main.json" <<'JSON'
{"total_commits":1,"commits":[{}]}
JSON
run_runs "$TMP/out"
expect_nothing "a tag whose commit main does not hold is not promoted" "is not on main"
cat >"$STATE/compare-main.json" <<'JSON'
{"total_commits":0,"commits":[]}
JSON

echo "app-promote: a branch named like a release is not a release"
write_state fractals "$OLDER"
cat >"$STATE/runs.json" <<JSON
{"total_count":1,"workflow_runs":[
 {"id":12,"workflow_id":"deploy.yml","status":"success","event":"workflow_dispatch","prettyref":"v-hotfix","commit_sha":"$STAGING_ONLY","created":"2026-10-09T05:00:00Z"}
]}
JSON
run_runs "$TMP/out"
expect_nothing "a successful deploy run at a branch named v-hotfix has no tag behind it: nothing" "no successful deploy.yml run on a v* tag"
cat >"$STATE/runs.json" <<JSON
{"total_count":2,"workflow_runs":[
 {"id":12,"workflow_id":"deploy.yml","status":"success","event":"workflow_dispatch","prettyref":"v-hotfix","commit_sha":"$STAGING_ONLY","created":"2026-10-09T05:00:00Z"},
 {"id":7,"workflow_id":"deploy.yml","status":"success","event":"push","prettyref":"v0.1.0","commit_sha":"$OLDER","created":"2026-10-09T02:59:20Z"}
]}
JSON
write_state fractals ""
run_runs "$TMP/out"
if [ "$rc" == 0 ] && [ "$(cat "$STATE/promotes")" == "promote --rig fractals --sha $OLDER" ]; then
  pass "the newer branch run is passed over for the older run that is a real tag"
else
  fail "the newer branch run is passed over (rc=$rc, $(cat "$TMP/out"))"
fi
write_tag v0.1.0 "$STAGING_ONLY"
run_runs "$TMP/out"
expect_nothing "a tag that names another commit than the run's is not a release of it" "no successful deploy.yml run on a v* tag"
write_tag v0.1.0 "$OLDER"
write_runs

echo "app-promote: a release run past the first page of runs is still found"
write_state fractals "$OLDER"
{
  printf '{"total_count":51,"workflow_runs":['
  for i in $(seq 100 149); do
    printf '{"id":%s,"workflow_id":"deploy.yml","status":"success","event":"workflow_dispatch","prettyref":"main","commit_sha":"%s","created":"2026-10-10T00:%02d:00Z"},' "$i" "$STAGING_ONLY" $((i - 100))
  done
  printf '{"id":13,"workflow_id":"deploy.yml","status":"failure","event":"push","prettyref":"main","commit_sha":"%s","created":"2026-10-10T01:00:00Z"}' "$STAGING_ONLY"
  printf ']}'
} >"$STATE/runs.json"
cat >"$STATE/runs-page2.json" <<JSON
{"total_count":51,"workflow_runs":[
 {"id":9,"workflow_id":"deploy.yml","status":"success","event":"push","prettyref":"v0.2.0","commit_sha":"$GREEN","created":"2026-10-09T03:14:42Z"}
]}
JSON
run_runs "$TMP/out"
if [ "$rc" == 0 ] && [ "$(cat "$STATE/promotes")" == "promote --rig fractals --sha $GREEN" ]; then
  pass "50 newer branch runs on page 1 do not hide the release on page 2"
else
  fail "50 newer branch runs on page 1 do not hide the release on page 2 (rc=$rc, $(cat "$TMP/out"))"
fi
rm -f "$STATE/runs-page2.json"
write_runs

echo "app-promote: a compare that cannot be read fails closed"
write_state fractals "$OLDER"
for body in '' 'not json' '{}' '{"total_commits":"two"}' '{"total_commits":null}'; do
  printf '%s' "$body" >"$STATE/compare-main.json"
  run_runs "$TMP/out"
  if [ "$rc" == 1 ] && [ ! -s "$STATE/promotes" ] && grep -qF "no commit count in the reply" "$TMP/out"; then
    pass "compare against main answering '$body': exit 1, nothing promoted"
  else
    fail "compare against main answering '$body' (rc=$rc, promotes=$(tr '\n' '|' <"$STATE/promotes"), $(cat "$TMP/out"))"
  fi
done
cat >"$STATE/compare-main.json" <<'JSON'
{"total_commits":0,"commits":[]}
JSON
printf '{}' >"$STATE/compare.json"
run_runs "$TMP/out"
if [ "$rc" == 1 ] && [ ! -s "$STATE/promotes" ] && grep -qF "no commit count in the reply" "$TMP/out"; then
  pass "compare against last_promoted answering {}: exit 1, nothing promoted"
else
  fail "compare against last_promoted answering {} (rc=$rc, promotes=$(tr '\n' '|' <"$STATE/promotes"), $(cat "$TMP/out"))"
fi
cat >"$STATE/compare.json" <<'JSON'
{"total_commits":2,"commits":[{},{}]}
JSON

echo "app-promote: an unreadable Forgejo fails closed"
run_runs "$TMP/out" 0 "" 1
if [ "$rc" == 1 ] && grep -qF "could not reach Forgejo" "$TMP/out" && [ ! -s "$STATE/promotes" ]; then
  pass "curl failure: exit 1, one line of cause, nothing promoted"
else
  fail "curl failure: exit 1, one line of cause, nothing promoted (rc=$rc, $(cat "$TMP/out"))"
fi
no_token_printed "the failed read prints no token"
run_runs "$TMP/out" 0 "" "" 500
if [ "$rc" == 1 ] && grep -qF "returned 500" "$TMP/out" && [ ! -s "$STATE/promotes" ]; then
  pass "HTTP 500: exit 1, the status is named, nothing promoted"
else
  fail "HTTP 500: exit 1, the status is named, nothing promoted (rc=$rc, $(cat "$TMP/out"))"
fi

echo "app-promote: a missing viewer token fails closed"
mv "$TMP/config/gt/forgejo-viewer.env" "$TMP/viewer.env"
run_runs "$TMP/out"
if [ "$rc" == 1 ] && grep -qF "no viewer token at" "$TMP/out"; then
  pass "no token file: exit 1 and the file is named"
else
  fail "no token file: exit 1 and the file is named (rc=$rc, $(cat "$TMP/out"))"
fi
mv "$TMP/viewer.env" "$TMP/config/gt/forgejo-viewer.env"

echo "app-promote: a refused promotion is this run's nothing-to-do"
write_state fractals "$OLDER"
run_runs "$TMP/out" 1 "Error: rig fractals names no merge_queue.forgejo.promote_target, so it does not promote"
if [ "$rc" == 0 ] && grep -qF "rig fractals: not promotable: Error: rig fractals names no merge_queue" "$TMP/out" \
  && grep -qF "[plugin-result skipped]" "$TMP/out"; then
  pass "exit 1 from gt promote is logged and the run records skipped"
else
  fail "exit 1 from gt promote is logged and the run records skipped (rc=$rc, $(cat "$TMP/out"))"
fi

echo "app-promote: a failed promotion fails the run"
run_runs "$TMP/out" 2 "Error: rig fractals: the promotion failed: pushing aaaa failed"
if [ "$rc" == 1 ] && grep -qF "exited 2" "$TMP/out" && grep -qF "the promotion failed" "$TMP/out"; then
  pass "exit 2 from gt promote: the plugin exits 1 with the cause"
else
  fail "exit 2 from gt promote: the plugin exits 1 with the cause (rc=$rc, $(cat "$TMP/out"))"
fi

echo "app-promote: a divergence is left to gt promote's escalation"
run_runs "$TMP/out" 2 "Error: rig fractals: the promotion target's main is at 1234, which is not an ancestor of aaaa, so it was left alone and the divergence is escalated"
if [ "$rc" == 0 ] && grep -qF "divergence is escalated" "$TMP/out" \
  && grep -qF "[plugin-result skipped]" "$TMP/out"; then
  pass "a diverged target exits 0 with the line, recording skipped"
else
  fail "a diverged target exits 0 with the line, recording skipped (rc=$rc, $(cat "$TMP/out"))"
fi

echo "app-promote: $PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]]
