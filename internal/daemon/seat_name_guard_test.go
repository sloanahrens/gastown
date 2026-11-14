package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/attention"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/sandbox"
)

// seatNameGuardNow is the tick these tests judge a sandbox's age against, so no
// test reads the wall clock.
var seatNameGuardNow = time.Date(2026, 11, 14, 19, 40, 0, 0, time.UTC)

// TestSeatNameGuardPassesOverAStaleSandboxName is the dispatcher's draw: a free
// name whose gateway sandbox is in phase Error is not drawn, the next free name
// is, and the line names the stale sandbox (gt-zapy97).
func TestSeatNameGuardPassesOverAStaleSandboxName(t *testing.T) {
	t.Parallel()
	guard := NewSeatNameGuard([]sandbox.Entry{
		{Name: "gt-dag", Phase: "Error"},
		{Name: "gt-nux", Phase: "Ready"},
	}, seatNameGuardNow)

	finding, ok := guard.Stale("gt-dag")
	if !ok || finding.Phase != "Error" {
		t.Fatalf("Stale(gt-dag) = %+v, %v; want the Error sandbox", finding, ok)
	}
	if got, want := guard.Polecats("gt"), []string{"dag"}; !slices.Equal(got, want) {
		t.Fatalf("Polecats(gt) = %v, want %v: the pool must not draw the stale name", got, want)
	}
	if line := finding.Line(); !strings.Contains(line, "gt-dag") || !strings.Contains(line, "Error") {
		t.Errorf("Line() = %q; want one line naming the stale sandbox and its phase", line)
	}

	// The dispatcher's pool, marked with the names the guard blocks, draws the
	// next free name.
	pool := polecat.NewNamePoolWithConfig(t.TempDir(), "rig", "", []string{"dag", "nux"}, 2)
	if err := pool.Load(); err != nil {
		t.Fatal(err)
	}
	for _, name := range guard.Polecats("gt") {
		pool.MarkInUse(name)
	}
	got, err := pool.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if got != "nux" {
		t.Errorf("the pool drew %q, want nux: the stale name dag must be passed over", got)
	}
}

// TestSeatNameGuardRaisesTheStaleSeatItem is the second box: the stale sandbox
// raises seat-sandbox-stale:<name>, naming the phase and how long it has been
// in it (gt-zapy97).
func TestSeatNameGuardRaisesTheStaleSeatItem(t *testing.T) {
	t.Parallel()
	guard := NewSeatNameGuard([]sandbox.Entry{{Name: "gt-dag", Phase: "Error"}}, seatNameGuardNow)
	// A later tick still reads the same sandbox, so the age runs from the
	// first reading.
	guard.Observe([]sandbox.Entry{{Name: "gt-dag", Phase: "Error"}}, seatNameGuardNow.Add(7*time.Minute))

	items := guard.Items(seatNameGuardNow.Add(7 * time.Minute))
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one for the stale sandbox", items)
	}
	it := items[0]
	if it.Key != "seat-sandbox-stale:gt-dag" || it.Kind != attention.KindSeatSandboxStale ||
		it.Severity != attention.SeverityHigh {
		t.Errorf("item = %+v, want a high seat-sandbox-stale:gt-dag item", it)
	}
	for _, want := range []string{"Error", "7m", "gt-dag"} {
		if !strings.Contains(it.Summary, want) {
			t.Errorf("summary %q should name %q", it.Summary, want)
		}
	}
}

// TestSeatNameGuardLeavesReadyAndAbsentNamesAlone is the third box: a sandbox
// the gateway reports Ready, a phase it does not leave on its own, and a name
// with no sandbox at all are unaffected (gt-zapy97).
func TestSeatNameGuardLeavesReadyAndAbsentNamesAlone(t *testing.T) {
	t.Parallel()
	guard := NewSeatNameGuard([]sandbox.Entry{
		{Name: "gt-nux", Phase: "Ready"},
		{Name: "gt-ava", Phase: "Running"},
		{Name: "gt-stopped", Phase: "Stopped"},
	}, seatNameGuardNow)

	if findings := guard.Findings(); len(findings) != 0 {
		t.Fatalf("findings = %+v, want none: only a failure phase is stale", findings)
	}
	if items := guard.Items(seatNameGuardNow); len(items) != 0 {
		t.Errorf("items = %+v, want none", items)
	}
	if got := guard.Polecats("gt"); len(got) != 0 {
		t.Errorf("Polecats(gt) = %v, want none", got)
	}
	for _, name := range []string{"gt-nux", "gt-ava", "gt-stopped", "gt-absent"} {
		if f, ok := guard.Stale(name); ok {
			t.Errorf("Stale(%s) = %+v; want no finding", name, f)
		}
	}
}

