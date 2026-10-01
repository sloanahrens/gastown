#!/usr/bin/env bash
# uninstall-gt.sh — THE way to remove the installed gt and bd binaries
# (gt-acdfp). It is the recipe docs/INSTALLING.md points at.
#
# Why this is a script and not the two-line recipe it replaces:
#
#   docs/INSTALLING.md said `rm $(which gt) $(which bd)`. `command -v` answers
#   "what runs if I type gt", not "where the installer put it", so inside a
#   running town it resolves to the live install. On 2026-09-30 a polecat that
#   ran the recipe to test it deleted ~/.local/bin/gt and ~/.local/bin/bd and
#   took every agent's bd down for two minutes — mail, escalate and the
#   daemon's own escalation all shell out to bd, so there was no working
#   escalation path and a broadcast was dropped (gt-acdfp; the guard gap that
#   let the rm through is gt-tt8sg).
#
# Two properties make that unrepeatable, and they are this script's contract:
#
#   1. The install directory is EXPLICIT. It comes from --install-dir, or
#      $INSTALL_GT_BIN_DIR, or the Makefile's default $HOME/.local/bin — never
#      from command -v or which. Every run prints the directory and which of
#      the three named it, so the target is never implicit.
#   2. The script REFUSES (exit 2) when the directory it was given is this
#      town's live install — $HOME/.local/bin with a town at or above this
#      clone — unless --force is passed. Removing the binaries a running town
#      runs from is a decommission, not a test, and it has to be asked for.
#
# It removes exactly two paths, <dir>/gt and <dir>/bd, and nothing else: no
# -r, no directory removal, and never the workspace. Deleting ~/gt — every
# rig, worktree and bead — stays a separate, deliberate act.
#
# gt-vya0s leaves the installed gt OS-immutable (BSD `chflags uchg`), which
# makes a plain rm fail with "Operation not permitted" — the reason the old
# recipe stopped working outright once that landed (gt-2xqtj). The flag comes
# off here first.
#
# Usage: uninstall-gt.sh [--install-dir DIR] [--force]
#
# Exit codes (2 and 1 keep install-gt.sh's meaning):
#   0  the binaries are gone, or were not there to begin with
#   1  failed: rm could not remove a binary that is present
#   2  refused: the target is this town's live install, or --install-dir names
#      a path this script will not touch (/ , $HOME, a relative path, a
#      non-directory, an unknown argument)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/install-gt-lib.sh
. "$SCRIPT_DIR/lib/install-gt-lib.sh"

usage='usage: uninstall-gt.sh [--install-dir DIR] [--force]'

BIN_DIR=""
BIN_DIR_SOURCE=""
FORCE=0
while [ $# -gt 0 ]; do
  case "$1" in
    --install-dir)
      [ $# -ge 2 ] || { echo "uninstall-gt: --install-dir needs a directory" >&2; exit 2; }
      BIN_DIR="$2"
      BIN_DIR_SOURCE="--install-dir"
      shift 2
      ;;
    --install-dir=*)
      BIN_DIR="${1#*=}"
      BIN_DIR_SOURCE="--install-dir"
      shift
      ;;
    --force)
      FORCE=1
      shift
      ;;
    -h|--help)
      echo "$usage"
      exit 0
      ;;
    *)
      echo "uninstall-gt: unknown argument: $1" >&2
      echo "$usage" >&2
      exit 2
      ;;
  esac
done

refuse() {
  echo "uninstall-gt: refusing to run: $1" >&2
  exit 2
}

[ -n "${HOME:-}" ] || refuse "HOME is not set; name the directory with --install-dir"

if [ -z "$BIN_DIR" ]; then
  if [ -n "${INSTALL_GT_BIN_DIR:-}" ]; then
    BIN_DIR="$INSTALL_GT_BIN_DIR"
    BIN_DIR_SOURCE="\$INSTALL_GT_BIN_DIR"
  else
    BIN_DIR="$HOME/.local/bin"
    BIN_DIR_SOURCE="the default (HOME/.local/bin)"
  fi
fi

