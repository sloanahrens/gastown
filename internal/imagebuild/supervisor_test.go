package imagebuild

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const buildPid = 4242

// fakeGroup is the build's process group. A signal in diesOn ends it, after
// leaveAfter further probes: the group does not leave the instant it is
// signalled. probeErr, when set, is what probing a group that is still there
// returns (EPERM, for a group the supervisor may not signal).
type fakeGroup struct {
	mu         sync.Mutex
	alive      bool
	diesOn     map[syscall.Signal]bool
	leaveAfter int
	probeErr   error

	sent     []syscall.Signal
	probes   int
	dying    bool
	leftIn   int
	exited   chan struct{}
	exitOnce sync.Once
}

func newGroup(diesOn ...syscall.Signal) *fakeGroup {
	g := &fakeGroup{alive: true, diesOn: map[syscall.Signal]bool{}, exited: make(chan struct{})}
	for _, s := range diesOn {
		g.diesOn[s] = true
	}
	return g
}

func (g *fakeGroup) signal(pgid int, sig syscall.Signal) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if pgid != buildPid {
		return fmt.Errorf("signalled group %d, want the build's %d", pgid, buildPid)
	}
	if sig == 0 {
		g.probes++
		if g.dying {
			if g.leftIn > 0 {
				g.leftIn--
			}
			if g.leftIn == 0 {
				g.alive = false
				g.exitOnce.Do(func() { close(g.exited) })
			}
		}
		if !g.alive {
			return syscall.ESRCH
		}
		return g.probeErr
	}
	g.sent = append(g.sent, sig)
	if g.diesOn[sig] {
		g.dying, g.leftIn = true, g.leaveAfter
	}
	return nil
}

func (g *fakeGroup) isAlive() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.alive
}

func (g *fakeGroup) signals() []syscall.Signal {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]syscall.Signal(nil), g.sent...)
}

// fakeBuild ends when its result is sent, when its group is gone, or when the
// test ends: the goroutine that waits on it must not outlive the test.
type fakeBuild struct {
	g        *fakeGroup
	result   chan waited
	stop     chan struct{}
	started  bool
	entered  chan struct{}
	returned chan struct{}
}

func newBuild(t *testing.T, g *fakeGroup) *fakeBuild {
	b := &fakeBuild{g: g, result: make(chan waited, 1), stop: make(chan struct{}),
		entered: make(chan struct{}), returned: make(chan struct{})}
	t.Cleanup(func() {
		close(b.stop)
		if b.started {
			<-b.returned // Wait returns once stop is closed
		}
	})
	return b
}

func (b *fakeBuild) Pid() int { return buildPid }

func (b *fakeBuild) Wait() (int, syscall.Signal) {
	close(b.entered)
	defer close(b.returned)
	var gone <-chan struct{}
	if b.g != nil {
		gone = b.g.exited
	}
	select {
	case <-gone:
		return 0, syscall.SIGKILL
	case r := <-b.result:
		return r.code, r.sig
	case <-b.stop:
		return 0, syscall.SIGKILL
	}
}

// finishing is a build that ends by itself with code or sig.
func finishing(t *testing.T, code int, sig syscall.Signal) *fakeBuild {
	b := newBuild(t, nil)
	b.result <- waited{code, sig}
	return b
}

// lockFS is the lock directory, in memory.
type lockFS struct {
	files   map[string]string
	removed []string
	// atRemoval is the group's state the moment the lock is removed.
	atRemoval func()
}

func (f *lockFS) read(path string) ([]byte, error) {
	if v, ok := f.files[path]; ok {
		return []byte(v), nil
	}
	return nil, errors.New("no such file")
}

func (f *lockFS) remove(path string) error {
	if f.atRemoval != nil {
		f.atRemoval()
	}
	f.removed = append(f.removed, path)
	return nil
}

// escalations records the commands the supervisor ran, failing the first
// failFirst of them.
type escalations struct {
	calls     [][]string
	failFirst int
	err       error
}

func (e *escalations) run(name string, args ...string) error {
	e.calls = append(e.calls, append([]string{name}, args...))
	if len(e.calls) <= e.failFirst {
		if e.err != nil {
			return e.err
		}
		return errors.New("escalate failed")
	}
	return nil
}

// rig builds a system around a build, with an alarm that fires at once when
// alarm is true and never otherwise.
type rig struct {
	sys    system
	fs     *lockFS
	esc    *escalations
	slept  time.Duration
	stderr strings.Builder
}

