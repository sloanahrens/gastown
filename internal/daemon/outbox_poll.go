package daemon

// The outbox poll (docs/plans/2026-11-14-no-bead-access-plan.md, O4): a
// sandbox seat writes its outbox as it runs, and the seat-end copy-out reads it
// only when the seat ends. The poll gives the seat's escalation request a second
// route to the host while the seat still runs, so a seat blocked on a HELP does
// not wait for its own end to be heard (gt-0hkj0c).
//
// One tick reads each running seat's outbox through the in-sandbox exec the
// seat-end copy-out reads it through, and hands what it finds to the filing
// seam, which the next bead fills (gt-nvii5d, gt-dq4ce6).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/outbox"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/session"
)

const (
	// outboxPollPatrol is the patrol's name in daemon.patrols, in
	// daemon.container_off and in the daemon audit table.
	outboxPollPatrol = "outbox_poll"

	// defaultOutboxPollInterval is the period between two polls: the plan gives
	// the patrol about ten minutes, so a seat's HELP waits at most one interval
	// from the seat's write rather than for the seat's end.
	defaultOutboxPollInterval = 10 * time.Minute

	// outboxPollTickTimeout bounds one tick, so a read that hangs frees the
	// patrol's single-flight guard at a known point. The per-seat read keeps its
	// own bound inside it.
	outboxPollTickTimeout = 2 * time.Minute

	// outboxPollSeatTimeout bounds one seat's read, so a wedged gateway call
	// costs its own seat and not the tick. One read prints two small files, and
	// it is the bound the seat-end copy-out gives the same files (gt-c6tofd).
	outboxPollSeatTimeout = 30 * time.Second

	// outboxPollJoinWait bounds how long Run waits for a tick still running
	// after the ticker stopped. A tick left past it finishes its own bounded
	// read, and the next start needs nothing it read.
	outboxPollJoinWait = 10 * time.Second

	// outboxPollFileCap is the most one outbox file may be, in bytes: the cap
	// the seat-end copy-out holds the same file to (gt-c6tofd), so the poll
	// hands nothing that copy would refuse.
	outboxPollFileCap = 512 << 10

	// outboxPollReadMax is the most bytes one seat's read may print. The read
	// script asks the sandbox for at most outboxPollFileCap+1 bytes of each
	// file, so this bounds what the exec buffers even when a seat writes a
	// larger file; the markers and the end marker take a few dozen bytes more.
	outboxPollReadMax = 2*(outboxPollFileCap+1) + 512

	// outboxPollEndMarker closes one seat's read, so a read cut short is an
	// error and never the empty answer of a seat that wrote no outbox.
	outboxPollEndMarker = "@@ end @@"

	// The read's marker shape: each file is reported between these, followed
	// by its bytes when present. The read script and the tests both format a
	// marker from these, so a change to the shape is one change.
	outboxPollMarkerOpen  = "@@ "
	outboxPollMarkerClose = " @@"
	// outboxPollPresentFormat is one present marker: the file's name and its
	// byte count in decimal. The count frames the bytes that follow, so neither
	// file's content can pass for a marker.
	outboxPollPresentFormat = outboxPollMarkerOpen + "%s present %s" + outboxPollMarkerClose + "\n"
	// outboxPollAbsentFormat is one absent marker: the file the seat did not
	// write.
	outboxPollAbsentFormat = outboxPollMarkerOpen + "%s absent" + outboxPollMarkerClose + "\n"
)

// outboxPollSandboxOutboxDir is where a no-bead-access seat writes its outbox
// inside its sandbox: outbox.Dir under the sandbox's root /sandbox
// (gt-1m0kb), the same directory session.CopySandboxSeatOutbox reads after the
// seat ends. The poll runs while the seat still writes there.
const outboxPollSandboxOutboxDir = "/sandbox/" + outbox.Dir

// outboxPollTickFunc is one tick: the read of every running seat's outbox and
// the hand-off to the filing seam, or the error that ended it. The daemon calls
// it once for each tick it runs, and it must return when ctx is done, so the
// tick's bound frees the patrol's single-flight guard.
type outboxPollTickFunc func(ctx context.Context) error

// outboxPollExec runs one command inside a sandbox: the method set of
// *sandbox.CLI's Exec, the seam the seat-end copy-out reads an outbox through.
type outboxPollExec interface {
	Exec(ctx context.Context, name string, env map[string]string, argv []string, stdin []byte) (stdout, stderr []byte, err error)
}

