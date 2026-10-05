#!/usr/bin/env bash
# Tests for scripts/forgejo-rollback.sh (gt-fn9e6.12). A stub town in a temp
# directory starts cut over — a merge_queue.forgejo block, remotes at the
# Forgejo URL, a town.json git_url at the Forgejo URL — and stub `curl`, `git`,
# `bd` and `gt` on PATH stand in for the live Forgejo and the operator's
# machine. HOME points inside the temp dir, so nothing the script writes can
# land in the real ~.
#
# The cases cover the refusal (nothing to roll back and no --github-url), a
# --dry-run that writes nothing, a real rollback that stops the mirror first,
# removes the block, repoints everything and restarts the daemon, a queued
# landing that suppresses the restart, and a second run over converged state.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROLLBACK="$SCRIPT_DIR/forgejo-rollback.sh"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/forgejo-rollback-test.XXXXXX")"
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

# The stub git records its calls, keeps a remote URL per directory, and appends
# to the shared order log so a test can see the mirror stop before the first
# repoint.
cat >"$TMP/bin/git" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
printf '%s\n' "$*" >> "$STUB_STATE/git.log"

url_of() {
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
        printf '%s %s\n' "$dir" "${4:-}" >> "$STUB_STATE/set-urls"
        printf 'set-url %s\n' "$dir" >> "$STUB_STATE/order.log" ;;
    esac ;;
  ls-remote)
    url=$(url_of "$@" || true)
    cat "$STUB_STATE/refs_$(key_of "$url")" 2>/dev/null || true ;;
esac
STUB
chmod +x "$TMP/bin/git"

# The stub curl: one Forgejo instance in a directory, and an order log shared
# with the stub git.
cat >"$TMP/bin/curl" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
orig_args=("$@")
method=GET
resp=/dev/null
url=""
config=""
want_status=0
while [ $# -gt 0 ]; do
  case "$1" in
    -sS) ;;
    -o) resp=$2; shift ;;
    -w) want_status=1 ;;
    -X) method=$2; shift ;;
    -K) config=$2; shift ;;
    -H) shift ;;
    -*) ;;
    *) url=$1 ;;
  esac
  shift
done
path=${url#"$STUB_API"}
path=${path%%\?*}
printf '%s %s\n' "$method" "$path" >> "$STUB_STATE/calls.log"
printf '%s %s\n' "$method" "$path" >> "$STUB_STATE/order.log"
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
  "DELETE /repos/acme/rig/push_mirrors/mirror-acme")
    printf 'deleted\n' >> "$STUB_STATE/mirrors-deleted"
    send 204 '' ;;
  *)
    send 404 '{"message":"unhandled stub route"}' ;;
esac
STUB
chmod +x "$TMP/bin/curl"

cat >"$TMP/bin/bd" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
printf '%s\n' "$*" >> "$STUB_STATE/bd.log"
[ -z "${STUB_BD_FAIL:-}" ] || exit 1
cat "$STUB_STATE/landing-count" 2>/dev/null || echo 0
STUB
chmod +x "$TMP/bin/bd"

cat >"$TMP/bin/gt" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
printf '%s\n' "$*" >> "$STUB_STATE/gt.log"
STUB
chmod +x "$TMP/bin/gt"

