#!/usr/bin/env bash
# forgejo-provision.sh — provision the Forgejo bots, their tokens and the branch
# protection the landing path needs (gt-fn9e6.4).
#
# Live runbook: docs/forgejo-runbook.md. Design of record:
# docs/design/forgejo-primary-landing.md.
#
# Each role's bot is minted only the token scopes its work needs, and with
# --repo granted only the repository access it needs (gt-fn9e6.15).
#
# Idempotent: a second run against unchanged state makes no write call, and a
# run is safe to repeat after a partial failure. The admin token never reaches
# argv — curl reads it from a mode-600 config file — and a minted bot token is
# written only to its role's env file, never to stdout.
#
# Usage: forgejo-provision.sh [options]
#   --api-url URL          API root (default $FORGEJO_API_URL,
#                          else http://127.0.0.1:3000/api/v1)
#   --web-url URL          web root written to the token files (default
#                          $FORGEJO_URL, else --api-url less /api/v1)
#   --admin-token-file F   file holding FORGEJO_ADMIN_TOKEN=... (default
#                          $FORGEJO_ADMIN_ENV, else $HOME/forgejo/.env)
#   --role NAME            bot role to provision; repeatable (default: polecat
#                          landing registry; known: polecat landing registry
#                          viewer)
#   --bot-prefix P         bot username prefix (default: bot-)
#   --bot-email-domain D   email domain for new bots (default: bots.invalid)
#   --config-dir DIR       where the role token files go (default
#                          $XDG_CONFIG_HOME/gt, else $HOME/.config/gt)
#   --token-name NAME      name of the minted token (default: gt-provision)
#   --repo OWNER/NAME      repository to protect; repeatable
#   --landing-bot LOGIN    login on the merge whitelist (default: bot-landing)
#   --gate-context CTX     required commit-status context; repeatable
#                          (default: "ci / gate (push)" and "om / review")
#   --main-branch NAME     protected landing target (default: main)
#   --land-branch PATTERN  candidate branch pattern (default: land/*)
#   --land-push LOGIN      extra login allowed to push the candidate branches;
#                          repeatable
#   --rotate               mint a new token even where a token file exists, and
#                          revoke the previous token of the same name
#   --dry-run              read but never write
#   -h, --help             this text
#
# The admin token resolves from $FORGEJO_ADMIN_TOKEN, else from
# FORGEJO_ADMIN_TOKEN= in --admin-token-file. It must belong to a site
# administrator: user creation and the admin token routes are admin-scoped.

set -euo pipefail

PROG=forgejo-provision.sh

die() { echo "$PROG: $*" >&2; exit 1; }
usage_die() { echo "$PROG: $*" >&2; echo "usage: $PROG --help" >&2; exit 2; }
log() { echo "$PROG: $*" >&2; }

usage() { sed -n '/^# Usage:/,/^$/p' "$0" | sed 's/^# \{0,1\}//'; }

# --- arguments ---------------------------------------------------------------

API_URL="${FORGEJO_API_URL:-http://127.0.0.1:3000/api/v1}"
WEB_URL="${FORGEJO_URL:-}"
ADMIN_TOKEN_FILE="${FORGEJO_ADMIN_ENV:-$HOME/forgejo/.env}"
BOT_PREFIX="bot-"
BOT_EMAIL_DOMAIN="bots.invalid"
TOKEN_NAME="gt-provision"
MAIN_BRANCH="main"
LAND_BRANCH="land/*"
LANDING_BOT=""
CONFIG_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/gt"
ROTATE=0
DRY_RUN=0
ROLES=()
REPOS=()
GATE_CONTEXTS=()
LAND_PUSH=()

