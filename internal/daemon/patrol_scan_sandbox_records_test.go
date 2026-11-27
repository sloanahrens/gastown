package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/session"
)

// recordReap is one reap scenario: a town with one registry record, a fake tmux
// and a scripted gateway.
type recordReap struct {
	t            *testing.T
	town         string
	host         *patrolScanHost
	asked        []string                // "gateway|sandbox" questions, in order
	markerAsked  []string                // "gateway|sandbox" ended-marker questions, in order
	deleterCalls []string                // sandboxes the record deleter was asked to delete
	order        []string                // "copy" and "delete", in the order the pass made them
	copied       []session.SandboxRecord // the records the outbox copy was asked to copy
	copyErr      error                   // the outbox copy's failure, read on each call: a test may change it between passes
	logs         strings.Builder
	agents       *beadsfake.Fake // the town's bead store: the seat's agent bead and the seat's own bead
}

// recordReapResult is the seat's result.json as the outbox copy writes it: a done
// result that carries one finding, so the seat end has something to apply.
const recordReapResult = `{"bead_id":"gt-x","status":"done","findings":["the retry never resets its counter"]}`

type recordReapOpts struct {
	age        time.Duration // the record's age at the fixed clock; negative leaves CreatedAt zero
	liveTmux   bool          // a live session answers to the record's name
	gateway    string        // the record's gateway
	exists     bool          // the gateway has the sandbox
	existsErr  error         // the gateway cannot be asked
	marker     bool          // the held sandbox answers that its ended marker is set
	markerErr  error         // the held sandbox cannot be asked for its marker
	deleterErr error         // the record deleter fails
	noDeleter  bool          // neither a seam nor a linked deleter exists
	copyErr    error         // the outbox copy fails
	noOutbox   bool          // the outbox copy succeeds but the seat wrote no file
	hook       string        // the seat's agent bead hook
	agentErr   error         // the seat's agent bead cannot be read
}

const recordReapSession = "gt-capable"

func newRecordReap(t *testing.T, o recordReapOpts) *recordReap {
	t.Helper()
	town := t.TempDir()
	clk := newFixedClock()
	tm := newFakeTmux(clk)
	if o.liveTmux {
		tm.addSession(recordReapSession, "claude", clk.Now())
	}
	r := &recordReap{t: t, town: town, copyErr: o.copyErr}
	rec := session.SandboxRecord{Session: recordReapSession, Rig: "gastown", Polecat: "capable", Gateway: o.gateway}
	if o.age >= 0 {
		rec.CreatedAt = clk.Now().Add(-o.age)
	}
	if err := session.WriteSandboxRecord(town, rec); err != nil {
		t.Fatal(err)
	}
	// The seat's agent bead: its hook decides whether the patrol may end a held
	// sandbox, and a store that refuses the read keeps it (gt-oxahwh).
	agents := beadsfake.New()
	id := beads.AgentBeadIDFor(constants.RolePolecat, rec.Rig, rec.Polecat, town)
	if _, err := agents.Create(beads.CreateOptions{
		ID: id, Title: id, Labels: []string{"gt:agent"}, Priority: -1,
		Description: beads.FormatAgentDescription(id, &beads.AgentFields{
			RoleType: "polecat", Rig: rec.Rig, HookBead: o.hook,
		}),
	}); err != nil {
		t.Fatal(err)
	}
	r.agents = agents
	openRecoveryBeads := func([]string) beads.Client { return agents }
	if o.agentErr != nil {
		openRecoveryBeads = func([]string) beads.Client {
			return failingShowClient{Client: agents, err: o.agentErr}
		}
	}
	d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(&r.logs, "", 0), tmux: tm, clock: clk,
		openRecoveryBeads: openRecoveryBeads}
	r.host = &patrolScanHost{d: d, openRecoveryBeads: openRecoveryBeads, sandboxExists: func(gateway, name string) (bool, error) {
		r.asked = append(r.asked, gateway+"|"+name)
		return o.exists, o.existsErr
	}, sandboxStatus: func(string, string) (sandbox.Status, error) {
		// A running sandbox: the phase question never reaches a real gateway.
		return sandbox.Status{Phase: "Ready"}, nil
	}, sandboxEndedMarker: func(gateway, name string) (bool, error) {
		r.markerAsked = append(r.markerAsked, gateway+"|"+name)
		return o.marker, o.markerErr
	}, noSandboxDeleterLinked: o.noDeleter}
	// The outbox copy never reaches a real gateway: the seam writes a result
	// file into the copy directory, as the real copy does for a seat that wrote
	// one, and records the order against the deleter.
	r.host.sandboxOutboxCopy = func(town string, rec session.SandboxRecord) error {
		r.order = append(r.order, "copy")
		r.copied = append(r.copied, rec)
		if r.copyErr != nil {
			return r.copyErr
		}
		if o.noOutbox {
			return nil // the seat wrote no outbox file: nothing to copy, no failure
		}
		dir := session.SandboxOutboxCopyDir(town, rec.Rig, rec.Polecat)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "result.json"), []byte(recordReapResult), 0o600)
	}
	if !o.noDeleter {
		r.host.sandboxDeleter = func(_ string, rec session.SandboxRecord) error {
			r.order = append(r.order, "delete")
			r.deleterCalls = append(r.deleterCalls, rec.Session)
			return o.deleterErr
		}
	}
	return r
}

// failingShowClient is the town's bead store refusing a read, every other call
// answering as the fake does.
type failingShowClient struct {
	beads.Client
	err error
}

func (c failingShowClient) Show(string) (*beads.Issue, error) { return nil, c.err }

// A held sandbox whose seat still hooks a bead is kept with its record, and
// neither its outbox nor its sandbox is touched: the seat's work is not saved
// yet, so the delete would lose it (gt-oxahwh).
func TestPatrolScanSandboxRecordsKeepsAHeldSandboxWhoseSeatStillHooksABead(t *testing.T) {
	t.Parallel()
	r := newRecordReap(t, recordReapOpts{age: sandboxRecordGrace + time.Minute, gateway: "gw1", exists: true, marker: true, hook: "gt-1"})
	r.host.patrolScanSandboxRecords()
	if !r.recorded() {
		t.Error("the record was removed although its seat still hooks gt-1")
	}
	if len(r.order) != 0 {
		t.Errorf("the pass went on to %v although the seat still hooks gt-1", r.order)
	}
	if want := "hook_bead=gt-1 is still set"; !strings.Contains(r.logs.String(), want) {
		t.Errorf("log = %q, want it to name %q", r.logs.String(), want)
	}
}

// An agent bead that cannot be read keeps the held sandbox and its record: the
// hook is unknown, never clear (gt-oxahwh).
func TestPatrolScanSandboxRecordsKeepsAHeldSandboxWhoseAgentBeadCannotBeRead(t *testing.T) {
	t.Parallel()
	r := newRecordReap(t, recordReapOpts{age: sandboxRecordGrace + time.Minute, gateway: "gw1", exists: true, marker: true, agentErr: errors.New("dolt unavailable")})
	r.host.patrolScanSandboxRecords()
	if !r.recorded() {
		t.Error("the record was removed although its agent bead could not be read")
	}
	if len(r.order) != 0 {
		t.Errorf("the pass went on to %v although the agent bead could not be read", r.order)
	}
	if want := "cannot read its agent bead: dolt unavailable"; !strings.Contains(r.logs.String(), want) {
		t.Errorf("log = %q, want it to name %q", r.logs.String(), want)
	}
}

// A nuked seat's agent bead is missing, which is the normal state after a nuke
// purges it, so the patrol deletes the seat's ended held sandbox rather than
// leaking it. A missing bead reads as nil fields with no error, and the pass
// goes on to the outbox copy and the delete (gt-t44zvb).
func TestPatrolScanSandboxRecordsDeletesAHeldSandboxOfANukedSeatWhoseAgentBeadIsMissing(t *testing.T) {
	t.Parallel()
	r := newRecordReap(t, recordReapOpts{age: sandboxRecordGrace + time.Minute, gateway: "gw1", exists: true, marker: true, agentErr: beads.ErrNotFound})
	r.host.patrolScanSandboxRecords()
	if got, want := strings.Join(r.order, ","), "copy,delete"; got != want {
		t.Fatalf("order of the copy and the delete = %q, want %q", got, want)
	}
	if r.recorded() {
		t.Error("the record survived although the nuked seat's held sandbox was deleted")
	}
}

// Only a bead that does not exist clears the patrol's hook check: any other read
// failure keeps the held sandbox and its record, because the hook is unknown and
// never clear. An error that wraps beads.ErrNotFound is a missing bead, as
// errors.Is reads it, so that one deletes (gt-t44zvb).
func TestPatrolScanSandboxRecordsKeepsAHeldSandboxWhoseAgentBeadReadFailsWithAnythingButNotFound(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		agentErr  error
		wantOrder string // the copy and the delete the pass makes, "" for neither
		wantLog   string // a fragment the pass logs, "" for no assertion
	}{
		{"dolt_timeout_keeps_the_record", errors.New("dolt: context deadline exceeded"), "", "cannot read its agent bead"},
		{"wrapped_not_found_deletes_the_sandbox", fmt.Errorf("queued read: %w", beads.ErrNotFound), "copy,delete", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newRecordReap(t, recordReapOpts{age: sandboxRecordGrace + time.Minute, gateway: "gw1", exists: true, marker: true, agentErr: tt.agentErr})
			r.host.patrolScanSandboxRecords()
			if got := strings.Join(r.order, ","); got != tt.wantOrder {
				t.Errorf("order of the copy and the delete = %q, want %q", got, tt.wantOrder)
			}
			if gone, wantGone := !r.recorded(), tt.wantOrder != ""; gone != wantGone {
				t.Errorf("record removed = %v, want %v", gone, wantGone)
			}
			if tt.wantLog != "" && !strings.Contains(r.logs.String(), tt.wantLog) {
				t.Errorf("log = %q, want it to contain %q", r.logs.String(), tt.wantLog)
			}
		})
	}
}

