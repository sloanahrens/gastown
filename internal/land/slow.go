package land

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"
)

// DefaultSlowAfter is how long a landing's gate or om stage runs before the
// slow-landing alarm fires. Normal landings take 2-5 minutes (gt-lcu5p).
const DefaultSlowAfter = 8 * time.Minute

// slowCaptureBudget bounds the evidence capture, so a wedged ps or lsof never
// outlives the alarm that asked for it.
const slowCaptureBudget = 5 * time.Second

// maxSlowPIDs bounds the pids an escalation names; the evidence file has the rest.
const maxSlowPIDs = 20

// SlowAlarm reports a landing stage that outlives After: one "[land] SLOW"
// line and one escalation per stage, with the stage's process tree and open
// TCP peers saved beside the landing's gate logs. It only reports; the stage
// deadlines kill (gt-b4w3y).
type SlowAlarm struct {
	// After is the stage run time that raises the alarm; 0 turns it off.
	After time.Duration
	// Escalate files the low-severity escalation. It runs in the alarm's own
	// goroutine, which recovers a panic, so a broken alert path never reaches
	// the landing.
	Escalate func(beadID, stage, message string)
	// EvidenceDir is the directory a landing's evidence file goes in; nil or
	// "" means no file.
	EvidenceDir func(ctx context.Context, dir string) string
	// Clock times the stage; nil means the real clock.
	Clock clockwork.Clock

	// run answers the evidence commands (ps, lsof); nil means execOutput.
	run diagRunner
}

// slowSnapshot is what the alarm saw of a stage's processes.
type slowSnapshot struct {
	PIDs []int
	// Text is the evidence file's body.
	Text string
}

func (s *SlowAlarm) clock() clockwork.Clock {
	if s.Clock != nil {
		return s.Clock
	}
	return clockwork.NewRealClock()
}

// watch arms the alarm for one stage of one landing. The returned context
// must run the stage, so the processes it starts are known; the returned func
// ends the watch and never waits on an alarm in flight.
func (s *SlowAlarm) watch(ctx context.Context, l *Lander, w Work, dir, stage string) (context.Context, func()) {
	if s == nil || s.After <= 0 {
		return ctx, func() {}
	}
	pids := &pidTracker{}
	ctx = context.WithValue(ctx, pidTrackerKey{}, pids)
	clk := s.clock()
	start := clk.Now()
	timer := clk.NewTimer(s.After)
	done := make(chan struct{})
	go func() {
		select {
		case <-timer.Chan():
			s.raise(ctx, l, w, dir, stage, clk.Since(start), pids.list())
		case <-done:
		case <-ctx.Done():
		}
	}()
	var once sync.Once
	return ctx, func() {
		once.Do(func() {
			timer.Stop()
			close(done)
		})
	}
}

func (s *SlowAlarm) raise(ctx context.Context, l *Lander, w Work, dir, stage string, elapsed time.Duration, roots []int) {
	elapsed = elapsed.Round(time.Second)
	l.logf("%s: SLOW %s %s", w.BeadID, stage, elapsed)
	defer func() {
		if r := recover(); r != nil {
			l.logf("%s: slow-landing alarm panicked: %v", w.BeadID, r)
		}
	}()
	// Detached from the landing: the evidence of a stage that just ended is
	// still worth the few seconds, and the budget caps them.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), slowCaptureBudget)
	defer cancel()
	run := s.run
	if run == nil {
		run = execOutput
	}
	snap := snapshotProcesses(cctx, roots, run)
	path, werr := s.writeEvidence(ctx, dir, stage, w, elapsed, snap)
	if s.Escalate == nil {
		return
	}
	s.Escalate(w.BeadID, stage, slowMessage(w, stage, elapsed, s.After, snap.PIDs, roots, path, werr))
}

func (s *SlowAlarm) writeEvidence(ctx context.Context, dir, stage string, w Work, elapsed time.Duration, snap slowSnapshot) (string, error) {
	if s.EvidenceDir == nil {
		return "", nil
	}
	evDir := s.EvidenceDir(ctx, dir)
	if evDir == "" {
		return "", nil
	}
	if err := os.MkdirAll(evDir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(evDir, "slow-"+stage+".txt")
	body := fmt.Sprintf("landing %s (%s @ %s): %s running %s, alarm after %s\n\n%s", w.BeadID, w.Branch, w.Head, stage, elapsed, s.After, snap.Text)
	return path, os.WriteFile(path, []byte(body), 0o600)
}

func slowMessage(w Work, stage string, elapsed, after time.Duration, tree, roots []int, evidence string, evidenceErr error) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Landing of %s (%s @ %s) is slow: its %s stage has run %s, past alarm_after %s. Nothing was killed; the stage deadlines decide that.",
		w.BeadID, w.Branch, w.Head, stage, elapsed, after)
	if len(tree) == 0 {
		tree = roots
	}
	if len(tree) == 0 {
		b.WriteString(" No child process is running (waiting on the container slot or a lint lock).")
	} else {
		shown := tree
		if len(shown) > maxSlowPIDs {
			shown = shown[:maxSlowPIDs]
		}
		fmt.Fprintf(&b, " Child pids: %s", joinInts(shown))
		if extra := len(tree) - len(shown); extra > 0 {
			fmt.Fprintf(&b, " (+%d more)", extra)
		}
		b.WriteString(".")
	}
	switch {
	case evidenceErr != nil:
		fmt.Fprintf(&b, " Evidence file not written: %v.", evidenceErr)
	case evidence != "":
		fmt.Fprintf(&b, " Process tree and TCP peers: %s.", evidence)
	}
	return b.String()
}

