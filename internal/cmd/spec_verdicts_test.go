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
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/lock"
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
	// under the more specific class (deferred, not skipped) with the line it
	// was deferred under: the later list wins both.
	deferredInSkipped := false
	for _, e := range r.Skipped {
		if e.Bead == "gt-defer" {
			deferredInSkipped = true
		}
		if e.Bead == "gt-extra" {
			if got := l.Verdicts["gt-extra"].Reason; got != e.Line {
				t.Errorf("gt-extra reason = %q, want the tick's line %q", got, e.Line)
			}
		}
	}
	if !deferredInSkipped {
		t.Fatalf("skipped = %+v, want the deferral listed there as well", r.Skipped)
	}
	if len(r.Deferred) != 1 {
		t.Fatalf("deferred = %+v, want the one deferral", r.Deferred)
	}
	if got := l.Verdicts["gt-defer"].Reason; got != r.Deferred[0].Line {
		t.Errorf("gt-defer reason = %q, want the line it was deferred under %q", got, r.Deferred[0].Line)
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
		var warn bytes.Buffer
		env := specVerdictEnv(newTown(), town, &warn)
		// The save fails on its own terms: a read-only directory would make
		// this case depend on the host's permissions, and pass or fail by
		// whether the tests run as a user those permissions bind.
		env.Ledger = &specLedgerFile{Save: func(string, *specdispatch.Ledger) error {
			return errors.New("publishing the ledger: no space left on device")
		}}
		got := runSpecDispatchCycle(env)
		if !reflect.DeepEqual(got, control) {
			t.Errorf("the failed ledger changed the tick:\n got  %+v\n want %+v", got, control)
		}
		if lines := strings.Count(warn.String(), "\n"); lines != 1 || !strings.Contains(warn.String(), "verdict ledger not recorded") {
			t.Errorf("warnings = %q, want one line saying the ledger was not recorded", warn.String())
		}
		l := readVerdictLedger(t, town)
		if len(l.Verdicts) != 1 {
			t.Errorf("the tick recorded onto a ledger it could not save: %+v", l.Verdicts)
		} else if _, ok := l.Verdicts["gt-old"]; !ok {
			t.Errorf("the tick replaced a ledger it could not save: %+v", l.Verdicts)
		}
	})

	t.Run("corrupt", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		path := specdispatch.LedgerPath(town)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("making the ledger directory: %v", err)
		}
		// A ledger that does not parse can never be read again, so the tick
		// replaces it and says so: leaving it would warn on every tick and
		// heal nothing (gt-tcsnr).
		corrupt := []byte(`{"verdicts": {"gt-old": {"bead": "gt-old", "class": "skipped"}}`)
		if err := os.WriteFile(path, corrupt, 0o644); err != nil {
			t.Fatalf("planting the corrupt ledger: %v", err)
		}
		var warn bytes.Buffer
		got := runSpecDispatchCycle(specVerdictEnv(newTown(), town, &warn))
		if !reflect.DeepEqual(got, control) {
			t.Errorf("the corrupt ledger changed the tick:\n got  %+v\n want %+v", got, control)
		}
		if lines := strings.Count(warn.String(), "\n"); lines != 1 || !strings.Contains(warn.String(), "verdict ledger replaced") {
			t.Errorf("warnings = %q, want one line saying the ledger was replaced", warn.String())
		}
		if after, err := os.ReadFile(path); err != nil || bytes.Equal(after, corrupt) {
			t.Errorf("the tick left the ledger it could not read in place: err %v\n got %s", err, after)
		}
		l := readVerdictLedger(t, town)
		if _, ok := l.Verdicts["gt-old"]; ok {
			t.Errorf("a verdict from the unparsable ledger survived it being replaced: %+v", l.Verdicts)
		}
		want := verdictClasses(control)
		if len(want) == 0 {
			t.Fatal("the control tick held no bead, so this case asserts nothing")
		}
		gotClasses := map[string]string{}
		for bead, v := range l.Verdicts {
			gotClasses[bead] = v.Class
		}
		if !reflect.DeepEqual(gotClasses, want) {
			t.Errorf("verdicts = %+v, want the tick's own record %+v", gotClasses, want)
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

// TestSpecDispatchVerdictsOneTickAtATime: two ticks — the daemon's and an
// operator's hand-run — share one town's ledger. Each tick loads the file,
// folds its verdicts and saves it, and that read-fold-save is one step under
// the ledger's lock: a tick that is between its load and its save holds it, so
// the other tick waits instead of loading a file the first is about to
// replace. Without the lock both loads see the same file and one tick of every
// verdict's count is lost (gt-tcsnr).
func TestSpecDispatchVerdictsOneTickAtATime(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := specdispatch.LedgerPath(town)
	inSave := make(chan struct{})
	release := make(chan struct{})
	var warn bytes.Buffer
	env := specVerdictEnv(newFakeSpecTown(
		cleanSpec("gt-a", 1, "2026-09-29T10:00:00Z"),
		cleanSpec("gt-b", 1, "2026-09-29T10:01:00Z"),
	), town, &warn)
	env.Ledger = &specLedgerFile{Save: func(path string, l *specdispatch.Ledger) error {
		close(inSave)
		<-release
		return l.Save(path)
	}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSpecDispatchCycle(env)
	}()
	<-inSave
	// The tick is mid-fold: the lock it took is still held, so the tick that
	// comes next cannot take it and must wait its turn.
	if unlock, held, err := lock.FlockTryAcquire(specVerdictLedgerLockPath(path)); err != nil || held {
		if unlock != nil {
			unlock()
		}
		t.Errorf("a second tick took the ledger lock while the first was saving: held=%v err=%v", held, err)
	}
	close(release)
	<-done
	if warn.Len() != 0 {
		t.Errorf("warnings = %q, want none", warn.String())
	}
	l := readVerdictLedger(t, town)
	if len(l.Verdicts) == 0 {
		t.Errorf("the tick recorded no verdict through the lock: %+v", l.Verdicts)
	}
	if !l.SavedAt.Equal(specTestNow) {
		t.Errorf("SavedAt = %v, want the tick's clock %v", l.SavedAt, specTestNow)
	}
}

// TestSpecTickVerdictsLaterListWins: the class a bead in two lists keeps is
// the later list's, and its reason is that list's line. The precedence is the
// fold's list order, not the order the report happened to build the lists in
// (gt-tcsnr).
func TestSpecTickVerdictsLaterListWins(t *testing.T) {
	t.Parallel()
	report := specDispatchReport{
		Skipped:  []specDispatchEntry{{Bead: "gt-a", Rig: "gastown", Line: "gt-a: skipped, no free seat"}},
		Deferred: []specDispatchEntry{{Bead: "gt-a", Rig: "gastown", Line: "gt-a: deferred, the seat started too slowly"}},
	}
	got := specTickVerdicts(report)
	if len(got) != 1 {
		t.Fatalf("entries = %+v, want one per bead", got)
	}
	if got[0].Class != specVerdictDeferred || got[0].Reason != report.Deferred[0].Line {
		t.Errorf("entry = %+v, want class %q and the deferred line %q", got[0], specVerdictDeferred, report.Deferred[0].Line)
	}
}

// verdictClasses is specTickVerdicts(report) as bead → class: the ledger a
// tick that decided what report decided should hold.
func verdictClasses(report specDispatchReport) map[string]string {
	out := map[string]string{}
	for _, e := range specTickVerdicts(report) {
		out[e.Bead] = e.Class
	}
	return out
}

// TestSpecDispatchAtRecordsTheTownsLedger runs the production tick, the one the
// daemon calls, over a town and reads the ledger back from that town's runtime
// directory: the town a tick records to is wired in runSpecDispatchAt, and a
// tick that lost that wiring would record nothing at all (gt-tcsnr).
func TestSpecDispatchAtRecordsTheTownsLedger(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, specTickSettings, specTickDaemon)
	// The operator's hold stops the tick before it reads the roster, so this
	// test needs no tmux server: the tick still returns through the defer that
	// records the ledger, which is the wiring under test.
	if err := os.WriteFile(dispatch.HoldFilePath(root), nil, 0o644); err != nil {
		t.Fatalf("planting the operator hold: %v", err)
	}
	if _, err := runSpecDispatchAt(root); err != nil {
		t.Fatalf("runSpecDispatchAt: %v", err)
	}
	if _, err := os.Stat(specdispatch.LedgerPath(root)); err != nil {
		t.Fatalf("the tick wrote no ledger under the town it ran for: %v", err)
	}
	l := readVerdictLedger(t, root)
	if l.SavedAt.IsZero() {
		t.Fatalf("the ledger under %s holds no tick", specdispatch.LedgerPath(root))
	}
}