// A held sandbox whose seat hooks nothing is deleted through the deleter as it
// was before the hook check: the outbox copy comes first (gt-oxahwh).
func TestPatrolScanSandboxRecordsStillDeletesAHeldSandboxWhoseHookIsClear(t *testing.T) {
	t.Parallel()
	r := newRecordReap(t, recordReapOpts{age: sandboxRecordGrace + time.Minute, gateway: "gw1", exists: true, marker: true})
	r.host.patrolScanSandboxRecords()
	if got, want := strings.Join(r.order, ","), "copy,delete"; got != want {
		t.Fatalf("order of the copy and the delete = %q, want %q", got, want)
	}
	if r.recorded() {
		t.Error("the record survived the deleted sandbox")
	}
}

// endListedSandbox holds the walk's keep steps for a listed sandbox: an end
// function that returns a reason leaves the record in place, and the reason is
// logged in the existing kept line (gt-7g2iir).
func TestPatrolScanEndListedSandboxKeepsWithItsReason(t *testing.T) {
	t.Parallel()
	r := newRecordReap(t, recordReapOpts{age: sandboxRecordGrace + time.Minute, gateway: "gw1", exists: true, marker: true})
	rec, ok, err := session.ReadSandboxRecord(r.town, recordReapSession)
	if err != nil || !ok {
		t.Fatalf("the record before the pass = %v, %v; want it present", ok, err)
	}
	calls := 0
	r.host.endListedSandbox(recordReapSession, rec, func(caller, name string, rec session.SandboxRecord) string {
		calls++
		return "its ended marker is not set"
	})
	if calls != 1 {
		t.Errorf("the end function was called %d times, want 1", calls)
	}
	if !r.recorded() {
		t.Error("the record was removed although the end kept the sandbox")
	}
	if want := "patrol_scan: sandbox record gt-capable kept: its ended marker is not set (gastown/capable)"; !strings.Contains(r.logs.String(), want) {
		t.Errorf("log = %q, want it to contain %q", r.logs.String(), want)
	}
}

func (r *recordReap) recorded() bool {
	r.t.Helper()
	ok, err := session.IsSandboxed(r.town, recordReapSession)
	if err != nil {
		r.t.Fatal(err)
	}
	return ok
}

// A registry record is removed only for a sandbox the gateway reports gone,
// and only once it is past the grace and no live session answers to its name.
// An unreachable gateway keeps the record, as does a young one: a sandbox that
// deleted itself is told from one still coming up by the gateway's answer and
// the record's age together. A sandbox the gateway still has is ended only when
// its ended marker is set, through the record deleter; every other answer, and a
// binary with no deleter linked, keeps the record and the sandbox.
func TestPatrolScanSandboxRecords(t *testing.T) {
	t.Parallel()
	old := sandboxRecordGrace + time.Minute
	tests := []struct {
		name             string
		opts             recordReapOpts
		wantGone         bool
		wantAsked        bool
		wantMarkerAsked  bool
		wantDeleterCalls bool
		wantLog          string // a fragment the pass logs, "" for no assertion
	}{
		{"old_record_gateway_has_no_sandbox", recordReapOpts{age: old, gateway: "gw1"}, true, true, false, false, ""},
		{"old_record_with_no_gateway_named", recordReapOpts{age: old}, true, true, false, false, ""},
		{"record_without_a_date_counts_as_old", recordReapOpts{age: -1, gateway: "gw1"}, true, true, false, false, ""},
		{"young_record_is_kept", recordReapOpts{age: sandboxRecordGrace - time.Second, gateway: "gw1"}, false, false, false, false, ""},
		{"gateway_unreachable_keeps_the_record", recordReapOpts{age: old, gateway: "gw1", existsErr: errors.New("connection refused")}, false, true, false, false, ""},
		{"live_session_keeps_the_record", recordReapOpts{age: old, gateway: "gw1", liveTmux: true}, false, false, false, false, ""},
		{"held_sandbox_with_marker_is_deleted_through_deleter", recordReapOpts{age: old, gateway: "gw1", exists: true, marker: true}, true, true, true, true, ""},
		// ParseAgentFields trims the hook, so the description reads as no hook and
		// the pass deletes; the patrol's own TrimSpace is not this row's subject.
		{"held_sandbox_with_a_blank_hook_is_deleted", recordReapOpts{age: old, gateway: "gw1", exists: true, marker: true, hook: "   "}, true, true, true, true, ""},
		{"held_sandbox_without_marker_keeps_the_record", recordReapOpts{age: old, gateway: "gw1", exists: true}, false, true, true, false, ""},
		{"held_sandbox_marker_exec_error_keeps_the_record", recordReapOpts{age: old, gateway: "gw1", exists: true, markerErr: errors.New("gateway unreachable")}, false, true, true, false, ""},
		{"held_sandbox_deleter_error_keeps_the_record", recordReapOpts{age: old, gateway: "gw1", exists: true, marker: true, deleterErr: errors.New("disk full")}, false, true, true, true, ""},
		{"held_sandbox_with_no_deleter_keeps_the_record", recordReapOpts{age: old, gateway: "gw1", exists: true, marker: true, noDeleter: true}, false, true, true, false, "no sandbox deleter is linked"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := newRecordReap(t, tt.opts)
			r.host.patrolScanSandboxRecords()
			if gone := !r.recorded(); gone != tt.wantGone {
				t.Fatalf("record removed = %v, want %v", gone, tt.wantGone)
			}
			if asked := len(r.asked) > 0; asked != tt.wantAsked {
				t.Errorf("gateway asked = %v (%v), want %v", asked, r.asked, tt.wantAsked)
			}
			if asked := len(r.markerAsked) > 0; asked != tt.wantMarkerAsked {
				t.Errorf("ended marker asked = %v (%v), want %v", asked, r.markerAsked, tt.wantMarkerAsked)
			}
			if called := len(r.deleterCalls) > 0; called != tt.wantDeleterCalls {
				t.Errorf("record deleter called = %v (%v), want %v", called, r.deleterCalls, tt.wantDeleterCalls)
			}
			if tt.wantAsked && len(r.asked) == 1 && r.asked[0] != tt.opts.gateway+"|"+recordReapSession {
				t.Errorf("gateway question = %q, want the record's gateway and its sandbox name", r.asked[0])
			}
			if tt.wantLog != "" && !strings.Contains(r.logs.String(), tt.wantLog) {
				t.Errorf("log = %q, want it to contain %q", r.logs.String(), tt.wantLog)
			}
		})
	}
}

// A held sandbox whose ended marker is set has its outbox copied to the host
// before the deleter runs, and the copy is on disk afterwards: a files-mode
// seat's result is in that outbox and nowhere else (gt-qem69c).
func TestPatrolScanSandboxRecordsCopiesTheOutboxBeforeDeletingAHeldSandbox(t *testing.T) {
	t.Parallel()
	r := newRecordReap(t, recordReapOpts{age: sandboxRecordGrace + time.Minute, gateway: "gw1", exists: true, marker: true})
	r.host.patrolScanSandboxRecords()
	if got, want := strings.Join(r.order, ","), "copy,delete"; got != want {
		t.Fatalf("order of the copy and the delete = %q, want %q", got, want)
	}
	copied := filepath.Join(session.SandboxOutboxCopyDir(r.town, "gastown", "capable"), "result.json")
	if _, err := os.Stat(copied); err != nil {
		t.Fatalf("the host copy of the outbox is not on disk after the pass: %v", err)
	}
	if r.recorded() {
		t.Error("the record is still there after the sandbox was deleted")
	}
}

// A failed outbox copy keeps the sandbox and its record with the copy's reason,
// and the deleter never runs: a seat's outbox is never deleted unread
// (gt-qem69c).
func TestPatrolScanSandboxRecordsKeepsASandboxWhoseOutboxCopyFailed(t *testing.T) {
	t.Parallel()
	r := newRecordReap(t, recordReapOpts{age: sandboxRecordGrace + time.Minute, gateway: "gw1", exists: true, marker: true, copyErr: errors.New("exec: transport error")})
	r.host.patrolScanSandboxRecords()
	if !r.recorded() {
		t.Error("the record was removed although the outbox copy failed")
	}
	if len(r.deleterCalls) != 0 {
		t.Errorf("the deleter ran (%v) after a failed outbox copy", r.deleterCalls)
	}
	if !strings.Contains(r.logs.String(), outboxNotCopiedReason) || !strings.Contains(r.logs.String(), "transport error") {
		t.Errorf("log = %q, want the kept reason and the copy's own error", r.logs.String())
	}
}

// A sandbox kept by a failed outbox copy is deleted on a later pass once the copy
// succeeds: the first pass keeps the sandbox and its record, and the second runs
// the copy and then the deleter, and removes the record (gt-5ms4cg).
func TestPatrolScanSandboxRecordsDeletesOnALaterPassOnceTheCopySucceeds(t *testing.T) {
	t.Parallel()
	r := newRecordReap(t, recordReapOpts{age: sandboxRecordGrace + time.Minute, gateway: "gw1", exists: true, marker: true, copyErr: errors.New("exec: transport error")})
	r.host.patrolScanSandboxRecords()
	if !r.recorded() {
		t.Fatal("the first pass removed the record although the outbox copy failed")
	}
	if len(r.deleterCalls) != 0 {
		t.Fatalf("the first pass ran the deleter (%v) after a failed outbox copy", r.deleterCalls)
	}
	r.copyErr = nil
	r.host.patrolScanSandboxRecords()
	if got, want := strings.Join(r.order, ","), "copy,copy,delete"; got != want {
		t.Fatalf("order of the copy and the delete over both passes = %q, want %q", got, want)
	}
	copied := filepath.Join(session.SandboxOutboxCopyDir(r.town, "gastown", "capable"), "result.json")
	if _, err := os.Stat(copied); err != nil {
		t.Errorf("the later pass's outbox copy is not on disk: %v", err)
	}
	if r.recorded() {
		t.Error("the record is still there after the later pass deleted the sandbox")
	}
}

