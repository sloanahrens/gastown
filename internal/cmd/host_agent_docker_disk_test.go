package cmd

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/hostprobe"
	"github.com/steveyegge/gastown/internal/templates"
	"github.com/steveyegge/gastown/internal/util"
)

// TestHostAgentFillsDockerDisk pins what the agent publishes for the Docker
// Desktop disk: the percentage in the field when df answers, and an error with
// no value when the read fails or its output names no capacity. A zero value
// with no error would read as a disk at 0% used.
func TestHostAgentFillsDockerDisk(t *testing.T) {
	t.Parallel()
	const dfOut = "Filesystem 1024-blocks Used Available Capacity Mounted on\n" +
		"/dev/vdb 1000000 724000 276000 72% /\n"
	cases := []struct {
		name    string
		out     string
		err     error
		ended   bool // the read's deadline has ended before the runner starts
		want    hostprobe.Disk
		wantErr bool
	}{
		{name: "df answers", out: dfOut, want: hostprobe.Disk{UsedPct: 72}},
		// A failed command can still print a df: the error, not the output,
		// decides whether the reading happened.
		{name: "the runner fails after printing a df", out: dfOut, err: errors.New("exit status 1"), wantErr: true},
		{name: "the output names no capacity", out: "not df output\n", wantErr: true},
		// A docker killed at its deadline returns the context's error, and that
		// error, not the df it may have printed, is the reading.
		{name: "the read's deadline ends it", out: dfOut, ended: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ran [][]string
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.ended {
				cancel()
			}
			run := func(ctx context.Context, argv []string) ([]byte, error) {
				ran = append(ran, argv)
				if err := ctx.Err(); err != nil {
					return []byte(tc.out), err
				}
				return []byte(tc.out), tc.err
			}

			got := hostAgentWithDockerDisk(ctx, hostprobe.Probes{V: hostprobe.SchemaVersion}, run)

			wantArgv := [][]string{{"docker", "exec", "gt-dolt", "df", "-P", "/"}}
			if !slices.EqualFunc(ran, wantArgv, func(a, b []string) bool { return slices.Equal(a, b) }) {
				t.Errorf("runner ran %q, want %q", ran, wantArgv)
			}
			if tc.wantErr {
				if got.DockerDisk.Err == "" {
					t.Fatalf("a failed read published %+v, want an error in the field", got.DockerDisk)
				}
				if got.DockerDisk.Value != (hostprobe.Disk{}) {
					t.Errorf("a failed read left Value = %+v, want it zero", got.DockerDisk.Value)
				}
				return
			}
			if got.DockerDisk.Err != "" || got.DockerDisk.Value != tc.want {
				t.Errorf("published %+v, want %+v with no error", got.DockerDisk, tc.want)
			}
		})
	}
}

