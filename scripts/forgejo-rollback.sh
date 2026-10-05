#!/usr/bin/env bash
# forgejo-rollback.sh — roll a rig back from Forgejo-primary landing to GitHub
# (gt-fn9e6.12). It is the operator's move, never an automatic one: the triggers
# (Forgejo or the runner down with landings queued, two consecutive
# infrastructure failures, a mirror error that will not clear) are read from the
# alerts by a human.
#
# In order it:
#
#   1. stops the rig's push mirror (when it has one) and prints the gh command
#      that removes its deploy key, BEFORE anything is repointed — a mirror left
#      running pushes every ref to GitHub and would overwrite it — and prints the
#      gh command that removes a promote deploy key;
#   2. removes the rig's merge_queue.forgejo block from settings/config.json, so
#      any promote_target and promote_key_file go with it, and deletes the
#      promote keypair the cutover minted;
#   3. repoints origin in the rig's bare repository, mayor/rig and every crew
#      clone, and git_url in mayor/town.json, back to the GitHub URL;
#   4. restarts the daemon (gt daemon restart) only when no landing is in
#      flight — a rollback runs while landings are queued, so the restart is
#      reported and left to the operator instead of forced over one.
#
# Idempotent: a run over a rig with no mirror, no block, no promote key and
# remotes already at the GitHub URL reports each of those and changes nothing.
# --dry-run reads and prints and writes nothing. Every edit of a config file
# copies it to a dated .bak- file first. The admin token travels only in a
# mode-600 curl config file, never in argv or on stdout.
#
# Live runbook: docs/forgejo-runbook.md. Design of record:
# docs/design/forgejo-primary-landing.md (section 3); the no-filter mirror and
# the rollback triggers are decisions 4 and 8 there.
#
# Usage: forgejo-rollback.sh <rig> [options]
#   --town-root DIR       town root holding <rig> (default $GT_TOWN_ROOT, else
#                         $GT_ROOT, else $HOME/gt)
#   --github-url URL      where the remotes and git_url go back to (default:
#                         the rig block's mirror_target)
#   --main-branch NAME    branch named in the operator's message (default: main)
#   --api-url URL         API root (default $FORGEJO_API_URL, else
#                         http://127.0.0.1:3000/api/v1)
#   --web-url URL         web root named in messages (default $FORGEJO_URL,
#                         else --api-url less /api/v1)
#   --admin-token-file F  file holding FORGEJO_ADMIN_TOKEN=... (default
#                         $FORGEJO_ADMIN_ENV, else $HOME/forgejo/.env)
#   --gt PATH             gt binary for the daemon restart (default $GT_BIN,
#                         else gt)
#   --dry-run             read and print, never write
#   -h, --help            this text
#
# A promote keypair the cutover minted — promote-<rig>.key and its .pub, under
# $XDG_CONFIG_HOME/gt else ~/.config/gt — is deleted with the block, so a
# rollback leaves no authorised key behind.

set -euo pipefail

PROG=forgejo-rollback.sh

# The label the landing worker drains (internal/land LabelReadyToLand); it is
# read only to decide whether a daemon restart is safe.
READY_LABEL="gt:ready-to-land"

die() { echo "$PROG: $*" >&2; exit 1; }
usage_die() { echo "$PROG: $*" >&2; echo "usage: $PROG --help" >&2; exit 2; }
log() { echo "$PROG: $*" >&2; }
# The operator's action items go to stdout: the gh command that removes the
# mirror's or the promote deploy key. Progress goes to stderr.
note() { echo "$*"; }

usage() { sed -n '/^# Usage:/,/^$/p' "$0" | sed 's/^# \{0,1\}//'; }

# --- arguments ---------------------------------------------------------------

