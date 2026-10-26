#!/usr/bin/env bash
# polecat-image/run.sh — keep gastown-polecat:dev at the installed gt's commit.
# Why, and how it is gated: plugin.md beside this script (gt-t4k72, gt-pmxo9).
#
# A sandbox sling refuses an image whose gt commit differs from the installed
# gt's, and gt is reinstalled after most landings, so the image is rebuilt here
# after each install. Every path and tool below can be overridden through the
# environment so run_test.sh can stub them.

set -euo pipefail

TOWN_ROOT="${POLECAT_IMAGE_TOWN_ROOT:-$HOME/gt}"
RIG_REPO="${POLECAT_IMAGE_RIG_REPO:-$TOWN_ROOT/gastown/.repo.git}"
BUILD_ROOT="${POLECAT_IMAGE_BUILD_ROOT:-$TOWN_ROOT/gastown/.polecat-image-build}"
BD_SRC="${POLECAT_IMAGE_BD_SRC:-$TOWN_ROOT/beads/mayor/rig}"
LOCK_DIR="${POLECAT_IMAGE_LOCK:-$TOWN_ROOT/.runtime/polecat-image.lock}"
# The gateway's per-sandbox records; this seam was named POLECAT_IMAGE_REGISTRY
# until that name went to the container registry below (gt-mm7q6).
SANDBOX_RECORDS="${POLECAT_IMAGE_SANDBOX_RECORDS:-$TOWN_ROOT/.runtime/sandboxes}"
IMAGE="${POLECAT_IMAGE_NAME:-gastown-polecat:dev}"
# Empty REGISTRY publishes nothing (plugin.md, "Publishing to a registry").
REGISTRY="${POLECAT_IMAGE_REGISTRY:-}"
# FAIL_CLOSED=1 makes a run that leaves the image unbuilt or unpublished fail
# instead of deferring or logging (gt-efupr); plugin.md, "Publishing to a
# registry", has the contract. Only 1 turns it on, so a stray
# POLECAT_IMAGE_FAIL_CLOSED=0 cannot fail a run.
FAIL_CLOSED=0
if [ "${POLECAT_IMAGE_FAIL_CLOSED:-}" = 1 ]; then FAIL_CLOSED=1; fi
# PUBLISHED holds "<image id> <registry>" once that image is fully pushed.
PUBLISHED="${POLECAT_IMAGE_PUBLISHED:-$TOWN_ROOT/.runtime/polecat-image.published}"
LABEL="org.gastown.gt-commit"
STALE_LOCK_MINUTES="${POLECAT_IMAGE_STALE_MINUTES:-20}"
MAX_ROUNDS=3
# KEEP_IMAGES is how many of the newest per-commit tags prune_old_images leaves.
KEEP_IMAGES="${POLECAT_IMAGE_KEEP:-3}"
# BUILDER_KEEP is how much of the build cache prune_build_cache leaves (most
# recently used first), as docker's --keep-storage value.
BUILDER_KEEP="${POLECAT_IMAGE_BUILDER_KEEP:-8GB}"
# CACHE_CAP bounds each per-rig cache volume (gt-cache-<rig>, mounted at
# /sandbox/.cache in every sandbox of the rig): nothing trimmed them and
# gt-cache-gastown reached 27.7 GB, growing about 3 GB an hour (gt-3g7p2). The
# helper mounts it at CACHE_MOUNT, which its script measures.
CACHE_CAP="${POLECAT_CACHE_CAP:-10GB}"
CACHE_MOUNT=/cache
CACHE_VOLUME_PREFIX="gt-cache-"

GT="${POLECAT_IMAGE_GT:-gt}"
DOCKER="${POLECAT_IMAGE_DOCKER:-docker}"
GIT="${POLECAT_IMAGE_GIT:-git}"
MAKE="${POLECAT_IMAGE_MAKE:-make}"
OPENSHELL="${POLECAT_IMAGE_OPENSHELL:-openshell}"
CRANE="${POLECAT_IMAGE_CRANE:-crane}"
PGREP="${POLECAT_IMAGE_PGREP:-pgrep}"

