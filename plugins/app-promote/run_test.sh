#!/usr/bin/env bash
# Tests for plugins/app-promote/run.sh (gt-5xrmp): a curl stub serves canned
# Forgejo Actions JSON and records every call, and a gt stub records the
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
# not resolve.
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
  */compare/*) shape=compare ;;
esac
if [ -n "${STUB_HTTP_STATUS:-}" ] && [ "$STUB_HTTP_STATUS" != 200 ]; then
  printf '{"message":"stub says %s"}' "$STUB_HTTP_STATUS" >"$out"
  printf '%s' "$STUB_HTTP_STATUS"
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
NOT_GREEN="cccc111122223333444455556666777788889999"
TOWN="$TMP/town"
mkdir -p "$TOWN/.runtime/red-main" "$TMP/config/gt"
printf 'FORGEJO_TOKEN=%s\n' "$VIEWER_TOKEN" >"$TMP/config/gt/forgejo-viewer.env"
chmod 600 "$TMP/config/gt/forgejo-viewer.env"

# A run list whose newest staging run on main succeeded, an older one that also
# did, a newer one still running, and one of the same workflow named by its path
# at a land ref. Nothing but the created stamp orders them.
cat >"$STATE/runs.json" <<JSON
{"total_count":5,"workflow_runs":[
 {"id":9,"workflow_id":"staging.yml","status":"success","event":"push","prettyref":"main","commit_sha":"$GREEN","created":"2026-10-09T03:14:42Z"},
 {"id":8,"workflow_id":"gate.yml","status":"success","event":"push","prettyref":"land/fr-x","commit_sha":"$GREEN","created":"2026-10-09T03:13:39Z"},
 {"id":7,"workflow_id":"staging.yml","status":"success","event":"push","prettyref":"main","commit_sha":"$OLDER","created":"2026-10-09T02:59:20Z"},
 {"id":6,"workflow_id":"staging.yml","status":"running","event":"push","prettyref":"main","commit_sha":"$NOT_GREEN","created":"2026-10-09T03:20:40Z"},
 {"id":5,"workflow_id":".forgejo/workflows/staging.yml","status":"success","event":"push","prettyref":"land/fr-y","commit_sha":"$OLDER","created":"2026-10-09T03:19:00Z"}
]}
JSON
cat >"$STATE/jobs.json" <<'JSON'
[{"id":1,"name":"build","status":"success"},
 {"id":2,"name":"staging","status":"success","needs":["build"]}]
JSON
cat >"$STATE/compare.json" <<'JSON'
{"total_commits":2,"commits":[{},{}]}
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

echo "app-promote: promotes the newest integration-green commit per rig"
write_state fractals "$OLDER"
write_state beaver "$GREEN"
run_runs "$TMP/out"
if [ "$rc" == 0 ] && grep -qF "rig fractals: promoted ${GREEN:0:8}" "$TMP/out"; then
  pass "the rig whose last_promoted is behind is promoted to the newest green sha"
else
  fail "the rig whose last_promoted is behind is promoted (rc=$rc, $(cat "$TMP/out"))"
fi
if [ "$(cat "$STATE/promotes")" == "promote --rig fractals --sha $GREEN" ]; then
  pass "gt promote is called with the rig and the green sha, and only for that rig"
else
  fail "gt promote is called with the rig and the green sha ($(tr '\n' '|' <"$STATE/promotes"))"
fi
if grep -qF "rig beaver: ${GREEN:0:8} is already promoted" "$TMP/out"; then
  pass "the rig already at the green sha is left alone"
else
  fail "the rig already at the green sha is left alone ($(cat "$TMP/out"))"
fi
if grep -qF "/repos/sloan/fractals-nextjs/actions/runs/9/jobs" "$STATE/calls"; then
  pass "the green run's jobs are read for the run the newest stamp names"
else
  fail "the green run's jobs are read ($(tr '\n' ' ' <"$STATE/calls"))"
fi
no_token_printed "the viewer token reaches no output and no argv"

echo "app-promote: nothing to promote records a skipped run"
write_state fractals "$GREEN"
write_state beaver "$GREEN"
run_runs "$TMP/out"
if [ "$rc" == 0 ] && grep -qF "[plugin-result skipped]" "$TMP/out" && [ ! -s "$STATE/promotes" ]; then
  pass "both rigs already promoted: no gt promote and the skip marker"
else
  fail "both rigs already promoted: no gt promote and the skip marker (rc=$rc, $(cat "$TMP/out"))"
fi

echo "app-promote: a green sha behind last_promoted is left alone"
write_state fractals "$NOT_GREEN"
write_state beaver "$NOT_GREEN"
cat >"$STATE/compare.json" <<'JSON'
{"total_commits":0,"commits":[]}
JSON
run_runs "$TMP/out"
if [ "$rc" == 0 ] && [ ! -s "$STATE/promotes" ] && grep -qF "is not ahead of last_promoted" "$TMP/out"; then
  pass "a sha behind last_promoted is reported and not promoted"
else
  fail "a sha behind last_promoted is reported and not promoted (rc=$rc, $(cat "$TMP/out"))"
fi
if [ "$(grep -cF '/repos/sloan/fractals-nextjs/compare/' "$STATE/calls")" == 1 ]; then
  pass "the comparison against last_promoted is one compare read"
else
  fail "the comparison against last_promoted is one compare read ($(tr '\n' ' ' <"$STATE/calls"))"
fi
cat >"$STATE/compare.json" <<'JSON'
{"total_commits":2,"commits":[{},{}]}
JSON

echo "app-promote: a failed integration job is not integration-green"
cat >"$STATE/jobs.json" <<'JSON'
[{"id":1,"name":"build","status":"success"},
 {"id":2,"name":"staging","status":"failure","needs":["build"]}]
JSON
write_state fractals "$OLDER"
run_runs "$TMP/out"
if [ "$rc" == 0 ] && [ ! -s "$STATE/promotes" ] && grep -qF "has no successful 'staging' job" "$TMP/out"; then
  pass "a run whose staging job failed is not promoted"
else
  fail "a run whose staging job failed is not promoted (rc=$rc, $(cat "$TMP/out"))"
fi

echo "app-promote: a missing integration job is not integration-green"
cat >"$STATE/jobs.json" <<'JSON'
[{"id":1,"name":"build","status":"success"}]
JSON
run_runs "$TMP/out"
if [ "$rc" == 0 ] && [ ! -s "$STATE/promotes" ] && grep -qF "has no successful 'staging' job" "$TMP/out"; then
  pass "a run with no staging job is not promoted"
else
  fail "a run with no staging job is not promoted (rc=$rc, $(cat "$TMP/out"))"
fi
cat >"$STATE/jobs.json" <<'JSON'
[{"id":1,"name":"build","status":"success"},
 {"id":2,"name":"staging","status":"success","needs":["build"]}]
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
write_state beaver "$GREEN"
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
