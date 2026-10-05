#!/usr/bin/env bash
# forgejo-cutover.sh — cut a rig over to Forgejo-primary landing (gt-fn9e6.12).
#
# Run it once per rig, from the gastown repo, after the rig's gate workflow is
# on its main and the runner image can run it. In order it:
#
#   1. refuses when a landing is in flight (an open gt:ready-to-land bead in the
#      rig) or when the probe is not green;
#   2. imports every GitHub ref into the rig's Forgejo copy and compares the two
#      ref lists, so the cut-over copy already holds every GitHub ref the
#      promotion fast-forwards from;
#   3. runs forgejo-provision.sh for the repository (bots, access, protection);
#   4. connects the rig to GitHub. By default it promotes: it mints an ed25519
#      keypair (private half at promote-<rig>.key in the config dir, mode 600,
#      never printed) and prints the exact gh command that adds the PUBLIC half
#      to GitHub with write access, so a green main verdict fast-forwards GitHub
#      main through internal/promote instead of a mirror that pushes every
#      commit before the slow tiers (gt-fn9e6.37, gt-fn9e6.40). With --mirror it
#      creates the push mirror (no branch filter) and prints the gh command for
#      the deploy key Forgejo minted — a mirror gets a NEW keypair and Forgejo
#      has no API to update one, so the operator adds it;
#   5. repoints origin in the rig's bare repository, mayor/rig and every crew
#      clone, and git_url in mayor/town.json;
#   6. writes the rig's merge_queue.forgejo block into settings/config.json:
#      promote_target and promote_key_file by default, mirror_target with
#      --mirror;
#   7. restarts the daemon (gt daemon restart) only when no landing is in flight;
#   8. prints the operator steps that remain (add the deploy key, disable GitHub
#      Actions on the GitHub repo, stop any self-hosted GitHub runner).
#
# The probe leaves no record — forgejo-probe.sh deletes its branch on every path
# and writes nothing outside its own temp dir — so a green probe is not a stored
# fact this script can read: step 1 makes one by running the probe. The mirror's
# deploy key (step 4, --mirror) is a fact Forgejo returns, so that one is printed
# rather than run.
#
# Idempotent: every step reads before it writes. A ref import over converged
# refs sends no update, an existing promote key is reused rather than
# regenerated, a mirror already pointing at the target is left alone, a remote
# already at the URL is reported and skipped, and a settings file whose block
# already matches is not rewritten (so it gains no second backup).
# --dry-run reads and prints and writes nothing: the probe is printed rather
# than run, because a probe pushes a branch to Forgejo, and ssh-keygen is
# printed rather than run.
#
# The script's own git work — the probe's push, the Forgejo ref listing and the
# import push — rides the admin base URL instead of --forgejo-url: WEB_URL
# /OWNER/NAME.git, the web root where the operator's admin credentials apply.
# --forgejo-url is the hostname form the rig's credential helper resolves for
# the bots, and the bots have no access to the repository until provisioning has
# run, so reading or pushing through it this early fails with "Repository not
# found" (gt-fn9e6.35).
#
# Every edit of a config file copies it to a dated .bak- file first. The admin
# token travels only in a mode-600 curl config file and in the environment of
# the provisioning run, never in argv or on stdout.
#
# Live runbook: docs/forgejo-runbook.md. Design of record:
# docs/design/forgejo-primary-landing.md (section 3) and the epic's cutover
# operations design (shadow mode removed, so the pre-cutover probe is the
# gate).
#
# Usage: forgejo-cutover.sh <rig> --repo OWNER/NAME [options]
#   --town-root DIR       town root holding <rig> (default $GT_TOWN_ROOT, else
#                         $GT_ROOT, else $HOME/gt)
#   --repo OWNER/NAME     the rig's Forgejo repository (required)
#   --github-url URL      the GitHub remote imported into and connected to
#                         (default: the rig bare repository's origin URL)
#   --forgejo-url URL     the Forgejo git URL written to the rig's remotes,
#                         town.json and the rig block — the credential-helper
#                         hostname form (default WEB_URL/OWNER/NAME.git). The
#                         script's own git work never rides it (see below)
#   --mirror              create the read-only push mirror instead of the
#                         default promote connection (gt-fn9e6.40)
#   --mirror-target URL   the push mirror's target, with --mirror (default: the
#                         ssh form of --github-url)
#   --main-branch NAME    protected landing target (default: main)
#   --gate-workflow NAME  gate workflow file basename (default: gate)
#   --bot-prefix P        bot login prefix, one bot per role (default: bot-)
#   --api-url URL         API root (default $FORGEJO_API_URL, else
#                         http://127.0.0.1:3000/api/v1)
#   --web-url URL         web root, the base of the git URL (default
#                         $FORGEJO_URL, else --api-url less /api/v1)
#   --admin-token-file F  file holding FORGEJO_ADMIN_TOKEN=... (default
#                         $FORGEJO_ADMIN_ENV, else $HOME/forgejo/.env)
#   --probe PATH          probe run before the cutover (default: the
#                         forgejo-probe.sh beside this script)
#   --provision PATH      provisioner run for the repo (default: the
#                         forgejo-provision.sh beside this script)
#   --gt PATH             gt binary for the daemon restart (default $GT_BIN,
#                         else gt)
#   --dry-run             read and print, never write
#   -h, --help            this text
#
# The admin token resolves from $FORGEJO_ADMIN_TOKEN, else from
# FORGEJO_ADMIN_TOKEN= in --admin-token-file. It must belong to a site
# administrator: the mirror and provisioning calls are admin-scoped.

