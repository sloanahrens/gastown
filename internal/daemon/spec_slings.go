package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// Background slings (gt-5h70s).
//
// A sling spawns a worktree and a session, and on a loaded host it takes from
// one to five minutes. The tick used to wait for its slowest sling before the
// next tick could start. The tick now plans and returns (`gt spec dispatch
// --background-slings`), handing each sling back as a job; the daemon runs each
// job as `gt spec sling --job` on its own goroutine and keeps it in this
// in-flight set until the sling returns. Every later tick is handed the set
// (--in-flight), so it neither slings a running bead again nor gives out a seat
// a running sling is filling.
//
// The set lives in daemon memory. A restart loses it, so an upgrade restart
// waits for the set to empty, up to the sling's own bound (upgradeHold), and
// no sling starts once a restart is pending.

// specSlingHoldLabel is what the upgrade-restart wait line calls the slings in
// flight.
const specSlingHoldLabel = "a spec dispatch sling"

// specSlingFlight is one running sling. job is the tick's job object, passed
// to `gt spec sling` as it came; the bead and its seat are the only parts of
// it the daemon reads, the job's file and test lists being the sling's own
// business (gt-mx656).
type specSlingFlight struct {
	Bead  string `json:"bead"`
	Rig   string `json:"rig"`
	Agent string `json:"agent"`

	job     json.RawMessage
	started time.Time
	// tickSeq is the tick that planned the sling: its outcome is counted back
	// into that tick's record and idle run (gt-pk2q8).
	tickSeq uint64
}

// specSlingResult is how a background sling ended, as the tick that planned it
// counts it: the tick counts every job it hands over as dispatched until the
// sling reports back (gt-pk2q8).
type specSlingResult int

const (
	// specSlingStarted hooked its bead, so the tick's dispatched count stands.
	specSlingStarted specSlingResult = iota
	// specSlingLost ran and came back deferred or failed without hooking its
	// bead: the tick attempted the sling and lost it.
	specSlingLost
	// specSlingNotStarted never ran, so the tick dispatched nothing.
	specSlingNotStarted
)

// specSlings is the in-flight set, keyed by bead: one running sling per bead.
type specSlings struct {
	mu      sync.Mutex
	flights map[string]specSlingFlight
}

// begin adds f to the set unless its bead already has a sling running, and
// reports whether it was added.
func (s *specSlings) begin(f specSlingFlight) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, running := s.flights[f.Bead]; running {
		return false
	}
	if s.flights == nil {
		s.flights = map[string]specSlingFlight{}
	}
	s.flights[f.Bead] = f
	return true
}

// end removes the bead's sling from the set.
func (s *specSlings) end(bead string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.flights, bead)
}

// list is the running slings in bead order.
func (s *specSlings) list() []specSlingFlight {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]specSlingFlight, 0, len(s.flights))
	for _, f := range s.flights {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bead < out[j].Bead })
	return out
}

// newest is when the youngest running sling started; false when none runs. The
// upgrade restart freezes this start as its wait's origin rather than taking
// the oldest's: a sling outlives its own bound only until the kill reaches it,
// and the younger ones behind it may still have most of theirs left (gt-nosod).
func (s *specSlings) newest() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var at time.Time
	for _, f := range s.flights {
		if at.IsZero() || f.started.After(at) {
			at = f.started
		}
	}
	return at, !at.IsZero()
}

// specSlingBound is how long one background sling may run: the one-sling bound
// the tick used to give a sling.
func specSlingBound() time.Duration { return specDispatchTimeout }

// specSlingContext is the context one background sling runs under: its own
// bound nested in the daemon's context, so a sling cut by a shutdown and one
// that ran out of time are told apart by their contexts. A test replaces it
// (specSlingCtx) so the sling's bound is already spent when its process is run,
// without a real wait (gt-vaivp).
func (d *Daemon) specSlingContext() (context.Context, context.CancelFunc) {
	if d.specSlingCtx != nil {
		return d.specSlingCtx(d.ctx)
	}
	return context.WithTimeout(d.ctx, specSlingBound())
}

