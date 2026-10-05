#!/usr/bin/env bash
# forgejo-probe.sh — prove a rig's gate runs green in the real runner before
# its cutover: clone the rig's GitHub main in full (never shallow — Forgejo
# refuses a shallow push), push it to the rig's Forgejo copy as
# land/probe-<rig>, wait for the real `<gate> (push)` commit status, require
# success, and delete the branch again on every path (gt-fn9e6.28).
#
# The copy that is probed is the full clone of the rig's GitHub repo, because
# the branch the probe pushes carries that tree: the refusal below reads the
# same tree Forgejo runs. A copy whose main lacks
# .forgejo/workflows/gate.yml refuses before any push, since Forgejo then falls
# back to the repo's .github/workflows and nothing gates. The required status
# context is derived from that same file (name and single job), so a renamed
# workflow cannot be probed against a stale context.
#
# A hand probe in a bare `docker run` missed a runner difference (no init
# process); only the real runner counts, so the probe's whole point is that it
# waits on the runner's own verdict.
#
# Design of record: docs/design/forgejo-primary-landing.md. Runbook:
# docs/forgejo-runbook.md. The admin token reaches curl through a mode-600
# config file, never argv; the push rides the operator's role-keyed credential
# helper, so no token is written here. Progress goes to stderr; the failing
# job's log tail is the one thing written to stdout, so it can be paged or
# captured without the progress around it.
#
# Usage: forgejo-probe.sh <rig> --repo OWNER/NAME [options]
#   --api-url URL          API root (default $FORGEJO_API_URL,
#                          else http://127.0.0.1:3000/api/v1)
#   --web-url URL          Forgejo web root, the base of the git URL (default
#                          $FORGEJO_URL, else --api-url less /api/v1)
#   --admin-token-file F   file holding FORGEJO_ADMIN_TOKEN=... (default
#                          $FORGEJO_ADMIN_ENV, else $HOME/forgejo/.env)
#   --repo OWNER/NAME      the rig's Forgejo repository (required)
#   --github-url URL       repository to clone the copy from (default
#                          https://github.com/OWNER/NAME.git)
#   --git-url URL          Forgejo git remote to push to (default
#                          --web-url/OWNER/NAME.git)
#   --main-branch NAME     branch the copy is taken from (default: main)
#   --timeout SECONDS      how long to wait for the gate (default: 1200)
#   --poll-interval SEC    wait between status reads (default: 15)
#   --dry-run              read but never write: no push, no branch deletion
#   -h, --help             this text

set -euo pipefail

PROG=forgejo-probe.sh

# TAIL_BYTES bounds the job log tail printed for a red verdict, the same window
# internal/land carries into a rework note.
TAIL_BYTES=16384

# CURL_CONNECT_TIMEOUT and CURL_CALL_TIMEOUT bound one API call, as
# internal/land's call timeout does. Without them a wedged instance holds the
# poll past its deadline and keeps the exit trap's branch deletion from ever
# running.
CURL_CONNECT_TIMEOUT=10
CURL_CALL_TIMEOUT=30

die() { echo "$PROG: $*" >&2; exit 1; }
usage_die() { echo "$PROG: $*" >&2; echo "usage: $PROG --help" >&2; exit 2; }
log() { echo "$PROG: $*" >&2; }

usage() { sed -n '/^# Usage:/,/^$/p' "$0" | sed 's/^# \{0,1\}//'; }

# --- arguments ---------------------------------------------------------------

RIG=""
REPO=""
API_URL="${FORGEJO_API_URL:-http://127.0.0.1:3000/api/v1}"
WEB_URL="${FORGEJO_URL:-}"
ADMIN_TOKEN_FILE="${FORGEJO_ADMIN_ENV:-$HOME/forgejo/.env}"
GITHUB_URL=""
GIT_URL=""
MAIN_BRANCH="main"
TIMEOUT=1200
POLL_INTERVAL=15
DRY_RUN=0

