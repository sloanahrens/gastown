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

# What the stub gt answers for a gateway: $TMP/endpoints/GATEWAY holds the three
# lines `gt sandbox endpoint` prints, and $TMP/endpoints/render-GATEWAY holds the
# forgejo-polecat profile `--render` prints for that endpoint. The rewrite itself
# is gt's and is unit tested in Go; these cases check that the script imports
# what gt prints and renders. set_endpoint writes both for a gateway.
# set_endpoint GATEWAY HOST PORT [CIDR...]
set_endpoint() {
  local gateway=$1 host=$2 port=$3 ips="" cidr
  shift 3
  for cidr in "$@"; do ips="${ips:+$ips,}$cidr"; done
  printf 'host=%s\nport=%s\nallowed_ips=%s\n' "$host" "$port" "$ips" >"$TMP/endpoints/$gateway"
  {
    printf 'id: forgejo-polecat\nendpoints:\n  - host: %s\n    port: %s\n' "$host" "$port"
    if [ $# -gt 0 ]; then printf '    allowed_ips: ["%s"]\n' "${ips//,/\",\"}"; fi
  } >"$TMP/endpoints/render-$gateway"
}
mkdir -p "$TMP/endpoints"
set_endpoint openshell host.docker.internal 3000
cp "$REPO_ROOT/deploy/polecat/profiles/forgejo-polecat.yaml" "$TMP/endpoints/render-openshell"
set_endpoint k8s control-plane.openshell.svc.cluster.local 3000 192.168.5.2/32
GT_CALLS="$TMP/gt-calls.log"
: >"$GT_CALLS"

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
    cp "$file" "$state/content-$id"
    echo "imported profile $id"
    ;;
  "profile export")
    # The profile as the gateway holds it: the last copy imported or updated.
    id=${!#}
    if [ ! -e "$state/content-$id" ]; then
      echo "Error: no custom profile $id" >&2
      exit 1
    fi
    printf 'resource_version: 7\n'
    cat "$state/content-$id"
    ;;
  "profile update")
    file="" prev=""
    for arg in "$@"; do
      [ "$prev" = "-f" ] && file=$arg
      prev=$arg
    done
    id=${!#}
    # The real gateway refuses an update that carries no resource_version.
    if ! grep -qE '^resource_version: [1-9]' "$file"; then
      echo "Error: custom provider profile update requires a non-zero resource_version; export the current profile before editing it" >&2
      exit 1
    fi
    cp "$file" "$state/content-$id"
    echo "updated profile $id"
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

# The stub gt answers `gt sandbox endpoint --gateway NAME --root DIR` with the
# file $STUB_ENDPOINTS/NAME, and refuses every gateway while $STUB_ENDPOINTS/fail
# exists, as the real command does for a gateway no sandbox profile names. Its
# argv is logged to $STUB_GT_CALLS, so the cases can see which gateway was asked.
# The stub gt answers `gt sandbox endpoint --gateway NAME --root DIR` with the
# file $STUB_ENDPOINTS/NAME, and refuses every gateway while $STUB_ENDPOINTS/fail
# exists, as the real command does for a gateway no sandbox profile names. Its
# argv is logged to $STUB_GT_CALLS, so the cases can see which gateway was asked.
#   --render FILE [--from-export EXPORT]  prints render-NAME, then EXPORT's
#       resource_version line when EXPORT is given
#   --check EXPORT  prints "bound" when EXPORT, less its resource_version lines,
#       is render-NAME, and "stale: ..." when it is not; while
#       $STUB_ENDPOINTS/garbled-check exists it prints neither. Whether a profile
#       binds an endpoint is gt's judgement, unit tested in Go; these cases check
#       that the script follows the verdict.
cat >"$TMP/bin/gt" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$STUB_GT_CALLS"
if [ "${1-} ${2-}" != "sandbox endpoint" ]; then
  echo "stub gt: unhandled $*" >&2
  exit 1
fi
shift 2
gateway="" render="" export="" check=""
while [ $# -gt 0 ]; do
  case "$1" in
    --gateway) gateway=$2 ;;
    --render) render=$2 ;;
    --from-export) export=$2 ;;
    --check) check=$2 ;;
  esac
  shift 2
done
if [ -e "$STUB_ENDPOINTS/fail" ]; then
  echo "Error: sandbox endpoint: no sandbox profile names gateway \"$gateway\"" >&2
  exit 1
fi
if [ -n "$render" ]; then
  cat "$STUB_ENDPOINTS/render-$gateway"
  if [ -n "$export" ]; then grep '^resource_version:' "$export" | head -n 1; fi