// specSlingTimeoutRetries bounds how many slings a bead whose process is killed
// at its bound is given. A kill at the bound is the host's load reaching the
// sling, not a defect in the bead, so the dispatcher clears the label this kill
// writes and slings the bead again; past this many kills the load is not
// clearing, and the label stays for a person (gt-vaivp).
const specSlingTimeoutRetries = specdispatch.TimeoutRetries

// specTickFlight is a running sling as the tick reads it: the bead, the seat it
// occupies, and the start the stagger measures from (gt-vames). The job's file
// and test lists stay out: the tick never reads them (gt-mx656).
type specTickFlight struct {
	Bead  string `json:"bead"`
	Rig   string `json:"rig"`
	Agent string `json:"agent"`
	// omitzero: omitempty has no effect on a time.Time, so a flight with no
	// start time would be written as 0001-01-01T00:00:00Z (gt-fzcge).
	Started time.Time `json:"started,omitzero"`
}

// specTickArgs are the flags every tick runs with: the slings are handed back
// rather than run, and the running ones are named so the tick leaves their
// beads and seats alone. A set that cannot be written down is an error: a tick
// run without it would sling a running bead again, so the caller skips the tick.
func (d *Daemon) specTickArgs() ([]string, error) {
	args := []string{"--background-slings"}
	if flights := d.specSlings.list(); len(flights) > 0 {
		payload := make([]specTickFlight, 0, len(flights))
		for _, f := range flights {
			payload = append(payload, specTickFlight{Bead: f.Bead, Rig: f.Rig, Agent: f.Agent, Started: f.started})
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("writing the in-flight slings: %w", err)
		}
		args = append(args, "--in-flight", string(raw))
	}
	return args, nil
}

// startSpecSlings runs each job a tick handed back in the background. It
// starts none while an upgrade restart is pending: the restart would cut it
// mid-sling (gt-cbaoy), and the bead stays ready for the new daemon's first
// tick. A job it does not run was counted as dispatched by the tick, so its
// outcome is fed back as not started (gt-pk2q8).
func (d *Daemon) startSpecSlings(jobs []json.RawMessage, tickSeq uint64) {
	for _, raw := range jobs {
		var f specSlingFlight
		f.tickSeq = tickSeq
		if err := json.Unmarshal(raw, &f); err != nil || f.Bead == "" {
			d.logger.Printf("spec_dispatch: sling job unreadable, not run: %v", err)
			// The job names no bead, so each reader drops one lost dispatch
			// without naming which (gt-pk2q8).
			reason := "the job names no bead"
			if err != nil {
				reason = err.Error()
			}
			d.feedSpecSlingOutcome(f, specSlingNotStarted, specDispatchTickEntry{
				Line: "sling job unreadable, not run: " + reason})
			continue
		}
		if d.upgradeRestartPending.Load() {
			reason := "an upgrade restart is pending; the bead stays ready for the next tick after it"
			d.logger.Printf("spec_dispatch: %s: sling not started: %s", f.Bead, reason)
			d.feedSpecSlingOutcome(f, specSlingNotStarted, specSlingNotStartedEntry(f.Bead, reason))
			continue
		}
		f.job = raw
		f.started = d.clk().Now()
		if !d.specSlings.begin(f) {
			reason := "one is already running for it"
			d.logger.Printf("spec_dispatch: %s: sling not started: %s", f.Bead, reason)
			d.feedSpecSlingOutcome(f, specSlingNotStarted, specSlingNotStartedEntry(f.Bead, reason))
			continue
		}
		d.specDispatchCycles.Add(1)
		go func() {
			defer d.specDispatchCycles.Done()
			defer d.specSlings.end(f.Bead)
			d.runSpecSling(f)
		}()
	}
}

// specSlingNotStartedReason heads the entry of a planned bead the daemon did
// not start, which the idle rule reads as work waiting on neither the
// dispatcher nor a person (gt-pk2q8).
const specSlingNotStartedReason = "sling not started: "

// specSlingNotStartedEntry is the waiting-work entry a job that never ran
// leaves on its tick.
func specSlingNotStartedEntry(bead, reason string) specDispatchTickEntry {
	return specDispatchTickEntry{Bead: bead, Line: bead + ": " + specSlingNotStartedReason + reason}
}

