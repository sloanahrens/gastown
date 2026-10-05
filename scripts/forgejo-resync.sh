#!/usr/bin/env bash
# forgejo-resync.sh — make a Forgejo repo's refs match GitHub's and restore
# main's branch protection (gt-fn9e6.29).
#
# Use it when GitHub moved while a rig was rolled back, and as the break-glass
# import after a fix is pushed to GitHub: the fix becomes Forgejo's main by
# resyncing, not by pushing to Forgejo directly.
#
# Forgejo refuses even a site-admin push to a protected main (the pre-receive
# hook declines it), so a resync lifts the rule first and puts it back by
# re-running forgejo-provision.sh. The restore runs from an exit trap, so a
# push that fails still leaves main protected. Every ref is imported because
# the Forgejo push mirror carries no branch filter and prunes: any GitHub ref
# Forgejo lacks is deleted at the next mirror sync (decision 8 of the cutover
# design), so Forgejo must hold them all.
#
# Idempotent: a second run over converged state sends no ref update and leaves
# the same protection. The admin token travels only in a mode-600 curl config
# file and in the environment of the provisioning re-run, never in argv or on
# stdout.
#
# Usage: forgejo-resync.sh [options] OWNER/NAME
#   --api-url URL          API root (default $FORGEJO_API_URL,
#                          else http://127.0.0.1:3000/api/v1)
#   --web-url URL          web root the git remote is derived from (default
#                          $FORGEJO_URL, else --api-url less /api/v1)
#   --github-url URL       git remote to import from (default
#                          https://github.com/OWNER/NAME.git)
#   --forgejo-url URL      git remote to push to (default
#                          WEB_URL/OWNER/NAME.git)
#   --admin-token-file F   file holding FORGEJO_ADMIN_TOKEN=... (default
#                          $FORGEJO_ADMIN_ENV, else $HOME/forgejo/.env)
#   --main-branch NAME     protected landing target to lift and restore
#                          (default: main)
#   --backup-dir DIR       where the lifted rule's dated .bak- copy goes
#                          (default $FORGEJO_BACKUP_DIR, else
#                          $HOME/forgejo/backups)
#   --provision PATH       provisioner re-run to restore the rule (default: the
#                          forgejo-provision.sh beside this script)
#   --dry-run              read and print, never write
#   -h, --help             this text

set -euo pipefail

PROG=forgejo-resync.sh
SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

die() { echo "$PROG: $*" >&2; exit 1; }
usage_die() { echo "$PROG: $*" >&2; echo "usage: $PROG --help" >&2; exit 2; }
log() { echo "$PROG: $*" >&2; }

usage() { sed -n '/^# Usage:/,/^$/p' "$0" | sed 's/^# \{0,1\}//'; }

# --- arguments ---------------------------------------------------------------

API_URL="${FORGEJO_API_URL:-http://127.0.0.1:3000/api/v1}"
WEB_URL="${FORGEJO_URL:-}"
ADMIN_TOKEN_FILE="${FORGEJO_ADMIN_ENV:-$HOME/forgejo/.env}"
MAIN_BRANCH="main"
BACKUP_DIR="${FORGEJO_BACKUP_DIR:-$HOME/forgejo/backups}"
PROVISION="$SELF_DIR/forgejo-provision.sh"
GITHUB_GIT_URL=""
FORGEJO_GIT_URL=""
DRY_RUN=0
REPO=""

while [ $# -gt 0 ]; do
  case "$1" in
    --api-url) [ $# -ge 2 ] || usage_die "--api-url needs a value"; API_URL=$2; shift 2 ;;
    --web-url) [ $# -ge 2 ] || usage_die "--web-url needs a value"; WEB_URL=$2; shift 2 ;;
    --github-url) [ $# -ge 2 ] || usage_die "--github-url needs a value"; GITHUB_GIT_URL=$2; shift 2 ;;
    --forgejo-url) [ $# -ge 2 ] || usage_die "--forgejo-url needs a value"; FORGEJO_GIT_URL=$2; shift 2 ;;
    --admin-token-file) [ $# -ge 2 ] || usage_die "--admin-token-file needs a value"; ADMIN_TOKEN_FILE=$2; shift 2 ;;
    --main-branch) [ $# -ge 2 ] || usage_die "--main-branch needs a value"; MAIN_BRANCH=$2; shift 2 ;;
    --backup-dir) [ $# -ge 2 ] || usage_die "--backup-dir needs a value"; BACKUP_DIR=$2; shift 2 ;;
    --provision) [ $# -ge 2 ] || usage_die "--provision needs a value"; PROVISION=$2; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    -*) usage_die "unknown option: $1" ;;
    *) [ -z "$REPO" ] || usage_die "one repository only (already have '$REPO')"; REPO=$1; shift ;;
  esac
done

