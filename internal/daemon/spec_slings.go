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

	job     json.RawMessage
	started time.Time
}

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

// specTickArgs are the flags every tick runs with: the slings are handed back
// rather than run, and the running ones are named so the tick leaves their
// beads and seats alone.
func (d *Daemon) specTickArgs() []string {
	args := []string{"--background-slings"}
	if flights := d.specSlings.list(); len(flights) > 0 {
		raw, err := json.Marshal(flights)
		if err == nil {
			args = append(args, "--in-flight", string(raw))
		}
	}
	return args
}

// startSpecSlings runs each job a tick handed back in the background. It
// starts none while an upgrade restart is pending: the restart would cut it
// mid-sling (gt-cbaoy), and the bead stays ready for the new daemon's first
// tick.
func (d *Daemon) startSpecSlings(jobs []json.RawMessage) {
	for _, raw := range jobs {
		var f specSlingFlight
		if err := json.Unmarshal(raw, &f); err != nil || f.Bead == "" {
			d.logger.Printf("spec_dispatch: sling job unreadable, not run: %v", err)
			continue
		}
		if d.upgradeRestartPending.Load() {
			d.logger.Printf("spec_dispatch: %s: sling not started: an upgrade restart is pending; the bead stays ready for the next tick after it", f.Bead)
			continue
		}
		f.job = raw
		f.started = d.clk().Now()
		if !d.specSlings.begin(f) {
			d.logger.Printf("spec_dispatch: %s: sling not started: one is already running for it", f.Bead)
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

// runSpecSling runs one sling job and logs how it ended. A failed sling is
// labeled and annotated by `gt spec sling` itself, as a failed foreground
// sling is (spec.go specApplyOutcome); the daemon only reports it.
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
		msg := lastLine(string(stderr))
		d.logger.Printf("spec_dispatch: %s: sling failed after %s of its %s bound: %v %s (its report is lost; the next tick reads the bead as it stands)", f.Bead, elapsed, specSlingBound(), err, msg)
		return
	}
	for _, line := range d.formatSpecSlingReport(out) {
		d.logger.Printf("spec_dispatch: %s (sling took %s)", line, elapsed)
	}
}

// formatSpecSlingReport renders a sling's report as log lines: one per entry,
// under the name the tick's line gives the same entry.
func (d *Daemon) formatSpecSlingReport(out []byte) []string {
	r, err := parseSpecDispatchReport(out)
	if err != nil {
		return []string{fmt.Sprintf("unparseable sling output (%v)", err)}
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
