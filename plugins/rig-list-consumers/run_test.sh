#!/usr/bin/env bash
# Hermetic regression tests for the gt rig list --json consumers:
# gitignore-reconcile, git-hygiene, submodule-commit (gt-chqi, gt-xxwx).
#
# On 2026-09-18 `gt rig list --json` emitted no repo_path field, and all three
# plugins treated "no repo paths" as a routine skip: exit 0 before their own
# record-run, so the daemon serialized the no-op as a SUCCESS receipt and the
# cooldown gate rode a full cycle on top of nothing. A health-check plugin that
# cannot see a single rig repo must FAIL LOUD instead — nonzero exit, a
# failure record, a dog — while a run that genuinely had nothing to do records
# a skipped receipt via the daemon marker ([plugin-result skipped]).
#
# The stub gt is fixture-driven (same pattern as rebuild-gt/run_test.sh):
# rig.json is the fixture the plugin's `gt rig list --json` reads, and every
# `plugin record-run` lands in gt.log so the test can assert on the receipt
# the plugin would have written.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
FAILURES=0
fail() { echo "FAIL: $*"; FAILURES=$((FAILURES + 1)); }
pass() { echo "PASS: $*"; }

# --- Hermetic gt stub ---------------------------------------------------------
# PATH is rebuilt to "$town/bin:/opt/homebrew/bin:/usr/bin:/bin" by run_plugin
# below: the real gt (~/.local/bin) must be unreachable so a stub miss fails
# loudly instead of writing a real plugin-run receipt into the town's beads.
make_town() {
  local town; town=$(mktemp -d)
  mkdir -p "$town/bin"
  cat > "$town/bin/gt" <<'STUB'
#!/usr/bin/env bash
case "$1" in
  rig)
    case "$2" in
      list)
        if [ "${RIG_LIST_FAIL:-0}" = "1" ]; then
          echo "gt: dolt unreachable" >&2
          exit 1
        fi
        cat "$GT_TEST_TOWN/fixture.json"
        ;;
      show)
        cat "$GT_TEST_TOWN/rig-show.json"
        ;;
      *) echo "stub gt: unhandled: $*" >&2; exit 1 ;;
    esac
    ;;
  plugin)
    case "$2" in
      record-run)
        # Log the full argv (plugin record-run --plugin X --result Y ...) so
        # the assertion can match "--result failure" etc.
        echo "$*" >> "$GT_TEST_TOWN/gt.log"
        ;;
      *) echo "stub gt: unhandled: $*" >&2; exit 1 ;;
    esac
    ;;
  *) echo "stub gt: unhandled: $*" >&2; exit 1 ;;
esac
STUB
  cat > "$town/bin/bd" <<'BDB'
#!/usr/bin/env bash
# gitignore-reconcile consults bd for existing/corresponding beads on the dirty
# path. Log the full argv so the test can assert a bead was filed, and fail
# closed (no stdout) like the real bd would for an unknown flag.
echo "bd $*" >> "$GT_TEST_TOWN/gt.log"
exit 0
BDB
  chmod +x "$town/bin/bd"
  chmod +x "$town/bin/gt"
  # Default fixtures: one rig with no repo_path (the 2026-09-18 regression
  # shape) and one rig show with the plugin disabled.
  echo '[{"name": "lilypad_chat", "status": "operational", "repo_path": null}]' > "$town/fixture.json"
  echo '{"name": "lilypad_chat", "plugins": {}}' > "$town/rig-show.json"
  echo "$town"
}

run_plugin() {
  # Capture the exit code without set -e aborting the test script on non-zero.
  local town="$1" script="$2" rc=0
  ( export GT_TEST_TOWN="$town" PATH="$town/bin:/opt/homebrew/bin:/usr/bin:/bin"
    bash "$script" ) > "$town/run.out" 2>&1 || rc=$?
  echo "$rc"
}

rig_log() { # rgrep TOWN PATTERN
  # -e: the pattern starts with "--" and ugrep (this system's grep) otherwise
  # parses it as a flag.
  grep -qF -e "$2" "$1/gt.log" 2>/dev/null
}

for P in gitignore-reconcile git-hygiene submodule-commit; do
  T=$(make_town)
  RUN="$SCRIPT_DIR/../$P/run.sh"

  # --- Case 1: rig list unavailable -> FAIL LOUD, failure receipt -------------
  echo "[]" > "$T/fixture.json"
  rc=$(RIG_LIST_FAIL=1 run_plugin "$T" "$RUN")
  if [ "$rc" -eq 0 ]; then
    fail "$P: broken rig list must not exit 0 (got $rc)"
  else
    pass "$P: broken rig list exits $rc"
  fi
  if rig_log "$T" "--result failure"; then
    pass "$P: failure receipt recorded"
  else
    fail "$P: failure receipt not recorded (gt.log: $(cat "$T/gt.log" 2>/dev/null))"
  fi
  if rig_log "$T" "--result success"; then
    fail "$P: broken rig list serialized as a success receipt"
  fi

  # --- Case 2: rig list returns rigs WITHOUT repo_path (the 09-18 shape) ------
  # -> FAIL LOUD, failure receipt. This is the regression the plugins had.
  rm -f "$T/gt.log"
  rc=$(run_plugin "$T" "$RUN")
  if [ "$rc" -eq 0 ]; then
    fail "$P: rigs without repo_path must not exit 0 (got $rc)"
  else
    pass "$P: rigs without repo_path exit $rc"
  fi
  if rig_log "$T" "--result failure"; then
    pass "$P: failure receipt recorded for missing repo_path"
  else
    fail "$P: failure receipt not recorded for missing repo_path"
  fi

  # --- Case 3: clean repos, nothing to do -> exit 0 + skip marker --------------
  # A receipt is written (it satisfies the cooldown gate) but it says skipped,
  # not success: a run that accomplished nothing must not green-check the
  # plugin history.
  rm -f "$T/gt.log"
  REPO="$T/repo"
  git init -q -b main "$REPO"
  echo x > "$REPO/f.txt"
  git -C "$REPO" add .
  git -C "$REPO" -c user.email=t@t -c user.name=t commit -q -m init
  printf '[{"name": "clean", "status": "operational", "repo_path": %s}]' "\"$REPO\"" > "$T/fixture.json"
  # Disabled plugin config for the opt-in check (submodule-commit).
  echo '{"name": "clean", "plugins": {}}' > "$T/rig-show.json"

  rc=$(run_plugin "$T" "$RUN")
  if [ "$rc" -ne 0 ]; then
    fail "$P: clean run must exit 0 (got $rc)"
  else
    pass "$P: clean run exits 0"
  fi
  if grep -qF "[plugin-result skipped]" "$T/run.out"; then
    pass "$P: skip marker printed"
  else
    fail "$P: skip marker missing from output"
  fi
  if rig_log "$T" "--result skipped"; then
    pass "$P: skipped receipt recorded"
  else
    fail "$P: skipped receipt not recorded (gt.log: $(cat "$T/gt.log" 2>/dev/null))"
  fi
  if rig_log "$T" "--result success"; then
    fail "$P: a run that did nothing serialized as a success receipt"
  fi

  rm -rf "$T"
done

echo ""
if [ "$FAILURES" -eq 0 ]; then
  echo "OK: all git rig-list plugin regression tests passed"
else
  echo "$FAILURES test(s) FAILED"
  exit 1
fi