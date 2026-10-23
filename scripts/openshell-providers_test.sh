#!/usr/bin/env bash
# Tests for scripts/openshell-providers.sh (gt-88rqe).
#
# The defect being guarded: `make openshell-providers` is documented as a no-op
# on rerun, but profile import is not idempotent ("custom provider profile
# 'deepseek-claude' already exists"), so the second run exited 1 after the two
# providers were already there.
#
# A stub openshell on PATH holds the gateway's state — the profiles and
# providers it lists — and records every call, so the assertions are about
# which calls happen, not about their output. GT_CONFIG_DIR points inside the
# temp dir, so the key files never touch the operator's ~/.config and nothing
# here can reach the real gateway.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROVIDERS="$SCRIPT_DIR/openshell-providers.sh"

TMP="$(mktemp -d "${TMPDIR:-/tmp}/openshell-providers-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

PASS=0
FAIL=0
DEEPSEEK_KEY="sk-deepseek-test-zqxvjkcd"
FORGEJO_KEY="forgejo-test-token-zqxvjkcd"

pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; FAIL=$((FAIL + 1)); if [ $# -gt 1 ]; then printf '%s\n' "$2" | sed 's/^/    | /'; fi; }
check() { # check DESCRIPTION COMMAND...
  local desc=$1
  shift
  if "$@"; then pass "$desc"; else fail "$desc"; fi
}
contains() { case "$2" in *"$1"*) return 0 ;; *) return 1 ;; esac; }
lacks() { ! contains "$1" "$2"; }

mkdir -p "$TMP/bin" "$TMP/config" "$TMP/state"
printf 'DEEPSEEK_API_KEY=%s\n' "$DEEPSEEK_KEY" >"$TMP/config/deepseek.env"
printf 'FORGEJO_TOKEN=%s\n' "$FORGEJO_KEY" >"$TMP/config/forgejo-polecat.env"
: >"$TMP/state/profiles"
: >"$TMP/state/providers"
: >"$TMP/calls.log"

# The stub answers the four subcommands the script uses. profile import fails
# the way the real gateway does when the id is already listed, which is what
# makes the rerun assertion meaningful; provider create prints the credential
# it received so the test can see the script's masking work.
cat >"$TMP/bin/openshell" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
printf '%s\n' "$*" >>"$STUB_CALLS"
state="$STUB_STATE"

case "$1 $2" in
  "profile list")
    printf 'NAME             TYPE      CATEGORY   SOURCE  SCOPE\n'
    cat "$state/profiles"
    ;;
  "profile lint")
    ;;
  "profile import")
    file=$4
    id=$(awk '/^id:/ { sub(/^id:[[:space:]]*/, ""); sub(/[[:space:]#].*$/, ""); print; exit }' "$file")
    if grep -qxF "$id" "$state/profiles"; then
      echo "provider profile import failed: custom provider profile '$id' already exists" >&2
      exit 1
    fi
    printf '%s\n' "$id" >>"$state/profiles"
    echo "imported profile $id"
    ;;
  "provider list")
    printf 'NAME  TYPE  SOURCE  SCOPE\n'
    cat "$state/providers"
    ;;
  "provider create")
    name="" credential=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --name) name=$2; shift ;;
        --credential) credential=$2; shift ;;
      esac
      shift
    done
    printf '%s\n' "$name" >>"$state/providers"
    if [ -n "$credential" ]; then
      printf 'created provider %s with credential value=%s\n' "$name" "${!credential-}"
    else
      printf 'created provider %s\n' "$name"
    fi
    ;;
  *)
    echo "stub openshell: unhandled $*" >&2
    exit 1
    ;;
esac
STUB
chmod +x "$TMP/bin/openshell"

# run: execute the script against the stub; sets RC and OUT. The stub appends
# every call to $TMP/calls.log, so the assertions count across the whole test.
run() {
  set +e
  OUT="$(PATH="$TMP/bin:$PATH" GT_CONFIG_DIR="$TMP/config" \
    STUB_CALLS="$TMP/calls.log" STUB_STATE="$TMP/state" \
    bash "$PROVIDERS" 2>&1)"
  RC=$?
  set -e
}

calls() { grep -c "$1" "$TMP/calls.log" || true; }
assert_rc() {
  if [ "$RC" -eq "$1" ]; then
    pass "$2"
  else
    fail "$2 (exit $RC, want $1)" "$OUT"
  fi
}

echo "fresh run: nothing in the gateway"
run
assert_rc 0 "exits 0"
check "imports both profiles" [ "$(calls '^profile import')" -eq 2 ]
check "creates both providers" [ "$(calls '^provider create')" -eq 2 ]
check "led the user to the imported profiles" contains "imported profile deepseek-claude" "$OUT"
check "never printed the deepseek key" lacks "$DEEPSEEK_KEY" "$OUT"
check "never printed the forgejo token" lacks "$FORGEJO_KEY" "$OUT"
check "masked the credential the provider create echoed" contains "value=***" "$OUT"

echo "rerun: both profiles and both providers already present"
run
assert_rc 0 "exits 0 on a rerun"
check "reported the existing profiles" contains "profile deepseek-claude exists" "$OUT"
check "did not call profile import again" [ "$(calls '^profile import')" -eq 2 ]
check "did not call provider create again" [ "$(calls '^provider create')" -eq 2 ]

echo "third run in a row is also clean"
run
assert_rc 0 "exits 0 the third time"
check "still no new import" [ "$(calls '^profile import')" -eq 2 ]
check "still no new provider create" [ "$(calls '^provider create')" -eq 2 ]

echo "partial state: profiles imported, providers missing"
: >"$TMP/state/providers"
run
assert_rc 0 "exits 0"
check "left the profiles alone" [ "$(calls '^profile import')" -eq 2 ]
check "created the two missing providers" [ "$(calls '^provider create')" -eq 4 ]

echo
if [ "$FAIL" -ne 0 ]; then
  echo "openshell-providers_test: $FAIL failed, $PASS passed"
  exit 1
fi
echo "openshell-providers_test: all $PASS passed"
