package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/hostprobe"
	"github.com/steveyegge/gastown/internal/templates"
)

// hostAgentEpoch is the fixed instant the loop tests start their clock at.
// NewFakeClock reads the wall clock, which would make a test depend on when it
// runs.
var hostAgentEpoch = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

// startHostAgentLoop runs l in the background and returns the channel its exit
// is reported on. A loop that never exits hangs the test rather than passing
// it: there is no timeout to spend, and the go test timeout names the test.
func startHostAgentLoop(l *hostAgentLoop, ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() { done <- l.run(ctx) }()
	return done
}

// waitForNextTick advances nothing and returns once the loop is blocked on its
// clock. Advancing before that lands the advance before the wait, and the tick
// then arrives a whole interval late; a loop that stopped instead of waiting
// fails the test here rather than blocking it forever.
func waitForNextTick(t *testing.T, ctx context.Context, clock *clockwork.FakeClock, done <-chan error) {
	t.Helper()
	blocked := make(chan error, 1)
	go func() { blocked <- clock.BlockUntilContext(ctx, 1) }()
	select {
	case err := <-blocked:
		if err != nil {
			t.Fatalf("waiting for the loop's next tick: %v", err)
		}
	case err := <-done:
		t.Fatalf("loop stopped before it waited for its next tick: %v", err)
	}
}

// TestHostAgentLoop_WritesThreeTicksOneMinuteApart is the agent's whole
// contract: a write at once, then one every interval, each stamped with the
// clock's reading at that tick.
func TestHostAgentLoop_WritesThreeTicksOneMinuteApart(t *testing.T) {
	t.Parallel()
	clock := clockwork.NewFakeClockAt(hostAgentEpoch)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const path = "/town/.runtime/host/probes.json"
	writes := make(chan time.Time, 8)
	loop := &hostAgentLoop{
		clock:    clock,
		interval: hostAgentInterval,
		path:     path,
		collect: func(_ context.Context, now time.Time) hostprobe.Probes {
			return hostprobe.Probes{At: now}
		},
		write: func(got string, p hostprobe.Probes) error {
			if got != path {
				t.Errorf("write path = %q, want %q", got, path)
			}
			writes <- p.At
			return nil
		},
		logf: func(string, ...any) {},
	}
	done := startHostAgentLoop(loop, ctx)

	for i := range 3 {
		want := hostAgentEpoch.Add(time.Duration(i) * hostAgentInterval)
		select {
		case got := <-writes:
			if !got.Equal(want) {
				t.Errorf("write %d stamped %s, want %s", i+1, got, want)
			}
		case err := <-done:
			t.Fatalf("loop stopped after %d write(s): %v", i, err)
		}
		if i < 2 {
			waitForNextTick(t, ctx, clock, done)
			clock.Advance(hostAgentInterval)
		}
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("loop after cancel = %v, want nil (a clean stop)", err)
	}
	if n := len(writes); n != 0 {
		t.Errorf("%d write(s) after cancel, want none", n)
	}
}

// TestHostAgentLoop_CancelStopsWriting covers SIGTERM's path: the context ends
// while the loop waits for its next tick, and nothing is written after it.
func TestHostAgentLoop_CancelStopsWriting(t *testing.T) {
	t.Parallel()
	clock := clockwork.NewFakeClockAt(hostAgentEpoch)
	ctx, cancel := context.WithCancel(context.Background())

	writes := make(chan time.Time, 8)
	loop := &hostAgentLoop{
		clock:    clock,
		interval: hostAgentInterval,
		path:     "/town/.runtime/host/probes.json",
		collect:  func(_ context.Context, now time.Time) hostprobe.Probes { return hostprobe.Probes{At: now} },
		write: func(_ string, p hostprobe.Probes) error {
			writes <- p.At
			return nil
		},
		logf: func(string, ...any) {},
	}
	done := startHostAgentLoop(loop, ctx)

	select {
	case <-writes:
	case err := <-done:
		t.Fatalf("loop stopped before its first write: %v", err)
	}
	// Cancel while the loop waits, so the test pins the wait's exit rather than
	// racing a write that is already running.
	waitForNextTick(t, ctx, clock, done)
	cancel()
	if err := <-done; err != nil {
		t.Errorf("loop after cancel = %v, want nil (a clean stop)", err)
	}
	clock.Advance(3 * hostAgentInterval)
	if n := len(writes); n != 0 {
		t.Errorf("%d write(s) after cancel, want none", n)
	}
}