// A sandbox whose outbox copy failed on one pass is copied and deleted on a later
// pass, and the seat end that reads the host copy afterwards applies the outbox to
// its bead: the seat's finding is the bead's comment, and the applied record names
// the bead as escalated and ready (gt-dytj5h).
func TestPatrolScanSandboxRecordsSeatEndAppliesTheOutboxCopiedOnALaterPass(t *testing.T) {
	t.Parallel()
	r := newRecordReap(t, recordReapOpts{age: sandboxRecordGrace + time.Minute, gateway: "gw1", exists: true, marker: true, copyErr: errors.New("exec: transport error")})
	if _, err := r.agents.Create(beads.CreateOptions{ID: "gt-x", Title: "the seat's task", Priority: 1}); err != nil {
		t.Fatal(err)
	}
	r.host.patrolScanSandboxRecords()
	if !r.recorded() {
		t.Fatal("the first pass removed the record although the outbox copy failed")
	}
	r.copyErr = nil
	r.host.patrolScanSandboxRecords()
	if r.recorded() {
		t.Fatal("the later pass kept the record after its outbox copy succeeded")
	}
	if reason := r.host.applySeatOutbox("gastown", "capable", "gt-x"); reason != "" {
		t.Fatalf("the seat end kept the seat for the outbox copied on a later pass: %s", reason)
	}
	comments, err := r.agents.Comments("gt-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].Text != "the retry never resets its counter" {
		t.Errorf("the bead's comments = %+v, want the seat's one finding", comments)
	}
	applied, have, err := readAppliedSeatOutbox(r.town, "gastown", "capable")
	if err != nil || !have {
		t.Fatalf("applied record (have=%v, err=%v), want one for the outbox the seat end applied", have, err)
	}
	if applied.Bead != "gt-x" || !applied.Escalated || !applied.Ready {
		t.Errorf("applied record = %+v, want bead gt-x escalated and ready", applied)
	}
}

// A seat that wrote no outbox is deleted as it was before the copy-out: the
// copy succeeds with nothing in it, the sandbox and its record go, and the seat
// end that reads the host copy later applies nothing to the bead (gt-zcky7n).
func TestPatrolScanSandboxRecordsDeletesASeatWithNoOutboxAsBefore(t *testing.T) {
	t.Parallel()
	r := newRecordReap(t, recordReapOpts{age: sandboxRecordGrace + time.Minute, gateway: "gw1", exists: true, marker: true, noOutbox: true})
	r.host.patrolScanSandboxRecords()
	if got, want := strings.Join(r.order, ","), "copy,delete"; got != want {
		t.Fatalf("order of the copy and the delete = %q, want %q", got, want)
	}
	if len(r.copied) != 1 {
		t.Errorf("the outbox copy was asked %d times, want once", len(r.copied))
	}
	if r.recorded() {
		t.Error("the record survived a seat that wrote no outbox")
	}
	// The seat end, landing after the patrol, finds an empty copy: it reports
	// nothing and records nothing applied.
	if reason := r.host.applySeatOutbox("gastown", "capable", "gt-x"); reason != "" {
		t.Errorf("applySeatOutbox kept the seat for an outbox that was never written: %s", reason)
	}
	if _, have, err := readAppliedSeatOutbox(r.town, "gastown", "capable"); err != nil || have {
		t.Errorf("applied record (have=%v, err=%v) after an empty outbox, want none written", have, err)
	}
	dir := session.SandboxOutboxCopyDir(r.town, "gastown", "capable")
	if entries, err := os.ReadDir(dir); err == nil && len(entries) != 0 {
		t.Errorf("the copy directory holds %v, want nothing for a seat that wrote no outbox", entries)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Errorf("reading the copy directory: %v", err)
	}
}

// The patrol copies from the record it holds, so a record that cannot be read
// again cannot turn the copy into a no-op before the delete.
func TestPatrolScanSandboxRecordsCopiesFromTheRecordItHolds(t *testing.T) {
	t.Parallel()
	r := newRecordReap(t, recordReapOpts{age: sandboxRecordGrace + time.Minute, gateway: "gw1", exists: true, marker: true})
	r.host.patrolScanSandboxRecords()
	if len(r.copied) != 1 || r.copied[0].Session != recordReapSession || r.copied[0].Gateway != "gw1" || r.copied[0].Rig != "gastown" || r.copied[0].Polecat != "capable" {
		t.Fatalf("the copy was asked for %+v, want the held record of %s on gw1", r.copied, recordReapSession)
	}
}

// The landing's own end copied the outbox before it ends the held sandbox, so
// endHeldSandbox, which it calls, does not copy a second time.
func TestEndHeldSandboxAtTheLandingDoesNotCopyTheOutboxAgain(t *testing.T) {
	t.Parallel()
	r := newRecordReap(t, recordReapOpts{age: sandboxRecordGrace + time.Minute, gateway: "gw1", exists: true, marker: true})
	rec := session.SandboxRecord{Session: recordReapSession, Rig: "gastown", Polecat: "capable", Gateway: "gw1"}
	if why := r.host.endHeldSandbox("sandbox seat end", recordReapSession, rec); why != "" {
		t.Fatalf("endHeldSandbox kept the sandbox: %s", why)
	}
	if got, want := strings.Join(r.order, ","), "delete"; got != want {
		t.Errorf("order = %q, want %q: the landing's end already copied the outbox", got, want)
	}
}

// writeOldRecords writes one old record per name under town.
func writeOldRecords(t *testing.T, town string, clk interface{ Now() time.Time }, names []string) {
	t.Helper()
	for _, n := range names {
		rec := session.SandboxRecord{Session: n, CreatedAt: clk.Now().Add(-time.Hour)}
		if err := session.WriteSandboxRecord(town, rec); err != nil {
			t.Fatal(err)
		}
	}
}

// One tick checks at most its budget of records, and each tick's walk starts
// after the name the last one ended on, so records the gateway still has cannot
// starve the ones behind them: every record is asked within ceil(n/budget)
// ticks, and the ones the gateway has are the only ones left. The two-minute
// case runs at the default cadence.
func TestPatrolScanSandboxRecordsRotateSoKeptRecordsDoNotStarveTheRest(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		records int
		budget  int
		step    time.Duration // between ticks
		kept    int           // the first kept records (sorted) are the gateway's
	}{
		{"minute_ticks_seven_records", 7, 3, time.Minute, 4},
		{"two_minute_ticks_forty_records", 40, 20, 2 * time.Minute, 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			clk := newFixedClock()
			names := make([]string, tt.records)
			for i := range names {
				names[i] = fmt.Sprintf("gt-p%02d", i)
			}
			writeOldRecords(t, town, clk, names)
			present := make(map[string]bool, tt.kept)
			for _, n := range names[:tt.kept] {
				present[n] = true
			}
			asked := make(map[string]bool)
			var tickAsked []string
			h := &patrolScanHost{
				d: &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0), tmux: newFakeTmux(clk), clock: clk},
				sandboxExists: func(_, name string) (bool, error) {
					tickAsked = append(tickAsked, name)
					asked[name] = true
					return present[name], nil
				},
				sandboxStatus:       readySandboxStatus,
				sandboxEndedMarker:  func(string, string) (bool, error) { return false, nil },
				sandboxRecordBudget: tt.budget,
			}

			ticks := (tt.records + tt.budget - 1) / tt.budget
			for tick := 0; tick < ticks; tick++ {
				tickAsked = nil
				h.patrolScanSandboxRecords()
				if len(tickAsked) > tt.budget {
					t.Fatalf("tick %d asked %d questions %v, want at most %d", tick, len(tickAsked), tickAsked, tt.budget)
				}
				clk.Advance(tt.step)
			}

			for _, n := range names {
				if !asked[n] {
					t.Errorf("record %s not asked within %d ticks", n, ticks)
				}
			}
			if left, want := session.ListSandboxSessions(town), names[:tt.kept]; !reflect.DeepEqual(left, want) {
				t.Errorf("records left after %d ticks = %v, want only the ones the gateway has %v", ticks, left, want)
			}
		})
	}
}

// readySandboxStatus answers the gateway's phase question as a running sandbox,
// so a record-pass test whose gateway has the sandbox never reaches a real
// gateway through session.GatewaySandboxPhase (gt-7h38pd).
func readySandboxStatus(string, string) (sandbox.Status, error) {
	return sandbox.Status{Phase: "Ready"}, nil
}

// churnHost is a record pass over town whose gateway has every sandbox, so the
// pass removes nothing and asked holds the names it judged, in order.
func churnHost(town string, clk *clockwork.FakeClock, budget int, asked *[]string) *patrolScanHost {
	return &patrolScanHost{
		d: &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0), tmux: newFakeTmux(clk), clock: clk},
		sandboxExists: func(_, name string) (bool, error) {
			*asked = append(*asked, name)
			return true, nil
		},
		sandboxStatus:       readySandboxStatus,
		sandboxEndedMarker:  func(string, string) (bool, error) { return false, nil },
		sandboxRecordBudget: budget,
	}
}

