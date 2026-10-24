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