# fresh resets the stub state and rebuilds a cut-over rig.
fresh() {
  unset STUB_MIRROR_ABSENT
  rm -rf "$STATE"
  mkdir -p "$STATE" "$RIG_ROOT/settings"
  rm -f "$RIG_ROOT/settings/"*.bak-* "$TOWN/mayor/"*.bak-* 2>/dev/null
  : > "$STATE/calls.log"; : > "$STATE/git.log"; : > "$STATE/bd.log"
  : > "$STATE/gt.log"; : > "$STATE/order.log"; : > "$STATE/set-urls"
  printf '0\n' > "$STATE/landing-count"
  printf '%s\n%s\n%s\n' \
    "$RIG_ROOT/.repo.git" "$RIG_ROOT/mayor/rig" "$RIG_ROOT/crew/sloan" > "$STATE/repos"
  local key
  for d in "$RIG_ROOT/.repo.git" "$RIG_ROOT/mayor/rig" "$RIG_ROOT/crew/sloan"; do
    key=$(printf '%s' "$d" | tr '/:@.' '_')
    printf '%s\n' "$FORGEJO_URL" > "$STATE/remote_$key"
  done
  cat > "$RIG_ROOT/settings/config.json" <<JSON
{
  "type": "rig-settings",
  "version": 1,
  "merge_queue": {
    "gate": "make gate",
    "forgejo": {
      "remote_url": "$FORGEJO_URL",
      "gate_workflow": "gate",
      "bots": {
        "polecat": "bot-polecat",
        "landing": "bot-landing",
        "registry": "bot-registry"
      },
      "mirror_target": "$GITHUB_URL"
    }
  }
}
JSON
  cat > "$TOWN/mayor/town.json" <<JSON
{
  "type": "town",
  "version": 2,
  "registry": {
    "version": 1,
    "rigs": {
      "acme": {
        "added_at": "2026-10-01T00:00:00Z",
        "dolt_database": "acme",
        "git_url": "$FORGEJO_URL"
      }
    }
  }
}
JSON
}

# never_cut_over leaves the rig with no block and its remotes on GitHub, as a
# rig that was never cut over (or already rolled back once).
never_cut_over() {
  cat > "$RIG_ROOT/settings/config.json" <<'JSON'
{
  "type": "rig-settings",
  "version": 1,
  "merge_queue": {
    "gate": "make gate"
  }
}
JSON
  local key
  for d in "$RIG_ROOT/.repo.git" "$RIG_ROOT/mayor/rig" "$RIG_ROOT/crew/sloan"; do
    key=$(printf '%s' "$d" | tr '/:@.' '_')
    printf '%s\n' "$GITHUB_URL" > "$STATE/remote_$key"
  done
  cat > "$TOWN/mayor/town.json" <<JSON
{
  "type": "town",
  "version": 2,
  "registry": {
    "version": 1,
    "rigs": {
      "acme": {
        "added_at": "2026-10-01T00:00:00Z",
        "dolt_database": "acme",
        "git_url": "$GITHUB_URL"
      }
    }
  }
}
JSON
}

run_rollback() {
  local out rc
  out=$(env -u FORGEJO_ADMIN_TOKEN -u FORGEJO_API_URL -u FORGEJO_URL \
    HOME="$TMP/home" XDG_CONFIG_HOME="$CFG" PATH="$TMP/bin:$PATH" \
    STUB_STATE="$STATE" STUB_API="$API" STUB_MIRROR_TARGET="$GITHUB_URL" \
    STUB_ADMIN_TOKEN="$STUB_ADMIN_TOKEN" STUB_MIRROR_ABSENT="${STUB_MIRROR_ABSENT:-}" \
    bash "$ROLLBACK" acme --town-root "$TOWN" --api-url "$API" --web-url "$WEB" \
    --admin-token-file "$TMP/admin.env" --gt "$TMP/bin/gt" "$@" 2>&1)
  rc=$?
  printf '%s' "$out"
  return "$rc"
}

calls() { cat "$STATE/calls.log" 2>/dev/null || true; }
backups() { find "$RIG_ROOT" "$TOWN/mayor" -name '*.bak-*' 2>/dev/null | sort; }
# first_line FILE NEEDLE — the line number of the first line holding NEEDLE.
first_line() {
  awk -v n="$1" 'index($0, n) { print NR; exit }' "$2"
}

echo "=== a rig that was never cut over is a no-op ==="
fresh
never_cut_over
out=$(run_rollback); rc=$?
if [ "$rc" = 0 ]; then pass "a rig with no block exits 0"; else fail "a rig with no block exits 0 (rc=$rc)" "$out"; fi
check "it reports that the rig does not look cut over" contains "does not look cut over" "$out"
check "it reports the remotes already right" contains "origin is already" "$out"
check "nothing is written" [ -z "$(backups)" ]

