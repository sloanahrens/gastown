#!/usr/bin/env bash
# Tests for scripts/forgejo-cutover.sh (gt-fn9e6.12). A stub town in a temp
# directory — a bare repository, a mayor clone and a crew clone with recorded
# remotes, a town.json and a settings/config.json — plus stub `curl`, `git`,
# `bd`, `gt` and probe scripts on PATH stand in for the operator's machine and
# the live Forgejo. HOME points inside the temp dir, so nothing the script
# writes can land in the real ~.
#
# The cases cover the three refusals (a landing in flight, a probe that is not
# green, an unreadable queue), a full --dry-run that writes nothing and leaves
# no backup behind, a default cutover that mints the promote key (mode 600,
# never printed) and writes promote_target and promote_key_file instead of a
# push mirror, a second cutover that reuses the key, --mirror reproducing the
# push mirror, a hostname-form --forgejo-url whose own git work still rides the
# admin base, a landing queued at the restart, and the probe carrying the
# cutover's --town-root, so a rig at a non-default town root still probes through
# the worktree in its own repository, where the rig's hooks run (gt-ck1if).
#
# The import's own safety is its own set (gt-fn9e6.51): a Forgejo ref ahead of
# GitHub's is kept and never rewound, a Forgejo ref behind GitHub's is
# fast-forwarded, a missing ref is created, a diverged ref stops the run (or is
# skipped with --accept-diverged), nothing is ever force-pushed, and --dry-run
# shows every would-push and would-keep line without touching Forgejo.
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
# The admin base the script's own git work rides: the web root plus the repo.
ADMIN_GIT_URL="$WEB/acme/rig.git"
FORGEJO_URL="$ADMIN_GIT_URL"
# The credential-helper hostname form of the same repository: what an operator
# writes into the rig, and what the bots cannot reach before provisioning.
HOSTNAME_GIT_URL="http://forgejo:3000/acme/rig.git"
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

# The stub git: remotes live in files named for the directory, each remote's
# refs in a file named for the URL, and the scratch repository the import builds
# in a file named for its directory. A ref file holds "ref sha" lines; ls-remote
# prints them back the way real git does ("sha<TAB>ref"), so the script's own
# "ref sha" parser is exercised. fetch and push apply their refspecs for real,
# so a push moves only the refs the script asked for, and a push that would not
# fast-forward is rejected unless it carries '+'. A '+' or --force anywhere is
# also recorded as a force violation, so a test can prove the import never
# forces.
#
# $STUB_STATE/commits holds the commit graph the ancestry checks read, one
# "<sha> <parent>" line per commit (a root's parent is empty).
#
# $STUB_FORGEJO_GIT_URLS lists the git URLs the Forgejo instance answers on. A
# Forgejo URL that is not the admin one is served as a bot, and the bots have no
# access to the repository until provisioning has run, so listing it and pushing
# to it fail the way the live instance does (gt-fn9e6.35).
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
# reachable URL returns 1 when URL is a Forgejo git URL the bots cannot reach,
# printing the refusals the live instance sends.
reachable() { # reachable URL
  local u
  for u in ${STUB_FORGEJO_GIT_URLS:-}; do
    [ "$1" = "$u" ] || continue
    [ "$1" = "${STUB_ADMIN_GIT_URL:-}" ] && return 0
    printf 'remote: Repository not found.\n' >&2
    printf 'fatal: repository %s not found\n' "$1" >&2
    return 1
  done
  return 0
}