// TestHostAgentLoop_DropsATickCancelledMidCollect covers the stop that lands
// while a tick's reads are running: the reading is dropped, not written after
// the job was told to stop.
func TestHostAgentLoop_DropsATickCancelledMidCollect(t *testing.T) {
	t.Parallel()
	clock := clockwork.NewFakeClockAt(hostAgentEpoch)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	writes := make(chan time.Time, 8)
	var calls int
	loop := &hostAgentLoop{
		clock:    clock,
		interval: hostAgentInterval,
		path:     "/town/.runtime/host/probes.json",
		collect: func(_ context.Context, now time.Time) hostprobe.Probes {
			calls++
			if calls == 2 {
				// The stop arrives while this tick's collection is in flight.
				cancel()
			}
			return hostprobe.Probes{At: now}
		},
		write: func(_ string, p hostprobe.Probes) error {
			writes <- p.At
			return nil
		},
		logf: func(string, ...any) {},
	}
	done := startHostAgentLoop(loop, ctx)

	select {
	case <-writes:
	case err := <-done:
		t.Fatalf("loop stopped before its first write: %v", err)
	}
	waitForNextTick(t, ctx, clock, done)
	clock.Advance(hostAgentInterval)

	if err := <-done; err != nil {
		t.Errorf("loop after a mid-tick cancel = %v, want nil (a clean stop)", err)
	}
	if n := len(writes); n != 0 {
		t.Errorf("%d write(s) after a mid-tick cancel, want none", n)
	}
}

// TestHostAgentLoop_LogsAFailedWriteAndKeepsGoing covers the write-error
// branch: one failed tick is logged and the next tick still runs, so a
// transient failure does not turn into a job launchd restarts forever.
func TestHostAgentLoop_LogsAFailedWriteAndKeepsGoing(t *testing.T) {
	t.Parallel()
	clock := clockwork.NewFakeClockAt(hostAgentEpoch)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var logs []string
	failing := true
	writes := make(chan time.Time, 8)
	loop := &hostAgentLoop{
		clock:    clock,
		interval: hostAgentInterval,
		path:     "/town/.runtime/host/probes.json",
		collect:  func(_ context.Context, now time.Time) hostprobe.Probes { return hostprobe.Probes{At: now} },
		write: func(_ string, p hostprobe.Probes) error {
			mu.Lock()
			first := failing
			failing = false
			mu.Unlock()
			if first {
				return errors.New("no space left on device")
			}
			writes <- p.At
			return nil
		},
		logf: func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			logs = append(logs, fmt.Sprintf(format, args...))
		},
	}
	done := startHostAgentLoop(loop, ctx)

	// The loop can only be waiting on the clock once the failed write is done,
	// so this is also the point by which it was logged.
	waitForNextTick(t, ctx, clock, done)
	clock.Advance(hostAgentInterval)

	select {
	case got := <-writes:
		if want := hostAgentEpoch.Add(hostAgentInterval); !got.Equal(want) {
			t.Errorf("second tick stamped %s, want %s", got, want)
		}
	case err := <-done:
		t.Fatalf("a failed write ended the loop: %v", err)
	}

	mu.Lock()
	n := len(logs)
	mu.Unlock()
	if n != 1 {
		t.Errorf("logged %d failure(s), want 1: %v", n, logs)
	}
}

