#!/usr/bin/env bash
# Tests for scripts/forgejo-cutover.sh (gt-fn9e6.12). A stub town in a temp
# directory — a bare repository, a mayor clone and a crew clone with recorded
# remotes, a town.json and a settings/config.json — plus stub `curl`, `git`,
# `bd`, `gt` and probe scripts on PATH stand in for the operator's machine and
# the live Forgejo. HOME points inside the temp dir, so nothing the script
# writes can land in the real ~.
#
# The cases cover the two refusals (a landing in flight, a probe that is not
# green), a full --dry-run that writes nothing, and a real cutover that writes
# the block, backs up both config files, repoints every remote and restarts the
# daemon.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CUTOVER="$SCRIPT_DIR/forgejo-cutover.sh"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/forgejo-cutover-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT
PASS=0
FAIL=0

STUB_ADMIN_TOKEN="stub-admin-token"
API="http://forgejo.test/api/v1"
WEB="http://forgejo.test"
FORGEJO_URL="$WEB/acme/rig.git"
GITHUB_URL="git@github.com:acme/rig.git"
TOWN="$TMP/town"
RIG_ROOT="$TOWN/acme"
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

mkdir -p "$TMP/bin" "$TMP/home" "$CFG" "$STATE" "$TOWN/mayor" "$RIG_ROOT/settings" \
  "$RIG_ROOT/.repo.git" "$RIG_ROOT/mayor/rig/.git" "$RIG_ROOT/crew/sloan/.git" "$RIG_ROOT/crew/notes"
printf 'FORGEJO_ADMIN_TOKEN=%s\n' "$STUB_ADMIN_TOKEN" > "$TMP/admin.env"

# The probe the cutover runs: it records its call and exits with the code in
# $STUB_STATE/probe-rc.
cat >"$TMP/probe.sh" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
printf '%s\n' "$*" >> "$STUB_STATE/probe.log"
exit "$(cat "$STUB_STATE/probe-rc" 2>/dev/null || echo 0)"
STUB
chmod +x "$TMP/probe.sh"

# The provisioner the cutover runs: it records its call and exits with the code
# in $STUB_STATE/provision-rc.
cat >"$TMP/provision.sh" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
printf '%s\n' "$*" >> "$STUB_STATE/provision.log"
exit "$(cat "$STUB_STATE/provision-rc" 2>/dev/null || echo 0)"
STUB
chmod +x "$TMP/provision.sh"

# The stub git: remotes live in files named for the directory, refs in files
# named for the URL, and an import (fetch then push) copies the fetched refs
# into the pushed URL's file, so the comparison after the push is a real one.
cat >"$TMP/bin/git" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
printf '%s\n' "$*" >> "$STUB_STATE/git.log"

url_of() { # url_of ARGS... prints the first argument that is a remote URL
  local a
  for a in "$@"; do
    case "$a" in
      http://*|https://*|git@*|ssh://*) printf '%s' "$a"; return 0 ;;
    esac
  done
  return 1
}
key_of() { printf '%s' "$1" | tr '/:@.' '_'; }

dir=""
if [ "${1:-}" = "-C" ]; then dir=$2; shift 2; fi
sub=${1:-}
case "$sub" in
  rev-parse)
    grep -Fxq "$dir" "$STUB_STATE/repos" && exit 0
    exit 1 ;;
  remote)
    key="remote_$(key_of "$dir")"
    case "${2:-}" in
      get-url) cat "$STUB_STATE/$key" 2>/dev/null || exit 1 ;;
      set-url)
        printf '%s\n' "${4:-}" > "$STUB_STATE/$key"
        printf '%s %s\n' "$dir" "${4:-}" >> "$STUB_STATE/set-urls" ;;
    esac ;;
  ls-remote)
    url=$(url_of "$@" || true)
    cat "$STUB_STATE/refs_$(key_of "$url")" 2>/dev/null || true ;;
  init) mkdir -p "${!#}" ;;
  fetch)
    url=$(url_of "$@" || true)
    printf '%s' "$url" > "$STUB_STATE/fetched-url"
    cat "$STUB_STATE/refs_$(key_of "$url")" 2>/dev/null > "$STUB_STATE/fetched-refs" || : > "$STUB_STATE/fetched-refs" ;;
  push)
    url=$(url_of "$@" || true)
    printf '%s\n' "$url" >> "$STUB_STATE/pushes"
    cp "$STUB_STATE/fetched-refs" "$STUB_STATE/refs_$(key_of "$url")" 2>/dev/null || true ;;
