package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// The tick writes what it decided about every bead it did not sling to the
// town's verdict ledger, so the dashboard and an operator can read why a bead
// waits and since when (gt-8thox). A dry run decides without acting and
// records nothing, because its reasons would overwrite the real tick's.

// specVerdictEnv is a fake tick wired to record into town's real ledger the
// way runSpecDispatchAt wires the daemon's tick: the town, and the writer its
// warning goes to.
func specVerdictEnv(f *fakeSpecTown, town string, warn *bytes.Buffer) specDispatchEnv {
	env := f.env()
	env.TownRoot = town
	env.LedgerWarn = warn
	return env
}

// seedVerdictLedger writes a ledger to town as an earlier tick left it.
func seedVerdictLedger(t *testing.T, town string, at time.Time, entries []specdispatch.Entry) {
	t.Helper()
	path := specdispatch.LedgerPath(town)
	l, err := specdispatch.LoadLedger(path)
	if err != nil {
		t.Fatalf("loading the ledger to seed it: %v", err)
	}
	l.Record(at, entries)
	if err := l.Save(path); err != nil {
		t.Fatalf("seeding the ledger: %v", err)
	}
}

// readVerdictLedger reads town's ledger back off disk, which is how the
// dashboard will read it.
func readVerdictLedger(t *testing.T, town string) *specdispatch.Ledger {
	t.Helper()
	l, err := specdispatch.LoadLedger(specdispatch.LedgerPath(town))
	if err != nil {
		t.Fatalf("reading the ledger back: %v", err)
	}
	return l
}

// TestSpecDispatchVerdictsRecordsEachHeldBead runs a real tick over a fixture
// that lands a bead in every class the ledger holds, and reads the file back:
// one entry per bead the tick did not sling, with the class of its list and
// the line it was reported under as the reason. The bead that was slung is not
// in it.
func TestSpecDispatchVerdictsRecordsEachHeldBead(t *testing.T) {
	t.Parallel()
	shapeless := cleanSpec("gt-ref", 1, "2026-09-29T10:00:00Z")
	shapeless.Description = strings.Replace(shapeless.Description, "## Gate\nmake gate", "", 1)
	plan := cleanSpec("gt-plan", 1, "2026-09-29T10:01:00Z")
	plan.Labels = append(plan.Labels, specdispatch.NeedsPlanningLabel)
	f := newFakeSpecTown(
		shapeless,
		plan,
		cleanSpec("gt-defer", 1, "2026-09-29T10:02:00Z"),
		cleanSpec("gt-fail", 1, "2026-09-29T10:03:00Z"),
		cleanSpec("gt-slung", 1, "2026-09-29T10:04:00Z"),
		cleanSpec("gt-extra", 1, "2026-09-29T10:05:00Z"),
	)
	f.slingErrs["gt-defer"] = []error{fmt.Errorf("starting session: %w", session.ErrImageDrift)}
	f.slingErrs["gt-fail"] = []error{errors.New("starting polecat session: tmux: no server")}
	town := t.TempDir()
	var warn bytes.Buffer
	env := specVerdictEnv(f, town, &warn)
	// Room for three slings: the deferral, the failure and the one that takes
	// a seat. The last clean bead is over the limit, so the tick only skips it.
	env.PerTick = 3
	r := runSpecDispatchCycle(env)
	if warn.Len() != 0 {
		t.Fatalf("warnings = %q, want none: the ledger is writable", warn.String())
	}
	if len(f.slung) != 1 || f.slung[0] != "gt-slung" {
		t.Fatalf("slung %v, want only gt-slung", f.slung)
	}

	l := readVerdictLedger(t, town)
	want := map[string]string{
		"gt-ref":   specVerdictRefused,
		"gt-plan":  specVerdictPlanning,
		"gt-defer": specVerdictDeferred,
		"gt-fail":  specVerdictFailed,
		"gt-extra": specVerdictSkipped,
	}
	if len(l.Verdicts) != len(want) {
		t.Fatalf("verdicts = %+v, want one for each of %d held beads", l.Verdicts, len(want))
	}
	for bead, class := range want {
		v, ok := l.Verdicts[bead]
		if !ok {
			t.Errorf("%s is not in the ledger: %+v", bead, l.Verdicts)
			continue
		}
		if v.Class != class {
			t.Errorf("%s class = %q, want %q", bead, v.Class, class)
		}
		if v.Rig != "gastown" {
			t.Errorf("%s rig = %q, want the bead's rig", bead, v.Rig)
		}
		if v.Reason == "" {
			t.Errorf("%s has no reason", bead)
		}
		if !v.FirstAt.Equal(specTestNow) || !v.LastAt.Equal(specTestNow) || v.Ticks != 1 {
			t.Errorf("%s seen %v..%v over %d ticks, want the tick's one clock and one sighting", bead, v.FirstAt, v.LastAt, v.Ticks)
		}
	}
	// The reason is the line the tick reported the bead under.
	for bead, line := range map[string]string{
		"gt-ref":   r.Refused[0].Line,
		"gt-plan":  r.Planning[0].Line,
		"gt-defer": r.Deferred[0].Line,
		"gt-fail":  r.Failed[0].Line,
	} {
		if got := l.Verdicts[bead].Reason; got != line {
			t.Errorf("%s reason = %q, want the tick's line %q", bead, got, line)
		}
	}
	// The deferral stays in the tick's skipped list too, and is recorded once,
	// under the more specific class (deferred, not skipped).
	deferredInSkipped := ""
	for _, e := range r.Skipped {
		if e.Bead == "gt-defer" {
			deferredInSkipped = e.Line
		}
		if e.Bead == "gt-extra" {
			if got := l.Verdicts["gt-extra"].Reason; got != e.Line {
				t.Errorf("gt-extra reason = %q, want the tick's line %q", got, e.Line)
			}
		}
	}
	if deferredInSkipped == "" {
		t.Fatalf("skipped = %+v, want the deferral listed there as well", r.Skipped)
	}
	if got := l.Verdicts["gt-defer"].Reason; got != deferredInSkipped {
		t.Errorf("gt-defer reason = %q, want the line it was skipped under %q", got, deferredInSkipped)
	}
	// A bead the tick slung is not a verdict.
	if _, ok := l.Verdicts["gt-slung"]; ok {
		t.Errorf("the slung bead is in the ledger: %+v", l.Verdicts["gt-slung"])
	}
	// The ledger speaks for the tick that wrote it.
	if !l.SavedAt.Equal(specTestNow) {
		t.Errorf("SavedAt = %v, want the tick's clock %v", l.SavedAt, specTestNow)
	}
}

