package slot

import (
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// TestNewGate_Defaults pins what the package-level functions run on: the
// swappable default runtime, DefaultPollInterval, and options that override
// only what they name.
func TestNewGate_Defaults(t *testing.T) {
	t.Parallel()
	g := NewGate()
	if _, ok := g.runtime.(legacyRuntime); !ok {
		t.Errorf("default runtime = %T, want the package default (legacyRuntime)", g.runtime)
	}
	if g.pollInterval != DefaultPollInterval {
		t.Errorf("default poll interval = %s, want %s", g.pollInterval, DefaultPollInterval)
	}
	if _, ok := g.env.(osEnv); !ok {
		t.Errorf("default env = %T, want the process environment", g.env)
	}

	rt := &fakeRuntime{}
	clk := clockwork.NewFakeClockAt(testNow)
	g = NewGate(WithRuntime(rt), WithClock(clk), WithPollInterval(250*time.Millisecond))
	if g.runtime != rt || g.clock != clk || g.pollInterval != 250*time.Millisecond {
		t.Errorf("options not applied: runtime=%T clock=%T poll=%s", g.runtime, g.clock, g.pollInterval)
	}
	if g = NewGate(WithPollInterval(0)); g.pollInterval != DefaultPollInterval {
		t.Errorf("WithPollInterval(0) set %s, want the default kept", g.pollInterval)
	}
}

// TestReport_Busy: a single-slot report (no Total) is busy when held; a pool
// report only when every slot is held; either is busy on an unwrapped suite
// or an unverifiable docker check.
func TestReport_Busy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		rep  Report
		want bool
	}{
		{"free single slot", Report{}, false},
		{"held single slot", Report{Held: true}, true},
		{"single slot, unwrapped suite", Report{UnwrappedContainers: []string{"dolt x"}}, true},
		{"single slot, docker unknown", Report{DockerUnknown: true}, true},
		{"pool, one of two held", Report{Held: true, HeldCount: 1, Total: 2}, false},
		{"pool, all held", Report{Held: true, HeldCount: 2, Total: 2}, true},
		{"pool, docker unknown", Report{Total: 2, DockerUnknown: true}, true},
	} {
		if got := tc.rep.Busy(); got != tc.want {
			t.Errorf("%s: Busy() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestParseVMInfo: the doctor's Docker VM capacity comes from this parse of
// `docker info --format "{{.NCPU}} {{.MemTotal}}"`.
func TestParseVMInfo(t *testing.T) {
	t.Parallel()
	got, err := parseVMInfo("12 8485076992\n")
	if err != nil || got != (VMInfo{NCPU: 12, MemBytes: 8485076992}) {
		t.Fatalf("parseVMInfo = %+v, %v; want 12 vCPU / 8485076992 bytes", got, err)
	}
	for _, bad := range []string{"", "12", "12 8485076992 extra", "twelve 8485076992", "12 lots"} {
		if _, err := parseVMInfo(bad); err == nil {
			t.Errorf("parseVMInfo(%q) succeeded, want an error", bad)
		}
	}
}
