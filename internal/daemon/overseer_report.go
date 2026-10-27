package daemon

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	townreport "github.com/steveyegge/gastown/internal/report"
	"github.com/steveyegge/gastown/internal/townhealth"
)

// The overseer_report patrol (gt-a6fg7): every interval the daemon writes the
// status the overseer used to write by hand — done, in progress, left, open
// questions and the health line, plus gt report --hour's sections on the
// hour — to <town>/.runtime/overseer/reports/, where the dashboard's Report
// pane reads the newest timestamp-named file. The text is the digest of
// internal/report; this file is the cadence, the slot rule and the sources.
// Off unless patrols.overseer_report.enabled is true. It reads and writes
// that one directory and restarts or kills nothing.

const (
	defaultOverseerReportInterval = 15 * time.Minute

	// overseerReportBeadTimeout bounds one bd call the report makes.
	overseerReportBeadTimeout = 30 * time.Second

	// overseerReportLookback bounds how far back DONE reaches when the
	// previous report is older than this or absent, so a daemon that was down
	// for days does not ask bd for every bead closed since.
	overseerReportLookback = 24 * time.Hour

	// overseerReportMaxBytes keeps a report under the dashboard reader's cap
	// (internal/dashboard/reports.go), which refuses a larger file.
	overseerReportMaxBytes = 60 << 10

	// overseerReportLayout is a report file's name, which the dashboard reader
	// parses for the report's time: the UTC instant of its slot, then .md.
	overseerReportLayout = "20060102T150405Z"
	overseerReportExt    = ".md"

	// overseerLatestName is the fixed name of the copy of the newest report, so
	// a reader can take the newest one without sorting the directory by time
	// (gt-y0xcl). It is not a report of its own: overseerReportNameRe rejects
	// it, so the lister and the previous-report lookup never read it as a
	// digest.
	overseerLatestName = "latest.md"
)

