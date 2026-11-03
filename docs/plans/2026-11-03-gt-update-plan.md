> Status: historical (2026-11). Merged in: gt-o4sk8 (the epic; the live status of each bead is on the bead). Not maintained.

# gt update Implementation Plan

Written 2026-11-03 for the design in docs/plans/2026-11-03-gt-update-design.md, approved by the operator. The beads are filed from this plan one chain at a time under the epic gt-o4sk8; nothing in the plan is built when it lands.

> **For agentic workers:** every task below is one Gas Town bead (twenty minutes of one polecat's work). The overseer files them in the order under "Dispatch order", reviews the risk-path landings, and runs the steps marked "overseer-run" with the operator. A polecat reads only its own bead; the Interfaces block of each task gives the names it shares with its neighbours.

**Goal:** one host-side command `gt update` that does install, sandbox image and maintenance as one job with no modes, plus the small daemon trigger, state record and dashboard banner around it, and the Dolt container track.

**Architecture:** a new package `internal/update` holds all job logic behind small interfaces (clock, files, commands, hold, probes) so every step is tested without Docker, network or sleeps. `gt update` (internal/cmd/update.go) wires the real effects. The daemon gains a trigger that decides when an update is due and reads `.runtime/update/state.json`. Version 1 asks the daemon's existing maintenance cycle to run through a request file.

**Tech stack:** Go, the existing `internal/atomicfile`, `internal/dispatch` (hold path), `internal/constants` (runtime paths), `github.com/gofrs/flock`, cobra.

**Spec:** docs/plans/2026-11-03-gt-update-design.md

## Global Constraints

- Defaults, copied from the spec: drain timeout 30 minutes; a seat counts as drained when its last activity is 5 minutes old; restart wait 15 minutes; heartbeat every 15 seconds; stale heartbeat 3 minutes; overrun 2 hours; maintenance request wait 30 minutes; trigger `window` 03:00, `min_interval` 3h, `max_runtime` 2h; archive loop budget 10 minutes; archive batch bounds from `archive_commit_limit` and `archive_events_limit` (default 5000).
- Go with unit tests. No new scripts. No `time.Sleep`, no real Docker, network, cluster or host paths in tests; inject the clock and a sleep function.
- Packages `internal/daemon`, `internal/cmd`, `internal/beads`, `internal/git`, `internal/polecat`, `internal/rig` are container-suite packages: their acceptance boxes and gate lines use a narrow `-run` filter, never the whole package. `internal/update` and `internal/dispatch` are ordinary packages.
- Every acceptance box is one a polecat can tick alone: a named Go test, a `go vet` line, or a grep. No `make gate`, no Docker, no cluster, no live-town measurement in a box (the overseer measures after install).
- Docs under docs/ start with a `> Status:` line (the docs lint) and have only one.
- Risk-path flag "yes" means the landing gets an overseer review and PASS note (daemon, hold, installer, config).
- Commits and branches carry the polecat's own name; no AI attribution.

## File structure

| File | Responsibility |
|---|---|
| internal/update/state.go | State, StepRecord, Outcome, Store (atomic write, read), Dir |
| internal/update/support.go | history.jsonl append, heartbeat loop, lock |
| internal/update/plan.go | Probes, BuildPlan |
| internal/update/hold.go | Hold ownership, WithHold |
| internal/update/drain.go | SeatProbe, Drain |
| internal/update/installer.go | ParseInstallerResult, Installer, ExecInstaller |
| internal/update/verify.go | VerifyProbes, Verify, WaitForRestart |
| internal/update/maintenance.go | MaintenanceRequest (job side), result file |
| internal/update/run.go | Deps, Options, Run (the orchestrator) |
| internal/cmd/update.go | `gt update`, `gt update status`, real effects |
| internal/config/daemon_config.go | UpdateConfig under daemon.patrols.update; archive_budget |
| internal/daemon/update_due.go | pure due rules |
| internal/daemon/update_trigger.go | triggerUpdate, launch seam, coexistence |
| internal/daemon/update_alerts.go | stale and overrun alerts, attention item |
| internal/daemon/scheduled_maintenance.go | request consumption |
| internal/dashboard/{types.go,hub.go,index.html} and internal/cmd/dashboard.go | `update` feed section and banner |
| internal/doltserver/containerplan.go and internal/cmd/dolt.go | `gt dolt container plan` |
| docs/update-runbook.md, docs/dolt-container-runbook.md | runbooks |

## Track A: two small beads first

### A0: gt-5cl4i (already filed, deferred)

The hold stops only dispatch. Release it first. Risk path: yes. It edits standby.go, tier_sweep.go, scheduled_maintenance.go and maintenance_catchup.go, so everything in the daemon chain waits for it to land.

### A1: archive loop and archive_budget setting

**Depends on:** gt-s6srv (landed), A0. **Risk path:** yes. **Size:** S.
**Files:** internal/config/daemon_config.go; internal/daemon/scheduled_maintenance_archive.go; their tests.
**Interfaces.** Consumes `ParseArchiveCommitLimit` and `ParseArchiveEventsLimit` (landed). Produces `(*ScheduledMaintenanceConfig).ParseArchiveBudget() (time.Duration, error)` (key `archive_budget`, a Go duration string, default 10m, refuses below 1m or above 30m, which is the step timeout) and, in the daemon, `archiveUntilCaughtUp`.
**Goal.** Today one cycle archives at most one bounded batch of commits and one of events per database, so gt (23,523 commits and 138,430 events behind) needs about 28 cycles. Make the step keep taking batches for a database until a batch returns fewer rows than the bound (nothing left) or the budget ends, then move to the next database. Each batch advances its watermark exactly as today.
**Constraints.** Change only the two files and their tests. Do not change watermark, lookback, file naming or the gc and backup steps. The existing 30 minute step timeout still bounds the whole step.
**Out of scope.** The window job, request file, any other maintenance setting.
**Gate.** `go test ./internal/config -run 'Archive' -count=1`; `go test ./internal/daemon -run 'Archive' -count=1`.
**Acceptance.**
- [ ] TestArchiveLoopTakesSeveralBatchesInOneCycle: a finite fake source of 12 fresh commits with the bound at 5 is fully archived in one call (three batches) and the watermark ends at the last row
- [ ] TestArchiveLoopStopsAtTheBudget: with an injected clock that passes the budget after two batches, a third batch is not taken and the next call continues from the watermark
- [ ] TestArchiveLoopStopsWhenNothingIsBehind: a database with no fresh rows costs one read and no file
- [ ] TestArchiveBudgetSetting: unset or absent means 10m; "30s" and "45m" are refused with the key named
- [ ] go vet ./internal/config ./internal/daemon is clean

## Track B: the gt update core

All of B1 to B9 are in the new package `internal/update` and can be built in parallel (they touch different files). B10 onward touch shared packages and are chains.

### B1: state record and store

**Depends on:** none. **Risk path:** no. **Size:** S. **Files:** internal/update/state.go, state_test.go.
**Interfaces. Produces:**
```go
type Clock interface{ Now() time.Time }
type Outcome string // "running","ok","partial","aborted","failed","interrupted"
type StepName string // "plan","drain","maintenance","install","restart","verify","lift"
type StepResult string // "pending","running","ok","skipped","failed","deferred"
type StepRecord struct {
    Name StepName; WillRun bool; Reason string; Result StepResult
    StartedAt, EndedAt time.Time; Detail string
}
type Target struct{ SHA, InstalledSHA string }
type HoldRecord struct{ Owned bool; Note string }
type State struct {
    V int; RunID string; Source string // "manual" or "daemon"
    StartedAt, HeartbeatAt, EndedAt time.Time
    Target Target; Steps []StepRecord; Current StepName
    Hold HoldRecord; Outcome Outcome
}
func Dir(townRoot string) string // <town>/.runtime/update via constants.TownRuntimePath
type Store struct{ Dir string }
func (s *Store) Write(State) error // atomic: temp file in Dir, rename
func (s *Store) Read() (State, error) // ErrNoState when state.json is absent
var ErrNoState error
```
**Goal.** The one record the job writes at every step and the daemon and dashboard read. JSON tags are snake_case; `V` is 1.
**Constraints.** Use internal/atomicfile as tier_sweep.go does. The directory is created 0700. Read refuses a record with `v` other than 1.
**Out of scope.** History, heartbeat, lock (B2).
**Gate.** `go test ./internal/update -count=1`.
**Acceptance.**
- [ ] TestStoreWriteReadRoundTrip: a full State survives Write then Read unchanged
- [ ] TestStoreWriteIsAtomic: a failing rename (injected) leaves the previous state.json intact and no partial file
- [ ] TestStoreReadMissing: Read returns ErrNoState when there is no file
- [ ] TestStoreReadRefusesUnknownVersion: v 2 is an error naming the version
- [ ] go vet ./internal/update is clean

### B2: history, heartbeat, lock

**Depends on:** B1. **Risk path:** no. **Size:** S. **Files:** internal/update/support.go, support_test.go.
**Interfaces. Consumes** `State`, `Store`. **Produces:**
```go
func (s *Store) AppendHistory(State) error // one JSON line per finished run in history.jsonl, 0600
func RunHeartbeat(ctx context.Context, s *Store, snapshot func() State, ticks <-chan time.Time, clock Clock)
func AcquireLock(dir string) (release func(), err error) // flock on dir/lock; ErrLocked when held
var ErrLocked error
```
**Goal.** `RunHeartbeat` rewrites state.json with `HeartbeatAt` set on every tick until ctx ends (the caller passes `time.NewTicker(15 * time.Second).C`; tests pass their own channel). The lock stops two runs overlapping.
**Constraints.** `github.com/gofrs/flock`. AppendHistory writes only runs whose Outcome is not "running" and records per-step durations (`ended_at - started_at`) in a `durations_ms` map.
**Out of scope.** Reading history back.
**Gate.** `go test ./internal/update -count=1`.
**Acceptance.**
- [ ] TestHeartbeatRewritesOnEveryTick: three ticks give three writes with increasing heartbeat times
- [ ] TestHeartbeatStopsWithContext: cancelling ctx ends the loop with no further write
- [ ] TestLockExcludesASecondHolder: a second AcquireLock on the same dir returns ErrLocked, and succeeds after release
- [ ] TestAppendHistoryRecordsDurations: a finished run appends one line with per-step durations; a running state appends nothing
- [ ] go vet ./internal/update is clean

### B3: the plan step

**Depends on:** B1. **Risk path:** no. **Size:** S. **Files:** internal/update/plan.go, plan_test.go.
**Interfaces. Produces:**
```go
type Probes interface {
    InstalledSHA(ctx context.Context) (string, error)
    MainSHA(ctx context.Context) (string, error)
    ArchiveBehind(ctx context.Context) (commits, events int, err error)
    BackupToday(ctx context.Context) (bool, error)
    GCDue(ctx context.Context) (due bool, reason string, err error)
    ImagePublished(ctx context.Context, sha string) (bool, error)
}
type PlanOptions struct{ Skip, Only map[StepName]bool; Force bool }
func BuildPlan(ctx context.Context, p Probes, o PlanOptions) ([]StepRecord, Target, error)
```
**Goal.** Return the plan as StepRecords for drain, maintenance, install, restart, verify and lift, each with WillRun and a one-line Reason. Rules: install runs when installed != main, or the image for main is not published, or Force; drain runs only when install runs; maintenance runs when the archive is behind, today's backup is missing, or a gc is due (reason names which); restart and verify follow install; lift follows drain. `Only` keeps just the named steps; `Skip` removes them; a skipped step keeps its Reason ("skipped by --skip").
**Constraints.** Probe errors fail the plan with the probe named (no guessing). Pure: no I/O.
**Out of scope.** Running anything.
**Gate.** `go test ./internal/update -count=1`.
**Acceptance.**
- [ ] TestPlanInstallWhenMainMoved: installed != main plans drain, install, restart, verify, lift with the SHAs in the reason
- [ ] TestPlanNothingToDo: equal SHAs, image published, no archive gap, backup present, no gc due plans every step as skipped
- [ ] TestPlanMaintenanceReasons: each of the three triggers alone plans maintenance and names itself in the reason
- [ ] TestPlanOnlyAndSkip: --only maintenance drops the install chain; --skip drain keeps install but not drain
- [ ] TestPlanProbeErrorNamesTheProbe

### B4: hold ownership

**Depends on:** B1. **Risk path:** yes (the hold). **Size:** S. **Files:** internal/update/hold.go, hold_test.go.
**Interfaces. Consumes** `HoldRecord`; `dispatch.HoldFilePath(townRoot)`. **Produces:**
```go
type Hold struct{ Path string; Files FileOps }
type FileOps interface{ Stat(string) error; ReadFile(string) ([]byte, error); WriteFile(string, []byte, os.FileMode) error; Remove(string) error }
func NewHold(townRoot string, files FileOps) *Hold
func (h *Hold) Acquire(note string) (HoldRecord, error) // writes the note when no hold exists (Owned true); when one exists returns Owned false with its content as Note
func (h *Hold) SetNote(note string) error // only when owned; else a no-op
func (h *Hold) Release() error // removes the file only when owned; idempotent
func WithHold(ctx context.Context, h *Hold, note string, fn func(HoldRecord) error) error
```
**Goal.** The job sets `~/gt/seat-refill.hold` with a note and removes it only if it created it. `WithHold` always releases on return, on error, and when ctx is cancelled (the command cancels ctx on SIGINT and SIGTERM).
**Constraints.** Fail closed: a Stat error other than not-exist is an error, never "no hold". The file mode is 0644.
**Out of scope.** Signal handling itself (B10).
**Gate.** `go test ./internal/update -count=1`.
**Acceptance.**
- [ ] TestAcquireWritesNoteAndOwns: no hold present: file written with the note, Owned true
- [ ] TestAcquireRespectsAForeignHold: a hold present: file untouched, Owned false, Note is its content
- [ ] TestReleaseNeverRemovesAForeignHold: Release after a foreign Acquire leaves the file
- [ ] TestWithHoldReleasesOnErrorAndCancel: fn error and ctx cancel each remove an owned hold
- [ ] TestStatErrorFailsClosed: an unreadable path is an error from Acquire

### B5: the drain step

**Depends on:** B1. **Risk path:** no. **Size:** S. **Files:** internal/update/drain.go, drain_test.go.
**Interfaces. Produces:**
```go
type Seat struct{ Name string; Working bool; LastActive time.Time }
type SeatProbe interface{ Seats(ctx context.Context) ([]Seat, error) }
type DrainOptions struct{ Timeout, IdleAfter, Poll time.Duration }
type DrainResult struct{ Drained bool; Blocking []string; Waited time.Duration; Err error }
func Drain(ctx context.Context, clock Clock, sleep func(context.Context, time.Duration) error, p SeatProbe, o DrainOptions) (DrainResult, error)
```
**Goal.** Poll until no seat is blocking or Timeout passes. A seat blocks when Working and `now - LastActive < IdleAfter`. A working seat silent for IdleAfter or more does not block (it is left alone, never killed). On timeout return Drained false with the blocking seat names. Fail closed: when `SeatProbe.Seats` returns an error, the drain does not count that poll as "no seat blocking"; it retries on the next poll until the timeout, and if the probe still errors at the timeout it returns Drained false with the probe's error in the result's `Err` field (never Drained true).
**Constraints.** Defaults 30m, 5m, 15s are applied by the caller; Drain uses the given values. No side effects on seats.
**Out of scope.** Setting or lifting the hold; the real SeatProbe (B10 builds it from the polecat heartbeats).
**Gate.** `go test ./internal/update -count=1`.
**Acceptance.**
- [ ] TestDrainReturnsWhenSeatsFinish: a seat working then done after two polls ends Drained true
- [ ] TestDrainTreatsASilentSeatAsDrained: Working with LastActive 6m old (IdleAfter 5m) does not block
- [ ] TestDrainTimesOutNamingTheBlockers: a seat working and active past the timeout gives Drained false and its name
- [ ] TestDrainNeverActsOnSeats: the fake probe exposes no mutating method; the test asserts only Seats was called
- [ ] TestDrainProbeErrorIsNotDrained: a probe that always errors ends at the timeout with Drained false and Err set; a probe that errors once then reports a finished seat ends Drained true after the retry
- [ ] TestDrainStopsOnContextCancel

### B6: installer wrapper

**Depends on:** B1. **Risk path:** yes (installs). **Size:** S. **Files:** internal/update/installer.go, installer_test.go.
**Interfaces. Produces:**
```go
type InstallResult struct{ Event, Commit, Prev, Reason string }
func ParseInstallerResult(output string) (InstallResult, bool) // the last line "install-gt: RESULT <event> <commit> <prev> <reason...>"
type Installer interface{ Install(ctx context.Context, sha string) (InstallResult, error) }
type RunFunc func(ctx context.Context, dir string, env []string, name string, args ...string) (output string, exit int, err error)
type ExecInstaller struct{ RepoRoot string; Run RunFunc }
```
**Goal.** `ExecInstaller.Install` does what `rebuild_gt` does: run `make gt-installer` in RepoRoot, then `<RepoRoot>/gt-installer --sha <sha> --source gt-update` with env `INSTALL_GT_LOCK_WAIT=120` and `INSTALL_GT_RIG_DIR=<RepoRoot>`, and parse the RESULT line. First read internal/installer/installer.go (read only; the RESULT format is documented near line 37) and list its event names in a table in internal/update/installer.go (the file this bead creates); map each to ok, deferred or failed. Find how `rebuild_gt` chooses its repo root (internal/daemon/rebuild_gt.go, `triggerRebuildGT`) and use the same rule for the default in the command (B10), not here.
**Constraints.** A missing RESULT line is an error that includes the last 20 output lines. Exit code nonzero with a RESULT line still returns the parsed result and an error.
**Out of scope.** Verifying the install (B7); changing the installer.
**Gate.** `go test ./internal/update -count=1`.
**Acceptance.**
- [ ] TestParseInstallerResult: installed, deferred and failed lines parse into the four fields; a line without a reason parses with Reason "-"
- [ ] TestInstallerEventTableCoversEveryEvent: every event name constant found in internal/installer appears in the table (the test greps the package source through go/parser, no hard-coded list)
- [ ] TestExecInstallerRunsMakeThenInstaller: the fake RunFunc sees `make gt-installer` then the installer with the exact args and env
- [ ] TestExecInstallerMissingResultLine: error includes the output tail
- [ ] go vet ./internal/update is clean

### B7: verify and wait for the restart

**Depends on:** B1. **Risk path:** no. **Size:** S. **Files:** internal/update/verify.go, verify_test.go.
**Interfaces. Produces:**
```go
type VerifyProbes interface {
    GTVersion(ctx context.Context) (string, error)     // installed gt, full sha
    DaemonVersion(ctx context.Context) (string, error) // the running daemon's build
    ImageLabel(ctx context.Context) (string, error)    // org.gastown.gt-commit of gastown-polecat:dev
    Health(ctx context.Context) (string, error)        // "green" or the failing line
}
type Check struct{ Name string; OK bool; Detail string }
func Verify(ctx context.Context, p VerifyProbes, want string) []Check
func WaitForRestart(ctx context.Context, clock Clock, sleep func(context.Context, time.Duration) error, p VerifyProbes, want string, timeout, poll time.Duration) (bool, error)
```
**Goal.** `WaitForRestart` polls DaemonVersion until it equals want or timeout (15m by the caller). `Verify` returns four checks: gt version, daemon version, image label, health; each compares to want where it carries a sha (the image label compares by prefix).
**Constraints.** A probe error becomes a failed Check with the error text, never a panic or a silent pass.
**Out of scope.** Real probes (B10).
**Gate.** `go test ./internal/update -count=1`.
**Acceptance.**
- [ ] TestWaitForRestartSeesTheNewDaemon: old version twice then new returns true
- [ ] TestWaitForRestartTimesOut: never changes: false, nil error, within the injected timeout
- [ ] TestVerifyAllGreen and TestVerifyReportsEachFailure: four checks; a mismatched image label fails only that check
- [ ] TestVerifyProbeErrorIsAFailedCheck

### B8: maintenance request (job side)

**Depends on:** B1. **Risk path:** no. **Size:** S. **Files:** internal/update/maintenance.go, maintenance_test.go.
**Interfaces. Produces:**
```go
type MaintenanceRequest struct{ Dir string; Files FileOps; Clock Clock }
func (m *MaintenanceRequest) Request(runID string) error // writes Dir/maintenance-request {"run_id","at"}; nil when one from this run already exists
type MaintenanceResult struct{ Outcome string; At time.Time; Detail string } // "completed","deferred","failed"
func (m *MaintenanceRequest) Wait(ctx context.Context, sleep func(context.Context, time.Duration) error, timeout, poll time.Duration) (MaintenanceResult, error)
```
**Goal.** The job writes the request file and waits until it is gone, then reads `Dir/maintenance-result.json` (written by the daemon in B11). On timeout it returns Outcome "deferred" with Detail "daemon did not finish in time" and leaves the request file in place.
**Constraints.** A request file from another run id is an error (another job is waiting). Reuse `FileOps` from B4's file; if B4 has not landed, declare the interface in this file and let B4 import it (whichever lands second adjusts).
**Out of scope.** The daemon side (B11).
**Gate.** `go test ./internal/update -count=1`.
**Acceptance.**
- [ ] TestRequestWritesTheFile
- [ ] TestWaitReturnsTheDaemonsResult: file removed and result.json present gives its outcome
- [ ] TestWaitTimeoutLeavesTheRequest: Outcome "deferred", file still there
- [ ] TestRequestRefusesAForeignRequest

### B9: the orchestrator

**Depends on:** B1 to B8. **Risk path:** yes. **Size:** M (split in two beads if the polecat reports an overrun: B9a without the maintenance and restart steps, B9b adding them). **Files:** internal/update/run.go, run_test.go.
**Interfaces. Produces:**
```go
type Deps struct {
    Clock Clock; Sleep func(context.Context, time.Duration) error
    Store *Store; Probes Probes; Seats SeatProbe; Hold *Hold
    Installer Installer; Maint *MaintenanceRequest; Verify VerifyProbes
    NewRunID func() string; Log io.Writer
}
type Options struct {
    DryRun bool; Skip, Only map[StepName]bool; Force bool
    Source string // "manual", or "daemon" when the command was started with --from-daemon
    Drain DrainOptions; RestartWait, RestartPoll, MaintWait, MaintPoll time.Duration
}
func Run(ctx context.Context, d Deps, o Options) (State, error)
```
**Goal.** Build the plan; write it to state.json; on DryRun stop there. Otherwise run steps in order, writing state after each: drain (WithHold, SetNote with the phase), maintenance (Request, Wait), install (Installer), restart (WaitForRestart), verify, lift. Failure rules from the spec: a failed or deferred maintenance step does not stop the run and makes the outcome "partial"; a failed install aborts with outcome "failed"; a blocked drain aborts with "aborted" and the blocking seats named in Detail; ctx cancel gives "interrupted"; the hold is released on every exit; a restart timeout sets Detail "installed, restart pending" and the outcome "partial". The final State is written, appended to history and returned.
**Constraints.** The orchestrator never touches seats or processes other than through Deps. It does not take the lock or start the heartbeat (the command does, B10).
**Out of scope.** Real effects, flags, exit codes.
**Gate.** `go test ./internal/update -count=1`.
**Acceptance.**
- [ ] TestRunDryRunWritesThePlanAndRunsNothing
- [ ] TestRunHappyPath: drain, maintenance, install, restart, verify, lift all ok, outcome ok, hold released, history appended
- [ ] TestRunMaintenanceFailureIsPartial: install still runs; outcome partial
- [ ] TestRunInstallFailureAbortsAndReleasesTheHold
- [ ] TestRunBlockedDrainAborts: names the seat, no install attempted
- [ ] TestRunDrainProbeErrorAborts: a seat probe that keeps erroring gives outcome aborted with the error in Detail, no install attempted, the hold released
- [ ] TestRunCancelledIsInterruptedAndReleasesTheHold

### B10: the gt update command

**Depends on:** B9. **Risk path:** yes. **Size:** M. **Files:** internal/cmd/update.go, update_test.go.
**Goal.** Cobra command `gt update` with flags `--dry-run`, `--skip <step>` (repeatable), `--only <step>` (repeatable), `--force`, `--json`, `--from-daemon`; subcommand `gt update status` (prints state.json human-readable, or `--json`). It builds the real Deps: lock (AcquireLock, exit 5 with the running run id when held), heartbeat (RunHeartbeat with a 15 s ticker), signal handling (signal.NotifyContext on SIGINT and SIGTERM), the real SeatProbe (polecat session heartbeats via internal/polecat, the same source `workingPolecats` uses in internal/daemon/maintenance_gc.go), real Probes (installed sha from `gt version`, main from the rig's origin/main, archive behind and gc due from the daemon's `maintenance_state.json` and the Dolt status, backup from `~/gt-backups/dolt/<date>`), ExecInstaller with the repo root rule `rebuild_gt` uses, and VerifyProbes. Exit codes: 0 ok or dry-run, 1 failed, 3 partial, 4 aborted, 5 locked, 130 interrupted.
**Constraints.** Registration in the existing command tree; the effects live behind interfaces so the tests use fakes. Container-suite package: narrow `-run`.
**Out of scope.** The daemon trigger.
**Gate.** `go test ./internal/cmd -run 'Update' -count=1`.
**Acceptance.**
- [ ] TestUpdateFlagsParse: --skip and --only validate step names; an unknown name is an error listing the valid ones
- [ ] TestUpdateExitCodes: each outcome maps to its code
- [ ] TestUpdateLockedExitsFive: a held lock prints the running run id
- [ ] TestUpdateStatusPrintsAndJSON: both forms read a fixture state.json; absent state prints "no update has run"
- [ ] TestUpdateSignalReleasesTheHold: a cancelled context yields outcome interrupted and no hold left
- [ ] go vet ./internal/cmd is clean

### B11: the daemon consumes the maintenance request

**Depends on:** A0, A1. **Risk path:** yes. **Size:** S to M. **Files:** internal/daemon/maintenance_request.go (new), internal/daemon/maintenance_request_test.go (new), internal/daemon/scheduled_maintenance.go (runScheduledMaintenance), internal/daemon/maintenance_gc.go (startMaintenanceGC), internal/daemon/daemon.go (one field: the run id of the request the in-flight cycle answers, so a tick can tell a request already being served from a newer one).
**Goal.** At the 5-minute tick, if `.runtime/update/maintenance-request` exists, the daemon runs its maintenance cycle regardless of the window and the 20 hour gap. It still honors the ESTOP and the Ring (standby), the gc's quiet guard and the archive's own conditions. If the quiet guard defers, the request file stays and the next tick retries; under a standby the daemon answers "deferred" at once, naming the standby, and removes the file rather than leaving the job to time out. When the cycle finishes completed or failed, the daemon writes `.runtime/update/maintenance-result.json` (`outcome`, `at`, `detail`) and then removes the request file — re-reading it first and removing it only while it still holds the run id the cycle served, so a request another job wrote meanwhile survives to its own cycle. A request that arrives while a cycle is still running is answered "deferred": its file is removed when that cycle is serving it, and kept when it is not, so a later tick runs it. A request with no configured window runs the cycle and records no deferral against a window. A tick that cannot run the cycle at all (no databases, no local server) answers "failed" with the reason and removes the file; a failed request does not set the scheduled gap. A completed or failed cycle's answer is remembered in `daemon/maintenance_state.json` before the result is written, so a failed result write is re-answered by a later tick instead of rerunning the cycle; when that state cannot be read, a present request is answered failed and no cycle runs.
**Constraints.** Change only what the tick needs. A malformed request file is removed and logged, not retried forever. The run id is untrusted: it is bounded to 64 characters of `[A-Za-z0-9._-]`, and an invalid one is answered with an id-less failed result and never logged. Write the result before removing the request (so the job never sees "gone" without a result).
**Out of scope.** The trigger, config, alerts.
**Gate.** `go test ./internal/daemon -run 'MaintenanceRequest|ScheduledMaintenance' -count=1`.
**Acceptance.**
- [ ] TestMaintenanceRequestRunsOutsideTheWindow: a request at 14:00 starts a cycle although the window is 03:00 and the last cycle was 2 hours ago; without the request the same tick starts none
- [ ] TestMaintenanceRequestUnderStandbyAnswersDeferred: ESTOP and a waned Ring each start no cycle, write a "deferred" result naming the standby, and remove the request
- [ ] TestMaintenanceRequestDeferredByTheQuietGuardKeepsTheFile
- [ ] TestFinishMaintenanceRequestWritesResultThenRemovesIt: order proven with a recording file fake; a write error leaves the request; deferred and interrupted do nothing
- [ ] TestMaintenanceRequestMalformedOrPatrolInactive: a malformed file is removed and the tick runs as before; an inactive patrol is answered "failed" and the request removed
- [ ] go vet ./internal/daemon is clean; TestMaintenanceRequestWithNoWindowRuns (one cycle, no deferral streak); TestMaintenanceRequestResultWriteFailureDoesNotRerun (the next tick re-answers from the remembered run id and does not run gc again); TestMaintenanceRequestUnanswerablePathsWriteFailed (no databases and no local server answer failed and remove the request; the failed request leaves lastMaintenanceRun unchanged); TestMaintenanceRequestRunIDIsBounded (over 64 characters or outside the allowed set is refused with a failed result); TestMaintenanceRequestAnswerKeepsANewerRequest (answering A removes its file only while it still holds A's run id, so B's survives to its own cycle); TestMaintenanceRequestUnreadableStateDefers (an unreadable state answers the request failed and runs no cycle); TestMaintenanceRequestBusyCycleAnswersDeferred (a request the running cycle is not serving is deferred and kept; the cycle's own request is answered and released); TestMaintenanceRequestAnswerFailuresAreLogged (each answer failure leaves an alarm in the log)

### B12: update config and the due rules

**Depends on:** B11. **Risk path:** yes. **Size:** S. **Files:** internal/config/daemon_config.go, internal/daemon/update_due.go, tests.
**Interfaces. Produces:**
```go
// config: patrols.update
type UpdateConfig struct{ Enabled bool `json:"enabled"`; Window string `json:"window,omitempty"`; MinInterval string `json:"min_interval,omitempty"`; MaxRuntime string `json:"max_runtime,omitempty"` }
// the parsed form, built by (*UpdateConfig).Settings() (UpdateSettings, error):
type UpdateSettings struct{ Enabled bool; WindowHour, WindowMinute int; MinInterval, MaxRuntime time.Duration }
// daemon (pure):
func updateDue(now time.Time, cfg UpdateSettings, s UpdateFacts) (due bool, reason string)
type UpdateFacts struct{ Standby string; Running bool; InstallPending bool; MainMoved bool; Waiting int; LastEnded time.Time; LastWindowRun time.Time }
```
**Goal.** Add `Update *UpdateConfig` to the patrols block with parse helpers (defaults 03:00, 3h, 2h; refuse malformed values with the key named). `updateDue` returns due when the nightly window has opened and not run today, or when MainMoved and Waiting >= 1 and now - LastEnded >= MinInterval; never when Standby is non-empty, Running, InstallPending or the config is disabled; the reason names the rule.
**Constraints.** Pure function; no clock reads. `enabled` defaults to false.
**Out of scope.** Launching, registration.
**Gate.** `go test ./internal/config -run 'Update' -count=1`; `go test ./internal/daemon -run 'UpdateDue' -count=1`.
**Acceptance.**
- [ ] TestUpdateConfigDefaultsAndRefusals
- [ ] TestUpdateDueNightlyWindowOncePerDay
- [ ] TestUpdateDueWhenMainMovedAfterMinInterval and TestUpdateNotDueWithNothingWaiting
- [ ] TestUpdateNeverDueUnderStandbyRunningOrPending
- [ ] TestUpdateNeverDueWhenDisabled

### B13: the trigger and coexistence

**Depends on:** B12, B10. **Risk path:** yes. **Size:** M (split B13a trigger, B13b coexistence if it overruns). **Files:** internal/daemon/update_trigger.go, daemon.go (one line in heartbeatSteps), rebuild_gt.go and scheduled_maintenance.go (the coexistence guards), tests.
**Goal.** (a) `triggerUpdate` registered in `heartbeatSteps` as `{name: "update", lifecycle: true, run: (*Daemon).triggerUpdate}` (lifecycle steps are skipped under ESTOP and the Ring). It gathers UpdateFacts (state.json via the update Store, `installed` vs main, landings waiting to deploy from the landing record, the install marker), calls `updateDue`, and launches `gt update --from-daemon` through a launch seam (`updateLaunch func(ctx context.Context) error`), logging the launch with the reason; it does not wait. (b) When `patrols.update.enabled` is true, `triggerRebuildGT` returns at once with a log line "install decisions deferred to update" and the maintenance window tick starts no cycle by itself (a request still runs it).
**Constraints.** Disabled by default, so the existing behaviour is unchanged until the operator flips one key.
**Out of scope.** Alerts, dashboard.
**Gate.** `go test ./internal/daemon -run 'UpdateTrigger|RebuildGT|ScheduledMaintenance' -count=1`.
**Acceptance.**
- [ ] TestTriggerUpdateLaunchesWhenDue (fake launch seam sees one call with the reason)
- [ ] TestTriggerUpdateSkipsUnderEstop: ESTOP, no launch
- [ ] TestTriggerUpdateDoesNotLaunchTwice: a running state with a fresh heartbeat blocks it
- [ ] TestRebuildGTDefersWhenUpdateEnabled and TestRebuildGTUnchangedWhenUpdateDisabled
- [ ] TestMaintenanceWindowTickDefersWhenUpdateEnabled

### B14: stale and overrun alerts, attention item

**Depends on:** B13. **Risk path:** yes. **Size:** S. **Files:** internal/daemon/update_alerts.go, tests.
**Goal.** Each heartbeat the daemon reads state.json: outcome "running" with a heartbeat older than 3 minutes raises one escalation `update:stale` naming the run id and the hold note; a run older than `max_runtime` raises `update:overrun`; both clear when the state ends or the heartbeat resumes. An ended run with outcome failed or partial raises one attention item `update:<run id>` (acked by the usual flow). Use the same alert helper pattern as `rebuildGTAlert` in internal/daemon/rebuild_gt.go.
**Constraints.** The daemon never writes state.json. A missing state file is not an alert.
**Out of scope.** The dashboard.
**Gate.** `go test ./internal/daemon -run 'UpdateAlert' -count=1`.
**Acceptance.**
- [ ] TestUpdateStaleRaisesOnceAndClears
- [ ] TestUpdateOverrunRaises
- [ ] TestUpdateFailedOrPartialRaisesAnAttentionItem and TestUpdateOkRaisesNothing
- [ ] TestUpdateNoStateIsNotAnAlert

### B15: dashboard feed section

**Depends on:** B1, B13 (internal/cmd chain after B10). **Risk path:** no. **Size:** S. **Files:** internal/dashboard/types.go, hub.go, internal/cmd/dashboard.go, tests.
**Interfaces. Produces:** `type UpdateStatus struct{ Running bool; RunID string; ElapsedSec int; Current string; Steps []UpdateStep; Outcome string; LastEndedAt time.Time; HoldNote string; HoldAgeSec int }`; `Config.Update func() *UpdateStatus`; `State.Update *UpdateStatus` in `/api/state`.
**Goal.** Follow the tier sweep exactly (hub.go `pollTierSweep`, cmd provider): the provider reads `internal/update.Store.Read()` and the existing hold reader (internal/dashboard/hold.go) and returns nil when there is no update and no hold.
**Constraints.** Poll every 10 seconds like the other sections; no new dependency.
**Out of scope.** Rendering.
**Gate.** `go test ./internal/dashboard -count=1`; `go test ./internal/cmd -run 'DashboardUpdate' -count=1`.
**Acceptance.**
- [ ] TestUpdateStatusFromRunningState: elapsed, current step and compact steps match a fixture
- [ ] TestUpdateStatusFromHandSetHold: no update state, a hold file with a note gives HoldNote and HoldAgeSec only
- [ ] TestUpdateStatusAbsent: nothing returns nil and the feed omits the key
- [ ] TestStateFeedCarriesUpdate

### B16: the banner

**Depends on:** B15. **Risk path:** no. **Size:** S. **Files:** internal/dashboard/index.html, internal/dashboard/work_page_test.go (or a new update_banner_test.go).
**Goal.** A banner at the top of the work page: when `update.running`, "gt update: <current step>, <elapsed>. Dispatch held: <hold note>"; when only a hold exists, "Dispatch held: <note> (<age>)"; when the last run failed or was partial, a warning line linking its run id. Hidden when none apply.
**Constraints.** Follow the existing banner and pill styling; text only, no new library; the tests read the rendered page text as work_page_test.go does.
**Out of scope.** Feed changes.
**Gate.** `go test ./internal/dashboard -count=1`.
**Acceptance.**
- [ ] TestBannerShowsRunningUpdateWithHoldNote
- [ ] TestBannerShowsAHandSetHold
- [ ] TestBannerWarnsOnAFailedLastRun
- [ ] TestBannerHiddenWhenNothingApplies

### B17: the runbook

**Depends on:** B10. **Risk path:** no. **Size:** S. **Files:** docs/update-runbook.md.
**Goal.** First line `> Status: current ...`. Sections: what `gt update` does (the steps and skip rules), running it by hand (`--dry-run` first), reading `gt update status` and the dashboard banner, turning the daemon trigger on and off (`patrols.update.enabled`), the hold's note and what to do when a job dies (read the stale alert, check state.json, remove a hold whose note names a dead run), and the failure outcomes and their exit codes.
**Constraints.** Text only; the commands must match B10's flags (the polecat greps the command's `--help`).
**Out of scope.** The Dolt container runbook (C2).
**Gate.** `make docs-lint` (the polecat runs the lint target only).
**Acceptance.**
- [ ] The file exists with exactly one `> Status:` line, first
- [ ] Every flag named in the runbook appears in `internal/cmd/update.go` (grep)
- [ ] The outcome and exit code table matches B10's mapping (grep)

## Track C: Dolt in a container on the host

### C1: `gt dolt container plan`

**Depends on:** none (a different package from the daemon chain). **Risk path:** no. **Size:** S. **Files:** internal/doltserver/containerplan.go, containerplan_test.go, internal/cmd/dolt.go, internal/cmd/dolt_container_plan_test.go.
**Interfaces. Produces:**
```go
type ContainerSpec struct{ Name, Image, DataDir, BackupDir, ConfigPath string; MemoryGiB, HostPort, ContainerPort int; Restart string }
func ContainerPlan(s ContainerSpec) ([]string, error) // the docker create argv
```
**Goal.** A command that only PRINTS the `docker create` command, one shell-quoted line (or JSON with `--json`), for the operator to review and run. gt itself never creates the container (docs/design/dolt-storage.md). The argv has: `--name`, `--memory <n>g` and `--memory-swap` equal to it, `--restart <policy>` (default unless-stopped), `-p 127.0.0.1:<host>:<container>`, `-v <data>:<data>` and `-v <backup>:<backup>` (same absolute paths inside and outside), `-v <config>:<config>:ro`, then the image and `dolt sql-server --config <config>`. Required flags: `--name`, `--image`, `--config`; defaults: data dir `<town>/.dolt-data`, backup dir `~/gt-backups/dolt`, memory 12 GiB, ports 3307 and 3307.
**Constraints.** Refuse relative paths, a name outside the shape `ParseExternalContainer` accepts, a port outside 1 to 65535, memory below 1. Never emit `--privileged`, `--network host` or a Docker socket mount. The exact entrypoint is confirmed in the rehearsal (R1); the tests pin the properties above.
**Out of scope.** Running docker.
**Gate.** `go test ./internal/doltserver -run 'ContainerPlan' -count=1`; `go test ./internal/cmd -run 'DoltContainerPlan' -count=1`.
**Acceptance.**
- [ ] TestContainerPlanArgv: golden argv for the default spec
- [ ] TestContainerPlanLoopbackOnly: the published port is always bound to 127.0.0.1
- [ ] TestContainerPlanSamePathMounts: data and backup mounts use identical inside and outside paths
- [ ] TestContainerPlanRefusals: relative path, bad name, port 0 and 70000, memory 0
- [ ] TestContainerPlanNeverPrivileged: no `--privileged`, host network or socket mount for any input
- [ ] TestDoltContainerPlanCommandPrintsOneLine and its --json form

### C2: the Dolt container runbook

**Depends on:** C1. **Risk path:** no. **Size:** S. **Files:** docs/guides/dolt-container.md.
**Goal.** First line `> Status: current ...`. It holds the checklists R1 and R2 below verbatim, the rollback, and the verification commands.
**Gate.** `make docs-lint`.
**Acceptance.**
- [ ] One `> Status:` line, first
- [ ] Contains R1, R2 and the rollback as numbered steps
- [ ] Every command named exists in `gt dolt --help` or `gt dolt container plan --help` (grep)

### Overseer-run steps, with the operator (not polecat beads)

**R1 Rehearsal on a restored copy.** (1) Hold on, pause the Dolt writers per docs/dolt-history-offline.md "Before any rewrite". (2) Restore last night's backup into `~/gt/.runtime/dolt-rehearsal/` (docs/dolt-restore.md). (3) Print the plan with `gt dolt container plan --name gt-dolt-rehearsal --image <image> --config <rehearsal config> --host-port 3317`; confirm the image choice (same Dolt version as the host server, `dolt version`) and fix the entrypoint if the image needs one. (4) Create and start it. (5) `gt dolt compare` the live server against the rehearsal server (flags per its `--help`): row counts and content hashes must match for every database. (6) `gt dolt latency` against both; record p50, p95, max. (7) `docker stats` under a bd read burst and a gc: record memory; decide the limit (12 GiB unless the numbers say otherwise). (8) Remove the rehearsal container and directory. Record every number on the C2 bead.
**R2 Cutover (hold on).** (1) Record per-database row counts. (2) Hold on with a note; drain. (3) `gt dolt stop` (host server). (4) Create the real container from the printed plan, start it. (5) Set `dolt.external_container` in `mayor/town.json`. (6) `gt dolt status` shows the container; `gt dolt compare` against the recorded counts; one `bd create` and close as the write test. (7) Lift the hold; watch one dispatcher tick and one landing. **Rollback:** remove `dolt.external_container`, stop the container, `gt dolt start` (same data directory, so no copy).
**R3 Rolling squash of gt.** Prerequisite: `gt dolt status` shows every database `0 commits behind` and `0 events behind`. Follow docs/design/dolt-rolling-squash.md "First manual run" R0 to R9 with the operator present at the swap; record rebase time, disk peak, commits and size before and after in that document and on its bead. Hand-run only; no daemon step.

## Track D: later, outlines only

**D1 Version 2: extract maintenance.** Move archive, backup and gc out of internal/daemon into `internal/maintenance` (about 3,300 lines with about 180 uses of the daemon's logger, config, state and Dolt manager), with the watermark state under a file lock instead of the daemon's mutex. `gt update` then calls the package directly and the request file disappears; the daemon's window tick and `rebuild_gt` become thin triggers of `gt update` and are then deleted. Own plan, own beads, after the core has run for a week of real updates.
**D2 Cluster-runner backend for build and image.** The build and publish steps get a backend interface like `internal/sweeprunner`: a pure renderer for a Job in the control cluster (builder pod with a docker-in-docker sidecar, push to the registry), with the host keeping only the binary swap and the restart. Needs the cluster to reach the registry on the host (unmeasured) and a published-image check by digest. Design first, after the gate runner and Forgejo moves are decided (claude-2or.9 order).

## Dispatch order and the overseer's checks

1. **Release A0 (gt-5cl4i).** Check: `go test ./internal/daemon -run 'Standby|TierSweep|ScheduledMaintenance|CatchUp'` at the landed commit; under the hold file the 03:00 path and a sweep start. Ack the risk item, note PASS.
2. **A1 (archive loop)** after A0 lands. Check: the loop tests; after the next install, run the cycle once and read the per-database behind counts.
3. **B1, then B2 to B8 in parallel** (all `internal/update`, different files). Non-risk except B4 and B6; review those two.
4. **B9**, then **B10** (internal/cmd). Review both by risk. After B10: `gt update --dry-run` on the live town (read-only), read the plan for sense.
5. **Daemon chain, strictly one at a time:** B11, B12, B13, B14. Each lands before the next dispatches; review each by risk (daemon, hold, config).
6. **Dashboard chain:** B15 after B13 (it needs the state types from B1 and the `internal/cmd` slot free), then B16. In parallel with step 5 only if no bead in flight touches `internal/cmd`.
7. **B17, C1, C2** whenever a seat is free (docs and a small package).
8. **Install**, then the first real `gt update` by hand with the operator watching; record `history.jsonl` durations on the design bead.
9. **Overseer-run:** R3 (squash) once the archive reads 0 behind; R1 then R2 for the Dolt container; neither needs `gt update`.
10. **Switch on the trigger** (`patrols.update.enabled: true`) after two clean hand runs. Rollback is `enabled: false`.

After every landing the overseer checks: the risk-path diff and tests in a scratch worktree, the bead's acceptance boxes by name, that no unrelated file changed, and for daemon beads that the existing tests of that package family still pass with a narrow filter.
