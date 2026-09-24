> Status: design approved (2026-09-23). Implementation plan: `2026-09-23-install-gt-after-merge-plan.md`. Tracked in claude-7fc.

# Install gt after each refinery merge, with a fresh refinery session per unit — design

Date: 2026-09-23. Tracking: claude-7fc (handoff bead); gastown rig beads filed from the plan.

## Problem

A merged gastown runtime fix does nothing until the installed `gt` binary is
rebuilt from main. The only installer is the `rebuild-gt` daemon script plugin,
and two of its gates keep it from running:

1. **Gate-busy deferral.** It exits 3 whenever any gate-class slot holder is
   active (`plugins/rebuild-gt/run.sh:543-548`). The match is town-wide, so an
   hm or om refinery gate blocks it as well. The gastown refinery runs gates
   back to back (5.4–6 min each, gaps of 40 s–4.5 min), and rebuild-gt only
   retries on the 3-minute heartbeat, so it rarely lands in a gap. On
   2026-09-23 an install at 18:25 was 8 commits behind by 19:35. The operator
   had to hold slings and pause the refinery by hand to get 59d21ec installed
   at 19:47, and did the same again at 19:57.
2. **Install threshold.** `REBUILD_GT_INSTALL_THRESHOLD` defaults to 5
   commits (`run.sh:299`). Merge commits count toward it
   (`internal/version/stale.go:213`), and there is no priority override, so a
   single urgent fix is never due on its own.

**Goal:** a merged runtime change is installed (`gt version` == the merged
commit) within 10 minutes of landing, with no operator action. The install
must not kill any in-flight gate or om review, and there must be a rollback
path.

## Facts this design rests on

All cited from `origin/main` at ca65b4a.

- **The refinery holds no slot at post-merge time.** It holds a gate slot
  only while its test command runs (`gt slot run`, released on exit,
  `internal/cmd/slot.go:130`).
- **The single-MR merge path already ends in deterministic Go:**
  `runMQPostMerge` (`internal/cmd/mq.go:746`). That function already has a
  best-effort hook that never fails the merge, `handlePostMergeRubricChange`
  (`mq.go:768`).
- **The batch path doesn't go through that function.** It lands via
  `Engineer.HandleMRInfoSuccess` (`internal/refinery/engineer.go:1841`), which
  has its own copy of the rubric hook.
- **Post-merge has a long timeout.** The gastown refinery is Claude Code
  running deepseek-flash, with `BASH_DEFAULT_TIMEOUT_MS=2700000`, so a
  foreground `gt mq post-merge` gets 45 minutes. `gt mq batch run` runs with
  `run_in_background` and has no timeout at all.
- **`make safe-install` does not restart the daemon** (Makefile:170-184). It
  installs atomically through `scripts/install-binary.sh` (mktemp + `mv`) and
  keeps no backup. rebuild-gt restarts the daemon itself, with
  `gt daemon restart` (`launchctl kickstart -k`) as its last step.
- **A daemon restart kills work in flight.** It kills every in-flight script
  plugin, dogs included (`plugin_script.go:199-203`), and every main-branch-test
  gate, which runs inside the daemon (`main_branch_test_runner.go:1048`).
  Refinery gates run in the refinery's own tmux session and survive. The
  refinery-respawn storm on restart (gt-uj9k) is fixed:
  `DecideSessionReconcile` keeps the session whenever the gate slot is busy
  (`internal/refinery/manager.go:540`).
- **Only the daemon keeps running old code.** Every other `gt` invocation
  execs the binary fresh from disk. launchd runs
  `~/.local/bin/gt daemon run` with `KeepAlive {Crashed: true, SuccessfulExit: false}`,
  so a daemon that exits nonzero is restarted on whatever binary is installed.
  A daemon that exits 0 is not restarted.
- **The daemon already tracks what it has in flight:** the `scriptRunner`
  running map (`plugin_script.go:75-104`), `mainBranchTestRunning`
  (`daemon.go:240`), `bootTriageInFlight`, `scheduledSlingsRunning`,
  `mayorDispatchRunning` and `patrolWatchdogRunning`.
- **`gt plugin sync` no longer clobbers edits made at runtime.** It refuses
  instead of overwriting (gt-o848l).

## Measured cost

Measured on 2026-09-23 in a worktree at ca65b4a. The machine was under load
(load average 15–27) and the gastown refinery, om-review and a polecat all
held gate slots during the runs.

- **The three `go build`s:**
  - 9.5–9.9 s with a warm cache;
  - 11.2–13.2 s after a one-line edit in `internal/daemon` or `internal/cmd`;
  - 14.6 s with the cache as found.