// runSpecSling runs one sling job, logs how it ended and feeds the outcome back
// to the tick that planned it. A sling that fails inside `gt spec sling` is
// labeled and annotated by it, as a failed foreground sling is (spec.go
// specApplyOutcome). A sling whose process is killed at its bound or exits with
// no report never reached those writes, so the daemon makes them
// (failSpecSling).
func (d *Daemon) runSpecSling(f specSlingFlight) {
	ctx, cancel := d.specSlingContext()
	defer cancel()
	cmd := exec.CommandContext(ctx, d.gtPath, "spec", "sling", "--job", string(f.job)) //nolint:gosec // G204: gtPath resolved at daemon init
	cmd.Dir = d.config.TownRoot
	cmd.Env = daemonGTEnv(os.Environ())
	start := d.clk().Now()
	out, stderr, err := d.runCmd(cmd)
	elapsed := d.clk().Now().Sub(start).Round(time.Second)
	if err != nil {
		reason := fmt.Sprintf("sling process failed after %s of its %s bound: %v %s", elapsed, specSlingBound(), err, lastLine(string(stderr)))
		// The sling's own deadline is what a kill at the bound reports: the
		// daemon's ctx is untouched, so a cancelled sling is the shutdown
		// path, which failSpecSling leaves alone.
		killedAtBound := errors.Is(ctx.Err(), context.DeadlineExceeded)
		d.logger.Printf("spec_dispatch: %s: %s", f.Bead, reason)
		d.failSpecSling(f, reason, killedAtBound)
		d.feedSpecSlingOutcome(f, specSlingLost, specDispatchTickEntry{Bead: f.Bead, Rig: f.Rig,
			Line: f.Bead + ": sling process failed, left unassigned: " + reason})
		return
	}
	r, perr := parseSpecDispatchReport(out)
	for _, line := range d.formatSpecSlingReport(r, perr) {
		d.logger.Printf("spec_dispatch: %s (sling took %s)", line, elapsed)
	}
	// An unreadable report cannot show the bead hooked, so the tick must not
	// keep counting it dispatched: the sling is a loss the health records see
	// (gt-pk2q8).
	if perr != nil {
		d.feedSpecSlingOutcome(f, specSlingLost, specDispatchTickEntry{Bead: f.Bead, Rig: f.Rig,
			Line: f.Bead + ": sling output unreadable: " + perr.Error()})
		return
	}
	result, entry := specSlingResultOf(r, f.Bead)
	d.feedSpecSlingOutcome(f, result, entry)
}

// specSlingResultOf reads how one sling job ended out of its report: `gt spec
// sling --job` makes one decision about its bead, and a report naming none of
// them says the bead was not hooked.
func specSlingResultOf(r specDispatchTickReport, bead string) (specSlingResult, specDispatchTickEntry) {
	var entry specDispatchTickEntry
	switch {
	case len(r.Dispatched) > 0:
		return specSlingStarted, r.Dispatched[0]
	case len(r.Failed) > 0:
		entry = r.Failed[0]
	case len(r.Skipped) > 0:
		entry = r.Skipped[0]
	case len(r.Deferred) > 0:
		entry = r.Deferred[0]
	case r.Hold != "":
		return specSlingNotStarted, specDispatchTickEntry{Bead: bead, Line: bead + ": held: sling not made: " + r.Hold}
	default:
		return specSlingLost, specDispatchTickEntry{Bead: bead, Line: bead + ": sling reported no decision"}
	}
	if entry.Bead == "" {
		entry.Bead = bead
	}
	if entry.Line == "" {
		entry.Line = bead + ": sling did not start"
	}
	return specSlingLost, entry
}

// feedSpecSlingOutcome records one background sling's result where the two
// health readers keep it: the tick record townhealth judges, and the idle run
// the attention rule judges (gt-pk2q8).
func (d *Daemon) feedSpecSlingOutcome(f specSlingFlight, result specSlingResult, entry specDispatchTickEntry) {
	d.correctDispatchTick(f.tickSeq, result)
	d.dispatchIdle.resolveSling(f.tickSeq, f.Bead, entry, result)
}