// TestHostAgentDockerDiskDoesNotHoldTheWriteCycle pins that a docker exec still
// running does not hold the tick: the write goes out at once with the unknown
// reading, a second read does not start while the first is held, and the
// finished reading reaches a later tick.
func TestHostAgentDockerDiskDoesNotHoldTheWriteCycle(t *testing.T) {
	t.Parallel()
	const dfOut = "Filesystem 1024-blocks Used Available Capacity Mounted on\n" +
		"/dev/vdb 1000000 724000 276000 72% /\n"
	clock := clockwork.NewFakeClockAt(hostAgentEpoch)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHeld := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseHeld()
	started := make(chan struct{})
	var startOnce sync.Once
	// stored is closed by the agent's own hook after the held read's result is
	// stored, so the test waits for the store itself rather than hoping a tick
	// lands after it under whatever load the machine is under.
	stored := make(chan struct{})
	var storedOnce sync.Once
	var calls atomic.Int32
	run := func(ctx context.Context, argv []string) ([]byte, error) {
		if !slices.Contains(argv, "exec") {
			return nil, errors.New("not under test")
		}
		calls.Add(1)
		startOnce.Do(func() { close(started) })
		select {
		case <-release:
			return []byte(dfOut), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	dockerDisk := newHostAgentDockerDisk()
	dockerDisk.onStored = func() { storedOnce.Do(func() { close(stored) }) }
	town := t.TempDir()
	read := func(string) templates.SupervisorState { return templates.SupervisorState{Loaded: true} }
	writes := make(chan hostprobe.Probes, 16)
	loop := &hostAgentLoop{
		clock:    clock,
		interval: hostAgentInterval,
		path:     "/town/.runtime/host/probes.json",
		disk:     newFakeDiskReader().read,
		write: func(_ string, p hostprobe.Probes) error {
			writes <- p
			return nil
		},
		logf: func(string, ...any) {},
	}
	loop.collect = func(ctx context.Context, now time.Time) hostprobe.Probes {
		return hostAgentCollect(ctx, run, town, now, dockerDisk, read, loop.disk)
	}
	done := startHostAgentLoop(loop, ctx)

	first := awaitProbes(t, writes, done)
	if first.DockerDisk.Err != hostprobe.DockerDiskNotCollected || first.DockerDisk.Value != (hostprobe.Disk{}) {
		t.Errorf("tick 1 published %+v while docker is held, want the not-collected reading", first.DockerDisk)
	}

	// The first read starts in the background; wait for it, bounded, before counting.
	bound, stop := context.WithTimeout(context.Background(), hostAgentWait)
	defer stop()
	select {
	case <-started:
	case <-bound.Done():
		t.Fatal("docker exec did not start within the test's bound")
	}

	awaitNextTick(t, clock)
	clock.Advance(hostAgentInterval)
	awaitProbes(t, writes, done)
	// The first read has not returned, so no second docker exec can have started.
	if got := calls.Load(); got != 1 {
		t.Errorf("the write cycle started %d docker execs while the first was held, want 1", got)
	}

	releaseHeld()
	// Wait for the held read's result to be stored, bounded: the tick that
	// publishes it must come after the store, and a tick count would only sample
	// the scheduler's luck.
	select {
	case <-stored:
	case <-bound.Done():
		t.Fatal("the held docker read's result was not stored within the test's bound")
	}
	awaitNextTick(t, clock)
	clock.Advance(hostAgentInterval)
	got := awaitProbes(t, writes, done)
	if got.DockerDisk.Err != "" || got.DockerDisk.Value.UsedPct != 72 {
		t.Fatalf("the tick after the read finished published %+v, want the finished reading", got.DockerDisk)
	}
	cancel()
	if err := awaitExit(t, done); err != nil {
		t.Errorf("loop after cancel = %v, want nil", err)
	}
}

// TestHostAgentDockerDiskStuckReadPublishesAnError pins that a docker exec which
// never returns, not even at its context's deadline, is not published as the
// reading it never produced: once a tick finds the read past its deadline, the
// field carries an error, and no second docker exec starts behind it.
func TestHostAgentDockerDiskStuckReadPublishesAnError(t *testing.T) {
	t.Parallel()
	clock := clockwork.NewFakeClockAt(hostAgentEpoch)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A docker that ignores its context: the read returns only when the test ends.
	never := make(chan struct{})
	defer close(never)
	var calls atomic.Int32
	started := make(chan struct{})
	var startOnce sync.Once
	run := func(_ context.Context, argv []string) ([]byte, error) {
		if !slices.Contains(argv, "exec") {
			return nil, errors.New("not under test")
		}
		calls.Add(1)
		startOnce.Do(func() { close(started) })
		<-never
		return nil, errors.New("released")
	}

	dockerDisk := newHostAgentDockerDisk()
	town := t.TempDir()
	read := func(string) templates.SupervisorState { return templates.SupervisorState{Loaded: true} }
	writes := make(chan hostprobe.Probes, 16)
	loop := &hostAgentLoop{
		clock:    clock,
		interval: hostAgentInterval,
		path:     "/town/.runtime/host/probes.json",
		disk:     newFakeDiskReader().read,
		write: func(_ string, p hostprobe.Probes) error {
			writes <- p
			return nil
		},
		logf: func(string, ...any) {},
	}
	loop.collect = func(ctx context.Context, now time.Time) hostprobe.Probes {
		return hostAgentCollect(ctx, run, town, now, dockerDisk, read, loop.disk)
	}
	done := startHostAgentLoop(loop, ctx)

	first := awaitProbes(t, writes, done)
	if first.DockerDisk.Err != hostprobe.DockerDiskNotCollected {
		t.Fatalf("tick 1 published %+v, want the not-collected reading", first.DockerDisk)
	}
	bound, stop := context.WithTimeout(context.Background(), hostAgentWait)
	defer stop()
	select {
	case <-started:
	case <-bound.Done():
		t.Fatal("docker exec did not start within the test's bound")
	}

	awaitNextTick(t, clock)
	clock.Advance(hostAgentInterval)
	second := awaitProbes(t, writes, done)
	if second.DockerDisk.Err == "" || second.DockerDisk.Err == hostprobe.DockerDiskNotCollected {
		t.Errorf("tick 2 published %+v with the read still held past its deadline, want an error that names the held read", second.DockerDisk)
	}
	if second.DockerDisk.Value != (hostprobe.Disk{}) {
		t.Errorf("tick 2 left Value = %+v, want it zero", second.DockerDisk.Value)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("the write cycle started %d docker execs behind a held read, want 1", got)
	}

	cancel()
	if err := awaitExit(t, done); err != nil {
		t.Errorf("loop after cancel = %v, want nil", err)
	}
}

// awaitProbes returns the next reading the loop wrote, failing the test if none
// arrives before hostAgentWait or if the loop stops first.
func awaitProbes(t *testing.T, writes <-chan hostprobe.Probes, done <-chan error) hostprobe.Probes {
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
	return hostprobe.Probes{}
}

// fakeDiskReader is the town volume's disk read a test gives the agent: a fixed
// reading, and the paths it was asked, so the agent never reaches
// util.GetDiskSpace and no test starts diskutil (gt-yjzepk.15).
type fakeDiskReader struct {
	info  *util.DiskSpaceInfo
	paths []string
}

func newFakeDiskReader() *fakeDiskReader {
	return &fakeDiskReader{info: &util.DiskSpaceInfo{AvailableBytes: 1 << 30, UsedPercent: 42}}
}

func (f *fakeDiskReader) read(path string) (*util.DiskSpaceInfo, error) {
	f.paths = append(f.paths, path)
	return f.info, nil
}

// TestHostAgentLoopDefaultsToTheRealDiskReader pins the disk reader a loop's
// tick reads the town volume with: a loop with no reader set leaves the read to
// production, and a loop given a fake reads through the fake and never the
// production reader.
//
// The loop with no reader is given a town root that does not exist, so the
// production reader fails there before the APFS branch that runs diskutil: this
// test exercises the real reader without starting an external tool, on macOS as
// on Linux (gt-yjzepk.15). It asserts that the read's failure is published, not
// how the operating system words it.
func TestHostAgentLoopDefaultsToTheRealDiskReader(t *testing.T) {
	t.Parallel()
	missingTown := filepath.Join(t.TempDir(), "no-such-town")

	// No reader set: the read of the missing root fails, and that failure is
	// the published reading rather than an empty disk that reads as healthy.
	got := firstHostAgentReading(t, missingTown, nil)
	if got.Disk.Err == "" {
		t.Errorf("a loop with no disk reader published Disk = %+v, want the failed read's error", got.Disk)
	}
	if got.Disk.Value != (hostprobe.Disk{}) {
		t.Errorf("a failed disk read left Value = %+v, want it zero", got.Disk.Value)
	}

	// A fake reader: the loop publishes its reading, so the production reader
	// that would fail on the missing root was never the one read.
	fake := newFakeDiskReader()
	got = firstHostAgentReading(t, missingTown, fake.read)
	if want := (hostprobe.Disk{UsedPct: 42, FreeGB: 1}); got.Disk.Err != "" || got.Disk.Value != want {
		t.Errorf("a loop with a fake disk reader published %+v, want %+v with no error", got.Disk, want)
	}
	if want := []string{missingTown}; !slices.Equal(fake.paths, want) {
		t.Errorf("the fake disk reader was asked for %q, want %q", fake.paths, want)
	}
}

// firstHostAgentReading runs one tick of a loop wired the way production wires
// it, with the test's runner, launchd reader and write and the given disk
// reader, and returns the reading it wrote.
func firstHostAgentReading(t *testing.T, townRoot string, disk hostprobe.DiskReader) hostprobe.Probes {
	t.Helper()
	clock := clockwork.NewFakeClockAt(hostAgentEpoch)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := func(context.Context, []string) ([]byte, error) { return nil, errors.New("not under test") }
	read := func(string) templates.SupervisorState { return templates.SupervisorState{Loaded: true} }
	writes := make(chan hostprobe.Probes, 1)
	loop := &hostAgentLoop{
		clock:    clock,
		interval: hostAgentInterval,
		path:     "/town/.runtime/host/probes.json",
		disk:     disk,
		write: func(_ string, p hostprobe.Probes) error {
			writes <- p
			return nil
		},
		logf: func(string, ...any) {},
	}
	loop.collect = func(ctx context.Context, now time.Time) hostprobe.Probes {
		return hostAgentCollect(ctx, run, townRoot, now, newHostAgentDockerDisk(), read, loop.disk)
	}
	done := startHostAgentLoop(loop, ctx)

	first := awaitProbes(t, writes, done)
	cancel()
	if err := awaitExit(t, done); err != nil {
		t.Errorf("loop after cancel = %v, want nil", err)
	}
	return first
}