// The walk keeps its place when records come and go between ticks: a record
// removed under the cursor does not restart the walk, a record still inside the
// grace uses none of the budget, and a record added behind the cursor is judged
// once the walk wraps.
func TestPatrolScanSandboxRecordsCursorSurvivesChurn(t *testing.T) {
	t.Parallel()
	t.Run("removed_cursor_record_does_not_restart_the_walk", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		clk := newFixedClock()
		writeOldRecords(t, town, clk, []string{"gt-p00", "gt-p01", "gt-p02", "gt-p03"})
		var asked []string
		h := churnHost(town, clk, 2, &asked)
		h.patrolScanSandboxRecords()
		if want := []string{"gt-p00", "gt-p01"}; !reflect.DeepEqual(asked, want) {
			t.Fatalf("first tick asked %v, want %v", asked, want)
		}
		if err := session.RemoveSandboxRecord(town, "gt-p01"); err != nil {
			t.Fatal(err)
		}

		asked = nil
		h.patrolScanSandboxRecords()
		if want := []string{"gt-p02", "gt-p03"}; !reflect.DeepEqual(asked, want) {
			t.Errorf("second tick asked %v, want %v: the walk resumes after the removed cursor name", asked, want)
		}
	})
	t.Run("young_records_use_no_budget", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		clk := newFixedClock()
		for _, n := range []string{"gt-a", "gt-b"} {
			if err := session.WriteSandboxRecord(town, session.SandboxRecord{Session: n, CreatedAt: clk.Now().Add(-time.Minute)}); err != nil {
				t.Fatal(err)
			}
		}
		writeOldRecords(t, town, clk, []string{"gt-c", "gt-d", "gt-e"})
		var asked []string
		h := churnHost(town, clk, 2, &asked)
		h.patrolScanSandboxRecords()
		if want := []string{"gt-c", "gt-d"}; !reflect.DeepEqual(asked, want) {
			t.Errorf("tick asked %v, want %v: the young records are skipped without costing the budget", asked, want)
		}
	})
	t.Run("record_added_behind_the_cursor_is_judged_after_the_wrap", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		clk := newFixedClock()
		writeOldRecords(t, town, clk, []string{"gt-p02", "gt-p03", "gt-p04"})
		var asked []string
		h := churnHost(town, clk, 2, &asked)
		h.patrolScanSandboxRecords()
		if want := []string{"gt-p02", "gt-p03"}; !reflect.DeepEqual(asked, want) {
			t.Fatalf("first tick asked %v, want %v", asked, want)
		}
		writeOldRecords(t, town, clk, []string{"gt-p00"})

		asked = nil
		h.patrolScanSandboxRecords()
		if want := []string{"gt-p04", "gt-p00"}; !reflect.DeepEqual(asked, want) {
			t.Errorf("second tick asked %v, want %v: the new record sorts behind the cursor and is reached on the wrap", asked, want)
		}
	})
}

// probeCounter counts the tmux probes a record pass makes.
type probeCounter struct {
	sessionTmux
	probes int
}

func (p *probeCounter) HasSession(name string) (bool, error) {
	p.probes++
	return p.sessionTmux.HasSession(name)
}

// Every record a pass checks costs a tmux probe, live ones included, so one
// tick probes at most its budget of records: here all twelve records are live
// and the budget is four, so no tick probes more than four.
func TestPatrolScanSandboxRecordsBoundsTmuxProbes(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	clk := newFixedClock()
	tm := newFakeTmux(clk)
	names := make([]string, 12)
	for i := range names {
		names[i] = fmt.Sprintf("gt-l%02d", i)
		tm.addSession(names[i], "claude", clk.Now())
	}
	writeOldRecords(t, town, clk, names)
	probes := &probeCounter{sessionTmux: tm}
	h := &patrolScanHost{
		d:                   &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0), tmux: probes, clock: clk},
		sandboxRecordBudget: 4,
	}

	for tick := 0; tick < 3; tick++ {
		probes.probes = 0
		h.patrolScanSandboxRecords()
		if probes.probes > 4 {
			t.Fatalf("tick %d probed tmux %d times, want at most 4", tick, probes.probes)
		}
	}
}