while [ $# -gt 0 ]; do
  case "$1" in
    --api-url) [ $# -ge 2 ] || usage_die "--api-url needs a value"; API_URL=$2; shift 2 ;;
    --web-url) [ $# -ge 2 ] || usage_die "--web-url needs a value"; WEB_URL=$2; shift 2 ;;
    --admin-token-file) [ $# -ge 2 ] || usage_die "--admin-token-file needs a value"; ADMIN_TOKEN_FILE=$2; shift 2 ;;
    --repo) [ $# -ge 2 ] || usage_die "--repo needs a value"; REPO=$2; shift 2 ;;
    --github-url) [ $# -ge 2 ] || usage_die "--github-url needs a value"; GITHUB_URL=$2; shift 2 ;;
    --git-url) [ $# -ge 2 ] || usage_die "--git-url needs a value"; GIT_URL=$2; shift 2 ;;
    --main-branch) [ $# -ge 2 ] || usage_die "--main-branch needs a value"; MAIN_BRANCH=$2; shift 2 ;;
    --timeout) [ $# -ge 2 ] || usage_die "--timeout needs a value"; TIMEOUT=$2; shift 2 ;;
    --poll-interval) [ $# -ge 2 ] || usage_die "--poll-interval needs a value"; POLL_INTERVAL=$2; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    -*) usage_die "unknown option: $1" ;;
    *)
      [ -z "$RIG" ] || usage_die "unexpected argument: $1"
      RIG=$1
      shift ;;
  esac
done

# A name that lands in the probe branch: letters, digits and the separators a
# rig name may hold.
require_plain() { # require_plain LABEL VALUE
  case "$2" in
    ''|*[!A-Za-z0-9._-]*) usage_die "$1 '$2' must match [A-Za-z0-9._-]+" ;;
  esac
}
require_positive_int() { # require_positive_int LABEL VALUE
  case "$2" in
    ''|*[!0-9]*) usage_die "$1 '$2' must be a positive integer" ;;
    *) [ "$2" -gt 0 ] || usage_die "$1 '$2' must be a positive integer" ;;
  esac
}

[ -n "$RIG" ] || usage_die "<rig> is required"
require_plain "<rig>" "$RIG"
[ -n "$REPO" ] || usage_die "--repo is required"
case "$REPO" in
  */*/*|/*|*/|'') usage_die "--repo '$REPO' must be OWNER/NAME" ;;
  */*) ;;
  *) usage_die "--repo '$REPO' must be OWNER/NAME" ;;
esac
require_plain "--main-branch" "$MAIN_BRANCH"
require_positive_int "--timeout" "$TIMEOUT"
require_positive_int "--poll-interval" "$POLL_INTERVAL"
# 10# reads a leading zero as decimal rather than octal, so --timeout 010 is
# ten seconds and not eight.
TIMEOUT=$((10#$TIMEOUT))
POLL_INTERVAL=$((10#$POLL_INTERVAL))
case "$API_URL" in
  http://*|https://*) ;;
  *) usage_die "--api-url must be http:// or https:// (got '$API_URL')" ;;
esac
API_URL="${API_URL%/}"
[ -n "$WEB_URL" ] || WEB_URL="${API_URL%/api/v1}"

OWNER=${REPO%%/*}
NAME=${REPO#*/}
BRANCH="land/probe-$RIG"
# Forgejo resolves a branch name through the URL path, so the slash arrives
# encoded (go-gitea#21093, as in scripts/forgejo-provision.sh).
BRANCH_ENCODED=${BRANCH//\//%2F}

[ -n "$GITHUB_URL" ] || GITHUB_URL="https://github.com/$OWNER/$NAME.git"
[ -n "$GIT_URL" ] || GIT_URL="$WEB_URL/$OWNER/$NAME.git"

command -v curl >/dev/null 2>&1 || die "curl is not on PATH"
command -v git >/dev/null 2>&1 || die "git is not on PATH"

# --- HTTP --------------------------------------------------------------------

WORK="$(mktemp -d "${TMPDIR:-/tmp}/forgejo-probe.XXXXXX")"
COPY="$WORK/copy"
RESP_FILE="$WORK/response.json"
CURL_CONFIG="$WORK/curl.conf"
BRANCH_CREATED=0
RC=0

cleanup() {
  local rc=$?
  # A branch the probe cannot delete is left behind for every later run to
  # find, so it fails the run even when the gate was green.
  if [ "$BRANCH_CREATED" = 1 ]; then
    delete_branch || rc=1
  fi
  rm -rf "$WORK" || true
  exit "$rc"
}
trap cleanup EXIT

# The admin token goes in a mode-600 curl config rather than an argv -H, so it
# never shows in `ps` and never reaches a shell trace or a log line.
resolve_admin_token() {
  local token=""
  if [ -n "${FORGEJO_ADMIN_TOKEN:-}" ]; then
    token=$FORGEJO_ADMIN_TOKEN
  elif [ -f "$ADMIN_TOKEN_FILE" ]; then
    token=$(sed -n 's/^[[:space:]]*\(export[[:space:]]*\)\{0,1\}FORGEJO_ADMIN_TOKEN=//p' "$ADMIN_TOKEN_FILE" | tail -1 | sed "s/^['\"]//; s/['\"]$//")
  fi
  [ -n "$token" ] || die "no admin token: set FORGEJO_ADMIN_TOKEN, or FORGEJO_ADMIN_TOKEN= in $ADMIN_TOKEN_FILE"
  case "$token" in
    *$'\n'*|*'"'*|*\\*|*' '*) die "the admin token holds whitespace, a quote or a backslash; refusing to use it" ;;
  esac
  printf 'header = "Authorization: token %s"\n' "$token" > "$CURL_CONFIG"
  chmod 600 "$CURL_CONFIG"
}
resolve_admin_token