// TestRunHostAgentArgv_ReportsAFailedCommand drives the runner's error
// branch with a command that never starts: a Cmd carrying an error fails at
// Start, so no process is spawned and nothing is read from the host.
func TestRunHostAgentArgv_ReportsAFailedCommand(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	command := func(context.Context, string, ...string) *exec.Cmd {
		return &exec.Cmd{Err: boom}
	}
	out, err := runHostAgentArgv(context.Background(), []string{"gt", "version"}, command)
	if !errors.Is(err, boom) {
		t.Errorf("runHostAgentArgv error = %v, want %v", err, boom)
	}
	if out != nil {
		t.Errorf("runHostAgentArgv returned %q with a failed command, want no output", out)
	}
}

// TestInstallHostAgent_WritesThePlist checks the file install leaves behind:
// the render for the town it was given, at the path it was given, with the
// probe directory launchd's log file needs.
func TestInstallHostAgent_WritesThePlist(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(t.TempDir(), "com.gastown.host-agent.plist")
	got, err := installHostAgent(town, hostAgentInstallDepsFor(path, "darwin", templates.HostAgentPlist))
	if err != nil {
		t.Fatalf("installHostAgent: %v", err)
	}
	if got != path {
		t.Errorf("installHostAgent = %q, want %q", got, path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the installed plist: %v", err)
	}
	want, err := templates.HostAgentPlist(hostAgentTestGTPath, town)
	if err != nil {
		t.Fatalf("rendering the expected plist: %v", err)
	}
	if string(data) != want {
		t.Errorf("installed plist:\n%s\nwant:\n%s", data, want)
	}

	// launchd opens the plist's StandardOutPath before the program runs and
	// fails the job with EX_CONFIG if its directory is missing.
	if _, err := os.Stat(filepath.Dir(hostprobe.Path(town))); err != nil {
		t.Errorf("probe/log directory after install: %v", err)
	}
}

// TestInstallHostAgent_ReplacesItsOwnTownsPlist is the reinstall: a file that
// already serves this town is rewritten, not refused.
func TestInstallHostAgent_ReplacesItsOwnTownsPlist(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(t.TempDir(), "com.gastown.host-agent.plist")
	existing, err := templates.HostAgentPlist(hostAgentTestGTPath, town)
	if err != nil {
		t.Fatalf("rendering the existing plist: %v", err)
	}
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatalf("writing the existing plist: %v", err)
	}

	if _, err := installHostAgent(town, hostAgentInstallDepsFor(path, "darwin", templates.HostAgentPlist)); err != nil {
		t.Fatalf("installHostAgent over its own town's plist: %v", err)
	}
}

// TestInstallHostAgent_RefusesAPlistForAnotherTown covers the mismatch: the
// label names one job on the host, so a plist serving another town is refused
// and left untouched rather than silently repointed.
func TestInstallHostAgent_RefusesAPlistForAnotherTown(t *testing.T) {
	t.Parallel()
	live, scratch := t.TempDir(), t.TempDir()
	path := filepath.Join(t.TempDir(), "com.gastown.host-agent.plist")
	existing, err := templates.HostAgentPlist(hostAgentTestGTPath, live)
	if err != nil {
		t.Fatalf("rendering the existing plist: %v", err)
	}
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatalf("writing the existing plist: %v", err)
	}

	if _, err := installHostAgent(scratch, hostAgentInstallDepsFor(path, "darwin", templates.HostAgentPlist)); err == nil {
		t.Fatal("installHostAgent over another town's plist = nil, want a refusal")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the untouched plist: %v", err)
	}
	if string(data) != existing {
		t.Errorf("the refused install rewrote another town's plist:\n%s", data)
	}
}

// TestInstallHostAgent_RefusesAnUnreadableJob covers the file that exists but
// names no town: not knowing which town a job serves is not the same as
// knowing it serves this one, so install refuses it.
func TestInstallHostAgent_RefusesAnUnreadableJob(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(t.TempDir(), "com.gastown.host-agent.plist")
	if err := os.WriteFile(path, []byte("<plist><dict></dict></plist>\n"), 0o644); err != nil {
		t.Fatalf("writing the existing plist: %v", err)
	}
	if _, err := installHostAgent(town, hostAgentInstallDepsFor(path, "darwin", templates.HostAgentPlist)); err == nil {
		t.Fatal("installHostAgent over a plist with no town = nil, want a refusal")
	}
}

