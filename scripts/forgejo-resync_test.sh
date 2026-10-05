#!/usr/bin/env bash
# Tests for scripts/forgejo-resync.sh (gt-fn9e6.29). A stub curl and a stub git
# hold a small Forgejo and a small GitHub in a temp directory — branch
# protection, users, tokens and refs — so the lift, the ref import, the
# restore-on-failure trap and the --dry-run path run with no network and no live
# instance. The restore step runs the real scripts/forgejo-provision.sh against
# the same stub curl. HOME and XDG_CONFIG_HOME point inside the temp dir, so the
# token files the provisioner writes can never land in the operator's real
# ~/.config.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
RESYNC="$SCRIPT_DIR/forgejo-resync.sh"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/forgejo-resync-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT
PASS=0
FAIL=0
STUB_ADMIN_TOKEN="stub-admin-token"
STUB_TOKEN_VALUE="stubtoken-abcdefghijklmnop"
API="http://forgejo.test/api/v1"
GH_URL="https://github.com/acme/rig.git"
FJ_URL="http://forgejo.test/acme/rig.git"
STATE="$TMP/state"
CFG="$TMP/config"

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; FAIL=$((FAIL + 1)); if [ $# -gt 1 ]; then printf '%s\n' "$2" | sed 's/^/    | /'; fi; }
check() { # check DESCRIPTION COMMAND...
  local desc=$1
  shift
  if "$@"; then pass "$desc"; else fail "$desc"; fi
}
contains() { case "$2" in *"$1"*) return 0 ;; *) return 1 ;; esac; }
lacks() { ! contains "$1" "$2"; }
file_mode() { stat -f '%Lp' "$1" 2>/dev/null || stat -c '%a' "$1"; }

mkdir -p "$TMP/bin" "$TMP/home" "$TMP/config"
printf 'FORGEJO_ADMIN_TOKEN=%s\n' "$STUB_ADMIN_TOKEN" > "$TMP/admin.env"

# The stub curl keeps its Forgejo state in $STUB_STATE and appends every request
# to events.log, a shared record the stub git writes to as well so a test can
# assert the order of a curl call against a git call. It records a violation
# when the admin token reaches its argv or a call carries no curl config file,
# which is how the token is supposed to travel.
cat >"$TMP/bin/curl" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail

orig_args=("$@")
method=GET
resp=/dev/null
url=""
body=""
config=""
while [ $# -gt 0 ]; do
  case "$1" in
    -sS) ;;
    -o) resp=$2; shift ;;
    -w) shift ;;
    -X) method=$2; shift ;;
    -K) config=$2; shift ;;
    -H) shift ;;
    --data-binary) body=${2#@}; shift ;;
    -*) ;;
    *) url=$1 ;;
  esac
  shift
done
path=${url#"$STUB_API"}

printf 'curl %s %s\n' "$method" "$path" >> "$STUB_STATE/events.log"
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
  printf '%s' "$1"
}

field() { # field FILE KEY — the first "key":"value" in FILE
  sed -n 's/.*"'"$2"'":"\([^"]*\)".*/\1/p' "$1" | head -1
}

# filter_whitelist OWNER REPO FILE drops each merge_whitelist_usernames entry for
# a login that is not a collaborator with write access, modelling the field
# Forgejo silently discards (gt-fn9e6.23).
filter_whitelist() {
  local owner=$1 repo=$2 file=$3
  local wl entry out="" perm tmp
  wl=$(sed -n 's/.*"merge_whitelist_usernames":\[\([^]]*\)\].*/\1/p' "$file" | head -1)
  case "$wl" in *'"'*) ;; *) return 0 ;; esac
  while IFS= read -r entry; do
    entry=${entry//[\" ]/}
    [ -n "$entry" ] || continue
    perm=$(cat "$STUB_STATE/collaborators/${owner}__${repo}__${entry}" 2>/dev/null || true)
    case "$perm" in
      write|admin) out="${out:+$out,}\"$entry\"" ;;
    esac
  done <<< "$(printf '%s' "$wl" | tr ',' '\n')"
  tmp="$file.filtered"
  sed "s|\"merge_whitelist_usernames\":\[[^]]*\]|\"merge_whitelist_usernames\":[$out]|" "$file" > "$tmp" && mv "$tmp" "$file"
}

