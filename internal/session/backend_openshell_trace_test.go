package session

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/sandbox"
)

// openshellTimedBackend is the test backend with a clock that moves a second
// per openshell call, so each phase's duration follows from how many calls it
// makes. The clock is the trace's clock: Prepare times itself on the trace.
func openshellTimedBackend(town string, replies map[string]string) (*openshellBackend, *sandbox.Fake, *traceClock) {
	clock := newTraceClock()
	fake := openshellPrepareFake(replies)
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	run := b.cli.Run
	b.cli.Run = func(ctx context.Context, argv []string, stdin []byte, env []string) ([]byte, []byte, error) {
		clock.Advance(time.Second)
		return run(ctx, argv, stdin, env)
	}
	return b, fake, clock
}

// traceNames is the phase names in a trace summary, in order, without the
// total. A summary that does not end in a total fails the test.
func traceNames(t *testing.T, summary string) []string {
	t.Helper()
	parts := strings.Split(summary, ", ")
	last := parts[len(parts)-1]
	if !strings.HasPrefix(last, "total ") {
		t.Fatalf("summary %q does not end in a total", summary)
	}
	var names []string
	for _, part := range parts[:len(parts)-1] {
		i := strings.LastIndex(part, " ")
		if i < 0 {
			t.Fatalf("summary part %q has no duration", part)
		}
		names = append(names, part[:i])
	}
	return names
}

func TestOpenShellPrepareTimesEachPhaseInOrder(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	b, _, clock := openshellTimedBackend(town, map[string]string{"get": openshellReadyReply})
	spec := openshellSpawn(town, work)
	spec.Trace = NewSandboxTrace(clock.Now)

	if _, err := b.Prepare(context.Background(), spec); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	want := []string{"push", "create", "ready", "gt check", "skeleton and env", "clone", "workspace and record"}
	if got := traceNames(t, spec.Trace.Summary()); !reflect.DeepEqual(got, want) {
		t.Fatalf("phases = %q, want %q", got, want)
	}
}

func TestOpenShellPrepareDriftRefusalTimesTheTeardown(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	b, _, clock := openshellTimedBackend(town, map[string]string{
		"get":  openshellReadyReply,
		"exec": "gt version 1.2.1 (dev: main@" + openshellTestDriftedGT + ")\n",
	})
	spec := openshellSpawn(town, work)
	spec.Trace = NewSandboxTrace(clock.Now)

	if _, err := b.Prepare(context.Background(), spec); err == nil {
		t.Fatal("Prepare succeeded, want the drifted sandbox refused")
	}

	want := []string{"push", "create", "ready", "gt check", "teardown"}
	if got := traceNames(t, spec.Trace.Summary()); !reflect.DeepEqual(got, want) {
		t.Fatalf("phases = %q, want %q: a refused sandbox logs the phases it ran and its teardown", got, want)
	}
}
