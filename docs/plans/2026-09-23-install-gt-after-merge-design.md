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
  deferred release at `internal/cmd/slot.go:157`, `ReleaseWithExit` at :201).
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
  plugin, dogs included: they run under `d.ctx` (`exec.CommandContext` at
  `plugin_script.go:179`, context at :306). It also kills every
  main-branch-test gate, which runs inside the daemon (single-flight guard at
  `main_branch_test_runner.go:754-763`).
- **A clean daemon shutdown exits 0.** Every shutdown path returns
  `d.shutdown(state)`, which returns nil (`daemon.go:953`, :970), and
  `runDaemonRun` returns `d.Run()` (`internal/cmd/daemon.go:487`). launchd
  therefore leaves the daemon down after any shutdown it didn't cause.
  Refinery gates run in the refinery's own tmux session and survive. The
  refinery-respawn storm on restart (gt-uj9k) is fixed:
  `DecideSessionReconcile` keeps the session whenever the gate slot is busy
  (`internal/refinery/manager.go:540`).
- **Only the daemon keeps running old code.** Every other `gt` invocation
  execs the binary fresh from disk. launchd runs
  `~/.local/bin/gt daemon run` with `KeepAlive {Crashed: true, SuccessfulExit: false}`,
  so a daemon that exits nonzero is restarted on whatever binary is installed.
  A daemon that exits 0 is not restarted.
- **The daemon already tracks what it has in flight:**
  - the `scriptRunner` running map (`plugin_script.go:75-104`);
  - `mainBranchTestRunning` (`daemon.go:246`);
  - `compactorDogRunning`, a plain bool under `compactorDogMu`
    (`daemon.go:200`, `compactor_dog.go:168`);
  - `bootTriageInFlight`, `scheduledSlingsRunning`, `mayorDispatchRunning`
    and `patrolWatchdogRunning`.

  `pourDoctorMolecule` (`daemon.go:1316`) and the Dolt goroutines
  (`dolt.go:673`…) have no flag.

  `mainBranchTestRunning` stays true for up to 60 min while the run waits for
  a gate slot (`daemon.go:1043-1047`). An interrupted run is already treated as
  no verdict (gt-59yz, `main_branch_test_runner.go:765-770`).