while [ $# -gt 0 ]; do
  case "$1" in
    --api-url) [ $# -ge 2 ] || usage_die "--api-url needs a value"; API_URL=$2; shift 2 ;;
    --web-url) [ $# -ge 2 ] || usage_die "--web-url needs a value"; WEB_URL=$2; shift 2 ;;
    --admin-token-file) [ $# -ge 2 ] || usage_die "--admin-token-file needs a value"; ADMIN_TOKEN_FILE=$2; shift 2 ;;
    --role) [ $# -ge 2 ] || usage_die "--role needs a value"; ROLES+=("$2"); shift 2 ;;
    --bot-prefix) [ $# -ge 2 ] || usage_die "--bot-prefix needs a value"; BOT_PREFIX=$2; shift 2 ;;
    --bot-email-domain) [ $# -ge 2 ] || usage_die "--bot-email-domain needs a value"; BOT_EMAIL_DOMAIN=$2; shift 2 ;;
    --config-dir) [ $# -ge 2 ] || usage_die "--config-dir needs a value"; CONFIG_DIR=$2; shift 2 ;;
    --token-name) [ $# -ge 2 ] || usage_die "--token-name needs a value"; TOKEN_NAME=$2; shift 2 ;;
    --repo) [ $# -ge 2 ] || usage_die "--repo needs a value"; REPOS+=("$2"); shift 2 ;;
    --landing-bot) [ $# -ge 2 ] || usage_die "--landing-bot needs a value"; LANDING_BOT=$2; shift 2 ;;
    --gate-context) [ $# -ge 2 ] || usage_die "--gate-context needs a value"; GATE_CONTEXTS+=("$2"); shift 2 ;;
    --main-branch) [ $# -ge 2 ] || usage_die "--main-branch needs a value"; MAIN_BRANCH=$2; shift 2 ;;
    --land-branch) [ $# -ge 2 ] || usage_die "--land-branch needs a value"; LAND_BRANCH=$2; shift 2 ;;
    --land-push) [ $# -ge 2 ] || usage_die "--land-push needs a value"; LAND_PUSH+=("$2"); shift 2 ;;
    --rotate) ROTATE=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage_die "unknown option: $1" ;;
  esac
done

[ ${#ROLES[@]} -gt 0 ] || ROLES=(polecat landing registry)
[ ${#GATE_CONTEXTS[@]} -gt 0 ] || GATE_CONTEXTS=("ci / gate (push)" "om / review")
# The landing bot is the one login the protection rules name, so it is the
# landing role's bot unless the operator names another account.
[ -n "$LANDING_BOT" ] || LANDING_BOT="${BOT_PREFIX}landing"

# A name that lands in a URL path or a JSON string: letters, digits and the
# separators a Forgejo login or token name may hold.
require_plain() { # require_plain LABEL VALUE
  case "$2" in
    ''|*[!A-Za-z0-9._-]*) usage_die "$1 '$2' must match [A-Za-z0-9._-]+" ;;
  esac
}
# A name that lands in a JSON string only: no newline, quote or backslash, which
# would need escaping this script does not do.
require_json_safe() { # require_json_safe LABEL VALUE
  case "$2" in
    ''|*$'\n'*|*'"'*|*\\*) usage_die "$1 '$2' may not be empty or hold a newline, quote or backslash" ;;
  esac
}

# role_scopes ROLE prints the token scopes a role's bot needs, and returns 1 for
# a name no role uses (gt-fn9e6.15). A registry bot pushes packages with
# write:package and no repository scope at all, so it cannot touch a rig repo;
# the landing bot reads its own login; the viewer only reads.
role_scopes() {
  case "$1" in
    polecat)  printf '["write:repository"]' ;;
    landing)  printf '["write:repository","read:user"]' ;;
    registry) printf '["write:package","read:package"]' ;;
    viewer)   printf '["read:repository","read:user"]' ;;
    *) return 1 ;;
  esac
}

case "$API_URL" in
  http://*|https://*) ;;
  *) usage_die "--api-url must be http:// or https:// (got '$API_URL')" ;;
esac
API_URL="${API_URL%/}"
[ -n "$WEB_URL" ] || WEB_URL="${API_URL%/api/v1}"

command -v curl >/dev/null 2>&1 || die "curl is not on PATH"

for role in "${ROLES[@]}"; do
  require_plain "--role" "$role"
  role_scopes "$role" >/dev/null || usage_die "unknown role '$role' (known roles: polecat landing registry viewer)"