// A tmux error keeps every record untouched: the probe cannot say the session
// is gone, so the pass neither asks the gateway nor deletes anything, and it
// logs why for each record. The next tick judges them again (gt-0vb2fy).
func TestPatrolScanSandboxRecordsKeepsEverythingWhenTmuxErrors(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	clk := newFixedClock()
	writeOldRecords(t, town, clk, []string{"gt-a", "gt-b"})
	tm := newFakeTmux(clk)
	tm.hasErr = errors.New("tmux: no server")
	var logs strings.Builder
	gatewayAsked, removeCalls := 0, 0
	h := &patrolScanHost{
		d: &Daemon{config: &Config{TownRoot: town}, logger: log.New(&logs, "", 0), tmux: tm, clock: clk},
		sandboxExists: func(_, _ string) (bool, error) {
			gatewayAsked++
			return false, nil // the answer that would delete if it were asked
		},
		sandboxStatus: func(string, string) (sandbox.Status, error) {
			// A running sandbox: the probe error's phase question never reaches a
			// real gateway, and a non-Stopped answer keeps the record (gt-94q4eh).
			return sandbox.Status{Phase: "Running"}, nil
		},
		removeSandboxRecord: func(_, _ string) error {
			removeCalls++
			return nil
		},
		sandboxRecordBudget: 10,
	}

	h.patrolScanSandboxRecords()

	if gatewayAsked != 0 || removeCalls != 0 {
		t.Errorf("gateway asked %d times, records removed %d times, want 0 and 0 on a tmux error", gatewayAsked, removeCalls)
	}
	out := logs.String()
	for _, name := range []string{"gt-a", "gt-b"} {
		if want := "sandbox record " + name + " kept: cannot ask tmux whether the session is live"; !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

// A Stopped sandbox whose session probe errors is ended on its gateway phase,
// not kept for ever: the probe error is the same tmux error the record walk
// meets on every Stopped sandbox, so the walk reaches the Stopped end, which
// deletes the landed seat's sandbox and removes the record (gt-94q4eh).
func TestPatrolScanSandboxRecordsEndsAStoppedSeatOnProbeError(t *testing.T) {
	t.Parallel()
	p := newStoppedSeatPatrol(t, true)
	p.host.d.tmux.(*fakeTmux).hasErr = errors.New("tmux: server timed out")
	var logs strings.Builder
	p.host.d.logger = log.New(&logs, "", 0)

	p.host.patrolScanSandboxRecords()

	if want := []string{session.PolecatSessionName(session.DefaultPrefix, endPolecat)}; !reflect.DeepEqual(p.deleted, want) {
		t.Fatalf("sandboxes deleted = %v, want %v", p.deleted, want)
	}
	if p.recorded() {
		t.Error("the registry record survived the deleted sandbox")
	}
	if got := logs.String(); !strings.Contains(got, "warning") || !strings.Contains(got, "Stopped") {
		t.Errorf("log %q does not warn about the Stopped sandbox", got)
	}
}

// A session probe that errors asks the gateway's phase, and only a Stopped
// sandbox reaches the Stopped end. A Stopped row whose landing does not prove
// the seat landed now is kept there with its reason: an unlanded record, no
// record, or a record naming a branch the seat no longer holds. Any other phase,
// or a phase read that errors, is no proof the session is gone, so the walk keeps
// the record and says tmux could not be asked. No row may call the deleter or
// remove the record: the delete destroys the workspace volume with any unpushed
// commits (gt-onvurl).
func TestPatrolScanProbeErrorKeeps(t *testing.T) {
	t.Parallel()

	staleBranch := "polecat/agate/gt-other+x2"
	phase := func(want string) func(string, string) (sandbox.Status, error) {
		return func(string, string) (sandbox.Status, error) { return sandbox.Status{Phase: want}, nil }
	}
	tests := []struct {
		name string
		// patrol builds the seat the row probes: its kept record, its agent bead
		// and the gateway's phase answer.
		patrol func(t *testing.T) *keptSeatPatrol
		// wantLog is the line the row's keep logs: why the sandbox stays.
		wantLog string
	}{
		{
			name:    "stopped with an unlanded kept record",
			patrol:  func(t *testing.T) *keptSeatPatrol { return newStoppedSeatPatrol(t, false) },
			wantLog: stoppedUnlandedReason,
		},
		{
			name: "stopped with no kept record",
			patrol: func(t *testing.T) *keptSeatPatrol {
				p := newStoppedSeatPatrol(t, true)
				if err := removeKeptSandboxSeat(p.town, endRig, endPolecat); err != nil {
					t.Fatal(err)
				}
				return p
			},
			wantLog: "its seat's end is not recorded",
		},
		{
			name: "stopped with a kept record naming a branch the seat no longer holds",
			patrol: func(t *testing.T) *keptSeatPatrol {
				p := newStoppedSeatPatrol(t, true)
				p.seatFields.Branch = staleBranch
				p.setAgent()
				return p
			},
			wantLog: fmt.Sprintf(sandboxKeptRecordStaleReason, endBead, endBranch, "<missing>", staleBranch),
		},
		{
			name: "provisioning is not proof the session is gone",
			patrol: func(t *testing.T) *keptSeatPatrol {
				p := newStoppedSeatPatrol(t, true)
				p.host.sandboxStatus = phase("Provisioning")
				return p
			},
			wantLog: "cannot ask tmux whether the session is live",
		},
		{
			name: "an error phase is not proof the session is gone",
			patrol: func(t *testing.T) *keptSeatPatrol {
				p := newStoppedSeatPatrol(t, true)
				p.host.sandboxStatus = phase("Error")
				return p
			},
			wantLog: "cannot ask tmux whether the session is live",
		},
		{
			name: "a phase read that errors is not proof the session is gone",
			patrol: func(t *testing.T) *keptSeatPatrol {
				p := newStoppedSeatPatrol(t, true)
				p.host.sandboxStatus = func(string, string) (sandbox.Status, error) {
					return sandbox.Status{}, errors.New("gateway did not answer")
				}
				return p
			},
			wantLog: "cannot ask tmux whether the session is live",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := tt.patrol(t)
			p.host.d.tmux.(*fakeTmux).hasErr = errors.New("tmux: server timed out")
			var logs strings.Builder
			p.host.d.logger = log.New(&logs, "", 0)

			p.host.patrolScanSandboxRecords()

			if len(p.deleted) != 0 {
				t.Errorf("sandboxes deleted = %v, want none: the delete destroys the workspace volume", p.deleted)
			}
			if !p.recorded() {
				t.Error("the registry record was removed for a sandbox that stays")
			}
			if !strings.Contains(logs.String(), tt.wantLog) {
				t.Errorf("log %q does not name %q", logs.String(), tt.wantLog)
			}
		})
	}
}

// The failure branches keep what they cannot judge and never stop the pass: a
// gateway error keeps its record, and a removal that fails is logged while the
// next record is still judged and removed.
func TestPatrolScanSandboxRecordsFailureBranches(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	clk := newFixedClock()
	writeOldRecords(t, town, clk, []string{"gt-a", "gt-b", "gt-c", "gt-d"})
	tm := newFakeTmux(clk)
	var logs strings.Builder
	var removed []string
	h := &patrolScanHost{
		d: &Daemon{config: &Config{TownRoot: town}, logger: log.New(&logs, "", 0), tmux: tm, clock: clk},
		sandboxExists: func(_, name string) (bool, error) {
			if name == "gt-a" {
				return false, errors.New("connection refused")
			}
			return false, nil
		},
		removeSandboxRecord: func(_, name string) error {
			if name == "gt-b" {
				return errors.New("disk full")
			}
			removed = append(removed, name)
			return nil
		},
		sandboxRecordBudget: 10,
	}

	h.patrolScanSandboxRecords()

	if want := []string{"gt-c", "gt-d"}; !reflect.DeepEqual(removed, want) {
		t.Errorf("records removed = %v, want %v (gt-a: gateway error; gt-b: removal failed, pass went on)", removed, want)
	}
	out := logs.String()
	for _, want := range []string{
		"gt-a kept: cannot ask gateway",
		"gt-b: removing it failed: disk full",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

// keptSeatPatrol is one finished sandbox seat whose keep the end recorded and
// whose sandbox registry record the patrol finds: the record pass over the
// seat-end fake, with the record pass's own gateway and deleter seams, so the
// patrol's completion of the seat's end is the real end.
type keptSeatPatrol struct {
	*seatEndFake
	clk *clockwork.FakeClock
	// sandboxUp is the gateway's answer for the seat's sandbox: true until the
	// record deleter has deleted it.
	sandboxUp bool
}

func newKeptSeatPatrol(t *testing.T, now time.Time) *keptSeatPatrol {
	t.Helper()
	f := newSeatEndFake(t)
	clk := clockwork.NewFakeClockAt(now)
	f.host.d.clock = clk
	f.host.d.tmux = newFakeTmux(clk)
	f.host.d.openWorkBeads = keptSeatHookReader{}.open
	p := &keptSeatPatrol{seatEndFake: f, clk: clk}
	f.host.sandboxExists = func(string, string) (bool, error) { return p.sandboxUp, nil }
	f.host.sandboxEndedMarker = func(string, string) (bool, error) { return true, nil }
	f.host.sandboxDeleter = func(_ string, rec session.SandboxRecord) error {
		f.deleted = append(f.deleted, rec.Session)
		p.sandboxUp = false
		return nil
	}
	f.host.d.endSandboxSeat = f.host.endFinishedSandboxSeat
	return p
}

// registryRecord writes the seat's sandbox registry record, as the sling does
// when it launches the seat, old enough for the record pass to judge.
func (p *keptSeatPatrol) registryRecord(t *testing.T) {
	t.Helper()
	rec := session.SandboxRecord{
		Session:   session.PolecatSessionName(session.DefaultPrefix, endPolecat),
		Rig:       endRig,
		Polecat:   endPolecat,
		Gateway:   "gw1",
		CreatedAt: p.clk.Now().Add(-time.Hour),
	}
	if err := session.WriteSandboxRecord(p.town, rec); err != nil {
		t.Fatal(err)
	}
}

// recorded reports whether the seat's sandbox registry record is still there.
func (p *keptSeatPatrol) recorded() bool {
	p.t.Helper()
	ok, err := session.IsSandboxed(p.town, session.PolecatSessionName(session.DefaultPrefix, endPolecat))
	if err != nil {
		p.t.Fatal(err)
	}
	return ok
}

// keptSeatAttentionSeats is the attention collector's view of the town's kept
// records at now.
func keptSeatAttentionSeats(t *testing.T, d *Daemon, now time.Time) int {
	t.Helper()
	src := &attentionSources{d: d, now: now}
	items, err := src.collectKeptSandboxSeats(context.Background())
	if err != nil {
		t.Fatalf("collecting the kept-seat attention items: %v", err)
	}
	return len(items)
}

// The patrol that deletes a held sandbox completes the kept seat's end in the
// same pass: the end runs again with the landing facts the kept record holds,
// every guard agrees, the host directories and the record go, and the queue
// raises no false sandbox-seat-kept item (gt-cz5tdz).
func TestPatrolScanCompletesTheEndOfADeletedKeptSeat(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 12, 16, 57, 28, 0, time.UTC)
	p := newKeptSeatPatrol(t, now)
	p.seedAgent()
	p.retire(intent.DesiredStop)
	p.seatDirs()
	p.sandboxUp = true
	p.registryRecord(t)
	// The agent's session is still ending: its ended marker is not set yet, so
	// the landing has no sandbox it may delete (gt-0vb2fy).
	markerSet := false
	p.host.sandboxEndedMarker = func(string, string) (bool, error) { return markerSet, nil }

	// The landing's own pass keeps the seat: the sandbox is still up and its
	// marker is not set.
	if p.host.endFinishedSandboxSeat(endRig, endPolecat, keptSeatLanding()) {
		t.Fatal("a seat whose sandbox is still up was removed")
	}
	// The session has ended, so the marker is set and the patrol deletes the
	// sandbox the landing left.
	markerSet = true
	// Half an hour on, the queue would raise the false item.
	p.clk.Advance(32 * time.Minute)
	if got := keptSeatAttentionSeats(t, p.host.d, p.clk.Now()); got != 1 {
		t.Fatalf("attention items before the patrol = %d, want the false kept item", got)
	}

	p.host.patrolScanSandboxRecords()

	if want := []string{session.PolecatSessionName(session.DefaultPrefix, endPolecat)}; !reflect.DeepEqual(p.deleted, want) {
		t.Fatalf("sandboxes deleted = %v, want %v", p.deleted, want)
	}
	if p.recorded() {
		t.Error("the registry record survived the deleted sandbox")
	}
	if seats, bad, err := KeptSandboxSeats(p.town); err != nil || len(bad) != 0 || len(seats) != 0 {
		t.Errorf("kept seats after the patrol = %+v, bad %v, %v; want none", seats, bad, err)
	}
	if dirExists(t, filepath.Join(p.town, endRig, "polecats", endPolecat)) {
		t.Error("the released seat kept its host directories")
	}
	if got := keptSeatAttentionSeats(t, p.host.d, p.clk.Now()); got != 0 {
		t.Errorf("attention items after the patrol = %d, want none", got)
	}
}

// The record pass deletes a held sandbox under the seat lock the landing's own
// end takes, and re-reads the record under it. A landing that removed the record
// between the pass's read and its delete leaves the patrol nothing to do: it
// makes no deleter call and does not fail the pass. Every other answer keeps the
// record with that condition's own reason (gt-0vb2fy).
func TestPatrolScanKeepsAHeldSandboxItCannotDeleteUnderTheSeatLock(t *testing.T) {
	t.Parallel()
	name := session.PolecatSessionName(session.DefaultPrefix, endPolecat)
	tests := []struct {
		name string
		// between runs in the gateway seam, after the pass read the record and
		// before it takes the seat lock.
		between     func(t *testing.T, p *keptSeatPatrol)
		lock        func(rigPath, name string, fn func() error) error
		removeErr   error
		rig         string
		wantDeleted int
		wantReason  string // "" for a quiet skip
	}{
		{
			name: "a landing removed the record first",
			between: func(t *testing.T, p *keptSeatPatrol) {
				if err := session.RemoveSandboxRecord(p.town, name); err != nil {
					t.Fatalf("removing the record as the landing would: %v", err)
				}
			},
		},
		{
			name: "an unreadable record keeps it",
			between: func(t *testing.T, p *keptSeatPatrol) {
				path := filepath.Join(session.SandboxRegistryDir(p.town), name+".json")
				if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantReason: "kept: cannot re-read its sandbox record under its seat lock:",
		},
		{
			name: "a busy seat lock keeps it",
			lock: func(string, string, func() error) error {
				return polecat.ErrSandboxSeatLockBusy
			},
			wantReason: "kept: its seat lock could not be taken: polecat directory lock is busy",
		},
		{
			name:        "a record that cannot be removed keeps the seat end",
			removeErr:   errors.New("registry is read-only"),
			wantDeleted: 1,
			wantReason:  "kept: its sandbox is deleted but its record could not be removed: registry is read-only",
		},
		{
			name:       "an unsafe rig keeps it",
			rig:        "../gastown",
			wantReason: "kept: sandbox seat directories: rig \"../gastown\" is not a single path segment",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 11, 12, 16, 57, 28, 0, time.UTC)
			p := newKeptSeatPatrol(t, now)
			p.seedAgent()
			p.retire(intent.DesiredStop)
			p.seatDirs()
			p.sandboxUp = true
			if tt.rig != "" {
				if err := session.WriteSandboxRecord(p.town, session.SandboxRecord{
					Session: name, Rig: tt.rig, Polecat: endPolecat, Gateway: "gw1",
					CreatedAt: p.clk.Now().Add(-time.Hour),
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				p.registryRecord(t)
			}
			var logs strings.Builder
			p.host.d.logger = log.New(&logs, "", 0)
			p.host.sandboxExists = func(string, string) (bool, error) {
				if tt.between != nil {
					tt.between(t, p)
				}
				return true, nil
			}
			p.host.sandboxSeatLock = tt.lock
			p.host.removeSandboxRecord = func(string, string) error { return tt.removeErr }

			p.host.patrolScanSandboxRecords()

			if len(p.deleted) != tt.wantDeleted {
				t.Errorf("record deleter calls = %v, want %d", p.deleted, tt.wantDeleted)
			}
			if tt.wantReason == "" {
				return
			}
			if !strings.Contains(logs.String(), tt.wantReason) {
				t.Errorf("log %q does not name the reason %q", logs.String(), tt.wantReason)
			}
		})
	}
}

// A seat kept for a reason that stays true is still kept after the patrol
// deletes its sandbox, and its record names that reason: the completed end
// measured the sandbox gone and the next guard kept the seat (gt-cz5tdz).
func TestPatrolScanLeavesASeatKeptForAnotherReasonKept(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 12, 16, 57, 28, 0, time.UTC)
	p := newKeptSeatPatrol(t, now)
	p.seatFields.PushFailed = true
	p.seedAgent()
	p.retire(intent.DesiredStop)
	p.seatDirs()
	p.sandboxUp = true
	p.registryRecord(t)

	if p.host.endFinishedSandboxSeat(endRig, endPolecat, keptSeatLanding()) {
		t.Fatal("a seat whose sandbox is still up was removed")
	}

	p.host.patrolScanSandboxRecords()

	if len(p.deleted) != 1 {
		t.Fatalf("sandboxes deleted = %v, want the held one", p.deleted)
	}
	seats, bad, err := KeptSandboxSeats(p.town)
	if err != nil || len(bad) != 0 {
		t.Fatalf("reading the kept seats: bad %v, %v", bad, err)
	}
	if len(seats) != 1 {
		t.Fatalf("kept seats after the patrol = %+v, want the still-kept seat", seats)
	}
	if seats[0].Reason != keptSeatReason {
		t.Errorf("keep reason = %q, want %q: the completed end names the fact that still keeps it", seats[0].Reason, keptSeatReason)
	}
	if !dirExists(t, filepath.Join(p.town, endRig, "polecats", endPolecat)) {
		t.Error("a seat kept for unpushed work lost its host directories")
	}
}

// A kept record written before it held landing facts is left as it is and the
// patrol logs why: with no facts to offer the end, the keep stands as today
// (gt-cz5tdz).
func TestPatrolScanLeavesAKeptRecordWithoutLandingFactsAlone(t *testing.T) {
	t.Parallel()
	r := newRecordReap(t, recordReapOpts{age: sandboxRecordGrace + time.Minute, gateway: "gw1", exists: true, marker: true})
	var logs strings.Builder
	r.host.d.logger = log.New(&logs, "", 0)
	since := time.Date(2026, 11, 12, 16, 57, 28, 0, time.UTC)
	old := keptSandboxSeatRecord{Bead: endBead, Reason: keptSeatReason, Since: since}
	if err := writeKeptSandboxSeat(r.town, "gastown", "capable", old); err != nil {
		t.Fatal(err)
	}
	var calls []string
	r.host.d.endSandboxSeat = func(rig, name string, _ sandboxSeatLanding) bool {
		calls = append(calls, rig+"/"+name)
		return true
	}

	r.host.patrolScanSandboxRecords()

	if len(r.deleterCalls) != 1 {
		t.Fatalf("the held sandbox was not deleted: %v", r.deleterCalls)
	}
	if r.recorded() {
		t.Error("the registry record survived the deleted sandbox")
	}
	if len(calls) != 0 {
		t.Errorf("the seat end was offered for a record with no landing facts: %v", calls)
	}
	seats, bad, err := KeptSandboxSeats(r.town)
	if err != nil || len(bad) != 0 || len(seats) != 1 {
		t.Fatalf("kept seats = %+v, bad %v, %v; want the untouched record", seats, bad, err)
	}
	if seats[0].Reason != old.Reason || !seats[0].Since.Equal(since) || seats[0].Bead != old.Bead {
		t.Errorf("kept record = %+v, want it left as it was", seats[0])
	}
	if want := "sandbox seat end gastown/capable: its kept record names no landing facts"; !strings.Contains(logs.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, logs.String())
	}
}

// keptNoSandboxPatrol is the gateway-silent kept record of the fixture seat with
// no sandbox record: its agent bead is seeded so the seat's own gateway (gw1 in
// endSettings) resolves, and every gateway ask is recorded with the gateway it
// went to. up is the answer per gateway: a gateway not listed has no sandbox.
func keptNoSandboxPatrol(t *testing.T, up map[string]bool) (*keptSeatPatrol, *[]string, *strings.Builder) {
	t.Helper()
	p := newKeptSeatPatrol(t, time.Date(2026, 11, 12, 16, 57, 28, 0, time.UTC))
	p.seedAgent()
	var asked []string
	p.host.sandboxExists = func(gateway, name string) (bool, error) {
		asked = append(asked, gateway)
		return up[gateway], nil
	}
	p.host.recordKeptSeat(endRig, endPolecat, keptSeatLanding(), gatewayNoAnswerKeepReason(p.clk.Now()))
	logs := &strings.Builder{}
	p.host.d.logger = log.New(logs, "", 0)
	return p, &asked, logs
}

func keptSeatCount(t *testing.T, town string) int {
	t.Helper()
	seats, bad, err := KeptSandboxSeats(town)
	if err != nil || len(bad) != 0 {
		t.Fatalf("kept seats: bad %v, %v", bad, err)
	}
	return len(seats)
}

// A gateway-silent kept record whose seat has no sandbox record and whose
// sandbox the seat's own gateway no longer has is dropped (gt-5lel7f).
func TestPatrolScanKeptRecordNoSandboxIsDropped(t *testing.T) {
	t.Parallel()
	p, asked, logs := keptNoSandboxPatrol(t, map[string]bool{})
	p.host.patrolScanSandboxRecords()

	if n := keptSeatCount(t, p.town); n != 0 {
		t.Errorf("kept seats = %d, want the record dropped", n)
	}
	if want := []string{"gw1"}; !slices.Equal(*asked, want) {
		t.Errorf("gateways asked = %v, want the seat's own gateway %v", *asked, want)
	}
	if want := "patrol_scan: kept record of " + endRig + "/" + endPolecat + " dropped: its sandbox record is gone and the gateway has no sandbox"; !strings.Contains(logs.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, logs.String())
	}
}

