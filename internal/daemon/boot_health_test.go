package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// bootTestTimeout is the per-probe timeout the tests give bootHealth. Every
// test runs it on a fake clock, and only advancing that clock fires it, so no
// test waits on this value in real time.
const bootTestTimeout = 20 * time.Millisecond

// bootHangGuard bounds each wait in the hung-probe test. It is a hang guard
// only: the waits end on their event, and a guard that fires means the event
// never came.
const bootHangGuard = 10 * time.Second

// fakeProbe answers while up is set and counts its calls.
type fakeProbe struct {
	up    atomic.Bool
	calls atomic.Int32
}

func (f *fakeProbe) check(context.Context) error {
	f.calls.Add(1)
	if f.up.Load() {
		return nil
	}
	return errors.New("not answering")
}

func (f *fakeProbe) probe(name string) bootProbe {
	return bootProbe{Name: name, Check: f.check}
}

func mustBootHealth(t *testing.T, probes []bootProbe, clk clockwork.Clock) *bootHealth {
	t.Helper()
	h, err := newBootHealth(probes, bootTestTimeout)
	if err != nil {
		t.Fatalf("newBootHealth: %v", err)
	}
	h.clock = clk
	return h
}

func TestBootHealthRejectsNonPositiveTimeout(t *testing.T) {
	t.Parallel()
	for _, timeout := range []time.Duration{0, -time.Millisecond} {
		t.Run(timeout.String(), func(t *testing.T) {
			if _, err := newBootHealth(nil, timeout); err == nil {
				t.Fatalf("newBootHealth(timeout=%v) error = nil, want an error", timeout)
			}
		})
	}
}

