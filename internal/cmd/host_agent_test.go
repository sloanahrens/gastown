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
	"sort"
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

// hostAgentWait bounds every wait these tests make on the loop. A loop that
// neither writes nor returns within it fails the test, so a stuck loop cannot
// pass by running until go test's own timeout ends the package's run.
const hostAgentWait = 10 * time.Second

// startHostAgentLoop runs l in the background and returns the channel its exit
// is reported on.
func startHostAgentLoop(l *hostAgentLoop, ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() { done <- l.run(ctx) }()
	return done
}

// awaitWrite returns the next write the loop made, failing the test if none
// arrives before hostAgentWait or if the loop stops first.
func awaitWrite(t *testing.T, writes <-chan time.Time, done <-chan error) time.Time {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), hostAgentWait)
	defer cancel()
	select {
	case got := <-writes:
		return got
	case err := <-done:
		t.Fatalf("loop stopped before its next write: %v", err)
	case <-ctx.Done():
		t.Fatal("no write within the test's bound")
	}
	return time.Time{}
}

// awaitExit returns the loop's exit, failing the test if the loop is still
// running when hostAgentWait ends.
func awaitExit(t *testing.T, done <-chan error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), hostAgentWait)
	defer cancel()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		t.Fatal("the loop was still running at the test's bound")
	}
	return nil
}

