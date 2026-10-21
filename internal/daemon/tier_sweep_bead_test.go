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
	return lintTierSweepBeadAgainst(opts, specdispatch.Template{Sections: specdispatch.DefaultSections, Source: "built-in"})
}

// lintTierSweepBeadAgainst lints the bead against tmpl, the template the
// dispatcher would read for the operator's configured path.
func lintTierSweepBeadAgainst(opts beads.CreateOptions, tmpl specdispatch.Template) specdispatch.Verdict {
	return specdispatch.Lint(specdispatch.Spec{
		Title:       opts.Title,
		Labels:      opts.Labels,
		Description: opts.Description,
		Acceptance:  opts.Acceptance,
	}, tmpl)
}

// tierSweepWriteTemplate writes a spec template with the given "## " sections
// and points the daemon's patrol block at it, the way an operator's
// patrols.spec_dispatch.template does. It returns the template's path.
func tierSweepWriteTemplate(t *testing.T, d *Daemon, sections ...string) string {
	t.Helper()
	var body strings.Builder
	body.WriteString("# Spec template\n\n")
	for _, s := range sections {
		body.WriteString("## " + s + "\n\nThe operator's text.\n\n")
	}
	path := filepath.Join(t.TempDir(), "spec-template.md")
	if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SavePatrolConfig(d.config.TownRoot, &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		SpecDispatch: &SpecDispatchConfig{Template: path},
	}}); err != nil {
		t.Fatal(err)
	}
	return path
}

// The shape a sweep bead carries is the shape the dispatcher's configured
// template asks for: an operator who adds a section gets a bead that carries
// it, so the lint passes rather than holding the bead unshaped (gt-uzkic).
func TestTierSweepBeadShapeUsesTheConfiguredTemplate(t *testing.T) {
	t.Parallel()
	logPath := tierSweepWriteTestLog(t, ""+
		"tier-sweep: integration ./internal/cmd ...\n"+
		"    --- FAIL: TestCommandTreeWalkIsReadOnly (0.12s)\n")
	d, _, bd, _ := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	d.tierSweepSeams.beads = func(string) tierSweepBeadStore { return bd }
	tmplPath := tierSweepWriteTemplate(t, d, "Goal", "Constraints", "Out of scope", "Gate", "Size", "Testing")

	red := map[string]tierSweepTierResult{
		"integration": {Verdict: tierSweepRed, Passed: 32, Failed: 1, FailedNames: []string{"./internal/cmd"}, Log: logPath},
	}
	d.tierSweepRecordBeads("gastown", "abc1234", red)

	if len(bd.created) != 1 {
		t.Fatalf("created = %+v, want one bead", bd.created)
	}
	opts := bd.created[0]
	tmpl := specdispatch.LoadTemplate(tmplPath)
	if v := lintTierSweepBeadAgainst(opts, tmpl); !v.Clean() {
		t.Fatalf("the bead fails the configured template's lint: %s\ndescription:\n%s", v.ShapeNote(), opts.Description)
	}
	if !strings.Contains(opts.Description, "## Testing\n") {
		t.Errorf("description lacks the template's ## Testing section:\n%s", opts.Description)
	}
}

