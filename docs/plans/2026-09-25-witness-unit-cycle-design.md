> Status: step 3 implemented (2026-09-25), claude-8w7. Boundary approved by
> Sloan: idle-cap timeout after 3 cycles, backstop at 8, flag off by default.

# Witness: respawn on a unit boundary instead of one long compacting session — design

Date: 2026-09-25. Tracking: claude-8w7 (handoff bead, ~/.claude DB). The
refinery version of this change already landed:
`internal/cmd/unit_cycle.go`, gated by `merge_queue.cycle_session_after_merge`
(design: `2026-09-23-install-gt-after-merge-design.md`, section "Refinery: a
fresh session per unit of work").

Steps 1 and 3 are implemented: step 1 persists the stall sample, and step 3
respawns the witness at the approved boundary (see "Step 3: as implemented").
Step 5, the measurement, is still to do.

## Problem

The gastown witness is one long Claude session that loops through patrol,
report and await-signal, and compacts when its context fills. Measured on
2026-09-24 (session 2ae1113c, deepseek-flash):

- 10 compactions since 09:12, about one an hour;
- context runs from about 50k tokens after a compaction to about 167k before
  the next; the median call carries about 100k;
- a quiet (abbreviated) cycle is about 5 calls, so 250–800k input tokens,
  mostly cache reads;
- a long context carries stale beliefs from cycle to cycle, the bug class the
  refinery rework removed.

## What must survive a respawn

The formula already says state is discovered each cycle from reality. The
audit on the bead found that the idle/backoff counter (agent bead `idle:N`),
lessons (`gt remember`), findings (bead comments) and handoffs (mail) already
live outside the session. The one cross-cycle memory kept only in context was
**sample 1 of the stall rule** (gt-xb27): a restart needs two observations at
least 30 minutes apart showing that the transcript did not advance and that
the pane signature is byte-identical. A respawn between the two samples used
to lose sample 1, and stall detection then never fires. Step 1 persists it.

Ad-hoc watches the witness invents for itself (for example a container
sampler) are still lost on respawn. That is acceptable; a watch that must
survive belongs on a bead.

## Step 1: persist the stall sample (implemented in this pass)

### Where: a state file under the witness directory, not the agent bead

The sample lives in `<rig>/witness/stall_samples.json` (the directory
`witness.WitnessStateDir` resolves, so `witness/rig/` for legacy layouts).

Reasons for a file over an agent bead field:

- **Write rate.** The scan records or re-records a sample for every live
  polecat on every cycle. On a bead each write is a Dolt commit, which is
  the load the Dolt runbook warns about, and it would add bd latency (seconds
  when Dolt is slow) to every patrol.
- **Failure direction.** A file that cannot be read leaves the scan with no
  sample 1, which only delays a verdict by one window. A bead write that hangs
  on Dolt stalls the scan itself.
- **Precedent.** The composer-stall clock (`tmux.PendingInputClock`, gt-afa7)
  already keeps per-session stall evidence in JSON files with atomic renames.
- **Reaper safety.** Agent beads have been auto-closed by the reaper
  (gt-2qzr); a file under the witness directory is not in its path.
- **Not `state.json`.** The witness writes `state.json` by hand, and it has
  been found as invalid JSON and as a prose scratchpad. Go owns
  `stall_samples.json` and the agent never writes it.

A file is local to the host, which is fine: the witness and its polecats run
on one host.

### Schema

```json
{
  "version": 1,
  "samples": {
    "opal": {
      "polecat": "opal",
      "session": "gt-gastown-opal",
      "sampled_at": "2026-09-25T14:00:00Z",
      "last_observed_at": "2026-09-25T14:25:00Z",
      "transcript_path": "/…/opal.jsonl",
      "transcript_mtime": "2026-09-25T13:58:12Z",
      "transcript_bytes": 812345,
      "pane_signature": "3f9c…"
    }
  }
}
```

The fields the bead asked for are `polecat`, `sampled_at`, `transcript_mtime`
and `pane_signature`. Four more are there because `witness.AssessStall`
needs them or because they guard identity or the clock:

- `transcript_bytes`: `AssessStall` treats a size change as progress, since a
  same-second write can keep the mtime.
- `session` and `transcript_path`: a sample from another tmux session or
  another transcript (a restarted polecat, a reused name) is never compared.
  The scan records a fresh sample 1 instead.
- `last_observed_at`: the latest scan that saw the polecat, updated even when
  sample 1 is kept. The scan-gap guard below reads it.

### Write and read rules

- **Atomic write under a lock.** Write to a temp file and rename it
  (`atomicfile.EnsureDirAndWriteJSON`), so a reader never sees a half-written
  file. Load, compute and save run under an flock on a sibling
  `stall_samples.json.lock`. A scan that cannot take the lock within 10 s
  still judges against what it read, but saves nothing and warns.
- **Unreadable, corrupt or unknown-version file.** The scan treats it as "no
  samples", records fresh ones and warns on stderr. It never fails the scan.
- **When sample 1 is replaced:**
  - there is no sample; or it is from another session or transcript; or it
    has no transcript date; or it has no pane signature (one capture error
    must not disable detection for the rest of the session); or its
    `sampled_at` is missing or in the future → record the current observation
    as sample 1;
  - no scan saw the polecat for more than `stallSampleMaxScanGap` (15m) →
    nobody watched the window (host asleep, witness down), so re-record rather
    than report every quiet polecat stalled on wake. 15m is 3× the 5m cap,
    not 2×: a quiet witness scans once per cap interval plus the cycle's own
    duration and, after step 3, its boot, and a bound that ordinary cycles
    exceed would re-record on every scan and silently disable detection;
  - the transcript advanced (mtime or size) or the pane signature changed →
    the agent is working, so re-record sample 1 now;
  - otherwise keep sample 1. This covers "too soon", a missing signal on the
    current observation, a dead agent and a stall verdict.
- **Verdict.** Sample 2 is the current observation. The scan compares it with
  `witness.AssessStall(sample1, current, window)`, the existing Go rule, which
  until now had no production caller.
- **Cleanup.** At each scan, samples for polecats with no live session are
  deleted. A dead session is the zombie path's job, and a restarted one must
  start a new sample 1 anyway.

### Surface

`gt patrol scan` does all of this; there is no new command. Every witness
cycle already runs it, including abbreviated cycles. Scan output gains, per
activity item: `stall_sample_at`, `stall_sample_age_seconds`, `stalled` and
`stall_reason` (JSON); the human output prints a STALLED line only on a
positive verdict. `--stall-window` (default 30m, the gt-xb27 window) sets the
comparison window.

The formula's stall procedure changes from "note sample 1 yourself, then
compare later" to "read `stalled` and `stall_reason`". The policy on a stall is
unchanged: nudge, then escalate to the Mayor with both samples; never restart
on your own judgement.

## Step 2: the boundary

### Candidates

a. After each full patrol.
b. When the idle backoff reaches its cap.
c. Every N cycles, as a backstop.

Note: the formula caps await-signal at `--backoff-max 5m`, not 15m. The 15m in
the bead is the example in `gt mol await-signal --help`. With base 30s and
multiplier 2, the witness reaches the 5m cap on its fifth idle timeout, about
7.5 minutes into a quiet stretch.

### Cost model

Let:

- `C0` = context of a fresh session (Claude Code system prompt and tools, the
  16.7 KB witness static prompt, the prime hook payload, the formula step),
  assumed to be about 30k tokens;
- `g` = context growth per quiet cycle, about 10k tokens (50k→167k over roughly
  an hour of mixed cycles);
- `k` = calls per quiet cycle, about 5;
- `r` = price of a cached input token relative to an uncached one, about 0.1
  for DeepSeek.

A cycle at context `C` costs about `k·r·C`, in uncached-token equivalents. A
respawn costs about `C0` uncached tokens (the first call is a cold cache) plus
20–40 s of boot and one `gt prime` (6.8 s for the refinery).

Respawning every `N` cycles averages `C0/N + k·r·(C0 + g·N/2)` per cycle. That
is smallest at `N* = sqrt(2·C0 / (k·r·g))`, about 3.5 with the numbers above,
where it is about 32k per cycle. Today's session averages about `k·r·100k` =
50k per cycle, plus the compactions (each a large uncached call). Respawning
every cycle (`N = 1`) costs about 47.5k, no better than today: a cold 30k
start against cheap cache reads of a long context. Respawning every 12 cycles
also costs about 47.5k.

So the saving comes from **how often** the witness respawns, and the best
frequency is every few cycles. Respawning every cycle wastes the saving on
cold starts.

### Recommendation: (b) plus (c), not (a)

**Respawn at the end of a cycle whose await-signal timed out at the backoff
cap, once the session has run at least 3 cycles. As a backstop, respawn after
8 cycles whatever the effort level.**

- **(b) is the cheapest place to respawn.** The next cycle is a full cap
  interval away, so the boot latency mostly overlaps time the witness would
  have spent waiting (it still delays a wake-up that arrives mid-boot), and a
  quiet rig is where the long session's tokens are spent on nothing. The 3-cycle minimum keeps a
  quiet stretch to about one respawn every 15 minutes at a 5m cap, near `N*`,
  and not every cycle, where cold starts cost more than they save.
- **(c) bounds a rig that is never idle.** A busy gastown may never reach the
  cap. Eight cycles keeps context to about `C0 + 8g` = 110k at worst, below
  where it compacts today.
- **(a) is rejected.** Full patrols cluster during activity, 30 s to a few
  minutes apart. A respawn there adds 30–50 s of latency just when polecats
  need attention, and pays a cold start many times an hour. The stale-belief
  risk in an active stretch is covered by (c).

Both numbers are config values (below), so the measurement in step 5 can tune
them without a code change.

### Where the trigger lives: `gt patrol report`, in Go

The refinery lesson applies: the trigger cannot be left to the formula,
because patrol agents skip end-of-cycle steps (the refinery ran 2+ hours
without them, and the deacon ran 9 cycles skipping await-signal and handoff).
The one step the witness runs every cycle, after await-signal, is
`gt patrol report`, which already closes the patrol wisp and pours the next.
That is the refinery's `ClosePatrol` step, so the respawn goes right after
it, in `runPatrolReport`, when the caller is a witness.

The report needs two facts it does not have today:

1. **Did this cycle's await-signal time out at the cap?** `gt mol await-signal`
   writes `<cwd>/.runtime/await-signal-last.json`
   (`{reason, timeout, backoff_max, at_cap, at}`) atomically on every return.
   The report reads it and ignores one older than the cycle's wisp.
2. **How many cycles has this session run?** A counter in
   `<witness dir>/.runtime/witness-cycles.json`, keyed by the Claude session ID
   that the SessionStart hook already records. A new session ID resets it, so a
   respawn needs no explicit reset and a daemon restart is handled too.

### The fresh session's first cycle

A fresh session runs a patrol before its first await-signal, and it has no
EFFORT directive, so today it would run a **full** patrol, about 10 times the
tokens of an abbreviated one, on every respawn. That would cancel much of the
saving. `gt prime` for the witness must therefore print the effort the
persisted `idle:N` label implies (the same rule await-signal applies), so a
respawn at the cap is followed by an abbreviated cycle. This is part of step 3.

A respawn opens a short blind window for events (the deacon escalated a
similar one, hq-wisp-twfom). The fresh session's first cycle is a discovery
pass (mail drain, `gt patrol scan`), which covers anything the window missed,
and events addressed to the witness wait in its mail and nudge queue.
`drainNudges=false` on the report keeps queued nudges for the successor, as
the refinery does.

## Step 3: reuse the refinery mechanism

`completeUnitAndCycle` is refinery-specific in its chores and in
`unitCycleCallerMismatch` (`GT_ROLE == <rig>/refinery`). The respawn half is
generic. Factor these out of `unit_cycle.go` into a small role-agnostic
helper, `cycleOwnSession`, with the same injected-deps pattern:

| Piece | Today (refinery) | Shared helper |
|---|---|---|
| caller guard | `unitCycleCallerMismatch` (`GT_ROLE`, `TMUX_PANE`, pane's session) | same checks, taking the expected role and session as parameters |
| cooldown | `lastHandoffAge(workDir)` vs `constants.MinHandoffCooldown`; skip, never sleep | unchanged |
| record | `recordHandoffTimeIn`, `writeHandoffMarker(…, "unit-cycle")`, `LogHandoff`, `events.LogFeed` | unchanged; reason `"unit-cycle"` for both roles |
| respawn | `buildRestartCommandWithOpts(session, {ContinueSession: false})`, `updateSessionEnvForHandoff`, `respawnOwnPane` | unchanged |
| on failure | log, escalate `refinery-respawn-failed:<rig>` MEDIUM, keep the session | fingerprint `<role>-respawn-failed:<rig>` |

For the witness: the session is `session.WitnessSessionName(prefix)`, the work
directory is `witness.WitnessStateDir(townRoot, rig)` (the same path as
`Manager.witnessDir`), there are no chores, and `ClosePatrol` is already done
by the report that calls the helper. `completeUnitAndCycle` keeps its
behaviour and its tests; it calls the shared helper for its steps 2–3.

### Per-rig flag, off by default

`settings/config.json` of the rig gets a new `witness` block in
`config.RigSettings`:

```json
"witness": {
  "cycle_session_at_idle_cap": true,
  "cycle_session_min_cycles": 3,
  "cycle_session_max_cycles": 8
}
```

- `cycle_session_at_idle_cap` (bool, default false) turns the whole feature
  on. With it off, `gt patrol report` behaves exactly as today.
- `cycle_session_min_cycles` (default 3) sets the minimum number of cycles
  before a respawn at the cap.
- `cycle_session_max_cycles` (default 8, 0 = no backstop) sets the backstop.

These are separate from the town-level `operational.witness` thresholds
(`WitnessThresholds`), which tune detection rather than session lifetime. Start
with gastown only, as the refinery did.

### Step 3: as implemented

The shipped code differs from the plan above in these details:

- **Config lives in the rig root `config.json`**, next to
  `merge_queue.cycle_session_after_merge`, not in `settings/config.json`:
  `rig.RigConfig.Witness` (`config.WitnessSessionConfig`), read by
  `rig.ResolveWitnessSessionConfig`. To enable it for gastown:

  ```json
  "witness": { "cycle_session_at_idle_cap": true }
  ```

  `cycle_session_min_cycles` (default 3) and `cycle_session_max_cycles`
  (default 8; negative disables the backstop) are optional.
- **Files.** Both live in `<witness dir>/.runtime/`, the directory the
  session runs in and where the SessionStart hook persists `session_id`:
  - `await-signal-last.json`: `gt mol await-signal` writes it on every return
    when `GT_ROLE` is `<rig>/witness` and the flag is on (flag off: nothing
    is written). It records the reason, the FULL backoff window (a resumed
    wait still counts as at the cap), the cap, `at_cap`, the idle count, the
    effort level, the session ID and the time.
  - `session-cycles.json`: the counter, `{session_id, cycles, last_wait_at}`.
    A different `session_id` restarts the count, so a respawn, a daemon
    restart or a crash all start a fresh session at zero. The respawn also
    writes zero itself, which covers a session with no recorded ID.
  - Both are Go-owned, versioned and written atomically by
    `internal/patrolstate` (cycle.go). An unreadable file warns and reads as
    "nothing recorded", which can delay a respawn but never causes one.
- **Staleness is a watermark, not the wisp age.** The report attributes a wait
  outcome to its cycle only when the outcome is newer than `last_wait_at` (the
  last one a report consumed) and came from the same session. A report after a
  skipped await-signal, or a predecessor's unconsumed wait, therefore counts
  as "no wait recorded".
- **Order in `gt patrol report`** (`reportAndMaybeCycleWitness`,
  `internal/cmd/witness_cycle.go`): decide every gate (caller, counter,
  boundary, cooldown); run the report, with `drainNudges=false` only when
  the session is about to respawn; save the counter; record the cycle
  (cooldown stamp, handoff marker `unit-cycle`, town log); respawn last. On
  a skip it prints `○ session kept: <cause>`. With the flag off, the report
  is exactly today's and prints nothing extra. A failed report is returned
  and nothing is counted. A counter that cannot be saved keeps the session.
  A failed respawn escalates `witness-respawn-failed:<rig>` MEDIUM.
- **Shared helper** (`internal/cmd/session_cycle.go`): `ownPaneCallerMismatch`,
  `handoffCooldownCause`, `recordOwnSessionCycle` and `respawnOwnSessionFresh`
  were extracted from `unit_cycle.go`. The refinery calls them unchanged in
  behaviour. The deacon (claude-9jq tier 4) needs only a directory and flag in
  `patrolCycleDir` and a caller like `reportAndMaybeCycleWitness`.
- **First cycle's effort.** After a `unit-cycle` handoff, `gt prime` for the
  witness reads `await-signal-last.json` and, when the predecessor's last wait
  implied `abbreviated`, prints one `EFFORT: reduced ...` line (about 100
  characters), so the first patrol is not a full one. No bd call is added to
  prime.
- **Formula.** `mol-witness-patrol` v21 tells the agent that a respawn may
  follow a quiet report and that it need not act on it.

What the fresh session needs and where it comes from: stall sample 1
(`stall_samples.json`, step 1); `idle:N` and `backoff-until` (agent bead
labels, so the backoff stays at the cap); the patrol wisp (`gt patrol report`
already poured the next one); mail and queued nudges (the respawning report
does not drain them); lessons (`gt remember`); and role context (`gt prime`).
Ad-hoc watches the agent invented are still lost, as agreed above.

## Step 4: `gt prime` for the witness fits the hook limit

Claude Code delivers at most 10,000 characters of hook output and replaces
anything longer with a 2 KB preview (gt-layt). Measured:

- **Hermetic render** of the real `mol-witness-patrol` checklist and startup
  directive through `assemblePrimePayload` (the path `gt prime --hook` takes,
  no mail, memories or directives): 4,126 characters. The checklist is 3,720 of
  them. `TestPrimeRoleFixturesFitHookBudget` already holds the witness under
  8,000 with a 2,000+ character directive and 120 memory lines.
- **Live gastown witness transcripts** (2026-09-22 to 09-24): the SessionStart
  hook payload ranged from 2,461 to 7,120 characters. None was persisted to a
  file.
- **Static role text** (16.7 KB, `witness/.claude/system-prompt.md`) arrives
  through `--append-system-prompt-file`, not the hook, so the limit does not
  apply to it.

So the witness prime fits with room to spare. The step 2 change adds one
EFFORT line (under 100 characters). The fresh-prime monitor in
`~/.claude/tools/gt-watch` (`fresh-prime-watch.sh`) keeps watching for
`TRUNCATED`.

## Step 5: measure before and after

**Before** (flag off, now): take gastown witness transcripts in
`~/gt/.claude-town/projects/-Users-sloan-gt-gastown-witness/` for a 24 h
window.

- Split them into cycles at each `gt patrol report` tool call.
- For each cycle, sum `input + cache_read + cache_creation` tokens. Dedupe on
  `message.id` (usage is triple-counted otherwise) and use a timezone-aware
  window.
- Record the calls per cycle, the context size per call (median and p90),
  compactions per hour and the effort level of each cycle.

**After** (flag on for gastown, 24 h): the same numbers, plus respawns per
hour by trigger (cap or backstop), the prefix-cache cold-start cost of each
respawn (uncached tokens and price of the fresh session's first calls, which
tests the `C0` and `r` assumptions above), the boot time (handoff event to the first tool call), and skipped
respawns by reason (cooldown, guard).

**Success criteria:**

- tokens per quiet cycle drop by at least 30%, in uncached-token equivalents;
- the median context is below 70k;
- zero compactions;
- no rise in the time the witness takes to act on POLECAT_DONE and MERGED
  mail.

**Stall detection is proven hermetically, not on a live polecat.** Step 1's
tests stage a stalled polecat with fake observations: sample 1 is recorded by
one store, a new store instance (a respawned witness) reads it after the window,
and the verdict is `stalled`. A working polecat, a restarted session and a
vanished polecat each reset or prune the sample. After the rollout, check the
live scan's `stall_sample_age_seconds` grows across a respawn (read-only
`gt patrol scan --json`).

## Out of scope and related

- **gt-c9g2k:** the witness formula omits `--steps`, so the step ledger reads
  "NOT REPORTED". Fix it with step 3: once each session is short, the ledger is
  the main audit trail.
- **Deacon:** its loop is the same shape (await-signal blind window,
  hq-wisp-twfom). Apply the same helper once the witness numbers are in.
- **Local-model window.** The formula allows a 10-minute window for
  local-model sessions. The scan cannot tell a polecat's model, so the default
  stays at 30 minutes. `--stall-window` exists for a rig that runs only local
  models.