case "$method $path" in
  "GET /users/"*)
    user=${path#/users/}
    if [ -e "$STUB_STATE/users/$user" ]; then send 200 '{}'; else send 404 '{"message":"not found"}'; fi
    ;;
  "POST /admin/users")
    user=$(field "$body" username)
    if [ -e "$STUB_STATE/users/$user" ]; then
      send 422 '{"message":"user already exists"}'
    else
      mkdir -p "$STUB_STATE/users"
      : > "$STUB_STATE/users/$user"
      send 201 "{\"id\":1,\"login\":\"$user\"}"
    fi
    ;;
  "GET /admin/users/"*"/tokens")
    user=${path#/admin/users/}
    user=${user%/tokens}
    out=""
    id=0
    for f in "$STUB_STATE"/tokens/"$user"/*; do
      [ -e "$f" ] || continue
      id=$((id + 1))
      if [ -n "$out" ]; then out+=","; fi
      out+="{\"id\":$id,\"name\":\"$(basename "$f")\",\"scopes\":[\"write:repository\"]}"
    done
    send 200 "[$out]"
    ;;
  "POST /admin/users/"*"/tokens")
    user=${path#/admin/users/}
    user=${user%/tokens}
    name=$(field "$body" name)
    mkdir -p "$STUB_STATE/tokens/$user" "$STUB_STATE/mints"
    printf '%s' "$STUB_TOKEN_VALUE-$user" > "$STUB_STATE/tokens/$user/$name"
    cp "$body" "$STUB_STATE/mints/$user.json"
    send 201 "{\"id\":7,\"name\":\"$name\",\"sha1\":\"$STUB_TOKEN_VALUE-$user\",\"token_last_eight\":\"ijklmnop\"}"
    ;;
  "DELETE /admin/users/"*"/tokens/"*)
    rest=${path#/admin/users/}
    user=${rest%%/*}
    name=${rest#*/tokens/}
    rm -f "$STUB_STATE/tokens/$user/$name"
    send 204 ''
    ;;
  "GET /repos/"*"/branch_protections")
    rest=${path#/repos/}
    owner=${rest%%/*}
    repo=${rest#*/}
    repo=${repo%%/*}
    if [ ! -e "$STUB_STATE/repos/${owner}__${repo}" ]; then
      send 404 '{"message":"repo not found"}'
      exit 0
    fi
    out=""
    for f in "$STUB_STATE"/protections/"$owner"__"$repo"__*; do
      [ -e "$f" ] || continue
      if [ -n "$out" ]; then out+=","; fi
      out+="$(cat "$f")"
    done
    send 200 "[$out]"
    ;;
  "POST /repos/"*"/branch_protections")
    rest=${path#/repos/}
    owner=${rest%%/*}
    repo=${rest#*/}
    repo=${repo%%/*}
    rule=$(field "$body" rule_name)
    key="$STUB_STATE/protections/${owner}__${repo}__${rule//\//%2F}"
    if [ -e "$key" ]; then
      send 422 '{"message":"rule already exists"}'
    else
      mkdir -p "$STUB_STATE/protections"
      cat "$body" > "$key"
      filter_whitelist "$owner" "$repo" "$key"
      send 201 "$(cat "$key")"
    fi
    ;;
  "PATCH /repos/"*"/branch_protections/"*)
    rest=${path#/repos/}
    owner=${rest%%/*}
    rest=${rest#*/}
    repo=${rest%%/*}
    rule=${rest#*/branch_protections/}
    rule=${rule//%2F//}
    rule=${rule//%2A/*}
    key="$STUB_STATE/protections/${owner}__${repo}__${rule//\//%2F}"
    if [ ! -e "$key" ]; then
      send 404 '{"message":"not found"}'
    else
      cat "$body" > "$key"
      filter_whitelist "$owner" "$repo" "$key"
      send 200 "$(cat "$key")"
    fi
    ;;
  "DELETE /repos/"*"/branch_protections/"*)
    rest=${path#/repos/}
    owner=${rest%%/*}
    rest=${rest#*/}
    repo=${rest%%/*}
    rule=${rest#*/branch_protections/}
    rule=${rule//%2F//}
    rule=${rule//%2A/*}
    key="$STUB_STATE/protections/${owner}__${repo}__${rule//\//%2F}"
    if [ -e "$key" ]; then
      rm -f "$key"
      send 204 ''
    else
      send 404 '{"message":"not found"}'
    fi
    ;;
  "GET /repos/"*"/collaborators/"*)
    rest=${path#/repos/}
    owner=${rest%%/*}
    rest=${rest#*/}
    repo=${rest%%/*}
    user=${rest#*/collaborators/}
    if [ -e "$STUB_STATE/collaborators/${owner}__${repo}__${user}" ]; then
      send 204 ''
    else
      send 404 '{"message":"user is not a collaborator"}'
    fi
    ;;
  "PUT /repos/"*"/collaborators/"*)
    rest=${path#/repos/}
    owner=${rest%%/*}
    rest=${rest#*/}
    repo=${rest%%/*}
    user=${rest#*/collaborators/}
    perm=$(field "$body" permission)
    mkdir -p "$STUB_STATE/collaborators"
    printf '%s' "$perm" > "$STUB_STATE/collaborators/${owner}__${repo}__${user}"
    send 204 ''
    ;;
  *)
    send 404 '{"message":"unhandled stub route"}'
    ;;
esac
STUB
chmod +x "$TMP/bin/curl"

# The stub git keeps one ref list per remote in $STUB_STATE and models a push by
# copying GitHub's list onto Forgejo's — the mirror the script assembles is a
# fetch into a scratch repo followed by one push, and only the outcome of the
# push (Forgejo now holds GitHub's refs) is observable here. A ref line is
# "sha<TAB>ref", the shape git ls-remote prints.
cat >"$TMP/bin/git" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail

printf 'git %s\n' "$*" >> "$STUB_STATE/git-calls.log"
printf 'git %s\n' "$*" >> "$STUB_STATE/events.log"
if [ "${1:-}" = "-C" ]; then shift 2; fi
sub=${1:-}
shift || true

url_of() { # url_of ARGS... — the last non-option argument
  local a out=""
  for a in "$@"; do
    case "$a" in -*) ;; *) out=$a ;; esac
  done
  printf '%s' "$out"
}
refs_file() { # refs_file URL
  case "$1" in
    "$STUB_GITHUB_URL") printf '%s' "$STUB_STATE/github-refs" ;;
    *) printf '%s' "$STUB_STATE/forgejo-refs" ;;
  esac
}