// TestSeatNameGuardCollectorCarriesTheAgeAcrossTicks drives the production
// collector: the daemon's guard keeps the first reading of a stale sandbox, so
// a later heartbeat's item names the age, and the sandbox leaving the listing
// clears the item (gt-zapy97).
func TestSeatNameGuardCollectorCarriesTheAgeAcrossTicks(t *testing.T) {
	t.Parallel()
	entries := []sandbox.Entry{{Name: "gt-dag", Phase: "Error"}}
	d := seatNameGuardDaemon(t)
	src := &attentionSources{d: d, now: seatNameGuardNow, sandboxList: func(context.Context) ([]sandbox.Entry, error) {
		return entries, nil
	}}

	first, err := src.collectStaleSeatSandboxes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || !strings.Contains(first[0].Summary, "0m") {
		t.Fatalf("first items = %+v, want the sandbox read for the first time", first)
	}

	src.now = seatNameGuardNow.Add(6 * time.Minute)
	later, err := src.collectStaleSeatSandboxes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(later) != 1 || !strings.Contains(later[0].Summary, "6m") {
		t.Fatalf("later items = %+v, want the age carried from the first reading", later)
	}

	entries = nil
	cleared, err := src.collectStaleSeatSandboxes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cleared) != 0 {
		t.Errorf("items after the sandbox left the listing = %+v, want none", cleared)
	}
}

// TestSeatNameGuardCollectorKeepsTheItemWhenTheListingFails: a listing that
// cannot be read raises no item and reports the error, so the queue keeps the
// previous tick's item rather than clearing it (gt-zapy97).
func TestSeatNameGuardCollectorKeepsTheItemWhenTheListingFails(t *testing.T) {
	t.Parallel()
	d := seatNameGuardDaemon(t)
	src := &attentionSources{d: d, now: seatNameGuardNow, sandboxList: func(context.Context) ([]sandbox.Entry, error) {
		return nil, errors.New("gateway did not answer")
	}}

	items, err := src.collectStaleSeatSandboxes(context.Background())
	if err == nil {
		t.Fatal("a listing that cannot be read reported no error")
	}
	if len(items) != 0 {
		t.Errorf("items = %+v, want none from an unanswered read", items)
	}
}

// TestSeatNameGuardCollectorReportsAnUnreadableProfileList: a town settings read
// that fails is an unread answer, not a town with no gateway, so the collector
// reports it and does not ask the gateway (gt-zapy97).
func TestSeatNameGuardCollectorReportsAnUnreadableProfileList(t *testing.T) {
	t.Parallel()
	d := seatNameGuardDaemon(t)
	d.diskMaint.sandboxes = func() (map[string]agentconfig.SandboxProfile, error) {
		return nil, errors.New("the town's settings did not parse")
	}
	asked := false
	src := &attentionSources{d: d, now: seatNameGuardNow, sandboxList: func(context.Context) ([]sandbox.Entry, error) {
		asked = true
		return nil, nil
	}}

	items, err := src.collectStaleSeatSandboxes(context.Background())
	if err == nil {
		t.Fatal("an unreadable profile list reported no error")
	}
	if asked {
		t.Error("the gateway was listed after the town's profiles could not be read")
	}
	if len(items) != 0 {
		t.Errorf("items = %+v, want none", items)
	}
}

// TestSeatNameGuardCollectorLeavesAHostOnlyTownAlone: a town that declares no
// sandbox profile runs no gateway, so the collector asks it nothing and raises
// nothing (gt-zapy97).
func TestSeatNameGuardCollectorLeavesAHostOnlyTownAlone(t *testing.T) {
	t.Parallel()
	d := seatNameGuardDaemon(t)
	d.diskMaint.sandboxes = func() (map[string]agentconfig.SandboxProfile, error) { return nil, nil }
	asked := false
	src := &attentionSources{d: d, now: seatNameGuardNow, sandboxList: func(context.Context) ([]sandbox.Entry, error) {
		asked = true
		return nil, nil
	}}

	items, err := src.collectStaleSeatSandboxes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if asked {
		t.Error("the gateway was listed in a town with no sandbox profile")
	}
	if len(items) != 0 {
		t.Errorf("items = %+v, want none", items)
	}
}

// seatNameGuardDaemon is the daemon these collectors run against: a town whose
// settings name one sandbox profile, so the collector reaches the gateway.
func seatNameGuardDaemon(t *testing.T) *Daemon {
	t.Helper()
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}}
	d.diskMaint.sandboxes = func() (map[string]agentconfig.SandboxProfile, error) {
		return map[string]agentconfig.SandboxProfile{"polecat": {Image: "img", Policy: "p.yaml", Gateway: "k8s"}}, nil
	}
	return d
}