func newRig(b *fakeBuild, g *fakeGroup, alarm bool) *rig {
	r := &rig{fs: &lockFS{files: map[string]string{}}, esc: &escalations{}}
	r.sys = system{
		start: func(Config) (build, error) {
			if b == nil {
				return nil, errors.New("no build")
			}
			b.started = true
			return b, nil
		},
		signal:       g.signalOrNone,
		after:        func(time.Duration) <-chan time.Time { return alarmChan(alarm) },
		sleep:        func(d time.Duration) { r.slept += d },
		readFile:     r.fs.read,
		removeAll:    r.fs.remove,
		command:      r.esc.run,
		termGrace:    time.Second,
		killGrace:    5 * time.Second,
		pollInterval: 50 * time.Millisecond,
	}
	return r
}

// signalOrNone lets a rig with no group (a build that finishes by itself) share
// the constructor.
func (g *fakeGroup) signalOrNone(pgid int, sig syscall.Signal) error {
	if g == nil {
		return syscall.ESRCH
	}
	return g.signal(pgid, sig)
}

func alarmChan(fire bool) <-chan time.Time {
	if !fire {
		return nil
	}
	ch := make(chan time.Time)
	close(ch)
	return ch
}

func (r *rig) config(mutate ...func(*Config)) Config {
	cfg := Config{
		Alarm:   7 * time.Second,
		Lock:    "/town/image.lock",
		Command: []string{"bash", "run.sh"},
		Stderr:  &r.stderr,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	return cfg
}

func withFingerprint(c *Config) { c.GT, c.Fingerprint = "/bin/gt", "install-gt:image-build-failed" }

func TestFinishedBuildReturnsItsOwnStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		code int
		sig  syscall.Signal
		want int
	}{
		{"success", 0, 0, 0},
		{"failure", 3, 0, 3},
		{"killed by a signal", 0, syscall.SIGKILL, 128 + int(syscall.SIGKILL)},
		{"terminated", 0, syscall.SIGTERM, 128 + int(syscall.SIGTERM)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(finishing(t, tc.code, tc.sig), nil, false)

			got := r.sys.run(r.config())

			if got != tc.want {
				t.Errorf("status %d, want %d", got, tc.want)
			}
			if len(r.esc.calls) != 0 {
				t.Errorf("escalated without a fingerprint: %v", r.esc.calls)
			}
			if r.stderr.Len() != 0 {
				t.Errorf("a build that finished wrote %q", r.stderr.String())
			}
		})
	}
}

func TestFailedBuildEscalatesAndFinishedBuildClears(t *testing.T) {
	t.Parallel()
	t.Run("failed", func(t *testing.T) {
		r := newRig(finishing(t, 3, 0), nil, false)

		got := r.sys.run(r.config(withFingerprint))

		if got != 3 {
			t.Errorf("status %d, want 3", got)
		}
		if len(r.esc.calls) != 1 {
			t.Fatalf("%d commands, want one escalation: %v", len(r.esc.calls), r.esc.calls)
		}
		line := strings.Join(r.esc.calls[0], " ")
		for _, want := range []string{"/bin/gt escalate ", "did not finish (exit 3)", "-s medium", "--source script:install-gt", "--fingerprint install-gt:image-build-failed"} {
			if !strings.Contains(line, want) {
				t.Errorf("escalation %q does not contain %q", line, want)
			}
		}
	})
	t.Run("finished", func(t *testing.T) {
		r := newRig(finishing(t, 0, 0), nil, false)

		got := r.sys.run(r.config(withFingerprint))

		if got != 0 {
			t.Errorf("status %d, want 0", got)
		}
		want := "/bin/gt escalate clear --fingerprint install-gt:image-build-failed --reason install-gt: a detached image build finished"
		if len(r.esc.calls) != 1 || strings.Join(r.esc.calls[0], " ") != want {
			t.Errorf("commands %v, want exactly %q", r.esc.calls, want)
		}
	})
}

func TestRefusedEscalationIsTriedTwiceThenNamedInTheLog(t *testing.T) {
	t.Parallel()
	t.Run("second try works", func(t *testing.T) {
		r := newRig(finishing(t, 1, 0), nil, false)
		r.esc.failFirst = 1

		r.sys.run(r.config(withFingerprint))

		if len(r.esc.calls) != 2 {
			t.Errorf("%d tries, want 2", len(r.esc.calls))
		}
		if r.stderr.Len() != 0 {
			t.Errorf("logged %q though the second try reached the town", r.stderr.String())
		}
	})
	t.Run("both refused", func(t *testing.T) {
		r := newRig(finishing(t, 1, 0), nil, false)
		r.esc.failFirst = 2

		got := r.sys.run(r.config(withFingerprint))

		if len(r.esc.calls) != 2 {
			t.Errorf("%d tries, want 2", len(r.esc.calls))
		}
		if got != 1 {
			t.Errorf("status %d, want the build's 1", got)
		}
		if !strings.Contains(r.stderr.String(), "the escalation for this build did not reach the town") {
			t.Errorf("log %q does not name the lost escalation", r.stderr.String())
		}
	})
}