set -euo pipefail

PROG=forgejo-cutover.sh
SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# The label the landing worker drains: an open bead wearing it is a landing
# queued or in flight (internal/land LabelReadyToLand).
READY_LABEL="gt:ready-to-land"

# The bot roles the rig's block names, exactly as config.ForgejoConfig reads
# them; the login is the prefix plus the role.
BOT_ROLES=(polecat landing registry)

die() { echo "$PROG: $*" >&2; exit 1; }
usage_die() { echo "$PROG: $*" >&2; echo "usage: $PROG --help" >&2; exit 2; }
log() { echo "$PROG: $*" >&2; }
# The operator's action items go to stdout: the gh command for the promote or
# mirror deploy key and the reminders that no script can carry out. Everything
# else is progress and belongs on stderr.
note() { echo "$*"; }

usage() { sed -n '/^# Usage:/,/^$/p' "$0" | sed 's/^# \{0,1\}//'; }

# --- arguments ---------------------------------------------------------------

RIG=""
REPO=""
TOWN_ROOT="${GT_TOWN_ROOT:-${GT_ROOT:-$HOME/gt}}"
# The config dir the promote key lives in, the same dir the Forgejo token files
# use (internal/forgejo/token.go): $XDG_CONFIG_HOME/gt, else ~/.config/gt.
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/gt"
API_URL="${FORGEJO_API_URL:-http://127.0.0.1:3000/api/v1}"
WEB_URL="${FORGEJO_URL:-}"
ADMIN_TOKEN_FILE="${FORGEJO_ADMIN_ENV:-$HOME/forgejo/.env}"
MAIN_BRANCH="main"
GATE_WORKFLOW="gate"
BOT_PREFIX="bot-"
GITHUB_URL=""
FORGEJO_URL_OPT=""
# Promote is the default connection to GitHub (gt-fn9e6.40); --mirror restores
# the push mirror a rig that still uses it needs.
MODE="promote"
MIRROR_TARGET=""
PROBE="$SELF_DIR/forgejo-probe.sh"
PROVISION="$SELF_DIR/forgejo-provision.sh"
GT_BIN="${GT_BIN:-gt}"
DRY_RUN=0

