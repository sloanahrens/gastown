#!/usr/bin/env bash
# Tests for scripts/uninstall-gt.sh, the recipe docs/INSTALLING.md points at
# (gt-acdfp).
#
# The defect being guarded: the recipe was `rm $(which gt) $(which bd)`.
# `command -v` answers "what runs if I type gt", so a polecat that ran it to
# test the docs deleted the live town install (~/.local/bin/gt and bd) and took
# every agent's bd down for two minutes. The uninstall must name its directory
# explicitly and must refuse to remove the install a town is running from.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
UNINSTALL="$SCRIPT_DIR/uninstall-gt.sh"

WORK_DIR=""
PASS=0
FAIL=0

cleanup() {
  if [[ -n "$WORK_DIR" && -d "$WORK_DIR" ]]; then
    # gt-vya0s: the installed binary is left OS-immutable, and rm -rf cannot
    # remove it (nor the directories above it) until the flag comes off.
    chflags -R nouchg "$WORK_DIR" 2>/dev/null || true
    chattr -R -i "$WORK_DIR" 2>/dev/null || true
    rm -rf "$WORK_DIR"
  fi
}
trap cleanup EXIT

assert_eq() {
  local test_name="$1" expected="$2" actual="$3"
  if [[ "$expected" == "$actual" ]]; then
    echo "  PASS: $test_name"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $test_name"
    echo "    expected: $expected"
    echo "    actual:   $actual"
    FAIL=$((FAIL + 1))
  fi
}

# assert_run NAME EXPECTED_RC ACTUAL_RC — plus the output, on failure only.
assert_run() {
  local test_name="$1" expected="$2" actual="$3" output="${4:-}"
  if [[ "$expected" == "$actual" ]]; then
    echo "  PASS: $test_name"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $test_name (exit $actual, wanted $expected)"
    echo "$output" | sed 's/^/    | /'
    FAIL=$((FAIL + 1))
  fi
}

exists() { [[ -e "$1" || -L "$1" ]]; }

# A fresh fixture: a home, a town with a live daemon pid, and a scratch install
# dir holding stub gt/bd. HOME and GT_TOWN_ROOT are passed per run so nothing
# here ever reads the host's real town.
HOME_DIR=""
TOWN_DIR=""
SCRATCH_BIN=""

setup() {
  # A fresh fixture each time: one test leaves an immutable binary and another
  # removes files, so they must not see each other's leftovers.
  if [[ -n "$WORK_DIR" && -d "$WORK_DIR" ]]; then
    chflags -R nouchg "$WORK_DIR" 2>/dev/null || true
    chattr -R -i "$WORK_DIR" 2>/dev/null || true
    rm -rf "$WORK_DIR"
  fi
  WORK_DIR="$(mktemp -d)"
  HOME_DIR="$WORK_DIR/home"
  TOWN_DIR="$WORK_DIR/town"
  SCRATCH_BIN="$WORK_DIR/scratch-bin"
  mkdir -p "$HOME_DIR/.local/bin" "$TOWN_DIR/mayor" "$TOWN_DIR/daemon" "$SCRATCH_BIN"
  printf '{"name":"fixture"}\n' > "$TOWN_DIR/mayor/town.json"
  # A pid that is certainly alive: this test's own shell.
  printf '%s\nnonce\n' "$$" > "$TOWN_DIR/daemon/daemon.pid"
  printf '#!/bin/sh\necho live-gt\n' > "$HOME_DIR/.local/bin/gt"
  printf '#!/bin/sh\necho live-bd\n' > "$HOME_DIR/.local/bin/bd"
  chmod 755 "$HOME_DIR/.local/bin/gt" "$HOME_DIR/.local/bin/bd"
  printf '#!/bin/sh\necho scratch-gt\n' > "$SCRATCH_BIN/gt"
  printf '#!/bin/sh\necho scratch-bd\n' > "$SCRATCH_BIN/bd"
  chmod 755 "$SCRATCH_BIN/gt" "$SCRATCH_BIN/bd"
}