func TestNoEscalationWithoutBothGTAndFingerprint(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*Config){
		"fingerprint only": func(c *Config) { c.Fingerprint = "fp" },
		"gt only":          func(c *Config) { c.GT = "/bin/gt" },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(finishing(t, 1, 0), nil, false)

			r.sys.run(r.config(mutate))

			if len(r.esc.calls) != 0 {
				t.Errorf("escalated: %v", r.esc.calls)
			}
		})
	}
}

func TestBuildThatCannotStartIsAFailureOfTheBuild(t *testing.T) {
	t.Parallel()
	r := newRig(nil, nil, false)
	r.sys.start = func(Config) (build, error) { return nil, errors.New("exec format error") }

	got := r.sys.run(r.config(withFingerprint))

	if got != ExitStart {
		t.Errorf("status %d, want %d", got, ExitStart)
	}
	if !strings.Contains(r.stderr.String(), "exec format error") {
		t.Errorf("log %q does not name why the build did not start", r.stderr.String())
	}
	if len(r.esc.calls) != 1 || !strings.Contains(strings.Join(r.esc.calls[0], " "), "(exit 127)") {
		t.Errorf("escalations %v, want one for exit 127", r.esc.calls)
	}
}

// lockNaming is a lock directory whose pid file holds pid.
func lockNaming(r *rig, pid string) {
	r.fs.files[filepath.Join("/town/image.lock", "pid")] = pid
}

func TestAlarmStopsTheGroupAndRemovesTheLockOnlyOnceItIsGone(t *testing.T) {
	t.Parallel()
	g := newGroup(syscall.SIGTERM)
	g.leaveAfter = 3
	r := newRig(newBuild(t, g), g, true)
	lockNaming(r, fmt.Sprintf("%d\n", buildPid))
	aliveAtRemoval := true
	r.fs.atRemoval = func() { aliveAtRemoval = g.isAlive() }

	got := r.sys.run(r.config())

	if got != ExitAlarm {
		t.Errorf("status %d, want %d", got, ExitAlarm)
	}
	if sent := g.signals(); len(sent) != 1 || sent[0] != syscall.SIGTERM {
		t.Errorf("signals %v, want only TERM: the group left within its grace", sent)
	}
	if len(r.fs.removed) != 1 || r.fs.removed[0] != "/town/image.lock" {
		t.Errorf("removed %v, want the build's lock", r.fs.removed)
	}
	if aliveAtRemoval {
		t.Error("the lock was removed while the group was still alive")
	}
	if !strings.Contains(r.stderr.String(), "alarm after 7 s stopped the build") {
		t.Errorf("log %q does not say the alarm stopped the build", r.stderr.String())
	}
	if r.slept == 0 || r.slept >= time.Second {
		t.Errorf("waited %s for a group that leaves in 3 probes, want a few polls and under the grace", r.slept)
	}
}

func TestAlarmKillsAGroupThatIgnoresTerm(t *testing.T) {
	t.Parallel()
	g := newGroup(syscall.SIGKILL)
	r := newRig(newBuild(t, g), g, true)
	lockNaming(r, fmt.Sprint(buildPid))

	got := r.sys.run(r.config())

	if got != ExitAlarm {
		t.Errorf("status %d, want %d", got, ExitAlarm)
	}
	if sent := g.signals(); len(sent) != 2 || sent[0] != syscall.SIGTERM || sent[1] != syscall.SIGKILL {
		t.Errorf("signals %v, want TERM then KILL", sent)
	}
	if r.slept < time.Second {
		t.Errorf("waited %s, want at least the 1s TERM grace before KILL", r.slept)
	}
	if len(r.fs.removed) != 1 {
		t.Errorf("removed %v, want the lock once the group is gone", r.fs.removed)
	}
}

