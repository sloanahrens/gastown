package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// tierSweepWriteTestLog writes a tier log the sweep could have written and
// returns its path.
func tierSweepWriteTestLog(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "integration.log")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// lintTierSweepBead is the same check `gt spec lint` runs: the built-in
// template's sections, the acceptance items and the Size.
func lintTierSweepBead(opts beads.CreateOptions) specdispatch.Verdict {
	return specdispatch.Lint(specdispatch.Spec{
		Title:       opts.Title,
		Labels:      opts.Labels,
		Description: opts.Description,
		Acceptance:  opts.Acceptance,
	}, specdispatch.Template{Sections: specdispatch.DefaultSections, Source: "built-in"})
}

// A red-tier bead is born shaped: its description carries every section the
// dispatcher's lint reads, and the acceptance field holds the same items the
// description does, because gt done reads the field while the dispatcher lints
// the description (gt-1lzy1).
func TestTierSweepBead_BornShaped(t *testing.T) {
	t.Parallel()
	logPath := tierSweepWriteTestLog(t, ""+
		"tier-sweep: integration ./internal/cmd ...\n"+
		"    --- FAIL: TestCommandTreeWalkIsReadOnly (0.12s)\n")
	d, _, bd, _ := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	d.tierSweepSeams.beads = func(string) tierSweepBeadStore { return bd }

	red := map[string]tierSweepTierResult{
		"integration": {Verdict: tierSweepRed, Passed: 32, Failed: 1, FailedNames: []string{"./internal/cmd"}, Log: logPath},
	}
	d.tierSweepRecordBeads("gastown", "abc12345678", red)

	if len(bd.created) != 1 {
		t.Fatalf("created = %+v, want one bead for the failing package", bd.created)
	}
	opts := bd.created[0]
	if v := lintTierSweepBead(opts); !v.Clean() {
		t.Fatalf("the filed bead fails the spec lint: %s\ndescription:\n%s", v.ShapeNote(), opts.Description)
	}
	for _, heading := range []string{"## Goal", "## Constraints", "## Out of scope", "## Gate", "## Size", "## Acceptance"} {
		if !strings.Contains(opts.Description, heading+"\n") {
			t.Errorf("description lacks the %q section:\n%s", heading, opts.Description)
		}
	}
	// The one-line detail the sweep filed before the shape stays the first
	// paragraph, so the record still reads the way it did.
	if !strings.HasPrefix(opts.Description, "The daemon's tier sweep found this red on main (gt-vsct7.5). integration tier RED on main at abc12345 (32 passed, 1 failed). Log: "+logPath) {
		t.Errorf("description does not open with the one-line detail:\n%s", opts.Description)
	}
	// The field and the description carry the same items: the dispatcher lints
	// one and gt done reads the other.
	if got := specdispatch.Sections(opts.Description)["acceptance"]; got != opts.Acceptance {
		t.Errorf("acceptance field and description disagree:\nfield: %q\nsection: %q", opts.Acceptance, got)
	}
	if got := specdispatch.CountAcceptance(opts.Acceptance); got != 1 {
		t.Errorf("acceptance items = %d, want one check a polecat can tick", got)
	}
}

// The acceptance item names the failing tests the log records, in one narrow
// -run filter the polecat test guard allows, with the tier's build tag.
func TestTierSweepBead_NamesTheFailingTests(t *testing.T) {
	t.Parallel()
	logPath := tierSweepWriteTestLog(t, ""+
		"tier-sweep: integration ./internal/cmd ...\n"+
		"    --- FAIL: TestAlpha (0.01s)\n"+
		"    --- FAIL: TestBeta/sub (0.02s)\n"+
		"    --- FAIL: TestAlpha (0.01s)\n")
	d, _, bd, _ := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	d.tierSweepSeams.beads = func(string) tierSweepBeadStore { return bd }

	red := map[string]tierSweepTierResult{
		"integration": {Verdict: tierSweepRed, Passed: 2, Failed: 2, FailedNames: []string{"./internal/cmd"}, Log: logPath},
	}
	d.tierSweepRecordBeads("gastown", "abc1234", red)

	if len(bd.created) != 1 {
		t.Fatalf("created = %+v, want one bead", bd.created)
	}
	// The container opt-in and the slot wrapper are the sweep's own, and
	// without them the command either skips the container tests or is refused
	// by the container-suite guard.
	want := "`gt slot run -- env GT_TEST_DOCKER=1 go test -tags integration -run '^(TestAlpha|TestBeta)$' -count=1 ./internal/cmd` passes"
	if !strings.Contains(bd.created[0].Acceptance, want) {
		t.Errorf("acceptance = %q, want it to carry %q", bd.created[0].Acceptance, want)
	}
	if strings.Contains(bd.created[0].Acceptance, "-run '^(TestAlpha|TestAlpha") {
		t.Errorf("acceptance names TestAlpha twice: %q", bd.created[0].Acceptance)
	}
}