// TestSpecDispatchVerdictsDryRunRecordsNothing: a dry run decides without
// acting, so it writes no ledger and leaves an existing one exactly as the
// last real tick left it.
func TestSpecDispatchVerdictsDryRunRecordsNothing(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"))
	town := t.TempDir()
	seedVerdictLedger(t, town, specTestNow.Add(-time.Minute), []specdispatch.Entry{
		{Bead: "gt-old", Rig: "gastown", Class: specVerdictSkipped, Reason: "gt-old: no free seat this tick"},
	})
	path := specdispatch.LedgerPath(town)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the seeded ledger: %v", err)
	}
	var warn bytes.Buffer
	env := specVerdictEnv(f, town, &warn)
	env.DryRun = true
	r := runSpecDispatchCycle(env)
	if len(r.Dispatched) != 1 || !strings.Contains(r.Dispatched[0].Line, "would sling") {
		t.Fatalf("dispatched = %+v, want the dry run's would-sling line", r.Dispatched)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("the dry run rewrote the ledger: err %v\n before %s\n after  %s", err, before, after)
	}
	// A town with no ledger yet keeps none.
	empty := t.TempDir()
	var quiet bytes.Buffer
	fresh := specVerdictEnv(newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")), empty, &quiet)
	fresh.DryRun = true
	runSpecDispatchCycle(fresh)
	if _, err := os.Stat(specdispatch.LedgerPath(empty)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the dry run wrote a ledger: %v", err)
	}
}