func TestAlarmKeepsTheLockWhenTheStopCannotBeConfirmed(t *testing.T) {
	t.Parallel()
	cases := map[string]*fakeGroup{
		"the group survives KILL":          newGroup(),
		"the group refuses probes (EPERM)": func() *fakeGroup { g := newGroup(); g.probeErr = syscall.EPERM; return g }(),
		"the group stays after the signal": func() *fakeGroup { g := newGroup(syscall.SIGKILL); g.leaveAfter = 1 << 30; return g }(),
	}
	for name, g := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(newBuild(t, g), g, true)
			lockNaming(r, fmt.Sprint(buildPid))

			got := r.sys.run(r.config())

			if got != ExitAlarm {
				t.Errorf("status %d, want %d", got, ExitAlarm)
			}
			if len(r.fs.removed) != 0 {
				t.Errorf("removed %v: a lock must stay when the group may still be running", r.fs.removed)
			}
			if !strings.Contains(r.stderr.String(), "could not confirm the build stopped; the lock was not removed") {
				t.Errorf("log %q does not say the stop was not confirmed", r.stderr.String())
			}
			if sent := g.signals(); len(sent) != 2 {
				t.Errorf("signals %v, want TERM then KILL", sent)
			}
			bound := r.sys.termGrace + r.sys.killGrace + 2*r.sys.pollInterval
			if r.slept > bound {
				t.Errorf("waited %s, want at most %s: every wait is bounded", r.slept, bound)
			}
		})
	}
}

func TestAlarmLeavesALockThatNamesAnotherProcess(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*rig){
		"another pid":    func(r *rig) { lockNaming(r, "99999") },
		"not a number":   func(r *rig) { lockNaming(r, "garbage") },
		"empty pid file": func(r *rig) { lockNaming(r, "") },
		"no pid file":    func(r *rig) {},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			g := newGroup(syscall.SIGTERM)
			r := newRig(newBuild(t, g), g, true)
			setup(r)

			got := r.sys.run(r.config())

			if got != ExitAlarm || len(r.fs.removed) != 0 {
				t.Errorf("status %d, removed %v: a lock that is not the stopped build's must stay", got, r.fs.removed)
			}
			if !strings.Contains(r.stderr.String(), "stopped the build") {
				t.Errorf("log %q: the build was stopped and should say so", r.stderr.String())
			}
		})
	}
}

func TestAlarmWithNoLockConfiguredRemovesNothing(t *testing.T) {
	t.Parallel()
	g := newGroup(syscall.SIGTERM)
	r := newRig(newBuild(t, g), g, true)

	r.sys.run(r.config(func(c *Config) { c.Lock = "" }))

	if len(r.fs.removed) != 0 {
		t.Errorf("removed %v with no lock configured", r.fs.removed)
	}
}

// A detached build's escalation is its only report to the town (gt-vhn89), so
// the alarm's stop must carry the fingerprint a stalled image is found by.
func TestStoppedBuildEscalatesAsExit124(t *testing.T) {
	t.Parallel()
	g := newGroup(syscall.SIGTERM)
	r := newRig(newBuild(t, g), g, true)

	got := r.sys.run(r.config(withFingerprint))

	if got != ExitAlarm {
		t.Errorf("status %d, want %d", got, ExitAlarm)
	}
	if len(r.esc.calls) != 1 {
		t.Fatalf("%d commands, want one escalation: %v", len(r.esc.calls), r.esc.calls)
	}
	line := strings.Join(r.esc.calls[0], " ")
	for _, want := range []string{"/bin/gt escalate ", "did not finish (exit 124)", "-s medium", "--source script:install-gt", "--fingerprint install-gt:image-build-failed"} {
		if !strings.Contains(line, want) {
			t.Errorf("escalation %q does not contain %q", line, want)
		}
	}
}

func TestCloseInheritableClosesOnlyWhatAChildWouldInherit(t *testing.T) {
	t.Parallel()
	// The descriptors are moved to high numbers first. Whatever else this
	// process opens is handed the lowest free number, so a number this high is
	// not reused between the close and the check that it is closed.
	const floor = 100
	var pipe [2]int
	if err := unix.Pipe(pipe[:]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { unix.Close(pipe[0]); unix.Close(pipe[1]) })
	inheritable := highFd(t, pipe[0], floor, false)
	protected := highFd(t, pipe[1], floor, true)
	t.Cleanup(func() { unix.Close(protected) })

	got := closeInheritable(inheritable, inheritable)
	if len(got) != 1 || got[0] != inheritable {
		t.Errorf("closed %v, want the inheritable fd %d", got, inheritable)
	}
	if err := unix.Fstat(inheritable, &unix.Stat_t{}); !errors.Is(err, unix.EBADF) {
		t.Errorf("fd %d after closeInheritable: Fstat = %v, want EBADF (it was not closed)", inheritable, err)
	}
	if got := closeInheritable(protected, protected); len(got) != 0 {
		t.Errorf("closed %v: a close-on-exec descriptor is the runtime's or the caller's, not an inherited one", got)
	}
	if err := unix.Fstat(protected, &unix.Stat_t{}); err != nil {
		t.Errorf("the close-on-exec fd %d was closed: %v", protected, err)
	}
}

