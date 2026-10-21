# Spec Dispatcher Throughput Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep 7 of 9 polecat seats live by making a sling cheap, running slings in parallel, and stopping the daemon's own patrols and restarts from starving the dispatcher.

**Architecture:** Five bead-sized changes land one at a time (the sling's seat reads, the parked-seat rule, the recovery batch's idle skip, install coalescing, the bond's statement count), then one crew-session change rewires the tick to pass its roster snapshot into concurrent, seat-bounded slings. Every change is a Go edit with a unit test over the existing fakes (`fakeLister`, `beadsfake.Fake`, `rebuildGTTown`); no change needs the live town.

**Tech Stack:** Go 1.27, `go test ./internal/...`, the gastown fakes in `internal/cmd/*_test.go` and `internal/daemon/*_test.go`; the beads repo for task 5.

**Spec:** `docs/plans/2026-10-21-dispatcher-throughput-design.md`

## Global Constraints

- Read code from `origin/main`; the crew clones are detached and behind.
- Each task is one Gas Town bead: twenty minutes of one polecat, one deliverable, acceptance boxes the polecat ticks alone with `go test` of the named packages. No `make gate`, no live-town measurement, no branch binary against production.
- Tasks 1, 2 and 4 touch `internal/cmd/sling_pool.go`, `internal/cmd/pool_seats.go` or the daemon; file them with `bd dep add` so each lands before the next starts. Task 6 (crew session) starts after task 1 lands.
- Commit messages carry no AI attribution. After every commit: `bd comments add <bead-id> "commit: <hash> — <summary>"`.
- Measurement of the spec's target is the owner's, from `~/gt/daemon/daemon.log`, recorded on claude-rh7; it never appears in an acceptance box.

---

### Task 1: Sling reads through the batched seat picture (bead a)

**Files:**
- Modify: `internal/cmd/sling_pool.go:1120-1140` (`poolRouterFor`)
- Test: `internal/cmd/sling_pool_test.go`

**Interfaces:**
- Consumes: `newPoolSeatBatch(townRoot, store func(rigName string) beads.Client, show func(beads.Client, string) (*beads.Issue, error)) *poolSeatBatch` and `(*poolSeatBatch).disposition` (`pool_seats.go:139-200`); `newSpecTickReads()` / `(*specTickReads).client(beads.Client)` (`spec.go:338-390`) as the call counter in the test.
- Produces: `poolRouterForWith(townRoot string, store *poolSeatClaimStore, storeFor func(dir string) beads.Client) *poolRouter`; `poolRouterFor` keeps its signature and calls it with `beads.New`.

- [ ] **Step 1: Write the failing test**

```go
// sling_pool_test.go
func TestPoolRouterReadsSeatsThroughTheBatch(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// Two rigs, three idle polecat directories each; every polecat has an
	// agent bead and no assigned work.
	fake := beadsfake.New()
	for _, rig := range []string{"gastown", "devops"} {
		for _, name := range []string{"a", "b", "c"} {
			if err := os.MkdirAll(filepath.Join(town, rig, "polecats", name), 0o755); err != nil {
				t.Fatal(err)
			}
			prefix := map[string]string{"gastown": "gt", "devops": "dv"}[rig]
			fake.Seed(beads.Issue{
				ID: prefix + "-" + rig + "-polecat-" + name, Status: "open",
				Labels:      []string{"gt:agent"},
				Description: "role_type: polecat\nrig: " + rig + "\nagent_state: idle\ncleanup_status: clean\n",
			})
		}
	}
	writeTestRigsConfig(t, town, "gastown", "devops") // pool_seats_test.go helper: the rig registry knownRigNames reads
	pool := seatPool("claude-haiku", 3)                 // pool_seats_test.go helper; add a second seat so the pool owns seats:
	pool.Seats["deepseek-flash"] = config.SeatConfig{Cap: 3}
	reads := newSpecTickReads()
	r := poolRouterForWith(town, newPoolSeatClaimStore(), func(dir string) beads.Client { return reads.client(fake) })
	r.pool = func() *config.PolecatPool { return pool }
	r.sessions = func() sessionLister { return &fakeLister{} }
	if _, err := r.occupied()(); err != nil {
		t.Fatal(err)
	}
	// Two batched reads per rig (ShowMultiple of the agent beads, List of the
	// assigned work) and nothing per polecat directory.
	if got, want := reads.Calls(), 4; got != want {
		t.Fatalf("bd calls = %d, want %d (2 per rig for 2 rigs)", got, want)
	}
}
```

`seatPool`, `writeTestRigsConfig` and `writeTownPolecatDir` are the helpers `TestPoolSeatSessionsRestartKeepsTheCount` (`pool_seats_test.go:355-380`) uses; use `writeTownPolecatDir(t, town, rig, name)` in place of the `os.MkdirAll` above if it also writes the files `listPolecatDirectoryNames` expects. The pool needs at least two seats so `poolDeadHookedSessions` walks the directories (`poolOwnsSeats`). `r.occupied()` is the closure `route` builds at `sling_pool.go:1217-1219`; expose it as a method `func (r *poolRouter) occupied() func() ([]poolSession, error)` if it is inline today.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/cmd -run TestPoolRouterReadsSeatsThroughTheBatch -count=1`
Expected: FAIL, either `undefined: poolRouterForWith` or `bd calls = 12, want 4`.

- [ ] **Step 3: Implement**

```go
// sling_pool.go
// poolRouterFor wires the pool decision to the town on disk, the tmux server,
// bd and the seat claim of the spawn asking. The seat reads go through one
// poolSeatBatch, the two-reads-per-rig picture the spec dispatcher already
// uses (pool_seats.go), instead of two bd calls per polecat directory.
func poolRouterFor(townRoot string, store *poolSeatClaimStore) *poolRouter {
	return poolRouterForWith(townRoot, store, func(dir string) beads.Client { return beads.New(dir) })
}

// poolRouterForWith is poolRouterFor with the store for a directory passed
// in, so a test can run the wiring over a fake.
func poolRouterForWith(townRoot string, store *poolSeatClaimStore, storeFor func(dir string) beads.Client) *poolRouter {
	batch := newPoolSeatBatch(townRoot,
		func(rigName string) beads.Client { return storeFor(filepath.Join(townRoot, rigName)) },
		func(bd beads.Client, id string) (*beads.Issue, error) { return bd.Show(id) })
	return &poolRouter{
		townRoot: townRoot,
		pool: func() *config.PolecatPool {
			ts, err := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))
			if err != nil || ts == nil {
				return nil
			}
			return ts.PolecatPool
		},
		sessions:    newPoolSessionLister,
		disposition: batch.disposition,
		work:        poolSeatWorkFor(townRoot),
		seats:       poolSeatLedgerFor(townRoot, store),
		now:         time.Now,
	}
}
```

The batch holds what it read, and `realSlingSeatSpawn` builds one router per spawn (`polecat_spawn.go:728-741`), so one sling reads once. `peekPolecatPoolAgent` (`sling_pool.go:1146`) keeps calling `poolRouterFor`.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/cmd -run 'TestPoolRouter|TestPoolSeat|TestSling' -count=1`
Expected: PASS, including the existing router tests (a router built per sling still sees the live sessions and the claims through `poolSeatSessionsWith`).