elif [ -n "$check" ]; then
  if [ -e "$STUB_ENDPOINTS/garbled-check" ]; then
    echo "maybe"
  elif diff -q <(grep -v '^resource_version:' "$check") "$STUB_ENDPOINTS/render-$gateway" >/dev/null; then
    echo "bound"
  else
    echo "stale: the profile binds another endpoint than $gateway's"
  fi
else
  cat "$STUB_ENDPOINTS/$gateway"
fi
STUB
chmod +x "$TMP/bin/gt"

# A mktemp that logs each call and then does the real one, so a case can tell
# whether the script made a scratch dir.
REAL_MKTEMP="$(command -v mktemp)"
cat >"$TMP/bin/mktemp" <<STUB
#!/usr/bin/env bash
echo "\$*" >>"\$STUB_MKTEMP_LOG"
exec "$REAL_MKTEMP" "\$@"
STUB
chmod +x "$TMP/bin/mktemp"

# run ARGS...: execute the script against the stub; sets RC and OUT. Calls are
# recorded to $CALLS_LOG, so the gateway cases can keep their calls apart from
# the default gateway's; the stub appends, so a count spans whole scenarios.
CALLS_LOG="$TMP/calls.log"
run() {
  set +e
  OUT="$(PATH="$TMP/bin:$PATH" GT_CONFIG_DIR="$TMP/config" STUB_MKTEMP_LOG="$TMP/mktemp.log" \
    STUB_CALLS="$CALLS_LOG" STUB_STATE="$TMP/state" \
    STUB_GT_CALLS="$GT_CALLS" STUB_ENDPOINTS="$TMP/endpoints" \
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
# Three profiles, each exactly once: a wrong or extra import changes one of these
# counts, which the total alone would hide.
check "imports the anthropic-oauth profile once" [ "$(calls '^profile import -f .*anthropic-oauth.yaml')" -eq 1 ]
check "imports the deepseek-claude profile once" [ "$(calls '^profile import -f .*deepseek-claude.yaml')" -eq 1 ]
check "imports the forgejo-polecat profile once" [ "$(calls '^profile import -f .*forgejo-polecat.yaml')" -eq 1 ]
check "imports exactly three profiles" [ "$(calls '^profile import')" -eq 3 ]
check "creates both providers" [ "$(calls '^provider create')" -eq 2 ]
check "led the user to the imported profiles" contains "imported profile deepseek-claude" "$OUT"
check "never printed the deepseek key" lacks "$DEEPSEEK_KEY" "$OUT"
check "never printed the forgejo token" lacks "$FORGEJO_KEY" "$OUT"
check "masked the credential the provider create echoed" contains "value=***" "$OUT"
check "asked gt for the default gateway's endpoint" [ "$(grep -c '^sandbox endpoint --gateway openshell --root ' "$GT_CALLS")" -ge 1 ]
check "asked gt to render the forgejo profile" [ "$(grep -c '^sandbox endpoint --gateway openshell --root .* --render .*forgejo-polecat.yaml$' "$GT_CALLS")" -eq 1 ]
check "made its scratch dir through the mktemp stub, so the argument cases can see it" [ -s "$TMP/mktemp.log" ]
check "imported the Docker forgejo profile byte for byte" cmp -s "$TMP/state/content-forgejo-polecat" "$REPO_ROOT/deploy/polecat/profiles/forgejo-polecat.yaml"

echo "rerun: both profiles and both providers already present"
run
assert_rc 0 "exits 0 on a rerun"
check "reported the existing profiles" contains "profile deepseek-claude exists" "$OUT"
check "did not call profile import again" [ "$(calls '^profile import')" -eq 3 ]
check "did not call provider create again" [ "$(calls '^provider create')" -eq 2 ]

echo "third run in a row is also clean"
run
assert_rc 0 "exits 0 the third time"
check "still no new import" [ "$(calls '^profile import')" -eq 3 ]
check "still no new provider create" [ "$(calls '^provider create')" -eq 2 ]

echo "partial state: profiles imported, providers missing"
: >"$TMP/state/providers"
run
assert_rc 0 "exits 0"
check "left the profiles alone" [ "$(calls '^profile import')" -eq 3 ]
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
check "the gateway run made calls" [ -s "$CALLS_LOG" ]
check "every call named the gateway" [ "$(grep -c -v '^-g k8s ' "$CALLS_LOG" || true)" -eq 0 ]
check "imported the profiles on the gateway" [ "$(calls '^-g k8s profile import')" -ge 2 ]
check "created both providers on the gateway" [ "$(calls '^-g k8s provider create')" -eq 2 ]
check "never printed the deepseek key" lacks "$DEEPSEEK_KEY" "$OUT"
check "never printed the forgejo token" lacks "$FORGEJO_KEY" "$OUT"
check "masked the credential the provider create echoed" contains "value=***" "$OUT"
check "asked gt for the k8s gateway's endpoint" [ "$(grep -c '^sandbox endpoint --gateway k8s ' "$GT_CALLS")" -ge 1 ]
forgejo="$TMP/state/content-forgejo-polecat"
check "the k8s forgejo profile names the Service host" grep -qxF '  - host: control-plane.openshell.svc.cluster.local' "$forgejo"
check "the k8s forgejo profile names port 3000" grep -qxF '    port: 3000' "$forgejo"
check "the k8s forgejo profile allows the control plane address" grep -qxF '    allowed_ips: ["192.168.5.2/32"]' "$forgejo"
check "the k8s forgejo profile no longer names the Docker alias as its host" [ "$(grep -cxF '  - host: host.docker.internal' "$forgejo")" -eq 0 ]

echo "rerun on the named gateway"
imports_before="$(calls '^-g k8s profile import')"
run -g k8s
assert_rc 0 "exits 0 on a rerun"
check "reported the existing providers" contains "provider deepseek exists" "$OUT"
check "did not call profile import again" [ "$(calls '^-g k8s profile import')" -eq "$imports_before" ]
check "did not call provider create again" [ "$(calls '^-g k8s provider create')" -eq 2 ]

echo "long forms: --gateway=NAME and --gateway NAME name the gateway the same way"
for form in "--gateway=k8s" "--gateway k8s"; do
  read -r -a argv <<<"$form"
  : >"$TMP/state/profiles"
  : >"$TMP/state/providers" # an empty gateway of its own
  CALLS_LOG="$TMP/long-calls.log"
  : >"$CALLS_LOG"
  run "${argv[@]}"
  assert_rc 0 "$form exits 0"
  check "$form made calls" [ -s "$CALLS_LOG" ]
  check "$form: every call named the gateway" [ "$(grep -c -v '^-g k8s ' "$CALLS_LOG" || true)" -eq 0 ]
  check "$form: created both providers on the gateway" [ "$(calls '^-g k8s provider create')" -eq 2 ]
  check "$form: never printed the deepseek key" lacks "$DEEPSEEK_KEY" "$OUT"
  check "$form: masked the credential the provider create echoed" contains "value=***" "$OUT"
  run "${argv[@]}"
  assert_rc 0 "$form exits 0 on a rerun"
  check "$form: reported the existing providers" contains "provider deepseek exists" "$OUT"
  check "$form: did not call provider create again" [ "$(calls '^-g k8s provider create')" -eq 2 ]
done

echo "the endpoint is what gt prints: a different answer changes the profile"
set_endpoint k8s gt-chosen.example.test 4000 10.9.8.7/32 10.9.8.8/32
: >"$TMP/state/profiles"
: >"$TMP/state/providers"
CALLS_LOG="$TMP/chosen-calls.log"
: >"$CALLS_LOG"
run -g k8s
assert_rc 0 "exits 0"
forgejo="$TMP/state/content-forgejo-polecat"
check "the profile names the host gt printed" grep -qxF '  - host: gt-chosen.example.test' "$forgejo"
check "the profile names the port gt printed" grep -qxF '    port: 4000' "$forgejo"
check "the profile names each allowed address gt printed" grep -qxF '    allowed_ips: ["10.9.8.7/32","10.9.8.8/32"]' "$forgejo"
set_endpoint k8s control-plane.openshell.svc.cluster.local 3000 192.168.5.2/32

echo "rerun: the gateway's forgejo profile binds the Docker host, so it is updated"
printf 'forgejo-polecat\ndeepseek-claude\n' >"$TMP/state/profiles"
cp "$REPO_ROOT/deploy/polecat/profiles/forgejo-polecat.yaml" "$TMP/state/content-forgejo-polecat"
: >"$TMP/state/providers"
CALLS_LOG="$TMP/update-calls.log"
: >"$CALLS_LOG"
run -g k8s
assert_rc 0 "exits 0"
check "did not report the wrong-host profile as existing" lacks "profile forgejo-polecat exists" "$OUT"
check "said the profile is stale, with gt's reason" contains "profile forgejo-polecat is stale (the profile binds another endpoint than k8s's), updating it" "$OUT"
check "asked gt to check the exported profile" [ "$(grep -c '^sandbox endpoint --gateway k8s --root .* --check .*forgejo-polecat.export.yaml$' "$GT_CALLS")" -ge 1 ]
check "asked gt to render with the export's version" [ "$(grep -c -- '--render .*forgejo-polecat.yaml --from-export .*forgejo-polecat.export.yaml$' "$GT_CALLS")" -eq 1 ]
check "called profile update on the gateway" [ "$(calls '^-g k8s profile update --global -f .*forgejo-polecat.yaml forgejo-polecat$')" -eq 1 ]
check "did not import the forgejo profile again" [ "$(calls '^-g k8s profile import -f .*forgejo-polecat')" -eq 0 ]
check "the update carries the gateway's current resource_version" grep -qxF 'resource_version: 7' "$TMP/state/content-forgejo-polecat"
check "the version appears once" [ "$(grep -c '^resource_version:' "$TMP/state/content-forgejo-polecat")" -eq 1 ]
check "the gateway's profile now names the k8s host" grep -qxF '  - host: control-plane.openshell.svc.cluster.local' "$TMP/state/content-forgejo-polecat"
run -g k8s
assert_rc 0 "exits 0 on a rerun"
check "the rerun reports the updated profile as existing" contains "profile forgejo-polecat exists" "$OUT"
check "the rerun does not update it again" [ "$(calls '^-g k8s profile update')" -eq 1 ]

echo "the same host with other allowed_ips is stale too: gt judges the whole endpoint"
printf 'forgejo-polecat\ndeepseek-claude\n' >"$TMP/state/profiles"
sed 's#192.168.5.2/32#192.168.5.20/32#' "$TMP/endpoints/render-k8s" >"$TMP/state/content-forgejo-polecat"
CALLS_LOG="$TMP/ips-calls.log"
: >"$CALLS_LOG"
run -g k8s
assert_rc 0 "exits 0"
check "updated the profile" [ "$(calls '^-g k8s profile update')" -eq 1 ]
check "the gateway's profile now allows the control plane address" grep -qxF '    allowed_ips: ["192.168.5.2/32"]' "$TMP/state/content-forgejo-polecat"

echo "a verdict that is neither bound nor stale stops the run: it is not read as bound"
touch "$TMP/endpoints/garbled-check"
printf 'forgejo-polecat\ndeepseek-claude\n' >"$TMP/state/profiles"
cp "$REPO_ROOT/deploy/polecat/profiles/forgejo-polecat.yaml" "$TMP/state/content-forgejo-polecat"
CALLS_LOG="$TMP/garbled-calls.log"
: >"$CALLS_LOG"
run -g k8s
assert_rc 1 "exits 1"
check "named the answer it could not read" contains "answered neither bound nor stale for forgejo-polecat: maybe" "$OUT"
check "did not update the profile" [ "$(calls 'profile update')" -eq 0 ]
rm -f "$TMP/endpoints/garbled-check"

echo "a failed export stops the run: whether the profile binds is unknown"
printf 'forgejo-polecat\ndeepseek-claude\n' >"$TMP/state/profiles"
rm -f "$TMP/state/content-forgejo-polecat"
CALLS_LOG="$TMP/export-calls.log"
: >"$CALLS_LOG"
run -g k8s
assert_rc 1 "exits 1"
check "named the failed export" contains "profile export --global forgejo-polecat failed" "$OUT"
check "passed on the gateway's error" contains "no custom profile forgejo-polecat" "$OUT"
check "did not update the profile" [ "$(calls 'profile update')" -eq 0 ]

echo "gt refuses the gateway: nothing is imported, updated or created"
touch "$TMP/endpoints/fail"
: >"$TMP/state/profiles"
: >"$TMP/state/providers"
CALLS_LOG="$TMP/refused-calls.log"
: >"$CALLS_LOG"
run -g nope
assert_rc 1 "exits 1"
check "named the gateway gt refused" contains "gt sandbox endpoint --gateway nope failed" "$OUT"
check "passed on gt's reason" contains 'no sandbox profile names gateway "nope"' "$OUT"
check "called openshell for nothing" [ ! -s "$CALLS_LOG" ]
rm -f "$TMP/endpoints/fail"

echo "argument checking stops before any openshell call or scratch dir"
CALLS_LOG="$TMP/arg-calls.log"
: >"$CALLS_LOG"
: >"$TMP/mktemp.log"
run -g
assert_rc 2 "a -g with no name exits 2"
check "named the missing gateway name" contains "-g needs a gateway name" "$OUT"
run --gateway
assert_rc 2 "a --gateway with no name exits 2"
check "named the missing long-form gateway name" contains "--gateway needs a gateway name" "$OUT"
run --gateway=
assert_rc 2 "an empty --gateway= exits 2"
check "named the empty long-form gateway name" contains "--gateway needs a gateway name" "$OUT"
run --bogus
assert_rc 2 "an unknown argument exits 2"
check "named the unknown argument" contains "unknown argument: --bogus" "$OUT"
check "called openshell for neither" [ ! -s "$CALLS_LOG" ]
check "made no scratch dir for any bad argument" [ ! -s "$TMP/mktemp.log" ]

echo
if [ "$FAIL" -ne 0 ]; then
  echo "openshell-providers_test: $FAIL failed, $PASS passed"
  exit 1
fi
echo "openshell-providers_test: all $PASS passed"
