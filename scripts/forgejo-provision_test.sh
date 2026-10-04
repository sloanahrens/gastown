#!/usr/bin/env bash
# Tests for scripts/forgejo-provision.sh (gt-fn9e6.4). A stub curl on PATH keeps
# a small Forgejo state in a directory — users, tokens and branch-protection
# rules — and answers the provisioner's calls from it, so the create, converge,
# rotate and failure paths run with no live Forgejo and no network. HOME and
# XDG_CONFIG_HOME point inside the test's temp dir, so the token files the
# provisioner writes can never land in the operator's real ~/.config.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROVISION="$SCRIPT_DIR/forgejo-provision.sh"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/forgejo-provision-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT
PASS=0
FAIL=0
STUB_ADMIN_TOKEN="stub-admin-token"
STUB_TOKEN_VALUE="stubtoken-abcdefghijklmnop"
API="http://forgejo.test/api/v1"
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
# has_all FILE NEEDLE... — true when every needle is in FILE.
has_all() {
  local file=$1 needle
  shift
  for needle in "$@"; do grep -qF -- "$needle" "$file" || return 1; done
  return 0
}

mkdir -p "$TMP/bin" "$TMP/home" "$TMP/config"
printf 'FORGEJO_ADMIN_TOKEN=%s\n' "$STUB_ADMIN_TOKEN" > "$TMP/admin.env"

# The stub keeps its state in $STUB_STATE and logs every request to calls.log.
# It records a violation when the admin token reaches its argv or when the call
# carries no curl config file, which is how the token is supposed to travel.
cat >"$TMP/bin/curl" <<'STUB'
#!/usr/bin/env bash
# Stub curl for scripts/forgejo-provision_test.sh: one Forgejo instance in a
# directory.
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

printf '%s %s\n' "$method" "$path" >> "$STUB_STATE/calls.log"
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
    if [ -n "${STUB_DENY_TOKEN_MINT:-}" ]; then
      send 403 '{"message":"token does not have at least one of required scopes"}'
      exit 0
    fi
    name=$(field "$body" name)
    mkdir -p "$STUB_STATE/tokens/$user"
    printf '%s' "$STUB_TOKEN_VALUE-$user" > "$STUB_STATE/tokens/$user/$name"
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
      # The server applies the fields the client sent; the provisioner sends
      # every field it verifies, so storing the body models the result.
      cat "$body" > "$key"
      send 200 "$(cat "$key")"
    fi
    ;;
  *)
    send 404 '{"message":"unhandled stub route"}'
    ;;
esac
STUB
chmod +x "$TMP/bin/curl"

# run_provision [ARGS...] runs the provisioner against the stub and prints its
# combined output; rc holds the exit code.
# run_provision [ARGS...] runs the provisioner against the stub, prints its
# combined output, and returns its exit code (the callers read it from $?).
run_provision() {
  local out rc
  out=$(cd "$SCRIPT_DIR/.." && env -u FORGEJO_ADMIN_TOKEN -u FORGEJO_API_URL -u FORGEJO_URL \
    HOME="$TMP/home" XDG_CONFIG_HOME="$CFG" PATH="$TMP/bin:$PATH" \
    STUB_STATE="$STATE" STUB_API="$API" STUB_ADMIN_TOKEN="$STUB_ADMIN_TOKEN" \
    STUB_TOKEN_VALUE="$STUB_TOKEN_VALUE" \
    bash "$PROVISION" --api-url "$API" --admin-token-file "$TMP/admin.env" "$@" 2>&1)
  rc=$?
  printf '%s' "$out"
  return "$rc"
}

# fresh_state empties the stub's Forgejo and registers the one repository the
# protection cases provision, the way a cutover would have created it.
fresh_state() {
  rm -rf "$STATE"
  mkdir -p "$STATE/repos" "$CFG"
  : > "$STATE/calls.log"
  : > "$STATE/repos/acme__rig"
}

calls() { cat "$STATE/calls.log" 2>/dev/null || true; }
count_calls() { # count_calls METHOD [PATH_CONTAINS]
  local n=0 line
  while IFS= read -r line; do
    case "$line" in
      "$1 "*) case "$line" in *"${2:-}"*) n=$((n + 1)) ;; esac ;;
    esac
  done <<<"$(calls)"
  printf '%s' "$n"
}
token_file() { printf '%s/gt/forgejo-%s.env' "$CFG" "$1"; }
file_mode() { stat -f '%Lp' "$1" 2>/dev/null || stat -c '%a' "$1"; }

