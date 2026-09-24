# Install gt after each refinery merge, with a fresh refinery session per unit — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** a merged gastown runtime change is installed (`gt version` == the merged commit) within 10 min of landing with no operator action. The daemon picks it up without killing in-flight work, and the refinery starts every landed unit in a fresh session.

**Architecture:**
- **Post-merge hook.** A generic Go hook in `gt mq post-merge` and in `gt mq batch run` runs the rig-configured `post_merge_command`. Gastown's command is `scripts/install-after-merge.sh`, which applies the path denylist and calls `scripts/install-gt.sh`.
- **Shared install script.** `install-gt.sh` is shared with rebuild-gt. It runs under a lock and does: build, backup to `gt.prev`, `safe-install`, smoke test with rollback, syncs, restart marker, receipt.
- **Daemon restart.** The daemon restarts itself once it is idle: it exits 75 through a sentinel error and launchd restarts it on the new binary.
- **MR-B.** After a landed unit, post-merge also does the per-MR chores in Go, closes and re-pours the patrol wisp, and respawns the refinery pane in place.

**Tech stack:** Go (gastown `gt`), bash with perl flock for the scripts, tmux, launchd, beads (`bd`).

**Spec:** `docs/plans/2026-09-23-install-gt-after-merge-design.md` (commits 412e92e…973d4cc). Read it before any task. The spec's Decisions table and Facts section are binding.

## Global Constraints

- **Where the work happens:** entirely in the claude-7fc session, in worktree `~/gt/gastown/crew/sloan-7fc`. **Nothing is slung to polecats; never run `gt sling` on these beads.**
- **Branches:**
  - MR-A: `crew/sloan/claude-7fc-install`.
  - MR-B: `crew/sloan/claude-7fc-unit-cycle`, cut from MR-A's tip and rebased onto `origin/main` after MR-A lands.
  - Both merge **through the gastown refinery** (`gt mq submit`). Never push directly to main.
- **Commits:** no Claude/Anthropic/AI attribution in commit messages, code or docs. After every commit, run `bd comments add claude-7fc "commit: <hash> — <summary>"`, from `~/.claude`.
- **Pushing:** always `git push origin <branch>`, never a bare `git push`.
- **Test runs:**
  - Never gate on a piped test run; capture the exit code.
  - `grep -a` test logs.
  - Run local suites with `GT_TEST_DOCKER=0`.
  - Keep new tests fast; the `internal/cmd` gate is close to its budget.
- **File paths:** `~/gt/daemon/` for `restart-pending.json`, `install-receipts.jsonl`, `install-gt.lock` and `state.json`; `~/.local/bin/gt.prev` for the previous binary.
- **`install-gt.sh` exit codes:**

  | Code | Meaning |
  |---|---|
  | 0 | installed, no-op or skipped |
  | 1 | failed |
  | 2 | refused |
  | 3 | busy (lock or slot) |

  The last stdout line is `install-gt: RESULT <event> <commit> <prev> <reason>`.
- **Marker JSON:** `{"commit","requested_at","source","repo"}`. The daemon may add `attempted_from`.
- **Receipt JSONL fields:** `ts`, `event`, `commit`, `prev_commit`, `source`, `merged_at`, `reason`, `duration_s`.
- **Comparing commits:** always by git ancestry (`merge-base --is-ancestor`), never by string equality. The ldflags commit is short.
- **Where `post_merge_command` comes from:** only the rig's root `config.json`, never the repo or local settings overlays.
- **Nothing is live until the operator steps (Task 12) run:** both config flags ship off.

## Task order

| Group | Tasks |
|---|---|
| MR-A | T1 → T2 → T3 → T4 → T5a → T5b → T6 → T7 (gate + submit) |
| MR-B | T8 → T9 → T10 → T11 (gate + submit) |
| Operator | T12 |

---

## Shared facts (shipping, beads, gate)

Facts every shipping task relies on (checked 2026-09-23 against origin/main ca65b4a and the live town):

- **bd does not resolve in the worktree.** The `claude-7fc` worktree (`~/gt/gastown/crew/sloan-7fc`) has a tracked `.beads/` but no `.beads/redirect`. That file is gitignored (`.gitignore:64`); `crew/sloan` has one containing `../../mayor/rig/.beads`. Without it, `bd` there fails with "no beads database found". `gt mq submit` opens `beads.New(cwd)` (`internal/cmd/mq_submit.go`), so it fails the same way.
- **The rig DB is reachable from `~/gt/gastown`,** which redirects to `mayor/rig/.beads`, prefix `gt`. Never use `bd create --repo`.
- **No git hooks run in this clone.** `core.hooksPath` is `~/gt/gastown/crew/sloan/.beads/hooks`, which does not exist. So `.githooks/pre-push`, which only allows `main`, `polecat/*`, `integration/*` and `beads-sync` without an `upstream` remote, does not run. If a push is ever refused with "Invalid branch for Gas Town agents", the hooks path has been fixed since. Don't bypass it; stop and report.
- **How a crew branch gets into the merge queue:**
  1. Push the branch to origin.
  2. From the worktree, run `gt mq submit --branch <b> --issue <gt-id> --no-cleanup`.

  `gt mq submit` refuses an unpushed branch or a stale SHA (`verifyMQSubmitPushedBranch`). It creates an ephemeral MR wisp labelled `gt:merge-request`, nudges the refinery, and back-links `MR created: <id>` on the source issue. `gt done` is the polecat path; don't use it.
- **gastown's merge-queue config** lives in `~/gt/gastown/config.json` under `.merge_queue`. It is plain JSON, not `gt rig config`, which only reads layered properties.
  - `test_command` = `GOFLAGS=-p=8 make test`
  - `lint_command` = `make lint`
  - `editorial.required` = true: the refinery runs `gt mq review` (om) on the rehearsed branch and re-keys the note on the landed commit.
  - `batch_enabled` = true, `batch_min_count` 3, `batch_min_age` "5m": a lone MR can wait up to 5 min for company.
  - Past hand edits left `config.json.bak-<date>-<reason>` backups beside the file; keep that convention.
- **Where work gets auto-dispatched:**
  - `seat-refill` (`plugins/seat-refill/run.sh:237-251`) picks only **unassigned** `task|bug|feature` beads from `gt ready` with priority ≤ `GT_SEAT_REFILL_MAX_PRIORITY` (default 2).
  - The daemon's mayor-dispatch patrol nudges the mayor about ready work every 30 min.
  - So the MR-A and MR-B source beads are created **assigned to `gastown/crew/sloan`, status `in_progress`**. That keeps them out of `gt ready` and out of seat-refill. Never run `gt sling` on them.
- **Local gate cost:**
  - `make test` runs `test-makefile` (the shell suites), then `go test -timeout 20m ./...`.
  - One full suite alone is about 244 s. `internal/cmd` is about 264 s and `internal/refinery` about 126 s after gt-fo3h.
  - `GT_TEST_DOCKER` defaults to 1 in the recipe, which pulls in the container suite and needs a container-gate slot. Locally run with `GT_TEST_DOCKER=0`, as `gt done`'s gate does (gt-wx53). The refinery's gate runs the container suite under its own slot.
- **Never gate on a piped test run** (`go test | tail` hides the exit code). Always capture `$?` or `PIPESTATUS`, and `grep -a` the logs; they get classified as binary.
- **The refinery pane:** tmux socket `gt-3aa519`, session `gt-refinery`. `#{pane_pid}` changes on every `respawn-pane`. At 20:48 it was 37085.
- **Handoff mail** subjects default to `🤝 HANDOFF: Session cycling` (`handoff.go:1432`). The refinery's own handoffs use `-s` subjects that also start with "🤝 HANDOFF".

---

---

## MR-A: install pipeline

### Task 1: `scripts/install-gt.sh`, the shared install path

**Files:**
- Create: `scripts/lib/install-gt-lib.sh` (receipts, commit reading, result line; sourced by T1 and T2)
- Create: `scripts/install-gt.sh`
- Create: `scripts/install-gt_test.sh` (repo convention is `<name>_test.sh`, as in `scripts/install-binary_test.sh`; the spec's `install_gt_test.sh` is renamed to match)
- Modify: `Makefile:254-282` (`test-makefile` target: add the new syntax checks and tests)

**Interfaces:**
- Consumes: nothing from other tasks.
- Produces (relied on by T2, T5, T6 and the Go hook in T3):
  - CLI: `install-gt.sh --sha <commit> --source post-merge|rebuild-gt [--slot-role <role>] [--slot-timeout <seconds>]`
  - Exit codes: `0` installed, or no-op (the binary already contains `<sha>`); `1` failed (build, install, smoke check, or rollback); `2` refused (dirty, wrong branch, diverged, not forward, unknown commit); `3` busy (install lock, or container-gate slot, not free in time).
  - Last stdout line, always: `install-gt: RESULT <event> <commit> <prev_commit> <reason>`, with `-` for an empty field. `<event>` is one of `installed noop refused failed rolled_back`.
  - Env overrides: `INSTALL_GT_BIN_DIR` (default `$HOME/.local/bin`), `INSTALL_GT_DAEMON_DIR` (default `$GT_TOWN_ROOT/daemon`), `INSTALL_GT_RIG_DIR` (default `$GT_TOWN_ROOT/gastown/mayor/rig`), `INSTALL_GT_LOCK_WAIT` in seconds (default `300`).
  - Restart marker `$DAEMON_DIR/restart-pending.json`, written whole with tmp+rename and only after the smoke check passes: `{"commit": "<full sha>", "requested_at": "<UTC %Y-%m-%dT%H:%M:%SZ>", "source": "post-merge|rebuild-gt", "repo": "<abs path of INSTALL_GT_RIG_DIR>"}`. The daemon (T5) runs its ancestry checks in `repo`, and may rewrite the file to add `attempted_from`. Writers always overwrite the whole file.
  - Receipts: one JSON object per line appended to `$DAEMON_DIR/install-receipts.jsonl`, keys `ts, event, commit, prev_commit, source, merged_at, reason, duration_s`. `ts` and `merged_at` are UTC `%Y-%m-%dT%H:%M:%SZ`. Absent values are `null`. T5 appends `daemon_restarted` lines in this same shape.
  - Lock: `$DAEMON_DIR/install-gt.lock`, held with `flock(2)` via perl. The box has no `flock(1)` (`command -v flock` finds nothing on this Mac); `/usr/bin/perl` always exists on macOS, and a flock is released by the kernel if the holder dies, which a mkdir-lock is not.

Design notes that are not obvious from the spec:
- **Fetch before the no-op check.** The spec orders the no-op check before the fetch. A freshly merged `<sha>` is not in `mayor/rig` until that checkout fetches, so the order is lock → fetch → no-op check → refusals → fast-forward.
- **Build what is actually checked out.** It fast-forwards `mayor/rig` to `<sha>`. If `mayor/rig` is already past `<sha>` (a newer main), `merge --ff-only <sha>` is "Already up to date" and HEAD stays newer. It then builds and verifies HEAD, which contains `<sha>`. That is forward and correct. Verifying against `<sha>` there would fail a correct install.
- **Refusals do not escalate here.** `install-gt.sh` escalates only on exit 1. A refusal or a busy lock is escalated by the caller: the Go hook escalates `post-merge-command:<rig>` on any nonzero exit (T3), and rebuild-gt feeds refusals to its starvation clock (T6). This drops the spec error table's `install-gt:refused` fingerprint as redundant.
- **Smoke messages are chosen on purpose.** Its three failure messages carry the phrases that rebuild-gt's existing tests grep for: "cannot verify what came into force" and "the install did not take".

- [ ] **Step 1: Write the shared library**

Create `scripts/lib/install-gt-lib.sh`:

```bash
#!/usr/bin/env bash
# install-gt-lib.sh — helpers shared by install-gt.sh and install-after-merge.sh.
# Sourced, never run. Callers set DAEMON_DIR and SOURCE before using igt_receipt.

# igt_utc EPOCH — UTC RFC3339 for an epoch, or empty for an empty input.
igt_utc() {
  [ -n "${1:-}" ] || { echo ""; return 0; }
  python3 -c 'import datetime,sys; print(datetime.datetime.fromtimestamp(int(sys.argv[1]), datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"))' "$1"
}

# igt_receipt EVENT COMMIT PREV REASON MERGED_AT_EPOCH START_EPOCH
# Appends one line to $DAEMON_DIR/install-receipts.jsonl. A failed write is
# logged, never fatal: the receipt describes the install, it is not the install.
igt_receipt() {
  mkdir -p "$DAEMON_DIR" 2>/dev/null || true
  python3 - "$DAEMON_DIR/install-receipts.jsonl" "$1" "${2:-}" "${3:-}" "${SOURCE:-}" "${5:-}" "${4:-}" "${6:-}" <<'PY' || echo "[install-gt] WARNING: could not append a receipt to $DAEMON_DIR/install-receipts.jsonl" >&2
import datetime, json, sys, time
path, event, commit, prev, source, merged_epoch, reason, start = sys.argv[1:9]
def utc(t):
    return datetime.datetime.fromtimestamp(t, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
now = time.time()
rec = {
    "ts": utc(now),
    "event": event,
    "commit": commit or None,
    "prev_commit": prev or None,
    "source": source or None,
    "merged_at": utc(int(merged_epoch)) if merged_epoch else None,
    "reason": reason or None,
    "duration_s": int(now - int(start)) if start else None,
}
# One short line per append: O_APPEND keeps concurrent writers (the daemon's
# daemon_restarted receipts) from interleaving inside a line.
with open(path, "a") as f:
    f.write(json.dumps(rec, sort_keys=True) + "\n")
PY
}

# igt_result EVENT COMMIT PREV REASON — the machine-readable last line.
igt_result() {
  echo "install-gt: RESULT ${1:--} ${2:--} ${3:--} ${4:--}"
}

# igt_binary_commit_raw GT DIR — the commit baked into the gt binary at GT, as
# it reports it (usually SHORT), or empty. 'gt stale --json' binary_commit
# first; 'gt version' as the fallback, which from a non-repo cwd prints
# "(dev: <short>)" with no '@' (gt-b5mpe), so both forms are parsed.
igt_binary_commit_raw() {
  local gt="$1" dir="$2" c=""
  [ -x "$gt" ] || { echo ""; return 0; }
  c=$( (cd "$dir" && "$gt" stale --json 2>/dev/null) | python3 -c '
import json, sys
try:
    print(json.load(sys.stdin).get("binary_commit") or "")
except Exception:
    print("")
' 2>/dev/null || true)
  if [ -z "$c" ]; then
    c=$( (cd "$dir" && "$gt" version 2>/dev/null) | python3 -c '
import re, sys
m = re.search(r"\([^:()]+: (?:[^@()]*@)?([0-9a-f]{7,40})\)", sys.stdin.read())
print(m.group(1) if m else "")
' 2>/dev/null || true)
  fi
  echo "$c"
}

# igt_resolve DIR REF — full commit hash of REF inside DIR, or empty.
igt_resolve() {
  [ -n "${2:-}" ] || { echo ""; return 0; }
  git -C "$1" rev-parse --verify --quiet "$2^{commit}" 2>/dev/null || echo ""
}
```

- [ ] **Step 2: Write the failing test harness and the first case**

Create `scripts/install-gt_test.sh`:

```bash
#!/usr/bin/env bash
# Tests for scripts/install-gt.sh: the one install path shared by the
# post-merge hook and rebuild-gt (claude-7fc). A stub make on PATH "installs" by
# writing a stub gt that reports a chosen commit; the stub gt answers
# stale/version/sync/escalate/slot and logs what it was asked.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
INSTALLER="$SCRIPT_DIR/install-gt.sh"
FAILURES=0
fail() { echo "FAIL: $*"; FAILURES=$((FAILURES + 1)); }
pass() { echo "PASS: $*"; }

# gt stub template; __COMMIT__ is the commit this "binary" was built from.
# broken_<commit> makes 'stale --json' print junk; lockout_<commit> makes it
# write-protect the bin dir first (so a rollback cannot rename into it).
write_template() {
  cat > "$1/gt.tmpl" <<'STUB'
#!/usr/bin/env bash
C="__COMMIT__"
case "$1 ${2:-}" in
  "stale --json")
    [ -e "$T_WORLD/lockout_$C" ] && chmod a-w "$T_WORLD/bin"
    if [ -e "$T_WORLD/broken_$C" ]; then echo "not json"; exit 0; fi
    printf '{"stale": false, "binary_commit": "%s"}\n' "$C" ;;
  "version "*)
    if [ -n "$C" ]; then echo "gt version $C (dev: main@$C)"; else echo "gt version dev"; fi ;;
  "formula sync"|"plugin sync") echo "synced $1"; echo "sync $1" >> "$T_WORLD/gt.log" ;;
  "escalate "*) echo "escalate $*" >> "$T_WORLD/gt.log" ;;
  "slot run")
    echo "slot run $*" >> "$T_WORLD/gt.log"
    shift 2
    while [ $# -gt 0 ] && [ "$1" != "--" ]; do shift; done
    [ "${1:-}" = "--" ] && shift
    if [ -e "$T_WORLD/slot_refuse" ]; then echo "timed out waiting for slot" >&2; exit 1; fi
    echo "Container-gate slot acquired (role=test, waited 0s, slot 0/2)."
    exec "$@" ;;
  *) echo "stub gt: unhandled: $*" >&2; exit 1 ;;
esac
STUB
}

# install_stub T COMMIT — put a gt reporting COMMIT at $T/bin/gt.
install_stub() {
  mkdir -p "$1/bin"
  sed "s/__COMMIT__/$2/" "$1/gt.tmpl" > "$1/bin/gt"
  chmod +x "$1/bin/gt"
}

# make_world -> dir with: origin.git, rig (main at C2, C1 its parent), bin/gt
# reporting C1 (short), a stub make on PATH, and C1/C2 recorded in files.
make_world() {
  local t; t=$(mktemp -d)
  git init -q --bare -b main "$t/origin.git"
  git clone -q "$t/origin.git" "$t/rig" 2>/dev/null
  git -C "$t/rig" -c user.email=t@t -c user.name=t commit -q --allow-empty -m c1
  git -C "$t/rig" -c user.email=t@t -c user.name=t commit -q --allow-empty -m c2
  echo tracked > "$t/rig/tracked.txt"
  git -C "$t/rig" add tracked.txt
  git -C "$t/rig" -c user.email=t@t -c user.name=t commit -q -m c3
  git -C "$t/rig" push -q origin main
  git -C "$t/rig" rev-parse --short HEAD~1 > "$t/c1"
  git -C "$t/rig" rev-parse --short HEAD > "$t/c2"
  # rig's local main sits at c1 before the run, so the install must fetch+ff.
  git -C "$t/rig" reset -q --hard HEAD~1
  mkdir -p "$t/stubs" "$t/daemon"
  write_template "$t"
  install_stub "$t" "$(cat "$t/c1")"
  cat > "$t/stubs/make" <<'MAKE'
#!/usr/bin/env bash
echo "make $*" >> "$T_WORLD/make.log"
target="" inst=""
for a in "$@"; do
  case "$a" in
    INSTALL_DIR=*) inst="${a#INSTALL_DIR=}" ;;
    *=*) ;;
    *) target="$a" ;;
  esac
done
case "$target" in
  build) [ -e "$T_WORLD/fail_build" ] && { echo "build failed" >&2; exit 2; }; exit 0 ;;
  safe-install)
    if [ -e "$T_WORLD/already_at_head" ]; then echo "Binary is already at HEAD, nothing to do"; exit 1; fi
    [ -e "$T_WORLD/fail_install" ] && exit 2
    c=$(cat "$T_WORLD/new_commit" 2>/dev/null || git rev-parse --short HEAD)
    sed "s/__COMMIT__/$c/" "$T_WORLD/gt.tmpl" > "$inst/.gt.new"
    chmod +x "$inst/.gt.new"
    mv -f "$inst/.gt.new" "$inst/gt" ;;
  *) echo "stub make: unhandled target $target" >&2; exit 2 ;;
esac
MAKE
  chmod +x "$t/stubs/make"
  echo "$t"
}

# run_install T ARGS... -> exit code; output in $T/run.out
run_install() {
  local t="$1" rc=0; shift
  ( export T_WORLD="$t" INSTALL_GT_BIN_DIR="$t/bin" INSTALL_GT_DAEMON_DIR="$t/daemon" \
      INSTALL_GT_RIG_DIR="$t/rig" INSTALL_GT_LOCK_WAIT="${LOCK_WAIT:-5}" \
      PATH="$t/stubs:/usr/bin:/bin:/opt/homebrew/bin"
    bash "$INSTALLER" "$@" ) > "$t/run.out" 2>&1 || rc=$?
  echo "$rc"
}

# last_receipt T FIELD -> that field of the last receipt line ("None" if null)
last_receipt() {
  tail -1 "$1/daemon/install-receipts.jsonl" 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get(sys.argv[1]))' "$2" 2>/dev/null || echo "NO-RECEIPT"
}

# reported T -> the commit the installed gt stub reports
reported() { "$1/bin/gt" stale --json | python3 -c 'import json,sys; print(json.load(sys.stdin)["binary_commit"])'; }

echo "=== install-gt.sh tests ==="

# --- Case 1: happy path. c1 installed, merge c2 lands -> fetch, ff, build,
# install, smoke, sync, marker, receipt, RESULT line. ---
T=$(make_world)
C2_FULL=$(git -C "$T/origin.git" rev-parse main)
rc=$(run_install "$T" --sha "$C2_FULL" --source post-merge)
[ "$rc" = "0" ] && pass "happy: exit 0" || fail "happy: exit $rc: $(cat "$T/run.out")"
[ "$(reported "$T")" = "$(cat "$T/c2")" ] && pass "happy: c2 in force" || fail "happy: in force is $(reported "$T")"
grep -q "C=\"$(cat "$T/c1")\"" "$T/bin/gt.prev" && pass "happy: gt.prev is the c1 binary" || fail "happy: gt.prev missing or wrong"
[ "$(git -C "$T/rig" rev-parse HEAD)" = "$C2_FULL" ] && pass "happy: rig fast-forwarded" || fail "happy: rig HEAD not at c2"
python3 -c '
import json, sys
m = json.load(open(sys.argv[1]))
assert m["commit"] == sys.argv[2], m
assert m["source"] == "post-merge", m
assert m["repo"] == sys.argv[3], m
assert m["requested_at"].endswith("Z"), m
' "$T/daemon/restart-pending.json" "$C2_FULL" "$T/rig" && pass "happy: marker shape" || fail "happy: marker wrong: $(cat "$T/daemon/restart-pending.json" 2>/dev/null)"
[ "$(last_receipt "$T" event)" = "installed" ] && pass "happy: installed receipt" || fail "happy: receipt $(tail -1 "$T/daemon/install-receipts.jsonl" 2>/dev/null)"
[ "$(last_receipt "$T" commit)" = "$C2_FULL" ] && pass "happy: receipt commit is full" || fail "happy: receipt commit $(last_receipt "$T" commit)"
[ "$(last_receipt "$T" merged_at)" != "None" ] && pass "happy: receipt has merged_at" || fail "happy: no merged_at"
grep -q "sync formula" "$T/gt.log" && grep -q "sync plugin" "$T/gt.log" && pass "happy: syncs ran" || fail "happy: syncs missing: $(cat "$T/gt.log" 2>/dev/null)"
[ "$(tail -1 "$T/run.out")" = "install-gt: RESULT installed $C2_FULL $(git -C "$T/rig" rev-parse HEAD~1) -" ] && pass "happy: RESULT line" || fail "happy: RESULT line: $(tail -1 "$T/run.out")"
grep -q "make SKIP_UPDATE_CHECK=1 INSTALL_DIR=$T/bin safe-install" "$T/make.log" && pass "happy: safe-install into INSTALL_GT_BIN_DIR" || fail "happy: make.log $(cat "$T/make.log")"

if [ "$FAILURES" -ne 0 ]; then echo "$FAILURES failure(s)"; exit 1; fi
echo "all install-gt tests passed"
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `bash scripts/install-gt_test.sh`
Expected: FAIL. `scripts/install-gt.sh` does not exist, so every case fails, e.g. `FAIL: happy: exit 127: bash: .../scripts/install-gt.sh: No such file or directory`, and the script exits 1.

- [ ] **Step 4: Write `scripts/install-gt.sh`**

```bash
#!/usr/bin/env bash
# install-gt.sh — build, install and verify gt at one commit. The single
# install path, shared by the refinery's post-merge hook
# (scripts/install-after-merge.sh) and the rebuild-gt plugin, so there is one
# build under one lock and not two builds racing into one output (claude-7fc;
# design: docs/plans/2026-09-23-install-gt-after-merge-design.md).
#
# Exit codes — callers map these, so change them only together with
# internal/cmd (post-merge hook) and plugins/rebuild-gt/run.sh:
#   0  installed, or nothing to do (the binary already contains the commit)
#   1  failed: build/install failed, or the smoke check failed (rolled back)
#   2  refused: mayor/rig dirty, off main, diverged, not forward, unknown commit
#   3  busy: the install lock or the container-gate slot was not free in time
# The last stdout line is always "install-gt: RESULT <event> <commit> <prev> <reason>".
#
# It never restarts the daemon: it writes daemon/restart-pending.json and the
# daemon exits for a launchd restart once nothing is in flight.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_PATH="$SCRIPT_DIR/$(basename "$0")"
# shellcheck source=lib/install-gt-lib.sh
. "$SCRIPT_DIR/lib/install-gt-lib.sh"

ORIG_ARGS=("$@")
SHA="" SOURCE="" SLOT_ROLE="" SLOT_TIMEOUT=600
while [ $# -gt 0 ]; do
  case "$1" in
    --sha) SHA="${2:-}"; shift 2 ;;
    --source) SOURCE="${2:-}"; shift 2 ;;
    --slot-role) SLOT_ROLE="${2:-}"; shift 2 ;;
    --slot-timeout) SLOT_TIMEOUT="${2:-}"; shift 2 ;;
    *) echo "install-gt: unknown argument: $1" >&2; exit 1 ;;
  esac
done
case "$SOURCE" in post-merge|rebuild-gt) ;; *) echo "install-gt: --source must be post-merge or rebuild-gt" >&2; exit 1 ;; esac
[ -n "$SHA" ] || { echo "install-gt: --sha is required" >&2; exit 1; }

TOWN_ROOT="${GT_TOWN_ROOT:-}"
BIN_DIR="${INSTALL_GT_BIN_DIR:-$HOME/.local/bin}"
DAEMON_DIR="${INSTALL_GT_DAEMON_DIR:-${TOWN_ROOT:?install-gt: GT_TOWN_ROOT or INSTALL_GT_DAEMON_DIR must be set}/daemon}"
RIG_DIR="${INSTALL_GT_RIG_DIR:-${TOWN_ROOT:?install-gt: GT_TOWN_ROOT or INSTALL_GT_RIG_DIR must be set}/gastown/mayor/rig}"
LOCK_WAIT="${INSTALL_GT_LOCK_WAIT:-300}"
GT="$BIN_DIR/gt"
START=$(date +%s)
log() { echo "[install-gt] $*"; }

# escalate SEVERITY FINGERPRINT MESSAGE — through whatever gt is installed at
# that moment (after a rollback, the restored one).
escalate() {
  "$GT" escalate "install-gt: $3" -s "$1" --source "script:install-gt" --fingerprint "$2" >/dev/null 2>&1 \
    || log "WARNING: escalation $2 did not reach the town"
}

# --- Lock ---------------------------------------------------------------------
# flock(2) through perl (macOS has no flock(1)). The script re-execs itself with
# the lock fd inherited ($^F keeps it open across exec), so the kernel drops
# the lock if this process dies — a mkdir-lock would outlive a crash.
if [ -z "${INSTALL_GT_LOCKED:-}" ]; then
  mkdir -p "$DAEMON_DIR"
  rc=0
  INSTALL_GT_LOCKED=1 perl -e '
    use Fcntl qw(:flock);
    $^F = 255;
    my ($lock, $wait, @cmd) = @ARGV;
    open(my $fh, ">>", $lock) or die "install-gt: cannot open $lock: $!\n";
    my $deadline = time + $wait;
    until (flock($fh, LOCK_EX | LOCK_NB)) {
      exit 75 if time >= $deadline;
      select(undef, undef, undef, 0.5);
    }
    exec @cmd or die "install-gt: exec failed: $!\n";
  ' "$DAEMON_DIR/install-gt.lock" "$LOCK_WAIT" bash "$SCRIPT_PATH" "${ORIG_ARGS[@]}" || rc=$?
  if [ "$rc" = "75" ]; then
    log "Another install held the lock for ${LOCK_WAIT}s; not installing $SHA."
    igt_receipt refused "$SHA" "" lock-busy "" "$START"
    igt_result refused "$SHA" "" lock-busy
    exit 3
  fi
  exit "$rc"