// correctDispatchTick takes one sling's outcome out of the tick that planned
// it. A sling that never started joins the tick's skipped count; one that ran
// and was lost joins its failed count, which the dispatch field reads as a tick
// that attempted and lost rather than one that turned a bead away on purpose
// (gt-pk2q8).
func (d *Daemon) correctDispatchTick(seq uint64, result specSlingResult) {
	if result == specSlingStarted || seq == 0 {
		return
	}
	d.dispatchTicksMu.Lock()
	defer d.dispatchTicksMu.Unlock()
	for i := len(d.dispatchTicks) - 1; i >= 0; i-- {
		t := &d.dispatchTicks[i]
		if t.seq != seq {
			continue
		}
		if t.Dispatched > 0 {
			t.Dispatched--
		}
		if result == specSlingLost {
			t.Failed++
		} else {
			t.Skipped++
		}
		return
	}
}

// failSpecSling labels and annotates the bead of a sling whose process died
// without reporting, as a failed foreground sling does: the label holds the
// bead out of the queue until an operator clears it.
//
// A kill at the sling's own bound is the host's load, not the bead: the note
// records the kill as an attempt, and the dispatcher clears the label and slings
// the bead again until specSlingTimeoutRetries kills have spent the retries
// (gt-vaivp). A kill between the hook and the session start leaves a hook
// nothing works, so the bead is unslung; a polecat with a session keeps its
// bead, since nothing clears the label if that polecat later dies (gt-nosod). A
// daemon shutting down cut the sling itself and leaves the bead as it is.
func (d *Daemon) failSpecSling(f specSlingFlight, reason string, killedAtBound bool) {
	if d.ctx.Err() != nil {
		return
	}
	issue, err := d.workBeads(d.workBeadsEnv(f.Rig), workBeadReadTimeout).Show(f.Bead)
	if err != nil {
		// The label is the one write nothing clears without an operator, so a
		// read that failed withholds it: no read is no evidence the polecat
		// does not own the bead (gt-nosod).
		d.logger.Printf("spec_dispatch: %s: the bead could not be read before the failure label; leaving it unlabeled", f.Bead)
		return
	}
	held, known := specSlingBeadHeld(issue)
	if !known {
		// An empty or unrecognized status is no answer: the label is the one
		// write nothing clears, so only a status that says the bead is not held
		// lets it be written (gt-nosod).
		d.logger.Printf("spec_dispatch: %s: the bead's status is no answer for the failure label; leaving it unlabeled", f.Bead)
		return
	}
	if held && !d.handBackHalfHooked(f, issue) {
		d.logger.Printf("spec_dispatch: %s: sling died after hooking it; leaving the bead to its polecat and writing no %s label",
			f.Bead, constants.LabelSpecDispatchFailed)
		return
	}
	note := "spec-dispatch: " + reason + "; excluded from the queue until label " + constants.LabelSpecDispatchFailed + " is cleared"
	if killedAtBound {
		attempt := specdispatch.TimeoutAttempts(issue.Notes) + 1
		note = fmt.Sprintf("%s sling killed at its %s bound (attempt %d of %d); %s: %s",
			specdispatch.TimeoutNoteMarker, specSlingBound(), attempt, specSlingTimeoutRetries,
			specSlingTimeoutNext(attempt), reason)
	}
	for _, args := range [][]string{
		{"bead", "update", f.Bead, "--add-label", constants.LabelSpecDispatchFailed},
		{"bead", "note", f.Bead, note},
	} {
		d.beadWrite(f, args...)
	}
}

// specSlingTimeoutNext is what the note of attempt says happens next: the
// dispatcher clears the label and slings the bead again while a retry is left,
// and the label stays for a person once the retries are spent.
func specSlingTimeoutNext(attempt int) string {
	if attempt < specSlingTimeoutRetries {
		return "the dispatcher clears this label and slings the bead again"
	}
	return "the retries are spent, so the label stays until an operator clears it"
}

// specSlingBeadHeld reports whether the bead a dead sling was running is hooked
// or in progress, so the failure label does not hold out work a polecat owns,
// and whether that could be answered at all. An empty or unrecognized status is
// no answer: known is false, which the caller must read as "leave the bead
// alone", as a failed read is in workBeadStillSubmitted (gt-nosod).
func specSlingBeadHeld(issue *beads.Issue) (held, known bool) {
	switch beads.IssueStatus(strings.TrimSpace(issue.Status)) {
	case beads.IssueStatusHooked, beads.StatusInProgress:
		return true, true
	case beads.StatusOpen, beads.StatusClosed, beads.StatusBlocked, beads.StatusDeferred,
		beads.IssueStatusPinned, beads.StatusTombstone:
		return false, true
	}
	return false, false
}