# api METHOD PATH: sends one request; HTTP_STATUS is the status code and the
# response body lands in RESP_FILE. A dry run sends the read methods only.
api() {
  local method=$1 path=$2
  if [ "$DRY_RUN" = 1 ] && [ "$method" != GET ]; then
    HTTP_STATUS=000
    : > "$RESP_FILE"
    return 0
  fi
  if ! HTTP_STATUS=$(curl -sS --connect-timeout "$CURL_CONNECT_TIMEOUT" --max-time "$CURL_CALL_TIMEOUT" \
    -o "$RESP_FILE" -w '%{http_code}' -X "$method" -K "$CURL_CONFIG" \
    -H 'Accept: application/json' "$API_URL$path"); then
    die "$method $API_URL$path: curl failed"
  fi
}

# body_tail renders the response, truncated, for an error message.
body_tail() {
  if [ -s "$RESP_FILE" ]; then
    printf ' — %s' "$(head -c 300 "$RESP_FILE" | tr '\n' ' ')"
  fi
}

# status_for_context CONTEXT prints the state of the newest commit status
# carrying CONTEXT, or nothing when the commit has no such status yet. Forgejo
# mirrors the Go struct, so "id" and "status" precede "context" inside one
# status object; the objects are split on "},{", which never separates a field
# from its value. A re-probe of an unchanged main pushes a commit that already
# carries the previous run's status, so the newest one is the one this push
# made, not the first in the list.
status_for_context() {
  local ctx=$1 line id state best_id="" best_state=""
  while IFS= read -r line; do
    case "$line" in
      *"\"context\":\"$ctx\""*) ;;
      *) continue ;;
    esac
    id=${line#*\"id\":}
    id=${id%%,*}
    case "$id" in ''|*[!0-9]*) continue ;; esac
    state=${line#*\"status\":\"}
    state=${state%%\"*}
    if [ -z "$best_id" ] || [ "$id" -gt "$best_id" ]; then
      best_id=$id
      best_state=$state
    fi
  done <<<"$(sed 's/},{/}\n{/g' "$RESP_FILE")"
  printf '%s' "$best_state"
}

# run_id_for SHA prints the id of the workflow run whose commit is SHA, or
# nothing. The run object serializes "id" first, before any other key holding
# the substring id.
run_id_for() {
  local sha=$1 line id=""
  while IFS= read -r line; do
    case "$line" in
      *"\"commit_sha\":\"$sha\""*)
        id=${line#*\"id\":}
        id=${id%%,*}
        break ;;
    esac
  done <<<"$(sed 's/},{/}\n{/g' "$RESP_FILE")"
  printf '%s' "$id"
}

# job_id_for NAME prints the id of the job named NAME, or nothing.
job_id_for() {
  local name=$1 line id=""
  while IFS= read -r line; do
    case "$line" in
      *"\"name\":\"$name\""*)
        id=${line#*\"id\":}
        id=${id%%,*}
        break ;;
    esac
  done <<<"$(sed 's/},{/}\n{/g' "$RESP_FILE")"
  printf '%s' "$id"
}

# failed_job_id prints the id of the first job whose status is failure, or
# nothing: a run whose job name differs from the workflow's has still a failed
# job to read.
failed_job_id() {
  local line id=""
  while IFS= read -r line; do
    case "$line" in
      *"\"status\":\"failure\""*)
        id=${line#*\"id\":}
        id=${id%%,*}
        break ;;
    esac
  done <<<"$(sed 's/},{/}\n{/g' "$RESP_FILE")"
  printf '%s' "$id"
}

# print_job_tail prints the failing job's log tail, the reason the gate went
# red. Every read failure is reported and returns: the verdict is the probe's
# answer, and a missing log leaves it standing.
print_job_tail() {
  local run_id job_id logfile="$WORK/job.log"
  api GET "/repos/$OWNER/$NAME/actions/runs?event=push&head_sha=$SHA&limit=10"
  if [ "$HTTP_STATUS" != 200 ]; then
    log "no run to read for $SHA (HTTP $HTTP_STATUS)$(body_tail)"
    return 0
  fi
  run_id=$(run_id_for "$SHA")
  if [ -z "$run_id" ]; then
    log "no workflow run tested $SHA"
    return 0
  fi
  api GET "/repos/$OWNER/$NAME/actions/runs/$run_id/jobs"
  if [ "$HTTP_STATUS" != 200 ]; then
    log "run $run_id: jobs could not be read (HTTP $HTTP_STATUS)$(body_tail)"
    return 0
  fi
  job_id=$(job_id_for "$GATE_JOB")
  if [ -z "$job_id" ]; then job_id=$(failed_job_id); fi
  if [ -z "$job_id" ]; then
    log "run $run_id has no job '$GATE_JOB' and no failed job"
    return 0
  fi
  log "tail of the log of run $run_id's job '$GATE_JOB':"
  # A server that ignores the byte range returns the whole log, so the tail is
  # taken here as well: the note the operator reads stays bounded either way.
  # -f so an error body (a 404 for a job that has no log yet) never lands in
  # the log file and gets printed as if it were the gate's output.
  if ! curl -sS -f --connect-timeout "$CURL_CONNECT_TIMEOUT" --max-time "$CURL_CALL_TIMEOUT" \
    -o "$logfile" -K "$CURL_CONFIG" -H 'Accept: text/plain' \
    -H "Range: bytes=-$TAIL_BYTES" "$API_URL/repos/$OWNER/$NAME/actions/jobs/$job_id/logs"; then
    log "the job log could not be read"
    return 0
  fi
  tail -c "$TAIL_BYTES" "$logfile"
  printf '\n'
}

# delete_branch removes the probe branch. It reports failures without dying:
# it runs from the exit trap, where a die would mask the verdict of the run.
delete_branch() {
  local status
  if [ "$DRY_RUN" = 1 ]; then
    log "would delete branch $BRANCH on $REPO"
    return 0
  fi
  if ! status=$(curl -sS --connect-timeout "$CURL_CONNECT_TIMEOUT" --max-time "$CURL_CALL_TIMEOUT" \
    -o "$RESP_FILE" -w '%{http_code}' -X DELETE -K "$CURL_CONFIG" \
    -H 'Accept: application/json' "$API_URL/repos/$OWNER/$NAME/branches/$BRANCH_ENCODED"); then
    log "warning: could not delete branch $BRANCH on $REPO: curl failed"
    return 1
  fi
  case "$status" in
    204|200) log "deleted branch $BRANCH on $REPO" ;;
    404) log "branch $BRANCH on $REPO was already gone" ;;
    *) log "warning: could not delete branch $BRANCH on $REPO (HTTP $status)$(body_tail)"; return 1 ;;
  esac
}