fi

# refuse REASON MESSAGE — exit 2, binary untouched.
refuse() {
  log "Refused: $2"
  igt_receipt refused "${FULL_SHA:-$SHA}" "${PREV:-}" "$1" "${MERGED_AT:-}" "$START"
  igt_result refused "${FULL_SHA:-$SHA}" "${PREV:-}" "$1"
  exit 2
}

# --- Resolve the target ---------------------------------------------------------
[ -d "$RIG_DIR/.git" ] || [ -f "$RIG_DIR/.git" ] || { log "No build checkout at $RIG_DIR"; igt_result failed "$SHA" "" no-rig; exit 1; }
git -C "$RIG_DIR" fetch origin --quiet 2>/dev/null || log "WARNING: fetch failed; using local refs"
FULL_SHA=$(igt_resolve "$RIG_DIR" "$SHA")
[ -n "$FULL_SHA" ] || refuse unknown-commit "$SHA is not a commit in $RIG_DIR"
MERGED_AT=$(git -C "$RIG_DIR" log -1 --format=%ct "$FULL_SHA")
PREV=$(igt_resolve "$RIG_DIR" "$(igt_binary_commit_raw "$GT" "$RIG_DIR")")

# --- No-op: the installed binary already contains the commit ----------------------
if [ -n "$PREV" ] && git -C "$RIG_DIR" merge-base --is-ancestor "$FULL_SHA" "$PREV" 2>/dev/null; then
  log "Installed $PREV already contains $FULL_SHA; nothing to do."
  igt_receipt noop "$FULL_SHA" "$PREV" already-installed "$MERGED_AT" "$START"
  igt_result noop "$FULL_SHA" "$PREV" already-installed
  exit 0
fi

# --- Refusals (the rebuild-gt checks, plugins/rebuild-gt/run.sh:366-428) ------------
# Only tracked edits outside .beads/ change what 'make build' produces (gt-50k).
if [ -n "$(git -C "$RIG_DIR" status --porcelain --untracked-files=no -- . ':(exclude).beads' 2>/dev/null)" ]; then
  refuse dirty "$RIG_DIR has uncommitted changes"
fi
BRANCH=$(git -C "$RIG_DIR" branch --show-current 2>/dev/null || true)
[ "$BRANCH" = "main" ] || refuse wrong-branch "$RIG_DIR is on '$BRANCH', not main"
# ff-only, never reset: a real divergence is a human's call (gt-4g1m).
git -C "$RIG_DIR" merge --ff-only "$FULL_SHA" --quiet 2>/dev/null || refuse diverged "local main cannot fast-forward to $FULL_SHA"
if [ "$(git -C "$RIG_DIR" rev-list --count origin/main..HEAD 2>/dev/null || echo 0)" -gt 0 ]; then
  refuse diverged "local main has commits origin/main lacks"
fi
# HEAD can be past FULL_SHA when mayor/rig was already on a newer main; that is
# forward and contains the commit, so HEAD is what gets built and verified.
EXPECTED=$(git -C "$RIG_DIR" rev-parse HEAD)
if [ -n "$PREV" ] && ! git -C "$RIG_DIR" merge-base --is-ancestor "$PREV" "$EXPECTED" 2>/dev/null; then
  refuse not-forward "installed $PREV is not an ancestor of $EXPECTED (would be a downgrade)"
fi

# --- Keep the previous binary ------------------------------------------------------
if [ -f "$GT" ]; then
  cp -p "$GT" "$BIN_DIR/.gt.prev.$$"
  mv -f "$BIN_DIR/.gt.prev.$$" "$BIN_DIR/gt.prev"
fi

# rollback — put gt.prev back atomically; 0 on success.
rollback() {
  [ -f "$BIN_DIR/gt.prev" ] || return 1
  cp -p "$BIN_DIR/gt.prev" "$BIN_DIR/.gt.rollback.$$" 2>/dev/null || return 1
  mv -f "$BIN_DIR/.gt.rollback.$$" "$GT" 2>/dev/null || { rm -f "$BIN_DIR/.gt.rollback.$$"; return 1; }
}

# fail_install REASON MESSAGE SEVERITY FINGERPRINT EVENT — exit 1 after putting
# the old binary back when the installed file changed.
fail_install() {
  log "FAILED: $2"
  if [ -f "$BIN_DIR/gt.prev" ] && ! cmp -s "$BIN_DIR/gt.prev" "$GT"; then
    if rollback; then
      log "Rolled back to $PREV."
    else
      escalate critical install-gt:rollback-failed "$2; and restoring $BIN_DIR/gt.prev FAILED — the town may be running a broken gt"
      igt_receipt failed "$EXPECTED" "$PREV" rollback-failed "$MERGED_AT" "$START"
      igt_result failed "$EXPECTED" "$PREV" rollback-failed
      exit 1
    fi
  fi
  escalate "$3" "$4" "$2"
  igt_receipt "$5" "$EXPECTED" "$PREV" "$1" "$MERGED_AT" "$START"
  igt_result "$5" "$EXPECTED" "$PREV" "$1"
  exit 1
}

# --- Build and install -------------------------------------------------------------
log "Building $EXPECTED in $RIG_DIR (source: $SOURCE)"
if [ -n "$SLOT_ROLE" ]; then
  # Inside the container-gate slot, the way rebuild-gt builds past its
  # starvation threshold (gt-kox0). The acquired literal is slotAcquiredFormat
  # in internal/cmd/slot.go; without it the build never started.
  SLOT_LOG=$(mktemp)
  set +e
  (cd "$RIG_DIR" && "$GT" slot run --role "$SLOT_ROLE" --timeout "${SLOT_TIMEOUT}s" -- make SKIP_UPDATE_CHECK=1 build) 2>&1 | tee "$SLOT_LOG"
  BUILD_RC=${PIPESTATUS[0]}
  set -e
  if ! grep -q "Container-gate slot acquired" "$SLOT_LOG"; then
    rm -f "$SLOT_LOG"
    log "Did not get the container-gate slot within ${SLOT_TIMEOUT}s."
    igt_receipt refused "$EXPECTED" "$PREV" slot-busy "$MERGED_AT" "$START"
    igt_result refused "$EXPECTED" "$PREV" slot-busy
    exit 3
  fi
  rm -f "$SLOT_LOG"
else
  BUILD_RC=0
  (cd "$RIG_DIR" && make SKIP_UPDATE_CHECK=1 build) 2>&1 || BUILD_RC=$?
fi
[ "$BUILD_RC" = "0" ] || fail_install build-failed "make build failed for $EXPECTED" medium install-gt:build-failed failed

INSTALL_RC=0
INSTALL_OUT=$( (cd "$RIG_DIR" && make SKIP_UPDATE_CHECK=1 INSTALL_DIR="$BIN_DIR" safe-install) 2>&1 ) || INSTALL_RC=$?
echo "$INSTALL_OUT"
if [ "$INSTALL_RC" != "0" ]; then
  # check-forward-only exits 1 when the binary is already at HEAD — reachable
  # only when the installed commit could not be read above.
  if printf '%s\n' "$INSTALL_OUT" | grep -q "already at HEAD"; then
    igt_receipt noop "$EXPECTED" "$PREV" already-installed "$MERGED_AT" "$START"
    igt_result noop "$EXPECTED" "$PREV" already-installed
    exit 0
  fi
  fail_install build-failed "make safe-install failed for $EXPECTED" medium install-gt:build-failed failed
fi

# --- Smoke: the gt the town will execute is the commit built --------------------------
if ! (cd "$RIG_DIR" && "$GT" stale --json 2>/dev/null) | python3 -c 'import json,sys; json.load(sys.stdin)' >/dev/null 2>&1; then
  fail_install smoke-failed "the new binary cannot answer 'gt stale --json'" high install-gt:smoke-failed rolled_back
fi
GOT_RAW=$(igt_binary_commit_raw "$GT" "$RIG_DIR")
GOT=$(igt_resolve "$RIG_DIR" "$GOT_RAW")
if [ -z "$GOT" ]; then
  fail_install smoke-failed "installed a build but cannot verify what came into force (read '${GOT_RAW:-nothing}')" high install-gt:smoke-failed rolled_back
fi
if [ "$GOT" != "$EXPECTED" ]; then
  fail_install smoke-failed "the install did not take — $GT reports $GOT, built $EXPECTED" high install-gt:smoke-failed rolled_back
fi

# --- Syncs (non-fatal, as in rebuild-gt: convenience, not the install) ---------------
if OUT=$( (cd "$RIG_DIR" && "$GT" formula sync) 2>&1 ); then log "$OUT"; else log "formula sync failed (non-fatal): $OUT"; fi
if OUT=$( (cd "$RIG_DIR" && "$GT" plugin sync) 2>&1 ); then log "$OUT"; else log "plugin sync failed (non-fatal): $OUT"; fi

# --- Restart marker, receipt ---------------------------------------------------------
python3 - "$DAEMON_DIR/restart-pending.json" "$EXPECTED" "$SOURCE" "$(cd "$RIG_DIR" && pwd)" <<'PY'
import datetime, json, os, sys
path, commit, source, repo = sys.argv[1:5]
m = {"commit": commit,
     "requested_at": datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
     "source": source,
     "repo": repo}
tmp = path + ".tmp.%d" % os.getpid()
with open(tmp, "w") as f:
    json.dump(m, f, indent=2, sort_keys=True)
os.replace(tmp, path)
PY
igt_receipt installed "$EXPECTED" "$PREV" "" "$MERGED_AT" "$START"
"$GT" escalate clear --fingerprint install-gt:build-failed --fingerprint install-gt:smoke-failed \
  --reason "install-gt: $EXPECTED is in force" >/dev/null 2>&1 || true
log "In force: ${PREV:-unknown} -> $EXPECTED. The daemon restarts itself when idle."
igt_result installed "$EXPECTED" "$PREV" ""
```

`repo` uses the logical `pwd`, not `pwd -P`. On macOS, `mktemp -d` paths live under a `/var` → `/private/var` symlink, and the test compares against `$T/rig` exactly as given.

- [ ] **Step 5: Run the test to verify it passes**

Run: `bash scripts/install-gt_test.sh`
Expected: every line `PASS: happy: ...`, then `all install-gt tests passed`, exit 0.

- [ ] **Step 6: Add the remaining cases (no-op, rollback, build failure, lock, refusals, slot)**

Insert before the final `if [ "$FAILURES" ...` block of `scripts/install-gt_test.sh`:

```bash
# --- Case 2: already installed -> no-op: no make, no marker, noop receipt ---
T=$(make_world)
install_stub "$T" "$(cat "$T/c2")"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source post-merge)
[ "$rc" = "0" ] && [ ! -e "$T/make.log" ] && [ ! -e "$T/daemon/restart-pending.json" ] \
  && pass "noop (same): exit 0, no build, no marker" || fail "noop (same): rc=$rc $(cat "$T/run.out")"
[ "$(last_receipt "$T" event)" = "noop" ] && pass "noop (same): noop receipt" || fail "noop (same): receipt $(last_receipt "$T" event)"

# --- Case 3: an OLDER commit than the installed one -> no-op, never a downgrade ---
T=$(make_world)
install_stub "$T" "$(cat "$T/c2")"
rc=$(run_install "$T" --sha "$(cat "$T/c1")" --source rebuild-gt)
[ "$rc" = "0" ] && [ ! -e "$T/make.log" ] && pass "noop (ancestor): exit 0, no build" || fail "noop (ancestor): rc=$rc"

# --- Case 4: the new binary reports an unresolvable commit -> rollback to the
# previous binary, HIGH escalation, no marker. ---
T=$(make_world)
echo deadbee > "$T/new_commit"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source post-merge)
[ "$rc" = "1" ] && pass "smoke unverifiable: exit 1" || fail "smoke unverifiable: rc=$rc $(cat "$T/run.out")"
[ "$(reported "$T")" = "$(cat "$T/c1")" ] && pass "smoke unverifiable: c1 restored" || fail "smoke unverifiable: in force $(reported "$T")"
grep -q "escalate .*cannot verify what came into force.*--fingerprint install-gt:smoke-failed" "$T/gt.log" \
  && pass "smoke unverifiable: escalated" || fail "smoke unverifiable: gt.log $(cat "$T/gt.log" 2>/dev/null)"
grep -q -- "-s high" "$T/gt.log" && pass "smoke unverifiable: HIGH" || fail "smoke unverifiable: not HIGH"
[ ! -e "$T/daemon/restart-pending.json" ] && pass "smoke unverifiable: no marker" || fail "smoke unverifiable: marker written"
[ "$(last_receipt "$T" event)" = "rolled_back" ] && pass "smoke unverifiable: rolled_back receipt" || fail "smoke unverifiable: receipt $(last_receipt "$T" event)"

# --- Case 5: the new binary reports a real but wrong commit -> "did not take" ---
T=$(make_world)
cat "$T/c1" > "$T/new_commit"
# A c1 stub would be byte-identical to the installed one, and the rollback
# only fires when the file changed; the not-take stub differs by a comment.
python3 -c 'import sys; p=sys.argv[1]; s=open(p).read().replace("C=\"__COMMIT__\"", "# not-take\nC=\"__COMMIT__\"", 1); open(p,"w").write(s)' "$T/gt.tmpl"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source post-merge)
[ "$rc" = "1" ] && grep -q "escalate .*the install did not take" "$T/gt.log" \
  && pass "did not take: exit 1, escalated" || fail "did not take: rc=$rc $(cat "$T/gt.log" 2>/dev/null)"

# --- Case 6: new binary cannot answer 'stale --json' -> rollback ---
T=$(make_world)
touch "$T/broken_$(cat "$T/c2")"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source post-merge)
[ "$rc" = "1" ] && [ "$(reported "$T")" = "$(cat "$T/c1")" ] \
  && pass "stale unparseable: rolled back" || fail "stale unparseable: rc=$rc"

# --- Case 7: make build fails -> exit 1, binary untouched, MEDIUM escalation ---
T=$(make_world)
touch "$T/fail_build"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source rebuild-gt)
[ "$rc" = "1" ] && [ "$(reported "$T")" = "$(cat "$T/c1")" ] && pass "build fails: exit 1, untouched" || fail "build fails: rc=$rc"
grep -q -- "--fingerprint install-gt:build-failed" "$T/gt.log" && pass "build fails: escalated" || fail "build fails: no escalation"
[ ! -e "$T/daemon/restart-pending.json" ] && [ "$(last_receipt "$T" event)" = "failed" ] \
  && pass "build fails: no marker, failed receipt" || fail "build fails: marker or receipt wrong"

# --- Case 8: installed commit unreadable and safe-install says "already at
# HEAD" -> no-op, not a failure ---
T=$(make_world)
install_stub "$T" ""
touch "$T/already_at_head"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source rebuild-gt)
[ "$rc" = "0" ] && [ "$(last_receipt "$T" event)" = "noop" ] && pass "already at HEAD: noop" || fail "already at HEAD: rc=$rc"

# --- Case 9: another install holds the lock past the wait -> exit 3, lock-busy ---
T=$(make_world)
perl -e 'use Fcntl qw(:flock); open(my $f, ">>", $ARGV[0]) or die; flock($f, LOCK_EX) or die; sleep 20' "$T/daemon/install-gt.lock" &
HOLDER=$!
sleep 1
rc=$(LOCK_WAIT=1 run_install "$T" --sha "$(cat "$T/c2")" --source post-merge)
kill "$HOLDER" 2>/dev/null || true
[ "$rc" = "3" ] && [ ! -e "$T/make.log" ] && pass "lock busy: exit 3, no build" || fail "lock busy: rc=$rc $(cat "$T/run.out")"
[ "$(last_receipt "$T" reason)" = "lock-busy" ] && pass "lock busy: receipt reason" || fail "lock busy: reason $(last_receipt "$T" reason)"

# --- Case 10: refusals -> exit 2, no build, binary untouched, no escalation ---
T=$(make_world)
# The rig's pre-run HEAD (c2's parent chain before c3) tracks no files, so
# commit one on origin and locally, then dirty it.
echo base > "$T/rig/local.txt"; git -C "$T/rig" add local.txt
git -C "$T/rig" -c user.email=t@t -c user.name=t commit -q -m local-tracked
git -C "$T/rig" push -q origin HEAD:refs/heads/side-seed
echo dirty >> "$T/rig/local.txt"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source post-merge)
[ "$rc" = "2" ] && [ ! -e "$T/make.log" ] && [ "$(last_receipt "$T" reason)" = "dirty" ] \
  && pass "refused dirty" || fail "refused dirty: rc=$rc $(cat "$T/run.out")"
! grep -q "escalate" "$T/gt.log" 2>/dev/null && pass "refused dirty: not escalated here" || fail "refused dirty: escalated"

T=$(make_world)
git -C "$T/rig" checkout -q -b side
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source post-merge)
[ "$rc" = "2" ] && [ "$(last_receipt "$T" reason)" = "wrong-branch" ] && pass "refused wrong branch" || fail "refused wrong branch: rc=$rc"

T=$(make_world)
git -C "$T/rig" -c user.email=t@t -c user.name=t commit -q --allow-empty -m local-only
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source post-merge)
[ "$rc" = "2" ] && [ "$(last_receipt "$T" reason)" = "diverged" ] && pass "refused diverged" || fail "refused diverged: rc=$rc $(cat "$T/run.out")"

T=$(make_world)
rc=$(run_install "$T" --sha 0123456789abcdef0123456789abcdef01234567 --source post-merge)
[ "$rc" = "2" ] && [ "$(last_receipt "$T" reason)" = "unknown-commit" ] && pass "refused unknown commit" || fail "refused unknown commit: rc=$rc"

# --- Case 11: --slot-role builds inside 'gt slot run'; a slot never acquired is busy ---
T=$(make_world)
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source rebuild-gt --slot-role gastown/rebuild-gt --slot-timeout 30)
[ "$rc" = "0" ] && grep -q "slot run --role gastown/rebuild-gt --timeout 30s" "$T/gt.log" \
  && pass "slot role: built inside the slot" || fail "slot role: rc=$rc $(cat "$T/gt.log" 2>/dev/null)"
T=$(make_world)
touch "$T/slot_refuse"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source rebuild-gt --slot-role gastown/rebuild-gt --slot-timeout 30)
[ "$rc" = "3" ] && [ "$(last_receipt "$T" reason)" = "slot-busy" ] && [ "$(reported "$T")" = "$(cat "$T/c1")" ] \
  && pass "slot refused: exit 3, untouched" || fail "slot refused: rc=$rc"

# --- Case 12: the rollback itself fails -> CRITICAL ---
T=$(make_world)
echo deadbee > "$T/new_commit"
touch "$T/lockout_deadbee"
rc=$(run_install "$T" --sha "$(cat "$T/c2")" --source post-merge)
chmod u+w "$T/bin"
[ "$rc" = "1" ] && grep -q -- "-s critical .*--fingerprint install-gt:rollback-failed" "$T/gt.log" \
  && pass "rollback fails: CRITICAL" || fail "rollback fails: rc=$rc $(cat "$T/gt.log" 2>/dev/null)"
```

- [ ] **Step 7: Run all cases**

Run: `bash scripts/install-gt_test.sh`
Expected: every line starts with `PASS:`, the last line is `all install-gt tests passed`, and it exits 0. Any `FAIL:` line names the case and prints the run output.

- [ ] **Step 8: Wire into `make test-makefile`**

In `Makefile`, after the line `	bash scripts/check-deploy-source_test.sh` (currently line 257), add:

```make
	bash -n scripts/install-gt.sh
	bash -n scripts/lib/install-gt-lib.sh
	bash scripts/install-gt_test.sh
```

Run: `make test-makefile 2>&1 | tail -5`
Expected: ends with the last plugin test's success line, exit 0.

- [ ] **Step 9: Commit**

```bash
git add scripts/install-gt.sh scripts/lib/install-gt-lib.sh scripts/install-gt_test.sh Makefile
git commit -m "feat: add install-gt.sh, one locked install path with smoke and rollback

Build, install and verify gt at a commit under a flock, keep the previous
binary and restore it when the smoke check fails, and write the
restart-pending marker and an install receipt instead of restarting the
daemon."
```

---

### Task 2: `scripts/install-after-merge.sh`, gastown's post-merge command

**Files:**
- Create: `scripts/install-after-merge.sh`
- Create: `scripts/install-after-merge_test.sh`
- Modify: `Makefile` `test-makefile` (two lines)

**Interfaces:**
- Consumes (from T1): `scripts/install-gt.sh` CLI and exit codes; `scripts/lib/install-gt-lib.sh` (`igt_receipt`, `igt_binary_commit_raw`, `igt_resolve`); the receipt format.
- Consumes (from T3): the env the Go hook sets: `GT_MERGED_SHA` (the full merge commit, or **empty** when neither the MR's `MergeCommit` nor `origin/<target>` resolved), `GT_RIG`, `GT_TOWN_ROOT`, `GT_MR_IDS` (comma-joined). The cwd is `<rig>/refinery/rig`, so `git` here reads the refinery worktree, which contains `GT_MERGED_SHA`.
- Produces: exit code = install-gt's exit code (it `exec`s), or 0 after a `skipped` receipt, or 2 with a `refused`/`no-merged-sha` receipt when `GT_MERGED_SHA` is empty or unset. Operator step: `merge_queue.post_merge_command = "scripts/install-after-merge.sh"`.

- [ ] **Step 1: Write the failing test**

Create `scripts/install-after-merge_test.sh`:

```bash
#!/usr/bin/env bash
# Tests for scripts/install-after-merge.sh (claude-7fc): the denylist filter
# decides between a 'skipped' receipt and handing the merge to install-gt.sh.
# install-gt.sh is replaced by a stub that records its arguments, so this file
# tests only the filter.
set -euo pipefail

SRC_DIR="$(cd "$(dirname "$0")" && pwd)"
FAILURES=0
fail() { echo "FAIL: $*"; FAILURES=$((FAILURES + 1)); }
pass() { echo "PASS: $*"; }

# make_world -> dir with a repo (base commit B installed), the script under
# test copied into scripts/ beside a recording install-gt.sh stub, and a gt stub.
make_world() {
  local t; t=$(mktemp -d)
  git init -q -b main "$t/repo"
  git -C "$t/repo" -c user.email=t@t -c user.name=t commit -q --allow-empty -m base
  mkdir -p "$t/scripts/lib" "$t/bin" "$t/daemon"
  cp "$SRC_DIR/install-after-merge.sh" "$t/scripts/"
  cp "$SRC_DIR/lib/install-gt-lib.sh" "$t/scripts/lib/"
  cat > "$t/scripts/install-gt.sh" <<'EOF'
#!/usr/bin/env bash
echo "install-gt $*" >> "$T_WORLD/called.log"
exit "${STUB_RC:-0}"
EOF
  chmod +x "$t/scripts/install-gt.sh"
  set_installed "$t" "$(git -C "$t/repo" rev-parse --short HEAD)"
  echo "$t"
}

# set_installed T COMMIT — the installed gt reports COMMIT ("" = unreadable).
set_installed() {
  cat > "$1/bin/gt" <<EOF
#!/usr/bin/env bash
case "\$1 \${2:-}" in
  "stale --json") printf '{"binary_commit": "%s"}\n' "$2" ;;
  "version "*) echo "gt version dev" ;;
  *) exit 1 ;;
esac
EOF
  chmod +x "$1/bin/gt"
}

# land T FILE... — commit changes to FILEs, echo the new full sha
land() {
  local t="$1"; shift
  for f in "$@"; do mkdir -p "$t/repo/$(dirname "$f")"; echo "x$RANDOM" >> "$t/repo/$f"; done
  git -C "$t/repo" add -A
  git -C "$t/repo" -c user.email=t@t -c user.name=t commit -q -m change
  git -C "$t/repo" rev-parse HEAD
}

run_hook() {
  local t="$1" sha="$2" rc=0
  ( cd "$t/repo" && export T_WORLD="$t" GT_MERGED_SHA="$sha" GT_TOWN_ROOT="$t" \
      INSTALL_GT_BIN_DIR="$t/bin" INSTALL_GT_DAEMON_DIR="$t/daemon" \
      PATH="/usr/bin:/bin:/opt/homebrew/bin"
    bash "$t/scripts/install-after-merge.sh" ) > "$t/run.out" 2>&1 || rc=$?
  echo "$rc"
}

last_event() { tail -1 "$1/daemon/install-receipts.jsonl" 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin)["event"])' 2>/dev/null || echo none; }

echo "=== install-after-merge.sh tests ==="

# --- Only denylisted paths changed -> skipped receipt, install-gt not called ---
T=$(make_world)
SHA=$(land "$T" internal/foo_test.go README.md docs/guide.txt .beads/issues.jsonl)
rc=$(run_hook "$T" "$SHA")
[ "$rc" = "0" ] && [ ! -e "$T/called.log" ] && [ "$(last_event "$T")" = "skipped" ] \
  && pass "denylist only: skipped" || fail "denylist only: rc=$rc called=$(cat "$T/called.log" 2>/dev/null) $(cat "$T/run.out")"

# --- A runtime .go file changed -> install-gt with the merge sha ---
T=$(make_world)
SHA=$(land "$T" internal/foo.go README.md)
rc=$(run_hook "$T" "$SHA")
[ "$rc" = "0" ] && grep -q "install-gt --sha $SHA --source post-merge" "$T/called.log" \
  && pass "runtime .go: install-gt called" || fail "runtime .go: rc=$rc $(cat "$T/called.log" 2>/dev/null)"

# --- An unknown path changed -> install (fail toward installing) ---
T=$(make_world)
SHA=$(land "$T" weird/thing.bin)
rc=$(run_hook "$T" "$SHA")
grep -q "install-gt --sha $SHA" "$T/called.log" 2>/dev/null && pass "unknown path: installs" || fail "unknown path: not installed"

# --- Earlier runtime merge not installed, then a docs-only merge -> still
# installs: the diff starts at the INSTALLED commit, not this merge's parent ---
T=$(make_world)
land "$T" internal/runtime.go >/dev/null
SHA=$(land "$T" docs/only.md)
rc=$(run_hook "$T" "$SHA")
grep -q "install-gt --sha $SHA" "$T/called.log" 2>/dev/null && pass "masked runtime: installs" || fail "masked runtime: skipped a pending runtime change"

# --- Installed commit unreadable -> install ---
T=$(make_world)
set_installed "$T" ""
SHA=$(land "$T" docs/only.md)
rc=$(run_hook "$T" "$SHA")
grep -q "install-gt --sha $SHA" "$T/called.log" 2>/dev/null && pass "unreadable installed: installs" || fail "unreadable installed: skipped"

# --- install-gt's exit code is the hook's exit code (it execs) ---
T=$(make_world)
SHA=$(land "$T" internal/foo.go)
rc=$(STUB_RC=2 run_hook "$T" "$SHA")
[ "$rc" = "2" ] && pass "exit code passes through" || fail "exit code: got $rc"

# --- GT_MERGED_SHA empty (the hook could resolve no merge commit) -> exit 2,
# refused receipt no-merged-sha, nothing installed ---
T=$(make_world)
rc=$(run_hook "$T" "")
if [ "$rc" = "2" ] && [ ! -e "$T/called.log" ] \
  && tail -1 "$T/daemon/install-receipts.jsonl" | python3 -c 'import json,sys; r=json.load(sys.stdin); assert r["event"]=="refused" and r["reason"]=="no-merged-sha", r'; then
  pass "empty GT_MERGED_SHA: refused, receipt, exit 2"
else
  fail "empty GT_MERGED_SHA: rc=$rc $(cat "$T/run.out")"
fi

# --- GT_MERGED_SHA unset entirely -> the same refusal ---
T=$(make_world)
rc=0; ( cd "$T/repo" && T_WORLD="$T" GT_TOWN_ROOT="$T" INSTALL_GT_BIN_DIR="$T/bin" INSTALL_GT_DAEMON_DIR="$T/daemon" bash "$T/scripts/install-after-merge.sh" ) >/dev/null 2>&1 || rc=$?
[ "$rc" = "2" ] && [ ! -e "$T/called.log" ] && pass "unset GT_MERGED_SHA: refuses" || fail "unset GT_MERGED_SHA: rc=$rc"

