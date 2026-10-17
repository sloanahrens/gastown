#!/usr/bin/env bash
# app-promote/run.sh — advance each configured app rig's GitHub main to the
# newest commit a release proved on prod: GitHub follows prod.
#
# What "proved on prod" means, what it refuses and the manual procedure:
# plugin.md and README.md beside this script (gt-5xrmp, gt-vdhwk).

set -euo pipefail

log() { printf '[app-promote] %s\n' "$*"; }

# fail is the one-line cause the plugin contract exits on: the daemon records a
# nonzero exit as a failure and escalates it once
# (internal/daemon/plugin_script.go).
fail() { printf 'app-promote: %s\n' "$*" >&2; exit 1; }

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RIGS_FILE="$SELF_DIR/rigs.conf"
# The town the rigs and their red-main records live in, as the daemon exports it
# to a script plugin.
TOWN_ROOT="${GT_TOWN_ROOT:-${GT_ROOT:-$HOME/gt}}"

# The API root and the token file of the read-only viewer role: the base
# internal/forgejo defaults to, and the file internal/forgejo/token.go reads
# under the config home. FORGEJO_API_URL is the override
# scripts/forgejo-cutover.sh honors.
API_URL="${FORGEJO_API_URL:-http://127.0.0.1:3000/api/v1}"
TOKEN_FILE="${XDG_CONFIG_HOME:-$HOME/.config}/gt/forgejo-viewer.env"

# The workflow whose run on a release tag deploys that commit to prod, the tag
# prefix that marks a run as a release (deploy.yml triggers on v*), the branch
# the released commit must be on, and the job that runs the integration tier
# against prod. A run at a branch is not a release and proves nothing about what
# prod serves; the job is a column of rigs.conf because a workflow that names it
# otherwise would read as a repo that never goes green.
DEPLOY_WORKFLOW="deploy.yml"
TAG_PREFIX="v"
MAIN_BRANCH="main"
DEFAULT_JOB="prod-integration"

# The run list is asked bounded; Forgejo 16.0.5 honors `limit` only alongside
# `page` and answers the repo's whole history otherwise
# (internal/dashboard/deploys.go).
RUNS_PAGE="limit=50&page=1"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/app-promote.XXXXXX")"
RESP_FILE="$WORK/response.json"
CURL_CONFIG="$WORK/curl.conf"
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

[ -d "$TOWN_ROOT" ] || fail "no town root at $TOWN_ROOT"
[ -f "$RIGS_FILE" ] || fail "no rig list at $RIGS_FILE"
command -v python3 >/dev/null || fail "python3 is not on PATH; the JSON reads need it"
command -v curl >/dev/null || fail "curl is not on PATH; the Forgejo reads need it"

# --- Forgejo reads -----------------------------------------------------------

# read_viewer_token writes the token into a mode-600 curl config rather than an
# argv -H, so it never shows in `ps` and never reaches a log line
# (scripts/forgejo-cutover.sh).
read_viewer_token() {
  [ -f "$TOKEN_FILE" ] ||
    fail "no viewer token at $TOKEN_FILE; create it with FORGEJO_TOKEN=... (mode 600)"
  local token
  token=$(sed -n 's/^[[:space:]]*\(export[[:space:]]*\)\{0,1\}FORGEJO_TOKEN=//p' "$TOKEN_FILE" |
    tail -n 1 | sed "s/^['\"]//; s/['\"]$//")
  [ -n "$token" ] || fail "no FORGEJO_TOKEN= line in $TOKEN_FILE"
  case "$token" in
  *[[:space:]]* | *'"'* | *'\\'*)
    fail "$TOKEN_FILE holds a token with whitespace, a quote or a backslash; refusing to use it"
    ;;
  esac
  printf 'header = "Authorization: token %s"\n' "$token" >"$CURL_CONFIG"
  chmod 600 "$CURL_CONFIG"
}

# api PATH GETs one API path. HTTP_STATUS is the code, RESP_FILE the body, and
# both a curl that could not reach Forgejo and a non-200 answer are failures: a
# read that failed must never read as "nothing to promote".
api() {
  local path=$1
  HTTP_STATUS=$(curl -sS -o "$RESP_FILE" -w '%{http_code}' -K "$CURL_CONFIG" \
    -H 'Accept: application/json' "$API_URL$path") ||
    fail "GET $API_URL$path: curl could not reach Forgejo"
  [ "$HTTP_STATUS" = 200 ] ||
    fail "GET $API_URL$path returned $HTTP_STATUS: $(head -c 300 "$RESP_FILE" | tr '\n' ' ')"
}

# --- The rigs it promotes for ------------------------------------------------

# read_rigs prints the rig list: "<rig> <owner/repo> [<integration job>]" per
# line, with # comments and blank lines dropped.
read_rigs() {
  sed -e 's/#.*//' -e '/^[[:space:]]*$/d' "$RIGS_FILE"
}

# --- Per-rig reads -----------------------------------------------------------

# last_promoted RIG prints the commit GitHub's main was last advanced to for
# RIG, or "" when the rig has no red-main record yet (a first promotion).
last_promoted() {
  local state="$TOWN_ROOT/.runtime/red-main/$1.json"
  [ -f "$state" ] || return 0
  python3 -I -c '
import json, sys
with open(sys.argv[1]) as f:
    print(json.load(f).get("last_promoted") or "")
' "$state" || fail "rig $1: cannot read the red-main record $state"
}