// outboxPollRequest is one outbox a tick read from a running seat: the seat its
// sandbox record names and the two files the seat wrote, byte for byte. The
// bytes are the sandbox's claim and nothing here validates them: the filing
// seam holds them to outbox.Validate against Bead before it files anything
// (gt-dq4ce6).
type outboxPollRequest struct {
	// Seat is the sandbox name the read addressed: the record's session.
	Seat string
	Rig  string
	// Polecat is the seat's polecat name, the identity the request is filed
	// under.
	Polecat string
	// Bead is the bead the record names, which result.json's bead_id must match.
	Bead string
	// Result is result.json's bytes and Notes is notes.md's; Notes is nil when
	// the seat wrote no notes.
	Result []byte
	Notes  []byte
}

// outboxPollFileFunc files one request a tick read (gt-dq4ce6). An error is
// logged and the other seats' requests are still filed.
type outboxPollFileFunc func(ctx context.Context, req outboxPollRequest) error

// outboxPollFiling returns the production filing seam a tick hands its reads
// to, or nil until bead gt-dq4ce6 fills it. A nil seam refuses the whole tick:
// a read nothing files would be dropped, and the seat keeps no second copy of
// it. The daemon logs that refusal once, not once per tick.
func outboxPollFiling() outboxPollFileFunc { return nil }

// errOutboxPollNoFilingSeam is the tick's refusal when no filing seam is
// wired. triggerOutboxPoll logs it once, so an enabled patrol that has no seam
// yet says so without a line every interval.
var errOutboxPollNoFilingSeam = errors.New("no filing seam is wired, so no seat is read")

// outboxPoll is one tick's reader: the town whose sandbox records name the
// running seats, the exec that reads each outbox, and the filing seam the
// reads are handed to.
type outboxPoll struct {
	townRoot string
	exec     outboxPollExec
	file     outboxPollFileFunc
	logger   *log.Logger
	timeout  time.Duration
}

// newOutboxPoll returns the poll a tick runs. A zero timeout is
// outboxPollSeatTimeout.
func newOutboxPoll(townRoot string, exec outboxPollExec, file outboxPollFileFunc, logger *log.Logger, timeout time.Duration) *outboxPoll {
	if timeout <= 0 {
		timeout = outboxPollSeatTimeout
	}
	return &outboxPoll{townRoot: townRoot, exec: exec, file: file, logger: logger, timeout: timeout}
}

// liveOutboxPoll returns the poll a tick runs in production: every running
// seat's outbox read through the gateway's openshell CLI, which is the exec the
// seat-end copy-out reads through.
func liveOutboxPoll(townRoot string, logger *log.Logger) *outboxPoll {
	return newOutboxPoll(townRoot, &sandbox.CLI{}, outboxPollFiling(), logger, outboxPollSeatTimeout)
}

// tick reads every running seat's outbox once and hands each one it finds to
// the filing seam. A failure of the whole read — no town root, no exec seam, no
// filing seam, a registry that cannot be listed — is the tick's error and no
// seat is read. One seat's failure is logged and skipped: one wedged sandbox
// says nothing about the rest.
func (p *outboxPoll) tick(ctx context.Context) error {
	if p.townRoot == "" {
		return errors.New("no town root, so no seat is read")
	}
	if p.exec == nil {
		return errors.New("no exec seam, so no seat is read")
	}
	if p.file == nil {
		return errOutboxPollNoFilingSeam
	}
	logf := func(format string, args ...any) {
		if p.logger != nil {
			p.logger.Printf(format, args...)
		}
	}
	seats, err := outboxPollSeats(p.townRoot)
	if err != nil {
		return err
	}
	for _, seat := range seats {
		if err := ctx.Err(); err != nil {
			return err
		}
		if seat.err != nil {
			logf("outbox_poll: seat %s: its outbox is not read: %v", seat.name, seat.err)
			continue
		}
		p.readAndFile(ctx, seat.rec, logf)
	}
	return nil
}