if [ "$FAILURES" -ne 0 ]; then echo "$FAILURES failure(s)"; exit 1; fi
echo "all install-after-merge tests passed"
```

Note: `STUB_RC=2 run_hook ...` passes `STUB_RC` through because `run_hook`'s subshell inherits the caller's environment.

- [ ] **Step 2: Run it to verify it fails**

Run: `bash scripts/install-after-merge_test.sh`
Expected: FAIL. `cp: .../scripts/install-after-merge.sh: No such file or directory`, and `set -e` aborts at the first `make_world`, exiting nonzero. (T1 must already be done: the world copies `scripts/lib/install-gt-lib.sh`.)

- [ ] **Step 3: Write `scripts/install-after-merge.sh`**

```bash
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
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `bash scripts/install-after-merge_test.sh`
Expected: every line starts with `PASS:`, then `all install-after-merge tests passed`, exit 0.

- [ ] **Step 5: Wire into `make test-makefile` and commit**

In `Makefile`, after the T1 lines, add:

```make
	bash -n scripts/install-after-merge.sh
	bash scripts/install-after-merge_test.sh
```

Run: `make test-makefile 2>&1 | tail -3`. Expected: exit 0.

```bash
git add scripts/install-after-merge.sh scripts/install-after-merge_test.sh Makefile
git commit -m "feat: add install-after-merge.sh, the gastown post-merge install command

Skip the install only when every path changed since the installed binary's
commit is a test, markdown, docs or .beads file; hand everything else to
install-gt.sh."
```

---


### Task 3: Config fields, the post-merge command runner, and the single-MR call site

Adds `merge_queue.post_merge_command` / `merge_queue.post_merge_timeout`, a
runner that executes the command best-effort, and the call in
`runMQPostMerge`. The hook is off for every rig until its rig-root
`config.json` sets `post_merge_command`.

**Design note (decided here, flagged to the spec owner):**
`post_merge_command` is honored **only from the rig-root `config.json`**
(`~/gt/<rig>/config.json`, operator-controlled). `MergeSettingsCommand`
(`internal/config/loader.go:355`) overlays the repo tier (`mayor/rig`
settings, i.e. merged repo content) and the local `settings/config.json` tier
field-by-field. We deliberately do **not** add the new fields to that overlay
list, so merged repo content cannot pick the command the refinery runs.
`ResolveMergeQueueConfig` (`internal/rig/manager.go:1127`) copies the rig-root
tier wholesale as the base, so the fields flow through from there with no
resolver change. Gastown's live merge-queue config already lives in
`~/gt/gastown/config.json` `merge_queue`.

**Files:**
- Modify: `internal/config/types.go` (the `MergeQueueConfig` struct ends with the `Editorial` field at :1531; its getters sit at :1592-1725)
- Modify: `internal/config/loader.go:245-305` (`validateMergeQueueConfig`)
- Test: `internal/config/loader_test.go` (append)
- Create: `internal/cmd/post_merge_command.go`
- Create: `internal/cmd/post_merge_command_test.go`
- Modify: `internal/cmd/mq.go:746-774` (`runMQPostMerge`)

**Interfaces:**
- Consumes: `rig.ResolveMergeQueueConfig(townRoot, rigName string) *config.MergeQueueConfig` (`internal/rig/manager.go:1127`); `util.SetProcessGroup(*exec.Cmd)` (`internal/util/exec_unix.go:26`); `refinery.MergeRequest{ID, TargetBranch, MergeCommit}` (`internal/refinery/types.go:15-59`).
- Produces:
  - `config.MergeQueueConfig.PostMergeCommand string` (json `post_merge_command`)
  - `config.MergeQueueConfig.PostMergeTimeout string` (json `post_merge_timeout`)
  - `config.DefaultPostMergeTimeout = 20 * time.Minute`
  - `func (c *MergeQueueConfig) GetPostMergeTimeout() time.Duration` (nil-safe)
  - `type postMergeCommandParams struct { RigName, TownRoot, WorkDir, MergedSHA, Command string; MRIDs []string; Timeout time.Duration; Output io.Writer }`
  - `func runPostMergeCommand(p postMergeCommandParams)` — never returns an error
  - `var postMergeCommandFn = runPostMergeCommand` (test seam used by Task 4)
  - `var postMergeEscalate = escalatePostMergeCommand` (test seam), `func escalatePostMergeCommand(rigName, msg string)`
  - `func postMergeCommandFor(townRoot, rigName, rigPath string, mq *config.MergeQueueConfig, mergedSHA, target string, mrIDs []string, out io.Writer) (postMergeCommandParams, bool)` — `WorkDir = <rigPath>/refinery/rig`; `MergedSHA` falls back to `git rev-parse origin/<target>` in `WorkDir` when empty; may still be `""` if that fails.
  - `func runMRPostMergeCommand(townRoot, rigName, rigPath string, mq *config.MergeQueueConfig, mr *refinery.MergeRequest, out io.Writer)` — the single-MR wrapper.
  - **Call placement:** in `runMQPostMerge`, immediately after `printMQPostMergeResult(result, branchCleanup)` and before `return nil`. The orphan-branch early return (`mq.go:763-766`) is intentionally not covered; the rebuild-gt backstop installs those. **MR-B's `completeUnitAndCycle` goes directly after this call.**
  - The command sees `GT_MERGED_SHA`, `GT_RIG`, `GT_TOWN_ROOT`, `GT_MR_IDS` (comma-joined MR bead IDs) in its environment, runs with cwd `<rig>/refinery/rig`, and its stdout and stderr go to `Output`.

Test commands in this task need the ICU cgo flags that the Makefile sets
(`Makefile:36-43`). Every `go test` below is prefixed with them:
`CGO_CPPFLAGS=-I/opt/homebrew/opt/icu4c@78/include CGO_LDFLAGS=-L/opt/homebrew/opt/icu4c@78/lib`
(abbreviated below as `ICU…`; paste the full prefix).

- [ ] **Step 1: Write the failing config tests**

Append to `internal/config/loader_test.go`:

```go
func TestMergeQueueConfig_GetPostMergeTimeout(t *testing.T) {
	t.Parallel()

	var nilCfg *MergeQueueConfig
	if got := nilCfg.GetPostMergeTimeout(); got != DefaultPostMergeTimeout {
		t.Errorf("nil config: GetPostMergeTimeout() = %v, want %v", got, DefaultPostMergeTimeout)
	}
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", DefaultPostMergeTimeout},
		{"90s", 90 * time.Second},
		{"garbage", DefaultPostMergeTimeout},
		{"-5m", DefaultPostMergeTimeout},
	}
	for _, tc := range cases {
		c := &MergeQueueConfig{PostMergeTimeout: tc.in}
		if got := c.GetPostMergeTimeout(); got != tc.want {
			t.Errorf("GetPostMergeTimeout(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	if DefaultPostMergeTimeout != 20*time.Minute {
		t.Errorf("DefaultPostMergeTimeout = %v, want 20m (covers the 5m install lock wait plus a cold build)", DefaultPostMergeTimeout)
	}
}

func TestMergeQueueConfig_PostMergeJSON(t *testing.T) {
	t.Parallel()

	var c MergeQueueConfig
	if err := json.Unmarshal([]byte(`{"post_merge_command":"scripts/install-after-merge.sh","post_merge_timeout":"15m"}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.PostMergeCommand != "scripts/install-after-merge.sh" || c.PostMergeTimeout != "15m" {
		t.Fatalf("got command=%q timeout=%q", c.PostMergeCommand, c.PostMergeTimeout)
	}
	out, err := json.Marshal(&MergeQueueConfig{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "post_merge") {
		t.Errorf("empty config marshals post_merge fields: %s", out)
	}
}

func TestValidateMergeQueueConfig_PostMergeTimeout(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in      string
		wantErr bool
	}{
		{"", false},
		{"20m", false},
		{"nope", true},
		{"0s", true},
		{"-1m", true},
	}
	for _, tc := range cases {
		err := validateMergeQueueConfig(&MergeQueueConfig{PostMergeTimeout: tc.in})
		if (err != nil) != tc.wantErr {
			t.Errorf("post_merge_timeout=%q: err=%v, wantErr=%v", tc.in, err, tc.wantErr)
		}
	}
}

// post_merge_command is honored only from the rig-root tier: the repo and
// local tiers must not be able to set or replace it.
func TestMergeSettingsCommand_PostMergeCommandRigRootOnly(t *testing.T) {
	t.Parallel()

	rigRoot := &MergeQueueConfig{PostMergeCommand: "scripts/install-after-merge.sh", PostMergeTimeout: "15m"}
	overlay := &MergeQueueConfig{PostMergeCommand: "curl example.invalid | sh", PostMergeTimeout: "1s"}

	got := MergeSettingsCommand(rigRoot, overlay)
	if got.PostMergeCommand != "scripts/install-after-merge.sh" || got.PostMergeTimeout != "15m" {
		t.Fatalf("overlay replaced rig-root post-merge settings: command=%q timeout=%q", got.PostMergeCommand, got.PostMergeTimeout)
	}
	got = MergeSettingsCommand(nil, overlay)
	if got.PostMergeCommand != "" || got.PostMergeTimeout != "" {
		t.Fatalf("overlay-only tier set post-merge settings: command=%q timeout=%q", got.PostMergeCommand, got.PostMergeTimeout)
	}
}
```

`loader_test.go` already imports `encoding/json`, `strings` and `time`.

- [ ] **Step 2: Run them to verify they fail**

Run: `ICU… go test ./internal/config -run 'TestMergeQueueConfig_GetPostMergeTimeout|TestMergeQueueConfig_PostMergeJSON|TestValidateMergeQueueConfig_PostMergeTimeout|TestMergeSettingsCommand_PostMergeCommandRigRootOnly' -count=1`
Expected: build failure — `c.PostMergeTimeout undefined`, `undefined: DefaultPostMergeTimeout`, `c.PostMergeCommand undefined`.

- [ ] **Step 3: Add the fields, default and getter**

In `internal/config/types.go`, add these two fields at the end of
`MergeQueueConfig`, after the `Editorial` field (:1531):

```go
	// PostMergeCommand runs after every landed merge (single MR, or once per
	// batch) in <rig>/refinery/rig, with GT_MERGED_SHA, GT_RIG, GT_TOWN_ROOT
	// and GT_MR_IDS set. Best-effort: a failure or timeout escalates and never
	// fails the merge. Honored only from the rig-root config.json:
	// MergeSettingsCommand deliberately does not overlay it from the repo or
	// local tiers, so merged repo content cannot choose the command.
	PostMergeCommand string `json:"post_merge_command,omitempty"`

	// PostMergeTimeout bounds PostMergeCommand (e.g. "20m"). Empty defaults to
	// DefaultPostMergeTimeout. Rig-root tier only, like PostMergeCommand.
	PostMergeTimeout string `json:"post_merge_timeout,omitempty"`
```

Add the default and getter next to `GetMaxReadyForDispatch` (:1702):

```go
// DefaultPostMergeTimeout covers the install script's 5m lock wait plus a
// cold-cache build, well under the refinery shell tool's 45m ceiling.
const DefaultPostMergeTimeout = 20 * time.Minute

// GetPostMergeTimeout returns the post-merge command timeout. Nil-safe; an
// empty, unparsable or non-positive value yields DefaultPostMergeTimeout.
func (c *MergeQueueConfig) GetPostMergeTimeout() time.Duration {
	if c == nil || c.PostMergeTimeout == "" {
		return DefaultPostMergeTimeout
	}
	d, err := time.ParseDuration(c.PostMergeTimeout)
	if err != nil || d <= 0 {
		return DefaultPostMergeTimeout
	}
	return d
}
```

In `internal/config/loader.go` `validateMergeQueueConfig`, add this before
the final `return nil` (:304):

```go
	if c.PostMergeTimeout != "" {
		dur, err := time.ParseDuration(c.PostMergeTimeout)
		if err != nil {
			return fmt.Errorf("invalid post_merge_timeout: %w", err)
		}
		if dur <= 0 {
			return fmt.Errorf("post_merge_timeout must be positive, got %v", dur)
		}
	}
```

Do **not** touch `MergeSettingsCommand`.

- [ ] **Step 4: Run the config tests to verify they pass**

Run: the same command as Step 2.
Expected: `ok  	github.com/steveyegge/gastown/internal/config`

- [ ] **Step 5: Write the failing runner tests**

Create `internal/cmd/post_merge_command_test.go`:

```go
package cmd

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/refinery"
)

// capturePostMergeEscalations swaps postMergeEscalate for the test's
// duration. Tests that use it swap a package var, so they must not run with
// t.Parallel.
func capturePostMergeEscalations(t *testing.T) *[]string {
	t.Helper()
	var got []string
	orig := postMergeEscalate
	postMergeEscalate = func(rigName, msg string) { got = append(got, rigName+": "+msg) }
	t.Cleanup(func() { postMergeEscalate = orig })
	return &got
}

func capturePostMergeCommandCalls(t *testing.T) *[]postMergeCommandParams {
	t.Helper()
	var calls []postMergeCommandParams
	orig := postMergeCommandFn
	postMergeCommandFn = func(p postMergeCommandParams) { calls = append(calls, p) }
	t.Cleanup(func() { postMergeCommandFn = orig })
	return &calls
}

func TestRunPostMergeCommand_EmptyCommandIsNoop(t *testing.T) {
	esc := capturePostMergeEscalations(t)
	var out bytes.Buffer
	runPostMergeCommand(postMergeCommandParams{RigName: "gastown", WorkDir: t.TempDir(), Output: &out})
	if out.Len() != 0 {
		t.Errorf("empty command wrote output: %q", out.String())
	}
	if len(*esc) != 0 {
		t.Errorf("empty command escalated: %v", *esc)
	}
}