# run_uninstall [ENV=VAL ...] -- [args ...]
run_uninstall() {
  local envs=()
  while [[ $# -gt 0 && "$1" != "--" ]]; do envs+=("$1"); shift; done
  shift
  # ${envs[@]+...} rather than "${envs[@]}": an empty array under set -u is an
  # error on the bash 3.2 that ships as /bin/bash on macOS.
  env -u INSTALL_GT_BIN_DIR ${envs[@]+"${envs[@]}"} bash "$UNINSTALL" "$@" 2>&1
}

echo "=== uninstall-gt.sh tests ==="

# --- 1. Removes exactly the two binaries an explicit --install-dir names ------
setup
out="$(run_uninstall -- --install-dir "$SCRATCH_BIN")" || rc=$?
rc="${rc:-0}"
assert_run "explicit --install-dir: exit 0" 0 "$rc" "$out"
assert_eq "explicit --install-dir: gt removed" "gone" "$(exists "$SCRATCH_BIN/gt" && echo there || echo gone)"
assert_eq "explicit --install-dir: bd removed" "gone" "$(exists "$SCRATCH_BIN/bd" && echo there || echo gone)"
assert_eq "explicit --install-dir: the directory itself survives" "there" "$(exists "$SCRATCH_BIN" && echo there || echo gone)"
unset rc

# --- 2. Never resolves through command -v (the incident's root cause) ---------
# The live install is on PATH ahead of everything; the recipe must remove the
# directory it was given and leave whatever `command -v` would have answered.
setup
out="$(run_uninstall "PATH=$HOME_DIR/.local/bin:$PATH" GT_TOWN_ROOT="$WORK_DIR/no-such-town" \
  -- --install-dir "$SCRATCH_BIN")" || rc=$?
rc="${rc:-0}"
assert_run "cmd -v shadow: exit 0" 0 "$rc" "$out"
assert_eq "cmd -v shadow: the PATH-resolved gt is untouched" "there" "$(exists "$HOME_DIR/.local/bin/gt" && echo there || echo gone)"
assert_eq "cmd -v shadow: the PATH-resolved bd is untouched" "there" "$(exists "$HOME_DIR/.local/bin/bd" && echo there || echo gone)"
assert_eq "cmd -v shadow: the named directory was emptied" "gone" "$(exists "$SCRATCH_BIN/gt" && echo there || echo gone)"
unset rc

# --- 3. Refuses the live install of a running town ----------------------------
setup
out="$(run_uninstall "HOME=$HOME_DIR" "GT_TOWN_ROOT=$TOWN_DIR" \
  -- --install-dir "$HOME_DIR/.local/bin")" || rc=$?
rc="${rc:-0}"
assert_run "live install: refuses with exit 2" 2 "$rc" "$out"
assert_eq "live install: gt survives the refusal" "there" "$(exists "$HOME_DIR/.local/bin/gt" && echo there || echo gone)"
assert_eq "live install: bd survives the refusal" "there" "$(exists "$HOME_DIR/.local/bin/bd" && echo there || echo gone)"
if [[ "$out" == *"the live install of the town at $TOWN_DIR"* ]]; then
  echo "  PASS: live install: the refusal names the town"
  PASS=$((PASS + 1))
else
  echo "  FAIL: live install: the refusal names the town"
  echo "$out" | sed 's/^/    | /'
  FAIL=$((FAIL + 1))
fi
if [[ "$out" == *"daemon is running now"* ]]; then
  echo "  PASS: live install: the refusal reports the running daemon"
  PASS=$((PASS + 1))
else
  echo "  FAIL: live install: the refusal reports the running daemon"
  echo "$out" | sed 's/^/    | /'
  FAIL=$((FAIL + 1))
fi
unset rc

# --- 3b. A symlinked alias of the live install is the live install -----------
setup
ln -s "$HOME_DIR/.local/bin" "$WORK_DIR/bin-alias"
out="$(run_uninstall "HOME=$HOME_DIR" "GT_TOWN_ROOT=$TOWN_DIR" \
  -- --install-dir "$WORK_DIR/bin-alias")" || rc=$?
rc="${rc:-0}"
assert_run "symlinked alias: refuses with exit 2" 2 "$rc" "$out"
assert_eq "symlinked alias: gt survives the refusal" "there" "$(exists "$HOME_DIR/.local/bin/gt" && echo there || echo gone)"
unset rc

# --- 4. The same directory with no town above it is not the live install ------
setup
out="$(run_uninstall "HOME=$HOME_DIR" "GT_TOWN_ROOT=$WORK_DIR/no-such-town" \
  -- --install-dir "$HOME_DIR/.local/bin")" || rc=$?
rc="${rc:-0}"
assert_run "no town: a bare ~/.local/bin is removed" 0 "$rc" "$out"
assert_eq "no town: gt removed" "gone" "$(exists "$HOME_DIR/.local/bin/gt" && echo there || echo gone)"
unset rc

# --- 4b. A clone outside the town still finds the town in HOME/gt (gt-bqzoq) ---
# GT_TOWN_ROOT unset and the script not under a town used to mean "no town", so
# make uninstall from a clone outside ~/gt removed the live install (the
# 2026-09-30 incident). The conventional HOME/gt town is looked for as well.
setup
mkdir -p "$HOME_DIR/gt/mayor"
printf '{"name":"fixture"}\n' > "$HOME_DIR/gt/mayor/town.json"
out="$(run_uninstall "HOME=$HOME_DIR" "GT_TOWN_ROOT=$WORK_DIR/no-such-town" \
  -- --install-dir "$HOME_DIR/.local/bin")" || rc=$?
assert_run "home town: refuses with exit 2" 2 "${rc:-0}" "$out"
assert_eq "home town: gt survives the refusal" "there" "$(exists "$HOME_DIR/.local/bin/gt" && echo there || echo gone)"
assert_eq "home town: bd survives the refusal" "there" "$(exists "$HOME_DIR/.local/bin/bd" && echo there || echo gone)"
unset rc

# --- 5. FORCE=1 removes the live install anyway, and says so ------------------
setup
out="$(run_uninstall "HOME=$HOME_DIR" "GT_TOWN_ROOT=$TOWN_DIR" \
  -- --install-dir "$HOME_DIR/.local/bin" --force)" || rc=$?
rc="${rc:-0}"
assert_run "FORCE: exit 0" 0 "$rc" "$out"
assert_eq "FORCE: gt removed" "gone" "$(exists "$HOME_DIR/.local/bin/gt" && echo there || echo gone)"
assert_eq "FORCE: bd removed" "gone" "$(exists "$HOME_DIR/.local/bin/bd" && echo there || echo gone)"
assert_eq "FORCE: the town workspace is not touched" "there" "$(exists "$TOWN_DIR/mayor/town.json" && echo there || echo gone)"
if [[ "$out" == *"removing the live install"* ]]; then
  echo "  PASS: FORCE: the run announces what it is doing"
  PASS=$((PASS + 1))
else
  echo "  FAIL: FORCE: the run announces what it is doing"
  echo "$out" | sed 's/^/    | /'
  FAIL=$((FAIL + 1))
fi
unset rc

# --- 6. The install dir's own env var, and the Makefile's default -------------
setup
out="$(run_uninstall "HOME=$HOME_DIR" "GT_TOWN_ROOT=$WORK_DIR/no-such-town" \
  "INSTALL_GT_BIN_DIR=$SCRATCH_BIN" --)" || rc=$?
rc="${rc:-0}"
assert_run "INSTALL_GT_BIN_DIR: exit 0" 0 "$rc" "$out"
assert_eq "INSTALL_GT_BIN_DIR: gt removed" "gone" "$(exists "$SCRATCH_BIN/gt" && echo there || echo gone)"
unset rc

setup
out="$(run_uninstall "HOME=$HOME_DIR" "GT_TOWN_ROOT=$WORK_DIR/no-such-town" --)" || rc=$?
rc="${rc:-0}"
assert_run "default dir: exit 0" 0 "$rc" "$out"
assert_eq "default dir: removes \$HOME/.local/bin/gt" "gone" "$(exists "$HOME_DIR/.local/bin/gt" && echo there || echo gone)"
if [[ "$out" == *"the default (HOME/.local/bin)"* ]]; then
  echo "  PASS: default dir: the run names where the path came from"
  PASS=$((PASS + 1))
else
  echo "  FAIL: default dir: the run names where the path came from"
  echo "$out" | sed 's/^/    | /'
  FAIL=$((FAIL + 1))
fi
unset rc

# --- 7. Uninstalls a binary left OS-immutable (gt-vya0s, gt-2xqtj) ------------
setup
if chflags uchg "$SCRATCH_BIN/gt" 2>/dev/null && [[ -n "$(ls -lO "$SCRATCH_BIN/gt" 2>/dev/null | grep -o uchg)" ]]; then
  out="$(run_uninstall -- --install-dir "$SCRATCH_BIN")" || rc=$?
  rc="${rc:-0}"
  assert_run "immutable gt: exit 0" 0 "$rc" "$out"
  assert_eq "immutable gt: removed despite the flag" "gone" "$(exists "$SCRATCH_BIN/gt" && echo there || echo gone)"
  unset rc
else
  echo "  SKIP: immutable gt (chflags uchg unavailable here)"
fi

# --- 8. Refuses paths it must never delete ------------------------------------
setup
out="$(run_uninstall -- --install-dir "relative/bin")" || rc=$?
rc="${rc:-0}"
assert_run "relative --install-dir: refuses with exit 2" 2 "$rc" "$out"
unset rc

out="$(run_uninstall -- --install-dir "/")" || rc=$?
rc="${rc:-0}"
assert_run "filesystem root: refuses with exit 2" 2 "$rc" "$out"
unset rc

out="$(run_uninstall "HOME=$HOME_DIR" -- --install-dir "$HOME_DIR")" || rc=$?
rc="${rc:-0}"
assert_run "HOME itself: refuses with exit 2" 2 "$rc" "$out"
unset rc

out="$(run_uninstall -- --install-dir "$SCRATCH_BIN" --nonsense)" || rc=$?
rc="${rc:-0}"
assert_run "unknown argument: refuses with exit 2" 2 "$rc" "$out"
unset rc

# --- 9. Makefile target: exists, passes the dir through, needs no town --------
out="$(make --no-print-directory -C "$REPO_ROOT" -n uninstall INSTALL_DIR="$SCRATCH_BIN" 2>&1)" || rc=$?
rc="${rc:-0}"
assert_run "make -n uninstall: exit 0 with no town" 0 "$rc" "$out"
if [[ "$out" == *"--install-dir \"$SCRATCH_BIN\""* ]]; then
  echo "  PASS: make uninstall passes INSTALL_DIR through"
  PASS=$((PASS + 1))
else
  echo "  FAIL: make uninstall passes INSTALL_DIR through"
  echo "$out" | sed 's/^/    | /'
  FAIL=$((FAIL + 1))
fi
if [[ "$out" != *"--force"* ]]; then
  echo "  PASS: make uninstall is not forced by default"
  PASS=$((PASS + 1))
else
  echo "  FAIL: make uninstall is not forced by default"
  echo "$out" | sed 's/^/    | /'
  FAIL=$((FAIL + 1))
fi
out="$(make --no-print-directory -C "$REPO_ROOT" -n uninstall INSTALL_DIR="$SCRATCH_BIN" FORCE=1 2>&1)" || true
if [[ "$out" == *"--force"* ]]; then
  echo "  PASS: make uninstall FORCE=1 adds --force"
  PASS=$((PASS + 1))
else
  echo "  FAIL: make uninstall FORCE=1 adds --force"
  echo "$out" | sed 's/^/    | /'
  FAIL=$((FAIL + 1))
fi
unset rc

echo
echo "=== $PASS passed, $FAIL failed ==="
[[ "$FAIL" -eq 0 ]]