# is_ancestor A B: A is B, or an ancestor of B in the commits file.
is_ancestor() {
  local a=$1 cur=$2
  [ "$a" = "$cur" ] && return 0
  while [ -n "$cur" ]; do
    cur=$(awk -v c="$cur" '$1 == c { print $2 }' "$STUB_STATE/commits" 2>/dev/null)
    [ "$cur" = "$a" ] && return 0
  done
  return 1
}
reachable_from() { # reachable_from A prints A and its ancestors
  local cur=$1
  while [ -n "$cur" ]; do
    printf '%s\n' "$cur"
    cur=$(awk -v c="$cur" '$1 == c { print $2 }' "$STUB_STATE/commits" 2>/dev/null)
  done
}
rev_count() { # rev_count A B: commits reachable from B and not from A
  local a=$1 x n=0
  while read -r x; do
    is_ancestor "$x" "$a" || n=$((n + 1))
  done <<<"$(reachable_from "$2")"
  printf '%s' "$n"
}
# refspecs ARGS... prints "SRC DST" for every refspec argument, its leading '+'
# stripped. Only a refs/... argument is a refspec: a remote URL holds a ':' too.
refspecs() {
  local a r
  for a in "$@"; do
    case "$a" in
      +refs/*:*) r=${a#+}; printf '%s %s\n' "${r%%:*}" "${r#*:}" ;;
      refs/*:*) printf '%s %s\n' "${a%%:*}" "${a#*:}" ;;
    esac
  done
}
# map_ref SRC DST REF rewrites REF by one refspec, or fails when SRC misses it.
# A single '*' is the only pattern the script's specs use.
map_ref() { # map_ref SRC DST REF
  local src=$1 dst=$2 ref=$3 head
  case "$src" in
    *'*')
      head=${src%%\*}
      case "$ref" in
        "$head"*) printf '%s%s' "${dst%%\*}" "${ref#"$head"}" ;;
        *) return 1 ;;
      esac ;;
    *) [ "$ref" = "$src" ] && printf '%s' "$dst" || return 1 ;;
  esac
}
# apply_specs SOURCE TARGET reads "SRC DST" lines on stdin and rewrites TARGET's
# refs with the matching refs of SOURCE, the last write to a ref winning. A
# change that would not fast-forward is rejected unless it carried '+', which is
# how the stub refuses a force push the way a protected Forgejo branch does.
apply_specs() { # apply_specs SOURCE TARGET
  local source=$1 target=$2 src dst ref sha out cur have
  : > "$target.new"
  [ -f "$target" ] && cat "$target" >> "$target.new"
  have=$(cat "$source" 2>/dev/null || true)
  while read -r src dst; do
    [ -n "$src" ] || continue
    while read -r ref sha; do
      [ -n "$ref" ] || continue
      out=$(map_ref "$src" "$dst" "$ref") || continue
      cur=$(awk -v r="$out" '$1 == r { print $2 }' "$target.new")
      if [ -n "$cur" ] && [ "$cur" != "$sha" ] && ! is_ancestor "$cur" "$sha"; then
        printf 'remote: branch %s is protected from force push (would not fast-forward)\n' "$out" >&2
        exit 1
      fi
      printf '%s %s\n' "$out" "$sha" >> "$target.new"
    done <<<"$have"
  done
  awk '{ a[$1] = $2 } END { for (r in a) print r, a[r] }' "$target.new" | LC_ALL=C sort > "$target"
  rm -f "$target.new"
}

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
    reachable "$url" || exit 128
    awk '{ print $2"\t"$1 }' "$STUB_STATE/refs_$(key_of "$url")" 2>/dev/null || true ;;
  merge-base)
    [ "${2:-}" = "--is-ancestor" ] || exit 129
    is_ancestor "$3" "$4" && exit 0
    exit 1 ;;
  rev-list)
    [ "${2:-}" = "--count" ] || exit 129
    range=${3:-}
    rev_count "${range%%..*}" "${range#*..}"
    exit 0 ;;
  init) mkdir -p "${!#}" ;;
  fetch)
    url=$(url_of "$@" || true)
    reachable "$url" || exit 128
    refspecs "$@" | apply_specs "$STUB_STATE/refs_$(key_of "$url")" "$STUB_STATE/scratch_$(key_of "$dir")" ;;
  push)
    url=$(url_of "$@" || true)
    reachable "$url" || exit 128
    printf '%s\n' "$url" >> "$STUB_STATE/pushes"
    for a in "$@"; do
      case "$a" in
        --force|--force-with-lease*|-f|+refs/*:*) printf '%s\n' "$a" >> "$STUB_STATE/force-violations" ;;
      esac
    done
    refspecs "$@" | apply_specs "$STUB_STATE/scratch_$(key_of "$dir")" "$STUB_STATE/refs_$(key_of "$url")" ;;
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

# The stub ssh-keygen writes a fixed keypair at the -f path, mode 644 so a test
# can prove the script sets the private half to 600 itself, and logs its argv.
# It never invents key material beyond a recognizable marker, so a test can
# assert the private key does not reach stdout.
cat >"$TMP/bin/ssh-keygen" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
printf '%s\n' "$*" >> "$STUB_STATE/ssh-keygen.log"
args=("$@")
file=""
for ((i = 0; i < ${#args[@]}; i++)); do
  case "${args[i]}" in
    -f) file=${args[i + 1]:-} ;;
  esac
done
if [ "${args[0]:-}" = "-y" ]; then
  cat "$STUB_STATE/promote-key-private" 2>/dev/null || exit 1
  exit 0
fi
[ -n "$file" ] || exit 1
printf '%s\n' '-----BEGIN OPENSSH PRIVATE KEY-----' 'STUBPRIVATEKEYMATERIAL' '-----END OPENSSH PRIVATE KEY-----' > "$file"
printf '%s\n' 'ssh-ed25519 AAAASTUBPROMOTEKEY promote-acme' > "$file.pub"
chmod 644 "$file" "$file.pub"
STUB
chmod +x "$TMP/bin/ssh-keygen"

refs_key() { printf '%s' "$1" | tr '/:@.' '_'; }

# fresh resets the stub state and rebuilds the rig's files, so each case starts
# from the same pre-cutover world.
fresh() {
  unset STUB_MIRROR_ABSENT STUB_BD_FAIL STUB_FORGEJO_EXTRA_URL
  rm -rf "$STATE" "$RIG_ROOT/settings/config.json" "$RIG_ROOT/settings/"*.bak-* 2>/dev/null
  rm -f "$CFG/gt/promote-acme.key" "$CFG/gt/promote-acme.key.pub" 2>/dev/null
  mkdir -p "$STATE" "$RIG_ROOT/settings"
  : > "$STATE/calls.log"
  : > "$STATE/git.log"
  : > "$STATE/bd.log"
  : > "$STATE/gt.log"
  : > "$STATE/probe.log"
  : > "$STATE/ssh-keygen.log"
  printf '0\n' > "$STATE/landing-plan"
  : > "$STATE/bd-calls"
  # No commit graph by default: the cases that need ancestry write one.
  : > "$STATE/commits"
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
    "test_command": "make gate"
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
    STUB_ADMIN_GIT_URL="$ADMIN_GIT_URL" \
    STUB_FORGEJO_GIT_URLS="$ADMIN_GIT_URL${STUB_FORGEJO_EXTRA_URL:+ $STUB_FORGEJO_EXTRA_URL}" \
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
# mode_of FILE prints a file's permission bits, GNU stat first then the BSD one,
# so the test runs on Linux and macOS.
mode_of() { stat -c '%a' "$1" 2>/dev/null || stat -f '%Lp' "$1" 2>/dev/null; }
sk_lines() { wc -l < "$STATE/ssh-keygen.log" 2>/dev/null | tr -d ' ' || printf 0; }
PROMOTE_KEY_FILE="$CFG/gt/promote-acme.key"

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
check "the printed probe carries the cutover's town root" contains "--town-root $TOWN" "$out"
check "it prints the ref import" contains "ls-remote" "$out"
check "it prints the key it would mint" contains "ssh-keygen" "$out"
check "it says it would mint, not that it did" contains "dry run: no promote key was created" "$out"
check "it prints the gh deploy-key command" contains "gh repo deploy-key add" "$out"
check "it prints the daemon restart" contains "daemon restart" "$out"
check "it names the github Actions reminder" contains "actions/permissions -F enabled=false" "$out"
check "it does not print the string form that 422s" lacks "actions/permissions -f enabled=false" "$out"
check "it says it would back up, not that it did" contains "would back up" "$out"
check "it never claims a backup it did not make" lacks "backed up" "$out"
check "the settings file is byte-identical" [ "$(cat "$RIG_ROOT/settings/config.json")" = "$before_settings" ]
check "town.json is byte-identical" [ "$(cat "$TOWN/mayor/town.json")" = "$before_town" ]
check "no config backup is left" [ -z "$(backups)" ]
check "no town.json backup exists" [ -z "$(find "$TOWN/mayor" -name 'town.json.bak-*')" ]
check "no settings backup exists" [ -z "$(find "$RIG_ROOT/settings" -name 'config.json.bak-*')" ]
check "the ref import changed nothing" [ "$(cat "$STATE/refs_$(refs_key "$FORGEJO_URL")")" = "$before_refs" ]
check "no promote key is created" [ ! -e "$PROMOTE_KEY_FILE" ]
check "ssh-keygen was not run" [ "$(sk_lines)" = 0 ]
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
check "the probe ran with the cutover's town root" contains "--town-root $TOWN" "$(cat "$STATE/probe.log")"
# The provisioner that grants the panel's viewer bot read is the shared one
# (gt-fn9e6.49).
check "the repo is provisioned through the shared provisioner" contains "--repo acme/rig" "$(cat "$STATE/provision.log")"
check "every GitHub ref was imported" contains "imported every GitHub ref" "$out"
check "a default run creates no push mirror" [ "$(count_calls POST)" = 0 ]
check "no mirror body was sent" [ ! -f "$STATE/mirror-create-body" ]
check "the promote key was minted at the default path" contains "$PROMOTE_KEY_FILE" "$(cat "$STATE/ssh-keygen.log")"
check "the key is ed25519" contains "-t ed25519" "$(cat "$STATE/ssh-keygen.log")"
check "the private key is mode 600" [ "$(mode_of "$PROMOTE_KEY_FILE")" = 600 ]
check "the gh command names the public key" contains "gh repo deploy-key add $PROMOTE_KEY_FILE.pub" "$out"
check "the gh command grants write" contains "--allow-write" "$out"
check "the gh command titles the key for the rig" contains "--title promote-acme" "$out"
check "the private key never reaches stdout" lacks "STUBPRIVATEKEYMATERIAL" "$out"
check "the import listed the admin base" contains "ls-remote --heads --tags --refs $ADMIN_GIT_URL" "$(cat "$STATE/git.log")"
check "the import pushed to the admin base" contains "$ADMIN_GIT_URL" "$(cat "$STATE/pushes")"
check "the probe pushed through the admin base" contains "--git-url $ADMIN_GIT_URL" "$(cat "$STATE/probe.log")"
check "the bare repo is repointed" contains "$RIG_ROOT/.repo.git $FORGEJO_URL" "$(cat "$STATE/set-urls")"
check "the mayor clone is repointed" contains "$RIG_ROOT/mayor/rig $FORGEJO_URL" "$(cat "$STATE/set-urls")"
check "the crew clone is repointed" contains "$RIG_ROOT/crew/sloan $FORGEJO_URL" "$(cat "$STATE/set-urls")"
check "a non-repo crew dir is skipped" contains "skip $RIG_ROOT/crew/notes" "$out"
check "settings/config.json names the Forgejo remote" contains "$FORGEJO_URL" "$(cat "$RIG_ROOT/settings/config.json")"
check "settings/config.json names the gate workflow" contains '"gate_workflow": "gate"' "$(cat "$RIG_ROOT/settings/config.json")"
check "settings/config.json names the three bots" contains '"landing": "bot-landing"' "$(cat "$RIG_ROOT/settings/config.json")"
check "settings/config.json names the promote target" contains "\"promote_target\": \"$GITHUB_URL\"" "$(cat "$RIG_ROOT/settings/config.json")"
check "settings/config.json names the promote key" contains "\"promote_key_file\": \"$PROMOTE_KEY_FILE\"" "$(cat "$RIG_ROOT/settings/config.json")"
check "settings/config.json names no mirror target" lacks "mirror_target" "$(cat "$RIG_ROOT/settings/config.json")"
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
check "the promote key is reused" contains "reusing the promote key" "$out"
check "the key is not regenerated" [ "$(sk_lines)" = 1 ]
check "the remotes are reported already right" contains "origin is already" "$out"
check "no remote is set again" [ "$(cat "$STATE/set-urls" | grep -c 'set-url')" = "$starts_before" ]
check "town.json is unchanged" [ "$(cat "$TOWN/mayor/town.json")" = "$town_before" ]
check "no second backup is written" [ "$(backups)" = "$backups_before" ]
check "the block is reported already matching" contains "merge_queue.forgejo already matches" "$out"

echo "=== --mirror reproduces the push mirror ==="
fresh
printf 'refs/heads/main abc123\n' > "$STATE/refs_$(refs_key "$FORGEJO_URL")"
STUB_MIRROR_ABSENT=1
out=$(run_cutover --mirror); rc=$?
if [ "$rc" = 0 ]; then pass "the mirror cutover exits 0"; else fail "the mirror cutover exits 0 (rc=$rc)" "$out"; fi
check "the push mirror was created without a branch filter" contains '"branch_filter":""' "$(cat "$STATE/mirror-create-body" 2>/dev/null)"
check "the mirror enables ssh" contains '"use_ssh":true' "$(cat "$STATE/mirror-create-body" 2>/dev/null)"
check "the mirror's deploy key is printed" contains "AAAAMIRRORKEY" "$out"
check "the block names the mirror target" contains "\"mirror_target\": \"$GITHUB_URL\"" "$(cat "$RIG_ROOT/settings/config.json")"
check "the block names no promote fields" lacks "promote_" "$(cat "$RIG_ROOT/settings/config.json")"
check "no promote key is minted" [ "$(sk_lines)" = 0 ]
check "no promote key file is left" [ ! -e "$PROMOTE_KEY_FILE" ]

echo "=== a hostname-form --forgejo-url does the script's own git work as admin ==="
# The operator passes the credential-helper hostname form, which every rig
# remote and town.json entry carries, for a repository the bots cannot reach
# until the provisioner has run. The ref listing and the import must ride the
# admin base instead, while the hostname form is what gets written.
fresh
STUB_MIRROR_ABSENT=1
STUB_FORGEJO_EXTRA_URL="$HOSTNAME_GIT_URL"
out=$(run_cutover --forgejo-url "$HOSTNAME_GIT_URL"); rc=$?
if [ "$rc" = 0 ]; then pass "the cutover exits 0"; else fail "the cutover exits 0 (rc=$rc)" "$out"; fi
check "the ref listing and import succeeded" contains "imported every GitHub ref" "$out"
check "the import listed the admin base" contains "ls-remote --heads --tags --refs $ADMIN_GIT_URL" "$(cat "$STATE/git.log")"
check "the import pushed to the admin base" contains "$ADMIN_GIT_URL" "$(cat "$STATE/pushes")"
check "the unreachable hostname never carried a ref listing" lacks "ls-remote --heads --tags --refs $HOSTNAME_GIT_URL" "$(cat "$STATE/git.log")"
check "the unreachable hostname never carried a push" lacks "$HOSTNAME_GIT_URL" "$(cat "$STATE/pushes")"
check "the probe pushed through the admin base" contains "--git-url $ADMIN_GIT_URL" "$(cat "$STATE/probe.log")"
check "the hostname form is written to the bare remote" contains "$RIG_ROOT/.repo.git $HOSTNAME_GIT_URL" "$(cat "$STATE/set-urls")"
check "the hostname form is written to the mayor clone" contains "$RIG_ROOT/mayor/rig $HOSTNAME_GIT_URL" "$(cat "$STATE/set-urls")"
check "the hostname form is written to the crew clone" contains "$RIG_ROOT/crew/sloan $HOSTNAME_GIT_URL" "$(cat "$STATE/set-urls")"
check "the hostname form is written to town.json" contains "\"git_url\": \"$HOSTNAME_GIT_URL\"" "$(cat "$TOWN/mayor/town.json")"
check "the hostname form is written to the rig block" contains "\"remote_url\": \"$HOSTNAME_GIT_URL\"" "$(cat "$RIG_ROOT/settings/config.json")"
check "the promote target is the ssh form of the GitHub URL" contains "\"promote_target\": \"$GITHUB_URL\"" "$(cat "$RIG_ROOT/settings/config.json")"

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

echo "=== Forgejo ahead of GitHub keeps its refs, never rewinding them ==="
# A rig that is already cut over: GitHub's main is frozen at c1 while Forgejo's
# has gone on to c3, two commits past it. The old forced import rewound it.
fresh
printf 'c1 \nc2 c1\nc3 c2\n' > "$STATE/commits"
printf 'refs/heads/main c1\n' > "$STATE/refs_$(refs_key "$GITHUB_URL")"
printf 'refs/heads/main c3\n' > "$STATE/refs_$(refs_key "$FORGEJO_URL")"
forgejo_before=$(cat "$STATE/refs_$(refs_key "$FORGEJO_URL")")
out=$(run_cutover); rc=$?
if [ "$rc" = 0 ]; then pass "a second cutover of an ahead rig exits 0"; else fail "a second cutover of an ahead rig exits 0 (rc=$rc)" "$out"; fi
check "it says Forgejo is ahead, with the count" contains "kept: refs/heads/main — Forgejo is ahead of GitHub by 2 commits" "$out"
check "it names the Forgejo commit" contains "forgejo c3" "$out"
check "it names the GitHub commit" contains "github c1" "$out"
check "Forgejo main is unchanged" [ "$(cat "$STATE/refs_$(refs_key "$FORGEJO_URL")")" = "$forgejo_before" ]
check "nothing was pushed" [ ! -f "$STATE/pushes" ]
check "no force was used" [ ! -f "$STATE/force-violations" ]
check "it continued to the promote step" contains "\"promote_target\": \"$GITHUB_URL\"" "$(cat "$RIG_ROOT/settings/config.json")"
check "the daemon was restarted" contains "daemon restart" "$(cat "$STATE/gt.log")"

echo "=== Forgejo behind GitHub is fast-forwarded ==="
fresh
printf 'c1 \nc2 c1\nc3 c2\n' > "$STATE/commits"
printf 'refs/heads/main c3\nrefs/heads/feature c2\n' > "$STATE/refs_$(refs_key "$GITHUB_URL")"
printf 'refs/heads/main c1\n' > "$STATE/refs_$(refs_key "$FORGEJO_URL")"
out=$(run_cutover); rc=$?
if [ "$rc" = 0 ]; then pass "the cutover exits 0"; else fail "the cutover exits 0 (rc=$rc)" "$out"; fi
check "it plans the fast-forward" contains "will import refs/heads/main (fast-forward)" "$out"
check "it plans the ref Forgejo lacks" contains "will import refs/heads/feature (Forgejo has no copy)" "$out"
check "Forgejo main moved to GitHub's commit" contains "refs/heads/main c3" "$(cat "$STATE/refs_$(refs_key "$FORGEJO_URL")")"
check "the new ref arrived" contains "refs/heads/feature c2" "$(cat "$STATE/refs_$(refs_key "$FORGEJO_URL")")"
check "the push did not force" [ ! -f "$STATE/force-violations" ]
check "no push refspec carried a force marker" lacks "+refs/" "$(grep '^push' "$STATE/git.log")"

echo "=== a diverged ref stops the run before anything is pushed ==="
fresh
printf 'x1 \ny1 \nf1 \n' > "$STATE/commits"
printf 'refs/heads/main x1\nrefs/heads/feature f1\n' > "$STATE/refs_$(refs_key "$GITHUB_URL")"
printf 'refs/heads/main y1\n' > "$STATE/refs_$(refs_key "$FORGEJO_URL")"
forgejo_before=$(cat "$STATE/refs_$(refs_key "$FORGEJO_URL")")
out=$(run_cutover); rc=$?
if [ "$rc" != 0 ]; then pass "a diverged ref exits non-zero"; else fail "a diverged ref exits non-zero" "$out"; fi
check "it names the diverged ref" contains "diverged: refs/heads/main" "$out"
check "it names both commits" contains "(forgejo y1, github x1)" "$out"
check "it names the override" contains "--accept-diverged" "$out"
check "nothing reached Forgejo" [ "$(cat "$STATE/refs_$(refs_key "$FORGEJO_URL")")" = "$forgejo_before" ]
check "no push was attempted" [ ! -f "$STATE/pushes" ]
check "the cutover stopped before the promote key" [ ! -e "$PROMOTE_KEY_FILE" ]

echo "=== --accept-diverged skips that ref and imports the rest ==="
out=$(run_cutover --accept-diverged); rc=$?
if [ "$rc" = 0 ]; then pass "an accepted divergence exits 0"; else fail "an accepted divergence exits 0 (rc=$rc)" "$out"; fi
check "it still reports the divergence" contains "diverged: refs/heads/main" "$out"
check "the diverged ref is left as Forgejo has it" contains "refs/heads/main y1" "$(cat "$STATE/refs_$(refs_key "$FORGEJO_URL")")"
check "the other ref was imported" contains "refs/heads/feature f1" "$(cat "$STATE/refs_$(refs_key "$FORGEJO_URL")")"
check "no force was used" [ ! -f "$STATE/force-violations" ]

echo "=== --dry-run shows a rewind before it can happen ==="
fresh
printf 'c1 \nc2 c1\nc3 c2\n' > "$STATE/commits"
printf 'refs/heads/main c1\n' > "$STATE/refs_$(refs_key "$GITHUB_URL")"
printf 'refs/heads/main c3\n' > "$STATE/refs_$(refs_key "$FORGEJO_URL")"
forgejo_before=$(cat "$STATE/refs_$(refs_key "$FORGEJO_URL")")
out=$(run_cutover --dry-run); rc=$?
if [ "$rc" = 0 ]; then pass "the dry run exits 0"; else fail "the dry run exits 0 (rc=$rc)" "$out"; fi
check "the dry run prints the kept line" contains "kept: refs/heads/main — Forgejo is ahead of GitHub by 2 commits" "$out"
check "the dry run leaves Forgejo alone" [ "$(cat "$STATE/refs_$(refs_key "$FORGEJO_URL")")" = "$forgejo_before" ]
check "the dry run pushed nothing" [ ! -f "$STATE/pushes" ]

echo "=== --dry-run shows the fast-forward it would make ==="
fresh
printf 'c1 \nc2 c1\nc3 c2\n' > "$STATE/commits"
printf 'refs/heads/main c3\n' > "$STATE/refs_$(refs_key "$GITHUB_URL")"
printf 'refs/heads/main c1\n' > "$STATE/refs_$(refs_key "$FORGEJO_URL")"
forgejo_before=$(cat "$STATE/refs_$(refs_key "$FORGEJO_URL")")
out=$(run_cutover --dry-run); rc=$?
if [ "$rc" = 0 ]; then pass "the dry run exits 0"; else fail "the dry run exits 0 (rc=$rc)" "$out"; fi
check "the dry run prints the fast-forward" contains "will import refs/heads/main (fast-forward)" "$out"
check "the dry run leaves Forgejo alone" [ "$(cat "$STATE/refs_$(refs_key "$FORGEJO_URL")")" = "$forgejo_before" ]
check "the dry run pushed nothing" [ ! -f "$STATE/pushes" ]

echo "=== --dry-run reports a divergence as the stop it would be ==="
fresh
printf 'x1 \ny1 \n' > "$STATE/commits"
printf 'refs/heads/main x1\n' > "$STATE/refs_$(refs_key "$GITHUB_URL")"
printf 'refs/heads/main y1\n' > "$STATE/refs_$(refs_key "$FORGEJO_URL")"
out=$(run_cutover --dry-run); rc=$?
if [ "$rc" != 0 ]; then pass "the dry run stops on the divergence"; else fail "the dry run stops on the divergence" "$out"; fi
check "it prints the diverged line" contains "diverged: refs/heads/main" "$out"
check "nothing was pushed" [ ! -f "$STATE/pushes" ]

echo
echo "forgejo-cutover_test: passed=$PASS failed=$FAIL"
[ "$FAIL" = 0 ]
