#!/usr/bin/env bash
# install-after-merge.sh — gastown's merge_queue.post_merge_command (claude-7fc).
#
# Run by the refinery's post-merge hook (internal/cmd, runPostMergeCommand) in
# <rig>/refinery/rig with GT_MERGED_SHA set. Skips the install only when every
# path changed since the INSTALLED binary's commit is one that cannot affect
# the runtime; anything else — including a path nobody anticipated — installs.
# The diff starts at the installed commit, not this merge's parent, so a
# docs-only merge cannot hide an earlier runtime merge whose install failed.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/install-gt-lib.sh
. "$SCRIPT_DIR/lib/install-gt-lib.sh"

SOURCE=post-merge
BIN_DIR="${INSTALL_GT_BIN_DIR:-$HOME/.local/bin}"
DAEMON_DIR="${INSTALL_GT_DAEMON_DIR:-${GT_TOWN_ROOT:?install-after-merge: GT_TOWN_ROOT is not set}/daemon}"
START=$(date +%s)
log() { echo "[install-after-merge] $*"; }

# The hook sets GT_MERGED_SHA empty when neither the MR's merge commit nor
# origin/<target> resolved. Nothing to install against: refuse (2); the hook
# escalates any nonzero exit and rebuild-gt installs as the backstop.
if [ -z "${GT_MERGED_SHA:-}" ]; then
  log "GT_MERGED_SHA is empty; not installing."
  igt_receipt refused "" "" no-merged-sha "" "$START"
  exit 2
fi

INSTALLED=$(igt_resolve . "$(igt_binary_commit_raw "$BIN_DIR/gt" .)")

if [ -n "$INSTALLED" ] && git merge-base --is-ancestor "$INSTALLED" "$GT_MERGED_SHA" 2>/dev/null; then
  RUNTIME=""
  while IFS= read -r f; do
    case "$f" in
      *_test.go|*.md|docs/*|.beads/*) ;;
      *) RUNTIME="$f"; break ;;
    esac
  done < <(git diff --name-only "$INSTALLED" "$GT_MERGED_SHA")
  if [ -z "$RUNTIME" ]; then
    log "Nothing since $INSTALLED touches the runtime; not installing $GT_MERGED_SHA."
    igt_receipt skipped "$GT_MERGED_SHA" "$INSTALLED" no-runtime-change \
      "$(git log -1 --format=%ct "$GT_MERGED_SHA" 2>/dev/null || true)" "$START"
    exit 0
  fi
  log "Runtime path changed since $INSTALLED (first: $RUNTIME)."
else
  log "Installed commit unknown or not behind $GT_MERGED_SHA; handing to install-gt."
fi

exec bash "$SCRIPT_DIR/install-gt.sh" --sha "$GT_MERGED_SHA" --source post-merge