while [ $# -gt 0 ]; do
  case "$1" in
    --town-root) [ $# -ge 2 ] || usage_die "--town-root needs a value"; TOWN_ROOT=$2; shift 2 ;;
    --repo) [ $# -ge 2 ] || usage_die "--repo needs a value"; REPO=$2; shift 2 ;;
    --github-url) [ $# -ge 2 ] || usage_die "--github-url needs a value"; GITHUB_URL=$2; shift 2 ;;
    --forgejo-url) [ $# -ge 2 ] || usage_die "--forgejo-url needs a value"; FORGEJO_URL_OPT=$2; shift 2 ;;
    --mirror-target) [ $# -ge 2 ] || usage_die "--mirror-target needs a value"; MIRROR_TARGET=$2; shift 2 ;;
    --mirror) MODE=mirror; shift ;;
    --main-branch) [ $# -ge 2 ] || usage_die "--main-branch needs a value"; MAIN_BRANCH=$2; shift 2 ;;
    --gate-workflow) [ $# -ge 2 ] || usage_die "--gate-workflow needs a value"; GATE_WORKFLOW=$2; shift 2 ;;
    --bot-prefix) [ $# -ge 2 ] || usage_die "--bot-prefix needs a value"; BOT_PREFIX=$2; shift 2 ;;
    --api-url) [ $# -ge 2 ] || usage_die "--api-url needs a value"; API_URL=$2; shift 2 ;;
    --web-url) [ $# -ge 2 ] || usage_die "--web-url needs a value"; WEB_URL=$2; shift 2 ;;
    --admin-token-file) [ $# -ge 2 ] || usage_die "--admin-token-file needs a value"; ADMIN_TOKEN_FILE=$2; shift 2 ;;
    --probe) [ $# -ge 2 ] || usage_die "--probe needs a value"; PROBE=$2; shift 2 ;;
    --provision) [ $# -ge 2 ] || usage_die "--provision needs a value"; PROVISION=$2; shift 2 ;;
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

# A name that lands in a path under the town root: letters, digits and the
# separators a rig name may hold.
require_plain() { # require_plain LABEL VALUE
  case "$2" in
    ''|*[!A-Za-z0-9._-]*) usage_die "$1 '$2' must match [A-Za-z0-9._-]+" ;;
  esac
}
# A value that lands in a JSON string only: no newline, quote or backslash, which
# would need escaping this script does not do.
require_json_safe() { # require_json_safe LABEL VALUE
  case "$2" in
    ''|*$'\n'*|*'"'*|*\\*) usage_die "$1 '$2' may not be empty or hold a newline, quote or backslash" ;;
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
case "$MAIN_BRANCH" in
  ''|*[!A-Za-z0-9._/-]*) usage_die "--main-branch '$MAIN_BRANCH' must match [A-Za-z0-9._/-]+" ;;
esac
require_plain "--gate-workflow" "$GATE_WORKFLOW"
require_plain "--bot-prefix" "$BOT_PREFIX"
case "$API_URL" in
  http://*|https://*) ;;
  *) usage_die "--api-url must be http:// or https:// (got '$API_URL')" ;;
esac
API_URL="${API_URL%/}"
[ -n "$WEB_URL" ] || WEB_URL="${API_URL%/api/v1}"
[ -d "$TOWN_ROOT" ] || usage_die "--town-root '$TOWN_ROOT' is not a directory"
[ -f "$PROBE" ] || usage_die "--probe '$PROBE' is not a file"
[ -f "$PROVISION" ] || usage_die "--provision '$PROVISION' is not a file"

OWNER=${REPO%%/*}
NAME=${REPO#*/}

command -v curl >/dev/null 2>&1 || die "curl is not on PATH"
command -v git >/dev/null 2>&1 || die "git is not on PATH"
command -v bd >/dev/null 2>&1 || die "bd is not on PATH: it is how the script reads the rig's landing queue"
command -v python3 >/dev/null 2>&1 || die "python3 is not on PATH: it is how the script edits the rig settings and town.json"
if [ "$MODE" = promote ]; then
  command -v ssh-keygen >/dev/null 2>&1 || die "ssh-keygen is not on PATH: it is how the promote key is minted"
fi

# --- the rig's files ---------------------------------------------------------

RIG_ROOT="$TOWN_ROOT/$RIG"
[ -d "$RIG_ROOT" ] || die "no rig directory $RIG_ROOT (is '$RIG' a registered rig?)"
BARE="$RIG_ROOT/.repo.git"
[ -d "$BARE" ] || die "no bare repository $BARE"
SETTINGS="$RIG_ROOT/settings/config.json"
TOWN_JSON="$TOWN_ROOT/mayor/town.json"
[ -f "$TOWN_JSON" ] || die "no $TOWN_JSON"

[ -n "$FORGEJO_URL_OPT" ] || FORGEJO_URL_OPT="${WEB_URL%/}/$REPO.git"
FORGEJO_URL="$FORGEJO_URL_OPT"
require_json_safe "--forgejo-url" "$FORGEJO_URL"

# The admin base the script's OWN git work rides: the probe's push, the Forgejo
# ref listing and the import push. It is the web root, where the operator's
# admin credentials apply, and never --forgejo-url, which is only what gets
# written to the rig — that URL is the credential-helper hostname form, and the
# bots cannot reach the repository through it until provisioning has run
# (gt-fn9e6.35).
ADMIN_GIT_URL="${WEB_URL%/}/$REPO.git"