// TestSpecDispatchVerdictsClearsStaleVerdicts: the ledger states what the tick
// saw, so a tick that held everything and a tick that read no candidate at all
// both record an empty set, and the verdicts of beads the dispatcher no longer
// considers go with it.
func TestSpecDispatchVerdictsClearsStaleVerdicts(t *testing.T) {
	t.Parallel()
	stale := []specdispatch.Entry{
		{Bead: "gt-taken", Rig: "gastown", Class: specVerdictSkipped, Reason: "gt-taken: no free seat this tick"},
	}
	cases := []struct {
		name string
		town func() *fakeSpecTown
		hold string
	}{
		{name: "held_tick", town: func() *fakeSpecTown { return newFakeSpecTown(cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z")) }, hold: "operator hold"},
		{name: "no_candidates", town: func() *fakeSpecTown { return newFakeSpecTown() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := tc.town()
			f.hold = tc.hold
			town := t.TempDir()
			seedVerdictLedger(t, town, specTestNow.Add(-time.Minute), stale)
			var warn bytes.Buffer
			r := runSpecDispatchCycle(specVerdictEnv(f, town, &warn))
			if r.Hold != tc.hold {
				t.Fatalf("hold = %q, want %q", r.Hold, tc.hold)
			}
			l := readVerdictLedger(t, town)
			if len(l.Verdicts) != 0 {
				t.Fatalf("verdicts = %+v, want none: this tick saw no bead to hold", l.Verdicts)
			}
			if !l.SavedAt.Equal(specTestNow) {
				t.Errorf("SavedAt = %v, want the tick's clock %v", l.SavedAt, specTestNow)
			}
			if warn.Len() != 0 {
				t.Errorf("warnings = %q, want none", warn.String())
			}
		})
	}
}

// TestSpecDispatchVerdictsLedgerFailureLeavesTheTick: a ledger the tick cannot
// write, or one it cannot read back, is one warning line and nothing else —
// the tick decides and reports exactly what it decided without a ledger.
func TestSpecDispatchVerdictsLedgerFailureLeavesTheTick(t *testing.T) {
	t.Parallel()
	newTown := func() *fakeSpecTown {
		f := newFakeSpecTown(
			cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"),
			cleanSpec("gt-b", 1, "2026-09-29T10:01:00Z"),
		)
		f.slingErrs["gt-b"] = []error{errors.New("starting polecat session: tmux: no server")}
		return f
	}
	control := runSpecDispatchCycle(newTown().env())

	t.Run("unwritable", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		seedVerdictLedger(t, town, specTestNow.Add(-time.Minute), []specdispatch.Entry{
			{Bead: "gt-old", Rig: "gastown", Class: specVerdictSkipped, Reason: "gt-old: no free seat this tick"},
		})
		// The ledger reads back, so the tick gets as far as saving it, and the
		// directory takes no new file.
		dir := filepath.Dir(specdispatch.LedgerPath(town))
		//testpolicy:allow no-exec-files — the target is the fixture directory: a read-only dir needs its search bit
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatalf("making the ledger directory read-only: %v", err)
		}
		t.Cleanup(func() {
			//testpolicy:allow no-exec-files — the fixture directory takes its search bit back so the temp tree can be walked
			_ = os.Chmod(dir, 0o755)
		})
		var warn bytes.Buffer
		got := runSpecDispatchCycle(specVerdictEnv(newTown(), town, &warn))
		if !reflect.DeepEqual(got, control) {
			t.Errorf("the failed ledger changed the tick:\n got  %+v\n want %+v", got, control)
		}
		if lines := strings.Count(warn.String(), "\n"); lines != 1 || !strings.Contains(warn.String(), "verdict ledger not recorded") {
			t.Errorf("warnings = %q, want one line saying the ledger was not recorded", warn.String())
		}
		l := readVerdictLedger(t, town)
		if _, ok := l.Verdicts["gt-old"]; !ok {
			t.Errorf("the tick recorded onto a ledger it could not save: %+v", l.Verdicts)
		}
	})

	t.Run("corrupt", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		path := specdispatch.LedgerPath(town)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("making the ledger directory: %v", err)
		}
		// A ledger that cannot be read is not an empty one: recording onto it
		// would reset the first-seen time of every verdict it holds.
		corrupt := []byte(`{"verdicts": {"gt-old": {"bead": "gt-old", "class": "skipped"}}`)
		if err := os.WriteFile(path, corrupt, 0o644); err != nil {
			t.Fatalf("planting the corrupt ledger: %v", err)
		}
		var warn bytes.Buffer
		got := runSpecDispatchCycle(specVerdictEnv(newTown(), town, &warn))
		if !reflect.DeepEqual(got, control) {
			t.Errorf("the corrupt ledger changed the tick:\n got  %+v\n want %+v", got, control)
		}
		if lines := strings.Count(warn.String(), "\n"); lines != 1 || !strings.Contains(warn.String(), "verdict ledger not recorded") {
			t.Errorf("warnings = %q, want one line saying the ledger was not recorded", warn.String())
		}
		if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, corrupt) {
			t.Errorf("the tick recorded onto a ledger it could not read: err %v\n got  %s\n want %s", err, after, corrupt)
		}
	})

	t.Run("unreadable", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		runtime := constants.TownRuntimePath(town)
		if err := os.MkdirAll(runtime, 0o755); err != nil {
			t.Fatalf("making the runtime directory: %v", err)
		}
		// A regular file where the ledger's directory belongs: the read fails,
		// which must not read as an empty ledger to record onto.
		if err := os.WriteFile(filepath.Join(runtime, "spec_dispatch"), []byte("not a directory"), 0o644); err != nil {
			t.Fatalf("planting the file: %v", err)
		}
		var warn bytes.Buffer
		got := runSpecDispatchCycle(specVerdictEnv(newTown(), town, &warn))
		if !reflect.DeepEqual(got, control) {
			t.Errorf("the unreadable ledger changed the tick:\n got  %+v\n want %+v", got, control)
		}
		if lines := strings.Count(warn.String(), "\n"); lines != 1 || !strings.Contains(warn.String(), "verdict ledger not recorded") {
			t.Errorf("warnings = %q, want one line saying the ledger was not recorded", warn.String())
		}
	})
}
