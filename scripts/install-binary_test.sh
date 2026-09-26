#!/usr/bin/env bash
# Tests for scripts/install-binary.sh, the atomic binary install shared by the
# Makefile's install and safe-install targets (gt-0het).
#
# The defect being guarded: replacing the live binary with `rm -f` + `cp` leaves
# the installed path missing or partial for the duration of the copy, and
# exec'ing a partial Go binary is SIGKILLed on macOS with no output.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
INSTALLER="$SCRIPT_DIR/install-binary.sh"

WORK_DIR=""
DEST=""
PASS=0
FAIL=0

cleanup() {
  if [[ -n "$WORK_DIR" && -d "$WORK_DIR" ]]; then
    # gt-vya0s: the installer leaves the binary OS-immutable; rm -rf cannot
    # remove it (nor the directories above it) until the flag comes off.
    chflags -R nouchg "$WORK_DIR" 2>/dev/null || true
    chattr -R -i "$WORK_DIR" 2>/dev/null || true
    rm -rf "$WORK_DIR"
  fi
}
trap cleanup EXIT

setup() {
  WORK_DIR="$(mktemp -d)"
  DEST="$WORK_DIR/home/.local/bin"
}

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

assert_ok() {
  local test_name="$1" rc="$2"
  if [[ "$rc" -eq 0 ]]; then
    echo "  PASS: $test_name"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: $test_name (exit $rc)"
    FAIL=$((FAIL + 1))
  fi
}

echo "=== install-binary.sh tests ==="

# --- Installs into a missing directory and makes the result executable --------
setup
printf '#!/bin/sh\necho hi\n' > "$WORK_DIR/built-gt"
chmod 755 "$WORK_DIR/built-gt"
rc=0
bash "$INSTALLER" "$WORK_DIR/built-gt" "$DEST" gt || rc=$?
assert_ok "succeeds when the install dir does not exist yet" "$rc"
assert_eq "installs the source content" "#!/bin/sh echo hi" "$(tr '\n' ' ' < "$DEST/gt" | sed 's/ $//')"
if [[ -x "$DEST/gt" ]]; then
  echo "  PASS: installed binary is executable"
  PASS=$((PASS + 1))
else
  echo "  FAIL: installed binary is executable"
  FAIL=$((FAIL + 1))
fi
cleanup

# --- A missing source fails without touching the install dir ------------------
setup
mkdir -p "$DEST"
printf 'OLD\n' > "$DEST/gt"
rc=0
bash "$INSTALLER" "$WORK_DIR/does-not-exist" "$DEST" gt 2>/dev/null || rc=$?
if [[ "$rc" -ne 0 ]]; then
  echo "  PASS: missing source exits non-zero"
  PASS=$((PASS + 1))
else
  echo "  FAIL: missing source exits non-zero"
  FAIL=$((FAIL + 1))
fi
assert_eq "missing source leaves the existing binary alone" "OLD" "$(cat "$DEST/gt")"
assert_eq "missing source leaves no temp file" "" "$(find "$DEST" -name '.gt.tmp.*' -print)"
cleanup

# --- THE REGRESSION: the live path is never missing or partial mid-copy -------
#
# A `cp` shim stalls the copy after the temp file exists but before the rename.
# That is precisely the window the old `rm -f` + `cp` recipe exposed: mid-copy
# the live path was already unlinked, so a concurrent `gt` invocation saw a
# missing (then partial) binary and was SIGKILLed. Sampling the live path inside
# that window is what makes this deterministic rather than a race we hope to
# catch.
setup
mkdir -p "$DEST"
printf 'OLD-COMPLETE-BINARY\n' > "$DEST/gt"
printf 'NEW-COMPLETE-BINARY\n' > "$WORK_DIR/built-gt"

shim="$WORK_DIR/shim"
mkdir -p "$shim"
real_cp="$(command -v cp)"
cat > "$shim/cp" <<EOF
#!/usr/bin/env bash
sleep 2
exec "$real_cp" "\$@"
EOF
chmod +x "$shim/cp"

PATH="$shim:$PATH" bash "$INSTALLER" "$WORK_DIR/built-gt" "$DEST" gt &
installer_pid=$!

sleep 1 # well inside the shimmed copy's 2s stall

if [[ -f "$DEST/gt" ]]; then
  mid="$(cat "$DEST/gt")"
else
  mid="<live path missing>"
fi
assert_eq "live path still holds the complete old binary while a copy is in flight" \
  "OLD-COMPLETE-BINARY" "$mid"

rc=0
wait "$installer_pid" || rc=$?
assert_ok "installer completes once the copy finishes" "$rc"
assert_eq "live path holds the new binary after the copy completes" \
  "NEW-COMPLETE-BINARY" "$(cat "$DEST/gt")"
assert_eq "no temp file left behind" "" "$(find "$DEST" -name '.gt.tmp.*' -print)"
cleanup

# --- Concurrent installs cannot clobber each other's temp file ----------------
#
# With a fixed temp name (the old safe-install), interleaved writers could
# rename a half-written copy into place. Distinct payload sizes make a partial
# result detectable: the final file must match exactly one input, never a mix.
setup
mkdir -p "$DEST"
payload_a="$WORK_DIR/a" payload_b="$WORK_DIR/b" payload_c="$WORK_DIR/c"
head -c 1048576 /dev/zero | tr '\0' 'A' > "$payload_a"
head -c 1048576 /dev/zero | tr '\0' 'B' > "$payload_b"
head -c 524288 /dev/zero | tr '\0' 'C' > "$payload_c"
chmod 755 "$payload_a" "$payload_b" "$payload_c"