func joinInts(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, " ")
}

// pidTracker holds the pids of the processes a stage is running now.
type pidTracker struct {
	mu   sync.Mutex
	pids map[int]struct{}
}

type pidTrackerKey struct{}

// trackPID records pid on the stage ctx belongs to, if the alarm watches it,
// and returns the func that forgets it.
func trackPID(ctx context.Context, pid int) func() {
	t, _ := ctx.Value(pidTrackerKey{}).(*pidTracker)
	if t == nil {
		return func() {}
	}
	t.mu.Lock()
	if t.pids == nil {
		t.pids = map[int]struct{}{}
	}
	t.pids[pid] = struct{}{}
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		delete(t.pids, pid)
		t.mu.Unlock()
	}
}

func (t *pidTracker) list() []int {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]int, 0, len(t.pids))
	for p := range t.pids {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}

// procRow is one line of ps output.
type procRow struct {
	pid, ppid, pgid int
	line            string
}

// snapshotProcesses captures the process tree under roots (pid, state, cpu,
// command) and the TCP connections those processes hold. Each external command
// is bounded by ctx; a failure is written into the text, never returned, so
// the alarm still goes out.
func snapshotProcesses(ctx context.Context, roots []int, run diagRunner) slowSnapshot {
	var text strings.Builder
	var pids []int
	text.WriteString("== process tree (pid ppid pgid stat %cpu elapsed command)\n")
	if len(roots) == 0 {
		text.WriteString("no child process was running\n")
		return slowSnapshot{Text: text.String()}
	}
	out, err := run(ctx, "ps", "-axo", "pid=,ppid=,pgid=,stat=,pcpu=,etime=,command=")
	if err != nil {
		fmt.Fprintf(&text, "ps failed: %v\n", err)
	} else {
		for _, r := range descendants(parsePS(out), roots) {
			pids = append(pids, r.pid)
			text.WriteString(r.line + "\n")
		}
		if len(pids) == 0 {
			text.WriteString("the stage's processes had exited by the time of the capture\n")
		}
	}
	text.WriteString("\n== open TCP peers (lsof -nP -a -iTCP -p <pids>)\n")
	if len(pids) == 0 {
		text.WriteString("no live process to inspect\n")
		return slowSnapshot{PIDs: pids, Text: text.String()}
	}
	lsof, err := run(ctx, "lsof", "-nP", "-a", "-iTCP", "-p", joinCSV(pids))
	var exit *exec.ExitError
	switch {
	case err == nil || strings.TrimSpace(lsof) != "":
		text.WriteString(lsof)
	case errors.As(err, &exit):
		// lsof exits 1 when nothing matches.
		text.WriteString("none\n")
	default:
		fmt.Fprintf(&text, "lsof failed: %v\n", err)
	}
	return slowSnapshot{PIDs: pids, Text: text.String()}
}

func joinCSV(ns []int) string {
	return strings.ReplaceAll(joinInts(ns), " ", ",")
}

// diagRunner runs an evidence command and returns its stdout; tests answer it
// with canned output.
type diagRunner func(ctx context.Context, name string, args ...string) (string, error)

// execOutput runs name and returns its stdout. ctx's deadline kills it.
func execOutput(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: fixed diagnostic commands
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	return out.String(), err
}

// parsePS reads `ps -axo pid=,ppid=,pgid=,...` output.
func parsePS(out string) []procRow {
	var rows []procRow
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 7 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		pgid, e3 := strconv.Atoi(f[2])
		if e1 != nil || e2 != nil || e3 != nil {
			continue
		}
		rows = append(rows, procRow{pid: pid, ppid: ppid, pgid: pgid, line: strings.TrimSpace(line)})
	}
	return rows
}

// descendants returns the rows of roots and everything under them, in pid
// order. A row also counts when it shares a root's process group: the gate
// starts each step in its own group, which a child that reparents stays in.
func descendants(rows []procRow, roots []int) []procRow {
	in := map[int]bool{}
	groups := map[int]bool{}
	for _, r := range roots {
		in[r] = true
		groups[r] = true
	}
	for changed := true; changed; {
		changed = false
		for _, r := range rows {
			if in[r.pid] {
				continue
			}
			if in[r.ppid] || groups[r.pgid] {
				in[r.pid] = true
				changed = true
			}
		}
	}
	var out []procRow
	for _, r := range rows {
		if in[r.pid] {
			out = append(out, r)
		}
	}
	slices.SortFunc(out, func(a, b procRow) int { return a.pid - b.pid })
	return out
}