// highFd duplicates fd to the lowest free number at or above floor, close-on-exec
// when cloexec is set. F_DUPFD leaves it inheritable.
func highFd(t *testing.T, fd, floor int, cloexec bool) int {
	t.Helper()
	n, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD, floor)
	if err != nil {
		t.Fatalf("duplicate fd %d above %d: %v", fd, floor, err)
	}
	if n > 255 {
		t.Fatalf("fd %d is above the range closeInheritable covers: this process has over %d descriptors open", n, floor)
	}
	if cloexec {
		unix.CloseOnExec(n)
	}
	return n
}

// alarmLine reports whether a byte arrived on fd before the stream ended: a line
// fires the alarm, and end of file, like an error, ends the reader instead of
// spinning on a zero-byte read (dae70fc4, gt-86xpn).
//
// It sits in this untagged file because two test binaries read it: the unit test
// below, and runAsSupervisor in process_integration_test.go.
func alarmLine(fd int) bool { return alarmLineFrom(fd, unix.Read) }

// alarmLineFrom is alarmLine over a read function, so a test can bound the
// reader without a clock: a stream that has already ended cannot tell a reader
// that stopped from one that asked again, so the test's read fails that ask.
func alarmLineFrom(fd int, read readFunc) bool {
	buf := make([]byte, 1)
	for {
		n, err := read(fd, buf)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return n > 0
	}
}

// readFunc is the read alarmLineFrom makes: unix.Read, or a test's stand-in.
type readFunc func(fd int, p []byte) (int, error)

// TestAlarmLine pins the reader the supervisor's alarm stub runs: a written line
// fires the alarm, and the end of the stream ends the reader without firing it,
// whether that end is a pipe's end of file or a zero-byte read with no error, as
// /dev/null gives (dae70fc4). A reader that did not stop would spin, so every
// case bounds it with a read that fails the test on a second call: a unit test
// may not use a clock to stop a spin.
func TestAlarmLine(t *testing.T) {
	t.Parallel()
	t.Run("a written line fires the alarm", func(t *testing.T) {
		t.Parallel()
		p := newAlarmPipe(t)
		if _, err := unix.Write(p.w, []byte("fire\n")); err != nil {
			t.Fatal(err)
		}
		if !alarmLine(p.r) {
			t.Error("alarmLine() = false, want true: a line on the pipe fires the alarm")
		}
	})
	t.Run("end of file ends the reader without firing", func(t *testing.T) {
		t.Parallel()
		p := newAlarmPipe(t)
		p.closeWrite(t)
		if alarmLineFrom(p.r, atMostOnce(t, unix.Read)) {
			t.Error("alarmLineFrom() = true, want false: end of file is not a line")
		}
	})
	t.Run("a zero-byte read with no error ends the reader too", func(t *testing.T) {
		t.Parallel()
		// /dev/null, which the supervisor hands the build as stdin, answers
		// every read with zero bytes and no error; the reader must stop there.
		read := atMostOnce(t, func(int, []byte) (int, error) { return 0, nil })
		if alarmLineFrom(-1, read) {
			t.Error("alarmLineFrom() = true, want false: a zero-byte read is not a line")
		}
	})
}

// atMostOnce fails the test when read is called a second time: the stream has
// already ended, and a unit test cannot use a clock to stop a reader that asks
// again.
func atMostOnce(t *testing.T, read readFunc) readFunc {
	t.Helper()
	calls := 0
	return func(fd int, p []byte) (int, error) {
		calls++
		if calls > 1 {
			t.Fatalf("the reader asked %d times, want one look at the end of the stream", calls)
		}
		return read(fd, p)
	}
}

// alarmPipe is a pipe whose open ends the test's cleanup closes. A test that
// closes an end itself marks it closed first, so cleanup never closes a
// descriptor number a parallel test may own by then.
type alarmPipe struct{ r, w int }

func newAlarmPipe(t *testing.T) *alarmPipe {
	t.Helper()
	var fds [2]int
	if err := unix.Pipe(fds[:]); err != nil {
		t.Fatal(err)
	}
	p := &alarmPipe{r: fds[0], w: fds[1]}
	t.Cleanup(func() {
		for _, fd := range []int{p.r, p.w} {
			if fd >= 0 {
				_ = unix.Close(fd)
			}
		}
	})
	return p
}

// closeWrite closes the write end and marks it closed for the cleanup.
func (p *alarmPipe) closeWrite(t *testing.T) {
	t.Helper()
	if err := unix.Close(p.w); err != nil {
		t.Fatal(err)
	}
	p.w = -1
}
