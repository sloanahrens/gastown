package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// bootTestTimeout is the per-probe timeout for tests whose probes answer at
// once. They never wait on it, so it is long enough that a loaded host cannot
// push a healthy probe past it.
const bootTestTimeout = 10 * time.Second

// bootHungTimeout is the per-probe timeout for the one test that waits for a
// hung probe to time out.
const bootHungTimeout = 20 * time.Millisecond

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

func mustBootHealth(t *testing.T, probes []bootProbe, timeout time.Duration) *bootHealth {
	t.Helper()
	h, err := newBootHealth(probes, timeout)
	if err != nil {
		t.Fatalf("newBootHealth: %v", err)
	}
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
			h := mustBootHealth(t, set, bootTestTimeout)
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
	h := mustBootHealth(t, []bootProbe{docker.probe("docker")}, bootTestTimeout)
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
			h := mustBootHealth(t, []bootProbe{tt.probe, ready.probe("ready")}, bootTestTimeout)
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
	h := mustBootHealth(t, []bootProbe{hung}, bootHungTimeout)
	const rounds = 5
	if got := h.Unhealthy(context.Background()); got != "waiting for forgejo" {
		t.Fatalf("first Unhealthy = %q, want \"waiting for forgejo\"", got)
	}
	// The first run is inside its Check now, and it stays there until release.
	<-started
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
	h := mustBootHealth(t, nil, bootTestTimeout)
	if got := h.Unhealthy(context.Background()); got != "" {
		t.Fatalf("empty set Unhealthy = %q, want \"\"", got)
	}
}

func TestBootHealthConcurrentCallersAgree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	docker, forgejo := &fakeProbe{}, &fakeProbe{}
	h := mustBootHealth(t, []bootProbe{docker.probe("docker"), forgejo.probe("forgejo")}, bootTestTimeout)
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