func TestRunPostMergeCommand_PassesEnvAndWorkDir(t *testing.T) {
	esc := capturePostMergeEscalations(t)
	dir := t.TempDir()
	runPostMergeCommand(postMergeCommandParams{
		RigName:   "gastown",
		TownRoot:  "/town",
		WorkDir:   dir,
		MergedSHA: "abc123",
		MRIDs:     []string{"gt-mr1", "gt-mr2"},
		Timeout:   10 * time.Second,
		Output:    io.Discard,
		Command:   `printf '%s|%s|%s|%s|%s\n' "$GT_MERGED_SHA" "$GT_RIG" "$GT_TOWN_ROOT" "$GT_MR_IDS" "$(pwd -P)" > env.txt`,
	})
	if len(*esc) != 0 {
		t.Fatalf("successful command escalated: %v", *esc)
	}
	data, err := os.ReadFile(filepath.Join(dir, "env.txt"))
	if err != nil {
		t.Fatalf("command did not run in WorkDir: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := "abc123|gastown|/town|gt-mr1,gt-mr2|" + resolved
	if got := strings.TrimSpace(string(data)); got != want {
		t.Errorf("env = %q, want %q", got, want)
	}
}

func TestRunPostMergeCommand_FailureEscalatesAndReturns(t *testing.T) {
	esc := capturePostMergeEscalations(t)
	var out bytes.Buffer
	runPostMergeCommand(postMergeCommandParams{
		RigName:   "gastown",
		WorkDir:   t.TempDir(),
		MergedSHA: "abc123",
		Timeout:   10 * time.Second,
		Output:    &out,
		Command:   "echo building; exit 7",
	})
	if !strings.Contains(out.String(), "building") {
		t.Errorf("command output not streamed: %q", out.String())
	}
	if len(*esc) != 1 {
		t.Fatalf("escalations = %v, want exactly 1", *esc)
	}
	if !strings.Contains((*esc)[0], "exit status 7") || !strings.HasPrefix((*esc)[0], "gastown: ") {
		t.Errorf("escalation = %q, want the rig and the exit status", (*esc)[0])
	}
}

func TestRunPostMergeCommand_TimeoutKillsProcessGroup(t *testing.T) {
	esc := capturePostMergeEscalations(t)
	dir := t.TempDir()
	start := time.Now()
	runPostMergeCommand(postMergeCommandParams{
		RigName: "gastown",
		WorkDir: dir,
		Timeout: time.Second,
		Output:  io.Discard,
		Command: `sleep 30 & echo $! > child.pid; wait`,
	})
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("runner took %v; the 1s timeout did not fire", elapsed)
	}
	if len(*esc) != 1 || !strings.Contains((*esc)[0], "timed out") {
		t.Fatalf("escalations = %v, want one 'timed out'", *esc)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "child.pid"))
	if err != nil {
		t.Fatalf("reading child pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parsing child pid %q: %v", raw, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("background child %d survived the timeout: the process group was not killed", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestPostMergeCommandFor_UnconfiguredIsOff(t *testing.T) {
	t.Parallel()
	if _, ok := postMergeCommandFor("/town", "gastown", "/town/gastown", nil, "abc", "main", nil, io.Discard); ok {
		t.Error("nil config: ok = true, want false")
	}
	if _, ok := postMergeCommandFor("/town", "gastown", "/town/gastown", &config.MergeQueueConfig{}, "abc", "main", nil, io.Discard); ok {
		t.Error("empty post_merge_command: ok = true, want false")
	}
}

func TestPostMergeCommandFor_UsesRefineryWorktreeAndMergeCommit(t *testing.T) {
	t.Parallel()
	mq := &config.MergeQueueConfig{PostMergeCommand: "scripts/install-after-merge.sh", PostMergeTimeout: "90s"}
	p, ok := postMergeCommandFor("/town", "gastown", "/town/gastown", mq, "abc123", "main", []string{"gt-mr1"}, io.Discard)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if p.WorkDir != filepath.Join("/town/gastown", "refinery", "rig") {
		t.Errorf("WorkDir = %q, want <rig>/refinery/rig", p.WorkDir)
	}
	if p.MergedSHA != "abc123" || p.Command != "scripts/install-after-merge.sh" || p.Timeout != 90*time.Second {
		t.Errorf("params = %+v", p)
	}
	if p.RigName != "gastown" || p.TownRoot != "/town" || len(p.MRIDs) != 1 || p.MRIDs[0] != "gt-mr1" {
		t.Errorf("params = %+v", p)
	}
}

func TestPostMergeCommandFor_FallsBackToOriginTarget(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	work := filepath.Join(rigPath, "refinery", "rig")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", work, "-c", "user.email=t@example.com", "-c", "user.name=t"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	gitT("init", "-q")
	gitT("commit", "-q", "--allow-empty", "-m", "base")
	head := gitT("rev-parse", "HEAD")
	gitT("update-ref", "refs/remotes/origin/main", head)

	mq := &config.MergeQueueConfig{PostMergeCommand: "true"}
	p, ok := postMergeCommandFor("/town", "gastown", rigPath, mq, "", "main", nil, io.Discard)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if p.MergedSHA != head {
		t.Errorf("MergedSHA = %q, want origin/main %q", p.MergedSHA, head)
	}
}

func TestRunMRPostMergeCommand_CallsOnceWithMRFields(t *testing.T) {
	calls := capturePostMergeCommandCalls(t)
	mq := &config.MergeQueueConfig{PostMergeCommand: "scripts/install-after-merge.sh"}

	runMRPostMergeCommand("/town", "gastown", "/town/gastown", mq, nil, io.Discard)
	if len(*calls) != 0 {
		t.Fatalf("nil MR: calls = %d, want 0", len(*calls))
	}

	mr := &refinery.MergeRequest{ID: "gt-mr1", TargetBranch: "main", MergeCommit: "abc123"}
	runMRPostMergeCommand("/town", "gastown", "/town/gastown", mq, mr, io.Discard)
	if len(*calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(*calls))
	}
	got := (*calls)[0]
	if got.MergedSHA != "abc123" || len(got.MRIDs) != 1 || got.MRIDs[0] != "gt-mr1" {
		t.Errorf("params = %+v", got)
	}

	runMRPostMergeCommand("/town", "gastown", "/town/gastown", &config.MergeQueueConfig{}, mr, io.Discard)
	if len(*calls) != 1 {
		t.Errorf("unconfigured rig: calls = %d, want still 1", len(*calls))
	}
}
```

- [ ] **Step 6: Run them to verify they fail**

Run: `ICU… go test ./internal/cmd -run 'TestRunPostMergeCommand|TestPostMergeCommandFor|TestRunMRPostMergeCommand' -count=1`
Expected: build failure — `undefined: postMergeEscalate`, `undefined: postMergeCommandParams`, `undefined: runPostMergeCommand`, `undefined: postMergeCommandFor`, `undefined: runMRPostMergeCommand`.

- [ ] **Step 7: Write the runner**

Create `internal/cmd/post_merge_command.go`:

```go
package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/util"
)

// postMergeCommandParams describes one run of a rig's post_merge_command.
type postMergeCommandParams struct {
	RigName   string
	TownRoot  string
	WorkDir   string
	MergedSHA string
	Command   string
	MRIDs     []string
	Timeout   time.Duration
	Output    io.Writer
}

// Test seams: swapped by tests, the real implementations in production.
var (
	postMergeCommandFn = runPostMergeCommand
	postMergeEscalate  = escalatePostMergeCommand
)

// runPostMergeCommand runs the rig's post-merge command best-effort. The merge
// has already landed, so nothing here may fail it: a nonzero exit or timeout
// is escalated and swallowed.
func runPostMergeCommand(p postMergeCommandParams) {
	if strings.TrimSpace(p.Command) == "" {
		return
	}
	out := p.Output
	if out == nil {
		out = os.Stdout
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = config.DefaultPostMergeTimeout
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", p.Command)
	cmd.Dir = p.WorkDir
	cmd.Env = append(os.Environ(),
		"GT_MERGED_SHA="+p.MergedSHA,
		"GT_RIG="+p.RigName,
		"GT_TOWN_ROOT="+p.TownRoot,
		"GT_MR_IDS="+strings.Join(p.MRIDs, ","),
	)
	cmd.Stdout = out
	cmd.Stderr = out
	// Own process group so a timeout reaches make/go children, not just bash.
	util.SetProcessGroup(cmd)
	// A killed grandchild can hold the output pipe open; don't wait on it forever.
	cmd.WaitDelay = 5 * time.Second

	fmt.Fprintf(out, "post-merge command (%s @ %s): %s\n", p.RigName, p.MergedSHA, p.Command)
	start := time.Now()
	err := cmd.Run()
	if err == nil {
		fmt.Fprintf(out, "%s post-merge command finished in %s\n", style.Bold.Render("✓"), time.Since(start).Round(time.Second))
		return
	}
	reason := err.Error()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		reason = fmt.Sprintf("timed out after %s", timeout)
	}
	style.PrintWarning("post-merge command failed (%s); the merge stands", reason)
	postMergeEscalate(p.RigName, fmt.Sprintf("post-merge command failed on %s at %s: %s (command: %s)", p.RigName, p.MergedSHA, reason, p.Command))
}

// escalatePostMergeCommand notifies the operator that a rig's post-merge
// command failed. Best-effort, like escalateRubricChange: the merge landed.
func escalatePostMergeCommand(rigName, msg string) {
	cmd := exec.Command("gt", "escalate",
		"--severity", "medium",
		"--reason", "post-merge-command",
		"--source", "refinery:post-merge",
		"--fingerprint", "post-merge-command:"+rigName,
		msg)
	if err := cmd.Run(); err != nil {
		style.PrintWarning("post-merge command escalation failed: %v", err)
	}
}

// postMergeCommandFor builds the run for a rig's configured post-merge
// command. ok is false when the rig configures none. The command runs in the
// refinery's worktree so the script is the merged version; rigPath itself is
// the rig root, which has no scripts/.
func postMergeCommandFor(townRoot, rigName, rigPath string, mq *config.MergeQueueConfig, mergedSHA, target string, mrIDs []string, out io.Writer) (postMergeCommandParams, bool) {
	if mq == nil || strings.TrimSpace(mq.PostMergeCommand) == "" {
		return postMergeCommandParams{}, false
	}
	workDir := filepath.Join(rigPath, "refinery", "rig")
	return postMergeCommandParams{
		RigName:   rigName,
		TownRoot:  townRoot,
		WorkDir:   workDir,
		MergedSHA: resolvePostMergeSHA(workDir, mergedSHA, target),
		Command:   mq.PostMergeCommand,
		MRIDs:     mrIDs,
		Timeout:   mq.GetPostMergeTimeout(),
		Output:    out,
	}, true
}

// resolvePostMergeSHA prefers the recorded merge commit and falls back to the
// target's remote tip. Empty when neither resolves; the command decides.
func resolvePostMergeSHA(workDir, mergedSHA, target string) string {
	if s := strings.TrimSpace(mergedSHA); s != "" {
		return s
	}
	if target == "" {
		return ""
	}
	out, err := exec.Command("git", "-C", workDir, "rev-parse", "--verify", "origin/"+target).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// runMRPostMergeCommand runs the post-merge command for one landed MR.
func runMRPostMergeCommand(townRoot, rigName, rigPath string, mq *config.MergeQueueConfig, mr *refinery.MergeRequest, out io.Writer) {
	if mr == nil {
		return
	}
	if p, ok := postMergeCommandFor(townRoot, rigName, rigPath, mq, mr.MergeCommit, mr.TargetBranch, []string{mr.ID}, out); ok {
		postMergeCommandFn(p)
	}
}
```

- [ ] **Step 8: Run the runner tests to verify they pass**

Run: the same command as Step 6.
Expected: `ok  	github.com/steveyegge/gastown/internal/cmd`. The timeout test takes about 1–3 s.

- [ ] **Step 9: Wire the single-MR call site**

In `internal/cmd/mq.go` `runMQPostMerge`, replace the tail:

```go
	handlePostMergeRubricChange(r.Path, r.Name, rigGit, result.MR, branchCleanup.SubmittedHead)

	printMQPostMergeResult(result, branchCleanup)
	return nil
}
```

with:

```go
	handlePostMergeRubricChange(r.Path, r.Name, rigGit, result.MR, branchCleanup.SubmittedHead)

	printMQPostMergeResult(result, branchCleanup)

	townRoot := filepath.Dir(r.Path)
	runMRPostMergeCommand(townRoot, r.Name, r.Path, rig.ResolveMergeQueueConfig(townRoot, r.Name), result.MR, os.Stdout)
	return nil
}
```

`mq.go` already imports `os`, `path/filepath` and `internal/rig`
(`mq.go:15-20`); `filepath.Dir(r.Path)` is how other callers derive the town
root (`daemon_dispatch.go:529`).

- [ ] **Step 10: Build, vet, and rerun the focused tests**

Run: `ICU… go build ./... && ICU… go vet ./internal/cmd ./internal/config && ICU… go test ./internal/cmd -run 'TestRunPostMergeCommand|TestPostMergeCommandFor|TestRunMRPostMergeCommand|TestRunOrphanMQPostMerge|TestResolveMQPostMerge|TestMQPostMerge' -count=1`
Expected: no build or vet output; `ok  	github.com/steveyegge/gastown/internal/cmd`.

- [ ] **Step 11: Commit**

```bash
git add internal/config/types.go internal/config/loader.go internal/config/loader_test.go internal/cmd/post_merge_command.go internal/cmd/post_merge_command_test.go internal/cmd/mq.go
git commit -m "refinery: run a rig-configured command after each landed merge (claude-7fc)

merge_queue.post_merge_command runs in <rig>/refinery/rig after gt mq
post-merge, with the merged SHA and MR IDs in its environment. Best-effort:
failures and timeouts escalate and never fail the merge. Honored only from
the rig-root config so merged repo content cannot choose the command."
```

---

### Task 4: Batch call site — once per landed batch

`gt mq batch run` lands through `Engineer.ProcessBatch`, not
`runMQPostMerge`, so it needs its own call: exactly once per batch, whenever
the push landed (`result.MergeCommit != ""`), even if some members' cleanup
failed (`result.Error != nil`). It must not go in `HandleMRInfoSuccess`
(`internal/refinery/engineer.go:1841`), which runs once per member
(`batch.go:797`) and also inside `processSingleMR` (`batch.go:548`).

Two details at the call site:
- The batch path's `--json` mode writes its result to stdout, so the
  command's output goes to `cmd.ErrOrStderr()` to keep stdout pure JSON.
- The batch holds the container-gate slot until `runMQBatchRun` returns
  (`mq_batch.go:428-434`). Release it before the install so a 15–20 s build
  doesn't hold a gate slot. `Handle.Release` is idempotent
  (`internal/slot/slot.go:403-408`), so the existing deferred release stays
  harmless.

**Files:**
- Modify: `internal/cmd/post_merge_command.go` (add `runBatchPostMergeCommand`)
- Modify: `internal/cmd/mq_batch.go:436-492` (`runMQBatchRun`, after `ProcessBatch` and the result output, before the `result.Error` return)
- Test: `internal/cmd/post_merge_command_test.go` (append)

**Interfaces:**
- Consumes: `postMergeCommandFor`, `postMergeCommandFn` (Task 3); `refinery.BatchResult{Merged []*MRInfo, MergeCommit string, Error error}` (`internal/refinery/batch.go:43-73`).
- Produces:
  - `func runBatchPostMergeCommand(townRoot, rigName, rigPath string, mq *config.MergeQueueConfig, result *refinery.BatchResult, target string, out io.Writer)`
  - **Call placement:** in `runMQBatchRun`, after the JSON/text result output block and before `if result.Error != nil { return … }`, preceded by an explicit slot release. **MR-B's batch `completeUnitAndCycle` goes directly after this call.**

- [ ] **Step 1: Write the failing tests**

Append to `internal/cmd/post_merge_command_test.go`:

```go
func TestRunBatchPostMergeCommand_OncePerLandedBatch(t *testing.T) {
	calls := capturePostMergeCommandCalls(t)
	mq := &config.MergeQueueConfig{PostMergeCommand: "scripts/install-after-merge.sh"}
	result := &refinery.BatchResult{
		Merged:      []*refinery.MRInfo{{ID: "gt-a"}, {ID: "gt-b"}, {ID: "gt-c"}},
		MergeCommit: "tip123",
		Error:       errors.New("cleanup failed for gt-c"),
	}

	runBatchPostMergeCommand("/town", "gastown", "/town/gastown", mq, result, "main", io.Discard)

	if len(*calls) != 1 {
		t.Fatalf("calls = %d, want exactly 1 per batch", len(*calls))
	}
	got := (*calls)[0]
	if got.MergedSHA != "tip123" {
		t.Errorf("MergedSHA = %q, want the batch tip", got.MergedSHA)
	}
	if strings.Join(got.MRIDs, ",") != "gt-a,gt-b,gt-c" {
		t.Errorf("MRIDs = %v, want every merged member", got.MRIDs)
	}
}

func TestRunBatchPostMergeCommand_SkipsWhenNothingLanded(t *testing.T) {
	calls := capturePostMergeCommandCalls(t)
	mq := &config.MergeQueueConfig{PostMergeCommand: "scripts/install-after-merge.sh"}

	runBatchPostMergeCommand("/town", "gastown", "/town/gastown", mq, nil, "main", io.Discard)
	runBatchPostMergeCommand("/town", "gastown", "/town/gastown", mq, &refinery.BatchResult{Error: errors.New("gate red")}, "main", io.Discard)
	runBatchPostMergeCommand("/town", "gastown", "/town/gastown", &config.MergeQueueConfig{}, &refinery.BatchResult{MergeCommit: "tip123"}, "main", io.Discard)

	if len(*calls) != 0 {
		t.Fatalf("calls = %d, want 0 (nothing landed, or no command configured)", len(*calls))
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `ICU… go test ./internal/cmd -run 'TestRunBatchPostMergeCommand' -count=1`
Expected: build failure — `undefined: runBatchPostMergeCommand`.

- [ ] **Step 3: Implement the helper**

Append to `internal/cmd/post_merge_command.go`:

```go
// runBatchPostMergeCommand runs the post-merge command once for a landed
// batch, at the batch's final pushed SHA. A batch whose push landed runs it
// even when some members' cleanup failed; a batch that pushed nothing doesn't.
func runBatchPostMergeCommand(townRoot, rigName, rigPath string, mq *config.MergeQueueConfig, result *refinery.BatchResult, target string, out io.Writer) {
	if result == nil || strings.TrimSpace(result.MergeCommit) == "" {
		return
	}
	ids := make([]string, 0, len(result.Merged))
	for _, mr := range result.Merged {
		if mr != nil {
			ids = append(ids, mr.ID)
		}
	}
	if p, ok := postMergeCommandFor(townRoot, rigName, rigPath, mq, result.MergeCommit, target, ids, out); ok {
		postMergeCommandFn(p)
	}
}
```

- [ ] **Step 4: Run them to verify they pass**

Run: the same command as Step 2.
Expected: `ok  	github.com/steveyegge/gastown/internal/cmd`

- [ ] **Step 5: Wire the call in `runMQBatchRun`**

In `internal/cmd/mq_batch.go`, replace:

```go
	if result.Error != nil {
		return fmt.Errorf("batch processing error: %w", result.Error)
	}
	return nil
}
```

(the tail of `runMQBatchRun`, :488-492) with:

```go
	// The gate is done; don't hold its slot through the post-merge install.
	// Release is idempotent, so the deferred Release above stays harmless.
	if h != nil {
		_ = h.Release()
	}
	// stderr, not stdout: --json mode's stdout must stay a single JSON document.
	runBatchPostMergeCommand(townRoot, rigName, r.Path, mq, result, target, cmd.ErrOrStderr())

	if result.Error != nil {
		return fmt.Errorf("batch processing error: %w", result.Error)
	}
	return nil
}
```

`townRoot`, `r`, `mq`, `target`, `h` and `result` are all already in scope
(`mq_batch.go:345-436`).

- [ ] **Step 6: Build, vet, and run the batch tests**

Run: `ICU… go build ./... && ICU… go vet ./internal/cmd && ICU… go test ./internal/cmd -run 'TestRunBatchPostMergeCommand|TestAcquireBatchGateSlot_Skips|TestBuildBatchGateSteps|TestBelowBatchMinCount|TestRunPostMergeCommand' -count=1`
Expected: no build or vet output; `ok  	github.com/steveyegge/gastown/internal/cmd`.

- [ ] **Step 7: Commit**

```bash
git add internal/cmd/post_merge_command.go internal/cmd/post_merge_command_test.go internal/cmd/mq_batch.go
git commit -m "refinery: run the post-merge command once per landed batch (claude-7fc)

Batches land through ProcessBatch, not gt mq post-merge. Run the rig's
post-merge command once at the batch tip whenever the push landed, after
releasing the gate slot, with output on stderr so --json stays clean."
```

### Task 5a: The daemon can tell when it is idle, and publishes its own commit

**Files:**
- Modify: `internal/version/stale.go` (add `BuildCommit`, below `resolveCommitHash` at :44-58)
- Modify: `internal/daemon/types.go:49-64` (`State` gets `Commit`)
- Modify: `internal/daemon/plugin_script.go:80-105` (`scriptRunner.runningCount`)
- Modify: `internal/daemon/daemon.go:246` (new `mainBranchTestWaitingSlot` field) and `:656-663` (state stamped with commit at startup)
- Modify: `internal/daemon/main_branch_test_runner.go:1091` (set the flag around the slot wait)
- Create: `internal/daemon/upgrade_idle.go`
- Test: `internal/daemon/upgrade_idle_test.go`

**Interfaces:**
- Produces:
  - `version.BuildCommit() string`: the commit this binary was built from. The ldflag commit is **short** (`Makefile:27`), so every comparison goes through git.
  - `(*Daemon).isIdleForUpgrade() bool`: used by T5b.
  - `(*scriptRunner).runningCount() int`: safe to call on nil.
  - `Daemon.mainBranchTestWaitingSlot atomic.Bool`.
  - **Cross-task (T6):** the `"commit"` field of `$GT_TOWN_ROOT/daemon/state.json`. It is written at daemon startup and rewritten on every heartbeat, because `SaveState` rewrites the whole struct. Its value is the daemon's build commit, fully resolved through `git rev-parse` in `$GT_TOWN_ROOT/gastown/mayor/rig` when that succeeds, otherwise the short build commit. Readers must compare with `git merge-base --is-ancestor`, never with string equality.

- [ ] **Step 1: Write the failing test**

`internal/daemon/upgrade_idle_test.go`:

```go
package daemon

import (
	"io"
	"log"
	"testing"
)

func idleTestDaemon() *Daemon {
	return &Daemon{
		config: &Config{TownRoot: "/tmp/test"},
		logger: log.New(io.Discard, "", 0),
	}
}

func TestIsIdleForUpgrade(t *testing.T) {
	cases := []struct {
		name string
		set  func(d *Daemon)
		want bool
	}{
		{"nothing in flight", func(d *Daemon) {}, true},
		{"script plugin running", func(d *Daemon) {
			d.scripts = newScriptRunner()
			d.scripts.tryStart("rebuild-gt")
		}, false},
		{"script runner finished", func(d *Daemon) {
			d.scripts = newScriptRunner()
			d.scripts.tryStart("rebuild-gt")
			d.scripts.finish("rebuild-gt")
		}, true},
		{"compactor dog running", func(d *Daemon) { d.compactorDogRunning = true }, false},
		{"boot triage in flight", func(d *Daemon) { d.bootTriageInFlight.Store(true) }, false},
		{"scheduled slings running", func(d *Daemon) { d.scheduledSlingsRunning.Store(true) }, false},
		{"mayor dispatch running", func(d *Daemon) { d.mayorDispatchRunning.Store(true) }, false},
		{"patrol watchdog running", func(d *Daemon) { d.patrolWatchdogRunning.Store(true) }, false},
		{"main branch test mid-run", func(d *Daemon) { d.mainBranchTestRunning.Store(true) }, false},
		{"main branch test waiting for a slot", func(d *Daemon) {
			d.mainBranchTestRunning.Store(true)
			d.mainBranchTestWaitingSlot.Store(true)
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := idleTestDaemon()
			tc.set(d)
			if got := d.isIdleForUpgrade(); got != tc.want {
				t.Fatalf("isIdleForUpgrade() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestScriptRunnerRunningCountNilSafe(t *testing.T) {
	var r *scriptRunner
	if got := r.runningCount(); got != 0 {
		t.Fatalf("nil runner runningCount() = %d, want 0", got)
	}
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `go test ./internal/daemon -run 'TestIsIdleForUpgrade|TestScriptRunnerRunningCountNilSafe' -count=1`

Expected: a build failure, `d.mainBranchTestWaitingSlot undefined`, `d.isIdleForUpgrade undefined` and `r.runningCount undefined`.

- [ ] **Step 3: Implement**

`internal/version/stale.go`, directly after `resolveCommitHash`:

```go
// BuildCommit returns the commit this binary was built from: the ldflag value
// (short, see Makefile COMMIT) or the module's vcs.revision. Empty when neither
// is available. Compare it with git ancestry, not string equality.
func BuildCommit() string {
	return resolveCommitHash()
}
```

`internal/daemon/types.go`, a new field at the end of `State`:

```go
	// Commit is the build commit of the running daemon, fully resolved when
	// the gastown source repo is available (see resolveOwnCommit). rebuild-gt
	// reads it to tell whether the daemon is running the installed binary.
	Commit string `json:"commit,omitempty"`
```

`internal/daemon/plugin_script.go`, after `finish`:

```go
// runningCount reports how many script plugins are in flight. Safe on a nil
// runner: the daemon creates it lazily (scriptsOnce).
func (r *scriptRunner) runningCount() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.running)
}
```

`internal/daemon/daemon.go`, right after `mainBranchTestRunning atomic.Bool` (:246):

```go
	// mainBranchTestWaitingSlot is true only while a main_branch_test run is
	// blocked in acquireMainBranchTestSlot. Killing a run in that state costs
	// nothing (an interrupted run is a non-verdict, gt-59yz), so
	// isIdleForUpgrade treats it as idle.
	mainBranchTestWaitingSlot atomic.Bool
```

`internal/daemon/main_branch_test_runner.go:1091`. Replace

```go
	h, err := acquireMainBranchTestSlot(d.config.TownRoot, rigName)
```

with

```go
	d.mainBranchTestWaitingSlot.Store(true)
	h, err := acquireMainBranchTestSlot(d.config.TownRoot, rigName)
	d.mainBranchTestWaitingSlot.Store(false)
```

Create `internal/daemon/upgrade_idle.go`:

```go
package daemon

import (
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/version"
)

// isIdleForUpgrade reports whether restarting the daemon now would kill no
// in-flight work: no script plugin, compactor, boot triage, scheduled
// slings, mayor dispatch or patrol watchdog run, and no main_branch_test
// past its slot wait. pourDoctorMolecule and the Dolt goroutines are not
// counted: they are short or restartable.
func (d *Daemon) isIdleForUpgrade() bool {
	if d.scripts.runningCount() > 0 {
		return false
	}
	d.compactorDogMu.Lock()
	compactor := d.compactorDogRunning
	d.compactorDogMu.Unlock()
	if compactor {
		return false
	}
	if d.bootTriageInFlight.Load() || d.scheduledSlingsRunning.Load() ||
		d.mayorDispatchRunning.Load() || d.patrolWatchdogRunning.Load() {
		return false
	}
	if d.mainBranchTestRunning.Load() && !d.mainBranchTestWaitingSlot.Load() {
		return false
	}
	return true
}

// buildCommitFn is the daemon's own build commit; a test seam.
var buildCommitFn = version.BuildCommit

// resolveOwnCommit returns the full SHA of this build when the gastown source
// repo can resolve it, else the (short) build commit.
func (d *Daemon) resolveOwnCommit() string {
	own := buildCommitFn()
	if own == "" {
		return ""
	}
	repo := filepath.Join(d.config.TownRoot, "gastown", "mayor", "rig")
	out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", own+"^{commit}").Output()
	if err != nil {
		return own
	}
	return strings.TrimSpace(string(out))
}
```

`internal/daemon/daemon.go:656-660`. The startup state gets the commit:

```go
	state := &State{
		Running:   true,
		PID:       os.Getpid(),
		StartedAt: time.Now(),
		Commit:    d.resolveOwnCommit(),
	}
```

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/daemon -run 'TestIsIdleForUpgrade|TestScriptRunnerRunningCountNilSafe' -count=1`

Expected: `ok  	github.com/steveyegge/gastown/internal/daemon`

Run: `go build ./... && go vet ./internal/daemon ./internal/version`

Expected: no output.

- [ ] **Step 5: Commit**

```bash
git add internal/version/stale.go internal/daemon/types.go internal/daemon/plugin_script.go \
  internal/daemon/daemon.go internal/daemon/main_branch_test_runner.go \
  internal/daemon/upgrade_idle.go internal/daemon/upgrade_idle_test.go
git commit -m "daemon: idle predicate for upgrade restarts; record build commit in state"
```

---

### Task 5b: Restart once idle when a restart marker is present, and exit 75

**Files:**
- Create: `internal/daemon/upgrade_restart.go`
- Test: `internal/daemon/upgrade_restart_test.go`
- Modify: `internal/daemon/daemon.go`:
  - `heartbeat` (:1123) splits into a guard and `heartbeatWork`;
  - the run loop's initial heartbeat (:946-947) and `case <-timer.C:` (:1099-1103);
  - a new `upgradeRestartRequested` field next to `mainBranchTestWaitingSlot`;
  - the startup check after the state save (:661-663).
- Modify: `internal/cmd/daemon.go:487` (`runDaemonRun` goes through `daemonRunExit`)
- Test: `internal/cmd/daemon_test.go` (add `TestDaemonRunExitMapsUpgradeTo75`)

**Interfaces:**
- Consumes: `(*Daemon).isIdleForUpgrade()`, `(*Daemon).resolveOwnCommit()` and `buildCommitFn` from T5a.
- Produces:
  - `daemon.ErrRestartForUpgrade`, returned by `Run` after a normal `shutdown`.
  - `cmd.daemonExit` (`= os.Exit`) and `cmd.daemonRunExit(err error) error`.
- **Cross-task (T1 writes, T5b reads):** `$GT_TOWN_ROOT/daemon/restart-pending.json`. T5b adds one optional field to the spec's format, `repo`:

  ```json
  {"commit":"<full sha>","requested_at":"<UTC RFC3339>","source":"post-merge|rebuild-gt","repo":"<abs path of the checkout that built it, i.e. mayor/rig>"}
  ```

  `install-gt.sh` (T1) must write `repo`. The daemon adds `attempted_from` (its own commit) just before it exits, to guard against a restart loop. Writers must preserve unknown fields or overwrite the whole file.
- **Cross-task (T1 and T2 write, T5b writes):** a receipt line in `$GT_TOWN_ROOT/daemon/install-receipts.jsonl`:

  ```json
  {"ts":"<UTC RFC3339>","event":"daemon_restarted","commit":"<marker commit>","prev_commit":"<attempted_from or empty>","source":"<marker source>","merged_at":"<committer time of commit, or empty>","reason":"","duration_s":<seconds from requested_at to now>}
  ```

  `merged_at` comes from one `git show -s --format=%cI <commit>` in `marker.repo`. It is empty when that fails. The call runs once per restart, so the cost is negligible.

**Design notes for the implementer:**

- **Ancestry decides everything, in `marker.repo`:**
  - A marker is *covered* when `isAncestor(marker.commit, own)`. That includes equality, because `merge-base --is-ancestor A A` is true.
  - It is *newer* when `isAncestor(own, marker.commit) && !covered`.
  - When there is no `repo` or git fails, fall back to prefix equality: covered if either SHA is a prefix of the other, otherwise newer. The `attempted_from` guard below bounds the cost of a wrong "newer".
- **Covered markers clear at every heartbeat and at startup,** with a `daemon_restarted` receipt. This is review item 8: the daemon can restart onto binary Y before Y's marker is written.
- **A newer marker when idle:** set `attempted_from = own`, rewrite the marker, and set `d.upgradeRestartRequested`. `heartbeat` then returns before `heartbeatWork`, so no plugin is dispatched on that tick. The run loop calls `d.shutdown(state)`, the same path as SIGTERM, and returns `ErrRestartForUpgrade`.
- **An upgrade restart never stops Dolt.** Findings:
  1. **The live town doesn't have the daemon manage Dolt at all.** `mayor/daemon.json` has no `dolt_server` patrol, so `d.doltServer` is nil (`daemon.go:448-458`). The shutdown stop at `daemon.go:2879` is therefore a no-op here. Dolt is started by the launchd one-shot that runs `gt dolt start`.
  2. **daemon.log confirms it.** All roughly 40 restarts since 09-20 show `Daemon stopped` then `Daemon starting` with no `Stopping Dolt` line (for example 20:06:47 and 20:48:44).
  3. **A daemon-managed Dolt would survive a restart anyway.**
     - `startLocked` detaches it (`setSysProcAttr`, commented "so it survives daemon restart", `dolt.go:~960`), and writes `daemon/dolt.pid`.
     - On startup, `isRunning` adopts it through the pid-file nonce plus a port probe (`dolt.go:375-409`), and `startLocked` re-checks and logs "already running, skipping start".
     - Only `shutdown` explicitly stops it.
  4. **So skipping the stop is safe, and required.** Nothing depends on the old daemon stopping Dolt: the new daemon adopts it. Pushing to the Dolt remotes (`pushDoltRemotesBounded`) stays, since it's harmless and bounded.

  `shutdown` checks `!d.upgradeRestartRequested.Load()` before it stops Dolt, so a town that does turn on `dolt_server` never has Dolt bounced several times an hour. SIGTERM and `gt daemon stop` still stop it as they do today.
- **A newer marker whose `attempted_from == own`:** the restart already happened and the binary didn't change, for example the marker names a commit that was never installed. Do not exit again. Escalate once under `daemon:restart-pending-no-effect`.
- **A newer marker while busy:** remember when this marker was first seen. After 30 min, escalate once under `daemon:restart-pending-stuck` and keep waiting.
- **Severity differs from the spec.** `escalateAlert` always passes `-s HIGH` (`jsonl_git_backup.go:974`), and the spec says MEDIUM. The plan keeps `escalateAlert` rather than add a severity parameter to a shared helper. It is behind a seam (`upgradeEscalateFn`).
- **Concurrency:** these fields are touched only from the run-loop goroutine (startup and `heartbeat`), so they need no mutex.

- [ ] **Step 1: Write the failing tests**

`internal/daemon/upgrade_restart_test.go`:

```go
package daemon

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeHistory answers isAncestor from a linear history: index order is
// ancestry order (a..b..c).
func fakeHistory(t *testing.T, commits ...string) {
	t.Helper()
	pos := map[string]int{}
	for i, c := range commits {
		pos[c] = i
	}
	orig := isAncestorFn
	isAncestorFn = func(repo, ancestor, descendant string) (bool, bool) {
		a, okA := pos[ancestor]
		b, okB := pos[descendant]
		if !okA || !okB {
			return false, false
		}
		return a <= b, true
	}
	t.Cleanup(func() { isAncestorFn = orig })
}

func withOwnCommit(t *testing.T, c string) {
	t.Helper()
	orig := buildCommitFn
	buildCommitFn = func() string { return c }
	t.Cleanup(func() { buildCommitFn = orig })
}

func captureEscalations(t *testing.T) *[]string {
	t.Helper()
	var keys []string
	orig := upgradeEscalateFn
	upgradeEscalateFn = func(d *Daemon, key, msg string) { keys = append(keys, key) }
	t.Cleanup(func() { upgradeEscalateFn = orig })
	return &keys
}

func upgradeTestDaemon(t *testing.T) *Daemon {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0)}
}

func writeMarker(t *testing.T, d *Daemon, m restartPendingMarker) {
	t.Helper()
	data, _ := json.Marshal(m)
	if err := os.WriteFile(restartMarkerPath(d.config.TownRoot), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readReceipts(t *testing.T, d *Daemon) []installReceipt {
	t.Helper()
	f, err := os.Open(installReceiptsPath(d.config.TownRoot))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []installReceipt
	s := bufio.NewScanner(f)
	for s.Scan() {
		var r installReceipt
		if err := json.Unmarshal(s.Bytes(), &r); err != nil {
			t.Fatalf("bad receipt line %q: %v", s.Text(), err)
		}
		out = append(out, r)
	}
	return out
}

func TestUpgradeCoveredMarkerClearedWithReceipt(t *testing.T) {
	for _, tc := range []struct{ name, marker string }{
		{"equal", "bbb"},
		{"ancestor", "aaa"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeHistory(t, "aaa", "bbb", "ccc")
			withOwnCommit(t, "bbb")
			captureEscalations(t)
			d := upgradeTestDaemon(t)
			writeMarker(t, d, restartPendingMarker{Commit: tc.marker, Source: "post-merge",
				RequestedAt: time.Now().Add(-90 * time.Second).UTC().Format(time.RFC3339), Repo: "/repo"})

			if d.checkUpgradeRestart(time.Now()) {
				t.Fatal("covered marker must not request a restart")
			}
			if _, err := os.Stat(restartMarkerPath(d.config.TownRoot)); !os.IsNotExist(err) {
				t.Fatalf("covered marker not deleted (stat err=%v)", err)
			}
			rs := readReceipts(t, d)
			if len(rs) != 1 || rs[0].Event != "daemon_restarted" || rs[0].Commit != tc.marker {
				t.Fatalf("receipts = %+v, want one daemon_restarted for %s", rs, tc.marker)
			}
			if rs[0].DurationS < 89 {
				t.Fatalf("duration_s = %v, want >= 89", rs[0].DurationS)
			}
		})
	}
}

func TestUpgradeNewerMarkerBusyDoesNotRestart(t *testing.T) {
	fakeHistory(t, "aaa", "bbb")
	withOwnCommit(t, "aaa")
	keys := captureEscalations(t)
	d := upgradeTestDaemon(t)
	d.mayorDispatchRunning.Store(true)
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: "/repo"})

	now := time.Now()
	if d.checkUpgradeRestart(now) {
		t.Fatal("busy daemon must not restart")
	}
	if d.checkUpgradeRestart(now.Add(29 * time.Minute)) {
		t.Fatal("busy daemon must not restart")
	}
	if len(*keys) != 0 {
		t.Fatalf("escalated before 30m: %v", *keys)
	}
	d.checkUpgradeRestart(now.Add(31 * time.Minute))
	d.checkUpgradeRestart(now.Add(40 * time.Minute))
	if len(*keys) != 1 || (*keys)[0] != "daemon:restart-pending-stuck" {
		t.Fatalf("escalations = %v, want exactly one daemon:restart-pending-stuck", *keys)
	}
}

func TestUpgradeNewerMarkerIdleRequestsRestartAndStampsAttempt(t *testing.T) {
	fakeHistory(t, "aaa", "bbb")
	withOwnCommit(t, "aaa")
	captureEscalations(t)
	d := upgradeTestDaemon(t)
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: "/repo"})

	if !d.checkUpgradeRestart(time.Now()) {
		t.Fatal("idle daemon with newer marker must request restart")
	}
	if !d.upgradeRestartRequested.Load() {
		t.Fatal("upgradeRestartRequested not set")
	}
	m, err := readRestartMarker(d.config.TownRoot)
	if err != nil || m == nil || m.AttemptedFrom != "aaa" {
		t.Fatalf("marker after request = %+v (err %v), want attempted_from=aaa", m, err)
	}
}

func TestUpgradeNoEffectRestartDoesNotLoop(t *testing.T) {
	fakeHistory(t, "aaa", "bbb")
	withOwnCommit(t, "aaa")
	keys := captureEscalations(t)
	d := upgradeTestDaemon(t)
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: "/repo", AttemptedFrom: "aaa"})

	if d.checkUpgradeRestart(time.Now()) || d.checkUpgradeRestart(time.Now()) {
		t.Fatal("restart that already had no effect must not repeat")
	}
	if len(*keys) != 1 || (*keys)[0] != "daemon:restart-pending-no-effect" {
		t.Fatalf("escalations = %v, want exactly one daemon:restart-pending-no-effect", *keys)
	}
}

func TestUpgradeUnknownRepoFallsBackToPrefix(t *testing.T) {
	fakeHistory(t) // every lookup reports "unknown"
	withOwnCommit(t, "abc1234")
	captureEscalations(t)
	d := upgradeTestDaemon(t)
	writeMarker(t, d, restartPendingMarker{Commit: "abc1234def5678"})
	if d.checkUpgradeRestart(time.Now()) {
		t.Fatal("prefix-equal marker is covered, must not restart")
	}
	if _, err := os.Stat(restartMarkerPath(d.config.TownRoot)); !os.IsNotExist(err) {
		t.Fatal("prefix-equal marker should be cleared")
	}
}

func TestUpgradeShutdownLeavesDoltRunning(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upgrade  bool
		wantStop int
	}{
		{"upgrade restart keeps Dolt", true, 0},
		{"ordinary shutdown stops Dolt", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stops := 0
			d := upgradeTestDaemon(t)
			d.doltServer = &DoltServerManager{
				config:   &DoltServerConfig{Enabled: true},
				townRoot: d.config.TownRoot,
				logger:   func(string, ...interface{}) {},
				stopFn:   func() { stops++ },
			}
			d.upgradeRestartRequested.Store(tc.upgrade)
			_ = d.shutdown(&State{Running: true})
			if stops != tc.wantStop {
				t.Fatalf("Dolt stop calls = %d, want %d", stops, tc.wantStop)
			}
		})
	}
}

func TestHeartbeatSkipsWorkWhenRestartRequested(t *testing.T) {
	fakeHistory(t, "aaa", "bbb")
	withOwnCommit(t, "aaa")
	captureEscalations(t)
	calls := 0
	orig := heartbeatWorkFn
	heartbeatWorkFn = func(d *Daemon, s *State) { calls++ }
	t.Cleanup(func() { heartbeatWorkFn = orig })

	d := upgradeTestDaemon(t)
	d.heartbeat(&State{})
	if calls != 1 {
		t.Fatalf("no marker: heartbeatWork calls = %d, want 1", calls)
	}
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: "/repo"})
	d.heartbeat(&State{})
	if calls != 1 {
		t.Fatalf("restart requested: heartbeatWork (plugin dispatch) ran anyway; calls = %d", calls)
	}
}
```

Append to `internal/cmd/daemon_test.go`, adding `"errors"` and `"github.com/steveyegge/gastown/internal/daemon"` to its imports if they are missing:

```go
func TestDaemonRunExitMapsUpgradeTo75(t *testing.T) {
	var code = -1
	orig := daemonExit
	daemonExit = func(c int) { code = c }
	t.Cleanup(func() { daemonExit = orig })

	if err := daemonRunExit(fmt.Errorf("wrapped: %w", daemon.ErrRestartForUpgrade)); err != nil {
		t.Fatalf("daemonRunExit(upgrade) = %v, want nil", err)
	}
	if code != 75 {
		t.Fatalf("exit code = %d, want 75", code)
	}

	code = -1
	other := errors.New("boom")
	if err := daemonRunExit(other); err != other {
		t.Fatalf("daemonRunExit(other) = %v, want passthrough", err)
	}
	if err := daemonRunExit(nil); err != nil || code != -1 {
		t.Fatalf("daemonRunExit(nil) = %v, code %d; want nil and no exit", err, code)
	}
}
```

(Also add `"fmt"` if `daemon_test.go` doesn't import it.)

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/daemon -run 'TestUpgrade|TestHeartbeatSkipsWork' -count=1` (includes TestUpgradeShutdownLeavesDoltRunning)

Expected: a build failure, with `undefined: isAncestorFn`, `restartPendingMarker`, `upgradeEscalateFn`, `heartbeatWorkFn`, `checkUpgradeRestart` and others.

Run: `go test ./internal/cmd -run TestDaemonRunExitMapsUpgradeTo75 -count=1`

Expected: a build failure, `undefined: daemonExit`.

- [ ] **Step 3: Implement `internal/daemon/upgrade_restart.go`**

```go
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrRestartForUpgrade is returned by Run after a normal shutdown when the
// daemon exits so launchd restarts it on a newly installed binary. The
// caller maps it to exit code 75: launchd's KeepAlive {SuccessfulExit: false}
// restarts only a nonzero exit.
var ErrRestartForUpgrade = errors.New("daemon: restart for upgrade")

// upgradeStuckAfter is how long a newer marker may wait for an idle heartbeat
// before the daemon escalates (it keeps waiting afterwards).
const upgradeStuckAfter = 30 * time.Minute

// restartPendingMarker is daemon/restart-pending.json, written by
// scripts/install-gt.sh after a smoke-tested install.
type restartPendingMarker struct {
	Commit        string `json:"commit"`
	RequestedAt   string `json:"requested_at,omitempty"`
	Source        string `json:"source,omitempty"`
	Repo          string `json:"repo,omitempty"`
	AttemptedFrom string `json:"attempted_from,omitempty"`
}

// installReceipt is one line of daemon/install-receipts.jsonl; the shell
// scripts write the same shape.
type installReceipt struct {
	TS         string  `json:"ts"`
	Event      string  `json:"event"`
	Commit     string  `json:"commit"`
	PrevCommit string  `json:"prev_commit"`
	Source     string  `json:"source"`
	MergedAt   string  `json:"merged_at"`
	Reason     string  `json:"reason"`
	DurationS  float64 `json:"duration_s"`
}

func restartMarkerPath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", "restart-pending.json")
}

func installReceiptsPath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", "install-receipts.jsonl")
}