- [ ] **Step 5: Commit**

```bash
git add internal/cmd/sling_pool.go internal/cmd/sling_pool_test.go
git commit -m "sling: read the seat picture through the batched reads, two bd calls per rig"
```

---

### Task 2: Parked seats stop counting after 30 minutes (bead b)

**Files:**
- Modify: `internal/cmd/pool_seats.go:368-405` (`poolDeadHookedSessions`), `internal/cmd/spec.go:1383-1415` (`specRosterFrom` / `specRoster`), `internal/cmd/sling_pool.go:266-293` (`deadHookedNote`)
- Modify: `internal/daemon/daemon.go:2710-2735` (`checkPolecatHealth`)
- Test: `internal/cmd/pool_seats_test.go`, `internal/daemon/polecat_health_test.go`

**Interfaces:**
- Consumes: `agentpause.IsPaused(townRoot, rig, role, name) (bool, *agentpause.State, error)` with `State.PausedAt` (`internal/agentpause/agentpause.go:36-55`); `intent.Record.Held()` / `HoldReason()` (`internal/intent/intent.go:177-186`); `poolSession.deadHooked`.
- Produces: `const parkedSeatRelease = 30 * time.Minute` in `pool_seats.go`; `poolSession.parkedStale bool`; `specRoster.ParkedStale map[string]int`; roster text `(1 parked-stale)`.

- [ ] **Step 1: Write the failing pool test**