RIG=""
TOWN_ROOT="${GT_TOWN_ROOT:-${GT_ROOT:-$HOME/gt}}"
# The config dir the cutover mints the promote key in (gt-fn9e6.40), the same
# dir the Forgejo token files use: $XDG_CONFIG_HOME/gt, else ~/.config/gt.
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/gt"
API_URL="${FORGEJO_API_URL:-http://127.0.0.1:3000/api/v1}"
WEB_URL="${FORGEJO_URL:-}"
ADMIN_TOKEN_FILE="${FORGEJO_ADMIN_ENV:-$HOME/forgejo/.env}"
MAIN_BRANCH="main"
GITHUB_URL=""
GT_BIN="${GT_BIN:-gt}"
DRY_RUN=0

while [ $# -gt 0 ]; do
  case "$1" in
    --town-root) [ $# -ge 2 ] || usage_die "--town-root needs a value"; TOWN_ROOT=$2; shift 2 ;;
    --github-url) [ $# -ge 2 ] || usage_die "--github-url needs a value"; GITHUB_URL=$2; shift 2 ;;
    --main-branch) [ $# -ge 2 ] || usage_die "--main-branch needs a value"; MAIN_BRANCH=$2; shift 2 ;;
    --api-url) [ $# -ge 2 ] || usage_die "--api-url needs a value"; API_URL=$2; shift 2 ;;
    --web-url) [ $# -ge 2 ] || usage_die "--web-url needs a value"; WEB_URL=$2; shift 2 ;;
    --admin-token-file) [ $# -ge 2 ] || usage_die "--admin-token-file needs a value"; ADMIN_TOKEN_FILE=$2; shift 2 ;;
    --gt) [ $# -ge 2 ] || usage_die "--gt needs a value"; GT_BIN=$2; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    -*) usage_die "unknown option: $1" ;;
    *)
      [ -z "$RIG" ] || usage_die "one rig only (already have '$RIG')"
      RIG=$1
      shift ;;
  esac
done

case "$RIG" in
  ''|*[!A-Za-z0-9._-]*) usage_die "<rig> '$RIG' must match [A-Za-z0-9._-]+" ;;
esac
case "$MAIN_BRANCH" in
  ''|*[!A-Za-z0-9._/-]*) usage_die "--main-branch '$MAIN_BRANCH' must match [A-Za-z0-9._/-]+" ;;
esac
case "$API_URL" in
  http://*|https://*) ;;
  *) usage_die "--api-url must be http:// or https:// (got '$API_URL')" ;;
esac
API_URL="${API_URL%/}"
[ -n "$WEB_URL" ] || WEB_URL="${API_URL%/api/v1}"
[ -d "$TOWN_ROOT" ] || usage_die "--town-root '$TOWN_ROOT' is not a directory"

command -v curl >/dev/null 2>&1 || die "curl is not on PATH"
command -v git >/dev/null 2>&1 || die "git is not on PATH"
command -v python3 >/dev/null 2>&1 || die "python3 is not on PATH: it is how the script edits the rig settings and town.json"

# --- the rig's files ---------------------------------------------------------

RIG_ROOT="$TOWN_ROOT/$RIG"
[ -d "$RIG_ROOT" ] || die "no rig directory $RIG_ROOT (is '$RIG' a registered rig?)"
BARE="$RIG_ROOT/.repo.git"
[ -d "$BARE" ] || die "no bare repository $BARE"
SETTINGS="$RIG_ROOT/settings/config.json"
TOWN_JSON="$TOWN_ROOT/mayor/town.json"
[ -f "$TOWN_JSON" ] || die "no $TOWN_JSON"

# slug OWNER/NAME prints the OWNER/NAME a git URL points at.
slug() { # slug URL
  local url=$1 rest
  case "$url" in
    git@*:*) rest=${url#*:} ;;
    ssh://*) rest=${url#ssh://}; rest=${rest#*@}; rest=${rest#*/} ;;
    *://*) rest=${url#*://}; rest=${rest#*/} ;;
    *) rest=$url ;;
  esac
  printf '%s' "${rest%.git}"
}