done
require_plain "--bot-prefix" "$BOT_PREFIX"
require_plain "--token-name" "$TOKEN_NAME"
require_plain "--landing-bot" "$LANDING_BOT"
require_json_safe "--bot-email-domain" "$BOT_EMAIL_DOMAIN"
require_json_safe "--main-branch" "$MAIN_BRANCH"
require_json_safe "--land-branch" "$LAND_BRANCH"
for ctx in "${GATE_CONTEXTS[@]}"; do require_json_safe "--gate-context" "$ctx"; done
if [ ${#LAND_PUSH[@]} -gt 0 ]; then
  for login in "${LAND_PUSH[@]}"; do require_plain "--land-push" "$login"; done
fi
if [ ${#REPOS[@]} -gt 0 ]; then
  for repo in "${REPOS[@]}"; do
    case "$repo" in
      */*/*|/*|*/|'') usage_die "--repo '$repo' must be OWNER/NAME" ;;
      */*) ;;
      *) usage_die "--repo '$repo' must be OWNER/NAME" ;;
    esac
  done
fi

# --- HTTP --------------------------------------------------------------------

WORK="$(mktemp -d "${TMPDIR:-/tmp}/forgejo-provision.XXXXXX")"
RESP_FILE="$WORK/response.json"
BODY_FILE="$WORK/request.json"
RULE_FILE="$WORK/rule.json"
CURL_CONFIG="$WORK/curl.conf"
TOKEN_TMP=""
cleanup() {
  rm -rf "$WORK"
  if [ -n "$TOKEN_TMP" ]; then rm -f "$TOKEN_TMP"; fi
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

# api METHOD PATH [BODY]: sends one request; HTTP_STATUS is the status code and
# the response body lands in RESP_FILE. A dry run performs the read methods only
# and reports the writes it would have sent.
api() {
  local method=$1 path=$2 body=${3:-}
  if [ "$DRY_RUN" = 1 ] && [ "$method" != GET ]; then
    HTTP_STATUS=000
    : > "$RESP_FILE"
    return 0
  fi
  local args=(-sS -o "$RESP_FILE" -w '%{http_code}' -X "$method" -K "$CURL_CONFIG"
    -H 'Accept: application/json' "$API_URL$path")
  if [ -n "$body" ]; then
    args+=(-H 'Content-Type: application/json' --data-binary "@$body")
  fi
  if ! HTTP_STATUS=$(curl "${args[@]}"); then
    die "$method $API_URL$path: curl failed"
  fi
}

# body_tail renders the response, truncated, for an error message.
body_tail() {
  if [ -s "$RESP_FILE" ]; then
    printf ' — %s' "$(head -c 300 "$RESP_FILE" | tr '\n' ' ')"
  fi
}

# run DESCRIPTION COMMAND... : echoes the command, or runs it when not dry.
run() {
  local desc=$1
  shift
  if [ "$DRY_RUN" = 1 ]; then
    echo "would $desc" >&2
    return 0
  fi
  "$@"
}

json_escape() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'; }

# json_list renders the arguments as a JSON array of strings.
json_list() {
  local out="" item
  for item in "$@"; do
    if [ -n "$out" ]; then out+=","; fi
    out+="\"$(json_escape "$item")\""
  done
  printf '[%s]' "$out"
}

# json_has FILE NEEDLE... : true when every needle appears in FILE. The Forgejo
# API serializes compactly, so a field's exact spelling is a substring of the
# response; that is what the write path verifies itself against.
json_has() {
  local file=$1 needle
  shift
  for needle in "$@"; do
    grep -qF -- "$needle" "$file" || return 1
  done
  return 0
}

# json_object FILE KEY VALUE prints the first top-level object of a compact JSON
# array that holds "KEY":"VALUE". The objects carry no nested object, so
# splitting on "},{" separates them.
json_object() {
  sed 's/},{/}\n{/g' "$1" | grep -F "\"$2\":\"$3\"" | head -1 || true
}

# --- users and tokens --------------------------------------------------------

random_password() {
  # head reads a fixed slice first, so nothing downstream exits early and the
  # pipeline cannot die of SIGPIPE under `set -o pipefail`. 4096 random bytes
  # hold far more than 32 alphanumerics.
  local password
  password=$(head -c 4096 /dev/urandom | LC_ALL=C tr -dc 'A-Za-z0-9')
  printf '%s' "${password:0:32}"
}

# ensure_user ROLE: creates the role's bot when absent. The password is random and
# discarded — the bots authenticate by token — and it travels in a request body
# file, not argv, so it reaches no process listing or log.
ensure_user() {
  local role=$1 user="${BOT_PREFIX}$1"
  api GET "/users/$user"
  case "$HTTP_STATUS" in
    200)
      log "user $user exists"
      return 0 ;;
    404) ;;
    *) die "GET /users/$user: unexpected HTTP $HTTP_STATUS$(body_tail)" ;;
  esac
  local password
  password="$(random_password)"
  printf '{"username":"%s","email":"%s","password":"%s","must_change_password":false,"send_notify":false}\n' \
    "$(json_escape "$user")" "$(json_escape "$user@$BOT_EMAIL_DOMAIN")" "$password" > "$BODY_FILE"
  unset password
  run "create user $user" api POST /admin/users "$BODY_FILE"
  if [ "$DRY_RUN" = 1 ]; then return 0; fi
  case "$HTTP_STATUS" in
    201) log "created user $user" ;;
    # A concurrent provisioner may have created it between the GET and the POST.
    422) log "user $user exists" ;;
    *) die "POST /admin/users for $user: unexpected HTTP $HTTP_STATUS$(body_tail)" ;;
  esac
}

# token_file_path ROLE prints where the role's token file lives.
token_file_path() { printf '%s/forgejo-%s.env' "$CONFIG_DIR" "$1"; }

# ensure_token ROLE: writes the role's env file, minting a token only when the
# file is absent (or --rotate). A token's value is returned once, at creation, so
# a lost file means rotation, never a recovery. An existing token keeps the
# scopes it was minted with until --rotate replaces it (gt-fn9e6.15).
ensure_token() {
  local role=$1 user="${BOT_PREFIX}$1" path
  path="$(token_file_path "$role")"
  if [ -f "$path" ]; then
    chmod 600 "$path"
    if [ "$ROTATE" = 0 ]; then
      log "token $path exists; leaving it (--rotate mints a new one)"
      return 0
    fi
    log "rotating the token in $path"
  fi
  if [ "$DRY_RUN" = 1 ]; then
    echo "would mint a '$TOKEN_NAME' token for $user and write $path" >&2
    return 0
  fi
  if [ "$ROTATE" = 1 ]; then
    revoke_old_token "$user"
  fi
  printf '{"name":"%s","scopes":%s}\n' "$(json_escape "$TOKEN_NAME")" "$(role_scopes "$role")" > "$BODY_FILE"
  # The admin route is the one a token may mint through: /users/{name}/tokens
  # wants HttpBasic auth by design, so a site-admin token can create another
  # user's token only here (forgejo#12323).
  api POST "/admin/users/$user/tokens" "$BODY_FILE"
  case "$HTTP_STATUS" in
    201) ;;
    403) die "the admin token cannot mint a token for $user; it needs site-administrator rights, and a token limit of 'all'.$(body_tail)" ;;
    *) die "POST /admin/users/$user/tokens: unexpected HTTP $HTTP_STATUS$(body_tail)" ;;
  esac
  local token
  # The new value comes back once. Forgejo serializes it as "sha1" (the struct's
  # long-standing field name); "token" is accepted too, for a version that
  # renames it.
  token=$(sed -n 's/.*"token":"\([^"]*\)".*/\1/p' "$RESP_FILE" | head -1)
  if [ -z "$token" ]; then
    token=$(sed -n 's/.*"sha1":"\([^"]*\)".*/\1/p' "$RESP_FILE" | head -1)
  fi
  case "$token" in
    ''|*[!A-Za-z0-9_-]*) die "POST /admin/users/$user/tokens returned no usable token" ;;
  esac
  write_token_file "$path" "$user" "$token"
  unset token
  log "minted a '$TOKEN_NAME' token for $user as $path (mode 600)"
}

# revoke_old_token USER removes a token of this run's name so a rotation does not
# leave the previous credential live. A miss is a warning: the file may hold a
# token the operator made by hand, and replacing the file is what matters.
revoke_old_token() {
  local user=$1
  api GET "/admin/users/$user/tokens"
  if [ "$HTTP_STATUS" != 200 ]; then
    log "warning: cannot list $user's tokens (HTTP $HTTP_STATUS)$(body_tail); revoke the old '$TOKEN_NAME' token by hand"
    return 0
  fi
  local existing
  existing=$(json_object "$RESP_FILE" name "$TOKEN_NAME")
  if [ -z "$existing" ]; then
    log "no token named '$TOKEN_NAME' to revoke; if the file held a token minted under another name, revoke it in the web UI"
    return 0
  fi
  api DELETE "/admin/users/$user/tokens/$TOKEN_NAME"
  if [ "$HTTP_STATUS" = 204 ]; then
    log "revoked the previous '$TOKEN_NAME' token"
  else
    log "warning: could not revoke the previous '$TOKEN_NAME' token (HTTP $HTTP_STATUS)$(body_tail); revoke it in the web UI"
  fi
}

# write_token_file PATH USER TOKEN writes the role env file atomically at mode
# 600: a reader sees either the old file or the complete new one.
write_token_file() {
  local path=$1 user=$2 token=$3 dir
  dir=$(dirname "$path")
  mkdir -p "$dir"
  TOKEN_TMP=$(mktemp "$dir/.forgejo-token.XXXXXX")
  chmod 600 "$TOKEN_TMP"
  {
    printf 'FORGEJO_URL=%s\n' "$WEB_URL"
    printf 'FORGEJO_USER=%s\n' "$user"
    printf 'FORGEJO_TOKEN=%s\n' "$token"
  } > "$TOKEN_TMP"
  mv -f "$TOKEN_TMP" "$path"
  TOKEN_TMP=""
  chmod 600 "$path"
}

# --- collaborators -----------------------------------------------------------

# collaborator_permission ROLE prints the repository access a role's bot gets, or
# nothing when the role needs no repository access at all (gt-fn9e6.15).
collaborator_permission() {
  case "$1" in
    polecat|landing) printf 'write' ;;
    viewer) printf 'read' ;;
    registry) : ;;
    *) die "no repository access is defined for role '$1'" ;;
  esac
}

