package cmd

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/hostprobe"
	"github.com/steveyegge/gastown/internal/templates"
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
	town := t.TempDir()
	read := func(string) templates.SupervisorState { return templates.SupervisorState{Loaded: true} }
	writes := make(chan hostprobe.Probes, 16)
	loop := &hostAgentLoop{
		clock:    clock,
		interval: hostAgentInterval,
		path:     "/town/.runtime/host/probes.json",
		collect: func(ctx context.Context, now time.Time) hostprobe.Probes {
			return hostAgentCollect(ctx, run, town, now, dockerDisk, read)
		},
		write: func(_ string, p hostprobe.Probes) error {
			writes <- p
			return nil
		},
		logf: func(string, ...any) {},
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
	// The first finished read is published by a later tick; the bound is in ticks,
	// not wall time, and each tick waits for its own write.
	for range 10 {
		awaitNextTick(t, clock)
		clock.Advance(hostAgentInterval)
		got := awaitProbes(t, writes, done)
		if got.DockerDisk.Err == "" && got.DockerDisk.Value.UsedPct == 72 {
			cancel()
			if err := awaitExit(t, done); err != nil {
				t.Errorf("loop after cancel = %v, want nil", err)
			}
			return
		}
	}
	t.Fatal("no tick published the finished docker reading within 10 ticks")
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
		collect: func(ctx context.Context, now time.Time) hostprobe.Probes {
			return hostAgentCollect(ctx, run, town, now, dockerDisk, read)
		},
		write: func(_ string, p hostprobe.Probes) error {
			writes <- p
			return nil
		},
		logf: func(string, ...any) {},
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
