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
REGISTRY_DIR="${POLECAT_IMAGE_REGISTRY:-$TOWN_ROOT/.runtime/sandboxes}"
IMAGE="${POLECAT_IMAGE_NAME:-gastown-polecat:dev}"
LABEL="org.gastown.gt-commit"
STALE_LOCK_MINUTES="${POLECAT_IMAGE_STALE_MINUTES:-20}"
MAX_ROUNDS=3
# KEEP_IMAGES is how many of the newest per-commit tags prune_old_images leaves.
KEEP_IMAGES="${POLECAT_IMAGE_KEEP:-3}"

GT="${POLECAT_IMAGE_GT:-gt}"
DOCKER="${POLECAT_IMAGE_DOCKER:-docker}"
GIT="${POLECAT_IMAGE_GIT:-git}"
MAKE="${POLECAT_IMAGE_MAKE:-make}"
OPENSHELL="${POLECAT_IMAGE_OPENSHELL:-openshell}"
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
    if [ ! -f "$REGISTRY_DIR/$name.json" ]; then
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
HAVE_LOCK=0
cleanup() {
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

main() {
  local installed image reason
  installed="$(installed_commit)"
  [ -n "$installed" ] || die "cannot read the installed gt's commit from '$GT version'"
  # Docker Desktop stopped is a state to wait out, not a failure to raise every
  # cooldown: without the check an unreachable docker reads as "no image" and
  # the build then fails.
  if ! "$DOCKER" info >/dev/null 2>&1; then
    log "deferred: docker unavailable"
    return 0
  fi
  image="$(image_commit)"
  if same_commit "$image" "$installed"; then
    log "current at ${installed:0:12}"
    return 0
  fi
  reason="$(defer_reason)"
  if [ -n "$reason" ]; then
    log "deferred: $reason"
    return 0
  fi
  if ! take_lock; then
    log "already running (lock $LOCK_DIR)"
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
      log "deferred: $reason"
      return 0
    fi
    build_at "$installed"
  done
  installed="$(installed_commit)"
  if same_commit "$(image_commit)" "$installed"; then
    log "current at ${installed:0:12}"
    return 0
  fi
  die "the installed gt kept moving: $IMAGE is still not at ${installed:0:12} after $MAX_ROUNDS builds"
}

# prune_old_images untags the per-commit images (gastown-polecat:<hex sha>) every
# build leaves behind, keeping the newest KEEP_IMAGES. Never :dev, never a tag
# that is not a hex commit, never the image :dev names, and never forced: docker
# refuses to remove an image a container (a running sandbox) uses, and that
# refusal is the protection, logged as kept. A listing that fails prunes nothing
# (gt-mexpf).
prune_old_images() {
  local repo="${IMAGE%%:*}" tag n=0 dev_id id
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
    if "$DOCKER" rmi "$repo:$tag" >/dev/null 2>&1; then
      log "removed $repo:$tag"
    else
      log "kept $repo:$tag (in use)"
    fi
  done < <("$DOCKER" images --format '{{.Tag}}' "$repo" 2>/dev/null || true)
}

main "$@"
prune_old_images || true