log() { echo "[polecat-image] $*"; }
die() { log "ERROR: $*"; exit 1; }

# installed_commit prints the commit the installed gt was built from: the last
# parenthesised group of `gt version` is "(<build>: <commit>)" or
# "(<build>: <branch>@<commit>)", the same shape the sandbox backend reads. Output
# in any other shape, or a field that is not a 7+ character hex commit, prints
# nothing.
installed_commit() {
  local out grp field
  out="$("$GT" version 2>/dev/null | head -n 1)"
  case "$out" in *"("*) ;; *) return 0 ;; esac
  grp="${out##*(}"
  grp="${grp%%)*}"
  case "$grp" in *:*) ;; *) return 0 ;; esac
  field="${grp#*:}"
  field="${field##*@}"
  field="$(printf '%s' "$field" | tr -d ' ')"
  case "$field" in '' | *[!0-9a-f]*) return 0 ;; esac
  [ "${#field}" -ge 7 ] || return 0
  printf '%s\n' "$field"
}

# image_commit prints the gt commit label of the image, empty when the image or
# the label is missing. Only stdout counts: a docker warning on stderr must not
# read as a commit.
image_commit() {
  "$DOCKER" image inspect --format '{{index .Config.Labels "'"$LABEL"'"}}' "$IMAGE" 2>/dev/null || true
}

# same_commit succeeds when both are at least 7 characters and one is a prefix
# of the other: the same rule the sandbox backend's check applies.
same_commit() {
  local a=$1 b=$2
  [ "${#a}" -ge 7 ] && [ "${#b}" -ge 7 ] || return 1
  case "$a" in "$b"*) return 0 ;; esac
  case "$b" in "$a"*) return 0 ;; esac
  return 1
}

# defer_reason prints why a rebuild must wait, empty when it may run: a gt sling
# is running, or the gateway lists a sandbox whose registry record does not
# exist yet (a Prepare between create and its record). A retag then could land
# between a Prepare's image check and its create. A probe that cannot answer is
# a deferral too: "no sling running" and "pgrep failed" must not read the same.
defer_reason() {
  local rc=0 out name header=0
  "$PGREP" -f '^([^ ]*/)?gt sling ' >/dev/null 2>&1 || rc=$?
  case "$rc" in
    0) echo "sandbox sling in progress"; return ;;
    1) ;; # pgrep ran and matched nothing
    *) echo "cannot check for a running gt sling ($PGREP exited $rc)"; return ;;
  esac
  rc=0
  out="$("$OPENSHELL" sandbox list 2>/dev/null)" || rc=$?
  if [ "$rc" != 0 ]; then
    echo "cannot list sandboxes ($OPENSHELL exited $rc)"
    return
  fi
  # `openshell sandbox list` prints a NAME header and one row per sandbox, or
  # the single line "No sandboxes found."; rows count only after the header.
  while read -r name _; do
    if [ "$name" = "NAME" ]; then header=1; continue; fi
    [ "$header" = 1 ] && [ -n "$name" ] || continue
    if [ ! -f "$SANDBOX_RECORDS/$name.json" ]; then
      echo "sandbox $name has no registry record yet"
      return
    fi
  done <<<"$out"
}

# stale_lock succeeds when the directory is older than STALE_LOCK_MINUTES: it
# belongs to a run that died.
stale_lock() {
  [ -n "$(find "$1" -maxdepth 0 -mmin "+$STALE_LOCK_MINUTES" 2>/dev/null)" ]
}

# claim_lock makes the lock directory and records this run's pid in it.
claim_lock() {
  mkdir "$LOCK_DIR" 2>/dev/null || return 1
  echo "$$" >"$LOCK_DIR/pid"
}