case "$sub" in
  init)
    for a in "$@"; do
      case "$a" in -*) ;; *) mkdir -p "$a" ;; esac
    done
    ;;
  ls-remote)
    url=$(url_of "$@")
    if [ -n "${STUB_LSREMOTE_FAIL:-}" ]; then
      echo "fatal: unable to access '$url': Could not resolve host" >&2
      exit 128
    fi
    cat "$(refs_file "$url")"
    ;;
  fetch) : ;;
  push)
    url=$(url_of "$@")
    if [ -n "${STUB_PUSH_FAIL:-}" ]; then
      echo "remote: error: pre-receive hook declined" >&2
      exit 1
    fi
    cp "$STUB_STATE/github-refs" "$STUB_STATE/forgejo-refs"
    ;;
  *) : ;;
esac
STUB
chmod +x "$TMP/bin/git"

# run_resync [ARGS...] runs the resync against the stubs and prints its combined
# output; $? is its exit code. STUB_PUSH_FAIL and STUB_LSREMOTE_FAIL pass
# through from the caller's environment.
run_resync() {
  local out rc
  out=$(cd "$SCRIPT_DIR/.." && env -u FORGEJO_ADMIN_TOKEN -u FORGEJO_API_URL -u FORGEJO_URL \
    -u FORGEJO_BACKUP_DIR \
    HOME="$TMP/home" XDG_CONFIG_HOME="$CFG" PATH="$TMP/bin:$PATH" \
    STUB_STATE="$STATE" STUB_API="$API" STUB_ADMIN_TOKEN="$STUB_ADMIN_TOKEN" \
    STUB_TOKEN_VALUE="$STUB_TOKEN_VALUE" \
    STUB_GITHUB_URL="$GH_URL" STUB_FORGEJO_URL="$FJ_URL" \
    bash "$RESYNC" --api-url "$API" --github-url "$GH_URL" --forgejo-url "$FJ_URL" \
    --admin-token-file "$TMP/admin.env" --backup-dir "$TMP/backups" "$@" 2>&1)
  rc=$?
  printf '%s' "$out"
  return "$rc"
}