# ensure_collaborator ROLE OWNER/NAME PERMISSION grants the role's bot its access
# to the repository. The read decides first, so a second run sends no write.
ensure_collaborator() {
  local role=$1 repo=$2 permission=$3
  local owner=${repo%%/*} name=${repo#*/} user="${BOT_PREFIX}$role"
  api GET "/repos/$owner/$name/collaborators/$user"
  case "$HTTP_STATUS" in
    204)
      log "$user already has $permission access to $repo"
      return 0 ;;
    404) ;;
    *) die "GET /repos/$repo/collaborators/$user: unexpected HTTP $HTTP_STATUS$(body_tail)" ;;
  esac
  printf '{"permission":"%s"}\n' "$(json_escape "$permission")" > "$BODY_FILE"
  run "grant $user $permission access to $repo" api PUT "/repos/$owner/$name/collaborators/$user" "$BODY_FILE"
  if [ "$DRY_RUN" = 1 ]; then return 0; fi
  case "$HTTP_STATUS" in
    204|200|201) log "granted $user $permission access to $repo" ;;
    *) die "PUT /repos/$repo/collaborators/$user: unexpected HTTP $HTTP_STATUS$(body_tail)" ;;
  esac
}

# --- branch protection -------------------------------------------------------

# main_checks prints the fragments the landing target must carry: no pusher at
# all, admins included; no deploy key may push; the landing bot alone may merge;
# the required contexts; and a stale PR is refused rather than merged.
main_checks() {
  printf '%s\n' \
    '"enable_push":false' \
    '"apply_to_admins":true' \
    '"push_whitelist_deploy_keys":false' \
    '"enable_merge_whitelist":true' \
    "\"merge_whitelist_usernames\":[\"$(json_escape "$LANDING_BOT")\"]" \
    '"enable_status_check":true' \
    '"block_on_outdated_branch":true'
  local ctx
  for ctx in "${GATE_CONTEXTS[@]}"; do
    printf '"%s"\n' "$(json_escape "$ctx")"
  done
}