esac
STUB
chmod +x "$TMP/bin/git"

# The stub curl: one Forgejo instance in a directory. It records every call and
# a violation when the admin token reaches its argv or a call carries no curl
# config file.
cat >"$TMP/bin/curl" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
orig_args=("$@")
method=GET
resp=/dev/null
url=""
config=""
body=""
want_status=0
while [ $# -gt 0 ]; do
  case "$1" in
    -sS) ;;
    -o) resp=$2; shift ;;
    -w) want_status=1 ;;
    -X) method=$2; shift ;;
    -K) config=$2; shift ;;
    --data) body=$2; shift ;;
    -H) shift ;;
    -*) ;;
    *) url=$1 ;;
  esac
  shift
done
path=${url#"$STUB_API"}
path=${path%%\?*}
printf '%s %s\n' "$method" "$path" >> "$STUB_STATE/calls.log"
for arg in "${orig_args[@]}"; do
  case "$arg" in
    *"$STUB_ADMIN_TOKEN"*) echo "admin token in argv: $arg" >> "$STUB_STATE/argv-violations" ;;
  esac
done
[ -n "$config" ] || echo "no curl config file on $method $path" >> "$STUB_STATE/argv-violations"

send() { printf '%s' "$2" > "$resp"; if [ "$want_status" = 1 ]; then printf '%s' "$1"; fi; }

case "$method $path" in
  "GET /repos/acme/rig/push_mirrors")
    if [ -n "${STUB_MIRROR_ABSENT:-}" ]; then
      send 200 '[]'
    else
      send 200 "[{\"remote_name\":\"mirror-acme\",\"remote_address\":\"$STUB_MIRROR_TARGET\",\"branch_filter\":\"\",\"sync_on_commit\":true,\"interval\":\"10m\",\"public_key\":\"ssh-ed25519 AAAAMIRRORKEY forgejo-mirror-acme\"}]"
    fi ;;
  "POST /repos/acme/rig/push_mirrors")
    printf '%s' "$body" > "$STUB_STATE/mirror-create-body"
    send 201 "{\"remote_name\":\"mirror-acme\",\"remote_address\":\"$STUB_MIRROR_TARGET\",\"use_ssh\":true,\"branch_filter\":\"\",\"sync_on_commit\":true,\"interval\":\"10m\",\"public_key\":\"ssh-ed25519 AAAAMIRRORKEY forgejo-mirror-acme\"}" ;;
  "DELETE /repos/acme/rig/push_mirrors/mirror-acme")
    printf 'deleted\n' >> "$STUB_STATE/mirrors-deleted"
    send 204 '' ;;
  *)
    send 404 '{"message":"unhandled stub route"}' ;;
esac
STUB
chmod +x "$TMP/bin/curl"

# The stub bd answers the rig's landing-queue reads from a plan file, one count
# per line with the last repeating, so a case can make the queue empty at the
# cutover's first check and busy at its restart check.
cat >"$TMP/bin/bd" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
printf '%s\n' "$*" >> "$STUB_STATE/bd.log"
[ -z "${STUB_BD_FAIL:-}" ] || exit 1
n=$(cat "$STUB_STATE/bd-calls" 2>/dev/null || echo 0)
n=$((n + 1))
printf '%s' "$n" > "$STUB_STATE/bd-calls"
line=$(sed -n "${n}p" "$STUB_STATE/landing-plan")
[ -n "$line" ] || line=$(tail -1 "$STUB_STATE/landing-plan")
printf '%s\n' "${line:-0}"
STUB
chmod +x "$TMP/bin/bd"

