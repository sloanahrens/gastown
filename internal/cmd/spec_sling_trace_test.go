package cmd

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/session"
)

// slingTraceAt is a trace over a clock the test moves by hand; each entry of
// phases runs one phase that takes that long.
func slingTraceAt(phases map[string]time.Duration, order ...string) *session.SandboxTrace {
	clock := time.Unix(1_000_000, 0)
	now := func() time.Time { return clock }
	trace := session.NewSandboxTrace(now)
	for _, name := range order {
		_ = trace.Phase(name, func() error {
			clock = clock.Add(phases[name])
			return nil
		})
	}
	return trace
}

func TestSpecSlingTraceLine(t *testing.T) {
	t.Parallel()
	phases := map[string]time.Duration{"push": 2 * time.Second, "create": 18 * time.Second}
	tests := []struct {
		name  string
		trace *session.SandboxTrace
		err   error
		want  string
	}{
		{
			name:  "started sandbox names each phase and the total",
			trace: slingTraceAt(phases, "push", "create"),
			want:  "sling gt-0uh64 seat deepseek-flash-sandbox-k8s: push 2s, create 18s, total 20s; outcome: started",
		},
		{
			name:  "refused sandbox keeps the phases and the error",
			trace: slingTraceAt(phases, "push", "create"),
			err:   errors.New("sandbox gt-x: image drift"),
			want:  "sling gt-0uh64 seat deepseek-flash-sandbox-k8s: push 2s, create 18s, total 20s; outcome: error: sandbox gt-x: image drift",
		},
		{
			name:  "a multi-line error is cut to its first line",
			trace: slingTraceAt(phases, "push", "create"),
			err:   errors.New("sandbox gt-x: image drift\n  see: gt sandbox logs gt-x\n  exit status 1"),
			want:  "sling gt-0uh64 seat deepseek-flash-sandbox-k8s: push 2s, create 18s, total 20s; outcome: error: sandbox gt-x: image drift",
		},
		{
			name:  "a sling that touched no sandbox logs nothing",
			trace: slingTraceAt(nil),
			want:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := specSlingTraceLine("gt-0uh64", "deepseek-flash-sandbox-k8s", tt.trace, tt.err)
			if got != tt.want {
				t.Fatalf("specSlingTraceLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSpecSlingTracesReportsSortedAndSkipsEmpty(t *testing.T) {
	t.Parallel()
	var traces specSlingTraces
	traces.add("sling gt-b seat x: total 1s; outcome: started")
	traces.add("")
	traces.add("sling gt-a seat x: total 1s; outcome: started")

	want := []string{
		"sling gt-a seat x: total 1s; outcome: started",
		"sling gt-b seat x: total 1s; outcome: started",
	}
	if got := traces.notices(); !reflect.DeepEqual(got, want) {
		t.Fatalf("notices() = %q, want %q: the slings run in parallel, so the log must not depend on their order", got, want)
	}
}

func TestSpecSlingTracesWithNoSandboxSlingAddsNoNotice(t *testing.T) {
	t.Parallel()
	var traces specSlingTraces
	traces.add("")

	if got := traces.notices(); got != nil {
		t.Fatalf("notices() = %q, want nil so the tick report carries no empty notice", got)
	}
}

// The sling's spawn must carry the tick's trace, or the sandbox phases it
// times never reach the tick's log (gt-0uh64).
func TestSpecSlingSpawnRidesTheTicksTraceAndSeatSnapshot(t *testing.T) {
	t.Parallel()
	trace := session.NewSandboxTrace(func() time.Time { return time.Unix(1_000_000, 0) })
	occupied := func() ([]poolSession, error) { return []poolSession{{name: "box"}}, nil }
	var got SlingSpawnOptions
	spawn := func(rig string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
		if rig != "gastown" {
			t.Errorf("spawn rig = %q, want the rig the sling named", rig)
		}
		got = opts
		return &SpawnedPolecatInfo{}, nil
	}

	if _, err := specSlingSpawn(spawn, occupied, trace)("gastown", SlingSpawnOptions{Agent: "box"}); err != nil {
		t.Fatalf("spawn error = %v", err)
	}
	if got.trace != trace {
		t.Error("the spawn's options carry another trace: the session start would time nothing the tick logs")
	}
	if got.Agent != "box" {
		t.Errorf("Agent = %q, want the caller's option kept", got.Agent)
	}
	if got.occupied == nil {
		t.Fatal("occupied = nil, want the tick's snapshot")
	}
	if sessions, err := got.occupied(); err != nil || len(sessions) != 1 || sessions[0].name != "box" {
		t.Errorf("occupied() = %v, %v, want the tick's snapshot", sessions, err)
	}
}

// A tick with no roster snapshot yet leaves the seat decision reading the town.
func TestSpecSlingSpawnWithoutASnapshotLeavesTheReadToTheTown(t *testing.T) {
	t.Parallel()
	var got SlingSpawnOptions
	spawn := func(_ string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
		got = opts
		return &SpawnedPolecatInfo{}, nil
	}

	if _, err := specSlingSpawn(spawn, nil, nil)("gastown", SlingSpawnOptions{}); err != nil {
		t.Fatalf("spawn error = %v", err)
	}
	if got.occupied != nil {
		t.Error("occupied set with no snapshot: the pool would read an empty town instead of the real one")
	}
}

// The spawn's pre-sling steps are named on the spec dispatcher's trace: the
// backpressure check and the seat decision run before the spawner reports any
// stage of its own, and the admission follows them, so a sling that waits 30 s
// on a seat decision says so in the tick's log instead of hiding it in the
// total (gt-7nctk).
func TestSlingSeatSpawnNamesThePreSlingStepsOnTheTrace(t *testing.T) {
	t.Parallel()
	clock := time.Unix(1_000_000, 0)
	now := func() time.Time { return clock }
	trace := session.NewSandboxTrace(now)
	s := slingSeatSpawn{
		backpressure: func(string, string, SlingSpawnOptions) error {
			clock = clock.Add(2 * time.Second)
			return nil
		},
		resolvePool: func(_, requested string) (string, string, error) {
			clock = clock.Add(30 * time.Second)
			return "box", "", nil
		},
		releaseSeat: func() {},
		prepare: func(_, rigName string, opts SlingSpawnOptions, _ slingStores) (*SpawnedPolecatInfo, error) {
			clock = clock.Add(time.Second)
			opts.step("admission")
			return &SpawnedPolecatInfo{RigName: rigName, agent: opts.Agent}, nil
		},
	}

	if _, err := s.spawn("/town", "gastown", SlingSpawnOptions{HookBead: "gt-a", trace: trace, Steps: trace.Steps()}); err != nil {
		t.Fatalf("spawn: %v", err)
	}

	want := "backpressure 2s, seat decision 30s, admission 1s, total 33s"
	if got := trace.Summary(); got != want {
		t.Fatalf("Summary() = %q, want %q: the steps before the spawner's own are named and add up", got, want)
	}
}