// readAndFile reads one seat's outbox and hands it to the filing seam. A read
// that fails or does not parse, and a filing the seam refuses, are logged with
// the seat named and change nothing else: the next tick reads the seat again
// while its record lives.
func (p *outboxPoll) readAndFile(ctx context.Context, rec session.SandboxRecord, logf func(string, ...any)) {
	text, err := p.readOutbox(ctx, rec.Session)
	if err != nil {
		logf("outbox_poll: seat %s: its outbox is not read: %v; the next tick reads it again", rec.Session, err)
		return
	}
	read, err := parseOutboxPollRead(text)
	if err != nil {
		logf("outbox_poll: seat %s: its outbox is not read: %v; the next tick reads it again", rec.Session, err)
		return
	}
	if !read.resultOK {
		// A seat that wrote notes and no result has no request yet; its notes
		// stay in the sandbox for the seat-end copy-out, as they do for a seat
		// that ends now (gt-s4gfph).
		if read.notesOK {
			logf("outbox_poll: seat %s: its outbox holds %s but no %s, so nothing is filed", rec.Session, outbox.NotesFile, outbox.ResultFile)
		}
		return
	}
	req := outboxPollRequest{
		Seat:    rec.Session,
		Rig:     rec.Rig,
		Polecat: rec.Polecat,
		Bead:    rec.WorkBead,
		Result:  read.result,
		Notes:   read.notes,
	}
	if err := p.file(ctx, req); err != nil {
		logf("outbox_poll: seat %s: its outbox is not filed: %v", rec.Session, err)
	}
}

// readOutbox runs one seat's outbox read under the poll's per-seat bound. The
// name has been validated by outboxPollSeats, so it reaches the seam as an
// argument of the exec and nowhere else.
func (p *outboxPoll) readOutbox(ctx context.Context, seat string) ([]byte, error) {
	seatCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	stdout, _, err := p.exec.Exec(seatCtx, seat, nil, outboxPollReadArgv(), nil)
	if err != nil {
		return nil, err
	}
	return stdout, nil
}

// outboxPollSeat is one sandbox record to read: the seat whose record it is,
// and the error that keeps it from being read.
type outboxPollSeat struct {
	name string
	rec  session.SandboxRecord
	err  error
}

// outboxPollSeats lists the town's sandbox records in file name order, which
// are its running seats: a record is removed when its seat ends. A missing
// registry directory is a town that never ran a sandbox — no seats and no
// error. A registry that cannot be listed is an error, so a tick that cannot
// look never reports the same empty answer as a town with no seat.
func outboxPollSeats(townRoot string) ([]outboxPollSeat, error) {
	dir := session.SandboxRegistryDir(townRoot)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing the sandbox records in %s: %w", dir, err)
	}
	var seats []outboxPollSeat
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".json")
		rec, found, err := session.ReadSandboxRecord(townRoot, name)
		if err != nil {
			seats = append(seats, outboxPollSeat{name: name, err: err})
			continue
		}
		if !found {
			// Another process removed the record between the listing and the
			// read: the seat has ended, so there is no outbox to read.
			continue
		}
		if err := outboxPollSeatProblem(name, rec); err != nil {
			seats = append(seats, outboxPollSeat{name: name, err: err})
			continue
		}
		seats = append(seats, outboxPollSeat{name: rec.Session, rec: rec})
	}
	return seats, nil
}

// outboxPollSeatProblem reports why rec, read from the record file name, is not
// a seat a tick may read or file for. The session name becomes the exec's
// argument and the rig and polecat become the identity the request is filed
// under, so each is refused before it reaches either.
func outboxPollSeatProblem(name string, rec session.SandboxRecord) error {
	if rec.Session == "" {
		return errors.New("the record names no seat")
	}
	if !session.ValidSandboxSessionName(rec.Session) {
		return fmt.Errorf("seat name %q is not a sandbox name", rec.Session)
	}
	if rec.Session != name {
		return fmt.Errorf("the record names seat %q, not the %q of its file", rec.Session, name)
	}
	if err := polecat.CheckSandboxSeatNames(rec.Rig, rec.Polecat); err != nil {
		return err
	}
	if rec.WorkBead == "" {
		return errors.New("the record names no bead, so there is nothing to file an escalation against")
	}
	return nil
}

