# Polecat Worktree Cleanup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The daemon's `patrol_scan` tick removes polecat worktrees that can never be reused and hold nothing live, and surfaces every other leftover instead of forcing it.

**Architecture:** A new per-seat `reap` pass in `internal/patrolscan` (pure, behind an optional `ReapEnv` interface), run after the `seat()` pass and before `orphans()`. The daemon host implements `ReapEnv` by shelling out to `gt polecat check-recovery --json` (the safety verdict) and `gt polecat nuke` (never `--force`). Config is a nested `worktree_cleanup` block, default off and dry-run.

**Tech Stack:** Go; `internal/patrolscan`, `internal/daemon`, `internal/config`, `internal/cmd` (one exit code).

**Spec:** `docs/plans/2026-10-03-polecat-worktree-cleanup-design.md` (read its "Revision 1": it supersedes the spec's Env section). Handoff bead claude-0bg.

## Global Constraints

- Read code from `origin/main` only (`git show origin/main:<path>`); work in a worktree cut from `origin/main` after `git fetch origin`.
- Default off, dry-run on: `enabled=false`, `dry_run=true`. A missing or zero `max_per_tick` means 2, never unlimited.
- Removal is `gt polecat nuke <rig>/<name>` and NEVER passes `--force`.
- A failed read is `unknown` and never acts. Safe means `safe_to_nuke` AND `verdict=="SAFE_TO_NUKE"` AND `git_state_source=="live"`.
- Reusable seats are never reaped. Frozen seats are never reaped. Parked grace 24h, ordinary grace 30m.
- Never pass `--reconcile-cleanup` to `check-recovery`.
- Tests are hermetic: no live tmux socket, no live Dolt (gt-yav3).
- Gate: `make gate`. Attribution grep (`co-authored|anthropic|generated with`) must be 0.
- Rig scope: gastown only (`rigs` defaults to `["gastown"]`).

## Decisions made while planning (confirm at plan review)

1. `check-recovery --json` is the safety oracle (spec Revision 1), not a daemon-side rebuild.
2. `ReapEnv` is a separate optional interface, so slices 1 and 2 are independent and slice 1 builds alone.
3. A seat inside its grace emits no line (quiet), unlike the spec's `waiting` finding.
4. `gt polecat nuke` gets a distinct exit code for a safety refusal (slice 3).
5. The spec's standalone "verify" slice dissolved: escalate dedupe, config home and `Held()` were verified from source; the grace-clock source is resolved inside slice 3.

## Slice graph

```
S1 pure reap pass (patrolscan) ─┐
S2 config block (config)        ├─> S4 host ReapEnv + wiring (daemon) ─> S5 alerts (daemon)
S3 nuke refusal exit code (cmd) ┘
```

S1, S2, S3 are independent and parallel (three different packages). S4 needs all three. S5 needs S4 and touches `internal/daemon/patrol_scan.go`, so it chains after S4.

## File structure

| File | Slice | Responsibility |
|------|-------|----------------|
| `internal/patrolscan/reap.go` (new) | S1 | `ReapEnv`, `Recovery`, `ReapOptions`, `reap()` decision |
| `internal/patrolscan/scan.go` | S1 | `Options.Reap`, `Scanner.reapEnv`, `Tick` call, 3 outcomes, summary counters |
| `internal/patrolscan/reap_test.go` (new) | S1 | `fakeReapEnv`, veto/fail-closed/grace/cap/dry-run tests |
| `internal/config/daemon_config.go` | S2 | `WorktreeCleanupConfig` + defaults |
| `internal/cmd/polecat.go` | S3 | safety-refusal exit code |
| `internal/daemon/patrol_scan_reap.go` (new) | S4 | host `ReapEnv`: exec, JSON parse, `IdleSince`, `BranchClaimed` |
| `internal/daemon/patrol_scan.go` | S4, S5 | options mapping (S4); alerts in `runPatrolScan` (S5) |

---

### Task 1 (slice S1): pure reap pass in patrolscan

**Bead:** `Add a reap pass to patrolscan: decide which finished polecat seats may be removed`

**Files:**
- Create: `internal/patrolscan/reap.go`, `internal/patrolscan/reap_test.go`
- Modify: `internal/patrolscan/scan.go` (Options, Scanner, New, Tick, outcomes, `Report.Lines`), and any existing test asserting the exact summary line (`grep -n "error(s)" internal/patrolscan/*_test.go internal/daemon/*_test.go`)

**Interfaces:**
- Produces (S4 implements these; names are exact):

```go
type ReapEnv interface {
	Recovery(rig, polecat string) (Recovery, error)
	IdleSince(rig, polecat string) (time.Time, error)
	BranchClaimed(rig, branch string) (string, error)
	Reap(rig, polecat string) error
}

type Recovery struct {
	Verdict        string // SAFE_TO_NUKE, PENDING_MR, SUBMITTED, NEEDS_RECOVERY, NEEDS_MQ_SUBMIT, WORKING
	Reusable       bool
	SafeToNuke     bool
	Reason         string
	Branch         string
	Issue          string
	ActiveMR       string
	Blockers       []string
	GitStateSource string // live, unknown, recorded
}

type ReapOptions struct {
	DryRun      bool
	Grace       time.Duration // default 30m
	ParkedGrace time.Duration // default 24h
	MaxPerTick  int           // default 2
}
```
- `Options.Reap *ReapOptions` (nil = off). Outcomes `OutcomeReaped="reaped"`, `OutcomeWouldReap="would-reap"`, `OutcomeBlocked="blocked"`.

- [ ] **Step 1: Read first.** `sed -n 20,75p internal/patrolscan/scan_test.go` (the `fakeEnv`), `sed -n 205,226p` (`scanner`, the shared `now`), and `internal/intent/intent.go` (`Record.Frozen`, `Desired`, `DesiredPark`, `DesiredSubmitted`). Confirm `fakeEnv.Intent` and `SessionExists` return zero values for an unset seat.

- [ ] **Step 2: Write the failing test** in `internal/patrolscan/reap_test.go`:

```go
package patrolscan

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/intent"
)

// fakeReapEnv adds the ReapEnv half to the shared fakeEnv.
type fakeReapEnv struct {
	*fakeEnv
	recovery    map[string]Recovery
	recoveryErr map[string]error
	idleSince   map[string]time.Time
	idleErr     map[string]error
	claimed     map[string]string // branch -> bead
	claimErr    map[string]error
	reapErr     map[string]error
	reaped      []string
}

func (f *fakeReapEnv) Recovery(_, p string) (Recovery, error) { return f.recovery[p], f.recoveryErr[p] }
func (f *fakeReapEnv) IdleSince(_, p string) (time.Time, error) {
	return f.idleSince[p], f.idleErr[p]
}
func (f *fakeReapEnv) BranchClaimed(_, b string) (string, error) { return f.claimed[b], f.claimErr[b] }
func (f *fakeReapEnv) Reap(_, p string) error {
	if err := f.reapErr[p]; err != nil {
		return err
	}
	f.reaped = append(f.reaped, p)
	return nil
}

// eligible returns an env with one seat, "onyx", that passes every veto and is
// safe to remove: idle an hour, parked-free, SAFE_TO_NUKE measured live.
func eligible() *fakeReapEnv {
	base := newFake()
	base.polecats = []string{"onyx"}
	return &fakeReapEnv{
		fakeEnv:     base,
		recovery:    map[string]Recovery{"onyx": {Verdict: "SAFE_TO_NUKE", SafeToNuke: true, GitStateSource: "live", Branch: "polecat/onyx/gt-1"}},
		recoveryErr: map[string]error{},
		idleSince:   map[string]time.Time{"onyx": now.Add(-time.Hour)},
		idleErr:     map[string]error{},
		claimed:     map[string]string{},
		claimErr:    map[string]error{},
		reapErr:     map[string]error{},
	}
}

func reapScanner(env *fakeReapEnv, o ReapOptions) *Scanner {
	return New(env, memLedger{}, Options{
		Now:       func() time.Time { return now },
		IsRefusal: func(err error) bool { return errors.Is(err, errRefused) },
		Reap:      &o,
	})
}

func reapFinding(t *testing.T, r Report, p string) (Finding, bool) {
	t.Helper()
	for _, f := range r.Findings {
		if f.Kind == "reap" && f.Subject == p {
			return f, true
		}
	}
	return Finding{}, false
}

func TestReapEligibleSeatIsRemoved(t *testing.T) {
	env := eligible()
	r := reapScanner(env, ReapOptions{}).Tick("gastown")
	f, ok := reapFinding(t, r, "onyx")
	if !ok || f.Outcome != OutcomeReaped {
		t.Fatalf("finding = %+v, %v; want reaped", f, ok)
	}
	if len(env.reaped) != 1 || env.reaped[0] != "onyx" {
		t.Fatalf("reaped = %v, want [onyx]", env.reaped)
	}
}

func TestReapDryRunNeverRemoves(t *testing.T) {
	env := eligible()
	r := reapScanner(env, ReapOptions{DryRun: true}).Tick("gastown")
	if f, ok := reapFinding(t, r, "onyx"); !ok || f.Outcome != OutcomeWouldReap {
		t.Fatalf("finding = %+v, %v; want would-reap", f, ok)
	}
	if len(env.reaped) != 0 {
		t.Fatalf("dry-run removed %v", env.reaped)
	}
}

// Each row breaks exactly one fact of the eligible baseline. quiet means no
// reap finding at all; otherwise the outcome must match. Reap is never called.
func TestReapVetoes(t *testing.T) {
	boom := errors.New("read failed")
	rows := []struct {
		name   string
		mutate func(*fakeReapEnv)
		quiet  bool
		want   Outcome
	}{
		{"intent unreadable", func(e *fakeReapEnv) { e.intentErr = map[string]error{"onyx": boom} }, false, OutcomeUnknown},
		{"frozen", func(e *fakeReapEnv) { e.intents = map[string]intent.Record{"onyx": {Frozen: true}} }, true, ""},
		{"submitted", func(e *fakeReapEnv) { e.intents = map[string]intent.Record{"onyx": {Desired: intent.DesiredSubmitted}} }, true, ""},
		{"live session", func(e *fakeReapEnv) { e.sessions = map[string]bool{"onyx": true} }, true, ""},
		{"session read fails", func(e *fakeReapEnv) { e.sessionErr = map[string]error{"onyx": boom} }, false, OutcomeUnknown},
		{"fresh heartbeat", func(e *fakeReapEnv) {
			e.heartbeats = map[string]*Heartbeat{"onyx": {State: "working", At: now.Add(-time.Minute)}}
		}, true, ""},
		{"assigned bead", func(e *fakeReapEnv) { e.work = map[string]*Work{"onyx": {ID: "gt-9", Status: "in_progress"}} }, true, ""},
		{"assigned bead unreadable", func(e *fakeReapEnv) { e.workErr = map[string]error{"onyx": boom} }, false, OutcomeUnknown},
		{"recovery unreadable", func(e *fakeReapEnv) { e.recoveryErr["onyx"] = boom }, false, OutcomeUnknown},
		{"branch claimed by open bead", func(e *fakeReapEnv) { e.claimed["polecat/onyx/gt-1"] = "gt-2" }, false, OutcomeSkipped},
		{"branch claim unreadable", func(e *fakeReapEnv) { e.claimErr["polecat/onyx/gt-1"] = boom }, false, OutcomeUnknown},
		{"open MR", func(e *fakeReapEnv) { e.recovery["onyx"] = Recovery{Verdict: "PENDING_MR", GitStateSource: "live"} }, true, ""},
		{"working verdict", func(e *fakeReapEnv) { e.recovery["onyx"] = Recovery{Verdict: "WORKING", GitStateSource: "live"} }, true, ""},
		{"reusable seat is capacity", func(e *fakeReapEnv) {
			e.recovery["onyx"] = Recovery{Verdict: "SAFE_TO_NUKE", SafeToNuke: true, Reusable: true, GitStateSource: "live"}
		}, true, ""},
		{"idle-since unknown", func(e *fakeReapEnv) { e.idleSince["onyx"] = time.Time{} }, false, OutcomeSkipped},
		{"idle-since unreadable", func(e *fakeReapEnv) { e.idleErr["onyx"] = boom }, false, OutcomeUnknown},
		{"inside grace", func(e *fakeReapEnv) { e.idleSince["onyx"] = now.Add(-29 * time.Minute) }, true, ""},
		{"parked inside parked grace", func(e *fakeReapEnv) {
			e.intents = map[string]intent.Record{"onyx": {Desired: intent.DesiredPark}}
			e.idleSince["onyx"] = now.Add(-23 * time.Hour)
		}, true, ""},
		{"git state only recorded", func(e *fakeReapEnv) {
			e.recovery["onyx"] = Recovery{Verdict: "SAFE_TO_NUKE", SafeToNuke: true, GitStateSource: "recorded"}
		}, false, OutcomeBlocked},
		{"git state unknown", func(e *fakeReapEnv) {
			e.recovery["onyx"] = Recovery{Verdict: "SAFE_TO_NUKE", SafeToNuke: true, GitStateSource: "unknown"}
		}, false, OutcomeBlocked},
		{"needs recovery", func(e *fakeReapEnv) {
			e.recovery["onyx"] = Recovery{Verdict: "NEEDS_RECOVERY", Blockers: []string{"3 unpushed commits"}, GitStateSource: "live"}
		}, false, OutcomeBlocked},
		{"needs mq submit", func(e *fakeReapEnv) {
			e.recovery["onyx"] = Recovery{Verdict: "NEEDS_MQ_SUBMIT", GitStateSource: "live"}
		}, false, OutcomeBlocked},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			env := eligible()
			row.mutate(env)
			r := reapScanner(env, ReapOptions{}).Tick("gastown")
			f, ok := reapFinding(t, r, "onyx")
			if row.quiet && ok {
				t.Fatalf("want no reap finding, got %+v", f)
			}
			if !row.quiet && (!ok || f.Outcome != row.want) {
				t.Fatalf("finding = %+v, %v; want %s", f, ok, row.want)
			}
			if len(env.reaped) != 0 {
				t.Fatalf("Reap called for %v", env.reaped)
			}
		})
	}
}

func TestReapGraceBoundaries(t *testing.T) {
	cases := []struct {
		name   string
		parked bool
		idle   time.Duration
		reaped bool
	}{
		{"ordinary 31m", false, 31 * time.Minute, true},
		{"parked 25h", true, 25 * time.Hour, true},
		{"parked 31m is not enough", true, 31 * time.Minute, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := eligible()
			env.idleSince["onyx"] = now.Add(-c.idle)
			if c.parked {
				env.intents = map[string]intent.Record{"onyx": {Desired: intent.DesiredPark}}
			}
			reapScanner(env, ReapOptions{}).Tick("gastown")
			if got := len(env.reaped) == 1; got != c.reaped {
				t.Fatalf("reaped = %v, want %v", env.reaped, c.reaped)
			}
		})
	}
}

func TestReapPerTickCap(t *testing.T) {
	env := eligible()
	env.polecats = []string{"a", "b", "c", "d", "e"}
	for _, p := range env.polecats {
		env.recovery[p] = Recovery{Verdict: "SAFE_TO_NUKE", SafeToNuke: true, GitStateSource: "live"}
		env.idleSince[p] = now.Add(-time.Hour)
	}
	reapScanner(env, ReapOptions{}).Tick("gastown") // MaxPerTick unset => 2
	if len(env.reaped) != 2 {
		t.Fatalf("reaped %v, want exactly 2 (default cap)", env.reaped)
	}
	env2 := eligible()
	env2.polecats = env.polecats
	env2.recovery, env2.idleSince = env.recovery, env.idleSince
	reapScanner(env2, ReapOptions{MaxPerTick: 4}).Tick("gastown")
	if len(env2.reaped) != 4 {
		t.Fatalf("reaped %v, want 4", env2.reaped)
	}
}

func TestReapNukeRefusalIsBlockedNotFailed(t *testing.T) {
	env := eligible()
	env.reapErr["onyx"] = errRefused
	r := reapScanner(env, ReapOptions{}).Tick("gastown")
	if f, _ := reapFinding(t, r, "onyx"); f.Outcome != OutcomeBlocked {
		t.Fatalf("outcome = %s, want blocked", f.Outcome)
	}
	env = eligible()
	env.reapErr["onyx"] = errors.New("exec failed")
	r = reapScanner(env, ReapOptions{}).Tick("gastown")
	if f, _ := reapFinding(t, r, "onyx"); f.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %s, want failed", f.Outcome)
	}
}

// The Q1 line, pinned: today's town has 12 done, clean, reuse-eligible seats.
// None of them may be reaped or reported as would-reap.
func TestReapNeverTouchesReusableDoneSeats(t *testing.T) {
	env := eligible()
	env.polecats = nil
	for _, p := range []string{"agate", "basalt", "malachite", "marble", "mica", "pearl", "pyrite", "sapphire", "shale", "slate", "topaz", "turquoise"} {
		env.polecats = append(env.polecats, p)
		env.recovery[p] = Recovery{Verdict: "SAFE_TO_NUKE", SafeToNuke: true, Reusable: true, GitStateSource: "live"}
		env.idleSince[p] = now.Add(-72 * time.Hour)
	}
	for _, dry := range []bool{true, false} {
		r := reapScanner(env, ReapOptions{DryRun: dry}).Tick("gastown")
		if n := r.Count(OutcomeReaped) + r.Count(OutcomeWouldReap) + r.Count(OutcomeBlocked); n != 0 {
			t.Fatalf("dry=%v: %d reap findings on reusable seats: %+v", dry, n, r.Findings)
		}
	}
	if len(env.reaped) != 0 {
		t.Fatalf("reaped reusable seats: %v", env.reaped)
	}
}

func TestReapOffWhenOptionOrEnvMissing(t *testing.T) {
	env := eligible()
	New(env, memLedger{}, Options{Now: func() time.Time { return now }}).Tick("gastown") // Reap nil
	if len(env.reaped) != 0 {
		t.Fatalf("reaped with Options.Reap nil: %v", env.reaped)
	}
	// A plain fakeEnv has no ReapEnv half: the tick must still run.
	plain := newFake()
	plain.polecats = []string{"onyx"}
	o := ReapOptions{}
	New(plain, memLedger{}, Options{Now: func() time.Time { return now }, Reap: &o}).Tick("gastown")
}

func TestSummaryLineCountsReapOutcomes(t *testing.T) {
	r := Report{Rig: "gastown", Findings: []Finding{
		{Kind: "reap", Outcome: OutcomeReaped},
		{Kind: "reap", Outcome: OutcomeWouldReap},
		{Kind: "reap", Outcome: OutcomeBlocked},
		{Kind: "reap", Outcome: OutcomeBlocked},
	}}
	want := "1 reaped, 1 would-reap, 2 blocked"
	if got := r.Lines()[0]; !strings.Contains(got, want) {
		t.Fatalf("summary %q lacks %q", got, want)
	}
}
```

- [ ] **Step 3: Run it to confirm it fails to compile.**
Run: `go test ./internal/patrolscan -run 'TestReap|TestSummaryLineCountsReap' -count=1`
Expected: FAIL, `undefined: ReapOptions` / `ReapEnv` / `OutcomeReaped`.

- [ ] **Step 4: Add the types and wiring to `scan.go`.** In `Options` add `Reap *ReapOptions`. In `Scanner` add `reapEnv ReapEnv`. In `New`, after the existing defaults:

```go
	s := &Scanner{env: env, ledger: ledger, o: o}
	if o.Reap != nil {
		r := *o.Reap
		if r.Grace <= 0 {
			r.Grace = DefaultReapGrace
		}
		if r.ParkedGrace <= 0 {
			r.ParkedGrace = DefaultReapParkedGrace
		}
		if r.MaxPerTick <= 0 {
			r.MaxPerTick = DefaultReapMaxPerTick
		}
		s.o.Reap = &r
		s.reapEnv, _ = env.(ReapEnv)
	}
	return s
```
(replacing the existing `return &Scanner{...}`). Add the three outcomes to the `Outcome` const block, and in `Report.Lines` append `, %d reaped, %d would-reap, %d blocked` to the summary format with `r.Count(OutcomeReaped), r.Count(OutcomeWouldReap), r.Count(OutcomeBlocked)`. In `Tick`, after the `seat()` loop and before `s.orphans(rig, &r)`:

```go
		left := 0
		if s.reapEnv != nil {
			left = s.o.Reap.MaxPerTick
		}
		for _, name := range polecats {
			if f, ok := s.reap(rig, name, &left); ok {
				r.Findings = append(r.Findings, f)
			}
		}
```

- [ ] **Step 5: Write `internal/patrolscan/reap.go`:**

```go
package patrolscan

import (
	"fmt"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/intent"
)

// Reap defaults (design: docs/plans/2026-10-03-polecat-worktree-cleanup-design.md).
const (
	DefaultReapGrace       = 30 * time.Minute
	DefaultReapParkedGrace = 24 * time.Hour
	DefaultReapMaxPerTick  = 2
)

// ReapEnv is the optional half of Env the reap pass needs. A host that does not
// implement it never reaps.
type ReapEnv interface {
	// Recovery returns the verdict of `gt polecat check-recovery --json`.
	Recovery(rig, polecat string) (Recovery, error)
	// IdleSince is when the seat last did anything; the grace clock. A zero
	// time with no error means unknown.
	IdleSince(rig, polecat string) (time.Time, error)
	// BranchClaimed returns the ID of a non-terminal bead whose notes name
	// branch (including resume_branch), "" for none.
	BranchClaimed(rig, branch string) (string, error)
	// Reap removes the seat through `gt polecat nuke`, never with --force.
	Reap(rig, polecat string) error
}

// Recovery is the slice of the check-recovery verdict the reap pass reads.
type Recovery struct {
	Verdict        string
	Reusable       bool
	SafeToNuke     bool
	Reason         string
	Branch         string
	Issue          string
	ActiveMR       string
	Blockers       []string
	GitStateSource string
}

// ReapOptions tunes the reap pass.
type ReapOptions struct {
	DryRun      bool
	Grace       time.Duration
	ParkedGrace time.Duration
	MaxPerTick  int
}

// reap decides whether one seat is removed. Vetoes run cheapest first; the
// first to fire wins. A failed read is Unknown and never acts. A quiet veto
// (someone else owns the seat, or it is reusable capacity) reports nothing.
// left is the removals still allowed this tick.
func (s *Scanner) reap(rig, name string, left *int) (Finding, bool) {
	if s.reapEnv == nil || s.o.Reap == nil {
		return Finding{}, false
	}
	f := Finding{Kind: "reap", Subject: name}
	unknown := func(why string, err error) (Finding, bool) {
		f.Outcome, f.Detail = OutcomeUnknown, why+": "+err.Error()
		return f, true
	}

	rec, err := s.env.Intent(rig, name)
	if err != nil {
		return unknown("intent record unreadable", err)
	}
	if rec.Frozen || rec.Submitted() {
		return Finding{}, false // frozen is the supervisor's, submitted is the landing worker's
	}
	parked := rec.Desired == intent.DesiredPark

	up, err := s.env.SessionExists(rig, name)
	if err != nil {
		return unknown("session unreadable", err)
	}
	now := s.o.Now()
	if up {
		return Finding{}, false
	}
	if hb := s.env.Heartbeat(rig, name); hb != nil && now.Sub(hb.At) < s.o.HeartbeatFresh {
		return Finding{}, false
	}
	work, err := s.env.AssignedWork(rig, name)
	if err != nil {
		return unknown("assigned work unreadable", err)
	}
	if work != nil && !strings.EqualFold(work.Status, "closed") {
		return Finding{}, false // the seat pass owns a seat with live work
	}

	rv, err := s.reapEnv.Recovery(rig, name)
	if err != nil {
		return unknown("recovery verdict unreadable", err)
	}
	if rv.Branch != "" {
		bead, err := s.reapEnv.BranchClaimed(rig, rv.Branch)
		if err != nil {
			return unknown("branch claim unreadable", err)
		}
		if bead != "" {
			f.Outcome, f.Detail = OutcomeSkipped, fmt.Sprintf("branch %s is claimed by %s", rv.Branch, bead)
			return f, true
		}
	}
	switch rv.Verdict {
	case "WORKING", "SUBMITTED", "PENDING_MR":
		return Finding{}, false // someone owns it
	}
	if rv.Reusable {
		return Finding{}, false // reusable capacity is never reaped
	}

	since, err := s.reapEnv.IdleSince(rig, name)
	if err != nil {
		return unknown("idle-since unreadable", err)
	}
	if since.IsZero() {
		f.Outcome, f.Detail = OutcomeSkipped, "idle-since unknown; not eligible"
		return f, true
	}
	grace := s.o.Reap.Grace
	if parked {
		grace = s.o.Reap.ParkedGrace
	}
	if now.Sub(since) < grace {
		return Finding{}, false // quiet: a line per seat per tick would flood the log
	}

	safe := rv.SafeToNuke && rv.Verdict == "SAFE_TO_NUKE" && rv.GitStateSource == "live"
	if !safe {
		f.Outcome, f.Detail = OutcomeBlocked, blockedDetail(rv)
		return f, true
	}
	if s.o.Reap.DryRun {
		f.Outcome = OutcomeWouldReap
		f.Detail = fmt.Sprintf("idle %s, %s, git state live", now.Sub(since).Round(time.Minute), rv.Verdict)
		return f, true
	}
	if *left <= 0 {
		f.Outcome, f.Detail = OutcomeSkipped, "per-tick removal cap reached"
		return f, true
	}
	if err := s.reapEnv.Reap(rig, name); err != nil {
		if s.o.IsRefusal(err) {
			f.Outcome, f.Detail = OutcomeBlocked, "nuke refused: "+err.Error()
			return f, true
		}
		f.Outcome, f.Detail = OutcomeFailed, err.Error()
		return f, true
	}
	*left--
	f.Outcome, f.Detail = OutcomeReaped, fmt.Sprintf("idle %s, %s, git state live", now.Sub(since).Round(time.Minute), rv.Verdict)
	return f, true
}

// blockedDetail names why a seat is not safe: the verdict, its blockers, and a
// git state that was not measured live.
func blockedDetail(rv Recovery) string {
	parts := []string{"verdict " + rv.Verdict}
	if rv.GitStateSource != "live" {
		parts = append(parts, "git state "+orUnknown(rv.GitStateSource)+" (not measured live)")
	}
	if len(rv.Blockers) > 0 {
		parts = append(parts, strings.Join(rv.Blockers, "; "))
	} else if rv.Reason != "" {
		parts = append(parts, rv.Reason)
	}
	return strings.Join(parts, ": ")
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
```

- [ ] **Step 6: Run tests, update affected summary-line assertions.**
Run: `go test ./internal/patrolscan -count=1 && go vet ./internal/patrolscan ./internal/daemon`
Expected: PASS. If an existing test asserts the old summary line, append `, 0 reaped, 0 would-reap, 0 blocked` to its expected string.

- [ ] **Step 7: Commit** on a branch cut from `origin/main`:
`git add internal/patrolscan && git commit -m "patrolscan: add a fail-closed reap pass for finished polecat seats (off unless Options.Reap is set)"`

**Acceptance (bead):**
- [ ] `go test ./internal/patrolscan -run 'TestReap|TestSummaryLineCountsReap' -count=1` passes, including the 12-reusable-seats regression and the veto table
- [ ] With `Options.Reap` nil, or an `Env` that is not a `ReapEnv`, a tick reaps nothing and reports no reap findings
- [ ] Every erroring read returns `unknown` and `Reap` is never called (veto table rows)
- [ ] `Report.Lines()[0]` ends with `N reaped, M would-reap, K blocked`
- [ ] `make gate` passes; no file outside `internal/patrolscan` changes

**Out of scope:** the host implementation, config, alerts, any change to `seat()`.

---

### Task 2 (slice S2): `worktree_cleanup` config block

**Bead:** `Add the patrol_scan.worktree_cleanup config block with fail-safe defaults`

**Files:**
- Modify: `internal/config/daemon_config.go` (add to `PatrolScanConfig`, `:341` on main)
- Test: the existing config test file for `PatrolScanConfig` (`git grep -ln "PatrolScanConfig" -- 'internal/config/*_test.go'`); create `internal/config/worktree_cleanup_test.go` if none

**Interfaces:**
- Produces: `PatrolScanConfig.WorktreeCleanup *WorktreeCleanupConfig`; methods used by S4:

```go
type WorktreeCleanupConfig struct {
	Enabled               bool     `json:"enabled"`
	DryRun                *bool    `json:"dry_run,omitempty"` // nil => true
	Rigs                  []string `json:"rigs,omitempty"`    // empty => ["gastown"]
	GraceStr              string   `json:"grace,omitempty"`        // default "30m"
	ParkedGraceStr        string   `json:"parked_grace,omitempty"` // default "24h"
	MaxPerTick            int      `json:"max_per_tick,omitempty"` // <=0 => 2
	BlockedAlertThreshold int      `json:"blocked_alert_threshold,omitempty"` // <=0 => 5
}
func (c *WorktreeCleanupConfig) IsEnabled() bool      // nil-safe; false for nil
func (c *WorktreeCleanupConfig) IsDryRun() bool       // nil-safe; true unless DryRun is explicitly false
func (c *WorktreeCleanupConfig) CoversRig(rig string) bool
func (c *WorktreeCleanupConfig) Grace() time.Duration
func (c *WorktreeCleanupConfig) ParkedGrace() time.Duration
func (c *WorktreeCleanupConfig) Cap() int
func (c *WorktreeCleanupConfig) AlertThreshold() int
```
`DryRun` is a `*bool` on purpose: a plain bool cannot tell "unset" (must default to true) from "false".

- [ ] **Step 1: Read first.** `git show origin/main:internal/townconfig/townconfig.go | sed -n 1,60p` and find how `mayor/town.json` is validated: does it reject unknown keys under `patrols.patrol_scan`? If a strict schema exists, add the new keys there in this slice; if decoding is lenient, add a test proving the block round-trips through `townconfig.Load`.
- [ ] **Step 2: Write failing tests:** a nil block is disabled and dry-run; `{"enabled":true}` alone is dry-run, covers only `gastown`, grace 30m, parked 24h, cap 2, threshold 5; `{"enabled":true,"dry_run":false}` is the only way to get `IsDryRun()==false`; `max_per_tick: -3` and `0` give 2; a malformed duration string falls back to the default and does not panic; `CoversRig("beads")` is false by default and true when listed; the JSON round-trips through `mayor/town.json` loading.
- [ ] **Step 3: Run to see them fail** (`go test ./internal/config -run WorktreeCleanup -count=1`), **implement** the struct and methods, **run to see them pass**.
- [ ] **Step 4: Commit:** `config: add patrol_scan.worktree_cleanup with fail-safe defaults`.

**Acceptance (bead):**
- [ ] `{"enabled":true}` yields dry-run, rigs `[gastown]`, grace 30m, parked grace 24h, cap 2, alert threshold 5 (one table test)
- [ ] Only an explicit `"dry_run": false` makes `IsDryRun()` false
- [ ] `max_per_tick` of 0 or negative never means unlimited
- [ ] The block loads from `mayor/town.json` (test through `townconfig.Load`, or a recorded reason it needs no schema change)
- [ ] `make gate` passes

**Out of scope:** reading the config in the daemon (S4), any behavior change.

---

### Task 3 (slice S3): distinct exit code for a nuke safety refusal

**Bead:** `gt polecat nuke: exit with a distinct status when it refuses on a safety check`

**Files:**
- Modify: `internal/cmd/polecat.go` (`runPolecatNuke`, the `len(blocked) > 0` branch at ~`:2480`)
- Test: `internal/cmd/polecat_nuke_test.go` (find the existing nuke refusal test with `git grep -ln "failed nuke safety checks" -- internal/cmd`)

**Interfaces:**
- Produces: exported constant `NukeRefusedExitCode = 3` and an error type the root command maps to that exit code. S4 maps exit status 3 to "refused".

- [ ] **Step 1: Read first.** How other commands set a custom exit code (`git grep -n "os.Exit\|ExitCode\|exitError" -- internal/cmd/root.go internal/cmd/*.go | head`). Reuse that mechanism; do not invent a second one.
- [ ] **Step 2: Write the failing test:** with a polecat whose `checkPolecatSafety` returns `Blocked`, `runPolecatNuke` returns an error that the exit-code mapper turns into status 3; a non-safety failure (unknown polecat) stays 1; `--force` still bypasses.
- [ ] **Step 3: Implement,** keeping the human message and the `displaySafetyCheckBlocked` output unchanged.
- [ ] **Step 4: Commit:** `polecat nuke: exit 3 on a safety refusal so callers need not match text`.

**Acceptance (bead):**
- [ ] A nuke refused by the safety check exits 3; every other failure still exits 1
- [ ] The refusal text and blocked-list output are byte-identical to before
- [ ] `--force` and `--dry-run` behavior unchanged
- [ ] Test covers refusal, non-refusal failure and `--force`
- [ ] `make gate` passes

**Out of scope:** changing what counts as blocked; unifying the daemon and nuke verdicts.

---

### Task 4 (slice S4): host `ReapEnv` and options wiring

**Bead:** `Daemon host: implement ReapEnv over gt polecat check-recovery and nuke, wire worktree_cleanup config`

**Depends on:** S1, S2, S3.

**Files:**
- Create: `internal/daemon/patrol_scan_reap.go`, `internal/daemon/patrol_scan_reap_test.go`
- Modify: `internal/daemon/patrol_scan.go` (`patrolScanOptions` and the `IsRefusal` closure; keep the edit to a few lines)

**Interfaces:**
- Consumes: `patrolscan.ReapEnv`, `patrolscan.Recovery`, `patrolscan.ReapOptions` (S1); `WorktreeCleanupConfig` methods (S2); exit status 3 (S3).
- Produces: `(*patrolScanHost)` implements `ReapEnv`. `errReapRefused` sentinel; `patrolScanOptions` sets `Options.Reap` only when `IsEnabled()`, and `IsRefusal` also returns true for `errReapRefused`.

- [ ] **Step 1: Read first:** `h.d.sup()` and how `Restart` shells out (`gt session restart`) for the exec helper, timeout and env conventions; `beads.Issue` fields for update/closed times; `intent.Record` for any park timestamp; `patrolScanHost.readBeads`, `listByStatus`, `AgentRecord` (host methods) for the bead readers to reuse.
- [ ] **Step 2: `Recovery`:** exec `gt polecat check-recovery <rig>/<name> --json` (no `--reconcile-cleanup`) with a 60s timeout, decode into a struct with the `verdict`, `reusable`, `safe_to_nuke`, `reason`, `branch`, `issue`, `active_mr`, `blockers`, `git_state_source` JSON keys. Test with a fake exec: valid JSON maps field for field; invalid JSON, empty output and a non-zero exit are errors, never a zero `Recovery`. Exit 0 with a non-SAFE verdict is a normal result (the CLI exits 0 whatever the verdict).
- [ ] **Step 3: `Reap`:** exec `gt polecat nuke <rig>/<name>` (argv exactly that, three elements after `gt`; assert it contains no `--force`) with the 3-minute timeout `patrolScanRestartTimeout` uses. Exit status 3 returns `fmt.Errorf("%w: %s", errReapRefused, stderrTail)`; any other failure is a plain error.
- [ ] **Step 4: `IdleSince`:** the later of the agent bead's last update, and the hooked work bead's `closed_at` (else its last update); for a parked seat also the park time if `intent.Record` carries one. Any unreadable input is an error; no readable input is the zero time (the pure pass treats zero as not eligible).
- [ ] **Step 5: `BranchClaimed`:** list non-terminal beads in the rig (open, in_progress, hooked, blocked, deferred) and return the first whose notes satisfy `patrolscan.ResumeBranchFromNotes(notes) == branch`, or whose assignee names the branch's polecat. Use `listByStatus`. A failed list is an error. Always pass `--limit 0` to any bd query (bd defaults to 50 rows).
- [ ] **Step 6: Wire options:** in `patrolScanOptions`, when `c.WorktreeCleanup.IsEnabled()` and the rig is covered, set `o.Reap = &patrolscan.ReapOptions{DryRun: c.WorktreeCleanup.IsDryRun(), Grace: ..., ParkedGrace: ..., MaxPerTick: ...}`. Per-rig scoping: the host's `ReapEnv` methods return an error for a rig the block does not cover, so a mis-scoped call is `unknown`, not a removal. Test: disabled block leaves `Options.Reap` nil; enabled+default is dry-run; beads rig is not covered.
- [ ] **Step 7: Commit:** `daemon: wire patrol_scan reap host over check-recovery and nuke`.

**Acceptance (bead):**
- [ ] `Reap` runs exactly `gt polecat nuke <rig>/<name>`; a test asserts the argv and that `--force` is absent
- [ ] `Recovery` parses real `check-recovery --json` output (golden file from a captured run) and treats bad JSON, empty output and non-zero exit as errors
- [ ] Exit status 3 from nuke becomes `errReapRefused` and the tick reports `blocked`, not `failed`
- [ ] A disabled or absent config leaves the tick byte-identical to today; an enabled block with no other keys is dry-run on gastown only
- [ ] `IdleSince` returns an error rather than a guess when its inputs are unreadable
- [ ] `make gate` passes

**Out of scope:** alerts (S5); turning the feature on in the live town config; any change to `check-recovery` itself.

---

### Task 5 (slice S5): blocked-seat alerts

**Bead:** `patrol_scan: raise deduped alerts for blocked reap candidates and a threshold alert`

**Depends on:** S4 (same hot file, `internal/daemon/patrol_scan.go`).

**Files:**
- Modify: `internal/daemon/patrol_scan.go` (`runPatrolScan`, after the `Tick` report loop)
- Create: `internal/daemon/patrol_scan_reap_alert.go`, `internal/daemon/patrol_scan_reap_alert_test.go`

**Interfaces:**
- Consumes: `Report.Findings` with `Kind=="reap"` and `Outcome==OutcomeBlocked`; `Daemon.escalateAlert(key, source, message)` (dedupes by key through `gt escalate --fingerprint`); `WorktreeCleanupConfig.AlertThreshold()`.
- Produces: `func (d *Daemon) reapAlerts(rig string, r patrolscan.Report, threshold int)`.

- [ ] **Step 1: Write failing tests** with a recording alert function injected into `reapAlerts`: one blocked seat raises one alert keyed `reap-blocked:<rig>/<name>:<hash of detail>`; the same detail on the next tick raises the same key (the CLI dedupes), a changed detail raises a new key; blocked count below the threshold raises no threshold alert; at the threshold raises one alert keyed `reap-blocked-threshold:<rig>`; a seat no longer blocked has its key cleared via `gt escalate clear --fingerprint` (record these calls).
- [ ] **Step 2: Implement.** Source label `patrol-scan`. The message names the seat, the blockers and the operator next step (`gt polecat check-recovery <rig>/<name>`, then nuke by hand with `--force` only after reading the diff). The hash is the first 8 hex chars of a SHA-256 of the detail string.
- [ ] **Step 3: Call it** from `runPatrolScan` after `report.Lines()` is logged, only when the rig's `WorktreeCleanup` block is enabled.
- [ ] **Step 4: Commit:** `patrol_scan: alert on blocked reap candidates, deduped, with a threshold alert`.

**Acceptance (bead):**
- [ ] One alert per blocked seat per distinct blocker set; an unchanged blocker set reuses the same key
- [ ] A threshold alert fires once at `blocked_alert_threshold` (default 5) and is cleared when the count falls below it
- [ ] A seat that stops being blocked has its per-seat alert cleared
- [ ] No alerts are raised when `worktree_cleanup` is disabled
- [ ] `make gate` passes

**Out of scope:** the attention-queue UI; `gt tail` formatting; auto-forcing any blocked seat.

---

## Rollout (operator steps, not beads)

1. Land S1–S5 with the block absent (feature off).
2. Add `"worktree_cleanup": {"enabled": true}` to `patrol_scan` in `mayor/town.json` (dry-run, gastown only). Watch `would-reap` lines for a few days: every one must be a seat you would nuke by hand, and no reusable `done` seat may appear.
3. Set `"dry_run": false`. `max_per_tick` stays 2.
4. Kill switch: `enabled: false`. No state to unwind: the nuke preserves the branch to origin first.

## Self-review

- **Spec coverage:** Q1 → S1 (reusable veto + regression test). Q2 → S1 grace tests, S2 defaults. Q3 → S1 safe rule (live git via check-recovery), S4 never passes `--reconcile-cleanup`. Q4 → S1 vetoes (assigned bead, `BranchClaimed`, `PENDING_MR`, session, intent); `orphans()` ordering is by construction (reap runs before it, vetoed seats keep their dir). Q5 → S2 `CoversRig`, S4 per-rig scoping. Q6 → S1 summary line + log lines, S5 alerts. Q7 → S2 defaults, S1 dry-run and cap, S4 argv assertion, S3 refusal exit code. Revision 1 items 1–7 each map to a task.
- **Placeholders:** none; the "read X first" steps name files and what to look for. The one deliberate open input is `IdleSince`'s park-time source, resolved in S4 step 1 with a stated fallback.
- **Type consistency:** `ReapEnv`, `Recovery`, `ReapOptions`, `Options.Reap`, the three outcomes, `errReapRefused`, `NukeRefusedExitCode` and the `WorktreeCleanupConfig` methods are used with the same names in every task that touches them.