# take_lock takes the lock, taking over a stale one. A takeover renames the stale
# directory aside first, so of two runs that both saw it stale only one rename
# succeeds, and it re-checks the directory it moved: one that turned out fresh
# was another run's new lock and goes back.
take_lock() {
  mkdir -p "$(dirname "$LOCK_DIR")"
  claim_lock && return 0
  stale_lock "$LOCK_DIR" || return 1
  local aside="$LOCK_DIR.stale.$$"
  mv "$LOCK_DIR" "$aside" 2>/dev/null || return 1
  if ! stale_lock "$aside"; then
    [ -e "$LOCK_DIR" ] || mv "$aside" "$LOCK_DIR" 2>/dev/null || true
    rm -rf "$aside"
    return 1
  fi
  log "taking over a stale lock ($LOCK_DIR)"
  rm -rf "$aside"
  claim_lock
}

WORKTREE=""
TAR_DIR=""
HAVE_LOCK=0
cleanup() {
  [ -z "$TAR_DIR" ] || rm -rf "$TAR_DIR"
  if [ -n "$WORKTREE" ] && [ -d "$WORKTREE" ]; then
    "$GIT" -C "$RIG_REPO" worktree remove --force "$WORKTREE" >/dev/null 2>&1 || rm -rf "$WORKTREE"
  fi
  # Only a lock this run still owns: after a takeover the directory is another
  # run's, and removing it would let a third run build alongside it.
  if [ "$HAVE_LOCK" = 1 ] && [ "$(cat "$LOCK_DIR/pid" 2>/dev/null || true)" = "$$" ]; then
    rm -rf "$LOCK_DIR"
  fi
}
trap cleanup EXIT

# build_at <commit> builds the image from a detached worktree of the rig repo
# at that commit, under BUILD_ROOT, and checks the label it ends up with.
build_at() {
  local want=$1 full out
  full="$("$GIT" -C "$RIG_REPO" rev-parse --verify "$want^{commit}" 2>&1)" || die "the installed commit $want is not in $RIG_REPO: $full"
  WORKTREE="$BUILD_ROOT/$full"
  mkdir -p "$BUILD_ROOT"
  rm -rf "$WORKTREE"
  "$GIT" -C "$RIG_REPO" worktree add --detach "$WORKTREE" "$full" >/dev/null 2>&1 || die "cannot make a worktree of $full under $BUILD_ROOT"
  log "building $IMAGE at ${full:0:12} (BD_SRC=$BD_SRC)"
  if ! out="$(cd "$WORKTREE" && "$MAKE" polecat-image "BD_SRC=$BD_SRC" 2>&1)"; then
    printf '%s\n' "$out" | tail -n 15
    die "make polecat-image failed at ${full:0:12}"
  fi
  "$GIT" -C "$RIG_REPO" worktree remove --force "$WORKTREE" >/dev/null 2>&1 || rm -rf "$WORKTREE"
  WORKTREE=""
  same_commit "$(image_commit)" "$full" || die "after the build $IMAGE carries '$(image_commit)', not ${full:0:12}"
}

# Every tag docker holds for the image: the build's, and any registry alias.
REPO_TAGS_FORMAT='{{range .RepoTags}}{{println .}}{{end}}'

# image_id prints the id of the image, empty when it is missing.
image_id() {
  "$DOCKER" image inspect --format '{{.Id}}' "$IMAGE" 2>/dev/null || true
}

# publish_stamp is what PUBLISHED holds after a complete push of the current
# image; another image or another registry makes it differ.
publish_stamp() { printf '%s %s\n' "$(image_id)" "$REGISTRY"; }

# publish_pending succeeds when a registry is named and the image is not yet
# fully pushed to it, so a failed push is retried while the image stays current.
publish_pending() {
  [ -n "$REGISTRY" ] || return 1
  [ "$(cat "$PUBLISHED" 2>/dev/null || true)" != "$(publish_stamp)" ]
}