# fresh_state lays down the state a rolled-back rig leaves: GitHub ahead on
# main, a branch Forgejo holds that GitHub dropped (the mirror would prune it),
# and main protected by the rule a cutover wrote.
fresh_state() {
  rm -rf "$STATE" "$TMP/backups"
  mkdir -p "$STATE/repos" "$STATE/protections" "$CFG"
  : > "$STATE/events.log"
  : > "$STATE/git-calls.log"
  : > "$STATE/repos/acme__rig"
  printf 'aaaa1111\trefs/heads/main\nbbbb2222\trefs/heads/feature-a\ncccc3333\trefs/tags/v1.0.0\n' > "$STATE/github-refs"
  printf 'dddd4444\trefs/heads/main\nbbbb2222\trefs/heads/feature-a\neeee5555\trefs/heads/stale-branch\ncccc3333\trefs/tags/v1.0.0\n' > "$STATE/forgejo-refs"
  printf '{"rule_name":"main","enable_push":false,"apply_to_admins":true,"push_whitelist_deploy_keys":false,"enable_merge_whitelist":true,"merge_whitelist_usernames":["bot-landing"],"enable_status_check":true,"status_check_contexts":["ci / gate (push)","om / review"],"block_on_outdated_branch":true}\n' > "$STATE/protections/acme__rig__main"
}

events() { cat "$STATE/events.log" 2>/dev/null || true; }
first_event_line() { grep -n -F -- "$1" "$STATE/events.log" 2>/dev/null | head -1 | cut -d: -f1; }
count_events() { grep -c -F -- "$1" "$STATE/events.log" 2>/dev/null || true; }
main_rule() { printf '%s/acme__rig__main' "$STATE/protections"; }

echo "=== a successful resync imports GitHub's refs and restores protection ==="
fresh_state
out=$(run_resync acme/rig); rc=$?
if [ "$rc" = 0 ]; then pass "resync exits 0"; else fail "resync exits 0 (rc=$rc)" "$out"; fi
check "Forgejo now holds exactly GitHub's refs" cmp -s "$STATE/github-refs" "$STATE/forgejo-refs"
check "the branch GitHub dropped is gone from Forgejo" lacks "stale-branch" "$(cat "$STATE/forgejo-refs")"
check "main's new sha landed on Forgejo" grep -q "aaaa1111	refs/heads/main" "$STATE/forgejo-refs"
check "main protection is restored" test -f "$(main_rule)"
if [ -f "$(main_rule)" ]; then
  check "the restored rule refuses every push" grep -q '"enable_push":false' "$(main_rule)"
  check "the restored rule names the landing bot" grep -q '"merge_whitelist_usernames":\["bot-landing"\]' "$(main_rule)"