# land_checks prints the fragments the candidate branches must carry: the landing
# bot and any --land-push login may push, no deploy key may (a candidate branch
# is written only through the landing path), and admins get no bypass.
land_checks() {
  printf '%s\n' '"enable_push":true' '"enable_push_whitelist":true' '"apply_to_admins":true' '"push_whitelist_deploy_keys":false'
  local login
  for login in "$LANDING_BOT" ${LAND_PUSH[@]+"${LAND_PUSH[@]}"}; do
    printf '"%s"\n' "$(json_escape "$login")"
  done
}

# ensure_rule OWNER/REPO RULE_NAME BODY CHECK... creates the rule when absent and
# patches it when present but drifted. The write's own response is verified, so a
# silently-ignored field fails the run instead of passing it.
ensure_rule() {
  local repo=$1 rule=$2 body=$3
  shift 3
  local owner=${repo%%/*} name=${repo#*/}
  # The rule name is a branch glob, and the per-rule routes take it in the path:
  # chi routes on the raw path, so "/" must arrive encoded as %2F for the
  # handler's PathUnescape to rebuild it (go-gitea#21093).
  local escaped=${rule//\//%2F}
  api GET "/repos/$owner/$name/branch_protections"
  case "$HTTP_STATUS" in
    200) ;;
    404) die "GET /repos/$repo/branch_protections returned 404: the repository is not on $API_URL, or the admin token cannot see it$(body_tail)" ;;
    *) die "GET /repos/$repo/branch_protections: unexpected HTTP $HTTP_STATUS$(body_tail)" ;;
  esac
  local existing
  existing=$(json_object "$RESP_FILE" rule_name "$rule")
  if [ -n "$existing" ]; then
    printf '%s' "$existing" > "$RULE_FILE"
    if json_has "$RULE_FILE" "$@"; then
      log "branch protection $rule on $repo is already set"
      return 0
    fi
    run "update branch protection $rule on $repo" api PATCH "/repos/$owner/$name/branch_protections/$escaped" "$body"
    if [ "$DRY_RUN" = 1 ]; then return 0; fi
    case "$HTTP_STATUS" in
      200) ;;
      404) die "PATCH .../branch_protections/$escaped returned 404: the rule exists but Forgejo did not resolve its name. Set '$rule' by hand in the web UI, then re-run" ;;
      *) die "PATCH .../branch_protections/$escaped: unexpected HTTP $HTTP_STATUS$(body_tail)" ;;
    esac
    log "updated branch protection $rule on $repo"
  else
    run "create branch protection $rule on $repo" api POST "/repos/$owner/$name/branch_protections" "$body"
    if [ "$DRY_RUN" = 1 ]; then return 0; fi
    case "$HTTP_STATUS" in
      201) log "created branch protection $rule on $repo" ;;
      *) die "POST .../branch_protections for $rule: unexpected HTTP $HTTP_STATUS$(body_tail)" ;;
    esac
  fi
  json_has "$RESP_FILE" "$@" || die "the write for $rule on $repo did not take: $(head -c 300 "$RESP_FILE" | tr '\n' ' ')"
}