[ -n "$REPO" ] || usage_die "OWNER/NAME is required"
case "$REPO" in
  */*/*|/*|*/|'') usage_die "'$REPO' must be OWNER/NAME" ;;
  */*) ;;
  *) usage_die "'$REPO' must be OWNER/NAME" ;;
esac
case "$API_URL" in
  http://*|https://*) ;;
  *) usage_die "--api-url must be http:// or https:// (got '$API_URL')" ;;
esac
API_URL="${API_URL%/}"
[ -n "$WEB_URL" ] || WEB_URL="${API_URL%/api/v1}"
# The branch name and the two refspecs land in a URL path or on git's command
# line; a name outside this class is one this script cannot hand to git safely.
case "$MAIN_BRANCH" in
  ''|*[!A-Za-z0-9._/-]*) usage_die "--main-branch '$MAIN_BRANCH' must match [A-Za-z0-9._/-]+" ;;
esac
[ -f "$PROVISION" ] || usage_die "--provision '$PROVISION' is not a file"

OWNER=${REPO%%/*}
NAME=${REPO#*/}
[ -n "$GITHUB_GIT_URL" ] || GITHUB_GIT_URL="https://github.com/$REPO.git"
[ -n "$FORGEJO_GIT_URL" ] || FORGEJO_GIT_URL="${WEB_URL%/}/$REPO.git"

command -v curl >/dev/null 2>&1 || die "curl is not on PATH"
command -v git >/dev/null 2>&1 || die "git is not on PATH"

# --- work dir, token and trap ------------------------------------------------

WORK="$(mktemp -d "${TMPDIR:-/tmp}/forgejo-resync.XXXXXX")"
RESP_FILE="$WORK/response.json"
CURL_CONFIG="$WORK/curl.conf"
READY=0

# finish restores main's protection on every exit that got far enough to lift
# it, then removes the scratch dir. The restore is the same provisioning run the
# operator would make by hand, so it is safe to repeat after a partial failure.
finish() {
  local rc=$?
  if [ "$READY" = 1 ] && ! restore_protection; then
    log "ERROR: could not restore $MAIN_BRANCH protection on $REPO; run 'bash scripts/forgejo-provision.sh --repo $REPO' by hand"
    rc=1
  fi
  rm -rf "$WORK"
  exit "$rc"
}
trap finish EXIT

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

# --- HTTP --------------------------------------------------------------------

# api METHOD PATH: sends one request; HTTP_STATUS is the status code and the
# response body lands in RESP_FILE. A dry run sends the read methods only and
# prints the write it would have sent.
api() {
  local method=$1 path=$2
  if [ "$DRY_RUN" = 1 ] && [ "$method" != GET ]; then
    echo "+ $method $API_URL$path (dry run: not sent)" >&2
    HTTP_STATUS=000
    : > "$RESP_FILE"
    return 0
  fi
  echo "+ $method $API_URL$path" >&2
  local args=(-sS -o "$RESP_FILE" -w '%{http_code}' -X "$method" -K "$CURL_CONFIG"
    -H 'Accept: application/json' "$API_URL$path")
  HTTP_STATUS=$(curl "${args[@]}") || die "$method $API_URL$path: curl failed"
}

# body_tail renders the response, truncated, for an error message.
body_tail() {
  if [ -s "$RESP_FILE" ]; then
    printf ' — %s' "$(head -c 300 "$RESP_FILE" | tr '\n' ' ')"
  fi
}

# run COMMAND...: prints the command and runs it, or prints it alone under
# --dry-run. Nothing here carries the admin token: the curl calls read it from
# CURL_CONFIG, and the git remotes are credentials-free URLs the operator's
# credential helper resolves.
run() {
  echo "+ $*" >&2
  if [ "$DRY_RUN" = 1 ]; then
    return 0
  fi
  "$@"
}

# json_object FILE KEY VALUE prints the first top-level object of a compact JSON
# array that holds "KEY":"VALUE". The objects carry no nested object, so
# splitting on "},{" separates them.
json_object() {
  sed 's/},{/}\n{/g' "$1" | grep -F "\"$2\":\"$3\"" | head -1 || true
}

# --- protection --------------------------------------------------------------

# backup_rule RULE_JSON copies the rule this run is about to delete to a dated
# .bak- file, so an operator can restore it by hand if the provisioning re-run
# fails. It is the one piece of state the resync changes outside git.
backup_rule() {
  local path="$BACKUP_DIR/${OWNER}__${NAME}__${MAIN_BRANCH//\//%2F}.json.bak-$(date +%Y%m%d-%H%M%S)"
  echo "+ write the $MAIN_BRANCH rule to $path" >&2
  if [ "$DRY_RUN" = 1 ]; then
    return 0
  fi
  mkdir -p "$BACKUP_DIR"
  printf '%s\n' "$1" > "$path"
  log "saved the $MAIN_BRANCH rule to $path"
}