fi
check "the plan names the branch it will push" contains "will push refs/heads/main" "$out"
check "the plan names the branch it will delete" contains "will delete refs/heads/stale-branch" "$out"
check "the reported ref lists match" contains "refs match: Forgejo holds every GitHub ref" "$out"
check "the run prints the git push it sends" contains "git -C" "$out"

echo "=== the protection is lifted before the push ==="
delete_line=$(first_event_line "curl DELETE /repos/acme/rig/branch_protections/main")
push_line=$(first_event_line "push --prune")
check "the rule is deleted exactly once" test "$(count_events "curl DELETE /repos/acme/rig/branch_protections/main")" = 1
check "the push is sent exactly once" test "$(grep -c -F 'push --prune' "$STATE/git-calls.log" || true)" = 1
check "the delete precedes the push" test -n "$delete_line" -a -n "$push_line" -a "$delete_line" -lt "$push_line"
check "the push prunes what GitHub lacks" grep -q -F -- '--prune' "$STATE/git-calls.log"
check "the push carries the branch refspec" grep -q -F '+refs/heads/*:refs/heads/*' "$STATE/git-calls.log"
check "the push carries the tag refspec" grep -q -F '+refs/tags/*:refs/tags/*' "$STATE/git-calls.log"

echo "=== the lifted rule is copied to a dated backup ==="
backup=$(find "$TMP/backups" -type f -name '*.bak-*' 2>/dev/null | head -1)
check "a dated .bak- copy of the rule exists" test -n "$backup"
if [ -n "$backup" ]; then
  check "the copy holds the rule that was lifted" grep -q '"rule_name":"main"' "$backup"
fi

echo "=== a failed push still restores protection and exits non-zero ==="
fresh_state
out=$(STUB_PUSH_FAIL=1 run_resync acme/rig); rc=$?
if [ "$rc" != 0 ]; then pass "a rejected push exits non-zero"; else fail "a rejected push exits non-zero (rc=$rc)" "$out"; fi
check "the push failure is surfaced" contains "pre-receive hook declined" "$out"
check "main was unprotected for the push" grep -q -F "curl DELETE /repos/acme/rig/branch_protections/main" "$STATE/events.log"
check "protection is restored anyway" test -f "$(main_rule)"
check "the restore ran after the failed push" contains "provisioning complete" "$out"

echo "=== a GitHub that cannot be read leaves the rule lifted-free ==="
fresh_state
out=$(STUB_LSREMOTE_FAIL=1 run_resync acme/rig); rc=$?
if [ "$rc" != 0 ]; then pass "an unreadable GitHub exits non-zero"; else fail "an unreadable GitHub exits non-zero (rc=$rc)" "$out"; fi
check "no protection was lifted for a read that never happened" test "$(count_events "curl DELETE /repos/acme/rig/branch_protections/main")" = 0
check "the original protection is untouched" test -f "$(main_rule)"

echo "=== a rule another run lifted is restored, not double-deleted ==="
fresh_state
rm -f "$STATE/protections/acme__rig__main"
out=$(run_resync acme/rig); rc=$?
if [ "$rc" = 0 ]; then pass "a resync over an unprotected repo exits 0"; else fail "a resync over an unprotected repo exits 0 (rc=$rc)" "$out"; fi
check "no delete is sent when there is nothing to lift" test "$(count_events "curl DELETE /repos/acme/rig/branch_protections/main")" = 0
check "protection is still restored" test -f "$(main_rule)"
check "the run reports nothing to lift" contains "nothing to lift" "$out"

echo "=== a second run converges ==="
fresh_state
run_resync acme/rig >/dev/null 2>&1
: > "$STATE/events.log"
out=$(run_resync acme/rig); rc=$?
if [ "$rc" = 0 ]; then pass "the second run exits 0"; else fail "the second run exits 0 (rc=$rc)" "$out"; fi
check "the second run still reports matching refs" contains "refs match" "$out"
check "nothing is left to delete" lacks "will delete" "$out"
check "main stays protected" test -f "$(main_rule)"