```go
// pool_seats_test.go
func TestPoolDeadHookedSessions_ParkedSeatReleasesAfterThirtyMinutes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		pausedAgo time.Duration
		wantCount int
	}{
		{"parked 29 minutes ago still counts", 29 * time.Minute, 1},
		{"parked 31 minutes ago is released", 31 * time.Minute, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeTestRigsConfig(t, town, "gastown")
			writeTownPolecatDir(t, town, "gastown", "slate")
			if err := agentpause.Pause(town, "gastown", constants.RolePolecat, "slate", "overrun (20 minute rule)", "overrun_guard", "working"); err != nil {
				t.Fatal(err)
			}
			// Age the park: the marker's PausedAt is what the release reads.
			seat := intent.Seat{Rig: "gastown", Role: constants.RolePolecat, Name: "slate"}
			if _, err := intent.Update(town, seat, func(r *intent.Record) error {
				r.PausedAt = time.Now().UTC().Add(-tc.pausedAgo)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			disposition := func(rig, name string) (poolSeatState, error) {
				return poolSeatState{HookHeld: true, Disposition: polecat.WorkstateDisposition{CountsTowardCapacity: true}}, nil
			}
			pool := seatPool("deepseek-flash", 3)
			pool.Seats["claude-haiku"] = config.SeatConfig{Cap: 3}
			got, err := poolDeadHookedSessions(town, pool, disposition, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.wantCount {
				t.Fatalf("dead-hooked seats = %d, want %d: %+v", len(got), tc.wantCount, got)
			}
		})
	}
}
```

`config.SeatConfig` is the seat map's value type (`internal/config/types.go:1667`).

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/cmd -run TestPoolDeadHookedSessions_ParkedSeatReleasesAfterThirtyMinutes -count=1`
Expected: FAIL on the 31-minute case: `dead-hooked seats = 1, want 0`.

- [ ] **Step 3: Implement the release in the pool**

```go
// pool_seats.go
// parkedSeatRelease is how long a parked polecat with no session keeps its
// seat. The overrun guard parks a seat it stopped, crash detection skips a
// parked seat, and the pool counted it toward capacity for as long as it
// stayed parked: devops/mariner held a seat for 23 hours that way on
// 2026-10-20. After this long the seat is free and the roster says why.
const parkedSeatRelease = 30 * time.Minute

// parkedStale reports whether a sessionless polecat's park is older than
// parkedSeatRelease. An unreadable marker is not a park (the seat counts).
func parkedStale(townRoot, rigName, polecatName string, now time.Time) bool {
	paused, state, err := agentpause.IsPaused(townRoot, rigName, constants.RolePolecat, polecatName)
	if err != nil || !paused || state == nil || state.PausedAt.IsZero() {
		return false
	}
	return now.Sub(state.PausedAt) > parkedSeatRelease
}
```

In `poolDeadHookedSessions`, after `state, err := disposition(rigName, name)` passes its checks:

```go
			if parkedStale(townRoot, rigName, name, time.Now()) {
				// Released: listed so the roster can say "(1 parked-stale)",
				// but counted on no seat.
				out = append(out, poolSession{name: "parked/" + rigName + "/" + name, rig: rigName, polecat: name, parkedStale: true})
				continue
			}
