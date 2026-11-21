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

// TestSeatNameGuardQuotesTheSandboxNameDeleteCommand: the sandbox name comes
// from the gateway's listing, so the suggested delete command carries it as one
// quoted shell word, never as a second argument or a flag (gt-xrt3pb).
func TestSeatNameGuardQuotesTheSandboxNameDeleteCommand(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		want string
	}{
		{"gt-dag", "openshell sandbox delete 'gt-dag'"},
		{"-f", "openshell sandbox delete '-f'"},
		{"gt dag; rm -rf ~", "openshell sandbox delete 'gt dag; rm -rf ~'"},
		{"gt-dag'$(id)", `openshell sandbox delete 'gt-dag'\''$(id)'`},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			guard := NewSeatNameGuard([]sandbox.Entry{{Name: tc.name, Phase: "Error"}}, seatNameGuardNow)
			finding, ok := guard.Stale(tc.name)
			if !ok {
				t.Fatalf("Stale(%q) = no finding, want the Error sandbox", tc.name)
			}
			if line := finding.Line(); !strings.Contains(line, tc.want) {
				t.Errorf("Line() = %q, should carry %q", line, tc.want)
			}
			items := guard.Items(seatNameGuardNow)
			if len(items) != 1 || !strings.Contains(items[0].Summary, tc.want) {
				t.Errorf("items = %+v, should carry %q", items, tc.want)
			}
		})
	}
}

// TestSeatNameGuardLineWarnsBeforeTheDeleteCommand: a failed sandbox may hold
// unsaved work, so the dispatcher line names the check and the runbook before
// the delete command, which stays quoted after them (gt-1h6tbf).
func TestSeatNameGuardLineWarnsBeforeTheDeleteCommand(t *testing.T) {
	t.Parallel()
	guard := NewSeatNameGuard([]sandbox.Entry{{Name: "gt-dag", Phase: "Error"}}, seatNameGuardNow)
	finding, ok := guard.Stale("gt-dag")
	if !ok {
		t.Fatal("Stale(gt-dag) = no finding, want the Error sandbox")
	}
	line := finding.Line()
	del := strings.Index(line, "openshell sandbox delete 'gt-dag'")
	if del < 0 {
		t.Fatalf("Line() = %q, want the quoted delete command", line)
	}
	for _, want := range []string{"gt polecat check-recovery", "docs/sandbox-rescue-runbook.md"} {
		at := strings.Index(line, want)
		if at < 0 {
			t.Errorf("Line() = %q, want it to name %q", line, want)
			continue
		}
		if at > del {
			t.Errorf("Line() names %q after the delete command; want the check and runbook first: %q", want, line)
		}
	}
}

// TestSeatNameGuardItemWarnsBeforeTheDeleteCommand: the attention item's summary
// names the check and the runbook before the delete command; key and severity
// are unchanged (gt-h88z0p).
func TestSeatNameGuardItemWarnsBeforeTheDeleteCommand(t *testing.T) {
	t.Parallel()
	guard := NewSeatNameGuard([]sandbox.Entry{{Name: "gt-dag", Phase: "Error"}}, seatNameGuardNow)
	items := guard.Items(seatNameGuardNow)
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one for the stale sandbox", items)
	}
	it := items[0]
	if it.Key != "seat-sandbox-stale:gt-dag" || it.Kind != attention.KindSeatSandboxStale ||
		it.Severity != attention.SeverityHigh {
		t.Errorf("item = %+v, want a high seat-sandbox-stale:gt-dag item", it)
	}
	del := strings.Index(it.Summary, "openshell sandbox delete 'gt-dag'")
	if del < 0 {
		t.Fatalf("summary %q, want the quoted delete command", it.Summary)
	}
	for _, want := range []string{"gt polecat check-recovery", "docs/sandbox-rescue-runbook.md", "delete it only when check-recovery reads clean"} {
		at := strings.Index(it.Summary, want)
		if at < 0 {
			t.Errorf("summary %q, want it to name %q", it.Summary, want)
			continue
		}
		if at > del {
			t.Errorf("summary names %q after the delete command; want the check and runbook first: %q", want, it.Summary)
		}
	}
}