- **The rest of the install tail:**
  - `cp` to `gt.prev`: 0.06 s
  - `gt version`: 0.07 s
  - `gt stale --json`: 1.0 s
  - formula and plugin sync, as dry runs: 1.2 s
  - git fast-forward, `mv` and receipt: about 1–2 s
- **Total:** about 15–20 s per runtime merge. Against a gate of about 6 min,
  that is a 4–5% slowdown, and only on runtime merges.
- **Not measured:** a cold build cache, for example after a `go.mod` change.

**Merge history, 2026-09-21 to 09-23** (first-parent `origin/main`;
rebuild-gt receipts; `.events.jsonl`):

- **Mix:** 94% of the 225 merges touch runtime paths, so the denylist rarely
  skips and the install cost applies to almost every merge. None of them was
  a multi-MR batch merge; each brought in 2–11 commits.
- **Rate:**
  - on average 4.1 merges an hour, peaking at 11 an hour;
  - median gap between merges 10.7 min, p90 35 min.
- **Queue pressure:**
  - the next MR was already waiting when at least 61% of merges landed;
  - median time from done to merge was 23 min (p90 105 min);
  - the refinery's gate slot was held only 18% of the time.

  Most of each cycle happens outside the gate, in the agent's own steps.
  Against that, 15–20 s of install is small. This is also the strongest
  argument for the deterministic Go driver, which is out of scope here.
- **Staleness today:** merge to install takes a median of 53 min (p90
  240 min, max 489 min). Only 6% of runtime merges were installed within
  10 min. The installed binary ran, time-weighted, 4.6 runtime merges behind
  main.

## Decisions

| # | Decision |
|---|---|
| 1 | The install runs synchronously inside the merge's post-merge step, in deterministic Go, on both the single-MR and batch paths. It never fails the merge. |
| 2 | The trigger is a denylist. Skip only when every changed path is `*_test.go`, `*.md`, `docs/**` or `.beads/**`. Unknown paths install. |
| 3 | Keep one previous binary (`gt.prev`). Run a smoke test after install; if it fails, roll back automatically and escalate. |
| 4 | Long-lived `gt` processes (nudge-pollers, heartbeat-poller, dashboard) and loaded formula text are out of scope; they get a follow-up bead. |
| 5 | Every install writes a receipt that records the latency from merge to install. |
| 6 | The daemon picks up the new binary by exiting nonzero when it is idle, never by being killed. |
| 7 | A generic rig-config hook (`merge_queue.post_merge_command`) points at a script in the gastown repo. rebuild-gt and the hook share one install script. |
| 8 | The build runs in `gastown/mayor/rig`, fast-forwarded to the merged commit, under rebuild-gt's existing refusal rules. |
| 9 | Installs hold a shared flock and wait up to 5 min for it. They do not wait for a gate slot. |
| 10 | rebuild-gt stays as the backstop: threshold 1, the shared install script, and the restart marker instead of kickstart. |
| 11 | A batch installs once, at the batch's final merged commit. |
| 12 | A daemon crash loop gets escalation only, from the rebuild-gt backstop. Automatic rollback of a crash-looping daemon is a follow-up bead. |
| 13 | The rollback path is proven by shell tests against temporary directories. There is no live drill with a broken binary. |
| 14 | After every landed unit (one MR or one batch) the refinery hands off to a fresh session in the same tmux session (`respawn-pane`), sending no handoff mail. Idle cycles keep today's path. |
| 15 | All work happens in the claude-7fc session and lands as two refinery MRs. Nothing is slung to polecats. |

## Architecture

```
refinery merge (single MR: gt mq post-merge │ batch: HandleMRInfoSuccess, once per batch)
  └─▶ [Go] runPostMergeCommand(rig, mergedSHA)            shared by both paths
        reads rig config merge_queue.post_merge_command (empty = off),
              merge_queue.post_merge_timeout (default 10m)
        runs it in the refinery worktree (so the script is the MERGED version)
        env: GT_MERGED_SHA, GT_RIG, GT_TOWN_ROOT, GT_MR_IDS
        nonzero / timeout → log + gt escalate; never fails the merge
  └─▶ [repo] scripts/install-after-merge.sh
        denylist over <installed binary commit>..GT_MERGED_SHA
        all denylisted → "skipped" receipt, exit 0
        else → scripts/install-gt.sh --sha $GT_MERGED_SHA --source post-merge
  └─▶ [repo] scripts/install-gt.sh                        shared with rebuild-gt
        flock daemon/install-gt.lock (wait ≤ 5m)
        → ff mayor/rig to sha (existing refusal rules)
        → cp gt gt.prev → make safe-install → smoke
              (fail: mv gt.prev back, escalate, exit 1)
        → gt formula sync, gt plugin sync
        → write daemon/restart-pending.json → append daemon/install-receipts.jsonl
[Go daemon] heartbeat: marker present and daemon idle → exit(75) → launchd restarts it on the new binary
            startup: marker.commit == own commit → clear marker, append "daemon_restarted" receipt
            marker older than 30m and never idle → escalate once
[rebuild-gt] threshold 1; its install tail becomes install-gt.sh --source rebuild-gt;
             kickstart removed; the backstop escalates if the marker is more than 30m old
             or the daemon is not on the installed commit
```