```

Add `parkedStale bool` to `poolSession` next to `deadHooked`. In `specRosterFrom` (`spec.go:1383`) skip a `parkedStale` session for `r.Live` and count it in a new `r.ParkedStale[agent]`; in the roster text (`deadHookedNote`, `sling_pool.go:266-293`, and wherever `specRoster` prints `(N dead-hooked)`) append `(N parked-stale)` the same way. In `poolRouter.route` the admission count must also skip `parkedStale` sessions: find where `route` tallies `sessions` per agent (`sling_pool.go` around line 1230-1280, the loop over `occupied` results) and `continue` on `s.parkedStale`.

- [ ] **Step 4: Run the pool tests**

Run: `go test ./internal/cmd -run 'TestPoolDeadHooked|TestSpecRoster|TestPoolRouter' -count=1`
Expected: PASS.

- [ ] **Step 5: Write the failing crash-detection test**

```go
// polecat_health_test.go
func TestCheckPolecatHealth_StaleParkIsLoggedAsReleased(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := agentpause.Pause(townRoot, "myr", constants.RolePolecat, "mycat", "overrun (20 minute rule)", "overrun_guard", "working"); err != nil {
		t.Fatal(err)
	}
	seat := intent.Seat{Rig: "myr", Role: constants.RolePolecat, Name: "mycat"}
	if _, err := intent.Update(townRoot, seat, func(r *intent.Record) error {
		r.PausedAt = time.Now().UTC().Add(-31 * time.Minute)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var logBuf strings.Builder
	d := &Daemon{
		config:   &Config{TownRoot: townRoot},
		logger:   log.New(&logBuf, "", 0),
		tmux:     newFakeTmux(newFixedClock()),
		notifier: notifyfake.New(),
	}
	d.checkPolecatHealth("myr", "mycat")
	d.checkPolecatHealth("myr", "mycat")
	got := logBuf.String()
	if strings.Contains(got, "CRASH DETECTED") {
		t.Fatalf("a parked seat was called crashed: %q", got)
	}
	if n := strings.Count(got, "parked 31m"); n != 1 {
		t.Fatalf("release lines = %d, want one over two passes: %q", n, got)
	}
	if !strings.Contains(got, "seat released; recover with gt polecat check-recovery myr/mycat") {
		t.Fatalf("log = %q, want the recovery hint", got)
	}
}
```

- [ ] **Step 6: Run it to verify it fails**

Run: `go test ./internal/daemon -run TestCheckPolecatHealth_StaleParkIsLoggedAsReleased -count=1`
Expected: FAIL: `release lines = 0`.

- [ ] **Step 7: Implement the log line**

In `checkPolecatHealth` (`daemon.go:2725-2730`), replace the parked skip with:

```go
	if err != nil || rec.Held() {
		skipped = true
		age := time.Duration(0)
		if err == nil && !rec.PausedAt.IsZero() {
			age = d.clk().Now().Sub(rec.PausedAt)
		}
		if age > parkedSeatReleaseAge {
			d.crashSkipLog.logf(d.logger.Printf, rigName+"/"+polecatName,
				"Skipping crash detection for %s/%s: parked %s ago (%s); seat released; recover with gt polecat check-recovery %s/%s",
				rigName, polecatName, age.Round(time.Minute), rec.HoldReason(), rigName, polecatName)
			return
		}
		d.crashSkipLog.logf(d.logger.Printf, rigName+"/"+polecatName, "Skipping crash detection for %s/%s: agent is parked (%s)",
			rigName, polecatName, rec.HoldReason())
		return
	}
```

with `const parkedSeatReleaseAge = 30 * time.Minute` in `daemon.go` (the daemon package cannot import `internal/cmd`; the two constants carry the same value and a comment naming the other). `d.clk()` is the daemon's clock accessor used by `triggerRebuildGT`; if the test's `Daemon` literal has no clock, use `time.Now()` the way the surrounding code does.

- [ ] **Step 8: Run the daemon tests**

Run: `go test ./internal/daemon -run 'TestCheckPolecatHealth' -count=1`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/cmd/pool_seats.go internal/cmd/spec.go internal/cmd/sling_pool.go internal/cmd/pool_seats_test.go internal/daemon/daemon.go internal/daemon/polecat_health_test.go
git commit -m "pool: a parked seat with no session is released after 30 minutes and crash detection says so"
```

---

### Task 3: Recovery batch skips empty seats (bead c)

**Files:**
- Modify: `internal/cmd/polecat.go:1530-1580` (`runPolecatCheckRecoveryBatch`)
- Test: `internal/cmd/polecat_check_recovery_batch_test.go` (new)

**Interfaces:**
- Consumes: `beads.ListAgentBeads(bd) (map[string]*beads.Issue, error)`; `beads.ParseAgentFields(issue.Description)` plus `beads.ResolveAgentState(issue.Description, issue.AgentState)` (what `recoveryAgentBead` does at `polecat.go:2076-2081`); `loadPolecatSessionSet(townRegistry(), newPoolSessionLister()) (polecatSessionSet, error)` and `(polecatSessionSet).lookup(rigName, polecatName) (string, bool)` (`polecat_inventory.go:218-232`, used by `runPolecatList` at `polecat.go:856`); `polecatBeadIDForRig`.
- Produces: `func recoveryBatchIdle(fields *beads.AgentFields, hasSession bool) bool` and `func idleRecoveryStatus(rigName, polecatName string, p *polecat.Polecat, fields *beads.AgentFields) RecoveryStatus`.

- [ ] **Step 1: Write the failing test**

```go
// polecat_check_recovery_batch_test.go
package cmd

import (
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestRecoveryBatchIdle(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		fields     *beads.AgentFields
		hasSession bool
		want       bool
	}{
		{"done, no hook, no session", &beads.AgentFields{AgentState: "done"}, false, true},
		{"idle, no hook, no session", &beads.AgentFields{AgentState: "idle"}, false, true},
		{"idle with a live session", &beads.AgentFields{AgentState: "idle"}, true, false},
		{"done but hooked", &beads.AgentFields{AgentState: "done", HookBead: "gt-1"}, false, false},
		{"working, no session (a crash)", &beads.AgentFields{AgentState: "working"}, false, false},
		{"no agent bead", nil, false, false},
	}
	for _, tc := range cases {
		if got := recoveryBatchIdle(tc.fields, tc.hasSession); got != tc.want {
			t.Errorf("%s: recoveryBatchIdle = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIdleRecoveryStatus(t *testing.T) {
	t.Parallel()
	fields := &beads.AgentFields{AgentState: "done", CleanupStatus: "clean", Branch: "polecat/slate/gt-1"}
	got := idleRecoveryStatus("gastown", "slate", nil, fields)
	if got.Verdict != "SAFE_TO_NUKE" || !got.Reusable || got.CountsTowardCapacity || got.NeedsRecovery {
		t.Fatalf("idle status = %+v", got)
	}
	if got.Reason != "idle-from-agent-bead" || got.Branch != "polecat/slate/gt-1" {
		t.Fatalf("idle status = %+v", got)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/cmd -run 'TestRecoveryBatchIdle|TestIdleRecoveryStatus' -count=1`
Expected: FAIL: `undefined: recoveryBatchIdle`.

- [ ] **Step 3: Implement the predicate and the report**

```go
// polecat.go, next to runPolecatCheckRecoveryBatch

// recoveryBatchIdle says whether the batch can report a polecat from its
// preloaded agent bead alone: no tmux session, no hooked bead, and an agent
// state that means the seat finished (done) or never started (idle). Every
// other polecat gets the full per-directory check, which is 5-9 bd calls and
// about ten git commands, two of them ls-remote; the batch runs every ten
// minutes over every polecat directory in the town, idle ones included.
func recoveryBatchIdle(fields *beads.AgentFields, hasSession bool) bool {
	if fields == nil || hasSession || fields.HookBead != "" {
		return false
	}
	return fields.AgentState == "done" || fields.AgentState == "idle"
}

// idleRecoveryStatus is the batch's report for a polecat recoveryBatchIdle
// accepted: reusable, counted on no seat, nothing to recover. The branch and
// cleanup status come from the agent bead; the worktree is not probed.
func idleRecoveryStatus(rigName, polecatName string, p *polecat.Polecat, fields *beads.AgentFields) RecoveryStatus {
	st := RecoveryStatus{
		Rig: rigName, Polecat: polecatName,
		CleanupStatus: polecat.CleanupStatus(fields.CleanupStatus),
		Verdict:       "SAFE_TO_NUKE",
		Reason:        "idle-from-agent-bead",
		Reusable:      true,
		SafeToNuke:    true,
		ReuseStatus:   "idle-" + fields.CleanupStatus,
		Branch:        fields.Branch,
		MQStatus:      "not_required",
	}
	if p != nil && p.Branch != "" {
		st.Branch = p.Branch
	}
	return st
}
```

In `runPolecatCheckRecoveryBatch`, after `polecatNames` is listed and before the loop:

```go
	sessions, sessErr := loadPolecatSessionSet(townRegistry(), newPoolSessionLister())
	if sessErr != nil {
		// No session picture: nothing is skipped, every polecat gets the
		// full check, as before.
		fmt.Fprintf(os.Stderr, "warning: cannot list sessions in %s: %v — checking every polecat\n", rigName, sessErr)
		sessions = nil
	}
```

and inside the loop, after `mgr.Get` (so a vanished directory is still dropped) and before `checkRecoveryForPolecat`:

```go
		if sessions != nil {
			fields := recoveryBatchFields(agentBeadsByID, polecatBeadIDForRig(r, rigName, polecatName))
			_, hasSession := sessions.lookup(rigName, polecatName)
			if recoveryBatchIdle(fields, hasSession) {
				statuses = append(statuses, idleRecoveryStatus(rigName, polecatName, p, fields))
				continue
			}
		}
```

with

```go
// recoveryBatchFields parses a preloaded agent bead the way recoveryAgentBead
// does; nil when the batch did not hold the id (a closed agent bead), which
// sends the polecat down the full check.
func recoveryBatchFields(preloaded map[string]*beads.Issue, agentBeadID string) *beads.AgentFields {
	issue, ok := preloaded[agentBeadID]
	if !ok || issue == nil {
		return nil
	}
	fields := beads.ParseAgentFields(issue.Description)
	fields.AgentState = beads.ResolveAgentState(issue.Description, issue.AgentState)
	return fields
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/cmd -run 'TestRecoveryBatch|TestIdleRecovery|TestIntegrationCheckRecovery' -count=1`
Expected: PASS (the integration tests drive `checkRecoveryForPolecat` directly and are unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/cmd/polecat.go internal/cmd/polecat_check_recovery_batch_test.go
git commit -m "check-recovery-batch: report done and idle seats with no session from the preloaded agent bead"
```

---

### Task 4: Install coalescing (bead d)

**Files:**
- Modify: `internal/config/types.go:509-514` (`PatrolConfig`)
- Modify: `internal/daemon/rebuild_gt.go:176-200` (`rebuildGTInterval`, `triggerRebuildGT`), `:255-270` (`requestRebuildGTInstallNow`)
- Test: `internal/daemon/rebuild_gt_test.go`

**Interfaces:**
- Consumes: `evaluatePatrolDue(townRoot, name, time.Time{}, now, interval)` and `savePatrolLastRun`; `d.rebuildGTRequested atomic.Bool`; the test harness `rebuildGTTown(t)`, `withRebuildGTCli`, `watchRebuildGTCycle`, `awaitRebuildGTIdle` (`rebuild_gt_test.go:544-590`).
- Produces: `PatrolConfig.MinInterval string json:"min_interval,omitempty"`; `d.rebuildGTInstallNow atomic.Bool`; `rebuildGTMinInterval(config) time.Duration` (default 30m).

- [ ] **Step 1: Write the failing test**

```go
// rebuild_gt_test.go
func TestRebuildGTCoalescesInstallsWithinMinInterval(t *testing.T) {
	t.Parallel()
	d, _ := rebuildGTTown(t)
	clk := d.clk().(*clockwork.FakeClock) // daemonWithRecorder installs newFixedClock(); see clock_helpers_test.go
	cli := withRebuildGTCli(t, d, func(c cliCall) cliReply {
		if gitSub(c, "branch") {
			return cliReply{stdout: "main\n"}
		}
		if c.name == "bash" {
			return cliReply{stdout: "install-gt: RESULT installed abc1234567890 beefbeef -\n"}
		}
		return cliReply{}
	})
	installs := func() int { return len(installCalls(cli)) }
	// First landing: installs.
	d.requestRebuildGTInstall()
	end := watchRebuildGTCycle(d)
	d.triggerRebuildGT()
	awaitRebuildGTIdle(t, d, end)
	// Second landing ten minutes later: held (the request stays armed).
	clk.Advance(10 * time.Minute)
	d.requestRebuildGTInstall()
	d.triggerRebuildGT()
	if installs() != 1 {
		t.Fatalf("installs after 10 min = %d, want 1 (coalesced)", installs())
	}
	if !d.rebuildGTRequested.Load() {
		t.Fatal("the held request was dropped")
	}
	// gt:install-now bypasses the window.
	d.requestRebuildGTInstallNow()
	end = watchRebuildGTCycle(d)
	d.triggerRebuildGT()
	awaitRebuildGTIdle(t, d, end)
	if installs() != 2 {
		t.Fatalf("installs after install-now = %d, want 2", installs())
	}
	// Thirty minutes after the last install the plain request runs.
	clk.Advance(31 * time.Minute)
	d.requestRebuildGTInstall()
	end = watchRebuildGTCycle(d)
	d.triggerRebuildGT()
	awaitRebuildGTIdle(t, d, end)
	if installs() != 3 {
		t.Fatalf("installs after 31 min = %d, want 3", installs())
	}
}
```

The `RESULT installed` reply is the one `rebuild_gt_test.go:99` already uses so a cycle counts as accomplished and `savePatrolLastRun` records the time the window is measured from. If `d.clk()` is not a `*clockwork.FakeClock` in this fixture, set `d.clock = newFixedClock()` before the first trigger (read `clock.go` and `clock_helpers_test.go` for the field name).

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/daemon -run TestRebuildGTCoalescesInstallsWithinMinInterval -count=1`
Expected: FAIL: `installs after 10 min = 2, want 1`.

- [ ] **Step 3: Implement**

`internal/config/types.go`:

```go
type PatrolConfig struct {
	Enabled  bool     `json:"enabled"`
	Interval string   `json:"interval,omitempty"`
	Agent    string   `json:"agent,omitempty"`
	Rigs     []string `json:"rigs,omitempty"`
	// MinInterval is the least time between two runs a request may force.
	// rebuild_gt reads it: a landing requests an install, and installs
	// closer together than this wait for the window (default 30m); a
	// gt:install-now landing ignores it.
	MinInterval string `json:"min_interval,omitempty"`
}
```

`internal/daemon/rebuild_gt.go`:

```go
// defaultRebuildGTMinInterval is the least time between two installs a
// landing's request may force: 41 installs in one day, each a full go build
// and a daemon restart, were what cut dispatch ticks mid-sling (2026-10-21).
const defaultRebuildGTMinInterval = 30 * time.Minute

func rebuildGTMinInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.RebuildGT != nil {
		if d, err := time.ParseDuration(config.Patrols.RebuildGT.MinInterval); err == nil && d > 0 {
			return d
		}
	}
	return defaultRebuildGTMinInterval
}
```

In `triggerRebuildGT`, replace the due check:

```go
	requested := d.rebuildGTRequested.Load()
	now := d.clk().Now()
	dec := evaluatePatrolDue(d.config.TownRoot, "rebuild_gt", time.Time{}, now, rebuildGTInterval(d.patrolConfig))
	if !dec.due && !requested {
		return
	}
	// A plain request waits out the install window; an install-now one does
	// not. The window is measured from the last accomplished cycle, which
	// savePatrolLastRun records.
	if requested && !d.rebuildGTInstallNow.Load() && !dec.due {
		if last, ok, err := loadPatrolLastRun(d.config.TownRoot, "rebuild_gt"); err == nil && ok && now.Sub(last) < rebuildGTMinInterval(d.patrolConfig) {
			if d.rebuildGTWindowLogged.CompareAndSwap(false, true) {
				d.logger.Printf("rebuild_gt: install requested %s after the last install; waiting for the %s window", now.Sub(last).Round(time.Second), rebuildGTMinInterval(d.patrolConfig))
			}
			return
		}
	}
	d.rebuildGTWindowLogged.Store(false)
```

`loadPatrolLastRun(townRoot, patrol string) (time.Time, bool, error)` is at `patrol_last_run.go:44`. Add `rebuildGTInstallNow atomic.Bool` and `rebuildGTWindowLogged atomic.Bool` to `Daemon`; `requestRebuildGTInstallNow` sets `rebuildGTInstallNow.Store(true)` before arming the request; the cycle goroutine clears it with `d.rebuildGTInstallNow.Store(false)` right after `armed := d.rebuildGTRequested.Swap(false)`.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/daemon -run 'TestRebuildGT' -count=1 && go test ./internal/config -count=1`
Expected: PASS, including the strict-decoding config tests (the new key is optional).

- [ ] **Step 5: Commit**

```bash
git add internal/config/types.go internal/daemon/rebuild_gt.go internal/daemon/rebuild_gt_test.go
git commit -m "rebuild_gt: a landing's install request waits out a 30 minute window; gt:install-now does not"
```

---

### Task 5: Bond writes in few statements (bead e, beads rig)

**Files:** (beads repo, `~/gt/beads`; read from its `origin/main`)
- Modify: `cmd/bd/template.go:650-760` (`cloneSubgraphInto`) and the store method(s) it calls per row
- Test: the store's test package next to the dolt store tests

**Interfaces:**
- Consumes: `transact(..., "bd: clone template subgraph", ...)` (`template.go:654`), the regular and ignored-table transactions (`internal/storage/dolt/transaction.go:194-284`).
- Produces: a multi-row insert path for wisps and wisp dependencies inside the one bond transaction; a statement counter in the test.

_This task's concrete steps are filled in from the beads lookup (facts-1 report: the per-row insert calls, any existing batch method, the test server helper and statement-count tooling). Until then the task is not filed._

---

### Task 6: Tick snapshot and parallel slings (crew session; not polecat-sized)

**Files:**
- Modify: `internal/cmd/spec.go:858-1120` (`runSpecDispatchCycle`), `:1493-1600` (`runSpecDispatchAt`, `env.Sling`), `:1957` (`specSlingParams`)
- Modify: `internal/cmd/sling_pool.go:820-880` (`poolSeatLedger.begin`), `:1120-1145` (`poolRouterForWith`, from task 1), `:1217-1219` (`route`'s `occupied` closure)
- Modify: `internal/cmd/polecat_spawn.go:728-741` (`realSlingSeatSpawn`)
- Test: `internal/cmd/spec_test.go`, `internal/cmd/sling_pool_test.go`

**Interfaces:**
- Consumes: task 1's `poolRouterForWith`; `specRosterSessions` (the batched roster, `spec.go:1603-1618`); `specdispatch.RetryOnContention`; the test env fake in `spec_test.go:160-260` (its `Sling` func records `f.slung`).
- Produces: `type specOccupancy struct{ sessions []poolSession; at time.Time }`; `specDispatchEnv.Occupancy func() (*specOccupancy, error)`; `specDispatchEnv.Sling(c specCandidate, resumeBranch string, seat specdispatch.SeatChoice, occ *specOccupancy) (string, error)`; `poolRouter.routeWith(requested string, live bool, occupied func() ([]poolSession, error)) (agent, reason string, err error)`; `specDispatchEnv.Parallel int` (0 = free seats).

Steps (each with its own test cycle; commit after each):

- [ ] **Step 1: Snapshot type and roster reuse.** In `runSpecDispatchAt`, read the roster once into a `specOccupancy` and build `Roster` from it (today `Roster` re-reads through `specRosterSessions`). Test: the fake env's `Roster` is called once per tick; `reads.Calls()` for a tick with 2 rigs does not grow when the candidate count grows.

- [ ] **Step 2: `routeWith` on the router.** Extract the body of `route(requested, live)` into `routeWith(requested, live, occupied)`; `route` calls it with the router's own `occupied` closure. `begin` is unchanged: it still takes the lock, runs `cleanupStale`, calls `occupied()` and appends the claims. Test: `routeWith` given a canned `occupied` that lists 2 live seats on `claude-haiku` with cap 3 answers `claude-haiku`; given 3 it refuses with the cap reason; with the fake claim store a second concurrent `routeWith` for the same seat sees the first's claim and takes the next seat.

- [ ] **Step 3: Dispatcher sling takes the snapshot.** `slingSpec(townRoot, c, resume, seat)` grows an `occ *specOccupancy`; `realSlingSeatSpawn` gets a variant whose `resolvePool` calls `poolRouterForWith(...).routeWith(requested, true, func() ([]poolSession, error) { return occ.sessions, nil })`. A nil `occ` keeps today's full read (hand-run `gt sling`). Test: through the fake store, a dispatched sling makes no agent-bead or assigned-work reads (`reads.Calls()` unchanged by the sling except the pre-sling `showFresh` and the writes).

- [ ] **Step 4: Collect, then sling concurrently.** Split the loop in `runSpecDispatchCycle` at the `showFresh` re-read: the first pass does everything up to and including `ChooseSeat`, `BranchOnOrigin` and the shape annotation, appending a `pendingSling{candidate, seat, resume, gone, shapeNote}` until `len(pending) == parallel` where `parallel` is `env.Parallel` if > 0 else the roster's free seats (`budget` knows the caps and live counts; add `specdispatch.Budget.Free()` returning cap-minus-live summed over open seats, bounded by `max_total`). The second pass runs each pending sling in its own goroutine (`sync.WaitGroup`, results into a slice indexed by position), each doing `showFresh`, `Eligible`, `RetryOnContention(env.Sling(..., occ))`, `AddLabel` and the rollback exactly as the serial code does; `budget.Bump` happens before the goroutines start, in candidate order, so the seat choices are the ones the snapshot supports. After the wait, append `Dispatched`/`Failed`/`Skipped` entries in candidate order. Tests (fake env, `go test -race`): 9 free seats and 12 candidates sling 9 in one tick, `f.slung` has 9 distinct ids and no seat over its cap; a sling that returns an error after `RetryAttempts` leaves its seat free and is listed under `Failed`; `Parallel: 2` slings 2; `PerTick: 2` (the config ceiling) slings 2 even with 5 free seats; dry run lists "would sling" for `free seats` candidates.

- [ ] **Step 5: Config ceiling semantics.** `readSpecDispatchConfig`'s `MaxPerTick` 0 or absent means "free seats"; `PerTick` in the env is set from it; `defaultSpecMaxPerTick` becomes 0. Test: a patrol block without `max_per_tick` gives `PerTick: 0` and the cycle slings up to the free seats.

- [ ] **Step 6: Log lines.** The tick line keeps its shape; the cost note becomes `tick cost: N bd read call(s) outside the sling path in Ts; M sling(s) in parallel, slowest Ss`. Test: the note names the parallel count.

- [ ] **Step 7: Land.** `make presubmit` in the crew worktree, `gt done` on `crew/sloan/dispatch`, then remove `max_per_tick: 5` from `~/gt/settings/config.json` (Sloan's word first) and record the first post-landing tick numbers on claude-rh7.

---

## Filing

| Task | Rig | Depends on | Title |
|---|---|---|---|
| 1 | gastown | — | sling: read the seat picture through the batched reads (two bd calls per rig) |
| 2 | gastown | 1 | pool: release a parked, sessionless seat after 30 minutes; crash detection logs the release |
| 3 | gastown | 1 | check-recovery-batch: report done/idle seats with no session from the preloaded agent bead |
| 4 | gastown | 2 | rebuild_gt: coalesce landing-requested installs to one per 30 minutes; gt:install-now bypasses |
| 5 | beads | — | bd mol bond --ephemeral: write the molecule in few statements |
| 6 | gastown (crew) | 1 | spec dispatch: tick snapshot into slings, slings in parallel up to free seats |

Each bead: `## Goal`, `## Constraints` (files above; no behaviour change beyond the task), `## Out of scope` (the other tasks), `## Gate` (`go test ./internal/cmd ./internal/daemon -count=1`; the owner's live measurement is recorded on claude-rh7 after the landing), `## Size` (one worker, one landing), `## Acceptance` (the task's tests by name, passing).