echo "=== fresh provision ==="
fresh_state
out=$(run_provision --repo acme/rig); rc=$?
if [ "$rc" = 0 ]; then pass "first run exits 0"; else fail "first run exits 0 (rc=$rc)" "$out"; fi
check "the three bots are created" test -e "$STATE/users/bot-polecat" -a -e "$STATE/users/bot-landing" -a -e "$STATE/users/bot-registry"
for role in polecat landing registry; do
  path=$(token_file "$role")
  check "token file for $role exists" test -f "$path"
  check "token file for $role is mode 600" test "$(file_mode "$path")" = 600
  check "token file for $role names its bot" grep -q "^FORGEJO_USER=bot-$role$" "$path"
  check "token file for $role holds the minted token" grep -q "^FORGEJO_TOKEN=$STUB_TOKEN_VALUE-bot-$role$" "$path"
  check "token file for $role names the web URL" grep -q "^FORGEJO_URL=http://forgejo.test$" "$path"
done
check "the token value never reaches stdout" lacks "$STUB_TOKEN_VALUE" "$out"
check "two protection rules are stored" test "$(find "$STATE/protections" -type f | wc -l | tr -d ' ')" = 2
main_rule="$STATE/protections/acme__rig__main"
land_rule="$STATE/protections/acme__rig__land%2F*"
check "main takes no push, admins included" has_all "$main_rule" '"enable_push":false' '"apply_to_admins":true'
check "main merges through the landing bot alone" grep -q '"merge_whitelist_usernames":\["bot-landing"\]' "$main_rule"
check "main requires the gate and review contexts" has_all "$main_rule" 'ci / gate (push)' 'om / review'
check "main refuses a stale candidate" grep -q '"block_on_outdated_branch":true' "$main_rule"
check "land/** push is whitelisted to the landing bot" grep -q '"push_whitelist_usernames":\["bot-landing"\]' "$land_rule"
check "land/** lets the push mirror's deploy keys push" grep -q '"push_whitelist_deploy_keys":true' "$land_rule"
check "land/** takes no push beyond the whitelist" has_all "$land_rule" '"enable_push_whitelist":true' '"apply_to_admins":true'
check "each role mints exactly one token" test "$(count_calls POST /tokens)" = 3
check "nothing is written into HOME" test "$(find "$TMP/home" -type f | wc -l | tr -d ' ')" = 0
check "the config dir holds the three token files and nothing else" test "$(find "$CFG" -type f | wc -l | tr -d ' ')" = 3

echo "=== a second run converges without writing ==="
before=$(cat "$(token_file landing)")
: > "$STATE/calls.log"
out=$(run_provision --repo acme/rig); rc=$?
if [ "$rc" = 0 ]; then pass "second run exits 0"; else fail "second run exits 0 (rc=$rc)" "$out"; fi
check "second run sends no POST" test "$(count_calls POST)" = 0
check "second run sends no PATCH or DELETE" test "$(count_calls PATCH)" = 0 -a "$(count_calls DELETE)" = 0
check "second run leaves the token file alone" test "$(cat "$(token_file landing)")" = "$before"
check "second run reports the rules as already set" contains "already set" "$out"

echo "=== drift is repaired ==="
printf '{"rule_name":"main","enable_push":true,"apply_to_admins":true}\n' > "$main_rule"
: > "$STATE/calls.log"
out=$(run_provision --repo acme/rig); rc=$?
if [ "$rc" = 0 ]; then pass "drift run exits 0"; else fail "drift run exits 0 (rc=$rc)" "$out"; fi
check "the drifted main rule is patched" test "$(count_calls PATCH /branch_protections/main)" = 1
check "the patch restores no-push for admins too" grep -q '"enable_push":false' "$main_rule"
check "the patch restores the required contexts" grep -q 'om / review' "$main_rule"
check "the untouched land rule is not patched" test "$(count_calls PATCH /branch_protections/land)" = 0

echo "=== rotation replaces the token ==="
: > "$STATE/calls.log"
out=$(run_provision --repo acme/rig --rotate); rc=$?
if [ "$rc" = 0 ]; then pass "rotate run exits 0"; else fail "rotate run exits 0 (rc=$rc)" "$out"; fi
check "rotation deletes the previous token by name" test "$(count_calls DELETE /admin/users/bot-landing/tokens/gt-provision)" = 1
check "rotation mints a new token" test "$(count_calls POST /tokens)" = 3
check "the token file holds the new token" grep -q "^FORGEJO_TOKEN=$STUB_TOKEN_VALUE-bot-landing$" "$(token_file landing)"