- **The refinery respawns in place with `gt handoff`.** `gt handoff` calls
  `tmux respawn-pane -k` on its own pane from inside the agent's tool call; it
  deliberately does not kill its own processes first (`handoff.go:353-377`,
  issue #859). Handoffs are rate-limited: `MinHandoffCooldown` is 2 min, and
  the command sleeps until it has passed (`handoff.go:1856`,
  `constants.go:117`).
- **The refinery's per-MR chores after post-merge are LLM steps.** In the
  single-MR path, after `gt mq post-merge` the formula has the agent:
  - add the attestation comment;
  - send the MERGED mail to the witness;
  - archive the MERGE_READY mail;
  - delete the `temp` branch.

  These are at `mol-refinery-patrol.formula.toml:1352-1420`. The batch path
  already sends MERGED in Go (`Engineer.notifyWitnessMerged`,
  `engineer.go:1962`).
- **`loop-check` goes straight to the next MR while the queue has work**
  (formula :1424-1467). `burn-or-loop` is reached only once the queue drains.
  On 2026-09-23 the live gastown refinery went more than 2 hours without a
  patrol report while it merged MRs.
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
| 14 | Go `gt mq post-merge` completes the whole unit (it absorbs the chores from the formula) and then respawns the refinery pane in place, with no handoff mail. The trigger lives in Go, not in the formula. The batch path does the same once per batch. Idle cycles and rejects keep today's path. |
| 15 | All work happens in the claude-7fc session and lands as two refinery MRs. Nothing is slung to polecats. |

## Architecture

```
refinery merge (single MR: runMQPostMerge │ batch: once, after result.MergeCommit = tipSHA)
  └─▶ [Go] runPostMergeCommand(rig, mergedSHA)            shared by both paths
        reads rig config merge_queue.post_merge_command (empty = off),
              merge_queue.post_merge_timeout (default 20m)
        runs it in <rig>/refinery/rig (so the script is the MERGED version)
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
[Go daemon] heartbeat, BEFORE dispatching plugins: marker present and daemon idle
              → Run returns ErrRestartForUpgrade → runDaemonRun os.Exit(75) → launchd restarts it
            startup: marker.commit == own commit → clear marker, append "daemon_restarted" receipt
            marker older than 30m and never idle → escalate once
[rebuild-gt] threshold 1; after its deferral checks it delegates the WHOLE job
             (fetch, ff, build, install) to install-gt.sh --source rebuild-gt, so every
             build happens under the flock; kickstart removed; the backstop escalates if
             the marker is more than 30m old or the daemon is not on the installed commit
[Go, MR-B] end of the unit (post-merge, after the hook): the per-MR chores move from the
             formula into Go → close the patrol wisp and pour the next one
             → respawn-pane -k on its own pane (only when GT_ROLE=<rig>/refinery,
               in that refinery's tmux pane, and merge_queue.cycle_session_after_merge
               is set; skipped inside MinHandoffCooldown)
```

### Components

**`runPostMergeCommand` (Go, new).** A single function with two call sites:

- **Single MR:** called from `runMQPostMerge` next to
  `handlePostMergeRubricChange`, with `result.MR.MergeCommit`. If that is
  empty it falls back to `origin/<target>`. It does not use
  `branchCleanup.SubmittedHead`, which is the polecat's head, not the merge.
- **Batch:** called once per batch, after `result.MergeCommit = tipSHA`
  (`internal/refinery/batch.go:786`). It is not called from
  `HandleMRInfoSuccess`, which runs once per member (`batch.go:797`) and also
  inside `processSingleMR` (:548). It fires whenever the push landed, even if
  some members' cleanup failed.
- **Resumed merges:** `resumeLandedMerge` (`resume_landed_merge.go:69`) can
  hand it a merge commit that is already installed. `install-gt.sh` treats
  that as a no-op.
- **Orphan-branch merges:** `runMQPostMerge` returns on its orphan-branch path
  before the rubric hook (`mq.go:763-766`), so those merges never reach the
  hook. The rebuild-gt backstop installs them; that is accepted, not fixed.

Behaviour:

- If the rig config has no `post_merge_command`, it does nothing.
- Otherwise it runs the command with `bash -c` in `<rig>/refinery/rig`, inside
  its own process group, under `post_merge_timeout`. `r.Path` is the rig
  root, which has no `scripts/`, so it is not used.
- It streams the output to the refinery's log.
- If the command exits nonzero or times out, it escalates and returns nil. The
  merge result never changes.

No Go code knows which rig builds `gt`; that knowledge lives in gastown's rig
config.

**`merge_queue.post_merge_command` / `merge_queue.post_merge_timeout`
(config, new).** Two new fields on the existing `merge_queue` struct
(`MergeQueueConfig`, `internal/config/types.go:1384`). **`post_merge_command`
is read only from the rig's root `config.json`.** `MergeSettingsCommand`
excludes it from the repo and local settings overlays, so content merged into
the repo can never choose the command the refinery runs. The command is empty by default. The
timeout defaults to 20m: that covers the 5m lock wait plus a cold-cache build
with room to spare, and the refinery's shell tool allows 45m.

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

1. Take the flock, waiting up to 5 min. If it isn't free by then, exit 3
   (busy): the binary is untouched and a `refused` receipt is written with
   reason `lock-busy`.
2. If the installed commit is `<sha>` or a descendant of it, write a `noop`
   receipt and exit 0.
3. `git fetch origin`, then fast-forward `mayor/rig` to `<sha>`. With
   `--slot-role`, build inside `gt slot run --role <role>`. Refuse with exit 2 if the tree is
   dirty, the branch is wrong or local main has diverged.
4. Copy the current `gt` to `gt.prev`.
5. Run `make SKIP_UPDATE_CHECK=1 safe-install`.
6. Run the smoke test. `gt version` must resolve to `<sha>`, and
   `gt stale --json` must parse. If either check fails, roll back and exit 1.
7. Run `gt formula sync` and `gt plugin sync`. A failure here does not stop
   the install.
8. Write the restart marker.
9. Append an `installed` receipt.

**Exit codes:**

| Code | Meaning |
|---|---|
| 0 | installed, no-op or skipped |
| 1 | failed (build, smoke test or rollback) |
| 2 | refused (dirty, wrong branch, diverged) |
| 3 | lock busy |

It never restarts the daemon itself. The install directory, the `daemon/`
directory, and `make` and `gt` are resolved through overridable variables so
the tests can point them elsewhere.

**Daemon restart-pending handling (Go, new).** This lives in the daemon's
heartbeat and startup.

- **Idle** means all of the following:
  - the `scriptRunner` map is empty;
  - `compactorDogRunning` is false (read under `compactorDogMu`);
  - `bootTriageInFlight`, `scheduledSlingsRunning`, `mayorDispatchRunning`
    and `patrolWatchdogRunning` are false;
  - `mainBranchTestRunning` is false, or the main-branch test is still
    waiting for a gate slot. A new `mainBranchTestWaitingSlot` atomic is set
    around the slot wait. Killing a run that is waiting is safe, because an
    interrupted run already counts as no verdict (gt-59yz). Without this the
    daemon would seldom be idle while the refinery gates back to back.
  - `pourDoctorMolecule` and the Dolt goroutines are not counted: they are
    short or restartable.
- **The check runs at the top of the heartbeat, before the heartbeat
  dispatches plugins.** Otherwise each heartbeat would start a script and make
  itself busy.
- **Comparing a marker with the daemon's own build commit** uses git ancestry,
  checked in `mayor/rig` (`merge-base --is-ancestor`), not string equality:
  - **Newer** means the daemon's commit is a strict ancestor of the marker's
    commit.
  - **Covered** means the marker's commit equals the daemon's commit or is an
    ancestor of it.
- **At every heartbeat, not only at startup,** a covered marker is deleted and
  a `daemon_restarted` receipt is appended. That handles a race: the daemon
  exits for marker X, and launchd restarts it on binary Y before install-gt
  writes marker Y. Marker Y then equals the daemon's own commit and would never
  clear.
- **When the marker is present, its commit is newer than the daemon's own
  build commit, and the daemon is idle,** the daemon cancels its context,
  runs the normal shutdown, and returns a sentinel error,
  `ErrRestartForUpgrade`, from `Run`. `runDaemonRun`
  (`internal/cmd/daemon.go:487`) matches the error and calls `os.Exit(75)`.
  launchd starts it again on the new binary.
- **At startup** the same covered-marker check runs first.
- **A marker that has waited more than 30 min without the daemon going idle**
  escalates once and keeps waiting.
- **Exit code 75 is deliberate.** A clean exit 0 would leave the daemon down.

**rebuild-gt (changed).**

- `REBUILD_GT_INSTALL_THRESHOLD` defaults to 1.
- After its deferral checks, rebuild-gt hands the whole job to
  `install-gt.sh --sha <origin/main> --source rebuild-gt`: fetch,
  fast-forward, build, install and the rest. That covers its current fetch,
  fast-forward, `gt slot run … make build` and install tail (`run.sh:120`,
  :397-428, :664-792).
  - Every build and every write to `mayor/rig` then happens under the flock,
    so rebuild-gt and the hook can no longer build into one output at once.
  - It also removes rebuild-gt's `gt daemon restart`, so it no longer kills
    in-flight plugins, itself included.
- `install-gt.sh` takes an optional `--slot-role <role>`. rebuild-gt passes it
  to keep its existing rule of building inside a gate slot; the post-merge
  hook doesn't.
- rebuild-gt maps the script's exit codes onto its own daemon contract:

  | install-gt.sh exits | rebuild-gt exits |
  |---|---|
  | 0 | 0 |
  | 1 | 1 |
  | 2 (refused) | its own refusal: 0 with a skipped receipt |
  | 3 (lock busy) | 3, deferred: retry at the next heartbeat, write no receipt, don't start the cooldown |

  Exit 3 already means "deferred" to the daemon because rebuild-gt's
  `plugin.md` sets `allow_deferred_exit = true` (`plugin_script.go:40-64`,
  :133-135).
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

**Why the trigger can't live in the formula.** `loop-check` goes straight to
the next MR while the queue has work, so `burn-or-loop` is reached only when
the queue drains. And the live refinery skipped its end-of-cycle steps for
more than 2 hours. The only step it reliably runs per unit is
`gt mq post-merge`, because the MERGED mail waits on it. So the unit boundary
goes there, in Go.

**Change.** A new function, `completeUnitAndCycle` (Go), runs at the end of a
landed unit:

- **Single MR:** at the end of `runMQPostMerge`, after the install hook.
- **Batch:** once, after the batch's post-merge work and the install hook.

It does the following, in order:

1. **Do the per-MR chores that the formula leaves to the agent today:**
   - the attestation comment, when `--landed-commit` was used;
   - the MERGED mail to the witness, through the same builder as
     `Engineer.notifyWitnessMerged`, factored out so both paths share it;
   - archive the MR's MERGE_READY mail, found by MR bead ID or branch in the
     refinery inbox;
   - delete the local `temp` branch in `refinery/rig`.

   Each chore is best-effort and idempotent, and a failure is logged. The
   formula's Step 3 to Step 5 text changes to "post-merge did this; verify its
   ✓ lines". So an agent that runs an old formula and repeats a chore does no
   harm, except that a MERGED mail would go out twice. The witness already
   handles duplicate MERGED mail idempotently; the plan must verify this.
2. **Close the current patrol wisp and pour the next one,** through the same
   code path as `gt patrol report` (`patrol_report.go:48`). The summary is
   mechanical: MR IDs, merge commit, gate result.
3. **Respawn the pane** with the respawn part of `gt handoff`, factored out of
   `handoff.go:340-377`. That code sets remain-on-exit, then runs
   `respawn-pane -k` on its own pane. It sends no handoff mail; the refinery
   needs nothing from it, because queue state lives in MR beads and the
   formula says "nothing has to be remembered across calls" (:434). The fresh
   Claude runs `gt prime` and picks up the new patrol wisp.

**Guards.** Step 3 runs only when all of these hold:

- `GT_ROLE` is `<rig>/refinery`;
- the process is inside that refinery's tmux session (`TMUX_PANE` resolves
  to the refinery session);