# lift_protection removes main's rule so an admin push is accepted. A rule
# another run already lifted is reported, not an error, so a run interrupted
# after the delete still converges.
lift_protection() {
  api GET "/repos/$OWNER/$NAME/branch_protections"
  case "$HTTP_STATUS" in
    200) ;;
    404) die "GET /repos/$REPO/branch_protections returned 404: the repository is not on $API_URL, or the admin token cannot see it$(body_tail)" ;;
    *) die "GET /repos/$REPO/branch_protections: unexpected HTTP $HTTP_STATUS$(body_tail)" ;;
  esac
  local existing
  existing=$(json_object "$RESP_FILE" rule_name "$MAIN_BRANCH")
  if [ -z "$existing" ]; then
    log "no $MAIN_BRANCH protection on $REPO: nothing to lift"
    return 0
  fi
  backup_rule "$existing"
  # The rule name is a branch glob and the per-rule route takes it in the path:
  # chi routes on the raw path, so "/" must arrive encoded as %2F (go-gitea#21093).
  local escaped=${MAIN_BRANCH//\//%2F}
  api DELETE "/repos/$OWNER/$NAME/branch_protections/$escaped"
  if [ "$DRY_RUN" = 1 ]; then
    return 0
  fi
  case "$HTTP_STATUS" in
    204) log "lifted $MAIN_BRANCH protection on $REPO" ;;
    404) log "lifted $MAIN_BRANCH protection on $REPO (already absent)" ;;
    *) die "DELETE /repos/$REPO/branch_protections/$escaped: unexpected HTTP $HTTP_STATUS$(body_tail)" ;;
  esac
}

# restore_protection re-runs the provisioner, which recreates main's rule from
# the same flags a cutover used. It also re-grants the role access the rule
# names, so a rule Forgejo dropped as unmergeable is repaired in the same run.
restore_protection() {
  run bash "$PROVISION" --repo "$REPO" --main-branch "$MAIN_BRANCH" \
    --api-url "$API_URL" --admin-token-file "$ADMIN_TOKEN_FILE"
}

# --- refs --------------------------------------------------------------------

# ls_remote URL prints one sorted "ref sha" line per branch and tag, so a plain
# string compare of two calls is the ref-list comparison.
ls_remote() {
  echo "+ git ls-remote --heads --tags --refs $1" >&2
  git ls-remote --heads --tags --refs "$1" | awk '{print $2" "$1}' | LC_ALL=C sort
}

normalize() { if [ -n "$1" ]; then printf '%s\n' "$1"; fi; }

# report_plan prints what the push will update or delete, so --dry-run shows the
# ref change before it happens and a real run records it.
report_plan() {
  local lines ref
  lines=$(comm -23 <(normalize "$1") <(normalize "$2") || true)
  if [ -n "$lines" ]; then
    while IFS=' ' read -r ref _; do
      if [ -n "$ref" ]; then log "will push $ref"; fi
    done <<< "$lines"
  fi
  lines=$(comm -13 <(normalize "$1") <(normalize "$2") || true)
  if [ -n "$lines" ]; then
    while IFS=' ' read -r ref _; do
      if [ -n "$ref" ]; then log "will delete $ref (absent from GitHub)"; fi
    done <<< "$lines"
  fi
  return 0
}

# --- run ---------------------------------------------------------------------

resolve_admin_token
READY=1

SCRATCH="$WORK/refs.git"
github_before=$(ls_remote "$GITHUB_GIT_URL")
forgejo_before=$(ls_remote "$FORGEJO_GIT_URL")
report_plan "$github_before" "$forgejo_before"

# Fetch GitHub first: a GitHub that cannot be read must not cost a window with
# main unprotected. The scratch repo is where the mirror is assembled before the
# one push to Forgejo.
run git init --bare --quiet "$SCRATCH"
run git -C "$SCRATCH" fetch --quiet "$GITHUB_GIT_URL" \
  '+refs/heads/*:refs/heads/*' '+refs/tags/*:refs/tags/*'

lift_protection

# --prune deletes every Forgejo ref the fetched GitHub set lacks, so the two ref
# lists match afterwards and the mirror has nothing left to prune.
run git -C "$SCRATCH" push --prune --quiet "$FORGEJO_GIT_URL" \
  '+refs/heads/*:refs/heads/*' '+refs/tags/*:refs/tags/*'

if [ "$DRY_RUN" = 1 ]; then
  log "dry run: no write was sent"
  exit 0
fi

github_after=$(ls_remote "$GITHUB_GIT_URL")
forgejo_after=$(ls_remote "$FORGEJO_GIT_URL")
if [ "$github_after" != "$forgejo_after" ]; then
  log "ERROR: Forgejo and GitHub refs differ after the push; a mirror sync would prune GitHub"
  diff <(normalize "$github_after") <(normalize "$forgejo_after") >&2 || true
  exit 1
fi
log "refs match: Forgejo holds every GitHub ref ($(printf '%s\n' "$forgejo_after" | grep -c .) refs)"
log "resync complete"