// outboxPollReadArgv is the argv one seat's outbox read runs: a shell that
// prints a marker line for each of the seat's two outbox files — naming the
// file, and for a file the seat wrote, its true byte count and then at most
// outboxPollFileCap+1 of its bytes — and closes with the end marker. The count
// frames the bytes, so a notes.md that holds marker text cannot pass for the
// end of the read; the cap bounds what the exec buffers even when the seat
// wrote a larger file, and a file over the cap is refused by its count. A seat
// that wrote no outbox prints only the end marker.
func outboxPollReadArgv() []string {
	dir := outboxPollSandboxOutboxDir
	capPlusOne := strconv.Itoa(outboxPollFileCap + 1)
	// The format constants end in a newline character; a newline inside a
	// single-quoted string is part of the format printf prints.
	script := "D='" + dir + "'\n" +
		"if test -d \"$D\"; then\n" +
		"for f in " + outbox.ResultFile + " " + outbox.NotesFile + "; do\n" +
		"if test -f \"$D/$f\"; then\n" +
		"printf '" + outboxPollPresentFormat + "' \"$f\" \"$(wc -c < \"$D/$f\")\"\n" +
		"head -c " + capPlusOne + " \"$D/$f\"\n" +
		"else\n" +
		"printf '" + outboxPollAbsentFormat + "' \"$f\"\n" +
		"fi\n" +
		"done\n" +
		"fi\n" +
		"printf '" + outboxPollEndMarker + "\\n'\n"
	return []string{"sh", "-c", script}
}

// outboxPollRead is what one seat's read found: each outbox file's bytes and
// whether the seat wrote it.
type outboxPollRead struct {
	result   []byte
	resultOK bool
	notes    []byte
	notesOK  bool
}

// parseOutboxPollRead reads the stdout of outboxPollReadArgv. The sandbox
// chooses those bytes, so a read larger than outboxPollReadMax, a line that is
// not a marker, a file the outbox does not hold, a file reported twice, a
// marker that is neither present nor absent, a marker that names no byte count,
// a byte count that is not a whole number under the cap, content shorter than
// its count, a read without its end marker, and bytes after the end marker are
// each an error: never a shorter read reported as the seat's outbox.
func parseOutboxPollRead(out []byte) (outboxPollRead, error) {
	if len(out) > outboxPollReadMax {
		return outboxPollRead{}, fmt.Errorf("the read is %d bytes, more than the %d-byte bound two capped outbox files take", len(out), outboxPollReadMax)
	}
	var read outboxPollRead
	seen := make(map[string]bool, 2)
	for i := 0; ; {
		end := bytes.IndexByte(out[i:], '\n')
		if end < 0 {
			return outboxPollRead{}, errors.New("the read ends inside a marker line")
		}
		line := string(out[i : i+end])
		i += end + 1
		if line == outboxPollEndMarker {
			if i != len(out) {
				return outboxPollRead{}, fmt.Errorf("the read holds %d bytes after its end marker", len(out)-i)
			}
			break
		}
		name, size, present, err := parseOutboxPollMarker(line)
		if err != nil {
			return outboxPollRead{}, err
		}
		if seen[name] {
			return outboxPollRead{}, fmt.Errorf("the read reports %s twice", name)
		}
		seen[name] = true
		if !present {
			continue
		}
		if size > outboxPollFileCap {
			return outboxPollRead{}, fmt.Errorf("%s is %d bytes, more than the %d-byte cap the poll reads", name, size, outboxPollFileCap)
		}
		if size > int64(len(out)-i) {
			return outboxPollRead{}, fmt.Errorf("%s is %d bytes, and the read holds %d", name, size, len(out)-i)
		}
		content := out[i : i+int(size)]
		i += int(size)
		if name == outbox.ResultFile {
			read.result, read.resultOK = content, true
		} else {
			read.notes, read.notesOK = content, true
		}
	}
	// The script prints a marker for each of the two files once it has found
	// the outbox directory, and neither for a seat that wrote no outbox.
	switch len(seen) {
	case 0:
		return read, nil
	case 2:
		return read, nil
	default:
		return outboxPollRead{}, errors.New("the read names one outbox file and not the other")
	}
}