# block_fields prints the block's remote_url, mirror_target, promote_target and
# promote_key_file, separated by the ASCII unit separator, empty fields when the
# rig has no block. A promote cutover writes the promote pair and no
# mirror_target (gt-fn9e6.40); a mirror cutover writes mirror_target and neither
# promote field. The separator is a unit separator and not a tab because read
# treats a run of tabs as one delimiter and drops an empty field, which is
# exactly what a promote block has where mirror_target would be.
block_fields() {
  python3 - "$SETTINGS" <<'PY'
import json, os, sys
path = sys.argv[1]
block = {}
if os.path.exists(path):
    with open(path, encoding="utf-8") as fh:
        block = json.load(fh).get("merge_queue", {}).get("forgejo") or {}
print("\x1f".join((
    block.get("remote_url", ""),
    block.get("mirror_target", ""),
    block.get("promote_target", ""),
    block.get("promote_key_file", ""),
)))
PY
}

IFS=$'\x1f' read -r BLOCK_REMOTE_URL BLOCK_MIRROR_TARGET BLOCK_PROMOTE_TARGET BLOCK_PROMOTE_KEY \
  <<<"$(block_fields)"
if [ -z "$BLOCK_REMOTE_URL" ]; then
  log "$SETTINGS has no merge_queue.forgejo block: $RIG does not look cut over"
else
  log "$RIG is cut over to $BLOCK_REMOTE_URL"
fi

# Where the remotes go back to: --github-url, else the block's mirror_target or
# promote_target, else the rig's own origin. A rig already rolled back has no
# block, and its origin is the GitHub URL the run would set anyway, so a second
# rollback is a no-op that reports the state instead of refusing.
if [ -z "$GITHUB_URL" ]; then
  GITHUB_URL=$BLOCK_MIRROR_TARGET
fi
if [ -z "$GITHUB_URL" ]; then
  GITHUB_URL=$BLOCK_PROMOTE_TARGET
fi
if [ -z "$GITHUB_URL" ]; then
  GITHUB_URL=$(git -C "$BARE" remote get-url origin 2>/dev/null || true)
fi
[ -n "$GITHUB_URL" ] || die "no --github-url to repoint to, and $BARE has no origin URL to read one from"
case "$GITHUB_URL" in
  *://*|git@*:*) ;;
  *) usage_die "--github-url '$GITHUB_URL' is not a git URL" ;;
esac
GITHUB_SLUG="$(slug "$GITHUB_URL")"
case "$GITHUB_SLUG" in
  */*) ;;
  *) usage_die "--github-url '$GITHUB_URL' does not name OWNER/NAME" ;;
esac

MIRROR_TARGET="$BLOCK_MIRROR_TARGET"
# The default path the cutover mints the promote key at. It is derived from the
# rig, never read from the block, so a block naming some other file cannot make
# this script delete an unrelated path (gt-fn9e6.40).
PROMOTE_KEY="$CONFIG_DIR/promote-$RIG.key"
PROMOTE_PUB="$PROMOTE_KEY.pub"
OWNER=""
NAME=""
if [ -n "$BLOCK_REMOTE_URL" ]; then
  FORGEJO_SLUG="$(slug "$BLOCK_REMOTE_URL")"
  case "$FORGEJO_SLUG" in
    */*) OWNER=${FORGEJO_SLUG%%/*}; NAME=${FORGEJO_SLUG#*/} ;;
    *) die "the rig block's remote_url '$BLOCK_REMOTE_URL' does not name OWNER/NAME" ;;
  esac
fi

# --- work dir, token and trap ------------------------------------------------

WORK="$(mktemp -d "${TMPDIR:-/tmp}/forgejo-rollback.XXXXXX")"
RESP_FILE="$WORK/response.json"
CURL_CONFIG="$WORK/curl.conf"
cleanup() { rm -rf "$WORK"; }
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

# --- HTTP --------------------------------------------------------------------