- the rig config sets `merge_queue.cycle_session_after_merge: true`.

A human or crew member running `gt mq post-merge` by hand therefore never
kills their own pane. Within `MinHandoffCooldown` (2 min) of the last handoff,
step 3 is **skipped**, not slept on: the next unit cycles instead.

Step 1 (the chores) runs for any caller. Step 2 (the patrol wisp) is guarded
by role and pane like step 3, but not by the config flag, so a human running
post-merge never closes the live refinery's patrol wisp.
`cleanupMoleculeOnHandoff` is not called, because it would close the wisp step
2 just poured.

In batch mode:
- MERGED is skipped, because Go already sends it for each member;
- each member's MERGE_READY is archived;
- there is no attestation and no `temp` branch;
- the wisp is closed and the pane respawned once per batch.

Duplicate MERGED mail does no harm: the witness's `HandleMerged` is
idempotent (`internal/witness/handlers.go:435-477`).

**Batch path.** `gt mq batch run` runs in the background under the agent's
tool. Respawning the pane at its end kills the agent that is waiting on it,
but only after the batch's JSON result is written, and the successor needs
nothing from that result.

**What stays the same.** Rejected MRs and cycles that landed nothing keep
today's path, including the `await-event` idle loop and its context
thresholds. An idle session is cheap.