// A configured template that cannot be read is no reason to hold the sweep:
// the bead falls back to the built-in five sections, and that shape still
// passes the built-in lint.
func TestTierSweepBeadShapeFallsBackWhenTheTemplateIsMissing(t *testing.T) {
	t.Parallel()
	logPath := tierSweepWriteTestLog(t, ""+
		"tier-sweep: integration ./internal/cmd ...\n"+
		"    --- FAIL: TestCommandTreeWalkIsReadOnly (0.12s)\n")
	d, _, bd, _ := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	d.tierSweepSeams.beads = func(string) tierSweepBeadStore { return bd }
	if err := SavePatrolConfig(d.config.TownRoot, &DaemonPatrolConfig{Patrols: &PatrolsConfig{
		SpecDispatch: &SpecDispatchConfig{Template: filepath.Join(t.TempDir(), "absent.md")},
	}}); err != nil {
		t.Fatal(err)
	}

	red := map[string]tierSweepTierResult{
		"integration": {Verdict: tierSweepRed, Passed: 32, Failed: 1, FailedNames: []string{"./internal/cmd"}, Log: logPath},
	}
	d.tierSweepRecordBeads("gastown", "abc1234", red)

	if len(bd.created) != 1 {
		t.Fatalf("created = %+v, want one bead", bd.created)
	}
	if v := lintTierSweepBead(bd.created[0]); !v.Clean() {
		t.Fatalf("the fallback bead fails the built-in lint: %s\ndescription:\n%s", v.ShapeNote(), bd.created[0].Description)
	}
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

// The integration stage runs its docker list untagged after the tagged one,
// so a bead for a package that failed there re-runs it without -tags: the tag
// would compile a different file set than the run that failed.
func TestTierSweepBead_UntaggedDockerUnitKeepsNoBuildTag(t *testing.T) {
	t.Parallel()
	logPath := tierSweepWriteTestLog(t, ""+
		"tier-sweep: integration-docker ./internal/docker ...\n"+
		"    --- FAIL: TestDockerRun (0.01s)\n")
	d, _, bd, _ := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	d.tierSweepSeams.beads = func(string) tierSweepBeadStore { return bd }

	red := map[string]tierSweepTierResult{
		"integration": {Verdict: tierSweepRed, Failed: 1, FailedNames: []string{"./internal/docker"}, Log: logPath},
	}
	d.tierSweepRecordBeads("gastown", "abc1234", red)

	if len(bd.created) != 1 {
		t.Fatalf("created = %+v, want one bead", bd.created)
	}
	acceptance := bd.created[0].Acceptance
	if want := "go test -run '^(TestDockerRun)$' -count=1 ./internal/docker"; !strings.Contains(acceptance, want) {
		t.Errorf("acceptance = %q, want it to carry %q", acceptance, want)
	}
	if strings.Contains(acceptance, "-tags") {
		t.Errorf("acceptance = %q, want no build tag for the untagged docker run", acceptance)
	}
}

// A missing or unreadable log still files a shaped bead: the fallback item
// asks the polecat to read the log and re-run the failing test, and the shape
// is what the dispatcher needs. Filing must not depend on the log surviving.
func TestTierSweepBead_MissingLogFallsBack(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, log string }{
		{name: "missing file", log: filepath.Join(t.TempDir(), "gone.log")},
		{name: "no log path", log: ""},
		// A fail line with no unit marker above it belongs to no unit: the
		// old whole-log read handed it to every bead, and the -run filter then
		// matched nothing in that package and passed without running a test.
		{name: "no unit markers", log: tierSweepWriteTestLog(t, "    --- FAIL: TestOrphan (0.01s)\n")},
		// Another unit's marker opened the failure; this unit has none.
		{name: "failure belongs to another unit", log: tierSweepWriteTestLog(t, ""+
			"tier-sweep: integration ./internal/other ...\n"+
			"    --- FAIL: TestOtherPackage (0.01s)\n")},
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
			// Every command the shape can invent starts with this; the fallback
			// text mentions no test command at all.
			if strings.Contains(opts.Acceptance, "go test") {
				t.Errorf("acceptance = %q, want no invented command", opts.Acceptance)
			}
			if strings.Contains(opts.Acceptance, "TestOtherPackage") {
				t.Errorf("acceptance = %q, want no other unit's test", opts.Acceptance)
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

// When the sweep could not name a unit it files the reason instead ("no
// verdict line (exit 1)"). That reason is its own explanation, not a package
// or a script, so the bead's prose names the tier: a polecat reading "Fix the
// failure in no verdict line (exit 1)" learns nothing it can act on.
func TestTierSweepBead_StandInUnitNamesTheTier(t *testing.T) {
	t.Parallel()
	for _, unit := range []string{tierSweepNoName, "no verdict line (exit 1)", "no verdict line", "timeout"} {
		t.Run(unit, func(t *testing.T) {
			t.Parallel()
			description, acceptance := tierSweepBeadShape("integration", unit, "abc1234",
				tierSweepTierResult{Verdict: tierSweepRed, Passed: 0, Failed: 1, Log: "/tmp/integration.log"},
				specdispatch.Template{Sections: specdispatch.DefaultSections, Source: "built-in"})
			opts := beads.CreateOptions{
				Title:       "tier sweep (gastown): integration: " + unit,
				Labels:      []string{tierSweepRedLabel},
				Description: description,
				Acceptance:  acceptance,
			}
			if v := lintTierSweepBead(opts); !v.Clean() {
				t.Fatalf("the stand-in bead is unshaped: %s\ndescription:\n%s", v.ShapeNote(), description)
			}
			if strings.Contains(description, unit) {
				t.Errorf("description quotes the stand-in %q as a unit:\n%s", unit, description)
			}
			if !strings.Contains(description, "the integration tier") {
				t.Errorf("description does not name the tier in place of the stand-in:\n%s", description)
			}
			if !strings.Contains(acceptance, "the integration tier") {
				t.Errorf("acceptance = %q, want it to name the tier", acceptance)
			}
			if strings.Contains(acceptance, "go test") {
				t.Errorf("acceptance = %q, want no invented command for a stand-in", acceptance)
			}
		})
	}
}

// The shape reads a bounded prefix of the log: a file that has grown past the
// limit names nothing beyond it and the bead falls back, rather than the
// daemon reading an unbounded file to learn the same.
func TestTierSweepBead_LogReadIsBounded(t *testing.T) {
	t.Parallel()
	pad := strings.Repeat("# filler\n", tierSweepLogReadLimit/len("# filler\n")+1)
	logPath := tierSweepWriteTestLog(t, pad+
		"tier-sweep: integration ./internal/cmd ...\n"+
		"    --- FAIL: TestPastTheLimit (0.01s)\n")
	d, _, bd, _ := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	d.tierSweepSeams.beads = func(string) tierSweepBeadStore { return bd }

	red := map[string]tierSweepTierResult{
		"integration": {Verdict: tierSweepRed, Failed: 1, FailedNames: []string{"./internal/cmd"}, Log: logPath},
	}
	d.tierSweepRecordBeads("gastown", "abc1234", red)

	if len(bd.created) != 1 {
		t.Fatalf("created = %+v, want one bead", bd.created)
	}
	opts := bd.created[0]
	if v := lintTierSweepBead(opts); !v.Clean() {
		t.Fatalf("the bounded-read bead is unshaped: %s\ndescription:\n%s", v.ShapeNote(), opts.Description)
	}
	if strings.Contains(opts.Acceptance, "go test") {
		t.Errorf("acceptance = %q, want the fallback for a failure past the read limit", opts.Acceptance)
	}
}

// A read limit that lands mid fail line must not keep the half it read: a
// filter naming a truncated test matches nothing and passes without running
// anything, which is the failure the unit attribution exists to prevent.
func TestTierSweepBead_BoundedReadKeepsNoPartialLine(t *testing.T) {
	t.Parallel()
	const prefix = "tier-sweep: integration ./internal/cmd ...\n"
	const failLine = "    --- FAIL: TestStraddlesTheLimit (0.01s)\n"
	// Cut the fail line well past its marker, so the piece the limit keeps
	// reads as a complete FAIL line for a name cut short.
	pad := strings.Repeat("#", tierSweepLogReadLimit-len(prefix)-len(failLine)/2-7)
	logPath := tierSweepWriteTestLog(t, prefix+pad+"\n"+failLine)

	names, tag := tierSweepUnitFailures(logPath, "integration", "./internal/cmd")
	if len(names) != 0 || tag != "" {
		t.Fatalf("failures = %v (tag %q), want none from a read cut mid fail line", names, tag)
	}
}