# block_value KEY prints KEY from the rig's existing merge_queue.forgejo block,
# or nothing. After a cutover that block's mirror_target is the only record of
# the GitHub URL, because the rig's origin points at Forgejo by then; before one
# there is no block and the origin is read instead.
block_value() { # block_value KEY
  python3 - "$SETTINGS" "$1" <<'PY'
import json, os, sys
path, key = sys.argv[1], sys.argv[2]
if not os.path.exists(path):
    sys.exit(0)
with open(path, encoding="utf-8") as fh:
    block = json.load(fh).get("merge_queue", {}).get("forgejo") or {}
print(block.get(key, ""))
PY
}

# The GitHub remote the rig is cut over from. A block written by an earlier
# cutover is the record of it: mirror_target from a mirror cutover, or
# promote_target from a promotion one (gt-fn9e6.40).
if [ -z "$GITHUB_URL" ]; then
  GITHUB_URL=$(block_value mirror_target)
fi
if [ -z "$GITHUB_URL" ]; then
  GITHUB_URL=$(block_value promote_target)
fi
if [ -z "$GITHUB_URL" ]; then
  GITHUB_URL=$(git -C "$BARE" remote get-url origin 2>/dev/null || true)
fi
[ -n "$GITHUB_URL" ] || die "no --github-url, and $BARE has no origin URL to read one from"
require_json_safe "--github-url" "$GITHUB_URL"

# ssh_form URL prints the ssh form of an http(s) git URL, and an ssh URL
# unchanged: the push mirror authenticates with a deploy key, which needs the
# SSH remote, while the import reads the URL the rig already carries.
ssh_form() { # ssh_form URL
  local url=$1 rest host path
  case "$url" in
    git@*:*) printf '%s' "$url" ;;
    ssh://*) rest=${url#ssh://}; rest=${rest#*@}; host=${rest%%/*}; path=${rest#*/}; printf 'git@%s:%s' "$host" "$path" ;;
    *://*) rest=${url#*://}; host=${rest%%/*}; path=${rest#*/}; printf 'git@%s:%s' "$host" "$path" ;;
    *) printf '%s' "$url" ;;
  esac
}
# slug OWNER/NAME prints the OWNER/NAME a git URL points at, so a one-line gh
# command can name the GitHub repository.
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

[ -n "$MIRROR_TARGET" ] || MIRROR_TARGET="$(ssh_form "$GITHUB_URL")"
require_json_safe "--mirror-target" "$MIRROR_TARGET"

# The promotion target is the ssh form too: internal/promote pushes with
# `ssh -i <key>`, so the URL has to be one ssh reads (gt-fn9e6.40).
PROMOTE_TARGET="$(ssh_form "$GITHUB_URL")"
require_json_safe "--promote-target" "$PROMOTE_TARGET"
PROMOTE_KEY="$CONFIG_DIR/promote-$RIG.key"
PROMOTE_PUB="$PROMOTE_KEY.pub"
require_json_safe "the promote key path" "$PROMOTE_KEY"

