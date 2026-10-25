package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
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
// to `gt spec sling` as it came; the daemon reads only the fields below.
type specSlingFlight struct {
	Bead  string   `json:"bead"`
	Rig   string   `json:"rig"`
	Agent string   `json:"agent"`
	Paths []string `json:"paths,omitempty"`
	Tests []string `json:"tests,omitempty"`

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

// oldest is when the longest-running sling started; false when none runs.
func (s *specSlings) oldest() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var at time.Time
	for _, f := range s.flights {
		if at.IsZero() || f.started.Before(at) {
			at = f.started
		}
	}
	return at, !at.IsZero()
}

// specSlingBound is how long one background sling may run: the one-sling bound
// the tick used to give a sling.
func specSlingBound() time.Duration { return specDispatchTimeout }

// specTickFlight is a running sling as the tick reads it: the set's fields, and
// the start the stagger measures from (gt-vames).
type specTickFlight struct {
	specSlingFlight
	Started time.Time `json:"started,omitempty"`
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
			payload = append(payload, specTickFlight{specSlingFlight: f, Started: f.started})
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
	ctx, cancel := context.WithTimeout(d.ctx, specSlingBound())
	defer cancel()
	cmd := exec.CommandContext(ctx, d.gtPath, "spec", "sling", "--job", string(f.job)) //nolint:gosec // G204: gtPath resolved at daemon init
	cmd.Dir = d.config.TownRoot
	cmd.Env = daemonGTEnv(os.Environ())
	start := d.clk().Now()
	out, stderr, err := d.runCmd(cmd)
	elapsed := d.clk().Now().Sub(start).Round(time.Second)
	if err != nil {
		reason := fmt.Sprintf("sling process failed after %s of its %s bound: %v %s", elapsed, specSlingBound(), err, lastLine(string(stderr)))
		d.logger.Printf("spec_dispatch: %s: %s", f.Bead, reason)
		d.failSpecSling(f, reason)
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
// bead out of the queue until an operator clears it. A daemon that is shutting
// down cut the sling itself, so the bead stays ready for the next daemon.
func (d *Daemon) failSpecSling(f specSlingFlight, reason string) {
	if d.ctx.Err() != nil {
		return
	}
	note := "spec-dispatch: " + reason + "; excluded from the queue until label " + constants.LabelSpecDispatchFailed + " is cleared"
	for _, args := range [][]string{
		{"bead", "update", f.Bead, "--add-label", constants.LabelSpecDispatchFailed},
		{"bead", "note", f.Bead, note},
	} {
		cmd := exec.CommandContext(d.ctx, d.gtPath, args...) //nolint:gosec // G204: gtPath resolved at daemon init
		cmd.Dir = d.config.TownRoot
		cmd.Env = daemonGTEnv(os.Environ())
		if _, stderr, err := d.runCmd(cmd); err != nil {
			d.logger.Printf("spec_dispatch: %s: gt %s failed: %v %s", f.Bead, args[1], err, lastLine(string(stderr)))
		}
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