# The stub gt records the daemon restart.
cat >"$TMP/bin/gt" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
printf '%s\n' "$*" >> "$STUB_STATE/gt.log"
STUB
chmod +x "$TMP/bin/gt"

refs_key() { printf '%s' "$1" | tr '/:@.' '_'; }

# fresh resets the stub state and rebuilds the rig's files, so each case starts
# from the same pre-cutover world.
fresh() {
  unset STUB_MIRROR_ABSENT STUB_BD_FAIL
  rm -rf "$STATE" "$RIG_ROOT/settings/config.json" "$RIG_ROOT/settings/"*.bak-* 2>/dev/null
  mkdir -p "$STATE" "$RIG_ROOT/settings"
  : > "$STATE/calls.log"
  : > "$STATE/git.log"
  : > "$STATE/bd.log"
  : > "$STATE/gt.log"
  : > "$STATE/probe.log"
  printf '0\n' > "$STATE/landing-plan"
  : > "$STATE/bd-calls"
  printf '0\n' > "$STATE/probe-rc"
  printf '%s\n%s\n%s\n' \
    "$RIG_ROOT/.repo.git" "$RIG_ROOT/mayor/rig" "$RIG_ROOT/crew/sloan" > "$STATE/repos"
  printf '%s\n' "$GITHUB_URL" > "$STATE/remote_$(printf '%s' "$RIG_ROOT/.repo.git" | tr '/:@.' '_')"
  printf '%s\n' "$GITHUB_URL" > "$STATE/remote_$(printf '%s' "$RIG_ROOT/mayor/rig" | tr '/:@.' '_')"
  printf '%s\n' "$GITHUB_URL" > "$STATE/remote_$(printf '%s' "$RIG_ROOT/crew/sloan" | tr '/:@.' '_')"
  printf 'refs/heads/main abc123\nrefs/heads/feature def456\nrefs/tags/v1 789abc\n' > "$STATE/refs_$(refs_key "$GITHUB_URL")"
  printf 'refs/heads/main abc123\n' > "$STATE/refs_$(refs_key "$FORGEJO_URL")"
  cat > "$RIG_ROOT/settings/config.json" <<'JSON'
{
  "type": "rig-settings",
  "version": 1,
  "merge_queue": {
    "gate": "make gate"
  }
}
JSON
  cat > "$TOWN/mayor/town.json" <<'JSON'
{
  "type": "town",
  "version": 2,
  "registry": {
    "version": 1,
    "rigs": {
      "acme": {
        "added_at": "2026-10-01T00:00:00Z",
        "dolt_database": "acme",
        "git_url": "git@github.com:acme/rig.git"
      }
    }
  }
}
JSON
}

# run_cutover [ARGS...] runs the script against the stubs, prints its combined
# output, and returns its exit code.
run_cutover() {
  local out rc
  out=$(env -u FORGEJO_ADMIN_TOKEN -u FORGEJO_API_URL -u FORGEJO_URL \
    HOME="$TMP/home" XDG_CONFIG_HOME="$CFG" PATH="$TMP/bin:$PATH" \
    STUB_STATE="$STATE" STUB_API="$API" STUB_MIRROR_TARGET="$GITHUB_URL" \
    STUB_ADMIN_TOKEN="$STUB_ADMIN_TOKEN" STUB_MIRROR_ABSENT="${STUB_MIRROR_ABSENT:-}" \
    STUB_BD_FAIL="${STUB_BD_FAIL:-}" \
    bash "$CUTOVER" acme --town-root "$TOWN" --repo acme/rig \
    --api-url "$API" --web-url "$WEB" \
    --admin-token-file "$TMP/admin.env" --probe "$TMP/probe.sh" \
    --provision "$TMP/provision.sh" --gt "$TMP/bin/gt" "$@" 2>&1)
  rc=$?
  printf '%s' "$out"
  return "$rc"
}

