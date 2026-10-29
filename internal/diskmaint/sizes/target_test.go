package sizes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

var testNow = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

// fixture is a town and a home directory holding the paths the target lists,
// each dated by its age before testNow, plus two entries it must not list.
type fixture struct {
	town, home string
	doltBak    string
	trash      string
	backups    string
}

// aged is a path created with its mtime age before testNow.
type aged struct {
	path string
	age  time.Duration
}

// newFixture lays out the paths; withBackups false leaves gt-backups absent.
func newFixture(t *testing.T, withBackups bool) fixture {
	t.Helper()
	f := fixture{town: t.TempDir(), home: t.TempDir()}
	f.doltBak = filepath.Join(f.town, ".dolt-data.bak-1")
	f.trash = filepath.Join(f.town, ".dolt-remote-cache-trash-a")
	f.backups = filepath.Join(f.home, "gt-backups")
	dated := []aged{
		{f.doltBak, 4 * 24 * time.Hour},
		{f.trash, 10 * 24 * time.Hour},
		// Not listed: the live Dolt data directory, and a name no pattern matches.
		{filepath.Join(f.town, ".dolt-data"), 2 * 24 * time.Hour},
		{filepath.Join(f.town, "unrelated.bak-1"), 2 * 24 * time.Hour},
	}
	if withBackups {
		dated = append(dated, aged{f.backups, 24 * time.Hour})
	}
	for _, d := range dated {
		if err := os.Mkdir(d.path, 0o755); err != nil {
			t.Fatal(err)
		}
		when := testNow.Add(-d.age)
		if err := os.Chtimes(d.path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// fakeDu stands in for du -sk: kb maps a path to the block count it reports,
// and fail names the paths whose du exits non-zero.
type fakeDu struct {
	kb   map[string]string
	fail map[string]bool
}

func (f *fakeDu) run(_ context.Context, _ []string, name string, args ...string) ([]byte, error) {
	path := args[len(args)-1]
	if name != "du" || f.fail[path] {
		return nil, errors.New("du: failed")
	}
	out, ok := f.kb[path]
	if !ok {
		return nil, errors.New("du: no such file")
	}
	return []byte(out + "\t" + path + "\n"), nil
}

// du builds the fixture's du answers: 2048 KiB, 512 KiB and 1 KiB of blocks,
// with the named paths failing.
func (f fixture) du(fail ...string) *fakeDu {
	du := &fakeDu{
		kb: map[string]string{
			f.doltBak: "2048",
			f.trash:   "512",
			f.backups: "1",
		},
		fail: map[string]bool{},
	}
	for _, p := range fail {
		du.fail[p] = true
	}
	return du
}

func (f fixture) target(du *fakeDu, ledger *diskmaint.Ledger) *Target {
	return NewTarget(Config{
		Town:   f.town,
		Home:   f.home,
		Run:    du.run,
		Now:    func() time.Time { return testNow },
		Ledger: ledger,
	})
}

// keptRules maps each kept ref to its rule.
func keptRules(p diskmaint.Plan) map[string]string {
	out := map[string]string{}
	for _, k := range p.Kept {
		out[k.Ref] = k.Rule
	}
	return out
}

func TestNameIsSizes(t *testing.T) {
	t.Parallel()
	if got := NewTarget(Config{}).Name(); got != "sizes" {
		t.Fatalf("Name() = %q, want %q", got, "sizes")
	}
}

func TestPlanListsEveryMatchingPathAsKeptWithSizeAndAge(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	p, err := f.target(f.du(), nil).Plan(context.Background(), diskmaint.Policy{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if p.Refused != "" {
		t.Fatalf("Refused = %q, want empty", p.Refused)
	}
	if len(p.Actions) != 0 {
		t.Fatalf("Actions = %v, want none: this target never removes anything", p.Actions)
	}
	want := map[string]string{
		f.doltBak: "report-only: 2.0 MiB, 4d old, owner: operator",
		f.trash:   "report-only: 512.0 KiB, 10d old, owner: operator",
		f.backups: "report-only: 1.0 KiB, 1d old, owner: operator",
	}
	if got := keptRules(p); !reflect.DeepEqual(got, want) {
		t.Fatalf("kept = %v, want %v", got, want)
	}
}

func TestPlanShowsAnUnreadableSizeWithoutFailingThePlan(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	p, err := f.target(f.du(f.trash), nil).Plan(context.Background(), diskmaint.Policy{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if p.Refused != "" {
		t.Fatalf("Refused = %q, want the plan valid", p.Refused)
	}
	got := keptRules(p)
	if rule := got[f.trash]; rule != "report-only: unreadable, 10d old, owner: operator" {
		t.Fatalf("trash rule = %q, want the size named unreadable", rule)
	}
	if rule := got[f.doltBak]; !strings.HasPrefix(rule, "report-only: 2.0 MiB,") {
		t.Fatalf("dolt rule = %q, want its size still read", rule)
	}
}

func TestMeasureSumsEveryReadableSize(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	m, err := f.target(f.du(), nil).Measure(context.Background())
	if err != nil {
		t.Fatalf("Measure: %v", err)
	}
	const want = (2048 + 512 + 1) * 1024
	if m.Used != want {
		t.Fatalf("Used = %d, want %d", m.Used, want)
	}
}

func TestMeasureRefusesToTotalAPathItCannotRead(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	_, err := f.target(f.du(f.backups), nil).Measure(context.Background())
	if err == nil || !strings.Contains(err.Error(), f.backups) {
		t.Fatalf("Measure error = %v, want one naming %s", err, f.backups)
	}
}

func TestApplyDeletesNothingAndAppendsOneLedgerEntry(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	ledger := diskmaint.NewLedger(filepath.Join(t.TempDir(), "ledger.jsonl"), 0)
	tgt := f.target(f.du(), ledger)
	p, err := tgt.Plan(context.Background(), diskmaint.Policy{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	res, err := tgt.Apply(context.Background(), p)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Deferred == "" {
		t.Fatalf("Deferred is empty, want the report-only reason")
	}
	if len(res.Done) != 0 || len(res.Failed) != 0 || res.BytesFreed != 0 {
		t.Fatalf("Result = %+v, want nothing done, failed or freed", res)
	}
	for _, path := range []string{f.doltBak, f.trash, f.backups} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s was touched by apply: %v", path, err)
		}
	}

	entries, err := ledger.Tail(10)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ledger has %d entries, want exactly one", len(entries))
	}
	e := entries[0]
	if e.Target != "sizes" || e.Mode != "apply" || e.Planned != len(p.Kept) || e.Done != 0 || e.BytesFreed != 0 {
		t.Fatalf("ledger entry = %+v, want one apply line counting %d rows with nothing done", e, len(p.Kept))
	}
}

func TestPlanListsBackupsUnderATownWhoseNameHoldsGlobCharacters(t *testing.T) {
	t.Parallel()
	f := newFixture(t, false)
	town := filepath.Join(t.TempDir(), "town[1]*?")
	if err := os.Mkdir(town, 0o755); err != nil {
		t.Fatal(err)
	}
	bak := filepath.Join(town, ".dolt-data.bak-1")
	if err := os.Mkdir(bak, 0o755); err != nil {
		t.Fatal(err)
	}
	du := &fakeDu{kb: map[string]string{bak: "1"}, fail: map[string]bool{}}
	tgt := f.target(du, nil)
	tgt.cfg.Town = town
	p, err := tgt.Plan(context.Background(), diskmaint.Policy{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, ok := keptRules(p)[bak]; !ok {
		t.Fatalf("backup under a town named with glob characters was not listed: %v", p.Kept)
	}
}

func TestPlanAndMeasureRefuseAnEmptyTownOrHome(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	for _, tc := range []struct {
		name       string
		town, home string
	}{
		{name: "empty town", town: "", home: f.home},
		{name: "empty home", town: f.town, home: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tgt := f.target(f.du(), nil)
			tgt.cfg.Town, tgt.cfg.Home = tc.town, tc.home
			if _, err := tgt.Plan(context.Background(), diskmaint.Policy{}); err == nil {
				t.Fatal("Plan returned nil for a blank root, want a refusal")
			}
			if _, err := tgt.Measure(context.Background()); err == nil {
				t.Fatal("Measure returned nil for a blank root, want a refusal")
			}
		})
	}
}

func TestPlanFailsWhenTheTownCannotBeListed(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	// A town that is a regular file exists but cannot be listed as a directory.
	town := filepath.Join(t.TempDir(), "town")
	if err := os.WriteFile(town, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	tgt := f.target(f.du(), nil)
	tgt.cfg.Town = town
	if _, err := tgt.Plan(context.Background(), diskmaint.Policy{}); err == nil {
		t.Fatal("Plan returned nil for a town that cannot be listed, want an error")
	}
	if _, err := tgt.Measure(context.Background()); err == nil {
		t.Fatal("Measure returned nil for a town that cannot be listed, want an error")
	}
}

func TestPlanListsNoTownPathsWhenTheTownIsAbsent(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	tgt := f.target(f.du(), nil)
	tgt.cfg.Town = filepath.Join(t.TempDir(), "absent")
	p, err := tgt.Plan(context.Background(), diskmaint.Policy{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := keptRules(p); len(got) != 1 {
		t.Fatalf("kept = %v, want only gt-backups", got)
	}
}

func TestPlanShowsAGtBackupsStatFailureAsUnreadable(t *testing.T) {
	t.Parallel()
	f := newFixture(t, false)
	// A home that runs through a regular file: stat of gt-backups under it fails
	// with ENOTDIR, which is not absence, so the row is listed as unreadable.
	home := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(home, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	backups := filepath.Join(home, "gt-backups")
	tgt := f.target(f.du(), nil)
	tgt.cfg.Home = home
	p, err := tgt.Plan(context.Background(), diskmaint.Policy{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	rule, ok := keptRules(p)[backups]
	if !ok {
		t.Fatalf("gt-backups that cannot be stat'd was not listed: %v", p.Kept)
	}
	if want := "report-only: unreadable, age unreadable, owner: operator"; rule != want {
		t.Fatalf("gt-backups rule = %q, want %q", rule, want)
	}
}

func TestPlanShowsASizeThatIsNotACountAsUnreadable(t *testing.T) {
	t.Parallel()
	for _, out := range []string{"512M", "-4"} {
		f := newFixture(t, true)
		du := f.du()
		du.kb[f.trash] = out
		p, err := f.target(du, nil).Plan(context.Background(), diskmaint.Policy{})
		if err != nil {
			t.Fatalf("Plan with du output %q: %v", out, err)
		}
		if rule := keptRules(p)[f.trash]; rule != "report-only: unreadable, 10d old, owner: operator" {
			t.Fatalf("du output %q: trash rule = %q, want the size named unreadable", out, rule)
		}
	}
}

func TestPlanAndMeasureStopWhenTheContextIsCanceled(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tgt := f.target(f.du(), nil)
	if _, err := tgt.Plan(ctx, diskmaint.Policy{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Plan error = %v, want context.Canceled", err)
	}
	if _, err := tgt.Measure(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Measure error = %v, want context.Canceled", err)
	}
}

func TestApplyReportsALedgerThatCannotBeWritten(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	// A ledger path that is a directory cannot be read, so the append fails.
	tgt := f.target(f.du(), diskmaint.NewLedger(t.TempDir(), 0))
	if _, err := tgt.Apply(context.Background(), diskmaint.Plan{Target: "sizes"}); err == nil {
		t.Fatal("Apply returned nil with an unwritable ledger, want the record error")
	}
}

func TestApplyRefusesWithoutALedger(t *testing.T) {
	t.Parallel()
	f := newFixture(t, true)
	tgt := f.target(f.du(), nil)
	if _, err := tgt.Apply(context.Background(), diskmaint.Plan{Target: "sizes"}); err == nil {
		t.Fatal("Apply with no ledger returned nil, want a refusal")
	}
}

func TestMissingGtBackupsIsNotListed(t *testing.T) {
	t.Parallel()
	f := newFixture(t, false)
	p, err := f.target(f.du(), nil).Plan(context.Background(), diskmaint.Policy{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, ok := keptRules(p)[f.backups]; ok {
		t.Fatalf("absent gt-backups was listed: %v", p.Kept)
	}
}