// handBackHalfHooked unslings a bead a killed sling hooked without starting its
// polecat's session, and reports whether the bead is no longer held. A polecat
// that has a session owns its bead, and unslinging it would take work away from
// an agent mid-task, so the release is gated on the session being gone. A
// daemon with no tmux, or a tmux that cannot answer, hands nothing back: an
// unanswered read is no proof the seat is dead, as the stale-hook reap reads it.
func (d *Daemon) handBackHalfHooked(f specSlingFlight, issue *beads.Issue) bool {
	holder := strings.TrimSpace(issue.Assignee)
	parts := strings.Split(holder, "/")
	if len(parts) != 3 || parts[1] != "polecats" {
		return false
	}
	alive, known := d.polecatSessionAlive(parts[0], parts[2])
	if !known {
		d.logger.Printf("spec_dispatch: %s: cannot tell whether %s has a session; leaving the bead hooked", f.Bead, holder)
		return false
	}
	if alive {
		return false
	}
	cmd := exec.CommandContext(d.ctx, d.gtPath, "unsling", f.Bead, holder) //nolint:gosec // G204: gtPath resolved at daemon init
	cmd.Dir = d.config.TownRoot
	cmd.Env = daemonGTEnv(os.Environ())
	if _, stderr, err := d.runCmd(cmd); err != nil {
		d.logger.Printf("spec_dispatch: %s: handing the bead back from %s failed: %v %s", f.Bead, holder, err, lastLine(string(stderr)))
		return false
	}
	d.logger.Printf("spec_dispatch: %s: sling died after hooking it and before %s had a session; handed the bead back", f.Bead, holder)
	return true
}

// polecatSessionAlive reports whether rig/polecats/name has a live session, and
// whether tmux could be asked at all.
func (d *Daemon) polecatSessionAlive(rig, polecat string) (alive, known bool) {
	if d.tmux == nil {
		return false, false
	}
	name := session.PolecatSessionName(d.prefixRegistry().PrefixForRig(rig), polecat)
	alive, err := d.tmux.HasSession(name)
	if err != nil {
		return false, false
	}
	return alive, true
}

// beadWrite runs one gt bead verb for f in the town root and logs a failure. A
// failed write is reported, not fatal: the sling is already out of the in-flight
// set, and the next tick reads the bead as it stands.
func (d *Daemon) beadWrite(f specSlingFlight, args ...string) {
	cmd := exec.CommandContext(d.ctx, d.gtPath, args...) //nolint:gosec // G204: gtPath resolved at daemon init
	cmd.Dir = d.config.TownRoot
	cmd.Env = daemonGTEnv(os.Environ())
	if _, stderr, err := d.runCmd(cmd); err != nil {
		d.logger.Printf("spec_dispatch: %s: gt %s failed: %v %s", f.Bead, args[1], err, lastLine(string(stderr)))
	}
}

// formatSpecSlingReport renders a sling's report as log lines: one per entry,
// under the name the tick's line gives the same entry.
func (d *Daemon) formatSpecSlingReport(r specDispatchTickReport, parseErr error) []string {
	if parseErr != nil {
		return []string{fmt.Sprintf("unparseable sling output (%v)", parseErr)}
	}
	if r.Hold != "" {
		return []string{"held: sling not made: " + r.Hold}
	}
	var lines []string
	for _, group := range []struct {
		name    string
		entries []specDispatchTickEntry
	}{{"dispatched", r.Dispatched}, {"failed", r.Failed}, {"deferred", r.Deferred}} {
		for _, e := range group.entries {
			lines = append(lines, group.name+": "+e.Line)
		}
	}
	for _, n := range r.Notices {
		lines = append(lines, "note: "+n)
	}
	for _, e := range r.Errors {
		lines = append(lines, "error: "+e)
	}
	return lines
}