// A gateway-silent kept record whose sandbox record is gone is left when the
// seat's gateway cannot say its sandbox is gone: an error, a timeout, and a
// gateway that still has the sandbox each keep it, and a seat whose sandbox
// record is still there is left to the walk. Dropping on any of them would lose
// a record that keeps a seat whose sandbox may still hold unsaved work
// (gt-nd3kgv).
func TestPatrolScanKeptRecordKeeps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		exists   func(gateway, name string) (bool, error) // nil: the gateway has no sandbox
		registry bool                                     // the seat's sandbox record is still there
		wantAsks int
		wantLog  string // a fragment the pass logs, "" for no assertion
	}{
		{"gateway_error_keeps_the_record", func(string, string) (bool, error) { return false, errors.New("gateway down") }, false, 1, `cannot ask gateway "gw1" whether it has its sandbox: gateway down`},
		{"gateway_timeout_keeps_the_record", func(string, string) (bool, error) { return false, context.DeadlineExceeded }, false, 1, "whether it has its sandbox: context deadline exceeded"},
		{"gateway_has_the_sandbox_keeps_the_record", func(string, string) (bool, error) { return true, nil }, false, 1, ""},
		{"sandbox_record_still_present_is_left_to_the_walk", nil, true, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p, asked, logs := keptNoSandboxPatrol(t, map[string]bool{})
			p.host.sandboxExists = func(gateway, name string) (bool, error) {
				*asked = append(*asked, gateway)
				if tt.exists == nil {
					return false, nil
				}
				return tt.exists(gateway, name)
			}
			if tt.registry {
				p.registryRecord(t)
			}
			before, bad, err := KeptSandboxSeats(p.town)
			if err != nil || len(bad) != 0 || len(before) != 1 {
				t.Fatalf("kept seats before the pass = %+v, bad %v, %v; want the fixture's one", before, bad, err)
			}
			removed := 0
			p.host.removeKeptSeat = func(string, string, string) error { removed++; return nil }
			p.host.asks = newGatewayAskBudget(sandboxRecordsPerTick)

			p.host.dropKeptSandboxSeatsWithoutRecord()

			if removed != 0 {
				t.Errorf("kept records removed = %d, want none", removed)
			}
			after, bad, err := KeptSandboxSeats(p.town)
			if err != nil || len(bad) != 0 || len(after) != 1 {
				t.Fatalf("kept seats after the pass = %+v, bad %v, %v; want the record left", after, bad, err)
			}
			if !reflect.DeepEqual(after[0], before[0]) {
				t.Errorf("kept record = %+v, want it left as %+v", after[0], before[0])
			}
			if len(*asked) != tt.wantAsks {
				t.Errorf("gateway asks = %v, want %d", *asked, tt.wantAsks)
			}
			if tt.wantLog != "" && !strings.Contains(logs.String(), tt.wantLog) {
				t.Errorf("log lacks %q:\n%s", tt.wantLog, logs.String())
			}
		})
	}
}

// A sandbox that lives on the seat's own gateway, which is not the default one,
// keeps its record: the default gateway has none, and asking it would read the
// seat as gone and delete the record wrongly (gt-5lel7f).
func TestPatrolScanKeptRecordNoSandboxOnItsOwnGatewayStays(t *testing.T) {
	t.Parallel()
	p, asked, _ := keptNoSandboxPatrol(t, map[string]bool{"gw1": true})
	p.host.patrolScanSandboxRecords()

	if n := keptSeatCount(t, p.town); n != 1 {
		t.Errorf("kept seats = %d, want the record kept: the sandbox exists on gw1", n)
	}
	if want := []string{"gw1"}; !slices.Equal(*asked, want) {
		t.Errorf("gateways asked = %v, want only %v, never the default gateway", *asked, want)
	}
}

// A gateway that errors keeps the record and says why (gt-5lel7f).
func TestPatrolScanKeptRecordNoSandboxGatewayErrorStays(t *testing.T) {
	t.Parallel()
	p, _, logs := keptNoSandboxPatrol(t, map[string]bool{})
	p.host.sandboxExists = func(string, string) (bool, error) { return false, errors.New("gateway down") }
	p.host.patrolScanSandboxRecords()

	if n := keptSeatCount(t, p.town); n != 1 {
		t.Errorf("kept seats = %d, want the record kept", n)
	}
	if want := "kept record of " + endRig + "/" + endPolecat + " kept: cannot ask gateway \"gw1\" whether it has its sandbox: gateway down"; !strings.Contains(logs.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, logs.String())
	}
}

// A kept record whose seat's agent cannot be resolved to a gateway is kept and
// no gateway is asked (gt-5lel7f).
func TestPatrolScanKeptRecordNoSandboxUnknownGatewayStays(t *testing.T) {
	t.Parallel()
	p, asked, logs := keptNoSandboxPatrol(t, map[string]bool{})
	p.host.agentRecord = func(string, string) (string, error) { return "", nil }
	p.host.patrolScanSandboxRecords()

	if n := keptSeatCount(t, p.town); n != 1 {
		t.Errorf("kept seats = %d, want the record kept", n)
	}
	if len(*asked) != 0 {
		t.Errorf("gateways asked = %v, want none", *asked)
	}
	if want := "kept record of " + endRig + "/" + endPolecat + " kept: its agent is not a sandbox seat"; !strings.Contains(logs.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, logs.String())
	}
}

