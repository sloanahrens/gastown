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

// bootTestTimeout is the per-probe timeout the tests run with. The timeout
// cases wait for it to fire, so it stays short.
const bootTestTimeout = 20 * time.Millisecond

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
			h := newBootHealth(set, bootTestTimeout)
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
	h := newBootHealth([]bootProbe{docker.probe("docker")}, bootTestTimeout)
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

func TestBootHealthProbeErrorPanicAndTimeoutAreNotAnswering(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
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
		{
			name:  "timeout",
			probe: bootProbe{Name: "forgejo", Check: func(context.Context) error { <-release; return nil }},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newBootHealth([]bootProbe{tt.probe, ready.probe("ready")}, bootTestTimeout)
			want := "waiting for " + tt.probe.Name
			for range 2 {
				if got := h.Unhealthy(context.Background()); got != want {
					t.Fatalf("Unhealthy = %q, want %q (never latched)", got, want)
				}
			}
		})
	}
}

func TestBootHealthEmptySetIsHealthy(t *testing.T) {
	t.Parallel()
	h := newBootHealth(nil, bootTestTimeout)
	if got := h.Unhealthy(context.Background()); got != "" {
		t.Fatalf("empty set Unhealthy = %q, want \"\"", got)
	}
}

func TestBootHealthConcurrentCallersAgree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	docker, forgejo := &fakeProbe{}, &fakeProbe{}
	h := newBootHealth([]bootProbe{docker.probe("docker"), forgejo.probe("forgejo")}, bootTestTimeout)
	const callers, rounds = 16, 20
	results := make([][]string, callers)
	var wg sync.WaitGroup
	for c := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds {
				results[c] = append(results[c], h.Unhealthy(ctx))
			}
		}()
	}
	docker.up.Store(true)
	forgejo.up.Store(true)
	wg.Wait()
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