# api METHOD PATH: sends one request; HTTP_STATUS is the status code and the
# response body lands in RESP_FILE. A dry run sends the read methods only.
api() {
  local method=$1 path=$2
  if [ "$DRY_RUN" = 1 ] && [ "$method" != GET ]; then
    log "+ $method $API_URL$path (dry run: not sent)"
    HTTP_STATUS=000
    : > "$RESP_FILE"
    return 0
  fi
  local args=(-sS -o "$RESP_FILE" -w '%{http_code}' -X "$method" -K "$CURL_CONFIG"
    -H 'Accept: application/json' "$API_URL$path")
  HTTP_STATUS=$(curl "${args[@]}") || die "$method $API_URL$path: curl failed"
}

body_tail() {
  if [ -s "$RESP_FILE" ]; then
    printf ' — %s' "$(head -c 300 "$RESP_FILE" | tr '\n' ' ')"
  fi
}

# json_object FILE KEY VALUE prints the first object of a compact JSON array
# that holds "KEY":"VALUE" (as scripts/forgejo-resync.sh does).
json_object() {
  sed 's/},{/}\n{/g' "$1" | grep -F "\"$2\":\"$3\"" | head -1 || true
}

# json_field OBJECT KEY prints a top-level string field of OBJECT.
json_field() { # json_field OBJECT KEY
  printf '%s' "$1" | sed -n 's/.*"'"$2"'":"\([^"]*\)".*/\1/p'
}

# run COMMAND...: prints the exact command and runs it, or prints it alone under
# --dry-run. No argument carries the admin token.
run() {
  local q="" a
  for a in "$@"; do q="$q$(printf '%q' "$a") "; done
  log "+ ${q% }"
  if [ "$DRY_RUN" = 1 ]; then
    return 0
  fi
  "$@"
}

backup_file() {
  local f=$1 b
  if [ ! -f "$f" ]; then
    log "no $f to back up (it does not exist yet)"
    return 0
  fi
  b="$f.bak-$(date +%Y%m%d-%H%M%S)"
  run cp -p "$f" "$b"
  log "backed up $f to $b"
}

# landing_count prints the rig's open ready-to-land beads, or nothing when bd
# cannot answer. A rollback must not be blocked by a bad read, so an unreadable
# queue is treated as "something may be in flight" by the restart step.
landing_count() {
  local out
  if ! out=$(bd -C "$RIG_ROOT" count --status open --label "$READY_LABEL" 2>/dev/null); then
    return 1
  fi
  case "$out" in
    ''|*[!0-9]*) return 1 ;;
  esac
  printf '%s' "$out"
}

# --- the mirror and the block ------------------------------------------------

# stop_mirror is step 1's first half. It runs before anything is repointed: a
# running mirror pushes every ref to GitHub and would overwrite whatever lands
# there.
stop_mirror() {
  local existing name
  api GET "/repos/$OWNER/$NAME/push_mirrors"
  case "$HTTP_STATUS" in
    200) ;;
    404) die "GET /repos/$OWNER/$NAME/push_mirrors returned 404: the repository is not on $API_URL, or the admin token cannot see it$(body_tail)" ;;
    *) die "GET /repos/$OWNER/$NAME/push_mirrors: unexpected HTTP $HTTP_STATUS$(body_tail)" ;;
  esac
  existing=$(json_object "$RESP_FILE" remote_address "$MIRROR_TARGET")
  if [ -z "$existing" ]; then
    log "no push mirror to $MIRROR_TARGET on $OWNER/$NAME: nothing to stop"
    return 0
  fi
  name=$(json_field "$existing" remote_name)
  case "$name" in
    ''|*[!A-Za-z0-9._-]*) die "the mirror to $MIRROR_TARGET has remote_name '$name', which this script will not put in a URL path; delete it in the web UI under Settings → Mirrors" ;;
  esac
  api DELETE "/repos/$OWNER/$NAME/push_mirrors/$name"
  if [ "$DRY_RUN" = 1 ]; then
    return 0
  fi
  case "$HTTP_STATUS" in
    204|200) log "stopped the push mirror to $MIRROR_TARGET on $OWNER/$NAME" ;;
    404) log "the push mirror to $MIRROR_TARGET on $OWNER/$NAME was already gone" ;;
    *) die "DELETE /repos/$OWNER/$NAME/push_mirrors/$name: unexpected HTTP $HTTP_STATUS$(body_tail)" ;;
  esac
}