// A kept record the pass cannot remove is kept, and the failure is logged. The
// removal fails through the seam, not file permissions, which a root process
// ignores (gt-5lel7f).
func TestPatrolScanKeptRecordNoSandboxUnremovableStays(t *testing.T) {
	t.Parallel()
	p, _, logs := keptNoSandboxPatrol(t, map[string]bool{})
	p.host.removeKeptSeat = func(string, string, string) error { return errors.New("injected removal failure") }
	p.host.patrolScanSandboxRecords()

	if n := keptSeatCount(t, p.town); n != 1 {
		t.Errorf("kept seats = %d, want the record kept", n)
	}
	if want := "kept record of " + endRig + "/" + endPolecat + " kept: its record cannot be removed: injected removal failure"; !strings.Contains(logs.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, logs.String())
	}
}

// The record walk's and the drop pass's gateway questions together never exceed
// the tick's one budget (twice the records plus a reserve): under a budget of
// one record that is three questions. The walk spends two on its one
// record, the reserve is the drop pass's, and the pass stops with a log line when
// it is spent; the next tick continues after the seat asked last, so the second
// kept record is reached (gt-5lel7f).
func TestPatrolScanKeptRecordNoSandboxAsksShareOneBudget(t *testing.T) {
	t.Parallel()
	p, asked, logs := keptNoSandboxPatrol(t, map[string]bool{"gw1": true})
	rec := keptSandboxSeatRecord{Bead: "gt-other", Reason: gatewayNoAnswerKeepReason(p.clk.Now()), Since: p.clk.Now()}
	if err := writeKeptSandboxSeat(p.town, endRig, "basalt", rec); err != nil {
		t.Fatal(err)
	}
	// One registry record the gateway still has: the walk asks it twice.
	if err := session.WriteSandboxRecord(p.town, session.SandboxRecord{Session: "gt-w00", Gateway: "gw1", CreatedAt: p.clk.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	p.host.sandboxStatus = func(gateway, name string) (sandbox.Status, error) {
		*asked = append(*asked, "phase@"+gateway)
		return readySandboxStatus(gateway, name)
	}
	p.host.sandboxEndedMarker = func(string, string) (bool, error) { return false, nil }
	p.host.sandboxRecordBudget = 1
	p.host.agentRecord = func(string, string) (string, error) { return endSandboxIn, nil }

	const total = 1*gatewayAsksPerTickFactor + 1
	p.host.patrolScanSandboxRecords()
	if len(*asked) != total {
		t.Fatalf("tick 1 asked the gateway %d times %v, want exactly the tick's budget of %d", len(*asked), *asked, total)
	}
	if want := "kept-record drop pass stopped at the tick's gateway budget; the rest wait"; !strings.Contains(logs.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, logs.String())
	}
	before := len(*asked)
	p.host.patrolScanSandboxRecords()
	if got := len(*asked) - before; got > total {
		t.Errorf("tick 2 asked %d times, want at most %d", got, total)
	}
	if got := strings.Count(logs.String(), "kept-record drop pass stopped at the tick's gateway budget"); got != 1 {
		t.Errorf("the budget stop was logged %d times over two ticks that both stopped at it, want once:\n%s", got, logs.String())
	}
}

// A seat whose gateway cannot be resolved keeps its record, asks no gateway and
// logs one line; the same keep on the next tick is not logged again, and a
// changed reason is (gt-5lel7f).
func TestPatrolScanKeptRecordNoSandboxUnresolvedSeatLogsOncePerReason(t *testing.T) {
	t.Parallel()
	p, asked, logs := keptNoSandboxPatrol(t, map[string]bool{})
	p.host.agentRecord = func(string, string) (string, error) { return "", errors.New("agent bead unreadable") }
	p.host.patrolScanSandboxRecords()
	p.host.patrolScanSandboxRecords()

	if n := keptSeatCount(t, p.town); n != 1 {
		t.Errorf("kept seats = %d, want the record kept", n)
	}
	if len(*asked) != 0 {
		t.Errorf("gateways asked = %v, want none", *asked)
	}
	want := "kept record of " + endRig + "/" + endPolecat + " kept: cannot resolve its gateway: reading the agent bead: agent bead unreadable"
	if got := strings.Count(logs.String(), want); got != 1 {
		t.Errorf("the keep was logged %d times over two ticks, want once:\n%s", got, logs.String())
	}
	p.host.agentRecord = func(string, string) (string, error) { return "", errors.New("another reason") }
	p.host.patrolScanSandboxRecords()
	if !strings.Contains(logs.String(), "reading the agent bead: another reason") {
		t.Errorf("a changed reason was not logged:\n%s", logs.String())
	}
}

// The patrol offers a seat's end only once it has removed the sandbox's record
// and can read the landing facts from the kept record: a removal that fails and
// a kept record that cannot be read each leave the keep as it is (gt-cz5tdz).
func TestPatrolScanOffersNoSeatEndItCannotReadTheFactsFor(t *testing.T) {
	t.Parallel()
	landing := keptSeatLanding()
	tests := []struct {
		name string
		// noSeat is a registry record that names no rig or polecat: no kept
		// record can exist for it, so nothing is offered and nothing is logged.
		noSeat    bool
		removeErr error
		corrupt   bool
		wantLog   string
	}{
		{
			name:      "a removal that fails",
			removeErr: errors.New("disk full"),
			wantLog:   "sandbox record gt-agate: removing it failed: disk full",
		},
		{
			name:    "a kept record that cannot be read",
			corrupt: true,
			wantLog: "sandbox seat end gastown/agate: its kept record is unreadable, so its seat end is not completed",
		},
		{
			name:   "a record that names no seat",
			noSeat: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			clk := newFixedClock()
			reg := session.SandboxRecord{
				Session: "gt-agate", Rig: endRig, Polecat: "agate", Gateway: "gw1",
				CreatedAt: clk.Now().Add(-time.Hour),
			}
			if tt.noSeat {
				reg.Rig, reg.Polecat = "", ""
			}
			if err := session.WriteSandboxRecord(town, reg); err != nil {
				t.Fatal(err)
			}
			if !tt.noSeat {
				path, err := keptSandboxSeatPath(town, endRig, "agate")
				if err != nil {
					t.Fatal(err)
				}
				if tt.corrupt {
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
						t.Fatal(err)
					}
				} else if err := writeKeptSandboxSeat(town, endRig, "agate", keptSandboxSeatRecord{
					Bead: endBead, Reason: "the gateway has not confirmed the sandbox gone", Since: clk.Now(), Landing: &landing,
				}); err != nil {
					t.Fatal(err)
				}
			}
			var logs strings.Builder
			var calls []string
			d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(&logs, "", 0), tmux: newFakeTmux(clk), clock: clk}
			d.endSandboxSeat = func(rig, name string, _ sandboxSeatLanding) bool {
				calls = append(calls, rig+"/"+name)
				return true
			}
			h := &patrolScanHost{
				d:             d,
				sandboxExists: func(string, string) (bool, error) { return false, nil },
			}
			if tt.removeErr != nil {
				h.removeSandboxRecord = func(string, string) error { return tt.removeErr }
			}

			h.patrolScanSandboxRecords()

			if len(calls) != 0 {
				t.Errorf("seat ends offered = %v, want none", calls)
			}
			if tt.removeErr == nil {
				if left := session.ListSandboxSessions(town); len(left) != 0 {
					t.Errorf("registry records left = %v, want none", left)
				}
			}
			if tt.wantLog == "" {
				if strings.Contains(logs.String(), "sandbox seat end") {
					t.Errorf("log names a seat end for a record that names no seat:\n%s", logs.String())
				}
			} else if !strings.Contains(logs.String(), tt.wantLog) {
				t.Errorf("log lacks %q:\n%s", tt.wantLog, logs.String())
			}
		})
	}
}