// TestInstallHostAgent_RefusesAJobItCannotInstall drives the two refusals that
// come before any file is written: an OS with no launchd, and no town root to
// render for.
func TestInstallHostAgent_RefusesAJobItCannotInstall(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// townRoot is built per case: a real temporary town in the macOS case,
		// so the install would succeed there were it not for the guard, and the
		// refusal is the guard's and not a permission error on some fixture path.
		townRoot func(t *testing.T) string
		goos     string
		render   func(gtPath, townRoot string) (string, error)
	}{
		{name: "not macOS", townRoot: func(t *testing.T) string { return t.TempDir() }, goos: "linux", render: templates.HostAgentPlist},
		// The renderer refuses an empty town root itself, so this case renders
		// through one that would happily write a file: the refusal under test
		// is install's own, and it has to come before anything is rendered.
		{name: "no town root", townRoot: func(*testing.T) string { return "" }, goos: "darwin",
			render: func(string, string) (string, error) { return "plist\n", nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "com.gastown.host-agent.plist")
			if _, err := installHostAgent(tc.townRoot(t), hostAgentInstallDepsFor(path, tc.goos, tc.render)); err == nil {
				t.Fatal("installHostAgent = nil, want a refusal")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("a refused install wrote %s", path)
			}
		})
	}
}

// TestInstallHostAgent_RefusesWhenItCannotFindItsParts drives the four host
// reads install makes before it writes anything, one failure at a time: the
// plist path, the gt binary, the render, and a town root it cannot create
// directories under.
func TestInstallHostAgent_RefusesWhenItCannotFindItsParts(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	// A regular file stands in for a directory that cannot be created: every
	// path below it fails with ENOTDIR, and nothing is written anywhere.
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing the blocking file: %v", err)
	}

	for _, tc := range []struct {
		name     string
		townRoot func(t *testing.T) string
		broken   func(d *hostAgentInstallDeps)
	}{
		{
			name:     "no plist path",
			townRoot: func(t *testing.T) string { return t.TempDir() },
			broken:   func(d *hostAgentInstallDeps) { d.plistPath = func() (string, error) { return "", boom } },
		},
		{
			name:     "no gt binary",
			townRoot: func(t *testing.T) string { return t.TempDir() },
			broken:   func(d *hostAgentInstallDeps) { d.gtPath = func() (string, error) { return "", boom } },
		},
		{
			name:     "render fails",
			townRoot: func(t *testing.T) string { return t.TempDir() },
			broken:   func(d *hostAgentInstallDeps) { d.render = func(string, string) (string, error) { return "", boom } },
		},
		{
			name:     "town root under a file",
			townRoot: func(t *testing.T) string { return filepath.Join(blocked, "town") },
			broken:   func(*hostAgentInstallDeps) {},
		},
		{
			name:     "plist path under a file",
			townRoot: func(t *testing.T) string { return t.TempDir() },
			broken: func(d *hostAgentInstallDeps) {
				d.plistPath = func() (string, error) { return filepath.Join(blocked, "job.plist"), nil }
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "com.gastown.host-agent.plist")
			d := hostAgentInstallDepsFor(path, "darwin", templates.HostAgentPlist)
			tc.broken(&d)
			if _, err := installHostAgent(tc.townRoot(t), d); err == nil {
				t.Fatal("installHostAgent = nil, want a refusal")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("a refused install wrote %s", path)
			}
		})
	}
}

