#!/usr/bin/env bash
# Tests for the Makefile's install-local target (gt-tqxdd): the stale
# go-install shadow cleanup must not delete the binary install-local just
# installed.
#
# INSTALL_DIR=$HOME/go/bin is a supported setting, and that path is the first
# entry of the cleanup list ($(HOME)/go/bin/$(BINARY)): the recipe used to
# rm -f its own output, so that install finished successfully and left nothing
# installed where chflags/chattr do not apply.
#
# The target's prerequisites (check-up-to-date, check-forward-only, build) are
# declared up to date with make -o, so no build runs and nothing reads the
# network; scripts/install-binary.sh runs for real, into a temp INSTALL_DIR
# under a temp HOME. HOST SAFETY: this never touches the real ~/.local/bin or
# ~/go/bin.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MAKE_BIN="$(command -v "${MAKE:-make}")"
PASS=0
FAIL=0
pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "  FAIL: $1"; [[ $# -gt 1 ]] && printf '%s\n' "$2" | sed 's/^/    /'; FAIL=$((FAIL + 1)); }

T=""
cleanup() {
  if [[ -n "$T" && -d "$T" ]]; then
    # install-binary.sh leaves the installed binary OS-immutable (gt-vya0s);
    # rm -rf cannot remove it until the flag comes off.
    chflags -R nouchg "$T" 2>/dev/null || true
    chattr -R -i "$T" 2>/dev/null || true
    rm -rf "$T"
  fi
}
trap cleanup EXIT

# new_world makes a throwaway HOME with a stub build output at $T/build/gt.
new_world() {
  T="$(mktemp -d "${TMPDIR:-/tmp}/install-local.XXXXXX")"
  mkdir -p "$T/home/go/bin" "$T/home/bin" "$T/home/.local/bin" "$T/build"
  printf '#!/bin/sh\necho stub-gt\n' > "$T/build/gt"
  chmod 755 "$T/build/gt"
}

# run_install_local INSTALL_DIR — install-local against a temp HOME, with the
# build and source checks declared up to date. Output in $T/out, rc returned.
#
# INSTALL_GT_SIGN_DIR points install-binary.sh at signing material that does
# not exist, so it installs ad-hoc: a run from inside a town would otherwise
# pick up the town's real keychain through GT_TOWN_ROOT (gt-ykots) and unlock
# it to sign a throwaway binary.
run_install_local() {
  local rc=0
  ( HOME="$T/home" INSTALL_GT_SIGN_DIR="$T/no-signing-material" "$MAKE_BIN" --no-print-directory -C "$ROOT" \
      -o check-up-to-date -o check-forward-only -o build \
      install-local INSTALL_DIR="$1" BUILD_DIR="$T/build" ) > "$T/out" 2>&1 || rc=$?
  return "$rc"
}

echo "=== install-local tests ==="

# --- The acceptance case: INSTALL_DIR is the stale-shadow path itself ---
new_world
printf 'STALE-GO-INSTALL\n' > "$T/home/go/bin/gt"
run_install_local "$T/home/go/bin"; rc=$?
if [[ "$rc" -eq 0 ]] && grep -q 'stub-gt' "$T/home/go/bin/gt" 2>/dev/null; then
  pass "INSTALL_DIR=\$HOME/go/bin keeps the binary it installed"
else
  fail "INSTALL_DIR=\$HOME/go/bin keeps the binary it installed (rc=$rc, dir: $(ls "$T/home/go/bin" 2>&1))"
fi
if [[ "$(cat "$T/home/go/bin/gt" 2>/dev/null)" != "STALE-GO-INSTALL" ]]; then
  pass "the installed bytes are the build output, not the stale placeholder"
else
  fail "the installed bytes are the build output, not the stale placeholder"
fi
# The cleanup must not even try: an install whose INSTALL_DIR the shadow list
# contains used to log "Removing stale <INSTALL_DIR>/gt" and rm it, and only
# the gt-vya0s immutable flag kept the binary alive on macOS — on a host where
# chflags/chattr do not apply, the removal succeeds and the install deletes its
# own output.
if ! grep -q -F "Removing stale $T/home/go/bin/gt" "$T/out"; then
  pass "the cleanup does not name the just-installed path"
else
  fail "the cleanup does not name the just-installed path" "$(cat "$T/out")"
fi
cleanup

# --- Control: a stale shadow somewhere else is still removed ---
new_world
printf 'STALE-GO-INSTALL\n' > "$T/home/bin/gt"
run_install_local "$T/home/.local/bin"; rc=$?
if [[ "$rc" -eq 0 ]] && grep -q 'stub-gt' "$T/home/.local/bin/gt" 2>/dev/null && [[ ! -e "$T/home/bin/gt" ]]; then
  pass "a stale \$HOME/bin/gt is still removed"
else
  fail "a stale \$HOME/bin/gt is still removed (rc=$rc, out: $(tail -3 "$T/out"))"
fi
grep -q "Removing stale $T/home/bin/gt" "$T/out" \
  && pass "the removal is reported" \
  || fail "the removal is reported: $(grep -i removing "$T/out" || echo 'no removal line')"
cleanup

if [[ "$FAIL" -gt 0 ]]; then
  echo "install-local tests: $FAIL failure(s)"
  exit 1
fi
echo "all install-local tests passed"