# --- the copy ----------------------------------------------------------------

# gate_name FILE prints the workflow's name: line, unquoted.
gate_name() {
  sed -n 's/^name:[[:space:]]*//p' "$1" | sed -n '1p' | sed "s/^['\"]//; s/['\"]$//"
}

# gate_jobs FILE prints the job keys under jobs:, one per line. The key is
# stripped in a copy of the line, never in $0: this awk re-runs the rule set
# against a record whose $0 a sub() changed, and a trimmed $0 matches a
# different rule than the line it came from.
gate_jobs() {
  awk '
    /^[^[:space:]]/ && !/^jobs:[[:space:]]*$/ { in_jobs = 0 }
    /^jobs:[[:space:]]*$/ { in_jobs = 1; next }
    in_jobs && /^  [A-Za-z0-9_.-]+:[[:space:]]*$/ {
      job = $0
      sub(/^  /, "", job)
      sub(/:.*/, "", job)
      print job
    }
  ' "$1"
}

# clone_copy clones the rig's GitHub repo in full into COPY. A shallow clone is
# refused by Forgejo on push, so no --depth is passed.
clone_copy() {
  local err
  if [ "$DRY_RUN" = 1 ]; then
    log "cloning the copy (a read): $GITHUB_URL, full, all branches"
  fi
  if ! err=$(git clone --quiet "$GITHUB_URL" "$COPY" 2>&1); then
    die "git clone $GITHUB_URL failed: $err"
  fi
}

# --- probing -----------------------------------------------------------------