// TestInstallHostAgent_ReportsAPlistItCannotWrite: a write that fails is
// reported, not swallowed into a success. The write is the dep, so the failure
// is the test's own rather than one that depends on the mode of a file and
// whether the test runs as root.
func TestInstallHostAgent_ReportsAPlistItCannotWrite(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := filepath.Join(t.TempDir(), "com.gastown.host-agent.plist")
	d := hostAgentInstallDepsFor(path, "darwin", templates.HostAgentPlist)
	d.writeFile = func(string, []byte, os.FileMode) error { return errors.New("read-only file system") }
	if _, err := installHostAgent(town, d); err == nil {
		t.Fatal("installHostAgent with a failing write = nil, want the error")
	}
}

// TestInstallHostAgent_RefusesATemporaryBinary covers the go run case: the
// binary it names is deleted when the run ends, so a job installed from it
// would fail at every start. It drives install itself, so deleting the refusal
// from install turns this red.
func TestInstallHostAgent_RefusesATemporaryBinary(t *testing.T) {
	t.Parallel()
	const tempDir = "/var/folders/xy/T"
	for _, tc := range []struct {
		name   string
		gtPath string
		want   bool
	}{
		{name: "go run under the temp directory", gtPath: tempDir + "/go-build123/b001/exe/gt", want: true},
		{name: "an installed binary", gtPath: "/usr/local/bin/gt", want: false},
		// A sibling whose name merely starts with the temp path is not in it.
		{name: "a sibling of the temp directory", gtPath: tempDir + "-elsewhere/gt", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "com.gastown.host-agent.plist")
			d := hostAgentInstallDepsFor(path, "darwin", templates.HostAgentPlist)
			d.gtPath = func() (string, error) { return tc.gtPath, nil }
			d.tempDir = func() string { return tempDir }
			_, err := installHostAgent(t.TempDir(), d)
			if tc.want && err == nil {
				t.Errorf("installHostAgent with gt at %q = nil, want a refusal", tc.gtPath)
			}
			if !tc.want && err != nil {
				t.Errorf("installHostAgent with gt at %q = %v, want nil", tc.gtPath, err)
			}
		})
	}
}

// TestHostAgentWrite_PublishesUnderANewDirectory: the agent's first run has to
// create .runtime/host itself, since nothing does it for a town that never ran
// the agent.
func TestHostAgentWrite_PublishesUnderANewDirectory(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ".runtime", "host", "probes.json")
	if err := hostAgentWrite(path, hostprobe.Probes{At: hostAgentEpoch}); err != nil {
		t.Fatalf("hostAgentWrite: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("probe file after hostAgentWrite: %v", err)
	}
}

// TestHostAgentWrite_ReportsAPathItCannotCreate is the other side: a town root
// under a file cannot hold the probe directory, and the error says so instead
// of the tick passing.
func TestHostAgentWrite_ReportsAPathItCannotCreate(t *testing.T) {
	t.Parallel()
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing the blocking file: %v", err)
	}
	if err := hostAgentWrite(filepath.Join(blocked, "probes.json"), hostprobe.Probes{At: hostAgentEpoch}); err == nil {
		t.Error("hostAgentWrite under a file = nil, want an error")
	}
}

// TestRunHostAgentCommand_RunsTheLoopForTheResolvedTown is the run command's
// wiring: it resolves the town and hands it to the loop.
func TestRunHostAgentCommand_RunsTheLoopForTheResolvedTown(t *testing.T) {
	t.Parallel()
	var ranTown string
	d := hostAgentDeps{
		findTownRoot: func() (string, error) { return "/towns/live", nil },
		run: func(_ context.Context, townRoot string) error {
			ranTown = townRoot
			return nil
		},
		install: func(string) (string, error) {
			t.Error("install ran for a run command")
			return "", nil
		},
		out: io.Discard,
	}
	if err := runHostAgentCommand(context.Background(), d); err != nil {
		t.Fatalf("runHostAgentCommand: %v", err)
	}
	if ranTown != "/towns/live" {
		t.Errorf("loop ran for town %q, want /towns/live", ranTown)
	}
}