// awaitNextTick advances nothing and returns once the loop has registered its
// wait on the clock. Advancing before that lands the advance before the wait,
// and the tick then arrives a whole interval late; a loop that stopped instead
// fails the test here once hostAgentWait ends.
func awaitNextTick(t *testing.T, clock *clockwork.FakeClock) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), hostAgentWait)
	defer cancel()
	if err := clock.BlockUntilContext(ctx, 1); err != nil {
		t.Fatalf("the loop did not wait for its next tick within the test's bound: %v", err)
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
		if got := awaitWrite(t, writes, done); !got.Equal(want) {
			t.Errorf("write %d stamped %s, want %s", i+1, got, want)
		}
		if i < 2 {
			awaitNextTick(t, clock)
			clock.Advance(hostAgentInterval)
		}
	}

	cancel()
	if err := awaitExit(t, done); err != nil {
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

	awaitWrite(t, writes, done)
	// Cancel while the loop waits, so the test pins the wait's exit rather than
	// racing a write that is already running.
	awaitNextTick(t, clock)
	cancel()
	if err := awaitExit(t, done); err != nil {
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

	awaitWrite(t, writes, done)
	awaitNextTick(t, clock)
	clock.Advance(hostAgentInterval)

	if err := awaitExit(t, done); err != nil {
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
	awaitNextTick(t, clock)
	clock.Advance(hostAgentInterval)

	if got, want := awaitWrite(t, writes, done), hostAgentEpoch.Add(hostAgentInterval); !got.Equal(want) {
		t.Errorf("second tick stamped %s, want %s", got, want)
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

// TestInstallHostAgent_WritesThePlist checks the files install leaves behind:
// the renders for the town it was given, at the paths it returned, with the
// probe directory launchd's log files need.
func TestInstallHostAgent_WritesThePlist(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	dir := t.TempDir()
	path := filepath.Join(dir, "com.gastown.host-agent.plist")
	got, err := installHostAgent(town, hostAgentInstallDepsFor(path, "darwin", templates.HostAgentPlist))
	if err != nil {
		t.Fatalf("installHostAgent: %v", err)
	}
	if len(got) == 0 || got[0] != path {
		t.Fatalf("installHostAgent = %v, want %q first", got, path)
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
	for i, job := range templates.MacMaintJobs() {
		maintPath := filepath.Join(dir, job.PlistName())
		if i+1 >= len(got) || got[i+1] != maintPath {
			t.Errorf("installHostAgent = %v, want %q at index %d", got, maintPath, i+1)
		}
		if _, err := os.Stat(maintPath); err != nil {
			t.Errorf("maint job plist %s after install: %v", maintPath, err)
		}
	}

	// launchd opens the plists' StandardOutPath before the program runs and
	// fails the job with EX_CONFIG if its directory is missing.
	if _, err := os.Stat(filepath.Dir(hostprobe.Path(town))); err != nil {
		t.Errorf("probe/log directory after install: %v", err)
	}
}

// TestHostAgentInstallWritesBothJobs is the install's contract after the
// daily maint job was added: beside the agent's plist, one plist is written
// for every row of the job table, and install names every file it wrote.
func TestHostAgentInstallWritesBothJobs(t *testing.T) {
	t.Parallel()
	town, dir := t.TempDir(), t.TempDir()
	agentPath := filepath.Join(dir, templates.HostAgentLabel+".plist")
	paths, err := installHostAgent(town, hostAgentInstallDepsFor(agentPath, "darwin", templates.HostAgentPlist))
	if err != nil {
		t.Fatalf("installHostAgent: %v", err)
	}
	jobs := templates.MacMaintJobs()
	if len(jobs) == 0 {
		t.Fatal("the maint job table has no rows")
	}
	if len(paths) != 1+len(jobs) {
		t.Fatalf("installHostAgent reported %v, want the agent plist and %d maint job plists", paths, len(jobs))
	}
	if paths[0] != agentPath {
		t.Errorf("installHostAgent reported %q first, want the agent plist %q", paths[0], agentPath)
	}
	for i, job := range jobs {
		path := filepath.Join(dir, job.PlistName())
		if paths[i+1] != path {
			t.Errorf("installHostAgent reported %q at index %d, want %q", paths[i+1], i+1, path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading the maint job plist %s: %v", path, err)
		}
		want, err := templates.MacMaintJobPlist(hostAgentTestGTPath, town, job)
		if err != nil {
			t.Fatalf("rendering the expected maint job plist: %v", err)
		}
		if string(data) != want {
			t.Errorf("installed maint job plist:\n%s\nwant:\n%s", data, want)
		}
	}
}

// TestHostAgentInstallIsIdempotentForBothJobs checks the two install rules the
// daily job shares with the agent's: an unchanged file is left alone on a
// second run, and the temporary-binary refusal stops the whole install, so no
// job is written from a binary that will vanish.
func TestHostAgentInstallIsIdempotentForBothJobs(t *testing.T) {
	t.Parallel()

	t.Run("a second install with unchanged content writes nothing", func(t *testing.T) {
		t.Parallel()
		town, dir := t.TempDir(), t.TempDir()
		agentPath := filepath.Join(dir, templates.HostAgentLabel+".plist")
		d := hostAgentInstallDepsFor(agentPath, "darwin", templates.HostAgentPlist)
		realWrite := d.writeFile
		writes := 0
		d.writeFile = func(p string, data []byte, perm os.FileMode) error {
			writes++
			return realWrite(p, data, perm)
		}
		first, err := installHostAgent(town, d)
		if err != nil {
			t.Fatalf("the first installHostAgent: %v", err)
		}
		wantFirst := 1 + len(templates.MacMaintJobs())
		if writes != wantFirst {
			t.Fatalf("the first install wrote %d files, want %d", writes, wantFirst)
		}
		writes = 0
		second, err := installHostAgent(town, d)
		if err != nil {
			t.Fatalf("the second installHostAgent: %v", err)
		}
		if writes != 0 {
			t.Errorf("the second install wrote %d files, want none with unchanged content", writes)
		}
		if strings.Join(first, "\n") != strings.Join(second, "\n") {
			t.Errorf("the two installs reported different files: %v then %v", first, second)
		}
	})

	t.Run("a temporary binary is refused for both", func(t *testing.T) {
		t.Parallel()
		town, dir := t.TempDir(), t.TempDir()
		agentPath := filepath.Join(dir, templates.HostAgentLabel+".plist")
		d := hostAgentInstallDepsFor(agentPath, "darwin", templates.HostAgentPlist)
		d.gtPath = func() (string, error) { return hostAgentTestTempDir + "/go-build1/b001/exe/gt", nil }
		if _, err := installHostAgent(town, d); err == nil {
			t.Fatal("installHostAgent with a temporary gt = nil, want a refusal")
		}
		if names := hostAgentDirNames(t, dir); len(names) != 0 {
			t.Errorf("a refused install wrote %v, want no job at all", names)
		}
	})
}

// TestInstallHostAgent_ReplacesItsOwnTownsPlist is the reinstall: a file that
// already serves this town is accepted, not refused.
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

// TestInstallHostAgent_RefusesAPlistForAnotherTown covers the mismatch: a
// label names one job on the host, so an agent or maint plist serving another
// town is refused and left untouched rather than silently repointed.
func TestInstallHostAgent_RefusesAPlistForAnotherTown(t *testing.T) {
	t.Parallel()
	live, scratch := t.TempDir(), t.TempDir()
	for _, tc := range []struct {
		name string
		file func(t *testing.T, dir string) (string, string)
	}{
		{
			name: "the agent plist",
			file: func(t *testing.T, dir string) (string, string) {
				t.Helper()
				content, err := templates.HostAgentPlist(hostAgentTestGTPath, live)
				if err != nil {
					t.Fatalf("rendering the existing plist: %v", err)
				}
				return filepath.Join(dir, templates.HostAgentLabel+".plist"), content
			},
		},
		{
			name: "a maint job plist",
			file: func(t *testing.T, dir string) (string, string) {
				t.Helper()
				job := templates.MacMaintJobs()[0]
				content, err := templates.MacMaintJobPlist(hostAgentTestGTPath, live, job)
				if err != nil {
					t.Fatalf("rendering the existing maint plist: %v", err)
				}
				return filepath.Join(dir, job.PlistName()), content
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			agentPath := filepath.Join(dir, templates.HostAgentLabel+".plist")
			foreignPath, existing := tc.file(t, dir)
			if err := os.WriteFile(foreignPath, []byte(existing), 0o644); err != nil {
				t.Fatalf("writing the existing plist: %v", err)
			}

			if _, err := installHostAgent(scratch, hostAgentInstallDepsFor(agentPath, "darwin", templates.HostAgentPlist)); err == nil {
				t.Fatal("installHostAgent over another town's plist = nil, want a refusal")
			}
			data, err := os.ReadFile(foreignPath)
			if err != nil {
				t.Fatalf("reading the untouched plist: %v", err)
			}
			if string(data) != existing {
				t.Errorf("the refused install rewrote another town's plist:\n%s", data)
			}
			// In the agent-plist case agentPath is the foreign file the check
			// above already compared; otherwise a refused install writes nothing.
			if _, err := os.Stat(agentPath); err == nil && agentPath != foreignPath {
				t.Errorf("a refused install wrote the agent plist %s", agentPath)
			}
		})
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

// TestInstallHostAgent_RefusesAnExistingPlistItCannotRead covers a plist that
// exists and serves this town, so the free-for check passes and the read is the
// only thing that fails: install cannot tell what the file holds, so it refuses
// rather than overwrite it, and leaves the file as it was.
func TestInstallHostAgent_RefusesAnExistingPlistItCannotRead(t *testing.T) {
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
	boom := errors.New("boom")
	d := hostAgentInstallDepsFor(path, "darwin", templates.HostAgentPlist)
	d.readFile = func(string) ([]byte, error) { return nil, boom }

	if _, err := installHostAgent(town, d); !errors.Is(err, boom) {
		t.Fatalf("installHostAgent over an unreadable plist = %v, want the read error", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the untouched plist: %v", err)
	}
	if string(data) != existing {
		t.Errorf("a refused install rewrote the plist:\n%s", data)
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
			name:     "agent render fails",
			townRoot: func(t *testing.T) string { return t.TempDir() },
			broken:   func(d *hostAgentInstallDeps) { d.render = func(string, string) (string, error) { return "", boom } },
		},
		{
			name:     "maint render fails",
			townRoot: func(t *testing.T) string { return t.TempDir() },
			broken: func(d *hostAgentInstallDeps) {
				d.renderMaint = func(string, string, templates.MacMaintJob) (string, error) { return "", boom }
			},
		},
		{
			// install cannot tell what an unreadable file holds, so it reports
			// the read instead of overwriting a job it cannot inspect.
			name:     "an existing plist cannot be read",
			townRoot: func(t *testing.T) string { return t.TempDir() },
			broken:   func(d *hostAgentInstallDeps) { d.readFile = func(string) ([]byte, error) { return nil, boom } },
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
// reported, not swallowed into a success, whether it fails on the agent's
// plist or on a maint job's. The write is the dep, so the failure is the
// test's own rather than one that depends on the mode of a file and whether
// the test runs as root.
func TestInstallHostAgent_ReportsAPlistItCannotWrite(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		failOn int
	}{
		{name: "the agent plist", failOn: 0},
		{name: "a maint job plist", failOn: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			path := filepath.Join(t.TempDir(), "com.gastown.host-agent.plist")
			d := hostAgentInstallDepsFor(path, "darwin", templates.HostAgentPlist)
			realWrite := d.writeFile
			calls := 0
			d.writeFile = func(p string, data []byte, perm os.FileMode) error {
				if calls == tc.failOn {
					return errors.New("read-only file system")
				}
				calls++
				return realWrite(p, data, perm)
			}
			if _, err := installHostAgent(town, d); err == nil {
				t.Fatal("installHostAgent with a failing write = nil, want the error")
			}
		})
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

// TestInstallHostAgent_PublishesThePlistByReplacement: a reinstall whose
// content changed reaches the plist as a new file rather than a rewrite of the
// one launchd may be reading, and it leaves no temporary beside it. The write
// is production's own, so this is also the test that goes red if install is
// wired back to a plain write.
func TestInstallHostAgent_PublishesThePlistByReplacement(t *testing.T) {
	t.Parallel()
	town, dir := t.TempDir(), t.TempDir()
	path := filepath.Join(dir, "com.gastown.host-agent.plist")
	var first, second os.FileInfo
	for i, gtPath := range []string{"/usr/local/bin/gt", "/opt/gt/bin/gt"} {
		// A different binary changes every job's content without changing the
		// town, so the reinstall is not the refused case.
		d := hostAgentInstallDepsFor(path, "darwin", templates.HostAgentPlist)
		d.gtPath = func() (string, error) { return gtPath, nil }
		if _, err := installHostAgent(town, d); err != nil {
			t.Fatalf("install %d: %v", i+1, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat after install %d: %v", i+1, err)
		}
		if i == 0 {
			first = info
		} else {
			second = info
		}
	}
	if os.SameFile(first, second) {
		t.Error("the second install rewrote the plist in place, so a reader during it could have seen a partial job")
	}
	want := []string{filepath.Base(path)}
	for _, job := range templates.MacMaintJobs() {
		want = append(want, job.PlistName())
	}
	sort.Strings(want)
	if names := hostAgentDirNames(t, dir); strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("directory after two installs holds %v, want only the jobs %v", names, want)
	}
}

// hostAgentDirNames lists a directory's entries, sorted by name.
func hostAgentDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
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
		install: func(string) ([]string, error) {
			t.Error("install ran for a run command")
			return nil, nil
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
		install: func(string) ([]string, error) { return nil, nil },
		out:     io.Discard,
	}
	if err := runHostAgentCommand(context.Background(), d); err == nil {
		t.Fatal("runHostAgentCommand outside a town = nil, want an error")
	}
}

// TestRunHostAgentInstallCommand_ReportsTheWrittenPlist checks what the
// operator sees: every path install returned and the command that loads each
// of them.
func TestRunHostAgentInstallCommand_ReportsTheWrittenPlist(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	const path = "/home/user/Library/LaunchAgents/com.gastown.host-agent.plist"
	const maintPath = "/home/user/Library/LaunchAgents/com.gastown.host-maint-nodes.plist"
	d := hostAgentDeps{
		findTownRoot: func() (string, error) { return "/towns/live", nil },
		run:          func(context.Context, string) error { return nil },
		install:      func(townRoot string) ([]string, error) { return []string{path, maintPath}, nil },
		out:          &out,
	}
	if err := runHostAgentInstallCommand(context.Background(), d); err != nil {
		t.Fatalf("runHostAgentInstallCommand: %v", err)
	}
	for _, want := range []string{path, maintPath} {
		if !strings.Contains(out.String(), "wrote "+want+"\n") {
			t.Errorf("install output does not report writing %s:\n%s", want, out.String())
		}
		bootstrap := fmt.Sprintf("launchctl bootstrap gui/%d %s", os.Getuid(), want)
		if !strings.Contains(out.String(), bootstrap) {
			t.Errorf("install output does not tell the operator to run %q:\n%s", bootstrap, out.String())
		}
	}
}

// TestRunHostAgentInstallCommand_RefusesOutsideATown: a plist names a town to
// write for, so install has nothing to render without one.
func TestRunHostAgentInstallCommand_RefusesOutsideATown(t *testing.T) {
	t.Parallel()
	d := hostAgentDeps{
		findTownRoot: func() (string, error) { return "", errors.New("no town here") },
		run:          func(context.Context, string) error { return nil },
		install: func(string) ([]string, error) {
			t.Error("install ran with no town root")
			return nil, nil
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
		install:      func(string) ([]string, error) { return nil, errors.New("refused") },
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
// test can reach the operator's LaunchAgents. The write stays production's, so
// the tests that install exercise the writer install really uses.
func hostAgentInstallDepsFor(path, goos string, render func(gtPath, townRoot string) (string, error)) hostAgentInstallDeps {
	d := realHostAgentInstallDeps()
	d.goos = goos
	d.gtPath = func() (string, error) { return hostAgentTestGTPath, nil }
	d.tempDir = func() string { return hostAgentTestTempDir }
	d.plistPath = func() (string, error) { return path, nil }
	d.render = render
	return d
}

// The temp-binary guard compares paths with their symlinks resolved, because
// macOS's temp directory is reached by two names (/var and /private/var): a gt
// under the temp directory is refused whichever name either path arrives by,
// and one outside it is not.
func TestHostAgentRefuseATemporaryBinaryResolvesSymlinks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	gt := filepath.Join(real, "gt")
	if err := os.WriteFile(gt, []byte("a stand-in for the gt binary\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "gt")
	if err := os.WriteFile(elsewhere, []byte("a stand-in for the gt binary\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		gtPath  string
		tempDir string
		refused bool
	}{
		{"the same spelling", gt, real, true},
		{"the binary by the real name, the temp dir by the link", gt, link, true},
		{"the binary by the link, the temp dir by the real name", filepath.Join(link, "gt"), real, true},
		{"a binary outside the temp dir", elsewhere, real, false},
		{"a temp dir that cannot be resolved still compares by spelling", filepath.Join(dir, "gone", "gt"), filepath.Join(dir, "gone"), true},
	} {
		err := hostAgentRefuseATemporaryBinary(tc.gtPath, tc.tempDir)
		if (err != nil) != tc.refused {
			t.Errorf("%s: hostAgentRefuseATemporaryBinary(%q, %q) = %v, want refused=%v", tc.name, tc.gtPath, tc.tempDir, err, tc.refused)
		}
	}
}

// TestHostCommandHasNoLoadSubcommand pins that `gt host` names only commands
// that exist: the load subcommand is gone, so neither the command tree nor the
// help text may offer it.
func TestHostCommandHasNoLoadSubcommand(t *testing.T) {
	t.Parallel()
	for _, c := range hostCmd.Commands() {
		if c.Name() == "load" {
			t.Errorf("gt host has a child named %q; want none", c.Name())
		}
	}
	if strings.Contains(hostCmd.Long, "host load") {
		t.Errorf("gt host help names %q, which no command provides", "host load")
	}
}