// A seat end that does not complete is logged and does not stop the pass: every
// other record is still removed and its seat's end offered, with the landing
// facts its kept record holds (gt-cz5tdz).
func TestPatrolScanSeatEndFailureDoesNotStopThePass(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	clk := newFixedClock()
	seats := []struct{ session, polecat string }{
		{"gt-agate", "agate"},
		{"gt-bismuth", "bismuth"},
	}
	landing := keptSeatLanding()
	for _, s := range seats {
		if err := session.WriteSandboxRecord(town, session.SandboxRecord{
			Session: s.session, Rig: endRig, Polecat: s.polecat, Gateway: "gw1",
			CreatedAt: clk.Now().Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		if err := writeKeptSandboxSeat(town, endRig, s.polecat, keptSandboxSeatRecord{
			Bead: endBead, Reason: "the gateway has not confirmed the sandbox gone", Since: clk.Now(), Landing: &landing,
		}); err != nil {
			t.Fatal(err)
		}
	}
	var logs strings.Builder
	var ended []string
	var landed []sandboxSeatLanding
	d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(&logs, "", 0), tmux: newFakeTmux(clk), clock: clk}
	d.endSandboxSeat = func(rig, name string, l sandboxSeatLanding) bool {
		ended = append(ended, rig+"/"+name)
		landed = append(landed, l)
		return name != "agate" // the first seat's end fails here
	}
	h := &patrolScanHost{d: d, sandboxExists: func(string, string) (bool, error) { return false, nil }}

	h.patrolScanSandboxRecords()

	if want := []string{endRig + "/agate", endRig + "/bismuth"}; !reflect.DeepEqual(ended, want) {
		t.Errorf("seat ends offered = %v, want %v (the failed one does not stop the pass)", ended, want)
	}
	for _, got := range landed {
		if got != landing {
			t.Errorf("landing offered = %+v, want the recorded %+v", got, landing)
		}
	}
	if left := session.ListSandboxSessions(town); len(left) != 0 {
		t.Errorf("registry records left = %v, want none", left)
	}
	if want := "sandbox seat end gastown/agate: its seat end did not complete"; !strings.Contains(logs.String(), want) {
		t.Errorf("log lacks %q:\n%s", want, logs.String())
	}
}

// A re-run of a kept seat's end rewrites the kept record with the reason it
// decided on now, so the completion logs the reason the record holds after the
// run, never the stale one read before it. A record the re-run removed or left
// unreadable is named as such, with no reason (gt-z5ou4p).
func TestCompleteKeptSeatEndLogsTheReasonTheRerunLeft(t *testing.T) {
	t.Parallel()
	const (
		rig, name = "gastown", "capable"
		oldReason = "the seat works polecat/capable/gt-old+m1, not the landed polecat/capable/gt-landed+m2"
		newReason = "the seat works polecat/capable/gt-new+m3, not the landed polecat/capable/gt-landed+m2"
	)
	landing := sandboxSeatLanding{BeadID: "gt-landed", Branch: "polecat/capable/gt-landed+m2", Landed: true}
	tests := []struct {
		name string
		// rerun leaves the kept record the way the re-run's end leaves it.
		rerun   func(t *testing.T, town string, h *patrolScanHost)
		wantLog string

		// wantEnd, on a row whose re-run left no record, is where the
		// completion line must end: the record state, with no reason after it.
		wantEnd string
	}{
		{
			name: "a re-run that keeps the seat with a new reason",
			rerun: func(_ *testing.T, _ string, h *patrolScanHost) {
				h.recordKeptSeat(rig, name, landing, newReason)
			},
			wantLog: "its seat end did not complete after its sandbox was deleted; the keep stands (" + newReason + ")",
		},
		{
			name: "a re-run that removed the record",
			rerun: func(t *testing.T, town string, _ *patrolScanHost) {
				if err := removeKeptSandboxSeat(town, rig, name); err != nil {
					t.Fatal(err)
				}
			},
			wantLog: "its keep is gone",
			wantEnd: "its keep is gone (its kept record is not there)",
		},
		{
			name: "a re-run that left an unreadable record",
			rerun: func(t *testing.T, town string, _ *patrolScanHost) {
				path, err := keptSandboxSeatPath(town, rig, name)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantLog: "its kept record cannot be read",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			clk := newFixedClock()
			if err := writeKeptSandboxSeat(town, rig, name, keptSandboxSeatRecord{
				Bead: landing.BeadID, Reason: oldReason, Since: clk.Now(), Landing: &landing,
			}); err != nil {
				t.Fatal(err)
			}
			var logs strings.Builder
			d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(&logs, "", 0), clock: clk}
			h := &patrolScanHost{d: d}
			d.endSandboxSeat = func(string, string, sandboxSeatLanding) bool {
				tt.rerun(t, town, h)
				return false
			}

			h.completeKeptSeatEnd(session.SandboxRecord{Session: "gt-capable", Rig: rig, Polecat: name})

			if got := logs.String(); !strings.Contains(got, tt.wantLog) {
				t.Errorf("log lacks %q:\n%s", tt.wantLog, got)
			}
			if got := logs.String(); strings.Contains(got, oldReason) {
				t.Errorf("log names the stale reason %q:\n%s", oldReason, got)
			}
			if tt.wantEnd != "" {
				if got := strings.TrimSpace(logs.String()); !strings.HasSuffix(got, tt.wantEnd) {
					t.Errorf("line must end at %q with no reason after the record state:\n%s", tt.wantEnd, got)
				}
			}
		})
	}
}

// TestSandboxSeatEndCompletionsAskOutsideTheTickBudget drives the production
// seat end the pass completes a kept seat with, over a fake gateway at its
// exec boundary: each record the walk checks costs the walk one gateway
// question, and each completed seat end asks a question of its own. The walk
// still checks exactly its budget in records, and the completion's question is
// counted beside the walk's rather than against it.
func TestSandboxSeatEndCompletionsAskOutsideTheTickBudget(t *testing.T) {
	t.Parallel()
	const budget = 3
	town := t.TempDir()
	writeTownFile(t, town, "settings/config.json", endSettings)
	clk := newFixedClock()
	agentDB := beadsfake.New()
	names := []string{"p00", "p01", "p02", "p03"}
	landing := sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
	for _, n := range names {
		// The record's session name is the one the seat's end asks about, the
		// name the launch writes, not the polecat name its kept record is keyed by.
		if err := session.WriteSandboxRecord(town, session.SandboxRecord{
			Session: "gt-" + n, Rig: endRig, Polecat: n, Gateway: "gw1", CreatedAt: clk.Now().Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		if err := writeKeptSandboxSeat(town, endRig, n, keptSandboxSeatRecord{
			Bead: endBead, Reason: sandboxNotGoneReason, Since: clk.Now().Add(-time.Hour), Landing: &landing,
		}); err != nil {
			t.Fatal(err)
		}
		id := beads.AgentBeadIDFor(constants.RolePolecat, endRig, n, town)
		fields := beads.AgentFields{
			RoleType: "polecat", Rig: endRig, Agent: endSandboxIn,
			Branch: endBranch, CleanupStatus: string(polecat.CleanupClean),
		}
		if _, err := agentDB.Create(beads.CreateOptions{
			ID: id, Title: id, Labels: []string{"gt:agent"}, Priority: -1,
			Description: beads.FormatAgentDescription(id, &fields),
		}); err != nil {
			t.Fatal(err)
		}
	}
	var walkAsks, gatewayAsks int
	asked := map[string]int{}
	d := &Daemon{
		config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0),
		tmux: newFakeTmux(clk), clock: clk,
		openRecoveryBeads: func([]string) beads.Client { return agentDB },
	}
	// The gateway is the seat end's own seam, and it has no sandbox of any
	// name: the existence question is the one the end asks.
	d.sandboxExists = func(gateway, name string) (bool, error) {
		gatewayAsks++
		asked[name]++
		return false, nil
	}
	h := &patrolScanHost{
		d: d,
		sandboxExists: func(gateway, name string) (bool, error) {
			walkAsks++
			return d.sandboxExists(gateway, name)
		},
		sandboxRecordBudget: budget,
	}

	h.patrolScanSandboxRecords()

	if walkAsks != budget {
		t.Errorf("the walk asked %d records, want its budget of %d", walkAsks, budget)
	}
	if len(asked) != budget {
		t.Errorf("the tick asked about %d seats, want the budget of %d it checked: %v", len(asked), budget, asked)
	}
	for name, n := range asked {
		if n != 2 {
			t.Errorf("seat %s was asked about %d times, want the walk's question and its end's: %v", name, n, asked)
		}
	}
	if want := 2 * walkAsks; gatewayAsks != want {
		t.Errorf("the tick asked the gateway %d questions, want %d: the walk's and the seat ends the walk completed", gatewayAsks, want)
	}
}

// A completion's seat-end questions are outside the budget: a walk record the
// gateway no longer has is removed with one question, the completion that
// follows asks its own, and the budget has counted only the walk's. The tick's
// gateway questions are at most the budget plus four per checked record
// (gt-5lel7f).
func TestPatrolScanKeptRecordNoSandboxCompletionIsOutsideTheBudget(t *testing.T) {
	t.Parallel()
	p, asked, _ := keptNoSandboxPatrol(t, map[string]bool{})
	p.registryRecord(t) // the seat's sandbox record, whose sandbox the gateway no longer has
	p.host.sandboxRecordBudget = 1

	p.host.patrolScanSandboxRecords()

	const budget = 1*gatewayAsksPerTickFactor + 1
	const completionMost = 4
	if p.host.asks.asked != 1 {
		t.Errorf("the budget counted %d questions, want 1: only the walk's existence question", p.host.asks.asked)
	}
	if len(*asked) < 2 {
		t.Errorf("gateway questions = %v, want the walk's and the completion's", *asked)
	}
	if len(*asked) > budget+completionMost*1 {
		t.Errorf("gateway questions = %d, want at most %d: the budget plus four per checked record", len(*asked), budget+completionMost)
	}
}

// A sandbox record that a sling writes between the drop pass's first read and
// the seat lock keeps the kept record: the walk decides that seat now (gt-5lel7f).
func TestPatrolScanKeptRecordNoSandboxSlingRaceKeepsTheRecord(t *testing.T) {
	t.Parallel()
	p, _, _ := keptNoSandboxPatrol(t, map[string]bool{})
	reads := 0
	p.host.sandboxRecordRead = func(string, string) (session.SandboxRecord, bool, error) {
		reads++
		if reads == 1 {
			return session.SandboxRecord{}, false, nil // absent at the outer read
		}
		return session.SandboxRecord{Session: "written-by-the-sling"}, true, nil // present under the lock
	}
	removed := 0
	p.host.removeKeptSeat = func(string, string, string) error { removed++; return nil }
	p.host.asks = newGatewayAskBudget(sandboxRecordsPerTick)

	p.host.dropKeptSandboxSeatsWithoutRecord()

	if reads < 2 {
		t.Fatalf("the sandbox record was read %d times, want a re-read under the lock", reads)
	}
	if removed != 0 || keptSeatCount(t, p.town) != 1 {
		t.Errorf("removed %d, kept seats %d: want the record kept", removed, keptSeatCount(t, p.town))
	}
}

// A sandbox record that cannot be re-read under the lock keeps the kept record
// and is logged once, not on every tick (gt-5lel7f).
func TestPatrolScanKeptRecordNoSandboxLockedReadErrorLogsOnce(t *testing.T) {
	t.Parallel()
	p, _, logs := keptNoSandboxPatrol(t, map[string]bool{})
	reads := 0
	p.host.sandboxRecordRead = func(string, string) (session.SandboxRecord, bool, error) {
		reads++
		if reads%2 == 1 {
			return session.SandboxRecord{}, false, nil
		}
		return session.SandboxRecord{}, false, errors.New("registry unreadable")
	}
	p.host.asks = newGatewayAskBudget(sandboxRecordsPerTick)
	p.host.dropKeptSandboxSeatsWithoutRecord()
	p.host.dropKeptSandboxSeatsWithoutRecord()

	if keptSeatCount(t, p.town) != 1 {
		t.Error("the kept record was removed although its sandbox record could not be re-read")
	}
	want := "kept: its sandbox record cannot be re-read: registry unreadable"
	if got := strings.Count(logs.String(), want); got != 1 {
		t.Errorf("the keep was logged %d times over two passes, want once:\n%s", got, logs.String())
	}
}