// TestGatewaySandboxListingBoundsTheReadWithTheCallersTimeout: the one listing
// the attention collector and the sling tick share applies the caller's bound
// to the gateway call (gt-xrt3pb).
func TestGatewaySandboxListingBoundsTheReadWithTheCallersTimeout(t *testing.T) {
	t.Parallel()
	const timeout = time.Minute
	var remaining time.Duration
	var bounded bool
	cli := &sandbox.CLI{Run: func(ctx context.Context, _ []string, _ []byte, _ []string) ([]byte, []byte, error) {
		deadline, ok := ctx.Deadline()
		bounded = ok
		remaining = time.Until(deadline)
		return []byte(`{"sandboxes":[{"name":"gt-dag","phase":"Error"}]}`), nil, nil
	}}

	entries, err := GatewaySandboxListing(context.Background(), cli, timeout)
	if err != nil {
		t.Fatal(err)
	}
	if !bounded || remaining <= 0 || remaining > timeout {
		t.Errorf("the gateway call's remaining bound = %s (set %v), want the caller's %s", remaining, bounded, timeout)
	}
	if len(entries) != 1 || entries[0].Name != "gt-dag" {
		t.Errorf("entries = %+v, want the gateway's listing", entries)
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

// TestSeatNameGuardCollectorReportsAListingFailure: a listing that cannot be
// read raises no item and reports the error, so collectAttention keeps the
// previous tick's items rather than clearing them (gt-zapy97).
func TestSeatNameGuardCollectorReportsAListingFailure(t *testing.T) {
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

// TestSeatNameGuardCollectorKeepsTheStaleItemWhenTheListingFails drives the
// merge the failed collector's report goes through: the previous tick's stale
// item is kept, and another kind's item is not (gt-zapy97, gt-xrt3pb).
func TestSeatNameGuardCollectorKeepsTheStaleItemWhenTheListingFails(t *testing.T) {
	t.Parallel()
	d := seatNameGuardDaemon(t)
	src := &attentionSources{d: d, now: seatNameGuardNow, sandboxList: func(context.Context) ([]sandbox.Entry, error) {
		return nil, errors.New("gateway did not answer")
	}}
	stale := attention.Item{
		Key: seatSandboxStaleKeyPrefix + "gt-dag", Kind: attention.KindSeatSandboxStale,
		Severity: attention.SeverityHigh, Summary: "seat sandbox gt-dag is stale",
	}
	other := attention.Item{
		Key: "spot:gt-dag", Kind: attention.KindSandboxSeatKept,
		Severity: attention.SeverityHigh, Summary: "a kept seat",
	}
	prev := attention.State{Updated: seatNameGuardNow, Items: []attention.Item{stale, other}}
	collectors := []attentionCollector{{
		kind: attention.KindSeatSandboxStale, collect: src.collectStaleSeatSandboxes, owns: seatSandboxStaleItem,
	}}

	observed := collectAttention(context.Background(), prev, collectors, func(string, ...any) {})
	if len(observed) != 1 || observed[0].Key != stale.Key {
		t.Fatalf("observed = %+v, want only the previous stale item kept", observed)
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

// seatNameGuardListing is one gateway listing of sandboxes in phase.
func seatNameGuardListing(phase string, names ...string) []sandbox.Entry {
	out := make([]sandbox.Entry, 0, len(names))
	for _, n := range names {
		out = append(out, sandbox.Entry{Name: n, Phase: phase})
	}
	return out
}

// seatNameGuardTick is one listing the guard reads, at tick at.
type seatNameGuardTick struct {
	at      time.Time
	entries []sandbox.Entry
}

// TestSeatNameGuardObserve is the guard's picture across listings: a name is
// trimmed, an empty name is ignored, a phase change starts the clock again, the
// same failure keeps it, and a Since later than now is replaced by now
// (gt-xtwkwg).
func TestSeatNameGuardObserve(t *testing.T) {
	t.Parallel()
	now := seatNameGuardNow
	for _, tc := range []struct {
		name      string
		ticks     []seatNameGuardTick
		wantNames []string
		wantSince time.Time
	}{
		{"a padded name is keyed trimmed", []seatNameGuardTick{{now, seatNameGuardListing("Error", " gt-dag ")}}, []string{"gt-dag"}, now},
		{"an empty name is ignored", []seatNameGuardTick{{now, seatNameGuardListing("Error", "", "   ")}}, nil, time.Time{}},
		{"a phase change to another failure starts the clock again", []seatNameGuardTick{
			{now, seatNameGuardListing("Error", "gt-dag")},
			{now.Add(5 * time.Minute), seatNameGuardListing("Failed", "gt-dag")},
		}, []string{"gt-dag"}, now.Add(5 * time.Minute)},
		{"the same failure keeps the first reading's time", []seatNameGuardTick{
			{now, seatNameGuardListing("Error", "gt-dag")},
			{now.Add(5 * time.Minute), seatNameGuardListing("Error", "gt-dag")},
		}, []string{"gt-dag"}, now},
		{"a Since later than now is replaced by now", []seatNameGuardTick{
			{now.Add(10 * time.Minute), seatNameGuardListing("Error", "gt-dag")},
			{now, seatNameGuardListing("Error", "gt-dag")},
		}, []string{"gt-dag"}, now},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			guard := &SeatNameGuard{}
			for _, tick := range tc.ticks {
				guard.Observe(tick.entries, tick.at)
			}
			var got []string
			for _, f := range guard.Findings() {
				got = append(got, f.Sandbox)
			}
			if !slices.Equal(got, tc.wantNames) {
				t.Fatalf("findings = %v, want %v", got, tc.wantNames)
			}
			if len(tc.wantNames) > 0 {
				if f, _ := guard.Stale(tc.wantNames[0]); !f.Since.Equal(tc.wantSince) {
					t.Errorf("Since = %s, want %s", f.Since, tc.wantSince)
				}
			}
		})
	}
}

// TestSeatNameGuardPolecats is the pool's refusal: only a sandbox named for a
// polecat of the rig's prefix, with one name after the prefix and no slash,
// keeps that polecat out of the draw (gt-xtwkwg).
func TestSeatNameGuardPolecats(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		prefix    string
		sandboxes []string
		want      []string
	}{
		{"a seat of the rig is named", "gt", []string{"gt-dag"}, []string{"dag"}},
		{"a name with a slash names no polecat", "gt", []string{"gt-a/b"}, nil},
		{"an empty name after the prefix names no polecat", "gt", []string{"gt-"}, nil},
		{"a name outside the prefix is left alone", "gt", []string{"xx-dag"}, nil},
		{"an empty rig prefix names no seat", "", []string{"-dag", "gt-dag"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			guard := NewSeatNameGuard(seatNameGuardListing("Error", tc.sandboxes...), seatNameGuardNow)
			if got := guard.Polecats(tc.prefix); !slices.Equal(got, tc.want) {
				t.Errorf("Polecats(%q) = %v, want %v", tc.prefix, got, tc.want)
			}
		})
	}
}

// TestSeatNameGuardFindings: findings come back sorted by name whichever order
// the listing named the sandboxes in (gt-xtwkwg).
func TestSeatNameGuardFindings(t *testing.T) {
	t.Parallel()
	want := []string{"gt-dag", "gt-nux"}
	for _, tc := range []struct {
		name    string
		listing []sandbox.Entry
	}{
		{"listed in name order", seatNameGuardListing("Error", "gt-dag", "gt-nux")},
		{"listed in reverse name order", seatNameGuardListing("Failed", "gt-nux", "gt-dag")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got []string
			for _, f := range NewSeatNameGuard(tc.listing, seatNameGuardNow).Findings() {
				got = append(got, f.Sandbox)
			}
			if !slices.Equal(got, want) {
				t.Errorf("Findings() = %v, want %v", got, want)
			}
		})
	}
}

// TestSeatNameGuardItems: an item's age runs from the first reading in the
// current phase, and a reading in the future is an age of zero, not a negative
// one (gt-xtwkwg).
func TestSeatNameGuardItems(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		since   time.Time
		wantAge string
	}{
		{"a reading six minutes old is six minutes old", seatNameGuardNow.Add(-6 * time.Minute), "for 6m"},
		{"a reading in the future is zero minutes old", seatNameGuardNow.Add(5 * time.Minute), "for 0m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			guard := NewSeatNameGuard(seatNameGuardListing("Error", "gt-dag"), tc.since)
			items := guard.Items(seatNameGuardNow)
			if len(items) != 1 || !strings.Contains(items[0].Summary, tc.wantAge) {
				t.Errorf("items = %+v, want one whose summary says %q", items, tc.wantAge)
			}
		})
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