// overseerReportNameRe is the whole name of a report file.
var overseerReportNameRe = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z\.md$`)

// overseerReportInterval returns the configured interval, or 15m.
func overseerReportInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.OverseerReport != nil {
		if s := config.Patrols.OverseerReport.IntervalStr; s != "" {
			if d, err := time.ParseDuration(s); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultOverseerReportInterval
}

// overseerReportTickEvery is how often the patrol ticker fires for a report
// interval: four times a slot, so every slot sees a tick whatever the
// ticker's phase.
func overseerReportTickEvery(interval time.Duration) time.Duration {
	if tick := interval / 4; tick >= time.Second {
		return tick
	}
	return interval
}

// overseerReportSeams are the report's reads, its clock, and where it says what
// it could not do. A test sets all of them; production fills them in
// (overseerReportProdSeams).
type overseerReportSeams struct {
	now    func() time.Time
	digest func(slot, since time.Time) townreport.Digest
	// hour is the run behind the hour sections, ending at slot.
	hour func(slot time.Time) townreport.HourRun
	// logf is where the report says what it will not fail on: a latest.md copy
	// it could not refresh. Production takes the daemon's logger; a seam may
	// leave it nil, and the report then drops the message.
	logf func(format string, args ...any)
}

// triggerOverseerReport starts one report on its own goroutine unless one is
// already running.
func (d *Daemon) triggerOverseerReport() {
	if !d.overseerReportRunning.CompareAndSwap(false, true) {
		d.logger.Printf("overseer_report: previous run still going, skipping")
		return
	}
	d.overseerReportCycles.Add(1)
	go func() {
		defer d.overseerReportCycles.Done()
		defer d.overseerReportRunning.Store(false)
		d.runOverseerReport()
	}()
}

// runOverseerReport writes the current slot's report and logs the outcome.
func (d *Daemon) runOverseerReport() {
	if !d.isPatrolActive("overseer_report") || d.config == nil {
		return
	}
	seams := d.overseerReportSeams
	if seams == nil {
		seams = d.overseerReportProdSeams()
	}
	path, err := writeOverseerReport(d.config.TownRoot, overseerReportInterval(d.patrolConfig), seams)
	switch {
	case err != nil:
		d.logger.Printf("overseer_report: %v", err)
	case path != "":
		d.logger.Printf("overseer_report: wrote %s", filepath.Base(path))
	}
}

// writeOverseerReport writes the report for the slot the clock is in, and
// returns its path, or "" when the slot already has one. The slot is the
// interval-aligned UTC instant at or before now, and names the file; a report
// at or after the slot ends the run, so a second run in a slot, or a clock
// that stepped back, writes nothing. The report before the slot is "the
// previous report": DONE counts what closed since it, and the first report
// of a clock hour adds the hour sections.
func writeOverseerReport(townRoot string, interval time.Duration, s *overseerReportSeams) (string, error) {
	now := s.now()
	slot := now.UTC().Truncate(interval)
	dir := townreport.ReportsDir(townRoot)

	prev, newest, err := overseerReportPrevious(dir, slot)
	if err != nil {
		return "", err
	}
	if !newest.IsZero() && !newest.Before(slot) {
		if newest.After(now) {
			// Not a covered slot: a report named for an instant the clock has
			// not reached, which the run waits behind. Say so, or the patrol
			// looks alive while writing nothing.
			return "", fmt.Errorf("newest report %s is dated after now (%s), so no report is written until the clock reaches it",
				newest.UTC().Format(overseerReportLayout), now.UTC().Format(overseerReportLayout))
		}
		return "", nil
	}
	since := prev
	if since.IsZero() {
		since = slot.Add(-interval)
	}
	if floor := slot.Add(-overseerReportLookback); since.Before(floor) {
		since = floor
	}

	text := s.digest(slot, since).Render()
	if prev.IsZero() || !prev.Truncate(time.Hour).Equal(slot.Truncate(time.Hour)) {
		var hour bytes.Buffer
		r := s.hour(slot)
		r.Out = &hour
		r.Render(r.Collect())
		text += "\nHOUR\n" + hour.String()
	}
	text = overseerReportClamp(text)

	return writeOverseerReportFile(dir, slot, text, s.logf)
}

// overseerReportPrevious reads the report files in dir: prev is the newest
// one before slot, the previous report, and newest the newest of all. A
// missing directory holds no reports.
func overseerReportPrevious(dir string, slot time.Time) (prev, newest time.Time, err error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return time.Time{}, time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("reading %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() || !overseerReportNameRe.MatchString(e.Name()) {
			continue
		}
		at, perr := time.Parse(overseerReportLayout, strings.TrimSuffix(e.Name(), overseerReportExt))
		if perr != nil {
			continue
		}
		if at.After(newest) {
			newest = at
		}
		if at.Before(slot) && at.After(prev) {
			prev = at
		}
	}
	return prev, newest, nil
}

// overseerReportClamp keeps text under overseerReportMaxBytes, cutting at a
// line break and saying so.
func overseerReportClamp(text string) string {
	if len(text) <= overseerReportMaxBytes {
		return text
	}
	cut := strings.LastIndexByte(text[:overseerReportMaxBytes-64], '\n')
	if cut < 0 {
		cut = overseerReportMaxBytes - 64
	}
	return text[:cut+1] + "(report cut at the size the dashboard reads)\n"
}

// writeOverseerReportFile writes text as the slot's report and refreshes
// latest.md from the same text. Both names are published by a link or a rename
// and never by a write in place, so a reader gets a whole report or none of it:
// the report is linked into place, which fails if the name is taken, so a
// report the patrol did not write is never replaced, where the copy is replaced
// by every run that writes a report. A latest.md refresh that fails is logged
// and leaves the report write alone: the copy is a convenience, the slot's
// report is the record. logf may be nil, and the message is then dropped.
func writeOverseerReportFile(dir string, slot time.Time, text string, logf func(string, ...any)) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating %s: %w", dir, err)
	}
	name := slot.UTC().Format(overseerReportLayout) + overseerReportExt
	final := filepath.Join(dir, name)
	tmp, err := writeOverseerTemp(dir, name, text)
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := os.Link(tmp, final); err != nil {
		if os.IsExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("writing %s: %w", name, err)
	}
	if err := refreshOverseerLatest(dir, text); err != nil {
		// logf is a seam, and a seam may leave it unset: the report is already
		// in place, so a missing logger costs the message about the copy and
		// never a panic in the patrol's goroutine.
		if logf != nil {
			logf("overseer_report: %v", err)
		}
	}
	return final, nil
}

// refreshOverseerLatest puts text at dir/latest.md by renaming a temp file onto
// it, so the name holds a whole report: a reader opens the old copy or the new
// one, never a half-written file. The copy is its own file and not a second
// link to the report, since it is the one a human may annotate by hand and the
// report is the record.
func refreshOverseerLatest(dir, text string) error {
	tmp, err := writeOverseerTemp(dir, overseerLatestName, text)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := os.Rename(tmp, filepath.Join(dir, overseerLatestName)); err != nil {
		return fmt.Errorf("replacing %s: %w", overseerLatestName, err)
	}
	return nil
}

// writeOverseerTemp writes text to a fresh file in dir, named after hint, and
// returns its path: a whole file the caller puts in place under the name a
// reader uses. The caller owns it and removes it.
func writeOverseerTemp(dir, hint, text string) (string, error) {
	tmp, err := os.CreateTemp(dir, ".tmp-"+hint+"-*")
	if err != nil {
		return "", fmt.Errorf("creating a file in %s: %w", dir, err)
	}
	fail := func(err error) (string, error) {
		_ = os.Remove(tmp.Name())
		return "", fmt.Errorf("writing %s: %w", hint, err)
	}
	if _, err := tmp.WriteString(text); err != nil {
		_ = tmp.Close()
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	// CreateTemp makes the file 0600, and whoever runs the dashboard reads the
	// reports.
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fail(err)
	}
	return tmp.Name(), nil
}

// overseerReportProdSeams reads the town: bd pinned read-only for every
// store, the working seats the overrun guard reads, the spec dispatcher's dry
// run and the town health file.
func (d *Daemon) overseerReportProdSeams() *overseerReportSeams {
	townRoot := d.config.TownRoot
	rigs := func() ([]string, error) { return d.getKnownRigs(), nil } //nolint:unparam // the seam's signature carries the error gt report's registry read can return
	store := func(name string) (townreport.BeadStore, error) {
		dir, err := townreport.StoreDir(townRoot, name)
		if err != nil {
			return nil, err
		}
		b := beads.NewPlain(townRoot, bdReadOnlyPinnedEnv(beads.ResolveBeadsDir(dir)),
			beads.WithBin(d.bdPathOrDefault())).WithTimeout(overseerReportBeadTimeout)
		return townreport.WrapStore(b), nil
	}
	return &overseerReportSeams{
		now:  time.Now,
		logf: d.logger.Printf,
		digest: func(slot, since time.Time) townreport.Digest {
			return townreport.Digest{
				Now:    d.clk().Now(),
				Slot:   slot,
				Since:  since,
				Rigs:   rigs,
				Store:  store,
				Seats:  d.overseerReportSeats,
				Left:   d.overseerReportLeft,
				Health: d.overseerReportHealth,
			}
		},
		hour: func(slot time.Time) townreport.HourRun {
			// The daemon cannot name the installed gt's version (that is a
			// cmd ldflag), so the mains row names the running build.
			r := townreport.NewHourRun(townRoot, slot, "daemon", d.buildCommit(), rigs)
			r.Store = store
			return r
		},
	}
}

// overseerReportSeats lists the working seats and dates each from its bead's
// dispatch clock, the one the overrun guard judges.
func (d *Daemon) overseerReportSeats() ([]townreport.SeatWork, error) {
	seats, err := d.overrunWorkingSeats()
	if err != nil {
		return nil, err
	}
	return overseerSeatWork(seats, (&overrunSlungReader{d: d}).slungAt), nil
}

// overseerSeatWork dates each seat that holds a bead from that bead's dispatch
// clock (events.DispatchStart, else the seat's agent bead): the earliest sling
// of the bead this run, not the sling that reached this seat, so a row whose
// bead changed hands reads the time the work has been in progress.
func overseerSeatWork(seats []overrunSeat, slungAt func(bead, rig, polecat string) (time.Time, bool)) []townreport.SeatWork {
	out := make([]townreport.SeatWork, 0, len(seats))
	for _, s := range seats {
		if s.Bead == "" {
			continue
		}
		since, _ := slungAt(s.Bead, s.Rig, s.Polecat)
		out = append(out, townreport.SeatWork{Rig: s.Rig, Polecat: s.Polecat, Bead: s.Bead, Since: since})
	}
	return out
}

// overseerReportLeft asks the spec dispatcher what it would do now, without
// slinging, labeling or commenting. It runs the dry run with the flags the
// next real tick runs with, so a bead with a sling in flight is neither next in
// the queue nor counted as a free seat (gt-nosod).
func (d *Daemon) overseerReportLeft() (townreport.DispatchLeft, error) {
	args, err := d.specTickArgs()
	if err != nil {
		return townreport.DispatchLeft{}, err
	}
	out, err := d.runSpecDispatchCommand(d.specDispatchTickBound(), append(args, "--dry-run")...)
	if err != nil {
		return townreport.DispatchLeft{}, err
	}
	r, err := parseSpecDispatchReport(out)
	if err != nil {
		return townreport.DispatchLeft{}, fmt.Errorf("unparseable dispatch output: %w", err)
	}
	return overseerReportLeftFrom(r)
}

// overseerReportLeftFrom sorts a dry run's decisions. A bead the dry run would
// sling, or finds clean but without a seat or past the per-tick limit, is
// next in the queue, in the order the dispatcher named them; every other bead
// it declined is held, with its reason. The beads the dispatcher holds out
// with the spec-dispatch-failed label have no entry, only a count, which LEFT
// carries so a bead stuck behind the label is not read as an empty queue.
//
// A dry run that exits 0 can still carry Errors (an unreadable roster, a
// failed board read), and then its empty queue means "could not read", not "no
// ready work". Such a report with no decided bead is an error, so LEFT renders
// as unavailable the way a failed command does: an unreadable roster stops the
// run before it decides anything. Once any bead was decided, an error is one
// bead's failed read, and LEFT keeps the decided rows and carries the count.
func overseerReportLeftFrom(r specDispatchTickReport) (townreport.DispatchLeft, error) {
	decided := len(r.Dispatched)+len(r.Skipped)+len(r.Refused)+len(r.Planning)+len(r.Failed) > 0 || r.LabeledFailed > 0
	if len(r.Errors) > 0 && !decided {
		err := fmt.Errorf("dispatch dry run reported errors: %s", r.Errors[0])
		if more := len(r.Errors) - 1; more > 0 {
			err = fmt.Errorf("%w (+%d more)", err, more)
		}
		return townreport.DispatchLeft{}, err
	}
	left := townreport.DispatchLeft{Hold: r.Hold, LabeledFailed: r.LabeledFailed, Errors: len(r.Errors)}
	row := func(e specDispatchTickEntry, prefix string) townreport.DispatchRow {
		return townreport.DispatchRow{Bead: e.Bead, Rig: e.Rig, Text: prefix + specSkipReason(e)}
	}
	for _, e := range r.Dispatched {
		left.Next = append(left.Next, row(e, ""))
	}
	for _, e := range r.Skipped {
		reason := specSkipReason(e)
		// A pin to an agent the pool lacks waits on no seat that will free up,
		// so it is held, not next.
		waitsForSeat := strings.HasPrefix(reason, "no seat:") && !strings.HasPrefix(reason, "no seat: pinned to ")
		if strings.HasPrefix(reason, "clean, per-tick limit") || waitsForSeat {
			left.Next = append(left.Next, row(e, ""))
			continue
		}
		left.Held = append(left.Held, row(e, ""))
	}
	for _, g := range []struct {
		name    string
		entries []specDispatchTickEntry
	}{{"refused", r.Refused}, {"planning", r.Planning}, {"failed", r.Failed}} {
		for _, e := range g.entries {
			left.Held = append(left.Held, row(e, g.name+": "))
		}
	}
	return left, nil
}

// overseerReportHealth is the town health line from the file the heartbeat
// writes, judged stale by the same threshold the health computation uses.
func (d *Daemon) overseerReportHealth() (string, error) {
	r, err := townhealth.Read(d.config.TownRoot)
	if err != nil {
		return "", err
	}
	_, stale, err := d.loadOperationalConfig().GetHealthSettings().Resolve()
	if err != nil {
		stale = townhealth.DefaultStaleAfter
	}
	return townhealth.Line(r, d.clk().Now(), stale), nil
}