for _ in 1 2 3 4 5; do
  bash "$INSTALLER" "$payload_a" "$DEST" gt &
  bash "$INSTALLER" "$payload_b" "$DEST" gt &
  bash "$INSTALLER" "$payload_c" "$DEST" gt &
  wait
done

matched=""
for payload in "$payload_a" "$payload_b" "$payload_c"; do
  if cmp -s "$payload" "$DEST/gt"; then
    matched="$(basename "$payload")"
  fi
done
if [[ -n "$matched" ]]; then
  echo "  PASS: concurrent installs land one complete payload ($matched)"
  PASS=$((PASS + 1))
else
  echo "  FAIL: concurrent installs land one complete payload"
  echo "    installed size: $(wc -c < "$DEST/gt") (expected 1048576 or 524288)"
  FAIL=$((FAIL + 1))
fi
assert_eq "concurrent installs leave no temp files" "" "$(find "$DEST" -name '.gt.tmp.*' -print)"
cleanup

# --- gt-vya0s: installed binary is OS-immutable, not just hook-guarded --------
#
# gt-tnts5 added a Claude Code PreToolUse hook that blocks a polecat's
# Edit/Write/Bash from targeting the install dir, but a local-coder (ollama)
# polecat runs with zero Claude Code hooks (gt-be0z) — the exact seat that
# caused the incident this guards against. The installer also marks the
# binary immutable at the OS level (chflags/chattr) so a write is refused by
# the kernel regardless of which agent or tool attempts it.
#
# `chattr +i` silently no-ops without CAP_LINUX_IMMUTABLE (e.g. non-root CI on
# Linux), so probe for real enforcement first — asserting a rejection that the
# host can't actually produce would be a false PASS, worse than skipping.
immutable_supported() {
  local probe ok=1
  probe="$(mktemp)"
  if chflags uchg "$probe" 2>/dev/null; then
    ok=0
    chflags nouchg "$probe" 2>/dev/null || true
  elif chattr +i "$probe" 2>/dev/null; then
    ok=0
    chattr -i "$probe" 2>/dev/null || true
  fi
  rm -f "$probe"
  return "$ok"
}

if immutable_supported; then
  setup
  mkdir -p "$DEST"
  printf 'OLD\n' > "$WORK_DIR/built-gt"
  chmod 755 "$WORK_DIR/built-gt"
  bash "$INSTALLER" "$WORK_DIR/built-gt" "$DEST" gt

  rc=0
  ( printf 'STUB\n' > "$DEST/gt" ) 2>/dev/null || rc=$?
  if [[ "$rc" -ne 0 ]]; then
    echo "  PASS: a direct write to the installed binary is rejected by the OS"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: a direct write to the installed binary is rejected by the OS"
    FAIL=$((FAIL + 1))
  fi
  assert_eq "the direct write left the installed binary unchanged" "OLD" "$(cat "$DEST/gt")"

  rc=0
  rm -f "$DEST/gt" 2>/dev/null || rc=$?
  if [[ "$rc" -ne 0 ]]; then
    echo "  PASS: rm of the installed binary is rejected by the OS"
    PASS=$((PASS + 1))
  else
    echo "  FAIL: rm of the installed binary is rejected by the OS"
    FAIL=$((FAIL + 1))
  fi
  assert_eq "the rm attempt left the installed binary in place" "OLD" "$(cat "$DEST/gt" 2>/dev/null)"

  printf 'NEW\n' > "$WORK_DIR/built-gt-2"
  chmod 755 "$WORK_DIR/built-gt-2"
  rc=0
  bash "$INSTALLER" "$WORK_DIR/built-gt-2" "$DEST" gt || rc=$?
  assert_ok "the installer itself can still replace an already-immutable binary" "$rc"
  assert_eq "the installer's replacement content lands" "NEW" "$(cat "$DEST/gt")"
  cleanup
else
  echo "  SKIP: OS immutable-flag enforcement unavailable on this host (needs BSD chflags, or chattr with CAP_LINUX_IMMUTABLE) — gt-vya0s checks not run"
fi

# --- Static guard: the non-atomic recipe must not come back -------------------
if grep -qE 'cp[[:space:]]+\$\(BUILD_DIR\)/\$\(BINARY\)[[:space:]]+\$\(INSTALL_DIR\)' "$REPO_ROOT/Makefile"; then
  echo "  FAIL: Makefile copies the binary directly into the install dir (non-atomic)"
  FAIL=$((FAIL + 1))
else
  echo "  PASS: Makefile never copies the binary directly into the install dir"
  PASS=$((PASS + 1))
fi

refs="$(grep -c 'scripts/install-binary.sh' "$REPO_ROOT/Makefile")"
assert_eq "install and safe-install both use install-binary.sh" "2" "$refs"

echo "Results: $PASS passed, $FAIL failed"
[[ "$FAIL" -eq 0 ]] && exit 0 || exit 1