# --- run ---------------------------------------------------------------------

for role in "${ROLES[@]}"; do
  ensure_user "$role"
  ensure_token "$role"
done

if [ ${#REPOS[@]} -gt 0 ]; then
  land_body="$WORK/land-rule.json"
  land_whitelist=("$LANDING_BOT" ${LAND_PUSH[@]+"${LAND_PUSH[@]}"})
  printf '{"rule_name":"%s","enable_push":true,"enable_push_whitelist":true,"push_whitelist_usernames":%s,"push_whitelist_deploy_keys":false,"apply_to_admins":true}\n' \
    "$(json_escape "$LAND_BRANCH")" "$(json_list "${land_whitelist[@]}")" > "$land_body"

  main_body="$WORK/main-rule.json"
  printf '{"rule_name":"%s","enable_push":false,"enable_push_whitelist":false,"push_whitelist_usernames":[],"push_whitelist_deploy_keys":false,"apply_to_admins":true,"enable_merge_whitelist":true,"merge_whitelist_usernames":%s,"enable_status_check":true,"status_check_contexts":%s,"block_on_outdated_branch":true}\n' \
    "$(json_escape "$MAIN_BRANCH")" "$(json_list "$LANDING_BOT")" "$(json_list "${GATE_CONTEXTS[@]}")" > "$main_body"

  main_checks > "$WORK/main-checks"
  main_check_list=()
  while IFS= read -r line; do main_check_list+=("$line"); done < "$WORK/main-checks"
  land_checks > "$WORK/land-checks"
  land_check_list=()
  while IFS= read -r line; do land_check_list+=("$line"); done < "$WORK/land-checks"

  for repo in "${REPOS[@]}"; do
    ensure_rule "$repo" "$MAIN_BRANCH" "$main_body" "${main_check_list[@]}"
    ensure_rule "$repo" "$LAND_BRANCH" "$land_body" "${land_check_list[@]}"
    for role in "${ROLES[@]}"; do
      permission="$(collaborator_permission "$role")"
      [ -n "$permission" ] || continue
      ensure_collaborator "$role" "$repo" "$permission"
    done
  done
fi

if [ "$DRY_RUN" = 1 ]; then
  log "dry run: no write was sent"
else
  log "provisioning complete"
fi
