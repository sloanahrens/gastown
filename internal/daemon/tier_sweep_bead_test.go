package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

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

// lintTierSweepBead is the shape check `gt spec lint` runs: the built-in
// template's sections, the acceptance items and the Size. It is LintShape, the
// check the sweep itself asks: the target-path half needs a rig's repo, which
// the sweep's text is not written against here (gt-7t1im).
func lintTierSweepBead(opts beads.CreateOptions) specdispatch.LintVerdict {
	return lintTierSweepBeadAgainst(opts, specdispatch.Template{Sections: specdispatch.DefaultSections, Source: "built-in"})
}

// lintTierSweepBeadAgainst lints the bead against tmpl, the template the
// dispatcher would read for the operator's configured path.
func lintTierSweepBeadAgainst(opts beads.CreateOptions, tmpl specdispatch.Template) specdispatch.LintVerdict {
	return specdispatch.LintShape(specdispatch.Spec{
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
	// A section the sweep does not know still says what it covers, so it
	// reads as the bead's own text rather than a stock placeholder.
	testingBody := tierSweepSectionText(opts.Description, "Testing")
	if !strings.Contains(testingBody, "./internal/cmd") {
		t.Errorf("## Testing body = %q, want it to name the failing package", testingBody)
	}
}

// tierSweepSectionText returns the body under one "## " heading of desc.
func tierSweepSectionText(desc, heading string) string {
	return specdispatch.Sections(desc)[strings.ToLower(heading)]
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

// A patrol block that does not parse is the other read that can fail. The
// tick stops on that block, so the sweep only has to file a bead that passes
// the built-in lint and does not panic or drop the filing (gt-uzkic).
func TestTierSweepBeadShapeFallsBackWhenThePatrolBlockIsUnreadable(t *testing.T) {
	t.Parallel()
	logPath := tierSweepWriteTestLog(t, ""+
		"tier-sweep: integration ./internal/cmd ...\n"+
		"    --- FAIL: TestCommandTreeWalkIsReadOnly (0.12s)\n")
	d, _, bd, _ := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	d.tierSweepSeams.beads = func(string) tierSweepBeadStore { return bd }
	if err := os.WriteFile(PatrolConfigFile(d.config.TownRoot), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPatrolConfig(d.config.TownRoot); err == nil {
		t.Fatal("ReadPatrolConfig accepted the unparseable patrol block; the test does not reach the unreadable branch")
	}

	red := map[string]tierSweepTierResult{
		"integration": {Verdict: tierSweepRed, Passed: 32, Failed: 1, FailedNames: []string{"./internal/cmd"}, Log: logPath},
	}
	d.tierSweepRecordBeads("gastown", "abc1234", red)

	if len(bd.created) != 1 {
		t.Fatalf("created = %+v, want one bead", bd.created)
	}
	opts := bd.created[0]
	if v := lintTierSweepBead(opts); !v.Clean() {
		t.Fatalf("the bead fails the built-in lint: %s\ndescription:\n%s", v.ShapeNote(), opts.Description)
	}
	if got := specdispatch.Sections(opts.Description)["acceptance"]; got != opts.Acceptance {
		t.Errorf("acceptance field and description disagree:\nfield: %q\nsection: %q", opts.Acceptance, got)
	}
}

// A finding is read on its marker alone: the line slowcheck wrote is kept
// whole whatever its spelling, because the bead quotes it, while a -run filter
// is named only when the token after the marker is a test's name. A line that
// departs from the spelling the host log showed still reaches the bead, so a
// format this daemon cannot check against slowcheck's writer never costs the
// polecat the evidence; a token that is not a test name yields no filter, since
// a filter that matches nothing passes without running a test (gt-j6vlz).
func TestTierSweepSlowcheckFinding(t *testing.T) {
	t.Parallel()
	// The line hq-in8pg quotes from the host's sweep log.
	const pkgLine = "slowcheck: TestHostRehearsal 42.5s in gt.local/devops/tests/host-rehearsal is over the 30s integration cap and is not in internal/testpolicy/slow.txt: the test waits on a container"
	if got, ok := tierSweepFindingLine(pkgLine); !ok || got != pkgLine {
		t.Fatalf("finding line = (%q, %v), want the line whole", got, ok)
	}
	if name, ok := tierSweepFindingTest(pkgLine); !ok || name != "TestHostRehearsal" {
		t.Errorf("test = (%q, %v), want the test the line names", name, ok)
	}
	if name, ok := tierSweepFindingTest("slowcheck: TestHostRehearsal/sub 42.5s in gt.local/devops/tests/host-rehearsal: it waits"); !ok || name != "TestHostRehearsal" {
		t.Errorf("subtest test = (%q, %v), want the top-level test", name, ok)
	}
	// A line in another spelling is still a finding for this daemon: it is
	// quoted, and its test is named when the marker opens with one.
	for _, line := range []string{
		"slowcheck: TestHostRehearsal is over the cap",
		"slowcheck: TestHostRehearsal 2m5.5s some other tail",
		"  slowcheck: TestHostRehearsal built a container and gave up\n",
	} {
		if _, ok := tierSweepFindingLine(line); !ok {
			t.Errorf("%q is not read as a finding, want it kept", line)
		}
		if name, ok := tierSweepFindingTest(line); !ok || name != "TestHostRehearsal" {
			t.Errorf("%q test = (%q, %v), want the test", line, name, ok)
		}
	}
	// A marker line whose first token names no test yields no name, so no
	// filter is invented from it.
	for _, line := range []string{
		"slowcheck:",
		"slowcheck: 42.5s in gt.local/devops/tests/host-rehearsal over the cap",
		"slowcheck: TestMain 40s in gt.local/devops/tests/host-rehearsal",
		"slowcheck: TESThelper does nothing",
	} {
		if _, ok := tierSweepFindingTest(line); ok {
			t.Errorf("%q named a test, want no filter from a token that is no test name", line)
		}
	}
	// A line without the marker is no finding.
	for _, line := range []string{
		"--- FAIL: TestHostRehearsal (0.1s)",
		"",
		"go: downloading gt.local/devops",
	} {
		if got, ok := tierSweepFindingLine(line); ok {
			t.Errorf("%q read as a finding line (%q), want it ignored", line, got)
		}
	}
	// The unit log is the attribution: the script writes one unit's own output
	// per file, so every marker line in it is that unit's finding, whatever
	// package path it spells, and repeats are quoted once.
	log := pkgLine + "\n" + pkgLine + "\nslowcheck: TestOther 30s in some/other/pkg\n"
	if got := tierSweepSlowcheckFindings(log); len(got) != 2 {
		t.Errorf("findings = %v, want the two distinct lines of the log", got)
	}
	if got := tierSweepSlowcheckFindings(""); len(got) != 0 {
		t.Errorf("findings = %v, want none from an empty log", got)
	}
}

// A cap trip writes no FAIL line: slowcheck's finding in the unit's own log is
// the only record of it. The bead names the test in a narrow -run filter and
// quotes the finding inline, so a polecat that cannot open the host's log still
// knows a cap trip from a real failure (gt-j6vlz).
func TestTierSweepBead_NamesASlowcheckOnlyRed(t *testing.T) {
	t.Parallel()
	const finding = "slowcheck: TestIntegrationX 42.5s in gt.local/devops/tests/host-rehearsal is over the 30s integration cap and is not in internal/testpolicy/slow.txt: it waits"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "integration-tests_host-rehearsal.log"), []byte(finding+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stageLog := tierSweepWriteTestLog(t, "tier-sweep: integration ./tests/host-rehearsal ...\n")
	d, _, bd, _ := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	d.tierSweepSeams.beads = func(string) tierSweepBeadStore { return bd }

	red := map[string]tierSweepTierResult{
		"integration": {Verdict: tierSweepRed, Passed: 9, Failed: 1, FailedNames: []string{"./tests/host-rehearsal"}, Log: stageLog, LogDir: dir},
	}
	d.tierSweepRecordBeads("gastown", "abc1234", red)

	if len(bd.created) != 1 {
		t.Fatalf("created = %+v, want one bead", bd.created)
	}
	opts := bd.created[0]
	if v := lintTierSweepBead(opts); !v.Clean() {
		t.Fatalf("the slowcheck bead is unshaped: %s\ndescription:\n%s", v.ShapeNote(), opts.Description)
	}
	want := "`gt slot run -- env GT_TEST_DOCKER=1 go test -tags integration -run '^(TestIntegrationX)$' -count=1 ./tests/host-rehearsal` passes"
	if !strings.Contains(opts.Acceptance, want) {
		t.Errorf("acceptance = %q, want it to carry %q", opts.Acceptance, want)
	}
	if got := specdispatch.Sections(opts.Description)["acceptance"]; got != opts.Acceptance {
		t.Errorf("acceptance field and description disagree:\nfield: %q\nsection: %q", opts.Acceptance, got)
	}
	if !strings.Contains(opts.Description, finding) {
		t.Errorf("description does not quote the finding inline:\n%s", opts.Description)
	}
}

// The bead reads a finding on slowcheck's marker alone, so the line reaches it
// whatever spelling follows: each case below is quoted inline, and the filter
// names the test the marker opens with. slowcheck's source is in no repo of
// this sandbox, so a line in a spelling the daemon has not seen must still reach
// the polecat. A line whose token is no test name is quoted without a filter,
// because a filter over a name that does not exist passes without running
// anything (gt-j6vlz).
func TestTierSweepBead_QuotingDoesNotDependOnTheLineShape(t *testing.T) {
	t.Parallel()
	const pkg = "./tests/host-rehearsal"
	for _, tc := range []struct {
		name    string
		finding string
		filter  bool
	}{
		{
			name:    "the line the host log showed",
			finding: "slowcheck: TestIntegrationX 42.5s in gt.local/devops/tests/host-rehearsal is over the 30s integration cap and is not in internal/testpolicy/slow.txt: it waits",
			filter:  true,
		},
		{
			name:    "a shorter tail",
			finding: "slowcheck: TestIntegrationX over the cap at 42.5s",
			filter:  true,
		},
		{
			name:    "a tail in another sentence",
			finding: "slowcheck: TestIntegrationX took too long, so the harness gave up",
			filter:  true,
		},
		{
			name:    "no test name to read",
			finding: "slowcheck: the harness gave up before it named one",
			filter:  false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "integration-tests_host-rehearsal.log"), []byte(tc.finding+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			stageLog := tierSweepWriteTestLog(t, "tier-sweep: integration "+pkg+" ...\n")
			d, _, bd, _ := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
			d.tierSweepSeams.beads = func(string) tierSweepBeadStore { return bd }

			red := map[string]tierSweepTierResult{
				"integration": {Verdict: tierSweepRed, Passed: 9, Failed: 1, FailedNames: []string{pkg}, Log: stageLog, LogDir: dir},
			}
			d.tierSweepRecordBeads("gastown", "abc1234", red)

			if len(bd.created) != 1 {
				t.Fatalf("created = %+v, want one bead", bd.created)
			}
			opts := bd.created[0]
			if v := lintTierSweepBead(opts); !v.Clean() {
				t.Fatalf("the bead is unshaped: %s\ndescription:\n%s", v.ShapeNote(), opts.Description)
			}
			if !strings.Contains(opts.Description, tc.finding) {
				t.Errorf("description does not quote the finding:\n%s", opts.Description)
			}
			filter := "go test -tags integration -run '^(TestIntegrationX)$' -count=1 " + pkg
			if got := strings.Contains(opts.Acceptance, filter); got != tc.filter {
				t.Errorf("acceptance carries the filter = %v, want %v: %q", got, tc.filter, opts.Acceptance)
			}
			if !tc.filter && !strings.Contains(opts.Acceptance, "Read the tier log") {
				t.Errorf("acceptance = %q, want the fallback for a finding that names no test", opts.Acceptance)
			}
		})
	}
}

// A unit can fail both ways: a FAIL line in the stage log and a slowcheck
// finding in its own log. The filter names each test once, and a test only a
// finding names is added rather than the finding being dropped (gt-j6vlz).
func TestTierSweepBead_FailAndFindingNameEachOnce(t *testing.T) {
	t.Parallel()
	const pkg = "./internal/cmd"
	dir := t.TempDir()
	unitLog := "" +
		"slowcheck: TestAlpha 31.0s in github.com/steveyegge/gastown/internal/cmd is over the 30s integration cap and is not in internal/testpolicy/slow.txt: the test builds a container\n" +
		"slowcheck: TestGamma 33.0s in github.com/steveyegge/gastown/internal/cmd is over the 30s integration cap and is not in internal/testpolicy/slow.txt: the test builds a container\n"
	if err := os.WriteFile(filepath.Join(dir, "integration-internal_cmd.log"), []byte(unitLog), 0o600); err != nil {
		t.Fatal(err)
	}
	stageLog := tierSweepWriteTestLog(t, ""+
		"tier-sweep: integration "+pkg+" ...\n"+
		"    --- FAIL: TestAlpha (0.12s)\n")
	d, _, bd, _ := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	d.tierSweepSeams.beads = func(string) tierSweepBeadStore { return bd }

	red := map[string]tierSweepTierResult{
		"integration": {Verdict: tierSweepRed, Passed: 9, Failed: 1, FailedNames: []string{pkg}, Log: stageLog, LogDir: dir},
	}
	d.tierSweepRecordBeads("gastown", "abc1234", red)

	if len(bd.created) != 1 {
		t.Fatalf("created = %+v, want one bead", bd.created)
	}
	acceptance := bd.created[0].Acceptance
	want := "`gt slot run -- env GT_TEST_DOCKER=1 go test -tags integration -run '^(TestAlpha|TestGamma)$' -count=1 " + pkg + "` passes"
	if !strings.Contains(acceptance, want) {
		t.Errorf("acceptance = %q, want it to carry %q", acceptance, want)
	}
	if got := strings.Count(acceptance, "TestAlpha"); got != 1 {
		t.Errorf("acceptance names TestAlpha %d times, want once: %q", got, acceptance)
	}
}

// Filing a bead never depends on the unit log still existing: a missing
// directory, a missing unit log and a log past the read limit all leave the
// fallback acceptance, a bounded description and a shaped bead (gt-j6vlz).
func TestTierSweepBead_UnreadableUnitLogFallsBack(t *testing.T) {
	t.Parallel()
	const finding = "slowcheck: TestIntegrationX 42.5s in gt.local/devops/tests/host-rehearsal is over the 30s integration cap and is not in internal/testpolicy/slow.txt: the test waits on a container\n"
	oversized := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		pad := strings.Repeat("# filler\n", tierSweepLogReadLimit/len("# filler\n")+1)
		if err := os.WriteFile(filepath.Join(dir, "integration-internal_cmd.log"), []byte(pad+finding), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	for _, tc := range []struct {
		name   string
		logDir string
	}{
		{name: "missing directory", logDir: filepath.Join(t.TempDir(), "gone")},
		{name: "missing unit log", logDir: t.TempDir()},
		{name: "unit log past the read limit", logDir: oversized(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stageLog := tierSweepWriteTestLog(t, "tier-sweep: integration ./internal/cmd ...\n")
			d, _, bd, _ := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
			d.tierSweepSeams.beads = func(string) tierSweepBeadStore { return bd }

			red := map[string]tierSweepTierResult{
				"integration": {Verdict: tierSweepRed, Passed: 1, Failed: 1, FailedNames: []string{"./internal/cmd"}, Log: stageLog, LogDir: tc.logDir},
			}
			d.tierSweepRecordBeads("gastown", "abc1234", red)

			if len(bd.created) != 1 {
				t.Fatalf("created = %+v, want a bead filed with no readable unit log", bd.created)
			}
			opts := bd.created[0]
			if v := lintTierSweepBead(opts); !v.Clean() {
				t.Errorf("the fallback bead is unshaped: %s\ndescription:\n%s", v.ShapeNote(), opts.Description)
			}
			if !strings.Contains(opts.Acceptance, "Read the tier log") || !strings.Contains(opts.Acceptance, "narrow -run filter") {
				t.Errorf("acceptance = %q, want the fallback item", opts.Acceptance)
			}
			if strings.Contains(opts.Acceptance, "go test") {
				t.Errorf("acceptance = %q, want no invented command", opts.Acceptance)
			}
			if strings.Contains(opts.Description, "slowcheck:") {
				t.Errorf("description quotes a finding no readable log holds:\n%s", opts.Description)
			}
		})
	}
}

// A control character inside a finding leaves no residue when it is dropped: a
// run of spaces around it reads as one, and a line cut short never ends on a
// space before its "..." marker. The quoted line then reads as slowcheck's own
// text, with nothing of this daemon's quoting in it (gt-j6vlz).
func TestTierSweepBead_QuotedFindingLeavesNoResidue(t *testing.T) {
	t.Parallel()
	const pkg = "./internal/cmd"
	long := "slowcheck: TestAlpha 31.0s in github.com/steveyegge/gastown/internal/cmd is over the cap: " + strings.Repeat("the test waits on a container that never starts. ", 6)
	dir := t.TempDir()
	unitLog := "slowcheck: TestAlpha 31.0s in github.com/steveyegge/gastown/internal/cmd \x01\x02 is over the cap\n" +
		"slowcheck: TestBeta 31.0s in github.com/steveyegge/gastown/internal/cmd is over the cap:  \x1b\n" +
		long + "\n"
	if err := os.WriteFile(filepath.Join(dir, "integration-internal_cmd.log"), []byte(unitLog), 0o600); err != nil {
		t.Fatal(err)
	}
	stageLog := tierSweepWriteTestLog(t, "tier-sweep: integration "+pkg+" ...\n")
	d, _, bd, _ := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	d.tierSweepSeams.beads = func(string) tierSweepBeadStore { return bd }

	red := map[string]tierSweepTierResult{
		"integration": {Verdict: tierSweepRed, Passed: 9, Failed: 1, FailedNames: []string{pkg}, Log: stageLog, LogDir: dir},
	}
	d.tierSweepRecordBeads("gastown", "abc1234", red)

	if len(bd.created) != 1 {
		t.Fatalf("created = %+v, want one bead", bd.created)
	}
	var quoted int
	for _, line := range strings.Split(bd.created[0].Description, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, tierSweepSlowcheckPrefix) {
			continue
		}
		quoted++
		body := strings.TrimPrefix(line, tierSweepSlowcheckPrefix)
		if strings.Contains(body, "  ") {
			t.Errorf("a dropped control character left a double space: %q", line)
		}
		if strings.HasSuffix(body, " ...") {
			t.Errorf("a clipped finding ends on a space before its marker: %q", line)
		}
	}
	if quoted != 3 {
		t.Errorf("quoted %d findings, want 3: %q", quoted, bd.created[0].Description)
	}
}

// A bead quotes a bounded, sanitized sample of the findings: three lines at
// most, each cut to tierSweepFindingLineLen and marked, with control characters
// dropped and invalid UTF-8 replaced, because the text is slowcheck's account of
// someone else's test run and a polecat reads the bead (gt-j6vlz).
func TestTierSweepBead_QuotesFindingsBoundedAndSanitized(t *testing.T) {
	t.Parallel()
	const pkg = "./internal/cmd"
	const tail = " is over the 30s integration cap and is not in internal/testpolicy/slow.txt: "
	finding := func(name, why string) string {
		return "slowcheck: " + name + " 31.0s in github.com/steveyegge/gastown/internal/cmd" + tail + why + "\n"
	}
	longWhy := strings.Repeat("the test waits on a container that never starts. ", 10)
	// An escape sequence and a stray invalid byte, the kind a test's own output
	// can leave in the log slowcheck writes.
	noisyWhy := "\x1b\xffab and terminal red"
	dir := t.TempDir()
	unitLog := finding("TestAlpha", longWhy) +
		finding("TestBeta", noisyWhy) +
		finding("TestGamma", "the test builds a container") +
		finding("TestDelta", "the test builds a container") +
		finding("TestEpsilon", "the test builds a container")
	if err := os.WriteFile(filepath.Join(dir, "integration-internal_cmd.log"), []byte(unitLog), 0o600); err != nil {
		t.Fatal(err)
	}
	stageLog := tierSweepWriteTestLog(t, "tier-sweep: integration "+pkg+" ...\n")
	d, _, bd, _ := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	d.tierSweepSeams.beads = func(string) tierSweepBeadStore { return bd }

	red := map[string]tierSweepTierResult{
		"integration": {Verdict: tierSweepRed, Passed: 9, Failed: 1, FailedNames: []string{pkg}, Log: stageLog, LogDir: dir},
	}
	d.tierSweepRecordBeads("gastown", "abc1234", red)

	if len(bd.created) != 1 {
		t.Fatalf("created = %+v, want one bead", bd.created)
	}
	opts := bd.created[0]
	if v := lintTierSweepBead(opts); !v.Clean() {
		t.Fatalf("the bead is unshaped: %s\ndescription:\n%s", v.ShapeNote(), opts.Description)
	}
	if !utf8.ValidString(opts.Description) {
		t.Errorf("description is not valid UTF-8:\n%q", opts.Description)
	}
	for _, r := range opts.Description {
		if unicode.IsControl(r) && r != '\n' {
			t.Errorf("description holds control character %q:\n%q", r, opts.Description)
			break
		}
	}
	if got := strings.Count(opts.Description, "slowcheck: "); got != tierSweepFindingLines {
		t.Errorf("description quotes %d findings, want %d:\n%s", got, tierSweepFindingLines, opts.Description)
	}
	for _, want := range []string{"TestAlpha", "TestBeta", "TestGamma"} {
		if !strings.Contains(opts.Description, "slowcheck: "+want+" ") {
			t.Errorf("description does not quote %s:\n%s", want, opts.Description)
		}
	}
	for _, unwanted := range []string{"TestDelta", "TestEpsilon"} {
		if strings.Contains(opts.Description, "slowcheck: "+unwanted+" ") {
			t.Errorf("description quotes %s past the cap:\n%s", unwanted, opts.Description)
		}
	}
	if strings.Contains(opts.Description, longWhy) {
		t.Errorf("description quotes the long reason whole, want it clipped:\n%s", opts.Description)
	}
	for _, line := range strings.Split(opts.Description, "\n") {
		quoted, ok := strings.CutPrefix(strings.TrimSpace(line), "slowcheck: ")
		if !ok {
			continue
		}
		if len(quoted) > tierSweepFindingLineLen+len("...") {
			t.Errorf("quoted finding is %d bytes, want at most %d: %q", len(quoted), tierSweepFindingLineLen+len("..."), quoted)
		}
	}
	// The escape sequence is gone and the invalid byte is a replacement
	// character, but the words around them stay readable.
	if strings.ContainsRune(opts.Description, 0x1b) {
		t.Errorf("description pastes an escape sequence into the bead:\n%q", opts.Description)
	}
	if !strings.Contains(opts.Description, "\uFFFDab") {
		t.Errorf("description does not keep the readable text of the noisy finding:\n%s", opts.Description)
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

// The shell tier's check is the Go runner that runs every shell test, the
// command the shell stage runs: it names the runner, never the one script the
// sweep saw go red.
func TestTierSweepBead_ShellTierNamesTheRunner(t *testing.T) {
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
	if want := "`GT_TEST_DOCKER=0 go run ./internal/shelltier/cmd/shelltier --all` passes"; !strings.Contains(opts.Acceptance, want) {
		t.Errorf("acceptance = %q, want it to carry %q", opts.Acceptance, want)
	}
	if strings.Contains(opts.Acceptance, "test-makefile.sh") {
		t.Errorf("acceptance = %q, want no reference to scripts/test-makefile.sh", opts.Acceptance)
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

	red := tierSweepReadUnitRed(tierSweepTierResult{Log: logPath}, "integration", "./internal/cmd")
	if len(red.Names) != 0 || red.BuildTag != "" {
		t.Fatalf("failures = %v (tag %q), want none from a read cut mid fail line", red.Names, red.BuildTag)
	}
}

// A re-run filter taken from a log line can match no test, and go test then
// passes having run nothing. The acceptance item for a Go tier says what that
// output means, so a worker does not tick a box nothing was run for; the shell
// tier runs a script, where the sentence would mean nothing.
func TestTierSweepAcceptanceItemSaysAnEmptyRunProvesNothing(t *testing.T) {
	t.Parallel()
	red := tierSweepUnitRed{Names: []string{"TestSlow"}}
	for _, tier := range []string{"integration", "race"} {
		item := tierSweepAcceptanceItem(tier, "./internal/daemon", tierSweepTierResult{}, red)
		if !strings.Contains(item, "-run '^(TestSlow)$'") {
			t.Fatalf("%s item = %q, want the narrow command", tier, item)
		}
		if !strings.Contains(item, "no tests to run") || !strings.Contains(item, "instead of ticking this box") {
			t.Errorf("%s item = %q, want it to say a run of no tests proves nothing", tier, item)
		}
	}
	shell := tierSweepAcceptanceItem("shell", "scripts/example-test.sh", tierSweepTierResult{}, red)
	if strings.Contains(shell, "no tests to run") {
		t.Errorf("shell item = %q, want no go test wording for a script", shell)
	}
}

// A tab or other whitespace control in a quoted finding separates words as it
// did, so it reads as a space; any other control character is dropped.
func TestTierSweepClipReadsWhitespaceControlsAsSpaces(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, in, want string
	}{
		{"a tab", "TestSlow\ttook\t12s", "TestSlow took 12s"},
		{"a newline and a carriage return", "a\r\nb", "a b"},
		{"an escape sequence is dropped", "a\x1b[31mb", "a[31mb"},
		{"a run of mixed whitespace collapses", "a \t \n b", "a b"},
	} {
		if got := tierSweepClip(tc.in, 200); got != tc.want {
			t.Errorf("%s: tierSweepClip(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}