# print_deploy_key_removal is step 1's second half: stopping the mirror stops
# the overwriting, and removing the key stops a future one. The key is named for
# the rig, so the operator can find it by title.
print_deploy_key_removal() {
  note ""
  note "Remove the mirror's deploy key from GitHub (it can no longer be used once the mirror is stopped, but it should not stay authorised):"
  note ""
  note "  gh repo deploy-key list --repo $GITHUB_SLUG"
  note "  gh repo deploy-key delete <key-id> --repo $GITHUB_SLUG   # the key titled forgejo-mirror-$RIG"
}

# print_promote_key_removal is step 1 for a promoting rig: after the rollback
# the rig lands on GitHub again, so the promotion's deploy key should not stay
# authorised. It is de-authorised by hand, as it was added.
print_promote_key_removal() {
  note ""
  note "Remove the promote deploy key from GitHub (the rig lands on GitHub again after the rollback, so the promotion key should not stay authorised):"
  note ""
  note "  gh repo deploy-key list --repo $GITHUB_SLUG"
  note "  gh repo deploy-key delete <key-id> --repo $GITHUB_SLUG   # the key titled promote-$RIG"
}

# remove_settings_forgejo is step 2: drop the block, so the promote_target and
# promote_key_file fields go with it and the daemon (restarted below) resolves
# the rig to origin again.
remove_settings_forgejo() {
  python3 - "$SETTINGS" <<'PY'
import json, os, sys
path = sys.argv[1]
with open(path, encoding="utf-8") as fh:
    data = json.load(fh)
data.get("merge_queue", {}).pop("forgejo", None)
with open(path, "w", encoding="utf-8") as fh:
    json.dump(data, fh, indent=2)
    fh.write("\n")
PY
}

# remove_promote_key deletes the promote keypair the cutover minted at the
# default path: the public half and the private half. The path is derived from
# the rig, so a block naming a different promote_key_file is reported for the
# operator to delete rather than handed to rm. rm -f keeps a second rollback a
# no-op.
remove_promote_key() {
  local f
  if [ -n "$BLOCK_PROMOTE_KEY" ] && [ "$BLOCK_PROMOTE_KEY" != "$PROMOTE_KEY" ]; then
    log "the block names promote_key_file $BLOCK_PROMOTE_KEY; this rollback removes only the default $PROMOTE_KEY, so delete that file yourself"
  fi
  if [ ! -f "$PROMOTE_KEY" ] && [ ! -f "$PROMOTE_PUB" ]; then
    log "no promote key at $PROMOTE_KEY: nothing to remove"
    return 0
  fi
  for f in "$PROMOTE_PUB" "$PROMOTE_KEY"; do
    [ -f "$f" ] || continue
    run rm -f -- "$f"
    if [ "$DRY_RUN" = 1 ]; then
      log "would remove $f"
    else
      log "removed $f"
    fi
  done
}

# --- remotes and config ------------------------------------------------------

repoint() { # repoint DIR URL
  local dir=$1 url=$2 current
  if ! git -C "$dir" rev-parse --git-dir >/dev/null 2>&1; then
    log "skip $dir (not a git repository)"
    return 0
  fi
  current=$(git -C "$dir" remote get-url origin 2>/dev/null || true)
  if [ "$current" = "$url" ]; then
    log "$dir: origin is already $url"
    return 0
  fi
  run git -C "$dir" remote set-url origin "$url"
}