### Components

**`runPostMergeCommand` (Go, new).** A single function, called from
`runMQPostMerge` next to `handlePostMergeRubricChange`, and from the batch
path once per batch with the final merged commit.

- If the rig config has no `post_merge_command`, it does nothing.
- Otherwise it runs the command with `bash -c` in the refinery worktree,
  inside its own process group, under `post_merge_timeout`.
- It streams the output to the refinery's log.
- If the command exits nonzero or times out, it escalates and returns nil. The
  merge result never changes.

No Go code knows which rig builds `gt`; that knowledge lives in gastown's rig
config.

**`merge_queue.post_merge_command` / `merge_queue.post_merge_timeout`
(config, new).** Two new fields on the existing `merge_queue` struct
(`internal/config/types.go:744`). Both are empty by default.

**`scripts/install-after-merge.sh` (new).**

- It reads the installed binary's commit from `gt stale --json` and lists the
  paths changed between that commit and `GT_MERGED_SHA`.
- If every path matches the denylist, it writes a `skipped` receipt and exits 0.
- If the installed commit can't be read, it installs anyway.
- Otherwise it `exec`s `install-gt.sh`.

The diff starts at the installed commit, not at this merge's parent, so a
docs-only merge can't hide an earlier runtime install that failed.

**`scripts/install-gt.sh` (new, shared).** Arguments: `--sha <commit>` and
`--source post-merge|rebuild-gt`. The steps, in order:

1. Take the flock.
2. If the installed commit is `<sha>` or a descendant of it, write a `noop`
   receipt and exit 0.
3. Fast-forward `mayor/rig` to `<sha>`. Refuse with exit 2 if the tree is
   dirty, the branch is wrong or local main has diverged.
4. Copy the current `gt` to `gt.prev`.
5. Run `make SKIP_UPDATE_CHECK=1 safe-install`.
6. Run the smoke test. `gt version` must resolve to `<sha>`, and
   `gt stale --json` must parse. If either check fails, roll back and exit 1.
7. Run `gt formula sync` and `gt plugin sync`. A failure here does not stop
   the install.
8. Write the restart marker.
9. Append an `installed` receipt.

It never restarts the daemon itself. The install directory, the `daemon/`
directory, and `make` and `gt` are resolved through overridable variables so
the tests can point them elsewhere.

**Daemon restart-pending handling (Go, new).** This lives in the daemon's
heartbeat and startup.

- **Idle** means the `scriptRunner` map is empty and every in-flight flag
  listed in "Facts this design rests on" is false.
- **At each heartbeat,** if the marker is present, its commit is newer than the
  daemon's own build commit, and the daemon is idle, the daemon shuts down
  cleanly and exits 75. launchd starts it again on the new binary.
- **At startup,** a marker whose commit equals the daemon's own commit is
  deleted and a `daemon_restarted` receipt is appended. A marker older than
  the daemon's own commit is deleted.
- **A marker that has waited more than 30 min without the daemon going idle**
  escalates once and keeps waiting.
- **Exit code 75 is deliberate.** A clean exit 0 would leave the daemon down.

**rebuild-gt (changed).**

- `REBUILD_GT_INSTALL_THRESHOLD` defaults to 1.
- The install tail after its build (`run.sh:689-792`) becomes
  `install-gt.sh --sha <pinned HEAD> --source rebuild-gt`. That removes its
  `gt daemon restart`, so it no longer kills in-flight plugins, itself
  included. rebuild-gt maps the script's exit codes onto its own daemon
  contract: 0 stays 0, a refusal (2) becomes rebuild-gt's own refusal (exit 0
  with a skipped receipt), and a failure (1) stays 1.
- New backstop check: if the marker is more than 30 min old, or the daemon's
  running commit differs from the installed binary's commit for more than
  30 min, it escalates.
- All its existing deferral rules stay.