echo "=== argv and log hygiene ==="
check "the admin token never reaches the stub's argv" test ! -e "$STATE/argv-violations"
check "the admin token never reaches the output" lacks "$STUB_ADMIN_TOKEN" "$out"
check "every call carried the curl config file" test ! -e "$STATE/argv-violations"

echo "=== dry run writes nothing ==="
STATE="$TMP/state-dry"
CFG="$TMP/config-dry"
fresh_state
out=$(run_provision --repo acme/rig --dry-run); rc=$?
if [ "$rc" = 0 ]; then pass "dry run exits 0"; else fail "dry run exits 0 (rc=$rc)" "$out"; fi
check "dry run sends no write method" test "$(count_calls POST)" = 0 -a "$(count_calls PATCH)" = 0 -a "$(count_calls DELETE)" = 0
check "dry run creates no user" test ! -e "$STATE/users/bot-polecat"
check "dry run writes no token file" test ! -e "$(token_file polecat)"
check "dry run says what it would do" contains "would create user bot-polecat" "$out"
STATE="$TMP/state"
CFG="$TMP/config"

echo "=== a denied token mint fails loudly ==="
# A config dir with no token files, so the run reaches the mint that the stub
# refuses the way Forgejo refuses a token without site-admin rights.
CFG="$TMP/config-deny"
fresh_state
out=$(STUB_DENY_TOKEN_MINT=1 run_provision); rc=$?
if [ "$rc" != 0 ]; then pass "a 403 on the mint exits non-zero"; else fail "a 403 on the mint exits non-zero (rc=$rc)" "$out"; fi
check "the 403 names the admin rights the token needs" contains "site-administrator" "$out"
check "the 403 writes no token file" test ! -e "$(token_file polecat)"
CFG="$TMP/config"

echo "=== a repository with no protections yet ==="
fresh_state
out=$(run_provision --repo acme/rig); rc=$?
if [ "$rc" = 0 ]; then pass "an unprotected repo provisions and exits 0"; else fail "an unprotected repo provisions and exits 0 (rc=$rc)" "$out"; fi
check "both rules are created on it" test "$(count_calls POST /branch_protections)" = 2

echo "=== an unknown repository fails loudly ==="
fresh_state
out=$(run_provision --repo acme/missing); rc=$?
if [ "$rc" != 0 ]; then pass "a repo the instance does not hold exits non-zero"; else fail "a repo the instance does not hold exits non-zero (rc=$rc)" "$out"; fi
check "the failure names the repository" contains "repos/acme/missing" "$out"

echo "=== argument errors ==="
out=$(run_provision --repo not-a-repo); rc=$?
if [ "$rc" = 2 ]; then pass "--repo without OWNER/NAME exits 2"; else fail "--repo without OWNER/NAME exits 2 (rc=$rc)" "$out"; fi
out=$(run_provision --role 'bad/role'); rc=$?
if [ "$rc" = 2 ]; then pass "a role with a slash exits 2"; else fail "a role with a slash exits 2 (rc=$rc)" "$out"; fi
out=$(env -u FORGEJO_ADMIN_TOKEN HOME="$TMP/home" XDG_CONFIG_HOME="$TMP/config" PATH="$TMP/bin:$PATH" \
  STUB_STATE="$STATE" STUB_API="$API" STUB_ADMIN_TOKEN="$STUB_ADMIN_TOKEN" \
  bash "$PROVISION" --api-url "$API" --admin-token-file "$TMP/absent.env" 2>&1); rc=$?
if [ "$rc" = 1 ]; then pass "a missing admin token exits 1"; else fail "a missing admin token exits 1 (rc=$rc)" "$out"; fi
check "the missing token is named in the error" contains "no admin token" "$out"
out=$(run_provision --nope); rc=$?
if [ "$rc" = 2 ]; then pass "an unknown option exits 2"; else fail "an unknown option exits 2 (rc=$rc)" "$out"; fi

echo
if [ "$FAIL" = 0 ]; then
  echo "forgejo-provision_test.sh: all $PASS cases passed"
else
  echo "forgejo-provision_test.sh: $FAIL of $((PASS + FAIL)) cases failed"
  exit 1
fi