calls() { cat "$STATE/calls.log" 2>/dev/null || true; }
count_calls() { # count_calls METHOD
  local n=0 line
  while IFS= read -r line; do
    case "$line" in "$1 "*) n=$((n + 1)) ;; esac
  done <<<"$(calls)"
  printf '%s' "$n"
}
backups() { find "$RIG_ROOT" "$TOWN/mayor" -name '*.bak-*' 2>/dev/null | sort; }

echo "=== a landing in flight refuses the cutover ==="
fresh
printf '2\n' > "$STATE/landing-plan"
out=$(run_cutover); rc=$?
if [ "$rc" != 0 ]; then pass "a queued landing exits non-zero"; else fail "a queued landing exits non-zero" "$out"; fi
check "the refusal names the queued landings" contains "2 open gt:ready-to-land" "$out"
check "the probe never runs" [ ! -s "$STATE/probe.log" ]
check "nothing is written" [ -z "$(backups)" ]

echo "=== a probe that is not green refuses the cutover ==="
fresh
printf '1\n' > "$STATE/probe-rc"
out=$(run_cutover); rc=$?
if [ "$rc" != 0 ]; then pass "a red probe exits non-zero"; else fail "a red probe exits non-zero" "$out"; fi
check "the refusal says the probe is not green" contains "is not green" "$out"
check "the probe was run" [ -s "$STATE/probe.log" ]
check "no mirror is created" [ ! -f "$STATE/mirror-create-body" ]

echo "=== an unreadable landing queue refuses the cutover ==="
fresh
STUB_BD_FAIL=1
out=$(run_cutover); rc=$?
if [ "$rc" != 0 ]; then pass "an unreadable queue exits non-zero"; else fail "an unreadable queue exits non-zero" "$out"; fi
check "the refusal says the queue could not be read" contains "could not read the landing queue" "$out"
check "the probe never runs" [ ! -s "$STATE/probe.log" ]

echo "=== --dry-run prints every command and writes nothing ==="
fresh
STUB_MIRROR_ABSENT=1
before_settings=$(cat "$RIG_ROOT/settings/config.json")
before_town=$(cat "$TOWN/mayor/town.json")
before_refs=$(cat "$STATE/refs_$(refs_key "$FORGEJO_URL")")
out=$(run_cutover --dry-run); rc=$?
if [ "$rc" = 0 ]; then pass "a dry run exits 0"; else fail "a dry run exits 0 (rc=$rc)" "$out"; fi
check "it prints the probe command" contains "probe.sh acme --repo acme/rig" "$out"
check "it prints the ref import" contains "ls-remote" "$out"
check "it prints the push mirror it would create" contains "dry run: not sent" "$out"
check "it prints the daemon restart" contains "daemon restart" "$out"
check "it names the github Actions reminder" contains "actions/permissions -f enabled=false" "$out"
check "the settings file is byte-identical" [ "$(cat "$RIG_ROOT/settings/config.json")" = "$before_settings" ]
check "town.json is byte-identical" [ "$(cat "$TOWN/mayor/town.json")" = "$before_town" ]
check "no config backup is left" [ -z "$(backups)" ]
check "the ref import changed nothing" [ "$(cat "$STATE/refs_$(refs_key "$FORGEJO_URL")")" = "$before_refs" ]
check "no remote was set" lacks "set-url" "$(cat "$STATE/git.log")"
check "no write call was sent" [ "$(count_calls POST)" = 0 ]
check "the daemon was not restarted" [ ! -s "$STATE/gt.log" ]