echo "=== --dry-run prints every command and writes nothing ==="
fresh
before_settings=$(cat "$RIG_ROOT/settings/config.json")
before_town=$(cat "$TOWN/mayor/town.json")
out=$(run_rollback --dry-run); rc=$?
if [ "$rc" = 0 ]; then pass "a dry run exits 0"; else fail "a dry run exits 0 (rc=$rc)" "$out"; fi
check "it prints the mirror deletion" contains "DELETE" "$out"
check "it prints the deploy-key removal" contains "gh repo deploy-key delete" "$out"
check "it prints the block removal" contains "would remove merge_queue.forgejo" "$out"
check "it prints the repoint" contains "set-url" "$out"
check "it prints the daemon restart" contains "daemon restart" "$out"
check "the settings file is byte-identical" [ "$(cat "$RIG_ROOT/settings/config.json")" = "$before_settings" ]
check "town.json is byte-identical" [ "$(cat "$TOWN/mayor/town.json")" = "$before_town" ]
check "no config backup is left" [ -z "$(backups)" ]
check "no remote was set" lacks "set-url" "$(cat "$STATE/git.log")"
check "no write call was sent" [ ! -f "$STATE/mirrors-deleted" ]
check "the daemon was not restarted" [ ! -s "$STATE/gt.log" ]

echo "=== a real rollback converges the rig ==="
fresh
out=$(run_rollback); rc=$?
if [ "$rc" = 0 ]; then pass "the rollback exits 0"; else fail "the rollback exits 0 (rc=$rc)" "$out"; fi
check "the mirror is deleted" contains "DELETE /repos/acme/rig/push_mirrors/mirror-acme" "$(calls)"
check "the mirror stop precedes the first repoint" \
  [ "$(first_line 'DELETE' "$STATE/order.log")" -lt "$(first_line 'set-url' "$STATE/order.log")" ]
check "the deploy-key removal is printed" contains "gh repo deploy-key delete" "$out"
check "the block is gone" lacks "forgejo" "$(cat "$RIG_ROOT/settings/config.json")"
check "the settings file is backed up" contains "config.json.bak-" "$(backups)"
check "the bare repo is repointed back" contains "$RIG_ROOT/.repo.git $GITHUB_URL" "$(cat "$STATE/set-urls")"
check "the mayor clone is repointed back" contains "$RIG_ROOT/mayor/rig $GITHUB_URL" "$(cat "$STATE/set-urls")"
check "the crew clone is repointed back" contains "$RIG_ROOT/crew/sloan $GITHUB_URL" "$(cat "$STATE/set-urls")"
check "town.json git_url is repointed back" contains "\"git_url\": \"$GITHUB_URL\"" "$(cat "$TOWN/mayor/town.json")"
check "town.json is backed up" contains "town.json.bak-" "$(backups)"
check "the daemon was restarted" contains "daemon restart" "$(cat "$STATE/gt.log")"
check "no token reached argv" [ ! -f "$STATE/argv-violations" ]

echo "=== a second rollback over converged state changes nothing ==="
starts_before=$(grep -c 'set-url' "$STATE/set-urls")
backups_before=$(backups)
out=$(run_rollback); rc=$?
if [ "$rc" = 0 ]; then pass "the second rollback exits 0"; else fail "the second rollback exits 0 (rc=$rc)" "$out"; fi
check "no block is reported" contains "does not look cut over" "$out"
check "the remotes are reported already right" contains "origin is already" "$out"
check "no remote is set again" [ "$(grep -c 'set-url' "$STATE/set-urls")" = "$starts_before" ]
check "no second backup is written" [ "$(backups)" = "$backups_before" ]

echo "=== a queued landing suppresses the daemon restart ==="
fresh
printf '1\n' > "$STATE/landing-count"
out=$(run_rollback); rc=$?
if [ "$rc" = 0 ]; then pass "a rollback with a queued landing exits 0"; else fail "a rollback with a queued landing exits 0 (rc=$rc)" "$out"; fi
check "it says the landing queue is not empty" contains "1 open gt:ready-to-land" "$out"
check "the daemon was not restarted" [ ! -s "$STATE/gt.log" ]
check "it prints the restart command for later" contains "gt daemon restart" "$out"
check "the mirror was still stopped" contains "DELETE /repos/acme/rig/push_mirrors/mirror-acme" "$(calls)"

echo
echo "forgejo-rollback_test: passed=$PASS failed=$FAIL"
[ "$FAIL" = 0 ]