# registry_problem prints why nothing can be published, empty when it can. A
# path is an old POLECAT_IMAGE_REGISTRY, which named the sandbox-records
# directory, left in an environment.
registry_problem() {
  case "$REGISTRY" in
    /* | ./* | ../* | '~'*)
      echo "POLECAT_IMAGE_REGISTRY=$REGISTRY is a path, not a host:port (the sandbox records directory is POLECAT_IMAGE_SANDBOX_RECORDS now)"
      return ;;
  esac
  command -v "$CRANE" >/dev/null 2>&1 || echo "POLECAT_IMAGE_REGISTRY=$REGISTRY is set but $CRANE is not installed"
}

# FIRST_PUSHED is the ref the registry accepted first, and so is what tells a
# later failure it left the image only partly published. push_to_registry clears
# it at the start of every attempt.
FIRST_PUSHED=""

# publish_state says what the registry holds after a failed publish: nothing
# before the first push, the pushed tags after it. The failure log and the
# fail-closed error both use it, so one run does not report the image as partly
# published in one line and as not published in the next.
publish_state() {
  if [ -n "$FIRST_PUSHED" ]; then echo "only partly published"; else echo "not published"; fi
}

# publish_tags tags and pushes each local tag of the image. The image is saved
# and pushed once, under its first tag, through TAR_DIR; every other tag is made
# on the registry with crane tag, which copies no layer (gt-roa0c). It stops at
# the first failure and logs why; nothing is removed.
publish_tags() {
  local repo="${IMAGE%%:*}" ref tag reg tar out refs
  if ! refs="$("$DOCKER" image inspect --format "$REPO_TAGS_FORMAT" "$IMAGE" 2>&1)"; then
    log "registry: cannot read the tags of $IMAGE ($(last_line "$refs")); the image is not published"
    return 1
  fi
  tar="$TAR_DIR/image.tar"
  while read -r ref; do
    [ -n "$ref" ] || continue
    # Only this repo's own tags: an alias from an earlier push carries the registry prefix.
    [ "${ref%:*}" = "$repo" ] || continue
    tag="${ref##*:}"
    case "$tag" in
      dev) ;;
      '' | *[!0-9a-f]*) continue ;;
      *) [ "${#tag}" -ge 7 ] || continue ;;
    esac
    reg="$REGISTRY/$ref"
    if ! out="$("$DOCKER" tag "$ref" "$reg" 2>&1)"; then
      log "registry: cannot tag $reg ($(last_line "$out")); the image is $(publish_state)"
      return 1
    fi
    if [ -n "$FIRST_PUSHED" ]; then
      if ! out="$("$CRANE" tag --insecure "$FIRST_PUSHED" "$tag" 2>&1)"; then
        log "registry: tagging $reg failed ($(last_line "$out")); the image is $(publish_state)"
        return 1
      fi
      log "registry: tagged $reg"
      continue
    fi
    rm -f "$tar"
    if ! out="$("$DOCKER" save -o "$tar" "$reg" 2>&1)"; then
      log "registry: cannot save $reg to a tarball ($(last_line "$out")); the image is not published"
      return 1
    fi
    if ! out="$("$CRANE" push --insecure "$tar" "$reg" 2>&1)"; then
      log "registry: pushing $reg failed ($(last_line "$out")); the image is not published"
      return 1
    fi
    rm -f "$tar"
    FIRST_PUSHED="$reg"
    log "registry: pushed $reg"
  done <<<"$refs"
}

# fail_closed WHY ends the run non-zero when FAIL_CLOSED is set; WHY says what
# the run did not get done, and every caller logs the cause first. Unset, it
# lets the caller carry on.
fail_closed() {
  [ "$FAIL_CLOSED" = 0 ] || die "$1 (POLECAT_IMAGE_FAIL_CLOSED=1)"
}

# publish_unfinished ends the run when FAIL_CLOSED is set; the caller has logged
# why the push did not complete.
publish_unfinished() {
  fail_closed "the image is $(publish_state) to $REGISTRY, so this run fails"
}

# defer MSG logs why the run is deferring and ends it: exit 0 best effort, but
# under FAIL_CLOSED the image the run was asked for is not in place, so it fails.
defer() {
  log "$1"
  fail_closed "the image was not built and published at the installed commit, so this run fails"
  return 0
}

# push_to_registry publishes the image to POLECAT_IMAGE_REGISTRY (gt-mm7q6) and
# records the push in PUBLISHED when every tag went. A failure is logged and the
# run carries on unless FAIL_CLOSED is set, which makes it the run's failure.
push_to_registry() {
  local why
  FIRST_PUSHED=""
  [ -n "$REGISTRY" ] || return 0
  why="$(registry_problem)"
  if [ -n "$why" ]; then
    log "registry: $why; the image is not published"
    publish_unfinished
    return 0
  fi
  mkdir -p "$BUILD_ROOT"
  if ! TAR_DIR="$(mktemp -d "$BUILD_ROOT/push.XXXXXX")"; then
    TAR_DIR=""
    log "registry: cannot make a directory under $BUILD_ROOT; the image is not published"
    publish_unfinished
    return 0
  fi
  if publish_tags; then
    publish_stamp >"$PUBLISHED" || log "registry: cannot write $PUBLISHED; the next run pushes again"
  else
    publish_unfinished
  fi
  rm -rf "$TAR_DIR"
  TAR_DIR=""
}

# republish retries a push that did not complete, for an image that needs no
# build. A lock another run holds leaves the push to that run.
republish() {
  publish_pending || return 0
  if ! take_lock; then
    defer "registry: already running (lock $LOCK_DIR); the publish waits for the next run"
    return 0
  fi
  HAVE_LOCK=1
  push_to_registry
}

main() {
  local installed image reason
  installed="$(installed_commit)"
  [ -n "$installed" ] || die "cannot read the installed gt's commit from '$GT version'"
  # Docker Desktop stopped is a state to wait out, not a failure to raise every
  # cooldown: without the check an unreachable docker reads as "no image" and
  # the build then fails.
  if ! "$DOCKER" info >/dev/null 2>&1; then
    defer "deferred: docker unavailable"
    return 0
  fi
  image="$(image_commit)"
  if same_commit "$image" "$installed"; then
    republish
    log "current at ${installed:0:12}"
    return 0
  fi
  reason="$(defer_reason)"
  if [ -n "$reason" ]; then
    defer "deferred: $reason"
    return 0
  fi
  if ! take_lock; then
    defer "already running (lock $LOCK_DIR)"
    return 0
  fi
  HAVE_LOCK=1

  for _ in $(seq 1 "$MAX_ROUNDS"); do
    installed="$(installed_commit)"
    [ -n "$installed" ] || die "cannot read the installed gt's commit from '$GT version'"
    if same_commit "$(image_commit)" "$installed"; then
      log "current at ${installed:0:12}"
      return 0
    fi
    # A sling can start between rounds, and a build is long.
    reason="$(defer_reason)"
    if [ -n "$reason" ]; then
      defer "deferred: $reason"
      return 0
    fi
    build_at "$installed"
    push_to_registry
  done
  installed="$(installed_commit)"
  if same_commit "$(image_commit)" "$installed"; then
    log "current at ${installed:0:12}"
    return 0
  fi
  die "the installed gt kept moving: $IMAGE is still not at ${installed:0:12} after $MAX_ROUNDS builds"
}

# drop_registry_alias untags the registry reference of a per-commit tag that is
# about to go: left behind it keeps the image on disk. A reference that cannot
# be removed is logged with docker's reason and fails, so the caller keeps the tag.
drop_registry_alias() {
  local ref="$REGISTRY/$1" err
  [ -n "$REGISTRY" ] || return 0
  "$DOCKER" image inspect --format '{{.Id}}' "$ref" >/dev/null 2>&1 || return 0
  if err="$("$DOCKER" rmi "$ref" 2>&1)"; then
    log "removed $ref"
  else
    log "kept $1 ($ref: rmi failed: $(last_line "$err"))"
    return 1
  fi
}

# prune_old_images untags the per-commit images (gastown-polecat:<hex sha>) every
# build leaves behind, keeping the newest KEEP_IMAGES. Never :dev, never a tag
# that is not a hex commit, never the image :dev names, and never forced: a
# refusal (a running sandbox's container, an unreachable daemon) keeps the tag
# and logs docker's own reason, so a refusal never reads as some other failure.
# A listing that fails prunes nothing (gt-mexpf).
prune_old_images() {
  local repo="${IMAGE%%:*}" tag n=0 dev_id id err
  "$DOCKER" info >/dev/null 2>&1 || return 0
  dev_id="$("$DOCKER" image inspect --format '{{.Id}}' "$IMAGE" 2>/dev/null || true)"
  while read -r tag; do
    case "$tag" in '' | *[!0-9a-f]*) continue ;; esac
    [ "${#tag}" -ge 7 ] || continue
    n=$((n + 1))
    [ "$n" -gt "$KEEP_IMAGES" ] || continue
    id="$("$DOCKER" image inspect --format '{{.Id}}' "$repo:$tag" 2>/dev/null || true)"
    [ -n "$id" ] || continue
    [ "$id" != "$dev_id" ] || continue
    drop_registry_alias "$repo:$tag" || continue
    if err="$("$DOCKER" rmi "$repo:$tag" 2>&1)"; then
      log "removed $repo:$tag"
    else
      err="$(printf '%s\n' "$err" | tail -n 1)"
      log "kept $repo:$tag (rmi failed: ${err:-no output})"
    fi
  done < <("$DOCKER" images --format '{{.Tag}}' "$repo" 2>/dev/null || true)
}

# own_lock makes this run the plugin lock's owner for a trim, so a trim never
# overlaps a build: a run that built already owns it, any other takes it, and
# one that cannot sets LOCK_WHY and leaves the lock alone. It reports through a
# variable, not stdout: a `$(own_lock)` would run in a subshell, and the
# HAVE_LOCK it set there would be gone by the time cleanup looked at it.
LOCK_WHY=""
own_lock() {
  LOCK_WHY=""
  if [ "$HAVE_LOCK" != 1 ]; then
    take_lock || { LOCK_WHY="another run holds the lock"; return 1; }
    HAVE_LOCK=1
    return 0
  fi
  [ "$(cat "$LOCK_DIR/pid" 2>/dev/null || true)" = "$$" ] || { LOCK_WHY="the lock is another run's now"; return 1; }
}

# prune_build_cache keeps Docker's build cache to the newest BUILDER_KEEP. A
# failed command prunes nothing and is logged (gt-atxdz).
prune_build_cache() {
  "$DOCKER" info >/dev/null 2>&1 || return 0
  local out
  if ! own_lock; then
    log "build cache: skipped, $LOCK_WHY"
    return 0
  fi
  if out="$("$DOCKER" builder prune --keep-storage "$BUILDER_KEEP" -f 2>&1)"; then
    log "build cache trimmed to the newest $BUILDER_KEEP: $(printf '%s\n' "$out" | tail -n 1)"
  else
    log "build cache not trimmed: $(printf '%s\n' "$out" | tail -n 1)"
  fi
}

# cap_bytes prints a size such as 10GB, 512M or 1024 as bytes. Anything
# unreadable fails, so a typo trims nothing rather than reading as a cap near
# zero.
cap_bytes() {
  local v=$1 mult=1
  case "$v" in '' | *[!0-9kKmMgGtTpPbB]*) return 1 ;; esac
  v=${v%[Bb]}
  case "$v" in
    *[kK]) mult=$((1024)); v=${v%?} ;;
    *[mM]) mult=$((1024 * 1024)); v=${v%?} ;;
    *[gG]) mult=$((1024 * 1024 * 1024)); v=${v%?} ;;
    *[tT]) mult=$((1024 * 1024 * 1024 * 1024)); v=${v%?} ;;
    *[pP]) mult=$((1024 * 1024 * 1024 * 1024 * 1024)); v=${v%?} ;;
  esac
  case "$v" in '' | *[!0-9]*) return 1 ;; esac
  echo "$((v * mult))"
}

# human renders a byte count for a log line: 9.8GB, 512.0KB.
human() {
  awk -v b="$1" 'BEGIN { split("B KB MB GB TB PB", u, " "); n = 1; while (b >= 1024 && n < 6) { b /= 1024; n++ } printf "%.1f%s", b, u[n] }'
}

# last_line prints the last non-blank line of a command's output: the reason a
# docker call failed is its last line, and everything before is noise.
last_line() {
  printf '%s\n' "$1" | sed '/^[[:space:]]*$/d' | tail -n 1
}

# CACHE_TRIM_SCRIPT runs inside the helper container. A docker volume has no
# host path, and the polecat image already carries find, du and stat. Each file
# is deleted least recently accessed first: go-build's until the volume is back
# under the cap, then go/pkg/mod's only if the build cache alone could not
# reach it. A size it cannot read exits non-zero, so the caller logs the reason
# rather than a trim it cannot vouch for. Every tool it uses is portable,
# because run_test.sh runs it on the host (macOS, where `du -b` and
# `find -printf` do not exist).
CACHE_TRIM_SCRIPT=$(cat <<'SCRIPT'
set -eu
root=${CACHE_ROOT:-/cache}
cap=${CACHE_CAP_BYTES:?CACHE_CAP_BYTES is unset}

[ -d "$root" ] || { echo "no $root to measure" >&2; exit 3; }

# size prints the tree's disk usage in bytes, from du's portable KiB figure.
size() {
  local out
  out=$(du -sk "$root" 2>/dev/null) || return 1
  printf '%s\n' $(( ${out%%[[:space:]]*} * 1024 ))
}

# list_files prints "<last use> <size> <path>" per file. Last use is the later
# of atime and mtime: a noatime or relatime mount leaves atime stale, while Go
# touches the mtime of a cache entry it reuses. BSD and GNU stat differ in both
# flag and format. Probe for GNU first (the image's stat): `-c` is an illegal
# option on BSD, whereas GNU accepts BSD's `-f` as "filesystem" and succeeds
# with the wrong fields, so a probe for BSD would pass on Linux.
list_files() {
  if stat -c '%X' "$root" >/dev/null 2>&1; then
    find "$1" -type f -exec stat -c '%X %Y %s %n' {} +
  else
    find "$1" -type f -exec stat -f '%a %m %z %N' {} +
  fi 2>/dev/null |
    awk '{ t = ($2 + 0 > $1 + 0) ? $2 : $1; sub(/^[0-9]+ [0-9]+ /, ""); print t, $0 }'
}

# trim_tree DIR NEED deletes files oldest-accessed first until NEED bytes are
# freed. The caller measures again rather than trusting this arithmetic.
trim_tree() {
  local need=$2
  [ -d "$1" ] || return 0
  list_files "$1" | LC_ALL=C sort -n -k1,1 |
    while read -r _atime fsize path; do
      [ "$need" -gt 0 ] || break
      rm -f -- "$path" || continue
      need=$((need - fsize))
    done
  find "$1" -mindepth 1 -type d -empty -exec rmdir {} + 2>/dev/null || true
}

before=$(size) || { echo "cannot measure the size of $root" >&2; exit 3; }
if [ "$before" -le "$cap" ]; then
  echo "under $before"
  exit 0
fi
trim_tree "$root/go-build" "$((before - cap))"
after=$(size) || { echo "cannot measure the size of $root after trimming" >&2; exit 3; }
if [ "$after" -gt "$cap" ]; then
  trim_tree "$root/go/pkg/mod" "$((after - cap))"
  after=$(size) || { echo "cannot measure the size of $root after trimming" >&2; exit 3; }
fi
echo "trimmed $before $after"
SCRIPT
)

# trim_cache_volume trims one volume back under cap, or logs why it did not:
# a container still using it, a probe that cannot answer, or a helper that
# failed. The volume itself is never removed and nothing is pruned.
trim_cache_volume() {
  local vol=$1 cap=$2 out used last
  if ! out="$("$DOCKER" volume inspect "$vol" 2>&1)"; then
    log "cache: $vol: cannot inspect the volume ($(last_line "$out")); nothing trimmed"
    return 0
  fi
  if ! out="$("$DOCKER" ps --filter "volume=$vol" --format '{{.ID}}' 2>&1)"; then
    log "cache: $vol: cannot list the containers using it ($(last_line "$out")); nothing trimmed"
    return 0
  fi
  used="$(printf '%s\n' "$out" | sed '/^[[:space:]]*$/d' | wc -l | tr -d ' ')"
  if [ "$used" -gt 0 ]; then
    log "cache: $vol: in use by $used container(s); skipped"
    return 0
  fi
  # A sandbox can start after the check above; plugin.md says why that window
  # is accepted (gt-3g7p2). The container's own filesystem is read-only; the cache volume at
  # CACHE_MOUNT and a temp directory for sort are the only writable places.
  if ! out="$("$DOCKER" run --rm --read-only --tmpfs /tmp:size=256m --network none \
    -v "$vol:$CACHE_MOUNT" -e "CACHE_CAP_BYTES=$cap" \
    "$IMAGE" sh -c "$CACHE_TRIM_SCRIPT" 2>&1)"; then
    log "cache: $vol: not trimmed ($(last_line "$out"))"
    return 0
  fi
  last="$(last_line "$out")"
  if [[ "$last" =~ ^under\ ([0-9]+)$ ]]; then
    log "cache: $vol: $(human "${BASH_REMATCH[1]}") of $CACHE_CAP, under the cap"
  elif [[ "$last" =~ ^trimmed\ ([0-9]+)\ ([0-9]+)$ ]]; then
    log "cache: $vol: trimmed $(human "${BASH_REMATCH[1]}") to $(human "${BASH_REMATCH[2]}") (cap $CACHE_CAP)"
    [ "${BASH_REMATCH[2]}" -le "$cap" ] || log "cache: $vol: still over the cap with nothing left to trim"
  else
    log "cache: $vol: not trimmed, the helper printed '$last'"
  fi
}

# trim_cache_volumes bounds each gt-cache-<rig> volume at CACHE_CAP. It skips
# every volume when the listing or the cap cannot be read, and one volume when
# a sandbox still has it attached (gt-3g7p2).
trim_cache_volumes() {
  "$DOCKER" info >/dev/null 2>&1 || return 0
  local cap out vol
  if ! cap="$(cap_bytes "$CACHE_CAP")"; then
    log "cache: POLECAT_CACHE_CAP=$CACHE_CAP is not a size such as 10GB; nothing trimmed"
    return 0
  fi
  if ! own_lock; then
    log "cache: skipped, $LOCK_WHY"
    return 0
  fi
  if ! out="$("$DOCKER" volume ls --format '{{.Name}}' --filter "name=^$CACHE_VOLUME_PREFIX" 2>&1)"; then
    log "cache: cannot list the volumes ($(last_line "$out")); nothing trimmed"
    return 0
  fi
  while read -r vol; do
    case "$vol" in "$CACHE_VOLUME_PREFIX"?*) ;; *) continue ;; esac
    trim_cache_volume "$vol" "$cap"
  done <<<"$out"
}

main "$@"
prune_old_images || true
prune_build_cache || true
trim_cache_volumes || true