# pick_release_run reads a run list on stdin and prints "<sha>\t<run id>\t<tag>"
# for the newest run of the deploy workflow on a release tag that succeeded, or
# nothing. Newest is by the run's created stamp, not the order the API sent (the
# dashboard sorts the same way, internal/dashboard/deploys.go). A tag run's
# prettyref is the tag itself and a branch run's is the branch.
pick_release_run() {
  python3 -I -c '
import json, sys

workflow, prefix = sys.argv[1], sys.argv[2]
data = json.load(sys.stdin)
best = None
for run in data.get("workflow_runs") or []:
    if str(run.get("workflow_id") or "").rsplit("/", 1)[-1] != workflow:
        continue
    if not (run.get("prettyref") or "").startswith(prefix):
        continue
    if (run.get("status") or "") != "success":
        continue
    if best is None or (run.get("created") or "") > (best.get("created") or ""):
        best = run
if best is not None and best.get("commit_sha") and best.get("id"):
    print("%s\t%s\t%s" % (best["commit_sha"], best["id"], best["prettyref"]))
' "$DEPLOY_WORKFLOW" "$TAG_PREFIX"
}

# has_integration_job JOB reads a run's jobs on stdin and prints yes when JOB is
# among them and succeeded. A run the API calls successful can still lack it:
# the workflow predates the job, or the job is named otherwise in this rig.
has_integration_job() {
  python3 -I -c '
import json, sys

jobs = json.load(sys.stdin) or []
want = sys.argv[1]
print("yes" if any(
    (j.get("name") or "") == want and (j.get("status") or "") == "success"
    for j in jobs) else "no")
' "$1"
}

# compare_count reads a Forgejo compare of A...B on stdin and prints how many
# commits B holds that A does not. 16.0.5 sends no comparison status, only those
# commits, so the count is the whole answer: nonzero is "B is ahead of A", zero
# is "B is A or an ancestor of A". On the rig's main, where history is linear, a
# commit that is not ahead of last_promoted can only be behind it.
compare_count() {
  python3 -I -c '
import json, sys

print(int(json.load(sys.stdin).get("total_commits") or 0))
'
}

# --- Main --------------------------------------------------------------------

read_viewer_token

checked=0
promoted=0
while read -r rig repo job; do
  [ -n "$rig" ] && [ -n "$repo" ] ||
    fail "$RIGS_FILE: '$rig $repo' needs a rig and an owner/repo"
  job="${job:-$DEFAULT_JOB}"
  checked=$((checked + 1))

  api "/repos/$repo/actions/runs?$RUNS_PAGE"
  pick="$(pick_release_run <"$RESP_FILE")" || fail "rig $rig: cannot read the run list of $repo"
  if [ -z "$pick" ]; then
    log "rig $rig: no successful $DEPLOY_WORKFLOW run on a $TAG_PREFIX* tag in $repo"
    continue
  fi
  IFS=$'\t' read -r sha run_id tag <<<"$pick"
  short="${sha:0:8}"

  api "/repos/$repo/actions/runs/$run_id/jobs"
  if [ "$(has_integration_job "$job" <"$RESP_FILE")" != yes ]; then
    log "rig $rig: run $run_id ($tag, $short) has no successful '$job' job; prod is not proven"
    continue
  fi

  last="$(last_promoted "$rig")"
  if [ "$sha" = "$last" ]; then
    log "rig $rig: $short is already promoted; nothing to do"
    continue
  fi
  # A tag can name a commit that never reached the rig's main; gt promote
  # refuses that too, but the plugin says so without calling it.
  api "/repos/$repo/compare/$MAIN_BRANCH...$sha"
  if [ "$(compare_count <"$RESP_FILE")" -gt 0 ]; then
    log "rig $rig: $tag ($short) is not on $MAIN_BRANCH in $repo; not promoting"
    continue
  fi
  if [ -n "$last" ]; then
    api "/repos/$repo/compare/$last...$sha"
    if [ "$(compare_count <"$RESP_FILE")" -eq 0 ]; then
      log "rig $rig: $short is not ahead of last_promoted ${last:0:8}; nothing to do"
      continue
    fi
  fi

  rc=0
  out="$(cd "$TOWN_ROOT" && gt promote --rig "$rig" --sha "$sha" 2>&1)" || rc=$?
  cause="$(printf '%s\n' "$out" | grep -v '^[[:space:]]*$' | tail -n 1 || true)"
  if [ "$rc" -eq 0 ]; then
    log "rig $rig: promoted $short ($tag)"
    promoted=$((promoted + 1))
    continue
  fi
  # A divergence is gt promote's own alert (landing-promote-diverged:<rig>,
  # internal/cmd/promote.go); raising the plugin's on top of it would page a
  # human twice for one condition.
  case "$out" in
  *"divergence is escalated"*)
    log "rig $rig: $cause"
    continue
    ;;
  esac
  # Exit 1 is gt promote refusing (the rig names no promote_target, the sha is
  # not on the rig's main, another promotion holds it). The cooldown retries, so
  # it is this run's nothing-to-do rather than a plugin failure.
  if [ "$rc" -eq 1 ]; then
    log "rig $rig: not promotable: $cause"
    continue
  fi
  fail "rig $rig: gt promote --sha $sha exited $rc: $cause"
done < <(read_rigs)

# A rig list that parsed to nothing is an operator error, not a run with nothing
# to do: the receipt must not say "skipped" for a plugin that read no rig at all.
[ "$checked" -gt 0 ] || fail "no rigs in $RIGS_FILE"

if [ "$promoted" -eq 0 ]; then
  log "=== nothing to promote ($checked rig(s) checked) ==="
  # The marker is what the daemon reads an exit-0 run with no work as
  # (scriptSkippedMarker, internal/daemon/plugin_script.go), so the receipt says
  # skipped instead of claiming the plugin promoted something.
  printf '[plugin-result skipped]\n'
else
  log "=== promoted $promoted of $checked rig(s) ==="
fi