repoint_remotes() {
  local url=$1 dir
  repoint "$BARE" "$url"
  if [ -d "$RIG_ROOT/mayor/rig" ]; then
    repoint "$RIG_ROOT/mayor/rig" "$url"
  fi
  if [ -d "$RIG_ROOT/crew" ]; then
    for dir in "$RIG_ROOT"/crew/*/; do
      dir=${dir%/}
      [ -d "$dir" ] || continue
      repoint "$dir" "$url"
    done
  fi
}

town_git_url() {
  python3 - "$TOWN_JSON" "$RIG" <<'PY'
import json, sys
path, rig = sys.argv[1], sys.argv[2]
with open(path, encoding="utf-8") as fh:
    data = json.load(fh)
entry = data.get("registry", {}).get("rigs", {}).get(rig)
if entry is None:
    sys.exit("town.json has no registry.rigs.%s" % rig)
print(entry.get("git_url", ""))
PY
}

set_town_git_url() {
  python3 - "$TOWN_JSON" "$RIG" "$1" <<'PY'
import json, sys
path, rig, url = sys.argv[1], sys.argv[2], sys.argv[3]
with open(path, encoding="utf-8") as fh:
    data = json.load(fh)
data["registry"]["rigs"][rig]["git_url"] = url
with open(path, "w", encoding="utf-8") as fh:
    json.dump(data, fh, indent=2)
    fh.write("\n")
PY
}

# --- run ---------------------------------------------------------------------

log "rolling $RIG back to $GITHUB_URL"

# 1. The mirror first, before anything is repointed. A promoting rig has no
# mirror, only a deploy key to de-authorise.
if [ -n "$OWNER" ] && [ -n "$MIRROR_TARGET" ]; then
  resolve_admin_token
  stop_mirror
  print_deploy_key_removal
elif [ -n "$OWNER" ]; then
  log "the rig block names no mirror_target: no push mirror to stop"
fi
if [ -n "$BLOCK_PROMOTE_TARGET" ] || [ -n "$BLOCK_PROMOTE_KEY" ]; then
  print_promote_key_removal
fi

# 2. The block, and the promote keypair the cutover minted.
if [ -n "$BLOCK_REMOTE_URL" ]; then
  backup_file "$SETTINGS"
  if [ "$DRY_RUN" = 1 ]; then
    log "would remove merge_queue.forgejo from $SETTINGS"
  else
    remove_settings_forgejo
    log "removed merge_queue.forgejo from $SETTINGS"
  fi
fi
if [ -n "$BLOCK_PROMOTE_TARGET" ] || [ -n "$BLOCK_PROMOTE_KEY" ] \
  || [ -f "$PROMOTE_KEY" ] || [ -f "$PROMOTE_PUB" ]; then
  remove_promote_key
fi

# 3. The remotes and the town registry.
repoint_remotes "$GITHUB_URL"
current_git_url=$(town_git_url) || die "$TOWN_JSON has no registry.rigs.$RIG.git_url"
if [ "$current_git_url" = "$GITHUB_URL" ]; then
  log "$TOWN_JSON: registry.rigs.$RIG.git_url is already $GITHUB_URL"
else
  backup_file "$TOWN_JSON"
  if [ "$DRY_RUN" = 1 ]; then
    log "would set registry.rigs.$RIG.git_url to $GITHUB_URL in $TOWN_JSON"
  else
    set_town_git_url "$GITHUB_URL"
    log "set registry.rigs.$RIG.git_url to $GITHUB_URL in $TOWN_JSON"
  fi
fi

# 4. The daemon, only when no landing is in flight. A rollback runs while
# landings are queued, so an unreadable queue and a queued landing are both
# handled the same way: report the command, do not force the restart.
count=$(landing_count) || count=""
if [ -z "$count" ]; then
  log "could not read $RIG's landing queue; NOT restarting the daemon. When it is empty run:"
  note ""
  note "  gt daemon restart"
  note ""
elif [ "$count" != 0 ]; then
  log "$RIG has $count open $READY_LABEL bead(s); NOT restarting the daemon. When the queue is empty run:"
  note ""
  note "  gt daemon restart"
  note ""
else
  run env "GT_TOWN_ROOT=$TOWN_ROOT" "GT_ROOT=$TOWN_ROOT" "$GT_BIN" daemon restart
fi

log "rollback of $RIG complete; a landing through the old path proves it"
