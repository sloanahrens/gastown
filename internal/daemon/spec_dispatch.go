package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// Spec dispatcher ticker (gt-4k3fj.5).
//
// Every interval (default 60s) the daemon runs `gt spec dispatch --json`: one
// tick of the spec dispatcher, which lints ready work beads and slings clean
// ones onto a free seat of their class (internal/cmd/spec.go). The decision
// lives in internal/cmd next to the pool accounting and the sling path, which
// the daemon cannot import; this side is the cadence, the single-flight guard
// and the log. On unless patrols.spec_dispatch.enabled is false.
//
// The ticker never restarts or kills anything, and it honors the operator
// hold file and ESTOP through the command itself.

const (
	defaultSpecDispatchInterval = 60 * time.Second

	// dispatchTickHistory bounds the tick records the daemon keeps for
	// townhealth's dispatch field: two hours at the default 60s interval,
	// far past the health window (gt-xiw7o).
	dispatchTickHistory = 120

	// specDispatchSkipCap bounds the skipped beads the tick line names; past
	// it the line says how many more it left unnamed (gt-gzav5).
	specDispatchSkipCap = 6
)

// specDispatchTimeout bounds a tick that slings one bead: the read of the
// queue and the pool, and one sling, which spawns a worktree and a session and
// takes minutes when the host is saturated. Each sling past the first adds
// specDispatchSlingAllowance (specDispatchBound).
//
// A var rather than a const so a test can drive the bound without waiting it
// out, as ProcessGroupKillGrace is (gt-7uyfc).
var specDispatchTimeout = 5 * time.Minute

// specDispatchSlingAllowance is the time each sling past the first adds to a
// tick's bound. A tick slings up to max_per_tick beads, and each one is a
// worktree and a session.
const specDispatchSlingAllowance = 5 * time.Minute

// specDispatchBound is how long a tick may run when it slings up to perTick
// beads: timeout for the first sling and the read, plus specDispatchSlingAllowance
// for each sling past the first. A perTick below one counts as one, so the
// bound is never zero.
func specDispatchBound(timeout time.Duration, perTick int) time.Duration {
	if perTick < 1 {
		perTick = 1
	}
	return timeout + time.Duration(perTick-1)*specDispatchSlingAllowance
}

// specDispatchMaxPerTick reads the max_per_tick the tick will run with from the
// patrol block the tick reads (gt-jdnab). An absent block counts as the default
// of one. A block that cannot be read comes back as one with an error, so the
// caller can say the bound fell back (gt-zeqqg).
func (d *Daemon) specDispatchMaxPerTick() (int, error) {
	if d.config == nil {
		return 1, nil
	}
	pc, err := ReadPatrolConfig(d.config.TownRoot)
	if err != nil {
		return 1, err
	}
	if pc == nil || pc.Patrols == nil || pc.Patrols.SpecDispatch == nil {
		return 1, nil
	}
	return pc.Patrols.SpecDispatch.MaxPerTick, nil
}

// specDispatchTickBound is the bound the next tick runs under (gt-bbfpr). A
// patrol block that cannot be read leaves it at the one-sling size, which the
// tick's own parse error does not say, so the log says it, once per distinct
// error (gt-zeqqg).
func (d *Daemon) specDispatchTickBound() time.Duration {
	perTick, err := d.specDispatchMaxPerTick()
	reason := ""
	if err != nil {
		reason = "patrol config unreadable, so the tick bound stays at the one-sling size: " + err.Error()
	}
	if d.specPatrolHold.Changed(reason) && d.logger != nil {
		if reason != "" {
			d.logger.Printf("spec_dispatch: %s", reason)
		} else {
			d.logger.Printf("spec_dispatch: patrol config readable again; tick bound follows max_per_tick")
		}
	}
	return specDispatchBound(specDispatchTimeout, perTick)
}