## Refinery: a fresh session per unit of work

Today the refinery runs as one long patrol loop. Its formula's
`context-check` and `burn-or-loop` steps leave the decision to hand off to
the LLM's judgement (`mol-refinery-patrol.formula.toml:1665`, :1752; the
thresholds are 70% context, 1 GB RSS or 8 h). In practice it rarely hands
off: the gastown refinery ran as one turn for 144 minutes and 231 tool calls
on 2026-09-23. A long session costs in four ways:

- one-off instructions get lost in a turn full of gate output;
- compaction loses queue context mid-flow;
- a nudge can interrupt a gate;
- DeepSeek context grows and cache misses cost more.

**Change.** After a patrol cycle that landed a unit of work, the refinery
always hands off. A unit is one MR, or one batch; batching stays intact
(`batch_min_count` / `batch_min_age`). The steps are:

1. `gt patrol report`, which closes the wisp and creates the next one.
2. `gt handoff --no-mail`, which runs `tmux respawn-pane -k` in the same tmux
   session. The fresh Claude runs `gt prime` and picks up the hooked patrol
   wisp.

Cycles that landed nothing keep today's `await-event` idle path and its
existing health thresholds. An idle session is cheap.

**Why this shape.** It changes one formula file and adds one flag. The tmux
session name stays the singleton, the daemon keeps seeing an "already
running" refinery, and the gt-uj9k reconcile rules are untouched. Respawning
in place avoids the 3-minute heartbeat gap and the race over which refinery
is the only one, both of which come with real exit-and-respawn.

**`gt handoff --no-mail` (new flag).** Today every handoff mails the agent
itself (`internal/cmd/handoff.go:303`), and each mail is a permanent bead plus
a Dolt commit. The refinery needs nothing from that mail: queue state lives in
MR beads, pre-existing failures are found with `bd search`, and the formula
says "nothing has to be remembered across calls" (:434). So `--no-mail`
skips `sendHandoffMail` and does only the respawn. Without it, one mail per MR
would be clutter.

**What it gains beyond the refinery's own problems:**

- Each unit starts with the formula text most recently synced by the install
  step, which covers the refinery's share of decision 4.
- One-off instructions sent to the refinery reach a fresh session within one
  MR.

**Cost:**

- `gt prime` takes 6.8 s and produces 31.7 KB.
- Claude boot adds more; the estimate is about 20–40 s per unit, against a
  roughly 6-minute gate.
- Each fresh session starts with a cold DeepSeek prompt cache, where a long
  session instead re-reads an ever larger context.

**Scope.** The formula is shared, so this applies to every rig's refinery.

**Out of scope.** A deterministic Go driver for the routine path, with the LLM
used only for conflicts and judgement calls. It is size L and gets its own
/super-plan. `gt mq batch run` already does most of it in Go.

## Error handling

No failure in any row below changes the result of a merge.

| Where | Failure | Result | Escalation (fingerprint, severity) |
|---|---|---|---|
| Go hook | no `post_merge_command` | does nothing | none |
| Go hook | nonzero exit, or timeout (process group killed) | logs it; the merge stands | `post-merge-command:<rig>`, MEDIUM |
| install-after-merge | installed commit can't be read | installs anyway | none |
| install-gt | lock wait times out; `mayor/rig` dirty, on the wrong branch or diverged | refuses, exit 2, binary untouched; rebuild-gt retries later | `install-gt:refused`, MEDIUM |
| install-gt | `<sha>` is already installed, or an ancestor of the installed commit | does nothing, exit 0 | none |
| install-gt | `make build` or `safe-install` fails | binary untouched, exit 1 | `install-gt:build-failed`, MEDIUM |
| install-gt | smoke test fails | `gt.prev` moved back into place atomically, exit 1, no marker | `install-gt:smoke-failed`, HIGH |
| install-gt | the rollback itself fails | exit 1 | `install-gt:rollback-failed`, CRITICAL |
| daemon | marker can't be read, or is older than the daemon's own commit | logs it, deletes the marker | none |
| daemon | marker present but the daemon not idle for 30 min | keeps waiting, escalates once | `daemon:restart-pending-stuck`, MEDIUM |
| rebuild-gt | daemon not on the installed commit 30 min after install (for example, a crash loop) | escalates | `rebuild-gt:daemon-not-in-force`, HIGH |

## Data

All files are under `~/gt/daemon/` unless stated otherwise. Every file is
written through a temp file and a rename.

- **`restart-pending.json`**: `{"commit": "<full sha>", "requested_at": "<UTC RFC3339>", "source": "post-merge|rebuild-gt"}`.
  The last writer wins. It is written only after the smoke test passes.