// parseOutboxPollMarker reads one marker line: the outbox file it names,
// whether the seat wrote it, and the byte count that frames its content.
func parseOutboxPollMarker(line string) (name string, size int64, present bool, err error) {
	rest, ok := strings.CutPrefix(line, outboxPollMarkerOpen)
	if !ok {
		return "", 0, false, errors.New("the read holds a line that is not a marker")
	}
	rest, ok = strings.CutSuffix(rest, outboxPollMarkerClose)
	if !ok {
		return "", 0, false, fmt.Errorf("the read holds a marker that does not end in %q", outboxPollMarkerClose)
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 || (fields[0] != outbox.ResultFile && fields[0] != outbox.NotesFile) {
		return "", 0, false, errors.New("the read holds a marker that names no outbox file")
	}
	name = fields[0]
	if len(fields) < 2 {
		return "", 0, false, fmt.Errorf("the read's marker for %s is not in the outbox's shape", name)
	}
	switch fields[1] {
	case "absent":
		if len(fields) != 2 {
			return "", 0, false, fmt.Errorf("the read's marker for %s is not in the outbox's shape", name)
		}
		return name, 0, false, nil
	case "present":
		if len(fields) != 3 {
			return "", 0, false, fmt.Errorf("the read's marker for %s names no byte count", name)
		}
		n, parseErr := strconv.ParseInt(fields[2], 10, 64)
		if parseErr != nil || n < 0 {
			return "", 0, false, fmt.Errorf("the read's marker for %s names no byte count", name)
		}
		return name, n, true, nil
	default:
		return "", 0, false, fmt.Errorf("the read's marker for %s is neither present nor absent", name)
	}
}

// startOutboxPollTicker starts the outbox poll's own ticker when the patrol is
// active, and returns a nil channel and stop func when it is not: a town that
// names no key starts nothing (gt-0hkj0c). The ticker's interval is
// defaultOutboxPollInterval.
func (d *Daemon) startOutboxPollTicker() (<-chan time.Time, func()) {
	if !d.isPatrolActive(outboxPollPatrol) {
		return nil, nil
	}
	newTicker := d.newOutboxPollTicker
	if newTicker == nil {
		newTicker = func(interval time.Duration) (<-chan time.Time, func()) {
			ticker := time.NewTicker(interval)
			return ticker.C, ticker.Stop
		}
	}
	ticks, stop := newTicker(defaultOutboxPollInterval)
	d.logger.Printf("Outbox poll ticker started (interval %v)", defaultOutboxPollInterval)
	return ticks, stop
}

// startOutboxPollTick runs one poll on its own goroutine, so a read that waits
// on the gateway holds that goroutine rather than the main select loop, which
// carries the heartbeat and every other patrol. A tick that arrives while the
// previous one still runs is skipped and logged, since two polls at once would
// read the same seat rows twice.
func (d *Daemon) startOutboxPollTick() {
	if !d.outboxPollRunning.CompareAndSwap(false, true) {
		d.logger.Printf("outbox_poll: previous tick still running, skipping")
		return
	}
	d.outboxPollCycles.Add(1)
	go func() {
		defer d.outboxPollCycles.Done()
		defer d.outboxPollRunning.Store(false)
		d.triggerOutboxPoll()
	}()
}

// triggerOutboxPoll runs one tick under outboxPollTickTimeout: the daemon's
// seam when it has one, and the live poll over the town's running seats
// otherwise. A town that turned the patrol off since the ticker started reads
// nothing, and a tick that fails is logged rather than read as a quiet tick.
//
// A tick with no filing seam is the one failure logged once: until gt-dq4ce6
// wires the seam, the live tick refuses every interval, and a line every
// interval would say the same thing. Every other failure is logged as it
// happens.
func (d *Daemon) triggerOutboxPoll() {
	if !d.isPatrolActive(outboxPollPatrol) {
		return
	}
	parent := d.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, outboxPollTickTimeout)
	defer cancel()
	tick := d.outboxPollTick
	if tick == nil {
		var townRoot string
		if d.config != nil {
			townRoot = d.config.TownRoot
		}
		tick = liveOutboxPoll(townRoot, d.logger).tick
	}
	err := tick(ctx)
	if err == nil {
		return
	}
	if errors.Is(err, errOutboxPollNoFilingSeam) {
		d.outboxPollNoSeamOnce.Do(func() {
			d.logger.Printf("outbox_poll: %v; the patrol says this once", err)
		})
		return
	}
	d.logger.Printf("outbox_poll: %v", err)
}

// stopOutboxPollPatrol stops the patrol's ticker and joins the tick it may have
// running, bounded, so Run's return leaves no tick reading the town after the
// daemon is gone. A tick still running at the bound is logged and left: its
// read is bounded by outboxPollTickTimeout, and a shutdown does not wait for
// one.
func (d *Daemon) stopOutboxPollPatrol(stop func()) {
	if stop != nil {
		stop()
	}
	done := make(chan struct{})
	go func() {
		d.outboxPollCycles.Wait()
		close(done)
	}()
	bound := d.outboxPollJoinWait
	if bound <= 0 {
		bound = outboxPollJoinWait
	}
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		d.logger.Printf("outbox_poll: a tick is still running %s after the ticker stopped; Run leaves without it", bound)
	}
}
