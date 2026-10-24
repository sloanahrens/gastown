#!/usr/bin/env bash
# Tests for scripts/openshell-providers.sh (gt-88rqe).
#
# The defect being guarded: `make openshell-providers` is documented as a no-op
# on rerun, but profile import is not idempotent ("custom provider profile
# 'deepseek-claude' already exists"), so the second run exited 1 after the two
# providers were already there.
#
# The named-gateway cases (gt-ujzas) assert that every call carries the -g the
# caller asked for, and that a rerun on that gateway is still a no-op.
#
# A stub openshell on PATH holds the gateway's state — the profiles and
# providers it lists — and records every call, so the assertions are about
# which calls happen, not about their output. GT_CONFIG_DIR points inside the
# temp dir, so the key files never touch the operator's ~/.config and nothing
# here can reach the real gateway.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

TMP="$(mktemp -d "${TMPDIR:-/tmp}/openshell-providers-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

# The script finds its profiles relative to itself (../deploy/polecat/profiles),
# so it runs from a copy of the tree under the temp dir: a scenario can add a
# profile there without touching the repo's.
PROVIDERS="$TMP/root/scripts/openshell-providers.sh"
mkdir -p "$TMP/root/scripts" "$TMP/root/deploy/polecat"
cp "$SCRIPT_DIR/openshell-providers.sh" "$PROVIDERS"
cp -R "$REPO_ROOT/deploy/polecat/profiles" "$TMP/root/deploy/polecat/profiles"

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
# it received so the test can see the script's masking work. The stub takes a
# profile's id from its file name, never by parsing the yaml, so the script's
# own id parser is what the assertions exercise. profile list fails while
# $state/fail-profile-list exists.
cat >"$TMP/bin/openshell" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
printf '%s\n' "$*" >>"$STUB_CALLS"
state="$STUB_STATE"

# -g NAME is a global flag the script passes before the subcommand when the
# caller named a gateway. The whole argv is recorded above, so the assertions
# can see the flag; the case below dispatches on the subcommand.
if [ "${1-}" = "-g" ] && [ $# -ge 2 ]; then
  shift 2
fi
case "$1 $2" in
  "profile list")
    if [ -e "$state/fail-profile-list" ]; then
      echo "Error: gateway unreachable" >&2
      exit 1
    fi
    printf 'NAME             TYPE      CATEGORY   SOURCE  SCOPE\n'
    cat "$state/profiles"
    ;;
  "profile lint")
    ;;
  "profile import")
    file=$4
    id=$(basename "$file" .yaml)
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

# run ARGS...: execute the script against the stub; sets RC and OUT. Calls are
# recorded to $CALLS_LOG, so the gateway cases can keep their calls apart from
# the default gateway's; the stub appends, so a count spans whole scenarios.
CALLS_LOG="$TMP/calls.log"
run() {
  set +e
  OUT="$(PATH="$TMP/bin:$PATH" GT_CONFIG_DIR="$TMP/config" \
    STUB_CALLS="$CALLS_LOG" STUB_STATE="$TMP/state" \
    bash "$PROVIDERS" "$@" 2>&1)"
  RC=$?
  set -e
}

calls() { grep -c "$1" "$CALLS_LOG" || true; }
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

echo "quoted ids: a profile whose yaml quotes its id is still recognised"
printf 'id: "quoted-double"\n' >"$TMP/root/deploy/polecat/profiles/quoted-double.yaml"
printf "id: 'quoted-single' # comment\n" >"$TMP/root/deploy/polecat/profiles/quoted-single.yaml"
run
assert_rc 0 "imports the quoted profiles"
check "imported the double-quoted profile" [ "$(calls '^profile import -f .*quoted-double.yaml')" -eq 1 ]
check "imported the single-quoted profile" [ "$(calls '^profile import -f .*quoted-single.yaml')" -eq 1 ]
run
assert_rc 0 "exits 0 on a rerun"
check "reported the double-quoted profile as existing" contains "profile quoted-double exists" "$OUT"
check "reported the single-quoted profile as existing" contains "profile quoted-single exists" "$OUT"
check "did not import either again" [ "$(calls '^profile import -f .*quoted-')" -eq 2 ]

echo "failed list: profile list errors, so existence is unknown"
touch "$TMP/state/fail-profile-list"
imports_before="$(calls '^profile import')"
run
assert_rc 1 "exits 1"
check "named the failed list" contains "profile list --global failed" "$OUT"
check "passed on the gateway's error" contains "gateway unreachable" "$OUT"
check "did not try to import anything" [ "$(calls '^profile import')" -eq "$imports_before" ]
rm -f "$TMP/state/fail-profile-list"

echo "named gateway: -g k8s creates the providers on it"
check "the runs without a gateway passed no -g" [ "$(grep -c '^-g ' "$CALLS_LOG" || true)" -eq 0 ]
: >"$TMP/state/profiles"
: >"$TMP/state/providers" # an empty gateway of its own
CALLS_LOG="$TMP/gw-calls.log"
: >"$CALLS_LOG"
run -g k8s
assert_rc 0 "exits 0"
check "every call named the gateway" [ "$(grep -c -v '^-g k8s ' "$CALLS_LOG" || true)" -eq 0 ]
check "imported the profiles on the gateway" [ "$(calls '^-g k8s profile import')" -ge 2 ]
check "created both providers on the gateway" [ "$(calls '^-g k8s provider create')" -eq 2 ]
check "never printed the deepseek key" lacks "$DEEPSEEK_KEY" "$OUT"
check "never printed the forgejo token" lacks "$FORGEJO_KEY" "$OUT"
check "masked the credential the provider create echoed" contains "value=***" "$OUT"

echo "rerun on the named gateway"
imports_before="$(calls '^-g k8s profile import')"
run -g k8s
assert_rc 0 "exits 0 on a rerun"
check "reported the existing providers" contains "provider deepseek exists" "$OUT"
check "did not call profile import again" [ "$(calls '^-g k8s profile import')" -eq "$imports_before" ]
check "did not call provider create again" [ "$(calls '^-g k8s provider create')" -eq 2 ]

echo "argument checking stops before any openshell call"
CALLS_LOG="$TMP/arg-calls.log"
: >"$CALLS_LOG"
run -g
assert_rc 2 "a -g with no name exits 2"
check "named the missing gateway name" contains "-g needs a gateway name" "$OUT"
run --bogus
assert_rc 2 "an unknown argument exits 2"
check "named the unknown argument" contains "unknown argument: --bogus" "$OUT"
check "called openshell for neither" [ ! -s "$CALLS_LOG" ]

echo
if [ "$FAIL" -ne 0 ]; then
  echo "openshell-providers_test: $FAIL failed, $PASS passed"
  exit 1
fi
echo "openshell-providers_test: all $PASS passed"