// specDispatchInterval returns the configured interval, or 60s.
func specDispatchInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.SpecDispatch != nil {
		if s := config.Patrols.SpecDispatch.IntervalStr; s != "" {
			if d, err := time.ParseDuration(s); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultSpecDispatchInterval
}

// specDispatchTickEntry is one bead the tick decided about: Bead names it and
// Line is the sentence for the log, "<bead>: <reason>".
type specDispatchTickEntry struct {
	Bead string `json:"bead"`
	Rig  string `json:"rig,omitempty"`
	Line string `json:"line"`
}

// specDispatchTickReport is the slice of the tick's JSON the daemon logs. Its
// tags mirror the dispatcher's report struct. Decoding ignores omitempty, so
// the keys it requires are checked in parseSpecDispatchReport instead.
type specDispatchTickReport struct {
	Hold       string `json:"hold,omitempty"`
	Roster     string `json:"roster"`
	Candidates int    `json:"candidates"`
	// LabeledFailed is the tick's count of ready beads held out of the queue
	// by the spec-dispatch-failed label, which the health field surfaces
	// (gt-q6zoo).
	LabeledFailed int `json:"labeled_failed,omitempty"`
	// DetailsFetched, DetailsCached and DetailsUnread are the tick's reads of
	// the pool beads' design and notes: shown by bd, answered from the cache,
	// and left for a later tick by the per-tick limit (gt-l0vlf).
	DetailsFetched int                     `json:"details_fetched,omitempty"`
	DetailsCached  int                     `json:"details_cached,omitempty"`
	DetailsUnread  int                     `json:"details_unread,omitempty"`
	Dispatched     []specDispatchTickEntry `json:"dispatched"`
	Refused        []specDispatchTickEntry `json:"refused"`
	Planning       []specDispatchTickEntry `json:"planning"`
	Skipped        []specDispatchTickEntry `json:"skipped"`
	Failed         []specDispatchTickEntry `json:"failed"`
	// Bypassed is the P0 and P1 candidates the tick dispatched despite an
	// overlap with work in flight, each naming what would have held it
	// (gt-iydx1). The tick line counts them.
	Bypassed []specDispatchTickEntry `json:"bypassed,omitempty"`
	// Deferred is the sling-stage part of Skipped, logged one line per entry
	// rather than only inside the tick line's capped skip list (gt-4wcqd).
	Deferred []specDispatchTickEntry `json:"deferred,omitempty"`
	Notices  []string                `json:"notices,omitempty"`
	Errors   []string                `json:"errors,omitempty"`
	// Pending is the slings the tick planned and handed back to run in the
	// background, each the tick's own job object (spec_slings.go, gt-5h70s).
	Pending []json.RawMessage `json:"pending,omitempty"`
	// The tick's phase times, for the 'tick took' line (gt-bqch0).
	RosterNs     time.Duration `json:"roster_ns,omitempty"`
	BoardNs      time.Duration `json:"board_ns,omitempty"`
	CandidatesNs time.Duration `json:"candidates_ns,omitempty"`
	SlingNs      time.Duration `json:"sling_ns,omitempty"`
	// The bd reads each phase made, which the same line names after its times
	// so a slow phase is also known by its call count (gt-139dp).
	RosterCalls     int `json:"roster_calls,omitempty"`
	BoardCalls      int `json:"board_calls,omitempty"`
	CandidatesCalls int `json:"candidates_calls,omitempty"`
	// seq is the daemon's own id for the tick, set after decoding: a
	// background sling's outcome names the tick that planned it, which the
	// tick's own report cannot (gt-pk2q8).
	seq uint64
}

// specDispatchTickRun is when the running tick started. It is published before
// specDispatchRunning is set and cleared with the flag, so an upgrade restart
// that sees a running tick sees that tick's start, never the one before it
// (gt-32j1y).
type specDispatchTickRun struct {
	started time.Time
	// bound is the bound the tick runs under, read once when it is triggered.
	// An upgrade restart waits on this value, not on a fresh read of the
	// config, so a max_per_tick edit or an unreadable config mid-tick cannot
	// shorten the wait below the time the tick is given.
	bound time.Duration
}

// triggerSpecDispatch starts one tick on its own goroutine unless one is
// already running, the town's automatic dispatch is parked, or the seat
// settings cannot be read. It reports whether a tick started.
//
// A parked town starts no tick (gt-y6ovz): the tick would return the hold
// report without deciding anything, and its single-flight flag is one of
// daemonWorkHolds, which the catch-up backup's quiet check samples. The
// maintenance tick and this one fire into the same pass of the run loop, so a
// parked tick in flight was the usual state at a deferred catch-up check, not
// a rare one, and a backup a day overdue starved behind a tick that did
// nothing.
func (d *Daemon) triggerSpecDispatch() bool {
	if reason := d.specDispatchTownHold(); reason != "" {
		if d.specDispatchHold.Changed(reason) {
			d.logger.Printf("spec_dispatch: not ticking: %s", reason)
		}
		// No tick runs under the hold, so the run of idle ticks ends here
		// rather than resuming across it (gt-icmdq).
		d.dispatchIdle.Reset()
		return false
	}
	if d.specDispatchHold.Changed("") {
		d.logger.Printf("spec_dispatch: operator dispatch hold lifted; ticker resuming")
	}
	// A pending restart waits for the tick in flight, up to that tick's own
	// bound: a tick started now would outlast the wait and be cut by the
	// restart, leaving a sling half done, so it starts after the restart, from
	// the new daemon (gt-cbaoy). The drain being off does not license one
	// (gt-u1fzb); specDispatchParked says which notice parks the ticker.
	if d.specDispatchParked() {
		if d.specDispatchDeferLogged.CompareAndSwap(false, true) {
			d.logger.Printf("spec_dispatch: not ticking: an upgrade restart is pending; the next tick runs after it")
		}
		d.dispatchIdle.Reset()
		return false
	}
	d.specDispatchDeferLogged.Store(false)
	// The seats are read here rather than at startup: an edit to the mix lands
	// at the next tick (spec_seats.go, gt-jdnab).
	if !d.specSeatsReady() {
		// A half-read seat picture starts no tick, so the run of idle ticks
		// ends here rather than resuming across it (gt-icmdq).
		d.dispatchIdle.Reset()
		return false
	}
	// The start is published before the flag: a restart that reads the flag set
	// must not be handed the previous tick's start, which would read as a tick
	// already past its bound and let the restart cut this one (gt-32j1y).
	run := &specDispatchTickRun{started: d.clk().Now(), bound: d.specDispatchTickBound()}
	prev := d.specDispatchTick.Swap(run)
	if !d.specDispatchRunning.CompareAndSwap(false, true) {
		d.specDispatchTick.CompareAndSwap(run, prev)
		d.logger.Printf("spec_dispatch: previous tick still running, skipping")
		return false
	}
	d.specDispatchCycles.Add(1)
	go func() {
		defer d.specDispatchCycles.Done()
		defer func() {
			// Flag first, then the start, and a compare-and-swap rather than a
			// store: a tick that starts in between keeps its own, and a reader
			// never sees a running tick without a start (gt-32j1y).
			d.specDispatchRunning.Store(false)
			d.specDispatchTick.CompareAndSwap(run, nil)
		}()
		d.runSpecDispatchUnder(run.bound)
	}()
	return true
}

// specDispatchParked reports whether an upgrade restart keeps the ticker from
// starting a tick: the landing drain is on, or the restart waits on the tick in
// flight, where a new one would start behind it and extend the wait (gt-u1fzb).
func (d *Daemon) specDispatchParked() bool {
	return d.upgradeRestartDrains() || d.upgradeTickWaiting.Load()
}

// specSeatsReady reads the seat settings the next tick will run with and
// reports whether the ticker may tick (spec_seats.go). A file that does not
// parse parks the ticker with one line per distinct error, and no tick runs
// until a read works: nothing is dispatched on a half-read picture.
func (d *Daemon) specSeatsReady() bool {
	if d.config == nil || d.config.TownRoot == "" {
		return true
	}
	picture, err := d.specSeats.read(d.config.TownRoot)
	if err != nil {
		reason := "seat settings unreadable: " + err.Error()
		if d.specSeatHold.Changed(reason) {
			d.logger.Printf("spec_dispatch: not ticking: %s", reason)
		}
		return false
	}
	if d.specSeatHold.Changed("") {
		d.logger.Printf("spec_dispatch: seat settings readable again; ticker resuming")
	}
	d.specSeats.announce(d.logger, picture)
	return true
}

// specDispatchTownHold is the hold that parks the whole ticker: the operator
// hold file or a town ESTOP. A rig ESTOP parks one rig's slings only, which
// the tick itself applies to that rig's candidates.
func (d *Daemon) specDispatchTownHold() string {
	if d.config == nil {
		return ""
	}
	return dispatch.OperatorHold(d.config.TownRoot)
}

// runSpecDispatch runs one tick under the bound the config gives now.
func (d *Daemon) runSpecDispatch() {
	if !d.isPatrolActive("spec_dispatch") || d.config == nil {
		return
	}
	d.runSpecDispatchUnder(d.specDispatchTickBound())
}

// runSpecDispatchUnder runs one tick under bound and logs its outcome, one line
// per decision. The bound is the one the tick was triggered with, so the
// failure line names the bound the tick ran under, which a second read could
// change if the config was edited meanwhile (gt-zeqqg).
func (d *Daemon) runSpecDispatchUnder(bound time.Duration) {
	if !d.isPatrolActive("spec_dispatch") || d.config == nil {
		return
	}
	tickArgs, err := d.specTickArgs()
	if err != nil {
		d.logger.Printf("spec_dispatch: tick not run: %v; a tick that does not know the running slings would sling their beads again", err)
		d.dispatchIdle.Reset()
		return
	}
	// The census file holds this tick's rows and nothing else: a tick killed
	// before it writes leaves the last one's, which were already logged, so the
	// file is cleared first (gt-139dp).
	census := d.clearSpecDispatchCensus(d.specDispatchCensusPath())
	start := d.clk().Now()
	out, err := d.runSpecDispatchCommand(bound, tickArgs...)
	d.logSpecDispatchCensus(census, err == nil)
	if err != nil {
		elapsed := d.clk().Now().Sub(start).Round(time.Second)
		d.logger.Printf("spec_dispatch: tick failed after %s of its %s bound: %v", elapsed, bound, err)
		// A tick that stopped before its report printed one hook line per sling
		// it made, so those lines are the only record of them. The bead is not
		// named: the sling's line does not carry it.
		for range specSlingsHooked(out) {
			d.logger.Printf("spec_dispatch: dispatched: a sling hooked before the tick failed; its bead is not named in the partial output (after %s of its %s bound)", elapsed, bound)
		}
		// A tick that failed observed nothing, so the run of idle ticks ends
		// here: the next one counts from one (gt-icmdq).
		d.dispatchIdle.Reset()
		return
	}
	// The 'tick took' line is logged for a tick that decided something; a held
	// tick runs no phase, so it has no timing to name.
	took := ""
	if r, perr := parseSpecDispatchReport(out); perr == nil {
		now := d.clk().Now()
		// A held tick decided nothing and carries no roster, so it observes no
		// idle run either: the streak ends here, as it does when no tick runs
		// at all (gt-icmdq).
		if r.Hold != "" {
			d.dispatchIdle.Reset()
		} else {
			r.seq = d.dispatchTickSeq.Add(1)
			d.dispatchIdle.Observe(r, now)
			d.recordDispatchTick(r, now)
			took = specDispatchTookLine(r, now.Sub(start))
			// The tick has returned; its slings run on from here, and the next
			// tick does not wait for them (gt-5h70s). Each sling's outcome
			// returns to this tick's counts (gt-pk2q8).
			d.startSpecSlings(r.Pending, r.seq)
		}
	} else {
		// A report nothing can read says nothing about the town: the run of
		// idle ticks ends rather than carrying over the tick (gt-icmdq).
		d.dispatchIdle.Reset()
	}
	for _, line := range d.formatSpecDispatchReport(out) {
		d.logger.Printf("spec_dispatch: %s", line)
	}
	if took != "" {
		d.logger.Printf("spec_dispatch: %s", took)
	}
}

// specDispatchTookLine is the one line a tick that decided something logs
// about its own length: the whole tick as the daemon timed it, then the
// phases the tick reported, so a slow tick is attributed to a phase (gt-bqch0).
// The calls each phase made trail the times, after the fields that were there
// first so a monitor parsing them reads the same text (gt-139dp).
func specDispatchTookLine(r specDispatchTickReport, took time.Duration) string {
	return fmt.Sprintf("tick took %s (roster %s, board %s, candidates %d %s, sling %s); roster %d calls, board %d calls, candidates %d calls",
		took.Round(time.Millisecond), r.RosterNs.Round(time.Millisecond), r.BoardNs.Round(time.Millisecond),
		r.Candidates, r.CandidatesNs.Round(time.Millisecond), r.SlingNs.Round(time.Millisecond),
		r.RosterCalls, r.BoardCalls, r.CandidatesCalls)
}

// recordedDispatchTick is one tick as townhealth reads it, tagged with the
// daemon's id for the tick so the outcome of a sling that tick planned in the
// background finds the counts it was included in (gt-pk2q8).
type recordedDispatchTick struct {
	townhealth.DispatchTick
	seq uint64
}

// recordDispatchTick appends one tick's decision to the history townhealth's
// dispatch field judges, keeping the newest dispatchTickHistory records
// (gt-xiw7o). The history is in memory: a restart drops it, and the field
// reads the fresh daemon as too young to have ticked until the ticker has
// run again.
func (d *Daemon) recordDispatchTick(r specDispatchTickReport, at time.Time) {
	seats, unreadable := dispatchRosterSeats(r.Roster)
	t := townhealth.DispatchTick{
		At:               at,
		Candidates:       r.Candidates,
		Seats:            seats,
		RosterUnreadable: unreadable,
		Dispatched:       len(r.Dispatched),
		Refused:          len(r.Refused),
		Planning:         len(r.Planning),
		Skipped:          len(r.Skipped),
		Failed:           len(r.Failed),
		LabeledFailed:    r.LabeledFailed,
	}
	d.dispatchTicksMu.Lock()
	defer d.dispatchTicksMu.Unlock()
	d.dispatchTicks = append(d.dispatchTicks, recordedDispatchTick{DispatchTick: t, seq: r.seq})
	if n := len(d.dispatchTicks); n > dispatchTickHistory {
		d.dispatchTicks = append([]recordedDispatchTick(nil), d.dispatchTicks[n-dispatchTickHistory:]...)
	}
}

// dispatchTickRecords copies the recorded ticks, oldest first.
func (d *Daemon) dispatchTickRecords() []townhealth.DispatchTick {
	d.dispatchTicksMu.Lock()
	defer d.dispatchTicksMu.Unlock()
	out := make([]townhealth.DispatchTick, len(d.dispatchTicks))
	for i, t := range d.dispatchTicks {
		out[i] = t.DispatchTick
	}
	return out
}

// dispatchRosterSeats reads the seats out of a tick's roster, which
// Budget.Picture renders as "agent live/cap, agent live/cap" and "no seats"
// for none. It reports unreadable for a roster it cannot read in full: an
// empty roster (the tick could not count one) or a seat it cannot parse
// could be hiding a free seat, so the tick must not be judged as a full town
// (gt-xiw7o).
func dispatchRosterSeats(roster string) ([]townhealth.DispatchSeat, bool) {
	roster = strings.TrimSpace(roster)
	if roster == "" {
		return nil, true
	}
	if roster == "no seats" {
		return nil, false
	}
	var out []townhealth.DispatchSeat
	parts := splitRosterSeats(roster)
	for n, part := range parts {
		part = strings.TrimSpace(part)
		// The town total ("total 2/10") is Picture's last segment, after a
		// seat, under a positive cap, with live covering every seat's. It is
		// not a seat (gt-penmy). Any other "total" segment is a seat named
		// total (gt-38gsb); a malformed last one is unreadable. A last seat
		// named total with live at or above the seats' sum looks the same by
		// text and is read as the total.
		if n > 0 && n == len(parts)-1 && strings.HasPrefix(part, "total ") {
			total, ok := parseRosterCount(strings.TrimPrefix(part, "total "))
			if !ok {
				return nil, true
			}
			if total.Cap > 0 && total.Live >= seatLiveSum(out) {
				continue
			}
		}
		// A seat may carry any number of parenthetical notes after its count:
		// the dead-hooked breakdown Picture renders (gt-tldj4) and the closed
		// window (gt-rredd). None of them is part of the count parsed here.
		if i := strings.IndexByte(part, '('); i >= 0 {
			part = strings.TrimSpace(part[:i])
		}
		i := strings.LastIndexByte(part, ' ')
		if i < 0 {
			return nil, true
		}
		seat, ok := parseRosterCount(part[i+1:])
		if !ok {
			return nil, true
		}
		out = append(out, seat)
	}
	return out, false
}

// parseRosterCount reads a roster "live/cap" count.
func parseRosterCount(s string) (townhealth.DispatchSeat, bool) {
	live, cap, ok := strings.Cut(s, "/")
	if !ok {
		return townhealth.DispatchSeat{}, false
	}
	l, lerr := strconv.Atoi(live)
	c, cerr := strconv.Atoi(cap)
	if lerr != nil || cerr != nil {
		return townhealth.DispatchSeat{}, false
	}
	return townhealth.DispatchSeat{Live: l, Cap: c}, true
}

// seatLiveSum is the live count across seats.
func seatLiveSum(seats []townhealth.DispatchSeat) int {
	sum := 0
	for _, s := range seats {
		sum += s.Live
	}
	return sum
}

// splitRosterSeats splits a roster on the commas that separate its seats. A
// comma inside a parenthetical note ("closed 20:00-08:00, 1 dead-hooked") does
// not separate seats (gt-rredd).
func splitRosterSeats(roster string) []string {
	var parts []string
	depth, start := 0, 0
	for i, r := range roster {
		switch r {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				parts = append(parts, roster[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, roster[start:])
}

// runSpecDispatchCommand runs `gt spec dispatch --json` with extra flags; the
// overseer report adds --dry-run to read the queue without slinging. The caller
// passes the bound (specDispatchTickBound) so it knows the one the tick ran under.
//
// On failure the stdout comes back with the error: a tick killed at its bound
// has printed its progress before the report it never reached, and that
// progress is the record of the slings it made (gt-bbfpr).
func (d *Daemon) runSpecDispatchCommand(bound time.Duration, extra ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(d.ctx, bound)
	defer cancel()
	args := append([]string{"spec", "dispatch", "--json"}, extra...)
	cmd := exec.CommandContext(ctx, d.gtPath, args...) //nolint:gosec // G204: gtPath resolved at daemon init
	cmd.Dir = d.config.TownRoot
	cmd.Env = append(daemonGTEnv(os.Environ()), dispatch.SpecDetailOwnerEnv+"="+specDetailOwner)
	stdout, stderr, err := d.runCmd(cmd)
	if err != nil {
		if msg := strings.TrimSpace(string(stderr)); msg != "" {
			return stdout, fmt.Errorf("%w: %s", err, lastLine(msg))
		}
		return stdout, err
	}
	return stdout, nil
}

// specDispatchCensusPath is the file a tick writes its bd census rows to,
// beside the cache the daemon's ticks keep across runs (gt-139dp).
func (d *Daemon) specDispatchCensusPath() string {
	return dispatch.SpecCensusPath(d.config.TownRoot)
}

// removeCensusFile removes the census file, replaced by a test whose clear
// fails (gt-139dp).
func (d *Daemon) removeCensusFile(path string) error {
	if d.specCensusRemove != nil {
		return d.specCensusRemove(path)
	}
	return os.Remove(path)
}

// specCensus is the tick's census file as the daemon left it: the path, and
// the error that kept the file from being cleared, which leaves the last
// tick's rows in it (gt-139dp).
type specCensus struct {
	path     string
	clearErr error
}

// clearSpecDispatchCensus empties the census file before a tick runs, so the
// rows logged for the tick are its own. A clear that fails costs observability
// and never the tick: the state is logged once, after the tick, marked
// unavailable rather than read as zero calls (gt-139dp).
func (d *Daemon) clearSpecDispatchCensus(path string) specCensus {
	err := d.removeCensusFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	return specCensus{path: path, clearErr: err}
}

// logSpecDispatchCensus logs the rows the tick's census file holds, busiest
// caller first. A file that could not be cleared is marked unavailable rather
// than read, since its rows may be the last tick's. A file that cannot be read
// is logged with its path and error: a clean tick was told where to write, so
// losing it is a census lost, not a tick that read nothing; a failed tick that
// left no file wrote no census, and says nothing (gt-139dp).
func (d *Daemon) logSpecDispatchCensus(c specCensus, tickOK bool) {
	if c.clearErr != nil {
		d.logger.Printf("spec_dispatch: bd census (spec dispatch): unavailable: the file at %s was not cleared, so its rows may be the last tick's: %v", c.path, c.clearErr)
		return
	}
	rows, err := beads.ReadCensusSink(c.path)
	if err != nil {
		if !tickOK && errors.Is(err, fs.ErrNotExist) {
			return
		}
		d.logger.Printf("spec_dispatch: bd census (spec dispatch): unreadable at %s: %v", c.path, err)
		return
	}
	if total := beads.CensusRowsTotal(rows); total > 0 {
		d.logger.Printf("spec_dispatch: bd census (spec dispatch): total=%d %s", total, beads.CensusTopRows(rows))
	}
}

// specSlingsHooked counts the slings a tick's output shows hooked before the
// report, which is what a tick killed at its bound still prints (gt-bbfpr). A
// killed tick prints one hooked line per sling, the line sling.Run prints once
// it has hooked the bead; dispatch.CountSlingHooked counts those and not the
// other "Work attached to" lines `gt sling` and `gt hook` print.
func specSlingsHooked(out []byte) int {
	return dispatch.CountSlingHooked(out)
}

// specDetailOwner names this daemon process to the ticks it starts. The tick
// keeps the pool's design and notes text in a file between ticks, and the file
// is this daemon's alone: a restarted daemon has another owner and finds the
// file empty (gt-l0vlf).
var specDetailOwner = fmt.Sprintf("%d.%d", os.Getpid(), time.Now().UnixNano())

// specDispatchRequiredKeys are the report keys the dispatcher always writes:
// the ones its report struct (internal/cmd/spec.go) tags without omitempty.
// Decoding leaves a missing key at its zero value, so a report without one
// would read as an empty roster or a tick with no decisions. It is an error
// instead.
var specDispatchRequiredKeys = []string{"roster", "candidates", "dispatched", "refused", "planning", "skipped", "failed"}

// parseSpecDispatchReport reads a tick's report out of its output. The sling
// path prints progress to stdout before the JSON, so the report is the last
// JSON object in the output. A report missing a required key is an error.
func parseSpecDispatchReport(out []byte) (specDispatchTickReport, error) {
	var r specDispatchTickReport
	text := strings.TrimSpace(string(out))
	if i := strings.LastIndex(text, "\n{"); i >= 0 {
		text = text[i+1:]
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(text), &keys); err != nil {
		return r, err
	}
	var missing []string
	for _, k := range specDispatchRequiredKeys {
		if _, ok := keys[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return r, fmt.Errorf("report lacks %s", strings.Join(missing, ", "))
	}
	err := json.Unmarshal([]byte(text), &r)
	return r, err
}

// formatSpecDispatchReport renders a tick's JSON as log lines. The tick line
// names every skipped bead with its reason, and a bead skipped unshaped warns
// once, on the first tick that sees it (gt-gzav5). A sling-stage deferral is
// also logged on its own 'deferred:' line, outside the tick line's cap, so a
// deferral past the cap still reads with its bead and reason (gt-4wcqd).
func (d *Daemon) formatSpecDispatchReport(out []byte) []string {
	r, err := parseSpecDispatchReport(out)
	if err != nil {
		return []string{fmt.Sprintf("unparseable tick output (%v)", err)}
	}
	if r.Hold != "" {
		return []string{"held: " + r.Hold}
	}
	lines := []string{specDispatchTickLine(r)}
	for _, group := range []struct {
		name    string
		entries []specDispatchTickEntry
	}{{"dispatched", r.Dispatched}, {"refused", r.Refused}, {"planning", r.Planning}, {"bypassed", r.Bypassed}, {"failed", r.Failed}, {"deferred", r.Deferred}} {
		for _, e := range group.entries {
			lines = append(lines, group.name+": "+e.Line)
		}
	}
	for _, n := range r.Notices {
		lines = append(lines, "note: "+n)
	}
	lines = append(lines, d.specUnshapedWarnings(r.Skipped)...)
	for _, e := range r.Errors {
		lines = append(lines, "error: "+e)
	}
	return lines
}

// specDispatchTickLine renders the tick's counts and, after them, the beads it
// skipped with their reasons, capped at specDispatchSkipCap and then "+N more"
// (gt-gzav5). The counts text is unchanged so a monitor parsing the numbers
// still reads the same fields; the overlap-hold bypass count is appended only
// when it is nonzero, for the same reason (gt-iydx1).
func specDispatchTickLine(r specDispatchTickReport) string {
	line := fmt.Sprintf("tick: %d candidate(s), roster %s, %d dispatched, %d refused, %d planning, %d skipped, %d failed, %d held by the failed label",
		r.Candidates, r.Roster, len(r.Dispatched), len(r.Refused), len(r.Planning), len(r.Skipped), len(r.Failed), r.LabeledFailed)
	if len(r.Bypassed) > 0 {
		line += fmt.Sprintf(", %d overlap hold bypassed", len(r.Bypassed))
	}
	if r.DetailsFetched > 0 || r.DetailsCached > 0 || r.DetailsUnread > 0 {
		line += fmt.Sprintf("; design/notes: %d fetched, %d cached", r.DetailsFetched, r.DetailsCached)
		if r.DetailsUnread > 0 {
			line += fmt.Sprintf(", %d unread", r.DetailsUnread)
		}
	}
	if len(r.Skipped) == 0 {
		return line
	}
	named := make([]string, 0, specDispatchSkipCap)
	for i, e := range r.Skipped {
		if i == specDispatchSkipCap {
			break
		}
		named = append(named, specSkipLabel(e))
	}
	line += "; skipped: " + strings.Join(named, "; ")
	if more := len(r.Skipped) - specDispatchSkipCap; more > 0 {
		line += fmt.Sprintf(" +%d more", more)
	}
	return line
}

// specSkipLabel names one skipped bead and its reason: "gt-a (no seat:
// claude-sonnet 2/2)".
func specSkipLabel(e specDispatchTickEntry) string {
	if e.Bead == "" {
		return e.Line
	}
	return fmt.Sprintf("%s (%s)", e.Bead, specSkipReason(e))
}

// specSkipReason is a skipped entry's reason, the tail of its line, which
// starts "<bead>: ".
func specSkipReason(e specDispatchTickEntry) string {
	if e.Bead == "" {
		return e.Line
	}
	return strings.TrimPrefix(e.Line, e.Bead+": ")
}

// specUnshapedWarnings warns once per bead the tick skipped unshaped, naming
// the missing headings, and remembers it. An unshaped bead stays ready, so
// every tick holds it again; without the memory the same warning would repeat
// each interval (gt-gzav5).
func (d *Daemon) specUnshapedWarnings(skipped []specDispatchTickEntry) []string {
	var out []string
	d.specUnshapedWarnedMu.Lock()
	defer d.specUnshapedWarnedMu.Unlock()
	if d.specUnshapedWarned == nil {
		d.specUnshapedWarned = map[string]bool{}
	}
	for _, e := range skipped {
		reason := specSkipReason(e)
		if e.Bead == "" || !strings.HasPrefix(reason, "unshaped") || d.specUnshapedWarned[e.Bead] {
			continue
		}
		d.specUnshapedWarned[e.Bead] = true
		out = append(out, fmt.Sprintf("warning: %s %s", e.Bead, reason))
	}
	return out
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}
