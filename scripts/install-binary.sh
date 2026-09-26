#!/usr/bin/env bash
# install-binary.sh — Replace an installed binary atomically.
#
# Why this is a script and not two lines of `cp` in the Makefile (gt-0het):
#
# The old recipe was `rm -f $(INSTALL_DIR)/gt` followed by `cp`, which is not
# atomic. For the duration of the copy the live path either does not exist or
# holds a partial ~128MB binary. On Apple Silicon, exec'ing a Go binary whose
# bytes are incomplete is hard-killed by macOS (SIGKILL, exit 137) with no
# output — and a SIGKILLed gt is indistinguishable from a crashed agent, so a
# transient packaging race can drive the stuck-agent dog to restart agents and
# cascade.
#
# So: copy to a uniquely-named temp file in the SAME directory, then rename over
# the target. rename(2) within one filesystem is atomic, so a concurrent reader
# sees either the old complete binary or the new complete binary, never a
# partial one. Creating the temp in the destination directory (rather than
# /tmp, which is often a different filesystem) is what keeps the rename atomic.
#
# The unique temp name is load-bearing too. safe-install previously copied to a
# FIXED name (gt.new) and then renamed it; two concurrent installs would write
# into the same temp file and one would rename the other's half-written copy
# into place — the same defect, just moved.
#
# gt-vya0s: the installed binary is also left OS-immutable (BSD `chflags
# uchg` / Linux `chattr +i`) between installs. gt-tnts5 added a Claude Code
# PreToolUse hook that blocks a polecat's Edit/Write/Bash from targeting this
# directory, but a local-coder (ollama) polecat runs with zero Claude Code
# hooks (gt-be0z) — that guard is a no-op for the exact seat that caused the
# incident. An immutable flag is enforced by the kernel, not a hook, so it
# protects the file no matter which agent or tool is doing the writing.
# Best-effort only: `chattr +i` silently no-ops without CAP_LINUX_IMMUTABLE
# (e.g. non-root on Linux), so this hardens the common case (this repo is
# developed and installed on macOS) without failing installs where the
# platform can't grant it.
#
# Usage: install-binary.sh <built-binary> <install-dir> [name]

set -euo pipefail

usage='usage: install-binary.sh <built-binary> <install-dir> [name]'
src="${1:?$usage}"
dest_dir="${2:?$usage}"
name="${3:-$(basename "$src")}"

if [ ! -f "$src" ]; then
  echo "install-binary.sh: source binary not found: $src" >&2
  exit 1
fi

mkdir -p "$dest_dir"
dest="$dest_dir/$name"

# clear_immutable/set_immutable straddle the atomic replace below: the flag
# must come off before mv (an immutable destination refuses rename(2) the
# same way it refuses unlink/write) and go back on once the new binary is in
# place, so the file is writable for no longer than this script's own install.
clear_immutable() {
  local target="$1"
  [ -e "$target" ] || return 0
  chflags nouchg "$target" 2>/dev/null || true
  chattr -i "$target" 2>/dev/null || true
}

set_immutable() {
  local target="$1"
  chflags uchg "$target" 2>/dev/null && return 0
  chattr +i "$target" 2>/dev/null || true
}

clear_immutable "$dest"

tmp="$(mktemp "$dest_dir/.$name.tmp.XXXXXX")"
# Leftovers are only possible if the copy or rename below failed; never leave a
# stray temp file in a directory that is on the user's PATH.
trap 'rm -f "$tmp"' EXIT

cp "$src" "$tmp"
# mktemp creates the file 0600, and `cp` onto an existing file keeps its mode,
# so set the mode explicitly. Stay faithful to the built artifact (a plain `cp`
# to a fresh path would too) rather than imposing one; --reference is GNU-only,
# so fall back to go build's 0755 on macOS.
chmod --reference="$src" "$tmp" 2>/dev/null || chmod 0755 "$tmp"

# Atomic: replaces the directory entry, so readers never observe a partial file.
mv -f "$tmp" "$dest"

set_immutable "$dest"