echo "=== dry run reads and writes nothing ==="
fresh_state
out=$(run_resync acme/rig --dry-run); rc=$?
if [ "$rc" = 0 ]; then pass "dry run exits 0"; else fail "dry run exits 0 (rc=$rc)" "$out"; fi
check "dry run sends no delete" test "$(count_events "curl DELETE")" = 0
check "dry run sends no git push" test "$(count_events "push --prune")" = 0
check "dry run runs no provisioning write" test "$(count_events "curl POST /admin/users")" = 0
check "dry run leaves Forgejo behind GitHub" grep -q "stale-branch" "$STATE/forgejo-refs"
check "dry run leaves protection in place" test -f "$(main_rule)"
check "dry run writes no backup" test ! -d "$TMP/backups"
check "dry run prints the delete it would send" contains "DELETE http://forgejo.test/api/v1/repos/acme/rig/branch_protections/main" "$out"
check "dry run prints the push it would send" contains "push --prune" "$out"
check "dry run prints the restore it would run" contains "forgejo-provision.sh --repo acme/rig" "$out"
check "dry run says it wrote nothing" contains "dry run: no write was sent" "$out"

echo "=== token and log hygiene ==="
fresh_state
out=$(run_resync acme/rig); rc=$?
check "the admin token never reaches a stub's argv" test ! -e "$STATE/argv-violations"
check "the admin token never reaches the output" lacks "$STUB_ADMIN_TOKEN" "$out"
check "no minted bot token reaches the output" lacks "$STUB_TOKEN_VALUE" "$out"
check "nothing is written into HOME" test "$(find "$TMP/home" -type f | wc -l | tr -d ' ')" = 0
check "the token files land only in the test's config dir" test "$(find "$CFG" -type f -name 'forgejo-*.env' | wc -l | tr -d ' ')" = 3

echo "=== argument errors ==="
out=$(run_resync); rc=$?
if [ "$rc" = 2 ]; then pass "a missing OWNER/NAME exits 2"; else fail "a missing OWNER/NAME exits 2 (rc=$rc)" "$out"; fi
check "the missing repository is named" contains "OWNER/NAME is required" "$out"
out=$(run_resync not-a-repo); rc=$?
if [ "$rc" = 2 ]; then pass "a bare name exits 2"; else fail "a bare name exits 2 (rc=$rc)" "$out"; fi
out=$(run_resync --nope acme/rig); rc=$?
if [ "$rc" = 2 ]; then pass "an unknown option exits 2"; else fail "an unknown option exits 2 (rc=$rc)" "$out"; fi
out=$(run_resync a/b/c); rc=$?
if [ "$rc" = 2 ]; then pass "a three-part repository exits 2"; else fail "a three-part repository exits 2 (rc=$rc)" "$out"; fi
out=$(env -u FORGEJO_ADMIN_TOKEN HOME="$TMP/home" XDG_CONFIG_HOME="$CFG" PATH="$TMP/bin:$PATH" \
  STUB_STATE="$STATE" STUB_API="$API" STUB_ADMIN_TOKEN="$STUB_ADMIN_TOKEN" \
  STUB_GITHUB_URL="$GH_URL" STUB_FORGEJO_URL="$FJ_URL" \
  bash "$RESYNC" --api-url "$API" --admin-token-file "$TMP/absent.env" acme/rig 2>&1); rc=$?
if [ "$rc" = 1 ]; then pass "a missing admin token exits 1"; else fail "a missing admin token exits 1 (rc=$rc)" "$out"; fi
check "the missing token is named" contains "no admin token" "$out"

echo
if [ "$FAIL" = 0 ]; then
  echo "forgejo-resync_test.sh: all $PASS cases passed"
else
  echo "forgejo-resync_test.sh: $FAIL of $((PASS + FAIL)) cases failed"
  exit 1
fi
