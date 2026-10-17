#!/usr/bin/env bash
# app-promote/run.sh — GitHub follows prod (gt-5xrmp, gt-vdhwk). Before changing
# what it promotes, read plugin.md beside this script: it defines the gate.

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

# The run list is asked a page at a time, for the deploy workflow alone: the
# repo's other workflows (health.yml runs hourly) would otherwise push release
# runs off the page. Forgejo 16.0.5 honors `limit` only alongside `page`
# (internal/dashboard/deploys.go). Runs come newest first, so the search stops at
# the first page holding a release; MAX_PAGES bounds a repo that has none.
RUNS_LIMIT=50
MAX_PAGES=10

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

# api PATH [ALSO_OK] GETs one API path. HTTP_STATUS is the code, RESP_FILE the
# body, and both a curl that could not reach Forgejo and an answer other than 200
# (or ALSO_OK, for a read whose "no such thing" is an answer) are failures: a
# read that failed must never read as "nothing to promote".
api() {
  local path=$1 also_ok=${2:-200}
  HTTP_STATUS=$(curl -sS -o "$RESP_FILE" -w '%{http_code}' -K "$CURL_CONFIG" \
    -H 'Accept: application/json' "$API_URL$path") ||
    fail "GET $API_URL$path: curl could not reach Forgejo"
  [ "$HTTP_STATUS" = 200 ] || [ "$HTTP_STATUS" = "$also_ok" ] ||
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

# release_candidates reads a run list on stdin and prints "<sha>\t<run id>\t<ref>"
# per successful run of the deploy workflow at a ref named like a release tag,
# newest first. Newest is by the run's created stamp, not the order the API sent
# (the dashboard sorts the same way, internal/dashboard/deploys.go). A run's
# prettyref is the tag or the branch it ran at and does not say which, so the
# caller confirms each with tag_commit; the name pattern here only keeps the tag
# lookup's URL safe.
release_candidates() {
  python3 -I -c '
import json, re, sys

workflow, prefix = sys.argv[1], sys.argv[2]
name = re.compile("^" + re.escape(prefix) + "[A-Za-z0-9._-]*$")
runs = []
for run in json.load(sys.stdin).get("workflow_runs") or []:
    if str(run.get("workflow_id") or "").rsplit("/", 1)[-1] != workflow:
        continue
    if not name.match(run.get("prettyref") or ""):
        continue
    if (run.get("status") or "") != "success":
        continue
    if run.get("commit_sha") and run.get("id"):
        runs.append(run)
runs.sort(key=lambda r: r.get("created") or "", reverse=True)
for run in runs:
    print("%s\t%s\t%s" % (run["commit_sha"], run["id"], run["prettyref"]))
' "$DEPLOY_WORKFLOW" "$TAG_PREFIX"
}

# tag_commit reads a Forgejo tag on stdin and prints the commit it names.
tag_commit() {
  python3 -I -c '
import json, sys

print((json.load(sys.stdin).get("commit") or {}).get("sha") or "")
'
}

# find_release REPO sets sha, run_id and tag to the newest successful deploy run
# whose ref is a tag naming the run's own commit, or returns 1 when there is
# none. A branch named like a release has no tag behind it (404), and a ref that
# is a tag but names another commit is no release of this run's.
find_release() {
  local repo=$1 page=1 candidates line ref_sha size
  local page_file="$WORK/runs-page.json"
  while [ "$page" -le "$MAX_PAGES" ]; do
    api "/repos/$repo/actions/runs?workflow_id=$DEPLOY_WORKFLOW&limit=$RUNS_LIMIT&page=$page"
    cp "$RESP_FILE" "$page_file"
    candidates="$(release_candidates <"$page_file")" || fail "cannot read the run list of $repo"
    while IFS= read -r line; do
      [ -n "$line" ] || continue
      IFS=$'\t' read -r sha run_id tag <<<"$line"
      api "/repos/$repo/tags/$tag" 404
      [ "$HTTP_STATUS" = 200 ] || continue
      ref_sha="$(tag_commit <"$RESP_FILE")" || fail "cannot read tag $tag of $repo"
      if [ "$ref_sha" = "$sha" ]; then
        return 0
      fi
    done <<<"$candidates"
    size="$(runs_in_page <"$page_file")" || fail "cannot read the run list of $repo"
    # A short page is the last one.
    [ "$size" -ge "$RUNS_LIMIT" ] || return 1
    page=$((page + 1))
  done
  return 1
}

# runs_in_page reads a run list on stdin and prints how many runs it holds: a
# full page means older ones may follow.
runs_in_page() {
  python3 -I -c '
import json, sys

print(len(json.load(sys.stdin).get("workflow_runs") or []))
'
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

# commits_ahead REPO BASE HEAD prints how many commits HEAD holds that BASE does
# not. 16.0.5 sends no comparison status, only those commits, so the count is
# the whole answer: nonzero is "HEAD is ahead of BASE", zero is "HEAD is BASE or
# an ancestor of it". On the rig's main, where history is linear, a commit that
# is not ahead of last_promoted can only be behind it. A reply without an integer
# count fails the run, because a check that cannot read its answer must not pass.
commits_ahead() {
  local repo=$1 base=$2 head=$3 count
  api "/repos/$repo/compare/$base...$head"
  count="$(python3 -I -c '
import json, sys

total = json.load(sys.stdin).get("total_commits")
if isinstance(total, bool) or not isinstance(total, int) or total < 0:
    sys.exit(1)
print(total)
' <"$RESP_FILE")" || fail "compare $base...$head in $repo: no commit count in the reply"
  printf '%s\n' "$count"
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

  if ! find_release "$repo"; then
    log "rig $rig: no successful $DEPLOY_WORKFLOW run on a $TAG_PREFIX* tag in $repo"
    continue
  fi
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
  off_main=$(commits_ahead "$repo" "$MAIN_BRANCH" "$sha")
  if [ "$off_main" -gt 0 ]; then
    log "rig $rig: $tag ($short) is not on $MAIN_BRANCH in $repo; not promoting"
    continue
  fi
  if [ -n "$last" ]; then
    ahead=$(commits_ahead "$repo" "$last" "$sha")
    if [ "$ahead" -eq 0 ]; then
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