echo "=== a real cutover converges the rig ==="
fresh
printf 'refs/heads/main abc123\n' > "$STATE/refs_$(refs_key "$FORGEJO_URL")"
STUB_MIRROR_ABSENT=1
out=$(run_cutover); rc=$?
if [ "$rc" = 0 ]; then pass "the cutover exits 0"; else fail "the cutover exits 0 (rc=$rc)" "$out"; fi
check "the probe ran for the rig" contains "acme --repo acme/rig" "$(cat "$STATE/probe.log")"
check "every GitHub ref was imported" contains "imported every GitHub ref" "$out"
check "the push mirror was created without a branch filter" contains '"branch_filter":""' "$(cat "$STATE/mirror-create-body" 2>/dev/null)"
check "the mirror enables ssh" contains '"use_ssh":true' "$(cat "$STATE/mirror-create-body" 2>/dev/null)"
check "the gh deploy-key command is printed" contains "gh repo deploy-key add" "$out"
check "the deploy key is the mirror's" contains "AAAAMIRRORKEY" "$out"
check "the bare repo is repointed" contains "$RIG_ROOT/.repo.git $FORGEJO_URL" "$(cat "$STATE/set-urls")"
check "the mayor clone is repointed" contains "$RIG_ROOT/mayor/rig $FORGEJO_URL" "$(cat "$STATE/set-urls")"
check "the crew clone is repointed" contains "$RIG_ROOT/crew/sloan $FORGEJO_URL" "$(cat "$STATE/set-urls")"
check "a non-repo crew dir is skipped" contains "skip $RIG_ROOT/crew/notes" "$out"
check "settings/config.json names the Forgejo remote" contains "$FORGEJO_URL" "$(cat "$RIG_ROOT/settings/config.json")"
check "settings/config.json names the gate workflow" contains '"gate_workflow": "gate"' "$(cat "$RIG_ROOT/settings/config.json")"
check "settings/config.json names the three bots" contains '"landing": "bot-landing"' "$(cat "$RIG_ROOT/settings/config.json")"
check "settings/config.json names the mirror target" contains "\"mirror_target\": \"$GITHUB_URL\"" "$(cat "$RIG_ROOT/settings/config.json")"
check "town.json git_url is repointed" contains "\"git_url\": \"$FORGEJO_URL\"" "$(cat "$TOWN/mayor/town.json")"
check "the settings file is backed up" contains "config.json.bak-" "$(backups)"
check "town.json is backed up" contains "town.json.bak-" "$(backups)"
check "the daemon was restarted" contains "daemon restart" "$(cat "$STATE/gt.log")"
check "no token reached argv" [ ! -f "$STATE/argv-violations" ]

echo "=== a second cutover over converged state changes nothing ==="
unset STUB_MIRROR_ABSENT
starts_before=$(cat "$STATE/set-urls" | grep -c 'set-url')
backups_before=$(backups)
town_before=$(cat "$TOWN/mayor/town.json")
out=$(run_cutover); rc=$?
if [ "$rc" = 0 ]; then pass "the second cutover exits 0"; else fail "the second cutover exits 0 (rc=$rc)" "$out"; fi
check "the mirror is left alone" contains "already exists" "$out"
check "the remotes are reported already right" contains "origin is already" "$out"
check "no remote is set again" [ "$(cat "$STATE/set-urls" | grep -c 'set-url')" = "$starts_before" ]
check "town.json is unchanged" [ "$(cat "$TOWN/mayor/town.json")" = "$town_before" ]
check "no second backup is written" [ "$(backups)" = "$backups_before" ]
check "the block is reported already matching" contains "merge_queue.forgejo already matches" "$out"

echo "=== a landing queued at the restart is left to the operator ==="
fresh
STUB_MIRROR_ABSENT=1
printf '0\n1\n' > "$STATE/landing-plan"
out=$(run_cutover); rc=$?
if [ "$rc" = 0 ]; then pass "the cutover still exits 0"; else fail "the cutover still exits 0 (rc=$rc)" "$out"; fi
check "it says the landing queue is not empty" contains "1 open gt:ready-to-land" "$out"
check "the daemon was not restarted" [ ! -s "$STATE/gt.log" ]
check "it prints the restart command for later" contains "gt daemon restart" "$out"
check "the rig is still cut over" contains "\"remote_url\": \"$FORGEJO_URL\"" "$(cat "$RIG_ROOT/settings/config.json")"

echo
echo "forgejo-cutover_test: passed=$PASS failed=$FAIL"
[ "$FAIL" = 0 ]