// The integration stage runs its tagged packages and its untagged docker ones
// under the one daemon log, and the race tier shares that log too. A bead names
// only the tests its own tier's run of its own unit failed, so a polecat
// re-runs the failure it was handed.
func TestTierSweepBead_ReadsOnlyItsOwnUnitsFailures(t *testing.T) {
	t.Parallel()
	logPath := tierSweepWriteTestLog(t, ""+
		"tier-sweep: integration ./internal/cmd ...\n"+
		"    --- FAIL: TestIntegrationOnly (0.01s)\n"+
		"tier-sweep: integration ./internal/other ...\n"+
		"    --- FAIL: TestOtherPackage (0.01s)\n"+
		"tier-sweep: race ./internal/cmd ...\n"+
		"    --- FAIL: TestRaceOnly (0.01s)\n")
	d, _, bd, _ := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	d.tierSweepSeams.beads = func(string) tierSweepBeadStore { return bd }

	red := map[string]tierSweepTierResult{
		"integration": {Verdict: tierSweepRed, Failed: 2, FailedNames: []string{"./internal/cmd", "./internal/other"}, Log: logPath},
		"race":        {Verdict: tierSweepRed, Failed: 1, FailedNames: []string{"./internal/cmd"}, Log: logPath},
	}
	d.tierSweepRecordBeads("gastown", "abc1234", red)

	byTitle := map[string]string{}
	for _, c := range bd.created {
		byTitle[c.Title] = c.Acceptance
	}
	cases := map[string]string{
		"tier sweep (gastown): integration: ./internal/cmd":   "TestIntegrationOnly",
		"tier sweep (gastown): integration: ./internal/other": "TestOtherPackage",
		"tier sweep (gastown): race: ./internal/cmd":          "TestRaceOnly",
	}
	for title, want := range cases {
		acceptance, ok := byTitle[title]
		if !ok {
			t.Errorf("no bead titled %q; got %v", title, byTitle)
			continue
		}
		if !strings.Contains(acceptance, want) {
			t.Errorf("%q does not name %s: %q", title, want, acceptance)
		}
		// The failures of the other units and the other tier are not this
		// bead's.
		for _, other := range []string{"TestIntegrationOnly", "TestOtherPackage", "TestRaceOnly"} {
			if other == want {
				continue
			}
			if strings.Contains(acceptance, other) {
				t.Errorf("%q carries %s, which is not its own failure: %q", title, other, acceptance)
			}
		}
	}
	if got := byTitle["tier sweep (gastown): race: ./internal/cmd"]; !strings.Contains(got, "go test -race -tags integration") {
		t.Errorf("the race tier's re-run dropped -race: %q", got)
	}
}

// A missing or unreadable log still files a shaped bead: the fallback item
// asks the polecat to read the log and re-run the failing test, and the shape
// is what the dispatcher needs. Filing must not depend on the log surviving.
func TestTierSweepBead_MissingLogFallsBack(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, log string }{
		{name: "missing file", log: filepath.Join(t.TempDir(), "gone.log")},
		{name: "no failing test named", log: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d, _, bd, _ := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
			d.tierSweepSeams.beads = func(string) tierSweepBeadStore { return bd }

			red := map[string]tierSweepTierResult{
				"integration": {Verdict: tierSweepRed, Passed: 1, Failed: 1, FailedNames: []string{"./internal/cmd"}, Log: tc.log},
			}
			d.tierSweepRecordBeads("gastown", "abc1234", red)

			if len(bd.created) != 1 {
				t.Fatalf("created = %+v, want a bead filed with no readable log", bd.created)
			}
			opts := bd.created[0]
			if v := lintTierSweepBead(opts); !v.Clean() {
				t.Errorf("the fallback bead is unshaped: %s\ndescription:\n%s", v.ShapeNote(), opts.Description)
			}
			if !strings.Contains(opts.Acceptance, "Read the tier log") || !strings.Contains(opts.Acceptance, "narrow -run filter") {
				t.Errorf("acceptance = %q, want the fallback item", opts.Acceptance)
			}
			if strings.Contains(opts.Acceptance, "go test -run") {
				t.Errorf("acceptance = %q, want no invented command", opts.Acceptance)
			}
		})
	}
}

// The shell tier's unit is a script, so its check is the script the sweep ran,
// not a go test filter.
func TestTierSweepBead_ShellTierRunsTheScript(t *testing.T) {
	t.Parallel()
	d, _, bd, _ := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	d.tierSweepSeams.beads = func(string) tierSweepBeadStore { return bd }

	red := map[string]tierSweepTierResult{
		"shell": {Verdict: tierSweepRed, Passed: 5, Failed: 1, FailedNames: []string{"scripts/test-makefile.sh"}},
	}
	d.tierSweepRecordBeads("gastown", "abc1234", red)

	if len(bd.created) != 1 {
		t.Fatalf("created = %+v, want one bead", bd.created)
	}
	opts := bd.created[0]
	if v := lintTierSweepBead(opts); !v.Clean() {
		t.Fatalf("the shell bead is unshaped: %s\ndescription:\n%s", v.ShapeNote(), opts.Description)
	}
	if want := "`GT_TEST_DOCKER=0 bash scripts/test-makefile.sh` passes"; !strings.Contains(opts.Acceptance, want) {
		t.Errorf("acceptance = %q, want it to carry %q", opts.Acceptance, want)
	}
}