GITHUB_SLUG="$(slug "$GITHUB_URL")"
case "$GITHUB_SLUG" in
  */*) ;;
  *) usage_die "--github-url '$GITHUB_URL' does not name OWNER/NAME" ;;
esac

# --- work dir, token and trap ------------------------------------------------

WORK="$(mktemp -d "${TMPDIR:-/tmp}/forgejo-cutover.XXXXXX")"
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

# api METHOD PATH [BODY]: sends one request; HTTP_STATUS is the status code and
# the response body lands in RESP_FILE. A dry run sends the read methods only.
api() {
  local method=$1 path=$2 body=${3:-}
  if [ "$DRY_RUN" = 1 ] && [ "$method" != GET ]; then
    log "+ $method $API_URL$path (dry run: not sent)"
    HTTP_STATUS=000
    : > "$RESP_FILE"
    return 0
  fi
  local args=(-sS -o "$RESP_FILE" -w '%{http_code}' -X "$method" -K "$CURL_CONFIG"
    -H 'Accept: application/json')
  if [ -n "$body" ]; then
    args+=(-H 'Content-Type: application/json' --data "$body")
  fi
  args+=("$API_URL$path")
  HTTP_STATUS=$(curl "${args[@]}") || die "$method $API_URL$path: curl failed"
}

body_tail() {
  if [ -s "$RESP_FILE" ]; then
    printf ' — %s' "$(head -c 300 "$RESP_FILE" | tr '\n' ' ')"
  fi
}

# json_object FILE KEY VALUE prints the first object of a compact JSON array
# that holds "KEY":"VALUE". The objects carry no nested object, so splitting on
# "},{" separates them (as scripts/forgejo-resync.sh does).
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

# backup_file FILE copies FILE to a dated .bak- sibling and prints the copy. A
# dry run prints the copy it would make and writes nothing: no .bak- file is
# left behind (gt-fn9e6.35).
backup_file() {
  local f=$1 b
  if [ ! -f "$f" ]; then
    log "no $f to back up (it does not exist yet)"
    return 0
  fi
  b="$f.bak-$(date +%Y%m%d-%H%M%S)"
  run cp -p "$f" "$b"
  if [ "$DRY_RUN" = 1 ]; then
    log "would back up $f to $b"
  else
    log "backed up $f to $b"
  fi
}

# --- the rig's landing queue -------------------------------------------------

# landing_count prints the rig's open ready-to-land beads, the queue the landing
# worker drains, or returns 1 when the read fails. Refusing the cutover on a bad
# read (below) is what keeps "I could not tell" from reading as "nothing is in
# flight"; step 7 reads it again, where a failure means only that the restart is
# left to the operator.
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

# refuse_if_landing_in_flight is step 1's first half.
refuse_if_landing_in_flight() {
  local n
  if ! n=$(landing_count); then
    die "could not read the landing queue of $RIG: bd -C $RIG_ROOT count --status open --label $READY_LABEL failed"
  fi
  if [ "$n" != 0 ]; then
    die "$RIG has $n open $READY_LABEL bead(s): land them (or wait) before cutting the rig over"
  fi
  log "$RIG's landing queue is empty"
}

# probe_green is step 1's second half. The probe leaves no record, so this makes
# one: the probe script pushes the rig's main as land/probe-<rig>, waits for the
# real gate, and deletes the branch on every path. Its push rides the admin base
# URL, not the written one: it happens before provisioning, when the bots have
# no access yet. A dry run prints the run it would make instead.
probe_green() {
  local -a args=("$RIG" --repo "$REPO" --main-branch "$MAIN_BRANCH"
    --api-url "$API_URL" --web-url "$WEB_URL"
    --admin-token-file "$ADMIN_TOKEN_FILE"
    --github-url "$GITHUB_URL" --git-url "$ADMIN_GIT_URL")
  if [ "$DRY_RUN" = 1 ]; then
    run bash "$PROBE" "${args[@]}"
    log "(dry run: the probe was not run)"
    return 0
  fi
  log "probing $RIG through its real runner before the cutover"
  if ! bash "$PROBE" "${args[@]}"; then
    die "the probe of $RIG is not green; fix the runner or the gate workflow and re-run"
  fi
}

# --- refs --------------------------------------------------------------------

# ls_remote URL prints one sorted "ref sha" line per branch and tag, so a plain
# string compare of two calls is the ref-list comparison.
ls_remote() {
  log "+ git ls-remote --heads --tags --refs $1"
  git ls-remote --heads --tags --refs "$1" | awk '{print $2" "$1}' | LC_ALL=C sort
}

normalize() { if [ -n "$1" ]; then printf '%s\n' "$1"; fi; }

# report_plan prints the refs GitHub holds and Forgejo does not: those the
# import below creates. A ref Forgejo holds and GitHub does not is left alone
# (the import does not prune), because it may be a candidate branch a landing
# still references.
report_plan() {
  local ref
  while IFS=' ' read -r ref _; do
    if [ -n "$ref" ]; then log "will import $ref"; fi
  done <<<"$(comm -23 <(normalize "$1") <(normalize "$2") || true)"
  return 0
}

# import_refs is step 2. It fetches every GitHub branch and tag into a scratch
# bare repository and pushes them to Forgejo, then requires every GitHub ref to
# be present in Forgejo: a mirror has no branch filter and prunes, so a GitHub
# ref Forgejo lacks is deleted on GitHub at the next sync (--mirror), and the
# default promotion fast-forwards GitHub main from Forgejo's, so Forgejo must
# already hold GitHub's commits. Both Forgejo reads and the push ride the admin
# base URL, so the import never depends on bot access the not-yet-run
# provisioner would grant.
import_refs() {
  local scratch="$WORK/import.git" github_before forgejo_before github_after forgejo_after
  log "importing every GitHub ref through $ADMIN_GIT_URL"
  github_before=$(ls_remote "$GITHUB_URL")
  forgejo_before=$(ls_remote "$ADMIN_GIT_URL")
  report_plan "$github_before" "$forgejo_before"
  run git init --bare --quiet "$scratch"
  run git -C "$scratch" fetch --quiet "$GITHUB_URL" \
    '+refs/heads/*:refs/heads/*' '+refs/tags/*:refs/tags/*'
  run git -C "$scratch" push --quiet "$ADMIN_GIT_URL" \
    '+refs/heads/*:refs/heads/*' '+refs/tags/*:refs/tags/*'
  if [ "$DRY_RUN" = 1 ]; then
    return 0
  fi
  github_after=$(ls_remote "$GITHUB_URL")
  forgejo_after=$(ls_remote "$ADMIN_GIT_URL")
  if [ -n "$(comm -23 <(normalize "$github_after") <(normalize "$forgejo_after") || true)" ]; then
    log "ERROR: Forgejo is missing GitHub refs after the import; the mirror would prune them"
    comm -23 <(normalize "$github_after") <(normalize "$forgejo_after") >&2 || true
    exit 1
  fi
  log "imported every GitHub ref into $REPO ($(printf '%s\n' "$forgejo_after" | grep -c .) refs there)"
}

# --- the push mirror (--mirror) ----------------------------------------------

# mirror_public_key prints the public key Forgejo generated for the mirror, or
# nothing. It re-reads the mirror list, which carries public_key.
mirror_public_key() {
  api GET "/repos/$OWNER/$NAME/push_mirrors"
  if [ "$HTTP_STATUS" != 200 ]; then
    return 0
  fi
  json_field "$(json_object "$RESP_FILE" remote_address "$MIRROR_TARGET")" public_key
}

# gh_deploy_key_commands prints the operator's command for step 4: Forgejo mints
# a keypair per mirror and has no API to replace one, so the write deploy key is
# added on GitHub by hand.
gh_deploy_key_commands() {
  local key=$1
  note ""
  note "Add the mirror's write deploy key to GitHub (it gets a NEW keypair; there is no update API):"
  note ""
  note "  cat > ~/forgejo-mirror-$RIG.pub <<'KEY'"
  note "$key"
  note "KEY"
  note "  gh repo deploy-key add ~/forgejo-mirror-$RIG.pub --repo $GITHUB_SLUG --title forgejo-mirror-$RIG --allow-write"
}

# create_mirror is step 4: a push mirror with no branch filter (the mirror
# pushes every ref) and sync on commit. A second run finds the same
# remote_address and leaves it alone.
create_mirror() {
  local existing key mirror
  api GET "/repos/$OWNER/$NAME/push_mirrors"
  case "$HTTP_STATUS" in
    200) ;;
    404) die "GET /repos/$REPO/push_mirrors returned 404: the repository is not on $API_URL, or the admin token cannot see it$(body_tail)" ;;
    *) die "GET /repos/$REPO/push_mirrors: unexpected HTTP $HTTP_STATUS$(body_tail)" ;;
  esac
  existing=$(json_object "$RESP_FILE" remote_address "$MIRROR_TARGET")
  if [ -n "$existing" ]; then
    log "the push mirror to $MIRROR_TARGET already exists on $REPO"
    key=$(mirror_public_key)
  elif [ "$DRY_RUN" = 1 ]; then
    log "+ POST $API_URL/repos/$OWNER/$NAME/push_mirrors remote_address=$MIRROR_TARGET (no branch filter) (dry run: not sent)"
    note ""
    note "A real run creates the mirror here and prints the gh deploy-key command for the key Forgejo mints."
    return 0
  else
    mirror="{\"remote_address\":\"$MIRROR_TARGET\",\"use_ssh\":true,\"branch_filter\":\"\",\"sync_on_commit\":true,\"interval\":\"10m\"}"
    api POST "/repos/$OWNER/$NAME/push_mirrors" "$mirror"
    case "$HTTP_STATUS" in
      200|201) ;;
      *) die "POST /repos/$REPO/push_mirrors: unexpected HTTP $HTTP_STATUS$(body_tail)" ;;
    esac
    key=$(json_field "$(tr -d '\n' < "$RESP_FILE")" public_key)
    log "created the push mirror to $MIRROR_TARGET on $REPO (no branch filter, sync on commit)"
    if [ -z "$key" ]; then
      key=$(mirror_public_key)
    fi
  fi
  if [ -n "$key" ]; then
    gh_deploy_key_commands "$key"
  else
    log "no public key came back from the mirror API; read it under $WEB_URL/$REPO settings → mirrors and add it as a write deploy key on $GITHUB_SLUG"
  fi
}

# --- the promote key (default mode) ------------------------------------------

# ensure_promote_key is step 4's default-mode half: a rig with no promote key
# gets an ed25519 keypair, and a rig that has one keeps it (a second cutover
# must not rotate a key GitHub already trusts). The private half is created mode
# 600 in a directory created mode 700, and its contents are never printed; the
# public half lands beside it as <key>.pub. A dry run prints the commands.
ensure_promote_key() {
  local dir
  dir=$(dirname "$PROMOTE_KEY")
  if [ -f "$PROMOTE_KEY" ]; then
    log "reusing the promote key $PROMOTE_KEY"
    if [ -f "$PROMOTE_PUB" ]; then
      return 0
    fi
    # The public half is what the gh command adds. Re-derive it from the key
    # that is already there rather than mint a keypair GitHub does not know.
    if [ "$DRY_RUN" = 1 ]; then
      log "+ ssh-keygen -y -f $PROMOTE_KEY > $PROMOTE_PUB (dry run: not run)"
    else
      chmod 600 "$PROMOTE_KEY"
      local tmp="$PROMOTE_PUB.tmp.$$"
      if ssh-keygen -y -f "$PROMOTE_KEY" > "$tmp"; then
        chmod 644 "$tmp"
        mv -f "$tmp" "$PROMOTE_PUB"
        log "derived $PROMOTE_PUB from the existing promote key"
      else
        rm -f "$tmp"
        die "could not derive the public half of $PROMOTE_KEY; remove it and re-run to mint a new keypair"
      fi
    fi
    return 0
  fi
  if [ "$DRY_RUN" = 1 ]; then
    log "+ mkdir -p $dir (mode 700, when absent)"
    log "+ ssh-keygen -q -t ed25519 -N '' -C promote-$RIG -f $PROMOTE_KEY"
    log "(dry run: no promote key was created)"
    return 0
  fi
  if [ ! -d "$dir" ]; then
    mkdir -p "$dir"
    chmod 700 "$dir"
  fi
  ssh-keygen -q -t ed25519 -N "" -C "promote-$RIG" -f "$PROMOTE_KEY"
  chmod 600 "$PROMOTE_KEY"
  chmod 644 "$PROMOTE_PUB"
  log "created the promote key $PROMOTE_KEY (public half $PROMOTE_PUB)"
}

# gh_promote_key_commands prints the operator's command for step 4: the public
# half of the key this script minted is added to GitHub with write access, so
# the promotion can push the target's main. Only the public key is named.
gh_promote_key_commands() {
  note ""
  note "Add the promote key's PUBLIC half to GitHub as a write deploy key:"
  note ""
  note "  gh repo deploy-key add $PROMOTE_PUB --repo $GITHUB_SLUG --title promote-$RIG --allow-write"
}

# --- remotes and config ------------------------------------------------------

# repoint DIR URL sets DIR's origin to URL, or reports it already there. A
# directory that is not a git repository is reported and skipped: crew/ holds
# files beside the clones.
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

# repoint_remotes is step 5's git half: the bare repository the landing path
# pushes to, the mayor's clone, and every crew clone.
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

# town_git_url prints registry.rigs.<rig>.git_url from mayor/town.json.
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

# set_town_git_url URL writes registry.rigs.<rig>.git_url, keeping the key order
# and the file's 2-space indent.
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

# settings_forgejo_matches BLOCK_JSON prints "same" when the rig's
# merge_queue.forgejo already equals BLOCK_JSON, "changed" otherwise. The
# comparison is on the parsed block, so key order and formatting never force a
# rewrite of a file that already says the right thing.
settings_forgejo_matches() {
  python3 - "$SETTINGS" "$1" <<'PY'
import json, os, sys
path, want = sys.argv[1], json.loads(sys.argv[2])
have = {}
if os.path.exists(path):
    with open(path, encoding="utf-8") as fh:
        have = json.load(fh).get("merge_queue", {}).get("forgejo") or {}
print("same" if have == want else "changed")
PY
}

# set_settings_forgejo BLOCK_JSON writes merge_queue.forgejo = BLOCK_JSON,
# leaving the rest of the file alone.
set_settings_forgejo() {
  python3 - "$SETTINGS" "$1" <<'PY'
import json, os, sys
path, block = sys.argv[1], json.loads(sys.argv[2])
data = {}
if os.path.exists(path):
    with open(path, encoding="utf-8") as fh:
        data = json.load(fh)
data.setdefault("type", "rig-settings")
data.setdefault("version", 1)
data.setdefault("merge_queue", {})["forgejo"] = block
with open(path, "w", encoding="utf-8") as fh:
    json.dump(data, fh, indent=2)
    fh.write("\n")
PY
}

# --- run ---------------------------------------------------------------------

resolve_admin_token
if [ "$MODE" = mirror ]; then
  log "cutting $RIG over to $FORGEJO_URL (GitHub $GITHUB_URL, push mirror $MIRROR_TARGET)"
else
  log "cutting $RIG over to $FORGEJO_URL (GitHub $GITHUB_URL, promotion to $PROMOTE_TARGET)"
fi

# 1. No landing in flight, then a green probe.
refuse_if_landing_in_flight
probe_green

# 2. Every GitHub ref into Forgejo, then a comparison.
import_refs

# 3. Bots, access and protection for the repo.
run bash "$PROVISION" --repo "$REPO" --main-branch "$MAIN_BRANCH" \
  --api-url "$API_URL" --admin-token-file "$ADMIN_TOKEN_FILE"

# 4. The connection to GitHub: promotion by default, the push mirror with
# --mirror. Either way the deploy key GitHub needs is printed, not added.
if [ "$MODE" = mirror ]; then
  create_mirror
else
  ensure_promote_key
  gh_promote_key_commands
fi

# 5. Repoint the rig's remotes and the town registry.
repoint_remotes "$FORGEJO_URL"
current_git_url=$(town_git_url) || die "$TOWN_JSON has no registry.rigs.$RIG.git_url"
if [ "$current_git_url" = "$FORGEJO_URL" ]; then
  log "$TOWN_JSON: registry.rigs.$RIG.git_url is already $FORGEJO_URL"
else
  backup_file "$TOWN_JSON"
  if [ "$DRY_RUN" = 1 ]; then
    log "would set registry.rigs.$RIG.git_url to $FORGEJO_URL in $TOWN_JSON"
  else
    set_town_git_url "$FORGEJO_URL"
    log "set registry.rigs.$RIG.git_url to $FORGEJO_URL in $TOWN_JSON"
  fi
fi

# 6. The rig's merge_queue.forgejo block.
block="{\"remote_url\":\"$FORGEJO_URL\",\"gate_workflow\":\"$GATE_WORKFLOW\",\"bots\":{"
first=1
for role in "${BOT_ROLES[@]}"; do
  [ "$first" = 1 ] || block="$block,"
  first=0
  block="$block\"$role\":\"$BOT_PREFIX$role\""
done
if [ "$MODE" = mirror ]; then
  block="$block},\"mirror_target\":\"$MIRROR_TARGET\"}"
else
  block="$block},\"promote_target\":\"$PROMOTE_TARGET\",\"promote_key_file\":\"$PROMOTE_KEY\"}"
fi
if [ "$(settings_forgejo_matches "$block")" = same ]; then
  log "$SETTINGS: merge_queue.forgejo already matches"
else
  backup_file "$SETTINGS"
  if [ "$DRY_RUN" = 1 ]; then
    log "would write merge_queue.forgejo to $SETTINGS"
  else
    set_settings_forgejo "$block"
    log "wrote merge_queue.forgejo to $SETTINGS"
  fi
fi

# 7. The daemon reads a rig's block at start, so it restarts — but never over a
# landing in flight. The rig is cut over by now, so a queue this last read
# cannot read costs the restart, not the cutover.
count=$(landing_count) || count=""
if [ -z "$count" ]; then
  log "could not read $RIG's landing queue; NOT restarting the daemon. When the queue is empty run:"
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

# 8. What no script can do for the operator.
note ""
note "Finish the cutover by hand:"
if [ "$MODE" = mirror ]; then
  note "  - confirm the mirror's first sync and the rig's next landing, then delete the rig's GitHub-only workflows if any remain"
else
  note "  - add the promote deploy key above, then confirm the rig's next green main verdict advances GitHub main"
fi
note "  - disable GitHub Actions on $GITHUB_SLUG:"
note "      gh api --method PUT repos/$GITHUB_SLUG/actions/permissions -F enabled=false"
note "  - stop (not remove) any self-hosted GitHub runner for $GITHUB_SLUG"
note ""

log "cutover of $RIG complete"