**Why this shape.**

- The tmux session name stays the singleton.
- The daemon keeps seeing an "already running" refinery, and the gt-uj9k
  reconcile rules are untouched.
- Respawning in place avoids the 3-minute heartbeat gap and the race over which
  refinery is the only one, both of which come with a real exit and respawn.
- The per-MR chores become deterministic as a side effect.

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

**Scope.** The formula is shared, but the respawn is opt-in per rig through
`merge_queue.cycle_session_after_merge`, starting with gastown. The chores
moving into Go apply to every rig.

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
| install-gt | lock wait times out | exit 3, binary untouched; rebuild-gt defers to the next heartbeat; the hook escalates | `post-merge-command:<rig>`, MEDIUM (hook only) |
| install-gt | `mayor/rig` dirty, on the wrong branch or diverged | refuses, exit 2, binary untouched; rebuild-gt retries after its cooldown | `install-gt:refused`, MEDIUM |
| install-gt | `<sha>` is already installed, or an ancestor of the installed commit | does nothing, exit 0 | none |
| install-gt | `make build` or `safe-install` fails | binary untouched, exit 1 | `install-gt:build-failed`, MEDIUM |
| install-gt | smoke test fails | `gt.prev` moved back into place atomically, exit 1, no marker | `install-gt:smoke-failed`, HIGH |
| install-gt | the rollback itself fails | exit 1 | `install-gt:rollback-failed`, CRITICAL |
| daemon | marker can't be read, or is older than the daemon's own commit | logs it, deletes the marker | none |
| daemon | marker present but the daemon not idle for 30 min | keeps waiting, escalates once | `daemon:restart-pending-stuck`, MEDIUM |
| completeUnitAndCycle | one chore fails (mail, archive, comment, temp branch) | logs it, runs the rest; the merge stands | `refinery-unit-chore:<rig>`, MEDIUM, only for a failed MERGED send (worktrees would pile up) |
| completeUnitAndCycle | closing or pouring the patrol wisp fails | logs it; still respawns, and the successor finds the old wisp as today | none (the patrol watchdog already covers stuck wisps) |
| completeUnitAndCycle | a guard fails, or inside the cooldown | no respawn; steps 1–2 done | none |
| completeUnitAndCycle | `respawn-pane` fails | logs it; the agent continues in the old session, as today | `refinery-respawn-failed:<rig>`, MEDIUM |
| rebuild-gt | daemon not on the installed commit 30 min after install (for example, a crash loop) | escalates | `rebuild-gt:daemon-not-in-force`, HIGH |

## Data