// TestRunHostAgentCommand_RefusesOutsideATown: with no town the loop has no
// probe file to write, so nothing runs.
func TestRunHostAgentCommand_RefusesOutsideATown(t *testing.T) {
	t.Parallel()
	d := hostAgentDeps{
		findTownRoot: func() (string, error) { return "", errors.New("no town here") },
		run: func(context.Context, string) error {
			t.Error("the loop ran with no town root")
			return nil
		},
		install: func(string) (string, error) { return "", nil },
		out:     io.Discard,
	}
	if err := runHostAgentCommand(context.Background(), d); err == nil {
		t.Fatal("runHostAgentCommand outside a town = nil, want an error")
	}
}

// TestRunHostAgentInstallCommand_ReportsTheWrittenPlist checks what the
// operator sees: the path install returned and the command that loads it.
func TestRunHostAgentInstallCommand_ReportsTheWrittenPlist(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	const path = "/home/toast/Library/LaunchAgents/com.gastown.host-agent.plist"
	d := hostAgentDeps{
		findTownRoot: func() (string, error) { return "/towns/live", nil },
		run:          func(context.Context, string) error { return nil },
		install:      func(townRoot string) (string, error) { return path, nil },
		out:          &out,
	}
	if err := runHostAgentInstallCommand(context.Background(), d); err != nil {
		t.Fatalf("runHostAgentInstallCommand: %v", err)
	}
	if !strings.Contains(out.String(), path) {
		t.Errorf("install output does not name %s:\n%s", path, out.String())
	}
	if !strings.Contains(out.String(), "launchctl bootstrap") {
		t.Errorf("install output does not tell the operator how to load the job:\n%s", out.String())
	}
}

// TestRunHostAgentInstallCommand_RefusesOutsideATown: a plist names a town to
// write for, so install has nothing to render without one.
func TestRunHostAgentInstallCommand_RefusesOutsideATown(t *testing.T) {
	t.Parallel()
	d := hostAgentDeps{
		findTownRoot: func() (string, error) { return "", errors.New("no town here") },
		run:          func(context.Context, string) error { return nil },
		install: func(string) (string, error) {
			t.Error("install ran with no town root")
			return "", nil
		},
		out: io.Discard,
	}
	if err := runHostAgentInstallCommand(context.Background(), d); err == nil {
		t.Fatal("runHostAgentInstallCommand outside a town = nil, want an error")
	}
}

// TestRunHostAgentInstallCommand_ReportsAnInstallFailure: a refused install
// ends the command with its error and writes no success line.
func TestRunHostAgentInstallCommand_ReportsAnInstallFailure(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	d := hostAgentDeps{
		findTownRoot: func() (string, error) { return "/towns/live", nil },
		run:          func(context.Context, string) error { return nil },
		install:      func(string) (string, error) { return "", errors.New("refused") },
		out:          &out,
	}
	if err := runHostAgentInstallCommand(context.Background(), d); err == nil {
		t.Fatal("runHostAgentInstallCommand over a refused install = nil, want the error")
	}
	if out.Len() != 0 {
		t.Errorf("a refused install printed a success line:\n%s", out.String())
	}
}

// hostAgentTestGTPath is the binary every install test renders the job with.
const hostAgentTestGTPath = "/usr/local/bin/gt"

// hostAgentTestTempDir stands in for the host's temp directory: no gt path a
// test renders is under it, so the temporary-binary refusal is not the one a
// test meets unless it says so.
const hostAgentTestTempDir = "/var/folders/test/T"

// hostAgentInstallDepsFor is install's deps for a test: the given OS, a fixed
// gt path and the given plist path, always under a temporary directory so no
// test can reach the operator's LaunchAgents.
func hostAgentInstallDepsFor(path, goos string, render func(gtPath, townRoot string) (string, error)) hostAgentInstallDeps {
	return hostAgentInstallDeps{
		goos:      goos,
		gtPath:    func() (string, error) { return hostAgentTestGTPath, nil },
		tempDir:   func() string { return hostAgentTestTempDir },
		plistPath: func() (string, error) { return path, nil },
		render:    render,
		writeFile: os.WriteFile,
	}
}