func TestBootHealthHoldsUntilEveryProbeAnswers(t *testing.T) {
	t.Parallel()
	type named struct {
		name string
		up   bool
	}
	tests := []struct {
		name   string
		probes []named
		want   string
	}{
		{
			name:   "one failing names it",
			probes: []named{{"forgejo", true}, {"dolt", false}, {"docker", true}},
			want:   "waiting for dolt",
		},
		{
			name:   "two failing name both, sorted",
			probes: []named{{"forgejo", false}, {"dolt", true}, {"docker", false}},
			want:   "waiting for docker, forgejo",
		},
		{
			name:   "all answering is healthy",
			probes: []named{{"forgejo", true}, {"dolt", true}, {"docker", true}},
			want:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var set []bootProbe
			for _, p := range tt.probes {
				f := &fakeProbe{}
				f.up.Store(p.up)
				set = append(set, f.probe(p.name))
			}
			h := mustBootHealth(t, set, clockwork.NewFakeClock())
			if got := h.Unhealthy(context.Background()); got != tt.want {
				t.Fatalf("Unhealthy = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBootHealthLatchesAfterTheFirstHealthyReading(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	docker := &fakeProbe{}
	docker.up.Store(true)
	h := mustBootHealth(t, []bootProbe{docker.probe("docker")}, clockwork.NewFakeClock())
	if got := h.Unhealthy(ctx); got != "" {
		t.Fatalf("first healthy reading = %q, want \"\"", got)
	}
	docker.up.Store(false)
	calls := docker.calls.Load()
	if got := h.Unhealthy(ctx); got != "" {
		t.Fatalf("after outage Unhealthy = %q, want \"\" (latched)", got)
	}
	if got := docker.calls.Load(); got != calls {
		t.Fatalf("probe ran %d more time(s) after latching, want none", got-calls)
	}
}

func TestBootHealthErrorAndPanicAreNotAnswering(t *testing.T) {
	t.Parallel()
	ready := &fakeProbe{}
	ready.up.Store(true)
	tests := []struct {
		name  string
		probe bootProbe
	}{
		{
			name:  "error",
			probe: bootProbe{Name: "dolt", Check: func(context.Context) error { return errors.New("down") }},
		},
		{
			name:  "panic",
			probe: bootProbe{Name: "docker", Check: func(context.Context) error { panic("probe broke") }},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := mustBootHealth(t, []bootProbe{tt.probe, ready.probe("ready")}, clockwork.NewFakeClock())
			want := "waiting for " + tt.probe.Name
			for range 2 {
				if got := h.Unhealthy(context.Background()); got != want {
					t.Fatalf("Unhealthy = %q, want %q (never latched)", got, want)
				}
			}
		})
	}
}

func TestBootHealthHungProbeIsNotRunAgain(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	started := make(chan struct{}, 1)
	var calls atomic.Int32
	hung := bootProbe{Name: "forgejo", Check: func(context.Context) error {
		calls.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return nil
	}}
	clk := clockwork.NewFakeClock()
	h := mustBootHealth(t, []bootProbe{hung}, clk)
	const rounds = 5
	first := make(chan string, 1)
	go func() { first <- h.Unhealthy(context.Background()) }()
	// The first run is inside its Check now, and it stays there until release.
	select {
	case <-started:
	case <-time.After(bootHangGuard): //testpolicy:allow no-sleep — a hang guard only: the probe's start is the event the test waits for, and a probe that never starts fails here rather than hanging the test
		t.Fatal("hung probe never started")
	}
	// The first call waits on the fake clock's timeout, so advance it past.
	guard, cancel := context.WithTimeout(context.Background(), bootHangGuard) //testpolicy:allow no-sleep — a hang guard only: the timer's registration is the event the test waits for, and a call that never waits fails here rather than hanging the test
	defer cancel()
	if err := clk.BlockUntilContext(guard, 1); err != nil {
		t.Fatalf("first Unhealthy never set its timeout: %v", err)
	}
	clk.Advance(bootTestTimeout)
	select {
	case got := <-first:
		if got != "waiting for forgejo" {
			t.Fatalf("first Unhealthy = %q, want \"waiting for forgejo\"", got)
		}
	case <-time.After(bootHangGuard): //testpolicy:allow no-sleep — a hang guard only: the timeout's expiry is the event the call returns on, and a call that never returns fails here rather than hanging the test
		t.Fatal("first Unhealthy never returned after its timeout")
	}
	for range rounds - 1 {
		if got := h.Unhealthy(context.Background()); got != "waiting for forgejo" {
			t.Fatalf("Unhealthy = %q, want \"waiting for forgejo\"", got)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("hung probe ran %d times over %d calls, want 1", got, rounds)
	}
}

func TestBootHealthEmptySetIsHealthy(t *testing.T) {
	t.Parallel()
	h := mustBootHealth(t, nil, clockwork.NewFakeClock())
	if got := h.Unhealthy(context.Background()); got != "" {
		t.Fatalf("empty set Unhealthy = %q, want \"\"", got)
	}
}

func TestBootHealthConcurrentCallersAgree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	docker, forgejo := &fakeProbe{}, &fakeProbe{}
	h := mustBootHealth(t, []bootProbe{docker.probe("docker"), forgejo.probe("forgejo")}, clockwork.NewFakeClock())
	// Both probes are down, so this is the waiting side of the transition, and
	// it happens before any caller starts.
	if got := h.Unhealthy(ctx); got != "waiting for docker, forgejo" {
		t.Fatalf("first Unhealthy = %q, want the waiting reading", got)
	}
	const callers, rounds = 16, 20
	results := make([][]string, callers)
	var wg sync.WaitGroup
	for c := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range rounds {
				if c == 0 && r == rounds/2 {
					// Caller 0 brings the probes up while the others are still
					// calling, so its next reading is the healthy side.
					docker.up.Store(true)
					forgejo.up.Store(true)
				}
				results[c] = append(results[c], h.Unhealthy(ctx))
			}
		}()
	}
	wg.Wait()
	if got := results[0][rounds/2]; got != "" {
		t.Fatalf("caller 0 reading after the probes came up = %q, want \"\"", got)
	}
	for c, seq := range results {
		healthy := false
		for i, got := range seq {
			switch {
			case got == "":
				healthy = true
			case healthy:
				t.Fatalf("caller %d call %d: %q after a healthy reading", c, i, got)
			case !strings.HasPrefix(got, "waiting for "):
				t.Fatalf("caller %d call %d: %q, want waiting or healthy", c, i, got)
			}
		}
	}
	if got := h.Unhealthy(ctx); got != "" {
		t.Fatalf("final Unhealthy = %q, want \"\"", got)
	}
}