All files are under `~/gt/daemon/` unless stated otherwise. Every file is
written through a temp file and a rename.

- **`restart-pending.json`**: `{"commit": "<full sha>", "requested_at": "<UTC RFC3339>", "source": "post-merge|rebuild-gt", "repo": "<abs build checkout>"}`.
  - The daemon runs its ancestry checks in `repo`.
  - Before exiting, the daemon records `attempted_from`, its own commit. If
    it comes back on that same commit, the upgrade had no effect, so it
    escalates once (`daemon:restart-pending-no-effect`) instead of looping.
- **`state.json`** (existing) gains `"commit"`: the daemon's running build
  commit, written at startup and on every heartbeat save. It may be the short
  ldflags SHA, so readers compare by git ancestry, never by string equality.
  rebuild-gt's backstop reads it.
- **Dolt is untouched by an upgrade restart.** Today the live town doesn't
  manage Dolt through the daemon; launchd runs `gt dolt start`. The shutdown
  for `ErrRestartForUpgrade` skips stopping a daemon-managed Dolt anyway, and
  the new daemon adopts the running server (`dolt.go:375-409`).
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

- The idle predicate, as a table test over the in-flight flags. It includes
  a main-branch test that is waiting for a slot, which counts as idle.
- A covered marker (equal to the daemon's commit or an ancestor of it) is
  cleared and writes a receipt, both at startup and at a heartbeat. This is
  the race from item 8.
- A newer marker while busy: no exit.
- A newer marker while idle: `Run` returns `ErrRestartForUpgrade`, and
  `runDaemonRun` maps it to 75. The exit function is injected, so no test
  process exits.
- The idle check runs before plugin dispatch within one heartbeat.
- A marker stuck for 30 min escalates exactly once.

**Shell: `scripts/install_gt_test.sh`.** Temporary install and `daemon/`
directories, with stub `make` and `gt` on `PATH`. Cases:

- The smoke test fails: `gt.prev` is restored, an escalation is raised and no
  marker is written.
- The commit is already installed, or is an ancestor of the installed commit:
  nothing happens.
- The lock is held and the wait times out: exit 3 and a `lock-busy` receipt.
  The wait is shortened through an env override.
- Each refusal case.
- The marker is written only after the smoke test passes.
- Receipts have the expected shape.

**Shell: `install-after-merge` denylist.**

- Only `_test.go` or `.md` files changed: skip.
- Any `.go` file changed: install.
- An unknown path changed: install.
- The installed commit can't be read: install.

**Go: `completeUnitAndCycle`.** Tests inject a fake mailer, a fake patrol
closer and a fake respawner.

- **The chores:**
  - MERGED is sent once, with the same body as the batch path;
  - the MERGE_READY mail is archived;
  - the attestation comment is added only when `--landed-commit` was used;
  - a failure in one chore doesn't stop the others.
- **The patrol step** closes the current wisp and pours the next one.
- **The respawn guards:** no respawn when the role is wrong, when not inside
  the refinery pane, when the config flag is off, or within the cooldown. In
  every one of those cases step 1 still runs. Step 2 runs only with a
  matching role and pane.
- **The call sites:** the batch path calls it once per batch, not once per
  member; the single-MR path calls it after the install hook.
- **The respawn helper factored out of `handoff.go`:** existing handoff tests
  stay green, and plain `gt handoff` still sends its mail.

**Formula: `mol-refinery-patrol`.** The existing tests over embedded formulas
must still parse it. The merge-push Step 3 to Step 5 text now says to verify
post-merge's ✓ lines instead of doing the chores. Assert that it no longer
tells the agent to send MERGED itself.

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
- **MR-B: fresh refinery session per unit.**
  - `completeUnitAndCycle`;
  - the shared MERGED builder and the respawn helper, factored out;
  - the `merge_queue.cycle_session_after_merge` config field;
  - the formula text change.

  It lands after MR-A, because it builds on MR-A's post-merge call sites. It
  is off until the config flag is set. Its formula change can go live before
  the flag, because post-merge does the chores in both cases.

The operator steps follow, in order:

1. **One manual bootstrap install**, after MR-A merges: `make safe-install`,
   then `gt formula sync`, `gt plugin sync` and `gt daemon restart`. After
   this the running daemon understands markers.
2. **Set `merge_queue.post_merge_command`** to `scripts/install-after-merge.sh`
   in gastown's rig config.
3. **After MR-B is installed** (automatically, by then), set
   `merge_queue.cycle_session_after_merge: true` for gastown. Watch the first
   respawn live. Other rigs opt in later.
4. **Acceptance.** Over the next 3 runtime merges:
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