- **`install-receipts.jsonl`**: one JSON object per event, with these fields:
  - `ts`
  - `event`: one of `installed`, `skipped`, `noop`, `refused`, `failed`,
    `rolled_back`, `daemon_restarted`
  - `commit`, `prev_commit`, `source`
  - `merged_at`: the committer time of `commit`
  - `reason`, `duration_s`

  The two latencies are `installed.ts − merged_at` (merge to install) and
  `daemon_restarted.ts − merged_at` (merge to daemon). At roughly 200 bytes a
  line the file needs no rotation.
- **`install-gt.lock`**: the flock file.
- **`~/.local/bin/gt.prev`**: the one previous binary.

## Testing

No real builds and nothing that touches the live town. The `internal/cmd`
gate already runs close to its 10-minute budget, so every test must stay fast.

**Go: `runPostMergeCommand`.**

- No command configured: does nothing.
- The env vars reach the script.
- A nonzero exit calls the escalator, which is injected, and returns nil.
- A timeout kills the whole process group.
- A batch invokes the hook once, with the final commit.

**Go: daemon restart-pending handling.**

- The idle predicate, as a table test over the in-flight flags.
- A marker older than the daemon's commit is deleted.
- A marker matching the daemon's commit at startup is cleared and writes a
  receipt.
- A marker stuck for 30 min escalates exactly once.
- The exit function is injected, so no test process exits.

**Shell: `scripts/install_gt_test.sh`.** Temporary install and `daemon/`
directories, with stub `make` and `gt` on `PATH`. Cases:

- The smoke test fails: `gt.prev` is restored, an escalation is raised and no
  marker is written.
- The commit is already installed, or is an ancestor of the installed commit:
  nothing happens.
- The lock is held and the wait times out.
- Each refusal case.
- The marker is written only after the smoke test passes.
- Receipts have the expected shape.

**Shell: `install-after-merge` denylist.**

- Only `_test.go` or `.md` files changed: skip.
- Any `.go` file changed: install.
- An unknown path changed: install.
- The installed commit can't be read: install.

**Go: `gt handoff --no-mail`.**

- `sendHandoffMail` is never called.
- The respawn path still runs, checked through dry-run output or an injected
  respawner.
- Without the flag, behaviour is unchanged.

**Formula: `mol-refinery-patrol`.** The existing tests over embedded formulas
must still parse it. Add an assertion that `burn-or-loop` requires
`gt handoff --no-mail` after a cycle that landed work.

**Shell: rebuild-gt `run_test.sh`.**

- The threshold is 1.
- The install goes through `install-gt.sh`.
- There is no kickstart.
- The backstop escalation fires.

## Rollout

The work is implemented in the claude-7fc session on the
`claude-7fc/install-gt-after-merge` worktree branch. Nothing is slung to
polecats. It lands through the gastown refinery as two MRs:

- **MR-A: install pipeline.**
  - the scripts and their tests;
  - daemon restart-pending handling;
  - the Go hook and the config fields;
  - the rebuild-gt switch.

  It is safe to land as one MR because nothing turns on by itself:
  - the hook stays off until a rig sets `post_merge_command`;
  - the running old daemon ignores markers;
  - rebuild-gt's new tail takes effect only after a plugin sync.
- **MR-B: fresh refinery session per unit.** The `gt handoff --no-mail` flag
  and the formula change. It is independent of MR-A and can land in either
  order.

The operator steps follow, in order:

1. **One manual bootstrap install**, after MR-A merges: `make safe-install`,
   then `gt formula sync`, `gt plugin sync` and `gt daemon restart`. After
   this the running daemon understands markers.
2. **Set `merge_queue.post_merge_command`** to `scripts/install-after-merge.sh`
   in gastown's rig config.
3. **Acceptance.** Over the next 3 runtime merges:
   - the receipts show `installed` and `daemon_restarted` within 10 min of
     `merged_at`;
   - daemon.log shows no plugin or main-branch-test gate killed by a restart;
   - after MR-B, each merge is followed by a refinery respawn with no handoff
     mail.

   The shell tests cover the rollback path.

## Out of scope (follow-up beads)

- Long-lived `gt` processes (nudge-pollers, heartbeat-poller, dashboard)
  noticing a new binary and restarting themselves.
- Automatic rollback when a crash-looping daemon was started on a new binary.
- A deterministic Go driver for the refinery's routine path. It needs its own
  /super-plan. The post-merge hook lives in Go post-merge, so it keeps working
  whatever drives the refinery.