// isAncestorFn reports (isAncestor, known). known is false when git could not
// answer (no repo, unknown commit); a test seam.
var isAncestorFn = func(repo, ancestor, descendant string) (bool, bool) {
	if repo == "" {
		return false, false
	}
	err := exec.Command("git", "-C", repo, "merge-base", "--is-ancestor", ancestor, descendant).Run()
	if err == nil {
		return true, true
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, true
	}
	return false, false
}

// upgradeEscalateFn raises an upgrade-restart alert; a test seam.
var upgradeEscalateFn = func(d *Daemon, key, msg string) {
	d.escalateAlert(key, "upgrade-restart", msg)
}

// mergedAtFn returns the committer time of commit in repo, or "".
var mergedAtFn = func(repo, commit string) string {
	if repo == "" {
		return ""
	}
	out, err := exec.Command("git", "-C", repo, "show", "-s", "--format=%cI", commit).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func readRestartMarker(townRoot string) (*restartPendingMarker, error) {
	data, err := os.ReadFile(restartMarkerPath(townRoot))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m restartPendingMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m.Commit == "" {
		return nil, fmt.Errorf("restart marker has no commit")
	}
	return &m, nil
}

func writeRestartMarker(townRoot string, m *restartPendingMarker) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	path := restartMarkerPath(townRoot)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func appendInstallReceipt(townRoot string, r installReceipt) error {
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(installReceiptsPath(townRoot), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// markerCovered reports whether the marker's commit is already running:
// equal to, or an ancestor of, own. Falls back to prefix equality when git
// cannot answer.
func markerCovered(m *restartPendingMarker, own string) bool {
	if ok, known := isAncestorFn(m.Repo, m.Commit, own); known {
		return ok
	}
	return strings.HasPrefix(m.Commit, own) || strings.HasPrefix(own, m.Commit)
}

// checkUpgradeRestart handles daemon/restart-pending.json. It returns true
// (and sets upgradeRestartRequested) when the daemon should shut down now so
// launchd restarts it on the installed binary. Called at startup and at the
// top of every heartbeat, from the run-loop goroutine only.
func (d *Daemon) checkUpgradeRestart(now time.Time) bool {
	townRoot := d.config.TownRoot
	m, err := readRestartMarker(townRoot)
	if err != nil {
		d.logger.Printf("upgrade-restart: unreadable marker, removing: %v", err)
		_ = os.Remove(restartMarkerPath(townRoot))
		return false
	}
	if m == nil {
		d.upgradeWaitCommit = ""
		return false
	}
	own := buildCommitFn()
	if own == "" {
		d.logger.Printf("upgrade-restart: own build commit unknown; ignoring marker for %s", m.Commit)
		return false
	}

	if markerCovered(m, own) {
		rec := installReceipt{
			TS:         now.UTC().Format(time.RFC3339),
			Event:      "daemon_restarted",
			Commit:     m.Commit,
			PrevCommit: m.AttemptedFrom,
			Source:     m.Source,
			MergedAt:   mergedAtFn(m.Repo, m.Commit),
		}
		if t, err := time.Parse(time.RFC3339, m.RequestedAt); err == nil {
			rec.DurationS = now.Sub(t).Seconds()
		}
		if err := appendInstallReceipt(townRoot, rec); err != nil {
			d.logger.Printf("upgrade-restart: writing receipt: %v", err)
		}
		_ = os.Remove(restartMarkerPath(townRoot))
		d.upgradeWaitCommit = ""
		d.logger.Printf("upgrade-restart: running %s covers marker %s; cleared", own, m.Commit)
		return false
	}

	if d.upgradeWaitCommit != m.Commit {
		d.upgradeWaitCommit = m.Commit
		d.upgradeWaitSince = now
		d.upgradeWaitEscalated = false
	}

	if m.AttemptedFrom != "" && markerCovered(&restartPendingMarker{Commit: m.AttemptedFrom, Repo: m.Repo}, own) &&
		markerCovered(&restartPendingMarker{Commit: own, Repo: m.Repo}, m.AttemptedFrom) {
		if !d.upgradeWaitEscalated {
			d.upgradeWaitEscalated = true
			upgradeEscalateFn(d, "daemon:restart-pending-no-effect",
				fmt.Sprintf("Daemon restarted for upgrade to %s but is still running %s; the installed binary is not the marker's commit. Not restarting again.", m.Commit, own))
		}
		return false
	}

	if !d.isIdleForUpgrade() {
		if !d.upgradeWaitEscalated && now.Sub(d.upgradeWaitSince) >= upgradeStuckAfter {
			d.upgradeWaitEscalated = true
			upgradeEscalateFn(d, "daemon:restart-pending-stuck",
				fmt.Sprintf("Restart for upgrade to %s has waited %s for an idle daemon (running %s). Still waiting.",
					m.Commit, now.Sub(d.upgradeWaitSince).Round(time.Minute), own))
		}
		return false
	}

	m.AttemptedFrom = own
	if err := writeRestartMarker(townRoot, m); err != nil {
		d.logger.Printf("upgrade-restart: could not stamp attempted_from, not restarting: %v", err)
		return false
	}
	d.logger.Printf("upgrade-restart: idle; restarting from %s to pick up %s", own, m.Commit)
	d.upgradeRestartRequested.Store(true)
	return true
}
```

- [ ] **Step 4: Wire it into `internal/daemon/daemon.go`**

1. Fields, next to `mainBranchTestWaitingSlot`:

```go
	// upgradeRestartRequested is set by checkUpgradeRestart; the run loop
	// then shuts down and Run returns ErrRestartForUpgrade.
	upgradeRestartRequested atomic.Bool
	// upgradeWait* track the pending marker for the stuck/no-effect alarms.
	// Run-loop goroutine only.
	upgradeWaitCommit    string
	upgradeWaitSince     time.Time
	upgradeWaitEscalated bool
```

2. Startup, right after the startup `SaveState` block (:661-663). This handles a covered marker only; `heartbeat` handles a newer one:

```go
	// Clear a restart marker this binary already satisfies (the daemon may
	// have been restarted onto it by launchd or an operator).
	_ = d.checkUpgradeRestart(time.Now())
	if d.upgradeRestartRequested.Load() {
		_ = d.shutdown(state)
		return ErrRestartForUpgrade
	}
```

3. `heartbeat` (:1123). Keep the shutdown-in-progress and E-stop guards, add the check, and move everything from `d.metrics.recordHeartbeat(d.ctx)` down to the closing `d.logger.Printf("Heartbeat complete (#%d)", ...)` verbatim into a new method:

```go
func (d *Daemon) heartbeat(state *State) {
	if d.isShutdownInProgress() {
		d.logger.Println("Shutdown in progress, skipping heartbeat")
		return
	}
	if estop.IsActive(d.config.TownRoot) {
		d.logger.Println("E-STOP active, skipping agent management")
		return
	}
	// Before any dispatch: a heartbeat that starts plugins first would
	// make the daemon busy and never let it restart for an upgrade.
	if d.checkUpgradeRestart(time.Now()) {
		return
	}
	heartbeatWorkFn(d, state)
}

// heartbeatWorkFn is the body of a heartbeat; a test seam.
var heartbeatWorkFn = (*Daemon).heartbeatWork

func (d *Daemon) heartbeatWork(state *State) {
	d.metrics.recordHeartbeat(d.ctx)
	d.logger.Println("Heartbeat starting (recovery-focused)")
	// ... the rest of the former heartbeat body, unchanged, through
	// d.logger.Printf("Heartbeat complete (#%d)", state.HeartbeatCount)
}
```

4. The run loop. After the initial `d.heartbeat(state)` (:946):

```go
	d.heartbeat(state)
	startupComplete = true
	if d.upgradeRestartRequested.Load() {
		_ = d.shutdown(state)
		return ErrRestartForUpgrade
	}
```

and in `case <-timer.C:` (:1099):

```go
		case <-timer.C:
			d.heartbeat(state)
			if d.upgradeRestartRequested.Load() {
				d.logger.Println("Restarting for upgrade: shutting down so launchd restarts the daemon on the installed binary")
				_ = d.shutdown(state)
				return ErrRestartForUpgrade
			}

			// Fixed recovery interval (no activity-based backoff)
			timer.Reset(d.recoveryHeartbeatInterval())
```

5. `shutdown` (`daemon.go:2879`). Leave Dolt running across an upgrade restart. Replace

```go
	if d.doltServer != nil && d.doltServer.IsEnabled() && !d.doltServer.IsExternal() {
```

with

```go
	// An upgrade restart leaves Dolt running: the server is detached and the
	// next daemon adopts it via dolt.pid + port probe (isRunning). Bouncing
	// the data plane several times an hour for a binary swap is not safe.
	if d.upgradeRestartRequested.Load() {
		d.logger.Println("Upgrade restart: leaving Dolt server running for the next daemon to adopt")
	} else if d.doltServer != nil && d.doltServer.IsEnabled() && !d.doltServer.IsExternal() {
```

6. `internal/cmd/daemon.go`. Replace `return d.Run()` (:487) with `return daemonRunExit(d.Run())` and add:

```go
// daemonExit is os.Exit; a test seam.
var daemonExit = os.Exit

// daemonRunExit maps daemon.ErrRestartForUpgrade to exit code 75 so launchd
// (KeepAlive SuccessfulExit=false) restarts the daemon on the installed
// binary. A plain nil return would exit 0 and leave the daemon down.
func daemonRunExit(err error) error {
	if errors.Is(err, daemon.ErrRestartForUpgrade) {
		daemonExit(75)
		return nil
	}
	return err
}
```

(Add `"errors"` to the imports if it is missing; `os` and `daemon` are already imported.)

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `go test ./internal/daemon -run 'TestUpgrade|TestHeartbeatSkipsWork|TestIsIdleForUpgrade' -count=1`

Expected: `ok  	github.com/steveyegge/gastown/internal/daemon`

Run: `go test ./internal/cmd -run TestDaemonRunExitMapsUpgradeTo75 -count=1`

Expected: `ok  	github.com/steveyegge/gastown/internal/cmd`

Run: `go test ./internal/daemon -count=1 2>&1 | tail -3`

Expected: `ok`. This is the regression check: the `heartbeat` split must not change any existing daemon test.

- [ ] **Step 6: Commit**

```bash
git add internal/daemon/upgrade_restart.go internal/daemon/upgrade_restart_test.go \
  internal/daemon/daemon.go internal/cmd/daemon.go internal/cmd/daemon_test.go
git commit -m "daemon: restart for upgrade when idle via restart marker, exit 75"
```

### Task 6: rebuild-gt delegates to `install-gt.sh` and becomes the backstop

**Files:**
- Modify: `plugins/rebuild-gt/run.sh:1-24` (header), `:290-299` (threshold), new backstop block after `:337`, `:639-801` (the build/install tail, replaced)
- Modify: `plugins/rebuild-gt/plugin.md:39-43` (exit table), `:152-155` (threshold), `:210-213` (the pre-install re-read paragraph, removed), `:217-265` (Action / Record Result)
- Modify: `plugins/rebuild-gt/run_test.sh` (harness plus the cases listed below)

**Interfaces:**
- Consumes (T1): `install-gt.sh` CLI, exit codes 0/1/2/3, the `install-gt: RESULT` line, `INSTALL_GT_*` env, and the marker `$TOWN_ROOT/daemon/restart-pending.json` with `requested_at`.
- Consumes (T5, **cross-task interface**): `$GT_TOWN_ROOT/daemon/state.json` gains `"commit": "<build commit>"`, written at daemon startup and kept on every heartbeat save. It may be SHORT, so compare by ancestry (`git merge-base --is-ancestor` in the rig checkout), never by string equality. An old daemon writes no `commit`, and the second backstop condition then does nothing.
- Produces: fingerprint `rebuild-gt:daemon-not-in-force` (HIGH). Its open/closed state is tracked in `$TOWN_ROOT/daemon/rebuild-gt-daemon-lag` (exists = escalated).

Which `install-gt.sh`: the one in `$RIG_ROOT/scripts/`. rebuild-gt already fast-forwards `$RIG_ROOT` to `origin/main` (run.sh:397-428) before it gets there, and `install-gt.sh` lands in the same MR as this change. So by the time this `run.sh` is synced into the town's plugins (on an install), `origin/main`, and therefore `$RIG_ROOT`, contains `scripts/install-gt.sh`. A missing script is a failure (exit 1, `rebuild-gt:no-installer`), not a skip. It means `$RIG_ROOT` is older than the plugin, which only a hand-edited checkout produces.

**Deviation from the spec (flag at review): the pre-install "MR in flight" re-check is removed.** Today rebuild-gt re-reads `gt mq list --status=in_progress` between build and install (plugin.md "Quiet gate", case 25), because the install ended in `gt daemon restart`, which killed in-flight work. The install is now an atomic rename with no restart, which is exactly what the post-merge hook does mid-refinery by design. The pre-build reading stays, as the spec requires; only the second reading goes. Case 25 is rewritten to pin the new behaviour.

- [ ] **Step 1: Update the test harness and the cases that change (tests first)**

In `plugins/rebuild-gt/run_test.sh`:

(a) `make_town`: after the `git -C "$town/gastown/mayor/rig" push -q origin main` that follows the Makefile commit (currently line 44), add:

```bash
  # The plugin delegates the build and install to the rig's own
  # scripts/install-gt.sh (claude-7fc), so the fake rig carries the real one.
  mkdir -p "$town/gastown/mayor/rig/scripts/lib"
  cp "$SCRIPT_DIR/../../scripts/install-gt.sh" "$town/gastown/mayor/rig/scripts/"
  cp "$SCRIPT_DIR/../../scripts/lib/install-gt-lib.sh" "$town/gastown/mayor/rig/scripts/lib/"
  git -C "$town/gastown/mayor/rig" add scripts
  git -C "$town/gastown/mayor/rig" -c user.email=t@t -c user.name=t commit -q -m installer
  git -C "$town/gastown/mayor/rig" push -q origin main
```

(b) `run_plugin`: extend the export so `install-gt.sh` installs into the stub bin dir and does not wait 5 minutes on a lock:

```bash
  ( export GT_TEST_TOWN="$town" GT_TOWN_ROOT="$town" PATH="$town/bin:/opt/homebrew/bin:/usr/bin:/bin" \
      INSTALL_GT_BIN_DIR="$town/bin" INSTALL_GT_LOCK_WAIT=5 "$@"; bash "$RUN_SH" ) > "$town/run.out" 2>&1 || rc=$?
```

(c) Case 7: replace the line `if grep -q "daemon restart" "$T/gt.log"; then pass "quiet + behind: daemon restarted"; ...` with:

```bash
if grep -q "daemon restart" "$T/gt.log" 2>/dev/null; then fail "quiet + behind: restarted the daemon (install-gt leaves that to the daemon)"; else pass "quiet + behind: no daemon restart"; fi
TIP=$(git -C "$T/gastown/mayor/rig" rev-parse HEAD)
if python3 -c 'import json,sys; m=json.load(open(sys.argv[1])); assert m["commit"]==sys.argv[2] and m["source"]=="rebuild-gt"' "$T/daemon/restart-pending.json" "$TIP" 2>/dev/null; then
  pass "quiet + behind: restart marker names the installed tip"
else
  fail "quiet + behind: marker missing or wrong: $(cat "$T/daemon/restart-pending.json" 2>/dev/null)"
fi
```

(d) Case 8 (`write_stale "$T" 2` under-threshold) and Case 19's "exit 3 (deferred)" block: keep the fixtures and pin the old threshold explicitly, because the default is now 1. Change `rc=$(run_plugin "$T")` to `rc=$(run_plugin "$T" REBUILD_GT_INSTALL_THRESHOLD=5)` in both. Add a case after Case 8:

```bash
# --- Case 8b: the default threshold is 1 commit — a single merge is due
# (claude-7fc: a lone urgent fix was never due under the old default of 5) ---
T=$(make_town)
write_stale "$T" 1
rc=$(run_plugin "$T")
if [ "$rc" = "0" ] && [ -e "$T/build.marker" ]; then pass "default threshold: 1 behind installs"; else fail "default threshold: rc=$rc $(cat "$T/run.out")"; fi
```

(e) Case 13: its assertions keep working ("install did not take" is install-gt's smoke message and it goes through the stub `gt escalate`). Add after it:

```bash
if [ -e "$T/daemon/restart-pending.json" ]; then fail "install that did not take: wrote a restart marker"; else pass "install that did not take: no marker"; fi
if grep -q -- "--fingerprint install-gt:smoke-failed" "$T/gt.log"; then pass "install that did not take: install-gt fingerprint"; else fail "install that did not take: fingerprint $(cat "$T/gt.log")"; fi
```

(f) Case 21 (line ~672: `grep -q -- "--result success" ... && grep -q "daemon restart" "$T/gt.log"`): change `grep -q "daemon restart" "$T/gt.log"` to `[ -e "$T/daemon/restart-pending.json" ]`, and update its pass text to "... installed and marked for restart".

(g) The gt-b5mpe case (line ~886): replace the "daemon restarted onto the verified binary" pair with:

```bash
if [ -e "$T/daemon/restart-pending.json" ]; then
  pass "no-@ version + no stale commit: restart marker written for the verified binary"
else
  fail "no-@ version + no stale commit: no marker: $(cat "$T/run.out")"
fi
```

(h) Case 25: replace the whole case body with:

```bash
# Case 25: a merge that goes in flight while the build runs no longer holds
# the install back. The re-check existed because the install ended in a daemon
# restart that killed in-flight work; install-gt only renames the binary and
# leaves the restart to the daemon's idle point (claude-7fc).
T=$(make_town)
echo '[{"id": "gt-wisp-z", "status": "in_progress", "title": "Merge: gt-z"}]' > "$T/mq.busy.json"
touch "$T/mq.flip"
rc=$(run_plugin "$T")
if [ "$rc" = "0" ] && grep -q -- "--result success" "$T/gt.log" 2>/dev/null; then
  pass "merge goes in flight mid-build: the install still lands"
else
  fail "merge goes in flight mid-build: rc=$rc $(cat "$T/run.out")"
fi
```

(i) New backstop cases, appended before the final `if [ "$FAILURES" ...`:

```bash
# --- claude-7fc: backstop — the daemon must come into force after an install ---
# age_marker TOWN MINUTES -> a restart-pending marker requested MINUTES ago
age_marker() {
  mkdir -p "$1/daemon"
  python3 -c '
import datetime, json, sys, time
t = datetime.datetime.fromtimestamp(time.time() - int(sys.argv[2]) * 60, datetime.timezone.utc)
json.dump({"commit": "0" * 40, "requested_at": t.strftime("%Y-%m-%dT%H:%M:%SZ"), "source": "post-merge", "repo": "/x"}, open(sys.argv[1], "w"))
' "$1/daemon/restart-pending.json" "$2"
}

# Case 29: a marker older than 30m escalates HIGH, once per condition.
T=$(make_town)
age_marker "$T" 40
rc=$(run_plugin "$T" REBUILD_GT_INSTALL_THRESHOLD=99)
if grep -q "escalate .*-s high .*--fingerprint rebuild-gt:daemon-not-in-force" "$T/gt.log" 2>/dev/null; then
  pass "marker 40m old: escalated daemon-not-in-force"
else
  fail "marker 40m old: no escalation: $(cat "$T/gt.log" 2>/dev/null) $(cat "$T/run.out")"
fi
[ -e "$T/daemon/rebuild-gt-daemon-lag" ] && pass "marker 40m old: lag recorded" || fail "marker 40m old: lag not recorded"

# Case 30: a fresh marker is the daemon working as designed: no escalation.
T=$(make_town)
age_marker "$T" 5
rc=$(run_plugin "$T" REBUILD_GT_INSTALL_THRESHOLD=99)
if grep -q "daemon-not-in-force" "$T/gt.log" 2>/dev/null; then fail "marker 5m old: escalated early"; else pass "marker 5m old: quiet"; fi

# Case 31: no marker, but the daemon's recorded commit is behind the installed
# binary and the binary has been in place for over 30m -> escalate.
T=$(make_town)
RIG="$T/gastown/mayor/rig"
OLD=$(git -C "$RIG" rev-parse --short HEAD~2)
mkdir -p "$T/daemon"
printf '{"running": true, "commit": "%s"}\n' "$OLD" > "$T/daemon/state.json"
python3 -c 'import os,sys,time; t=time.time()-3600; os.utime(sys.argv[1], (t, t))' "$T/bin/gt"
rc=$(run_plugin "$T" REBUILD_GT_INSTALL_THRESHOLD=99)
if grep -q -- "--fingerprint rebuild-gt:daemon-not-in-force" "$T/gt.log" 2>/dev/null; then
  pass "daemon behind installed binary: escalated"
else
  fail "daemon behind installed binary: $(cat "$T/gt.log" 2>/dev/null) $(cat "$T/run.out")"
fi

# Case 32: the condition clears -> the open escalation is cleared, once.
T=$(make_town)
mkdir -p "$T/daemon"; touch "$T/daemon/rebuild-gt-daemon-lag"
rc=$(run_plugin "$T" REBUILD_GT_INSTALL_THRESHOLD=99)
if grep -q "escalate clear .*--fingerprint rebuild-gt:daemon-not-in-force" "$T/gt.log" && [ ! -e "$T/daemon/rebuild-gt-daemon-lag" ]; then
  pass "daemon in force again: escalation cleared"
else
  fail "daemon in force again: $(cat "$T/gt.log" 2>/dev/null)"
fi
```

Case 31 needs `HEAD~2` to exist in the fake rig. After (a) the rig has `init`, `makefile` and `installer` commits, so it does. The stub's pre-build `stale --json` reads `stale.json`, whose `binary_commit` is `HEAD~1` short (`write_stale`). The daemon commit (`HEAD~2`) is a strict ancestor of it, so the daemon is behind.

- [ ] **Step 2: Run the tests to verify the changed cases fail**

Run: `bash plugins/rebuild-gt/run_test.sh 2>&1 | grep -E '^FAIL' | head -20`
Expected: FAIL lines for at least Case 7 ("restarted the daemon" and "marker missing"), Case 8b ("default threshold"), Case 13's marker/fingerprint lines, Case 25, and Cases 29, 31 and 32. The old `run.sh` still restarts, still uses threshold 5, and has no backstop.

- [ ] **Step 3: Change the threshold default** (`run.sh:299` and its comment at `:291-298`)

```bash
# A merged commit that is not in force is a live defect, not a rounding error
# (gt-oqbw, gt-ww20, gt-rbfj): one commit behind is due. The post-merge hook
# (scripts/install-after-merge.sh) normally installs first; this plugin is the
# backstop for merges that bypass it (direct pushes, orphan-path merges, a
# failed hook), so waiting for a batch of commits only lengthens the inert
# window (claude-7fc).
THRESHOLD=${REBUILD_GT_INSTALL_THRESHOLD:-1}
```

- [ ] **Step 4: Add the daemon-in-force backstop** (insert after the drift block that ends at `:337`, before `if [ "$DRIFT_STALE" = "True" ]; then` at `:339`)

```bash
# --- Daemon in force (backstop, claude-7fc) -------------------------------------
#
# Installs no longer restart the daemon: install-gt.sh writes
# daemon/restart-pending.json and the daemon exits for a launchd restart at its
# own idle point. Two readings say that did not happen: a marker that has
# waited past the limit (the daemon never went idle, or never read it), or a
# daemon whose recorded commit (state.json "commit", possibly short) is not a
# descendant of the installed binary's while that binary has been in place past
# the limit (a crash loop, or an install by some path that wrote no marker).
# One HIGH escalation per episode; the flag file closes it when the readings
# clear, so a healthy run writes nothing.
DAEMON_LAG_MINUTES=${REBUILD_GT_DAEMON_LAG_MINUTES:-30}
LAG_FLAG="${TOWN_ROOT}/daemon/rebuild-gt-daemon-lag"

marker_age_minutes() {
  python3 - "${TOWN_ROOT}/daemon/restart-pending.json" <<'PY' 2>/dev/null || true
import datetime, json, sys, time
m = json.load(open(sys.argv[1]))
t = datetime.datetime.strptime(m["requested_at"], "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=datetime.timezone.utc)
print(int((time.time() - t.timestamp()) // 60))
PY
}
daemon_state_commit() {
  python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("commit") or "")' "${TOWN_ROOT}/daemon/state.json" 2>/dev/null || true
}
file_age_minutes() {
  python3 -c 'import os,sys,time; print(int((time.time() - os.path.getmtime(sys.argv[1])) // 60))' "$1" 2>/dev/null || echo 0
}

LAG=""
MARKER_AGE=$(marker_age_minutes)
if [ -n "$MARKER_AGE" ] && [ "$MARKER_AGE" -ge "$DAEMON_LAG_MINUTES" ]; then
  LAG="a restart-pending marker has waited ${MARKER_AGE}m for the daemon to restart"
elif [ "$DRIFT_READ" = "1" ]; then
  DC=$(daemon_state_commit)
  BC=$(echo "$DRIFT_JSON" | python3 -c "import json,sys; print(json.load(sys.stdin).get('binary_commit') or '')" 2>/dev/null || true)
  DCF=$( [ -n "$DC" ] && git -C "$RIG_ROOT" rev-parse --verify --quiet "$DC^{commit}" 2>/dev/null || true )
  BCF=$( [ -n "$BC" ] && git -C "$RIG_ROOT" rev-parse --verify --quiet "$BC^{commit}" 2>/dev/null || true )
  GT_BIN=$(command -v gt 2>/dev/null || true)
  if [ -n "$DCF" ] && [ -n "$BCF" ] && [ -n "$GT_BIN" ] \
    && ! git -C "$RIG_ROOT" merge-base --is-ancestor "$BCF" "$DCF" 2>/dev/null; then
    BIN_AGE=$(file_age_minutes "$GT_BIN")
    if [ "$BIN_AGE" -ge "$DAEMON_LAG_MINUTES" ]; then
      LAG="the daemon runs $DC but $BC has been installed for ${BIN_AGE}m"
    fi
  fi
fi
if [ -n "$LAG" ]; then
  log "Daemon not in force: $LAG."
  if gt escalate "rebuild-gt: $LAG" -s high --source "plugin:rebuild-gt" \
    --fingerprint "rebuild-gt:daemon-not-in-force" >/dev/null 2>&1; then
    touch "$LAG_FLAG" 2>/dev/null || true
  fi
elif [ -e "$LAG_FLAG" ]; then
  gt escalate clear --fingerprint "rebuild-gt:daemon-not-in-force" \
    --reason "rebuild-gt: the daemon runs the installed binary" >/dev/null 2>&1 || true
  rm -f "$LAG_FLAG"
fi
```

The `gt escalate` for a lag re-fires on every run while the condition holds. The town's fingerprint dedupe turns that into one bead with a growing occurrence count (see `gt escalate --help`, RECURRING ALERTS), so no local "escalated once" bookkeeping is needed beyond the clear flag. Case 29 then passes, and the remaining cases never see "escalate" when there is no lag.

- [ ] **Step 5: Replace the build/install tail** (`run.sh:639-801`, from `log "Rebuilding gt from ...` to end of file)

```bash
log "Installing gt from $RIG_ROOT ($BEHIND commits behind) through scripts/install-gt.sh..."

# One install path for the town (claude-7fc): the rig's own install-gt.sh builds,
# installs, verifies (rolling back on a failed smoke check), syncs formulas and
# plugins, and writes the restart-pending marker; the daemon restarts itself at
# its idle point, so nothing here kills in-flight plugins — this one included.
# It runs under the install lock, which the post-merge hook takes too, so the two
# can never build into one output. What it builds is RIG_ROOT's HEAD, which the
# sync above fast-forwarded to origin/main (SKIP_UPDATE_CHECK stays inside it,
# gt-9jax).
INSTALLER="$RIG_ROOT/scripts/install-gt.sh"
if [ ! -f "$INSTALLER" ]; then
  log "FAILED: $INSTALLER is missing (the rig checkout is older than this plugin)"
  gt plugin record-run --plugin rebuild-gt --result failure --rig gastown \
    --title "Plugin: rebuild-gt [no installer]" \
    --description "$INSTALLER does not exist" >/dev/null 2>&1 || true
  gt escalate "rebuild-gt: $INSTALLER is missing, so nothing can install gt" -s medium \
    --source "plugin:rebuild-gt" --fingerprint "rebuild-gt:no-installer" 2>/dev/null || true
  exit 1
fi
TARGET=$(git -C "$RIG_ROOT" rev-parse HEAD)
INSTALL_ARGS=(--sha "$TARGET" --source rebuild-gt)
if [ "$RESERVE" = "1" ]; then
  INSTALL_ARGS+=(--slot-role gastown/rebuild-gt --slot-timeout "$(reserve_remaining)")
fi

# Teed, not captured: a build waiting on the slot would otherwise print nothing
# to the plugin log until it finished, and read as hung (gt-kox0).
INSTALL_LOG=$(mktemp)
set +e
INSTALL_GT_RIG_DIR="$RIG_ROOT" bash "$INSTALLER" "${INSTALL_ARGS[@]}" 2>&1 | tee "$INSTALL_LOG"
INSTALL_RC=${PIPESTATUS[0]}
set -e
RESULT=$(grep '^install-gt: RESULT ' "$INSTALL_LOG" | tail -1 || true)
rm -f "$INSTALL_LOG"
# install-gt: RESULT <event> <commit> <prev> <reason>
read -r _ _ R_EVENT R_COMMIT R_PREV R_REASON <<<"$RESULT" || true

case "$INSTALL_RC" in
  0)
    if [ "${R_EVENT:-}" = "noop" ]; then
      log "Binary already contains $TARGET."
      starve_clear "binary is fresh" >/dev/null
      clear_alarms
      gt plugin record-run --plugin rebuild-gt --result success --rig gastown \
        --title "rebuild-gt: binary is fresh" >/dev/null 2>&1 || true
      exit 0
    fi
    FROM="${R_PREV:-$BINARY_COMMIT}"; [ "$FROM" = "-" ] && FROM="$BINARY_COMMIT"
    TO="${R_COMMIT:-$TARGET}"
    # The commits in this range were merged and not running until now: that
    # list is the inert window, so "was gt-ww20 ever in force, and when" is
    # answered by the receipt instead of reconstructed from merge timestamps.
    SUBJECTS=$(git -C "$RIG_ROOT" log --no-decorate --oneline "$FROM..$TO" 2>/dev/null | head -20 || true)
    gt plugin record-run --plugin rebuild-gt --result success --rig gastown \
      --title "rebuild-gt: in force $FROM -> $TO ($BEHIND commits)" \
      --description "Brought into force $FROM..$TO ($BEHIND commits); the daemon restarts at its idle point:
$SUBJECTS" >/dev/null 2>&1 || true
    starve_clear "installed $TO" >/dev/null
    clear_alarms
    log "In force: $FROM -> $TO. Restart marker written; the daemon restarts when idle."
    exit 0
    ;;
  2)
    log "install-gt refused: ${R_REASON:-unknown}"
    if [ -n "$DUE" ]; then note_blocked "install-gt refused: ${R_REASON:-unknown}"; fi
    gt plugin record-run --plugin rebuild-gt --result skipped --rig gastown \
      --title "Plugin: rebuild-gt [skipped]" \
      --description "Skipped: install-gt refused (${R_REASON:-unknown})" >/dev/null 2>&1 || true
    exit 0
    ;;
  3)
    if [ "${R_REASON:-}" = "slot-busy" ]; then
      blocked_defer "waited for the container-gate slot and did not get it"
    fi
    blocked_defer "another install holds the install lock"
    ;;
  *)
    log "FAILED: install-gt exited $INSTALL_RC (${R_REASON:-no result line})"
    gt plugin record-run --plugin rebuild-gt --result failure --rig gastown \
      --title "Plugin: rebuild-gt [failure]" \
      --description "install-gt failed: ${R_REASON:-exit $INSTALL_RC} (escalated by install-gt)" >/dev/null 2>&1 || true
    exit 1
    ;;
esac
```

Also delete the now-unused `in_flight_count`'s second caller only. Keep `install_requires_quiet warn` at `:622` (the pre-build reading). Delete the `install_requires_quiet` comment sentence "Called twice: before the build, and again immediately before the install ..." and replace it with "Called before the build."

And in `clear_alarms` (`:227-232`) add the new fingerprints, so a verified install closes them:

```bash
clear_alarms() {
  gt escalate clear --fingerprint "rebuild-gt:starved" \
    --fingerprint "rebuild-gt:drift" --fingerprint "rebuild-gt:drift-unknown" \
    --fingerprint "rebuild-gt:unverified" --fingerprint "rebuild-gt:no-installer" \
    --reason "rebuild-gt: the binary is in force" >/dev/null 2>&1 || true
}
```

- [ ] **Step 6: Update the header comment** (`run.sh:8-24`)

Replace the sentence "So this plugin installs, then verifies what came into force." with:

```bash
# So this plugin installs — through scripts/install-gt.sh, the same locked
# path the refinery's post-merge hook uses (claude-7fc) — and leaves the
# daemon restart to the daemon: install-gt.sh writes restart-pending.json and
# the daemon exits for launchd at its idle point, so no install kills
# in-flight plugins. It is the backstop: the post-merge hook installs most
# merges first, and this plugin also escalates when the daemon has not come
# into force (rebuild-gt:daemon-not-in-force).
```

and in the exit-code sentence replace "1 FAILED — build failed, or the install could not be verified as in force" with "1 FAILED — install-gt.sh failed (it escalates its own failure under install-gt:*), or the rig has no install-gt.sh".

- [ ] **Step 7: Update `plugin.md`**

Exit table (`:39-43`):

```markdown
| exit | when | recorded | escalates |
|------|------|----------|-----------|
| 0 | did the work (installed, or already fresh) — or refused safely: dirty checkout, wrong branch, diverged local main, not safe to rebuild, install-gt refused (exit 2), no rig root | yes, as success or skipped — except no rig root, which records nothing | on a refusal, only while the binary is due and past `REBUILD_GT_STARVE_MINUTES` (Starvation below) |
| 3 | deferred: nothing accomplished this run (gate busy, MR in flight, under the install threshold, an unreadable staleness check, the install lock or the container-gate slot not free — install-gt exit 3) — retry next heartbeat | no | the same starvation clock |
| 1 | failed: install-gt.sh failed (build, install, or smoke check — it rolls back and escalates under `install-gt:*`), or the rig has no `scripts/install-gt.sh` | yes, as failure | install-gt's own fingerprint, or `rebuild-gt:no-installer` |
```

Threshold paragraph (`:152-155`): replace "(default 5)" with "(default 1)", and append: "The refinery's post-merge hook (`scripts/install-after-merge.sh`) installs most merges within a minute of landing; this plugin is the backstop for merges that bypass it."

Quiet gate: delete the paragraph beginning "The second reading is taken again immediately before the install" and replace it with: "There is no second reading before the install. It existed because the install ended in a daemon restart; `install-gt.sh` only renames the binary and leaves the restart to the daemon's idle point (claude-7fc)."

Action and Record Result (`:217-265`): replace both sections with:

```markdown
## Action

Run the rig's own `scripts/install-gt.sh --sha <HEAD> --source rebuild-gt`
(plus `--slot-role gastown/rebuild-gt` past the starvation threshold), the one
install path the refinery's post-merge hook also uses. Under one flock it
builds, installs atomically (`scripts/install-binary.sh`, gt-0het), keeps the
previous binary as `gt.prev`, and verifies the commit in force the way this
plugin used to — the short commit the binary reports is resolved to a full
hash inside the rig before comparing (gt-oqbw, gt-b5mpe). On a failed check it
restores `gt.prev` and escalates (`install-gt:smoke-failed`,
`install-gt:rollback-failed`). Then `gt formula sync` and `gt plugin sync`
(non-fatal), then `daemon/restart-pending.json`.

The daemon is **not** restarted here. It reads the marker at each heartbeat
and exits (code 75, restarted by launchd) at a moment with no plugin, dog or
main-branch gate in flight, so an install never kills in-flight work.

## Daemon in force (backstop)

Each run also checks that the daemon came into force: a restart-pending
marker older than `REBUILD_GT_DAEMON_LAG_MINUTES` (default 30), or a daemon
whose `state.json` commit is not a descendant of the installed binary's while
that binary has been in place that long, escalates
`rebuild-gt:daemon-not-in-force` (HIGH). It clears once both readings are
healthy.

## Record Result

The receipt names the commits brought into force — `in force <old> -> <new>
(N commits)` plus their subjects. install-gt also appends a line to
`daemon/install-receipts.jsonl`. A refusal escalates only through the
starvation check's `rebuild-gt:starved`; reaching force — or a run that finds
the binary not due — clears `:starved`, `:drift`, `:drift-unknown`,
`:unverified` and `:no-installer` together.
```

- [ ] **Step 8: Run the tests**

Run: `bash -n plugins/rebuild-gt/run.sh && bash plugins/rebuild-gt/run_test.sh 2>&1 | tail -3`
Expected: `all rebuild-gt tests passed`, exit 0. Watch for a leftover assertion that `grep`s `"daemon restart"` expecting a hit (Cases 7, 21, gt-b5mpe); each must now expect the marker instead. Cases 10, 10b, 13, 14 and 17 assert it is *absent*, and stay as they are.

Then: `make test-makefile 2>&1 | tail -3`, expected exit 0.

- [ ] **Step 9: Commit**

```bash
git add plugins/rebuild-gt/run.sh plugins/rebuild-gt/plugin.md plugins/rebuild-gt/run_test.sh
git commit -m "feat: rebuild-gt installs through install-gt.sh and watches the daemon

Delegate build, install and verification to the shared locked installer,
stop restarting the daemon, install from one commit behind, and escalate
when a restart marker or the daemon's running commit shows the daemon has
not come into force."
```

### Task 7: MR-A quality gate and submit to the gastown merge queue

**Files:** none changed; `.beads/redirect` is created in the worktree (gitignored).

**Interfaces:**
- Consumes: branch `crew/sloan/claude-7fc-install` with Tasks 1–6 committed.
- Produces: `$MRA_ISSUE` (gastown source bead id), `$MRA_MR` (MR wisp id) and `$MRA_TIP` (pre-rebase tip SHA). Task 11 uses `$MRA_TIP` for `git rebase --onto`.

- [ ] **Step 1: Make bd resolve in the worktree**

```bash
cd ~/gt/gastown/crew/sloan-7fc
printf '../../mayor/rig/.beads\n' > .beads/redirect
git check-ignore -q .beads/redirect && echo ignored
bd where 2>/dev/null | head -2
```
Expected: `ignored`, then `/Users/sloan/gt/gastown/mayor/rig/.beads (via redirect ...)`.

- [ ] **Step 2: Rebase onto fresh main (fetch first)**

```bash
cd ~/gt/gastown/crew/sloan-7fc
git status --porcelain          # expect empty
git fetch origin
git rebase origin/main
git log --oneline origin/main..HEAD
```
Expected: `git status --porcelain` prints nothing. The rebase ends with `Successfully rebased`. The log lists only the spec, plan and Task 1–6 commits.

On a conflict, resolve it keeping both sides' intent, then `git rebase --continue`. Never use `--skip`. After resolving, re-run the Task 1–6 test commands before continuing.

- [ ] **Step 3: Record the pre-submit tip**

```bash
MRA_TIP=$(git rev-parse HEAD); echo "$MRA_TIP"
cd ~/.claude && bd comments add claude-7fc "MR-A tip before submit: $MRA_TIP"
```

- [ ] **Step 4: Build, vet, lint (exit codes, not output)**

```bash
cd ~/gt/gastown/crew/sloan-7fc
go build ./... ; echo "build=$?"
go vet ./internal/cmd/... ./internal/daemon/... ./internal/refinery/... ./internal/config/... ; echo "vet=$?"
make lint > /tmp/7fc-lint.log 2>&1 ; echo "lint=$?"
```
Expected: `build=0`, `vet=0`, `lint=0`.

If lint fails with "can't load config", golangci-lint was built with an older Go than go.mod needs. Run `make lint-tools`, then re-run `make lint`. For any other failure, read it with `grep -a -n 'error\|FAIL' /tmp/7fc-lint.log | head -40` and fix.

- [ ] **Step 5: Shell suites, then the full hermetic Go suite**

```bash
cd ~/gt/gastown/crew/sloan-7fc
make test-makefile > /tmp/7fc-testmk.log 2>&1 ; echo "testmk=$?"
```
Expected: `testmk=0`. `test-makefile` must list the new scripts' tests; Tasks 1, 2 and 6 add those lines.

Run the Go suite in the background: it takes 4–10 min under load. Wait for it to finish; don't poll.

```bash
cd ~/gt/gastown/crew/sloan-7fc
GT_TEST_DOCKER=0 go test -timeout 20m ./... > /tmp/7fc-gotest.log 2>&1 ; echo "gotest=$?" >> /tmp/7fc-gotest.log
tail -1 /tmp/7fc-gotest.log
grep -a -n -- '--- FAIL\|^FAIL' /tmp/7fc-gotest.log | head -20
```
Expected: the last line is `gotest=0`, and the grep prints nothing.

- [ ] **Step 6: Optional local editorial pre-check**

This is the same om the refinery runs. It catches `request_changes` before the queue round-trip. om lives in `~/go/bin` and isn't on a session's PATH.

```bash
cd ~/gt/gastown/crew/sloan-7fc
~/go/bin/om review -base origin/main ; echo "om=$?"
```
Expected: `om=0` (approve). Exit 1 means `request_changes`: fix the findings, commit, then go back to Step 4. Route on the **exit code**, never on the log text: an om-gate mirror failure isn't a verdict.

- [ ] **Step 7: Create the source issue in the gastown rig DB, held out of dispatch**

```bash
cd ~/gt/gastown
MRA_ISSUE=$(bd create \
  --title="Install gt after each refinery merge (post-merge hook, shared install script, idle daemon restart)" \
  --type=feature --priority=2 \
  --assignee=gastown/crew/sloan --status=in_progress \
  --labels=claude-7fc \
  --description="MR-A of docs/plans/2026-09-23-install-gt-after-merge-design.md. Implemented by crew session claude-7fc on branch crew/sloan/claude-7fc-install. Do NOT sling: crew-owned, lands through the refinery." \
  --json | jq -r .id)
echo "$MRA_ISSUE"
bd show "$MRA_ISSUE" | head -5
```
Expected: an id `gt-xxxxx`, shown as `IN_PROGRESS` and assigned to `gastown/crew/sloan`. If `bd create` doesn't accept `--json`, drop it and copy the id from the `✓ Created issue: gt-xxxxx` line.

- [ ] **Step 8: Push the branch**

```bash
cd ~/gt/gastown/crew/sloan-7fc
git push origin crew/sloan/claude-7fc-install
git ls-remote origin refs/heads/crew/sloan/claude-7fc-install
```
Expected: the ls-remote SHA equals `$MRA_TIP`.

A branch-name rejection from a hook means the hooks path is now active. Stop and report it; don't rename the branch to `polecat/*`.

- [ ] **Step 9: Submit**

```bash
cd ~/gt/gastown/crew/sloan-7fc
gt mq submit --branch crew/sloan/claude-7fc-install --issue "$MRA_ISSUE" --no-cleanup
```
Expected: `✓ Submitted to merge queue`, `MR ID: <id>` and `Source: crew/sloan/claude-7fc-install`. Save the id:

```bash
MRA_MR=<id from output>
cd ~/.claude && bd comments add claude-7fc "MR-A submitted: issue $MRA_ISSUE, MR $MRA_MR, tip $MRA_TIP"
```

- [ ] **Step 10: Watch it through the queue**

```bash
gt mq list gastown
gt mq status "$MRA_MR"
```
Expected: the MR shows `open`, then `in_progress` once the refinery picks it up. Because of batching (min 3 / 5 min) it can wait up to 5 min. Check every 5–10 min rather than in a tight loop; one gate takes about 5–6 min.

- [ ] **Step 11: On rejection, rework — never `gt mq reject`**

Read why:
```bash
gt mq status "$MRA_MR"
cd ~/gt/gastown && bd comments "$MRA_ISSUE" | tail -20
```
Fix it on the same branch with a new commit, not an amend. Re-run Steps 4–5, then:
```bash
cd ~/gt/gastown/crew/sloan-7fc
git push origin crew/sloan/claude-7fc-install
gt mq submit --branch crew/sloan/claude-7fc-install --issue "$MRA_ISSUE" --no-cleanup --resubmit
```
Never run `gt mq reject` to reset an MR: it resurrects finished work (gt-pvwy).

- [ ] **Step 12: Verify it actually landed (the artifact, not the MR status)**

```bash
cd ~/gt/gastown/crew/sloan-7fc
git fetch origin
git ls-remote origin refs/heads/main
git merge-base --is-ancestor "$MRA_TIP" origin/main && echo ancestor || echo not-ancestor
git cherry -v origin/main crew/sloan/claude-7fc-install
```
Expected:
- **The rebase-merge case:** the refinery may rebase, so `not-ancestor` is possible and is not a failure on its own. The source of truth is `git cherry`: every line must start with `-`, meaning an equivalent patch is on main.
- **Any `+` line is a commit that did not land.** Stop and investigate.
- **Also confirm the MR closed** (`gt mq status "$MRA_MR"`) and find the landed merge commit:
  ```bash
  git log --oneline --first-parent -5 origin/main
  ```

```bash
MRA_MERGE=$(git log --first-parent --format=%H -1 --grep="$MRA_ISSUE" origin/main); echo "$MRA_MERGE"
cd ~/.claude && bd comments add claude-7fc "MR-A landed: $MRA_MERGE (issue $MRA_ISSUE)"
cd ~/gt/gastown && bd close "$MRA_ISSUE" --reason="Landed via refinery as $MRA_MERGE"
```
If the refinery already closed `$MRA_ISSUE` during post-merge, `bd close` reports that it is already closed. That is fine.

---


## MR-B: fresh refinery session per landed unit

Branch: `crew/sloan/claude-7fc-unit-cycle`, cut from the MR-A tip and rebased onto
`origin/main` once MR-A lands. All paths are relative to the worktree
`~/gt/gastown/crew/sloan-7fc`.

**Facts established while drafting this section.** They correct or narrow the
spec.

- **No new MERGED builder is needed.** `internal/protocol.NewMergedMessage(rig, polecat, branch, issue, target, mergeCommit)`
  (`internal/protocol/messages.go:50-75`) is the canonical builder.
  `Engineer.notifyWitnessMerged` hand-rolls the same body only because
  `refinery` can't import `protocol` (import cycle, `engineer.go:1959-1961`).
  `internal/cmd` can import `protocol` (as `mail_thread.go` already does), so
  T9 calls `protocol.NewMergedMessage` directly and T8 exports nothing from
  `refinery`.
- **The witness handles a duplicate MERGED safely.** `witness.HandleMerged`
  (`internal/witness/handlers.go:435-477`) finds the cleanup wisp, verifies
  the commit is on main, and records `cleanup_status` as data; the sandbox is
  preserved (gt-4ac). It deletes and nukes nothing, so a second MERGED only
  repeats the same report. Duplicates already happen today: the batch path
  sends MERGED in Go (`engineer.go:1936-1947`, gt-9gjl) while the formula's
  batch-scan Step 4 (:501-512) still tells the agent to send it again. The
  cost of a duplicate is one extra mail bead, nothing destructive.
- **In batch mode the unit does not send MERGED.** `HandleMRInfoSuccess`
  already sends it once per polecat member. The unit only archives each
  member's MERGE_READY. It adds no attestation comment and deletes no `temp`
  branch, because batch never creates `temp`.
- **In single-MR mode (`gt mq post-merge`) the unit sends MERGED.** That path
  goes through `Manager.PostMergeMR` and sends nothing to the witness today;
  the agent does it (formula :1379-1398). So the unit sends it, scoped like
  Go to `polecat/` branches.
- **Spec gap: step 2 (the patrol wisp) must be role-guarded too.** The spec
  says steps 1–2 always run. But `gt mq post-merge` run by a human or a crew
  member would then close the refinery's live patrol wisp. So steps 2 and 3
  run only when the caller is this rig's refinery; step 1 (the chores) always
  runs.
- **Do not call `cleanupMoleculeOnHandoff`** (`handoff.go:1767`) on the
  unit-cycle path. It force-closes the molecule attached to the handoff bead,
  which would close the fresh patrol wisp that step 2 just poured.
- **The formula needs no version bump.** Its `version = 31`
  (`mol-refinery-patrol.formula.toml:117`) was not bumped by any of its last
  three edits (4b7fcbf, 16dd184, 15ac26b).
- **`MergeQueueConfig` lives at `internal/config/types.go:1384`**, not :744.
  It is resolved with `rig.ResolveMergeQueueConfig(townRoot, rigName)`
  (`mq_batch.go:350`).
- **Escalation convention in `internal/cmd`:**
  `exec.Command("gt", "escalate", "--severity", ..., "--fingerprint", ..., "--source", ..., msg)`.
  The flags are at `internal/cmd/escalate.go:180-184`; the precedent is
  `escalateRubricChange`, `mq.go:834`.

---

### Task 8: Extract reusable handoff and patrol pieces (no behaviour change)

**Files:**
- Modify: `internal/cmd/handoff.go`
  - `runHandoff` :320-377
  - `runHandoffCycle` :528-600
  - `getCurrentTmuxSession` :605-630
  - `enforceHandoffCooldown` :1856-1887
  - `recordHandoffTime` :1890-1900
- Modify: `internal/cmd/patrol_report.go`: `runPatrolReport` :48-200
- Test: `internal/cmd/handoff_test.go`, `internal/cmd/patrol_report_test.go` (new)

**Interfaces:**
- Produces, all in package `cmd`, for T9:
  - `func respawnOwnPane(t *tmux.Tmux, currentSession, pane, restartCmd string) error`
  - `func tmuxSessionForPane(pane string) (string, error)`
  - `func lastHandoffAge(dir string) (time.Duration, bool)`: `false` when no
    handoff has been recorded in `dir`.
  - `func recordHandoffTimeIn(dir string)`
  - `func writeHandoffMarker(dir, session, reason string)`
  - `func runPatrolReportFor(roleInfo RoleInfo, summary, steps string) error`

- [ ] **Step 1: Write the failing tests**

Append to `internal/cmd/handoff_test.go`:

```go
func TestLastHandoffAge(t *testing.T) {
	dir := t.TempDir()
	if _, ok := lastHandoffAge(dir); ok {
		t.Fatal("lastHandoffAge reported a handoff in an empty dir")
	}
	recordHandoffTimeIn(dir)
	age, ok := lastHandoffAge(dir)
	if !ok {
		t.Fatal("lastHandoffAge found no handoff after recordHandoffTimeIn")
	}
	if age < 0 || age > 5*time.Second {
		t.Fatalf("age = %v, want a fresh timestamp", age)
	}
}

func TestWriteHandoffMarker(t *testing.T) {
	dir := t.TempDir()
	writeHandoffMarker(dir, "gt-refinery", "unit-cycle")
	got, err := os.ReadFile(filepath.Join(dir, constants.DirRuntime, constants.FileHandoffMarker))
	if err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	if string(got) != "gt-refinery\nunit-cycle" {
		t.Fatalf("marker = %q, want session\\nreason", got)
	}
	writeHandoffMarker(dir, "gt-refinery", "")
	got, _ = os.ReadFile(filepath.Join(dir, constants.DirRuntime, constants.FileHandoffMarker))
	if string(got) != "gt-refinery" {
		t.Fatalf("marker without reason = %q, want bare session", got)
	}
}
```

Create `internal/cmd/patrol_report_test.go`:

```go
package cmd

import "testing"

func TestRunPatrolReportForRejectsNonPatrolRole(t *testing.T) {
	err := runPatrolReportFor(RoleInfo{Role: RoleCrew, Rig: "gastown", TownRoot: t.TempDir()}, "x", "")
	if err == nil {
		t.Fatal("runPatrolReportFor accepted a crew role; only deacon, witness and refinery patrol")
	}
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/cmd/ -run 'TestLastHandoffAge|TestWriteHandoffMarker|TestRunPatrolReportForRejectsNonPatrolRole' -count=1`
Expected: build failure: `undefined: lastHandoffAge`, `undefined: recordHandoffTimeIn`, `undefined: writeHandoffMarker`, `undefined: runPatrolReportFor`.

- [ ] **Step 3: Implement the extractions in `internal/cmd/handoff.go`**

Add these near `recordHandoffTime`:

```go
// lastHandoffAge reports how long ago the last handoff recorded in dir was.
// ok is false when none has been recorded there.
func lastHandoffAge(dir string) (age time.Duration, ok bool) {
	info, err := os.Stat(filepath.Join(dir, constants.DirRuntime, constants.FileLastHandoffTS))
	if err != nil {
		return 0, false
	}
	return time.Since(info.ModTime()), true
}

// recordHandoffTimeIn writes the handoff cooldown timestamp under dir.
func recordHandoffTimeIn(dir string) {
	runtimeDir := filepath.Join(dir, constants.DirRuntime)
	_ = os.MkdirAll(runtimeDir, 0755)
	_ = os.WriteFile(filepath.Join(runtimeDir, constants.FileLastHandoffTS),
		[]byte(fmt.Sprintf("%d", time.Now().Unix())), 0644)
}

// writeHandoffMarker tells the successor's gt prime it is post-handoff.
// Format "session\nreason"; see isCompactResume (prime.go:513).
func writeHandoffMarker(dir, session, reason string) {
	runtimeDir := filepath.Join(dir, constants.DirRuntime)
	_ = os.MkdirAll(runtimeDir, 0755)
	content := session
	if reason != "" {
		content += "\n" + reason
	}
	_ = os.WriteFile(filepath.Join(runtimeDir, constants.FileHandoffMarker), []byte(content), 0644)
}

// respawnOwnPane replaces the caller's own pane process with restartCmd.
// It must not kill pane processes first: that kills this process before
// respawn-pane runs (gh#859). respawn-pane -k does the kill atomically.
func respawnOwnPane(t *tmux.Tmux, currentSession, pane, restartCmd string) error {
	if err := t.SetRemainOnExit(pane, true); err != nil {
		style.PrintWarning("could not set remain-on-exit: %v", err)
	}
	if err := t.ClearHistory(pane); err != nil {
		style.PrintWarning("could not clear history: %v", err)
	}
	paneWorkDir, _ := t.GetPaneWorkDir(currentSession)
	if paneWorkDir != "" {
		if _, err := os.Stat(paneWorkDir); err != nil {
			if townRoot := detectTownRootFromCwd(); townRoot != "" {
				style.PrintWarning("pane working directory deleted, using town root")
				return t.RespawnPaneWithWorkDir(pane, townRoot, restartCmd)
			}
		}
	}
	return t.RespawnPane(pane, restartCmd)
}

// tmuxSessionForPane returns the session that owns pane. Unlike
// getCurrentTmuxSession it never consults GT_ROLE, so it tells where the
// process really runs.
func tmuxSessionForPane(pane string) (string, error) {
	out, err := tmux.BuildCommand("display-message", "-t", pane, "-p", "#{session_name}").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
```

Then rewire the existing callers, with the same observable behaviour:

- `recordHandoffTime()` body becomes
  `if cwd, err := os.Getwd(); err == nil { recordHandoffTimeIn(cwd) }`.
- In `enforceHandoffCooldown`, replace the `tsPath`/`os.Stat`/`time.Since`
  block with
  `age, ok := lastHandoffAge(cwd); if !ok || age >= constants.MinHandoffCooldown { return }`.
  The print and sleep stay as they are.
- In `getCurrentTmuxSession`, replace the `display-message` tail (:621-629)
  with `return tmuxSessionForPane(pane)`. Keep the `TMUX_PANE` empty check
  before it.
- In `runHandoff`:
  - delete the `t.ClearHistory(pane)` block (:328-332);
  - replace the inline marker write (:337-342) with
    `if cwd, err := os.Getwd(); err == nil { writeHandoffMarker(cwd, currentSession, "") }`;
  - replace everything from the `SetRemainOnExit` comment (:347) to
    `return t.RespawnPane(pane, restartCmd)` (:377) with
    `return respawnOwnPane(t, currentSession, pane, restartCmd)`.

  Clear-history moves after the marker write. Nothing observes the order.
- In `runHandoffCycle`:
  - replace the marker block with
    `if cwd, err := os.Getwd(); err == nil { writeHandoffMarker(cwd, currentSession, handoffReason) }`;
  - replace the `SetRemainOnExit` … `RespawnPane` tail with
    `return respawnOwnPane(t, currentSession, pane, restartCmd)`.

- [ ] **Step 4: Extract `runPatrolReportFor` in `internal/cmd/patrol_report.go`**

`runPatrolReport` becomes:

```go
func runPatrolReport(cmd *cobra.Command, args []string) error {
	roleInfo, err := GetRole()
	if err != nil {
		return fmt.Errorf("detecting role: %w", err)
	}
	return runPatrolReportFor(roleInfo, patrolReportSummary, patrolReportSteps)
}

// runPatrolReportFor closes roleInfo's active patrol with summary and pours
// the next cycle. It is shared by `gt patrol report` and the refinery's
// unit cycle (completeUnitAndCycle).
func runPatrolReportFor(roleInfo RoleInfo, summary, steps string) error {
	roleName := string(roleInfo.Role)
	// ... the former body of runPatrolReport from `var cfg PatrolConfig`
	//     (old :58) to its final `return nil`, unchanged except:
	//       patrolReportSummary -> summary
	//       patrolReportSteps   -> steps
}
```

This is a move, not a rewrite. Cut the old body verbatim and rename only the
two package-level flag reads. `stampDeaconHeartbeatOnReport(cfg.BeadsDir, summary)`
also takes the parameter.

- [ ] **Step 5: Run the new tests and the existing handoff and patrol suites**

Run: `go test ./internal/cmd/ -run 'TestLastHandoffAge|TestWriteHandoffMarker|TestRunPatrolReportForRejectsNonPatrolRole|TestRecordHandoffTime|TestEnforceHandoffCooldown|TestHandoff|TestBuildRestartCommand|Patrol' -count=1`
Expected: `ok  	github.com/steveyegge/gastown/internal/cmd`

- [ ] **Step 6: Commit**

```bash
git add internal/cmd/handoff.go internal/cmd/handoff_test.go internal/cmd/patrol_report.go internal/cmd/patrol_report_test.go
git commit -m "refactor(handoff): extract pane respawn, cooldown and patrol-report helpers

The refinery unit cycle needs to respawn its own pane, read the handoff
cooldown without sleeping, and close its patrol cycle without going
through cobra flags. Behaviour of gt handoff and gt patrol report is
unchanged."
```

---

### Task 9: `completeUnitAndCycle`: finish the unit in Go, then respawn the refinery

**Files:**
- Modify: `internal/config/types.go:1384` (the `MergeQueueConfig` struct)
- Create: `internal/cmd/unit_cycle.go`
- Create: `internal/cmd/unit_cycle_test.go`
- Modify: `internal/cmd/mq.go`: `runMQPostMerge` :746-771
- Modify: `internal/cmd/mq_batch.go`: `runMQBatchRun`, the landed path after
  the JSON or text output (:430-470)

**Interfaces:**
- Consumes from T8: `respawnOwnPane`, `tmuxSessionForPane`, `lastHandoffAge`,
  `recordHandoffTimeIn`, `writeHandoffMarker`, `runPatrolReportFor`.
- Consumes from MR-A: `runPostMergeCommand(p postMergeCommandParams)` is
  already called in `runMQPostMerge` right after `handlePostMergeRubricChange`,
  and once in `runMQBatchRun` after the batch landed. T9's calls go
  immediately after those.
- Produces:
  - `config.MergeQueueConfig.CycleSessionAfterMerge bool` (json `cycle_session_after_merge,omitempty`)
  - `type unitMode int` with `unitSingle`, `unitBatch`
  - `type unitMR struct{ ID, Branch, Worker, SourceIssue, Target string }`
  - `type unitCycleParams struct` (below)
  - `type unitCycleDeps struct` (below)
  - `func completeUnitAndCycle(p unitCycleParams, d unitCycleDeps) unitCycleReport`
  - `func defaultUnitCycleDeps(townRoot string, beadsPath string) unitCycleDeps`

- [ ] **Step 1: Add the config field**

In `internal/config/types.go`, inside `MergeQueueConfig` (after `BatchMinCount`, :1517):

```go
	// CycleSessionAfterMerge makes `gt mq post-merge` / `gt mq batch run`
	// respawn the rig's refinery session in place after each landed unit
	// (one MR or one batch), so every unit starts in a fresh context.
	// Off by default; opt in per rig.
	CycleSessionAfterMerge bool `json:"cycle_session_after_merge,omitempty"`
```

- [ ] **Step 2: Write the failing tests in `internal/cmd/unit_cycle_test.go`**

```go
package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/mail"
)

type fakeUnitCycle struct {
	sent        []*mail.Message
	sendErr     error
	inbox       []*mail.Message
	archived    []string
	comments    []string
	tempDeleted []string
	patrolSum   []string
	patrolErr   error
	respawned   int
	respawnErr  error
	escalations []string
	recorded    int
	paneSession string
	handoffAge  time.Duration
	hasHandoff  bool
	env         map[string]string
}

func (f *fakeUnitCycle) deps(out *bytes.Buffer) unitCycleDeps {
	return unitCycleDeps{
		Send:         func(m *mail.Message) error { f.sent = append(f.sent, m); return f.sendErr },
		ListInbox:    func() ([]*mail.Message, error) { return f.inbox, nil },
		Archive:      func(id string) error { f.archived = append(f.archived, id); return nil },
		AddComment:   func(id, text string) error { f.comments = append(f.comments, id+": "+text); return nil },
		DeleteTemp:   func(dir string) error { f.tempDeleted = append(f.tempDeleted, dir); return nil },
		ClosePatrol:  func(summary string) error { f.patrolSum = append(f.patrolSum, summary); return f.patrolErr },
		Respawn:      func() error { f.respawned++; return f.respawnErr },
		Escalate:     func(fp, sev, msg string) { f.escalations = append(f.escalations, fp+"|"+sev) },
		RecordCycle:  func() { f.recorded++ },
		PaneSession:  func() (string, error) { return f.paneSession, nil },
		HandoffAge:   func() (time.Duration, bool) { return f.handoffAge, f.hasHandoff },
		Getenv:       func(k string) string { return f.env[k] },
		Out:          out,
	}
}

func refineryFake() *fakeUnitCycle {
	return &fakeUnitCycle{
		paneSession: "gt-refinery",
		env:         map[string]string{"GT_ROLE": "gastown/refinery", "TMUX_PANE": "%7"},
		inbox: []*mail.Message{
			{ID: "hq-m1", Subject: "MERGE_READY nux", Body: "Branch: polecat/nux/gt-1+ab\nIssue: gt-1\n"},
			{ID: "hq-m2", Subject: "MERGE_READY opal", Body: "Branch: polecat/opal/gt-2+cd\nIssue: gt-2\n"},
		},
	}
}

func singleParams() unitCycleParams {
	return unitCycleParams{
		Rig: "gastown", Mode: unitSingle,
		RefinerySession: "gt-refinery",
		WorkDir:         "/town/gastown/refinery/rig",
		MRs:             []unitMR{{ID: "gt-mr1", Branch: "polecat/nux/gt-1+ab", Worker: "polecats/nux", SourceIssue: "gt-1", Target: "main"}},
		MergeCommit:     "abc123",
		CycleEnabled:    true,
	}
}

func TestCompleteUnitAndCycle_SingleDoesChoresPatrolAndRespawn(t *testing.T) {
	f := refineryFake()
	var out bytes.Buffer
	p := singleParams()
	p.LandedCommitAttested = true
	rep := completeUnitAndCycle(p, f.deps(&out))

	if len(f.sent) != 1 || f.sent[0].Subject != "MERGED nux" || f.sent[0].To != "gastown/witness" {
		t.Fatalf("MERGED sends = %+v, want one MERGED nux to gastown/witness", f.sent)
	}
	if !strings.Contains(f.sent[0].Body, "Merge-Commit: abc123") {
		t.Fatalf("MERGED body missing merge commit: %q", f.sent[0].Body)
	}
	if len(f.archived) != 1 || f.archived[0] != "hq-m1" {
		t.Fatalf("archived = %v, want only hq-m1 (the MR's own MERGE_READY)", f.archived)
	}
	if len(f.comments) != 1 || !strings.Contains(f.comments[0], "gt-mr1: post-merge: attested --landed-commit abc123") {
		t.Fatalf("attestation comments = %v", f.comments)
	}
	if len(f.tempDeleted) != 1 || f.tempDeleted[0] != "/town/gastown/refinery/rig" {
		t.Fatalf("temp deleted in %v, want the refinery worktree", f.tempDeleted)
	}
	if len(f.patrolSum) != 1 || f.patrolSum[0] != "unit landed: gt-mr1 @ abc123" {
		t.Fatalf("patrol summaries = %v", f.patrolSum)
	}
	if f.respawned != 1 || f.recorded != 1 || !rep.Respawned {
		t.Fatalf("respawned=%d recorded=%d report=%+v, want one respawn", f.respawned, f.recorded, rep)
	}
	for _, line := range []string{"✓ MERGED sent to gastown/witness", "✓ MERGE_READY archived: hq-m1", "✓ temp branch deleted"} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("output missing %q:\n%s", line, out.String())
		}
	}
}

func TestCompleteUnitAndCycle_NoAttestationWithoutLandedCommit(t *testing.T) {
	f := refineryFake()
	completeUnitAndCycle(singleParams(), f.deps(&bytes.Buffer{}))
	if len(f.comments) != 0 {
		t.Fatalf("comments = %v, want none when --landed-commit was not used", f.comments)
	}
}

func TestCompleteUnitAndCycle_NonPolecatBranchGetsNoMERGED(t *testing.T) {
	f := refineryFake()
	p := singleParams()
	p.MRs[0].Branch = "crew/sloan/fix"
	completeUnitAndCycle(p, f.deps(&bytes.Buffer{}))
	if len(f.sent) != 0 {
		t.Fatalf("sent %d MERGED for a non-polecat branch; the witness has no worktree to reap", len(f.sent))
	}
}

func TestCompleteUnitAndCycle_BatchSkipsMERGEDAndTempButArchivesEach(t *testing.T) {
	f := refineryFake()
	p := singleParams()
	p.Mode = unitBatch
	p.MRs = []unitMR{
		{ID: "gt-mr1", Branch: "polecat/nux/gt-1+ab", Worker: "polecats/nux"},
		{ID: "gt-mr2", Branch: "polecat/opal/gt-2+cd", Worker: "polecats/opal"},
	}
	completeUnitAndCycle(p, f.deps(&bytes.Buffer{}))
	if len(f.sent) != 0 {
		t.Fatalf("batch sent %d MERGED; HandleMRInfoSuccess already sent them (gt-9gjl)", len(f.sent))
	}
	if len(f.tempDeleted) != 0 {
		t.Fatalf("batch deleted temp in %v; batch never creates temp", f.tempDeleted)
	}
	if strings.Join(f.archived, ",") != "hq-m1,hq-m2" {
		t.Fatalf("archived = %v, want both members' MERGE_READY", f.archived)
	}
	if len(f.patrolSum) != 1 || f.patrolSum[0] != "unit landed: gt-mr1,gt-mr2 @ abc123" || f.respawned != 1 {
		t.Fatalf("batch patrol=%v respawned=%d, want one patrol close and one respawn per batch", f.patrolSum, f.respawned)
	}
}

func TestCompleteUnitAndCycle_GuardsSkipPatrolAndRespawnButKeepChores(t *testing.T) {
	cases := map[string]func(p *unitCycleParams, f *fakeUnitCycle){
		"human caller":       func(p *unitCycleParams, f *fakeUnitCycle) { f.env["GT_ROLE"] = "gastown/crew/sloan" },
		"other rig refinery": func(p *unitCycleParams, f *fakeUnitCycle) { f.env["GT_ROLE"] = "hm/refinery" },
		"not in tmux":        func(p *unitCycleParams, f *fakeUnitCycle) { delete(f.env, "TMUX_PANE") },
		"wrong pane session": func(p *unitCycleParams, f *fakeUnitCycle) { f.paneSession = "gt-crew-sloan" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := refineryFake()
			p := singleParams()
			mutate(&p, f)
			rep := completeUnitAndCycle(p, f.deps(&bytes.Buffer{}))
			if len(f.sent) != 1 || len(f.archived) != 1 {
				t.Fatalf("chores skipped: sent=%d archived=%d; chores always run", len(f.sent), len(f.archived))
			}
			if len(f.patrolSum) != 0 {
				t.Fatalf("closed the refinery patrol from a non-refinery caller: %v", f.patrolSum)
			}
			if f.respawned != 0 || rep.Respawned {
				t.Fatal("respawned from a non-refinery caller")
			}
		})
	}
}

func TestCompleteUnitAndCycle_FlagOffOrCooldownClosesPatrolButNoRespawn(t *testing.T) {
	for name, mutate := range map[string]func(p *unitCycleParams, f *fakeUnitCycle){
		"flag off": func(p *unitCycleParams, f *fakeUnitCycle) { p.CycleEnabled = false },
		"cooldown": func(p *unitCycleParams, f *fakeUnitCycle) { f.hasHandoff = true; f.handoffAge = 30 * time.Second },
	} {
		t.Run(name, func(t *testing.T) {
			f := refineryFake()
			p := singleParams()
			mutate(&p, f)
			rep := completeUnitAndCycle(p, f.deps(&bytes.Buffer{}))
			if len(f.patrolSum) != 1 {
				t.Fatalf("patrol not closed: %v", f.patrolSum)
			}
			if f.respawned != 0 || rep.Respawned {
				t.Fatal("respawned despite flag off / cooldown")
			}
		})
	}
}

func TestCompleteUnitAndCycle_OldHandoffIsNotCooldown(t *testing.T) {
	f := refineryFake()
	f.hasHandoff, f.handoffAge = true, 10*time.Minute
	completeUnitAndCycle(singleParams(), f.deps(&bytes.Buffer{}))
	if f.respawned != 1 {
		t.Fatalf("respawned=%d, want 1 when the last handoff is past MinHandoffCooldown", f.respawned)
	}
}

func TestCompleteUnitAndCycle_FailedMERGEDEscalatesAndOtherChoresRun(t *testing.T) {
	f := refineryFake()
	f.sendErr = errors.New("dolt down")
	completeUnitAndCycle(singleParams(), f.deps(&bytes.Buffer{}))
	if len(f.escalations) != 1 || f.escalations[0] != "refinery-unit-chore:gastown|medium" {
		t.Fatalf("escalations = %v, want one refinery-unit-chore:gastown", f.escalations)
	}
	if len(f.archived) != 1 || len(f.tempDeleted) != 1 || f.respawned != 1 {
		t.Fatal("a failed MERGED send stopped the other chores or the cycle")
	}
}

func TestCompleteUnitAndCycle_PatrolFailureStillRespawns(t *testing.T) {
	f := refineryFake()
	f.patrolErr = errors.New("bd timeout")
	completeUnitAndCycle(singleParams(), f.deps(&bytes.Buffer{}))
	if f.respawned != 1 {
		t.Fatal("patrol close failure blocked the respawn; the successor finds the old wisp as today")
	}
	if len(f.escalations) != 0 {
		t.Fatalf("patrol failure escalated %v; the patrol watchdog owns stuck wisps", f.escalations)
	}
}

func TestCompleteUnitAndCycle_RespawnFailureEscalates(t *testing.T) {
	f := refineryFake()
	f.respawnErr = errors.New("no server")
	rep := completeUnitAndCycle(singleParams(), f.deps(&bytes.Buffer{}))
	if rep.Respawned {
		t.Fatal("report claims a respawn that failed")
	}
	if len(f.escalations) != 1 || f.escalations[0] != "refinery-respawn-failed:gastown|medium" {
		t.Fatalf("escalations = %v", f.escalations)
	}
}
```

- [ ] **Step 3: Run the tests and confirm they fail**

Run: `go test ./internal/cmd/ -run 'TestCompleteUnitAndCycle' -count=1`
Expected: build failure: `undefined: completeUnitAndCycle`, `undefined: unitCycleDeps`, …

- [ ] **Step 4: Implement `internal/cmd/unit_cycle.go`**

```go
package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/protocol"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
)

type unitMode int

const (
	unitSingle unitMode = iota // gt mq post-merge: the agent's per-MR chores move here
	unitBatch                  // gt mq batch run: HandleMRInfoSuccess already sent MERGED per member
)

type unitMR struct {
	ID, Branch, Worker, SourceIssue, Target string
}

type unitCycleParams struct {
	Rig                  string
	Mode                 unitMode
	RefinerySession      string // session.RefinerySessionName(session.PrefixFor(Rig))
	WorkDir              string // <rig>/refinery/rig
	MRs                  []unitMR
	MergeCommit          string
	LandedCommitAttested bool // --landed-commit was passed
	CycleEnabled         bool // merge_queue.cycle_session_after_merge
}

type unitCycleDeps struct {
	Send        func(*mail.Message) error
	ListInbox   func() ([]*mail.Message, error)
	Archive     func(id string) error
	AddComment  func(beadID, text string) error
	DeleteTemp  func(workDir string) error
	ClosePatrol func(summary string) error
	Respawn     func() error
	Escalate    func(fingerprint, severity, msg string)
	RecordCycle func() // cooldown timestamp + handoff marker + town log
	PaneSession func() (string, error)
	HandoffAge  func() (time.Duration, bool)
	Getenv      func(string) string
	Out         io.Writer
}

type unitCycleReport struct {
	Respawned bool
	SkipCause string // why the patrol/respawn steps did not run; empty when they did
}

// completeUnitAndCycle ends a landed unit (one MR, or one batch): it does the
// per-MR chores the refinery formula used to leave to the agent, then, when
// the caller is this rig's refinery, closes the patrol cycle and respawns the
// refinery pane in place so the next unit starts in a fresh session.
// Every step is best-effort: a landed merge is never failed from here.
func completeUnitAndCycle(p unitCycleParams, d unitCycleDeps) unitCycleReport {
	ids := make([]string, 0, len(p.MRs))
	for _, mr := range p.MRs {
		ids = append(ids, mr.ID)
	}

	// 1. Chores. Always run: a human running post-merge by hand still wants them.
	for _, mr := range p.MRs {
		if p.Mode == unitSingle && strings.HasPrefix(mr.Branch, "polecat/") {
			polecat := strings.TrimPrefix(mr.Worker, "polecats/")
			msg := protocol.NewMergedMessage(p.Rig, polecat, mr.Branch, mr.SourceIssue, mr.Target, p.MergeCommit)
			if err := d.Send(msg); err != nil {
				fmt.Fprintf(d.Out, "  %s MERGED not sent for %s: %v\n", style.Error.Render("✗"), mr.ID, err)
				d.Escalate("refinery-unit-chore:"+p.Rig, "medium",
					fmt.Sprintf("MERGED to %s/witness failed for %s (%s): %v — polecat worktree will not be reaped until it is sent", p.Rig, mr.ID, mr.Branch, err))
			} else {
				fmt.Fprintf(d.Out, "  %s MERGED sent to %s/witness\n", style.Success.Render("✓"), p.Rig)
			}
		}
		archiveMergeReady(mr, d)
		if p.Mode == unitSingle && p.LandedCommitAttested {
			text := fmt.Sprintf("post-merge: attested --landed-commit %s", p.MergeCommit)
			if err := d.AddComment(mr.ID, text); err != nil {
				fmt.Fprintf(d.Out, "  %s attestation comment failed on %s: %v\n", style.Error.Render("✗"), mr.ID, err)
			}
		}
	}
	if p.Mode == unitSingle {
		if err := d.DeleteTemp(p.WorkDir); err != nil {
			fmt.Fprintf(d.Out, "  %s temp branch not deleted: %v\n", style.Dim.Render("○"), err)
		} else {
			fmt.Fprintf(d.Out, "  %s temp branch deleted\n", style.Success.Render("✓"))
		}
	}

	// 2 and 3 run only for this rig's refinery, inside its own pane: from any
	// other caller they would close the refinery's live patrol or kill the
	// caller's own pane.
	if cause := unitCycleCallerMismatch(p, d); cause != "" {
		return unitCycleReport{SkipCause: cause}
	}

	// 2. Close this patrol cycle and pour the next, so the successor starts clean.
	summary := fmt.Sprintf("unit landed: %s @ %s", strings.Join(ids, ","), p.MergeCommit)
	if err := d.ClosePatrol(summary); err != nil {
		fmt.Fprintf(d.Out, "  %s patrol cycle not closed: %v\n", style.Dim.Render("○"), err)
	}

	// 3. Respawn in place.
	if !p.CycleEnabled {
		return unitCycleReport{SkipCause: "cycle_session_after_merge is off"}
	}
	if age, ok := d.HandoffAge(); ok && age < constants.MinHandoffCooldown {
		fmt.Fprintf(d.Out, "  %s session kept: last handoff %v ago (< %v); next unit cycles\n",
			style.Dim.Render("○"), age.Round(time.Second), constants.MinHandoffCooldown)
		return unitCycleReport{SkipCause: "handoff cooldown"}
	}
	fmt.Fprintf(d.Out, "  %s unit complete — respawning %s for the next unit\n", style.Bold.Render("🔄"), p.RefinerySession)
	d.RecordCycle()
	if err := d.Respawn(); err != nil {
		fmt.Fprintf(d.Out, "  %s respawn failed: %v (continuing in this session)\n", style.Error.Render("✗"), err)
		d.Escalate("refinery-respawn-failed:"+p.Rig, "medium",
			fmt.Sprintf("refinery %s could not respawn after %s: %v", p.RefinerySession, summary, err))
		return unitCycleReport{SkipCause: "respawn failed"}
	}
	return unitCycleReport{Respawned: true}
}

func unitCycleCallerMismatch(p unitCycleParams, d unitCycleDeps) string {
	if d.Getenv("GT_ROLE") != p.Rig+"/refinery" {
		return "caller is not " + p.Rig + "/refinery"
	}
	if d.Getenv("TMUX_PANE") == "" {
		return "not inside tmux"
	}
	sess, err := d.PaneSession()
	if err != nil || sess != p.RefinerySession {
		return "pane is not in " + p.RefinerySession
	}
	return ""
}

// archiveMergeReady archives the MERGE_READY mail for mr, matched by its
// "Branch: <branch>" body line (protocol.formatMergeReadyBody).
func archiveMergeReady(mr unitMR, d unitCycleDeps) {
	msgs, err := d.ListInbox()
	if err != nil {
		fmt.Fprintf(d.Out, "  %s MERGE_READY not archived for %s: %v\n", style.Dim.Render("○"), mr.ID, err)
		return
	}
	want := "Branch: " + mr.Branch
	for _, m := range msgs {
		if !strings.HasPrefix(m.Subject, "MERGE_READY ") {
			continue
		}
		for _, line := range strings.Split(m.Body, "\n") {
			if strings.TrimSpace(line) == want {
				if err := d.Archive(m.ID); err != nil {
					fmt.Fprintf(d.Out, "  %s MERGE_READY %s not archived: %v\n", style.Dim.Render("○"), m.ID, err)
				} else {
					fmt.Fprintf(d.Out, "  %s MERGE_READY archived: %s\n", style.Success.Render("✓"), m.ID)
				}
				break
			}
		}
	}
}

// defaultUnitCycleDeps wires the real mail, beads, git, patrol and tmux seams.
func defaultUnitCycleDeps(townRoot, beadsPath, workDir, refinerySession string) unitCycleDeps {
	router := mail.NewRouter(townRoot)
	return unitCycleDeps{
		Send: router.Send,
		ListInbox: func() ([]*mail.Message, error) {
			mb, err := getMailbox(detectSender())
			if err != nil {
				return nil, err
			}
			return mb.List()
		},
		Archive: func(id string) error {
			mb, err := getMailbox(detectSender())
			if err != nil {
				return err
			}
			return mb.Delete(id)
		},
		AddComment: func(id, text string) error { return beads.New(beadsPath).AddComment(id, text) },
		DeleteTemp: func(dir string) error {
			// -D, not -d: temp is the disposable rehearsal branch and is never
			// itself merged, so -d can refuse it as "not fully merged".
			c := exec.Command("git", "branch", "-D", "temp")
			c.Dir = dir
			if out, err := c.CombinedOutput(); err != nil {
				return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
			}
			return nil
		},
		ClosePatrol: func(summary string) error {
			roleInfo, err := GetRole()
			if err != nil {
				return err
			}
			return runPatrolReportFor(roleInfo, summary, "")
		},
		Respawn: func() error {
			pane := os.Getenv("TMUX_PANE")
			restartCmd, err := buildRestartCommandWithOpts(refinerySession, buildRestartCommandOpts{ContinueSession: false})
			if err != nil {
				return err
			}
			t := tmux.NewTmuxWithSocket(tmux.SocketFromEnv())
			updateSessionEnvForHandoff(t, refinerySession)
			return respawnOwnPane(t, refinerySession, pane, restartCmd)
		},
		Escalate: func(fp, sev, msg string) {
			c := exec.Command("gt", "escalate", "--severity", sev, "--fingerprint", fp, "--source", "refinery:unit-cycle", msg)
			if err := c.Run(); err != nil {
				style.PrintWarning("escalation %s failed: %v", fp, err)
			}
		},
		RecordCycle: func() {
			recordHandoffTimeIn(workDir)
			writeHandoffMarker(workDir, refinerySession, "unit-cycle")
			agent := sessionToGTRole(refinerySession)
			_ = LogHandoff(townRoot, agent, "unit-cycle")
			_ = events.LogFeed(events.TypeHandoff, agent, events.HandoffPayload("unit-cycle", true))
		},
		PaneSession: func() (string, error) { return tmuxSessionForPane(os.Getenv("TMUX_PANE")) },
		HandoffAge:  func() (time.Duration, bool) { return lastHandoffAge(workDir) },
		Getenv:      os.Getenv,
		Out:         os.Stdout,
	}
}

// refineryWorkDir is where the refinery session runs and where its handoff
// runtime files live.
func refineryWorkDir(rigPath string) string { return filepath.Join(rigPath, "refinery", "rig") }

func refinerySessionFor(rigName string) string {
	return session.RefinerySessionName(session.PrefixFor(rigName))
}
```

Deliberately **not** called: `cleanupMoleculeOnHandoff`, which would force-close
the patrol wisp step 2 just poured; `sendHandoffMail`, since the refinery needs
nothing from it (spec, "fresh session per unit"); and `enforceHandoffCooldown`,
which would sleep. The unit skips instead.

Two notes on `defaultUnitCycleDeps`:

- `runPatrolReportFor` uses `GetRole()`, the same identity `gt patrol report`
  uses. Guard 2 has already proved `GT_ROLE` is `<rig>/refinery`.
- `HandoffAge` and `RecordCycle` both read and write in `workDir`
  (`<rig>/refinery/rig`, the refinery session's working directory). The
  formula's `gt handoff` context-yield path writes there too, when run from
  the session's home directory.

- [ ] **Step 5: Run the unit tests**

Run: `go test ./internal/cmd/ -run 'TestCompleteUnitAndCycle' -count=1`
Expected: `ok  	github.com/steveyegge/gastown/internal/cmd`

- [ ] **Step 6: Wire the single-MR call site**

`runMQPostMerge` (`internal/cmd/mq.go:746`) already ends with MR-A's install
hook (Task 3):

```go
	printMQPostMergeResult(result, branchCleanup)

	townRoot := filepath.Dir(r.Path)
	runMRPostMergeCommand(townRoot, r.Name, r.Path, rig.ResolveMergeQueueConfig(townRoot, r.Name), result.MR, os.Stdout)
	return nil
```

**Keep the `runMRPostMergeCommand` line.** The install must run before the
unit cycle can respawn the pane. Replace that tail with:

```go
	printMQPostMergeResult(result, branchCleanup)

	townRoot := filepath.Dir(r.Path)
	mqCfg := rig.ResolveMergeQueueConfig(townRoot, r.Name)
	runMRPostMergeCommand(townRoot, r.Name, r.Path, mqCfg, result.MR, os.Stdout)
	mergeCommit := strings.TrimSpace(result.MR.MergeCommit)
	if mergeCommit == "" {
		mergeCommit = resolveMQPostMergeCommit(rigGit, "origin/"+result.MR.TargetBranch)
	}
	sess := refinerySessionFor(r.Name)
	workDir := refineryWorkDir(r.Path)
	completeUnitAndCycle(unitCycleParams{
		Rig:             r.Name,
		Mode:            unitSingle,
		RefinerySession: sess,
		WorkDir:         workDir,
		MRs: []unitMR{{
			ID: result.MR.ID, Branch: result.MR.Branch, Worker: result.MR.Worker,
			SourceIssue: result.SourceIssueID, Target: result.MR.TargetBranch,
		}},
		MergeCommit:          mergeCommit,
		LandedCommitAttested: mqPostMergeLandedCommit != "",
		CycleEnabled:         mqCfg != nil && mqCfg.CycleSessionAfterMerge,
	}, defaultUnitCycleDeps(townRoot, r.BeadsPath(), workDir, sess))
	return nil
```

The call is the last thing `runMQPostMerge` does. Everything the post-merge
command owes its caller (MR and issue closed, branch deleted, ✓ lines
printed, MR-A's install hook) has finished before the respawn can kill the
process.

The `townRoot := filepath.Dir(r.Path)` local comes from MR-A's Task 3; don't
declare it twice. Also confirm that
`resolveMQPostMergeCommit(rigGit, ref)` (`mq.go:1215`) resolves a ref to a
SHA; if it doesn't, use `rigGit.RevParse(ref)`, or whatever MR-A's T3 used
for the same fallback.

The orphan branch (`if orphan { … return nil }`) stays as it is. An orphan
cleanup has no MR, and it is not a unit.

- [ ] **Step 7: Wire the batch call site**

In `runMQBatchRun` (`internal/cmd/mq_batch.go`), put this **directly after
MR-A's `runBatchPostMergeCommand(...)` call (Task 4) and before the
`if result.Error != nil { return … }` check.** A batch whose push landed but
where some members' cleanup failed still counts as a landed unit.

```go
	if result.MergeCommit != "" && len(result.Merged) > 0 {
		sess := refinerySessionFor(r.Name)
		workDir := refineryWorkDir(r.Path)
		mrs := make([]unitMR, 0, len(result.Merged))
		for _, m := range result.Merged {
			mrs = append(mrs, unitMR{ID: m.ID, Branch: m.Branch, Worker: m.Worker, SourceIssue: m.SourceIssue, Target: m.Target})
		}
		completeUnitAndCycle(unitCycleParams{
			Rig:             r.Name,
			Mode:            unitBatch,
			RefinerySession: sess,
			WorkDir:         workDir,
			MRs:             mrs,
			MergeCommit:     result.MergeCommit,
			CycleEnabled:    mq != nil && mq.CycleSessionAfterMerge,
		}, defaultUnitCycleDeps(townRoot, r.BeadsPath(), workDir, sess))
	}
```

The JSON has already been written to stdout at this point. The formula
redirects it to `/tmp/<rig>-batch.json`, and the successor never reads that
file. Only the waiting agent does, and it is being replaced.

In JSON mode, the chore and respawn lines go to stdout after the JSON object
and would corrupt it. So when `mqBatchRunJSON` is set, pass a copy of
`defaultUnitCycleDeps(...)` with `Out: cmd.ErrOrStderr()`:

```go
		deps := defaultUnitCycleDeps(townRoot, r.BeadsPath(), workDir, sess)
		if mqBatchRunJSON {
			deps.Out = cmd.ErrOrStderr()
		}
```

In that case, use `deps` in the call above.

- [ ] **Step 8: Add a call-site test for the batch once-per-batch rule**

Append to `internal/cmd/unit_cycle_test.go`:

```go
func TestCompleteUnitAndCycle_BatchIsOneUnit(t *testing.T) {
	f := refineryFake()
	p := singleParams()
	p.Mode = unitBatch
	p.MRs = []unitMR{{ID: "a", Branch: "polecat/x/1"}, {ID: "b", Branch: "polecat/y/2"}, {ID: "c", Branch: "polecat/z/3"}}
	completeUnitAndCycle(p, f.deps(&bytes.Buffer{}))
	if len(f.patrolSum) != 1 || f.respawned != 1 {
		t.Fatalf("3-member batch closed %d patrols and respawned %d times, want 1 and 1", len(f.patrolSum), f.respawned)
	}
}
```

- [ ] **Step 9: Build and run the cmd and config suites**

Run: `go build ./... && go test ./internal/cmd/ -run 'TestCompleteUnitAndCycle|TestRunVerifiedMQPostMerge|MQBatch|MQPostMerge' -count=1 && go test ./internal/config/ -count=1`
Expected: both `ok`.

- [ ] **Step 10: Commit**

```bash
git add internal/config/types.go internal/cmd/unit_cycle.go internal/cmd/unit_cycle_test.go internal/cmd/mq.go internal/cmd/mq_batch.go
git commit -m "feat(refinery): finish each landed unit in post-merge and respawn the refinery

The refinery skipped its end-of-cycle steps for hours while merging, and
loop-check never reaches burn-or-loop while the queue has work, so a
formula-level per-unit handoff would not fire. gt mq post-merge and
gt mq batch run now send MERGED (single-MR), archive MERGE_READY, add the
landed-commit attestation, delete temp, close the patrol cycle and, when
merge_queue.cycle_session_after_merge is set and the caller is that rig's
refinery pane, respawn the pane in place with no handoff mail."
```

---

### Task 10: Formula text: post-merge now does the per-MR chores

**Files:**
- Modify: `internal/formula/formulas/mol-refinery-patrol.formula.toml`
  - merge-push Step 3 to Step 5 plus the VERIFICATION GATE (:1379-1420)
  - batch-scan Step 4 (:497-512)
  - merged-pr-sweep MERGED instructions (:347-355)
- Create: `internal/formula/refinery_unit_cycle_test.go`

**Interfaces:** consumes T9's output lines, verbatim:
- `✓ MERGED sent to <rig>/witness`
- `✓ MERGE_READY archived: <id>`
- `✓ temp branch deleted`
- `🔄 unit complete — respawning`

- [ ] **Step 1: Write the failing test**

`internal/formula/refinery_unit_cycle_test.go`:

```go
package formula

import (
	"strings"
	"testing"
)

// claude-7fc: gt mq post-merge now sends MERGED, archives MERGE_READY,
// attests the landed commit and deletes temp, then may respawn the refinery
// session. The formula must stop telling the agent to do those chores after
// post-merge (a respawn would cut them off, and a repeat is a duplicate
// MERGED mail), and must tell it to expect the respawn.
func TestRefineryPatrolPostMergeOwnsPerMRChores(t *testing.T) {
	f := loadRefineryPatrolFormula(t)

	mergePush := requireFormulaStep(t, f, "merge-push").Description
	batchScan := requireFormulaStep(t, f, "batch-scan").Description
	mergedSweep := requireFormulaStep(t, f, "merged-pr-sweep").Description

	for id, desc := range map[string]string{"merge-push": mergePush, "batch-scan": batchScan, "merged-pr-sweep": mergedSweep} {
		if strings.Contains(desc, `gt mail send <rig>/witness -s "MERGED`) {
			t.Errorf("%s still tells the agent to send MERGED; post-merge / batch run sends it", id)
		}
	}
	if strings.Contains(mergePush, "gt mail archive <merge-ready-message-id>") {
		t.Error("merge-push still tells the agent to archive MERGE_READY; post-merge archives it")
	}
	if strings.Contains(mergePush, `bd comments add <mr-bead-id> "post-merge: attested`) {
		t.Error("merge-push still adds the attestation comment by hand; post-merge adds it")
	}
	for _, want := range []string{"✓ MERGED sent", "✓ MERGE_READY archived", "✓ temp branch deleted", "respawn"} {
		if !strings.Contains(mergePush, want) {
			t.Errorf("merge-push does not tell the agent to verify/expect %q", want)
		}
	}
	// The re-key must still precede post-merge (refinery_note_landed_copy_test).
	if !strings.Contains(mergePush, "gt mq post-merge <rig> <mr-bead-id>") {
		t.Fatal("merge-push lost its post-merge invocation")
	}
}
```

- [ ] **Step 2: Run the test and confirm it fails**

Run: `go test ./internal/formula/ -run TestRefineryPatrolPostMergeOwnsPerMRChores -count=1`
Expected: FAIL. The failures are "merge-push still tells the agent to send MERGED" (plus the same for batch-scan and merged-pr-sweep), "still … archive", "still … attestation", and "does not tell the agent to verify/expect …".

- [ ] **Step 3: Edit the formula**

In merge-push:

1. Delete the attestation block that follows
   `gt mq post-merge <rig> <mr-bead-id> $LANDED_FLAG $SKIP_FLAG`
   (the `if [ -n "$LANDED_FLAG" ]; then bd comments add … fi` fence and its
   lead-in sentence).
2. Replace everything from `**Step 3: Send MERGED Notification` through the
   end of the `**VERIFICATION GATE**` checklist with:

```
**Step 3: Verify post-merge finished the unit (REQUIRED)**

`gt mq post-merge` now does the per-MR chores itself (claude-7fc) and prints
a line for each:

- `✓ MERGED sent to <rig>/witness` — polecat branches only
- `✓ MERGE_READY archived: <id>`
- `✓ temp branch deleted`
- the landed-commit attestation comment on the MR bead, when you passed
  `--landed-commit`

If a line shows `✗` instead, do that one chore by hand
(`gt mail send <rig>/witness -s "MERGED <polecat-name>" …`,
`gt mail archive <id>`, `git branch -D temp`). Do NOT repeat a chore that
printed `✓` — a second MERGED is a duplicate mail.

**Expect a respawn.** When the rig sets `merge_queue.cycle_session_after_merge`,
post-merge closes this patrol cycle and respawns your session in place after
printing `🔄 unit complete — respawning …`. That is the designed end of the
unit, not a crash: your successor starts the next patrol cycle fresh. If you
see `session kept`, continue to loop-check as usual.

**VERIFICATION GATE**: You CANNOT proceed to loop-check without:
- [x] Post-merge completed (MR closed, source issue closed, branch deleted)
- [x] Its MERGED / MERGE_READY / temp lines are ✓, or you did the ✗ ones by hand
```

Keep the final line `Target branch has moved. Any remaining branches need
rebasing on new baseline.`

In batch-scan, replace the paragraph ending "It also runs the same post-merge
cleanup … You must do that yourself in Step 4." and the whole
`**Step 4: Notify Witness for every merged MR (REQUIRED)**` block with:

```
It also runs the same post-merge cleanup as a single-MR merge for every MR in
`.merged[]` — closes MR + source beads, deletes branches, nudges mayor,
sends MERGED to the witness for polecat branches (gt-9gjl), and archives each
MR's MERGE_READY (claude-7fc). Do NOT send MERGED yourself.

**Step 4: Expect a respawn**

When the rig sets `merge_queue.cycle_session_after_merge` and the batch
landed, `gt mq batch run` closes this patrol cycle and respawns your session
after writing its JSON. Your successor starts the next cycle fresh; the
single-MR path (Step 6) runs there.
```

In merged-pr-sweep, change the `PR_STATE=MERGED` bullet's fence to:

```
  gt mq post-merge <rig> <mr-id> --skip-branch-delete
  ```
  Post-merge sends MERGED and archives MERGE_READY itself (claude-7fc); verify
  its ✓ lines and do only a ✗ chore by hand.
```

That keeps `gt mq post-merge <rig> <mr-id> --skip-branch-delete` and the
`PR_STATE=CLOSED … do NOT send MERGED` text, both of which
`TestRefineryPatrolMergedPRSweepUsesAuthoritativeLookup` asserts.

No `version` bump: `version = 31` stays. The last three formula edits
(4b7fcbf, 16dd184, 15ac26b) did not bump it.

- [ ] **Step 4: Run the formula suite**

Run: `go test ./internal/formula/ -count=1`
Expected: `ok  	github.com/steveyegge/gastown/internal/formula`

This includes the new test, `TestRefineryPatrolMergePushRekeysNoteOntoLandedCommit`
(the re-key still precedes post-merge) and the merged-pr-sweep test.

- [ ] **Step 5: Commit**

```bash
git add internal/formula/formulas/mol-refinery-patrol.formula.toml internal/formula/refinery_unit_cycle_test.go
git commit -m "formula(refinery): post-merge owns the per-MR chores; expect a respawn

MERGED, MERGE_READY archiving, the landed-commit attestation and temp
deletion now happen inside gt mq post-merge / gt mq batch run. The agent
verifies the printed lines instead of repeating them, and is told that a
landed unit may end its session by design."
```

**Ordering note:** T9 and T10 ship together in MR-B. If T9 landed without
T10, a refinery on the old formula would send a second MERGED after
post-merge. That's harmless (see Facts, above) but noisy, so they are not
split across MRs.

### Task 11: MR-B quality gate, rebase onto landed MR-A, submit

**Files:** none changed.

**Interfaces:**
- Consumes: `$MRA_TIP` (from Task 7 Step 3) and branch `crew/sloan/claude-7fc-unit-cycle` with Tasks 8–10 committed. That branch was created from `$MRA_TIP` at the start of Task 8.
- Produces: `$MRB_ISSUE`, `$MRB_MR` and `$MRB_MERGE`.

- [ ] **Step 1: Wait for MR-A to land** (Task 7 Step 12 passed). MR-B builds on MR-A's post-merge call sites; don't submit it before then.

- [ ] **Step 2: Rebase MR-B onto main, dropping MR-A's original commits**

The refinery may have rebased MR-A, so replay only MR-B's own commits:
```bash
cd ~/gt/gastown/crew/sloan-7fc
git switch crew/sloan/claude-7fc-unit-cycle
git status --porcelain            # expect empty
git fetch origin
git rebase --onto origin/main "$MRA_TIP" crew/sloan/claude-7fc-unit-cycle
git log --oneline origin/main..HEAD
```
Expected: the log shows only the Task 8–10 commits. Conflicts are most likely in `internal/cmd/mq.go` and `internal/cmd/mq_batch.go`, where MR-A added the hook calls. Resolve them keeping MR-A's landed code plus MR-B's additions, then `git rebase --continue`.

- [ ] **Step 3: Gate** — the same commands as Task 7 Steps 4–6, with `/tmp/7fc-b-*.log` as the log names. Expected: `build=0`, `vet=0`, `lint=0`, `testmk=0`, `gotest=0`, and optionally `om=0`.

- [ ] **Step 4: Source issue, held out of dispatch**

```bash
cd ~/gt/gastown
MRB_ISSUE=$(bd create \
  --title="Refinery: fresh session per landed unit (post-merge completes the unit, then respawns the pane)" \
  --type=feature --priority=2 \
  --assignee=gastown/crew/sloan --status=in_progress \
  --labels=claude-7fc \
  --description="MR-B of docs/plans/2026-09-23-install-gt-after-merge-design.md. Off until merge_queue.cycle_session_after_merge is set. Crew-owned; do NOT sling." \
  --json | jq -r .id)
echo "$MRB_ISSUE"
```

- [ ] **Step 5: Push and submit**

```bash
cd ~/gt/gastown/crew/sloan-7fc
git push origin crew/sloan/claude-7fc-unit-cycle
git ls-remote origin refs/heads/crew/sloan/claude-7fc-unit-cycle
gt mq submit --branch crew/sloan/claude-7fc-unit-cycle --issue "$MRB_ISSUE" --no-cleanup
MRB_MR=<id from output>
MRB_TIP=$(git rev-parse HEAD)
cd ~/.claude && bd comments add claude-7fc "MR-B submitted: issue $MRB_ISSUE, MR $MRB_MR, tip $MRB_TIP"
```
Expected: the ls-remote SHA equals the local HEAD, then `✓ Submitted to merge queue`.

- [ ] **Step 6: Watch, rework on rejection, verify landing** — Task 7 Steps 10–12 with the MR-B variables. Record:
```bash
MRB_MERGE=$(git log --first-parent --format=%H -1 --grep="$MRB_ISSUE" origin/main)
cd ~/.claude && bd comments add claude-7fc "MR-B landed: $MRB_MERGE (issue $MRB_ISSUE)"
```

---

### Task 12: Operator steps: bootstrap, enable, acceptance, follow-ups

**Files:** `~/gt/gastown/config.json` (live rig config, not in git; backup beside it).

**Interfaces:**
- Consumes:
  - `$MRA_MERGE` and `$MRB_MERGE`;
  - Task 3's JSON tags `merge_queue.post_merge_command` and `merge_queue.post_merge_timeout`, a duration string such as `"20m"` in the same style as `batch_min_age`;
  - Task 9's tag `merge_queue.cycle_session_after_merge` (bool);
  - the receipt schema from Task 1: `ts`, `event`, `commit`, `prev_commit`, `source`, `merged_at`, `reason` and `duration_s`, with timestamps in UTC RFC3339;
  - Task 5's log line for an upgrade restart.
- Produces: the enabled pipeline, recorded acceptance evidence, and three follow-up beads.

- [ ] **Step 1: Check whether the old rebuild-gt already installed MR-A**

Until the bootstrap, the running rebuild-gt is the old script, and it can install MR-A on its own.
```bash
gt stale --json | jq '{binary_commit, stale, commits_behind}'
cd ~/gt/gastown/mayor/rig && git merge-base --is-ancestor "$MRA_MERGE" "$(gt stale --json | jq -r .binary_commit)" && echo "MR-A already installed"
```
If MR-A is already installed, the daemon has already restarted through the old kickstart path. Skip to Step 5 after confirming the daemon started after the install:
```bash
grep -a 'Daemon starting' ~/gt/daemon/daemon.log | tail -1
```

- [ ] **Step 2: Pick a quiet moment**

A bootstrap restart kills any in-flight script plugin and main-branch-test gate. Refinery gates are safe, because they run in the refinery's own tmux session.
```bash
gt slot status --json | jq -r '.. | objects | select(has("owner")) | .owner' 2>/dev/null
tail -20 ~/gt/daemon/daemon.log | grep -a -i 'plugin\|main_branch_test\|main-branch-test'
```
Proceed when no owner ends in `/main-branch-test` and the log tail shows no plugin that started but hasn't finished.

- [ ] **Step 3: Fast-forward the canonical build checkout**

A fast-forward of `mayor/rig` is pre-authorized when the tree is clean and nothing is ahead of origin.
```bash
cd ~/gt/gastown/mayor/rig
git status -sb | head -3
git fetch origin
git rev-list --count origin/main..HEAD     # expect 0
git merge --ff-only origin/main
git merge-base --is-ancestor "$MRA_MERGE" HEAD && echo has-MR-A
```
Expected: no tracked modifications other than `.beads/*` (a modified `.beads/config.yaml` is normal and allowed), `0` commits ahead, then `has-MR-A`. If `mayor/rig` is ahead or dirty outside `.beads/`, stop and nudge the mayor. Don't reset, clean or stash.

- [ ] **Step 4: Diff runtime plugins against the repo, then install**

`make install` runs `gt plugin sync`, which refuses (gt-o848l) rather than overwriting plugin edits made at runtime. Find any such edits first:
```bash
cd ~/gt/gastown/mayor/rig
for p in plugins/*/; do n=$(basename "$p"); [ -d ~/gt/plugins/$n ] && diff -rq "$p" ~/gt/plugins/$n; done
```
Expected: no output, or only differences you recognise as the repo being newer. A runtime-only edit must be reconciled into the repo before installing: stop and report it.

Install with `make install`. It is the one deploy path:
- the atomic replace (`install-binary.sh`);
- it restarts the daemon only if one is running, through `gt daemon restart` = `launchctl kickstart -k`, never stop then start;
- it removes shadow binaries;
- it syncs plugins.

`safe-install` doesn't fit this bootstrap. The old running daemon doesn't understand restart markers, so a restart is required anyway. Never `cp` a binary over `~/.local/bin/gt`: macOS codesign kills it.
```bash
cd ~/gt/gastown/mayor/rig
make install ; echo "install=$?"
gt formula sync ; echo "formula=$?"
```
Expected: `install=0`, `Installed gt to ...`, `Restarting daemon to pick up new binary...`, `Daemon restarted.`, and `formula=0`.

- [ ] **Step 5: Verify that the bootstrap is in force**

```bash
gt version
gt daemon status ; echo "daemon=$?"
launchctl print gui/$(id -u)/com.gastown.daemon | grep -E '^\s+(state|pid|last exit code)'
grep -a 'Daemon starting' ~/gt/daemon/daemon.log | tail -1
cd ~/.claude && bd comments add claude-7fc "Bootstrap: installed $(gt version | awk '{print $3}'), daemon restarted $(grep -a 'Daemon starting' ~/gt/daemon/daemon.log | tail -1 | cut -c1-19)"
```
Expected:
- `gt version` shows a commit at or after `$MRA_MERGE`;
- `daemon=0`;
- `state = running`;
- the `Daemon starting` time is after the install.

- [ ] **Step 6: Enable the post-merge install for gastown**

```bash
cd ~/gt/gastown
cp config.json "config.json.bak-$(date +%Y%m%d)-post-merge-cmd"
jq '.merge_queue.post_merge_command = "bash scripts/install-after-merge.sh"
    | .merge_queue.post_merge_timeout = "20m"' config.json > config.json.tmp \
  && jq -e '.merge_queue.post_merge_command' config.json.tmp >/dev/null \
  && mv config.json.tmp config.json
jq '.merge_queue | {post_merge_command, post_merge_timeout}' config.json
```
Expected:
```
{"post_merge_command":"bash scripts/install-after-merge.sh","post_merge_timeout":"20m"}
```
The command runs in `~/gt/gastown/refinery/rig` (Task 3), so `scripts/` resolves to the merged version of the repo. Note that the rig root `~/gt/gastown/scripts/` exists but holds only `om-gate.sh`, and isn't used.

No restart is needed: `gt mq post-merge` is a fresh `gt` process that reads the rig config on every call.

- [ ] **Step 7: Acceptance for MR-A, over the next 3 runtime merges**

After each merge lands, check the receipts. The window starts at the time Step 6 was done:
```bash
jq -s '
  def t: sub("\\.[0-9]+"; "") | fromdateiso8601;
  [ .[] | select(.event == "daemon_restarted") ] as $dr
  | [ .[] | select(.event == "installed" and .source == "post-merge") ][-3:]
  | map(. as $i | {
      commit: .commit[0:9],
      merged_at,
      install_lag_s: ((.ts | t) - (.merged_at | t)),
      daemon_lag_s: ([ $dr[] | select((.ts | t) >= ($i.ts | t)) ][0]
                     | if . then ((.ts | t) - ($i.merged_at | t)) else null end)
    })' ~/gt/daemon/install-receipts.jsonl
```
Pass when every row has `install_lag_s` ≤ 600 and `daemon_lag_s` non-null and ≤ 600. A null `daemon_lag_s` means the daemon hasn't gone idle yet; recheck after 30 min.

Other receipt events are information, not failures: `skipped` (a denylist-only diff), `noop` (already installed), and `refused` with `lock-busy`. A `failed` or `rolled_back` row fails acceptance: read its `reason`.

Check that no restart killed in-flight work. For each restart after Step 6:
```bash
grep -a -n 'Daemon starting' ~/gt/daemon/daemon.log | tail -5
grep -a -B60 'Daemon starting' ~/gt/daemon/daemon.log | tail -400 \
  | grep -a -iE 'signal: (killed|terminated)|context canceled|interrupted' \
  | grep -av 'patrol_watchdog: nudge'
```
Expected: the second command prints nothing. `patrol_watchdog: nudge … signal: killed` lines are routine nudge timeouts, not restart kills, so they are excluded. Each restart should also be preceded by Task 5's upgrade-restart log line, not by `Received signal terminated`.

```bash
gt escalate list 2>/dev/null | grep -E 'post-merge-command|install-gt|restart-pending-stuck|daemon-not-in-force'
```
Expected: no output.

Record the evidence:
```bash
cd ~/.claude && bd comments add claude-7fc "MR-A acceptance: <paste the 3 jq rows>; no restart kills; no install escalations"
```

- [ ] **Step 8: Enable the per-unit refinery cycle, after MR-B is installed automatically**

MR-B's landing merge is itself a runtime merge, so the pipeline from Step 6 installs it.
```bash
jq -c 'select(.event == "installed")' ~/gt/daemon/install-receipts.jsonl | tail -3
cd ~/gt/gastown/mayor/rig && git merge-base --is-ancestor "$MRB_MERGE" "$(gt stale --json | jq -r .binary_commit)" && echo has-MR-B
```
Expected: `has-MR-B`. Then:
```bash
cd ~/gt/gastown
cp config.json "config.json.bak-$(date +%Y%m%d)-cycle-session"
jq '.merge_queue.cycle_session_after_merge = true' config.json > config.json.tmp \
  && jq -e '.merge_queue.cycle_session_after_merge == true' config.json.tmp >/dev/null \
  && mv config.json.tmp config.json
jq '.merge_queue.cycle_session_after_merge' config.json
```
Expected: `true`.

- [ ] **Step 9: Watch the first respawn live, then acceptance for MR-B over 3 merges**

Before a merge, record the refinery pane pid:
```bash
tmux -L gt-3aa519 display -p -t gt-refinery '#{pane_pid}'
```
After the merge lands (`gt mq list gastown` shows it merged), run the same command again. Expected: a **different** pid within about a minute of the merge; the respawn skips the 2-min handoff cooldown. The pane then shows a fresh Claude session running `gt prime`:
```bash
tmux -L gt-3aa519 capture-pane -p -t gt-refinery | tail -15
```

Check for handoff mail since enabling:
```bash
gt mail inbox --identity gastown/refinery --all --json \
  | jq '[.[] | select((.subject // "") | test("HANDOFF"))] | length'
```
Expected: the same count as before Step 8. Per-unit respawns send no mail.

Check that the MERGED mail still reaches the witness, now from Go:
```bash
gt mail inbox --identity gastown/witness --all --json \
  | jq -r '.[] | select((.subject // "") | test("^MERGED")) | .subject' | tail -3
```
Expected: one `MERGED <polecat>` per landed polecat MR.

Pass when 3 consecutive merges each show a pid change, no new handoff mail, a MERGED mail to the witness, and the refinery picking up the next queued MR.
```bash
cd ~/.claude && bd comments add claude-7fc "MR-B acceptance: 3 merges, pids <a>-><b>-><c>-><d>, no handoff mail, MERGED delivered"
```

- [ ] **Step 10: File the follow-up beads, held out of dispatch**

They are P3, above seat-refill's P2 cap, and assigned to the crew. Never sling them.
```bash
cd ~/gt/gastown
bd create --title="Long-lived gt processes restart on a new binary (nudge-pollers, heartbeat-poller, dashboard)" \
  --type=task --priority=3 --assignee=gastown/crew/sloan --labels=claude-7fc,follow-up \
  --description="Out of scope in docs/plans/2026-09-23-install-gt-after-merge-design.md (decision 4). After an install, gt nudge-poller per session, gt deacon heartbeat-poller and gt dashboard keep executing the old binary until restarted. Design: detect a new installed binary (install receipt or binary mtime) and re-exec at a safe point."
bd create --title="Daemon crash-loop after upgrade: automatic rollback to gt.prev" \
  --type=task --priority=3 --assignee=gastown/crew/sloan --labels=claude-7fc,follow-up \
  --description="Out of scope in docs/plans/2026-09-23-install-gt-after-merge-design.md (decision 12). Today a daemon that crash-loops on a newly installed binary is only escalated by the rebuild-gt backstop (rebuild-gt:daemon-not-in-force). Design: count failed starts in a window at daemon startup; after N, restore ~/.local/bin/gt.prev atomically and escalate."
bd create --title="Deterministic Go driver for the refinery routine path (needs its own /super-plan)" \
  --type=task --priority=3 --assignee=gastown/crew/sloan --labels=claude-7fc,follow-up,needs-plan \
  --description="Merge data 2026-09-21..23: the refinery gate slot was held only 18% of the time; most of each cycle is LLM steps. Drive gate -> om review -> merge -> post-merge -> install in Go (gt mq batch run already does most of it), with the LLM called only for conflicts and judgment calls. Composes with the post-merge hook, which lives in Go post-merge."
```
Expected: three `✓ Created issue: gt-…` lines. Record their ids on `claude-7fc` with `bd comments add`.

- [ ] **Step 11: Close out**

When Steps 7 and 9 pass, record the final state on `claude-7fc`: merges, receipts and follow-up ids. Ask Sloan whether to close the handoff bead.