# 1. The repository must be on this instance, and the token must see it.
api GET "/repos/$OWNER/$NAME"
case "$HTTP_STATUS" in
  200) ;;
  404) die "GET /repos/$REPO returned 404: the repository is not on $API_URL, or the admin token cannot see it$(body_tail)" ;;
  *) die "GET /repos/$REPO: unexpected HTTP $HTTP_STATUS$(body_tail)" ;;
esac

# 2. The copy, then the refusal, both before anything is pushed. The gate file
# is read from the ref that will be pushed, not from the clone's checked-out
# tree: with --main-branch other than the clone's default those two differ, and
# the pushed tree is the one Forgejo runs.
clone_copy
GATE_PATH=".forgejo/workflows/gate.yml"
if ! GATE_DOC=$(git -C "$COPY" show "refs/remotes/origin/$MAIN_BRANCH:$GATE_PATH" 2>/dev/null); then
  die "refusing: the copy of $REPO's $MAIN_BRANCH has no $GATE_PATH, so Forgejo would run the repo's .github/workflows and nothing would gate"
fi
GATE_FILE="$WORK/gate.yml"
printf '%s\n' "$GATE_DOC" > "$GATE_FILE"
GATE_NAME=$(gate_name "$GATE_FILE")
[ -n "$GATE_NAME" ] || die "$GATE_FILE has no name: line, so the required status context cannot be derived"
GATE_JOB=""
gate_job_count=0
while IFS= read -r job; do
  [ -n "$job" ] || continue
  GATE_JOB=$job
  gate_job_count=$((gate_job_count + 1))
done <<<"$(gate_jobs "$GATE_FILE")"
# One job, as internal/land.ParseGateWorkflow requires: a second job makes the
# required context ambiguous, and a guess would probe a context the branch
# protection does not demand.
[ "$gate_job_count" = 1 ] || die "$GATE_FILE names $gate_job_count jobs under jobs:, and the required context can only be derived from exactly one"
CONTEXT="$GATE_NAME / $GATE_JOB (push)"

SHA=$(git -C "$COPY" rev-parse --verify "refs/remotes/origin/$MAIN_BRANCH^{commit}") \
  || die "the copy has no refs/remotes/origin/$MAIN_BRANCH"

# 3. The probe branch. It is throwaway and unprotected, so a leftover from an
# interrupted probe is replaced rather than allowed to block the re-run.
if [ "$DRY_RUN" = 1 ]; then
  log "would push $MAIN_BRANCH ($SHA) to $GIT_URL as $BRANCH"
  log "would wait up to ${TIMEOUT}s for $CONTEXT on $SHA"
  log "would delete branch $BRANCH on $REPO"
  log "dry run: no push and no deletion was sent"
  exit 0
fi
if ! git -C "$COPY" push --quiet --force "$GIT_URL" \
  "refs/remotes/origin/$MAIN_BRANCH:refs/heads/$BRANCH"; then
  die "git push of $MAIN_BRANCH to $GIT_URL as $BRANCH failed"
fi
BRANCH_CREATED=1
log "pushed $MAIN_BRANCH ($SHA) to $REPO as $BRANCH; waiting up to ${TIMEOUT}s for $CONTEXT"

# 4. The runner's own verdict on that commit.
deadline=$(( $(date +%s) + TIMEOUT ))
VERDICT=""
while :; do
  api GET "/repos/$OWNER/$NAME/commits/$SHA/status"
  if [ "$HTTP_STATUS" != 200 ]; then
    VERDICT="unreadable (HTTP $HTTP_STATUS)"
    break
  fi
  STATE=$(status_for_context "$CONTEXT")
  case "$STATE" in
    success) VERDICT=success; break ;;
    pending|'') ;;
    *) VERDICT=$STATE; break ;;
  esac
  now=$(date +%s)
  if [ "$now" -ge "$deadline" ]; then
    VERDICT=timeout
    break
  fi
  remaining=$((deadline - now))
  if [ "$remaining" -gt "$POLL_INTERVAL" ]; then remaining=$POLL_INTERVAL; fi
  sleep "$remaining"
done

case "$VERDICT" in
  success)
    log "$RIG's gate is green on $SHA ($CONTEXT)"
    ;;
  timeout)
    log "$CONTEXT reported nothing on $SHA within ${TIMEOUT}s"
    print_job_tail
    RC=1
    ;;
  *)
    log "$CONTEXT is $VERDICT on $SHA"
    print_job_tail
    RC=1
    ;;
esac

exit "$RC"