case "$BIN_DIR" in
  /*) ;;
  *) refuse "--install-dir must be an absolute path: '$BIN_DIR'" ;;
esac
# Trailing slashes would make the live-install comparison below miss, and a
# lone "/" must not survive to become the empty string.
while [ "${BIN_DIR%/}" != "$BIN_DIR" ] && [ "$BIN_DIR" != "/" ]; do
  BIN_DIR="${BIN_DIR%/}"
done
if [ "$BIN_DIR" = "/" ]; then
  refuse "the filesystem root is not an install directory"
fi
if [ "$BIN_DIR" = "$HOME" ]; then
  refuse "the home directory itself is not an install directory"
fi
if [ -e "$BIN_DIR" ] && [ ! -d "$BIN_DIR" ]; then
  refuse "--install-dir is not a directory: $BIN_DIR"
fi

# The town this clone belongs to, if any. GT_TOWN_ROOT wins (install-gt.sh
# reads it the same way); a value that is not a town is treated as no town
# rather than as an error, so a caller can point this at a scratch tree.
TOWN_ROOT="${GT_TOWN_ROOT:-$(igt_town_root_above "$SCRIPT_DIR")}"
if [ -n "$TOWN_ROOT" ] && [ ! -f "$TOWN_ROOT/mayor/town.json" ]; then
  TOWN_ROOT=""
fi

# daemon_running TOWN_ROOT — is this town's daemon alive right now? Best
# effort and read-only: the pid file's first line is the pid (daemon/pidfile.go)
# and a signal-0 probe cannot disturb the process.
daemon_running() {
  local pid_file="$1/daemon/daemon.pid" pid
  [ -f "$pid_file" ] || return 1
  pid=$(head -n1 "$pid_file" 2>/dev/null | tr -cd '0-9')
  [ -n "$pid" ] || return 1
  kill -0 "$pid" 2>/dev/null
}

# canonical_dir DIR — DIR with symlinks resolved, or DIR unchanged when it does
# not exist. A symlinked alias of the install directory (a ~/bin -> ~/.local/bin
# from an older setup) would otherwise slip past the refusal below and remove the
# live binaries through the link.
canonical_dir() {
  local dir="$1"
  if [ -d "$dir" ]; then
    (cd -P "$dir" 2>/dev/null && pwd) || echo "$dir"
  else
    echo "$dir"
  fi
}

LIVE_BIN_DIR="$(canonical_dir "$HOME/.local/bin")"
BIN_DIR_LIVE_CHECK="$(canonical_dir "$BIN_DIR")"
RUNNING=0
if [ -n "$TOWN_ROOT" ] && daemon_running "$TOWN_ROOT"; then
  RUNNING=1
fi

# The refusal that replaces the incident: this directory is the binary install
# every agent on the host resolves gt and bd from, and a town is here.
if [ -n "$TOWN_ROOT" ] && [ "$BIN_DIR_LIVE_CHECK" = "$LIVE_BIN_DIR" ] && [ "$FORCE" -ne 1 ]; then
  {
    echo "uninstall-gt: refusing to run: $BIN_DIR is the live install of the town at $TOWN_ROOT"
    echo "  it would remove $BIN_DIR/gt and $BIN_DIR/bd, which every agent on this host"
    echo "  resolves off PATH — gt mail, gt escalate and the daemon's own escalation"
    echo "  all shell out to bd, so the town loses its escalation path with them (gt-acdfp)."
    if [ "$RUNNING" -eq 1 ]; then
      echo "  the daemon is running now: stop it first with 'gt daemon stop'."
    fi
    echo "  to remove them anyway, say so: make uninstall FORCE=1"
    echo "  to exercise this script safely, point it at a scratch directory:"
    echo "    $(basename "$0") --install-dir \"\$(mktemp -d)\""
  } >&2
  exit 2
fi

if [ "$FORCE" -eq 1 ] && [ -n "$TOWN_ROOT" ] && [ "$BIN_DIR_LIVE_CHECK" = "$LIVE_BIN_DIR" ]; then
  echo "uninstall-gt: FORCE=1 — removing the live install of the town at $TOWN_ROOT"
  if [ "$RUNNING" -eq 1 ]; then
    echo "uninstall-gt: the daemon is still running; it will fail the moment it next uses bd"
  fi
fi

echo "uninstall-gt: target $BIN_DIR (from $BIN_DIR_SOURCE)"

if [ ! -d "$BIN_DIR" ]; then
  echo "uninstall-gt: nothing to do — $BIN_DIR does not exist"
  exit 0
fi

# clear_immutable mirrors scripts/install-binary.sh: the kernel refuses to
# unlink an immutable file, so the flag has to come off first. -h keeps a
# symlinked bd (the Host's bd is one) pointing at its target untouched.
clear_immutable() {
  local target="$1"
  [ -e "$target" ] || [ -L "$target" ] || return 0
  chflags -h nouchg "$target" 2>/dev/null || chflags nouchg "$target" 2>/dev/null || true
  chattr -i "$target" 2>/dev/null || true
}

FAILED=0
REMOVED=0
for name in gt bd; do
  target="$BIN_DIR/$name"
  if [ ! -e "$target" ] && [ ! -L "$target" ]; then
    echo "uninstall-gt: not present: $target"
    continue
  fi
  clear_immutable "$target"
  if rm -f -- "$target" && [ ! -e "$target" ] && [ ! -L "$target" ]; then
    echo "uninstall-gt: removed $target"
    REMOVED=$((REMOVED + 1))
  else
    echo "uninstall-gt: FAILED to remove $target (still present after rm -f)" >&2
    FAILED=1
  fi
done

if [ "$FAILED" -ne 0 ]; then
  echo "uninstall-gt: $REMOVED removed, at least one failed; nothing else was touched" >&2
  exit 1
fi

echo "uninstall-gt: done — $REMOVED removed from $BIN_DIR; the workspace was not touched"
