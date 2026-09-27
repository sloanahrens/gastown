package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
)

func TestGetFormulaNames(t *testing.T) {
	t.Parallel()
	// Create temp directory structure
	tmpDir := t.TempDir()
	formulasDir := filepath.Join(tmpDir, "formulas")
	if err := os.MkdirAll(formulasDir, 0755); err != nil {
		t.Fatalf("creating formulas dir: %v", err)
	}

	// Create some formula files
	formulas := []string{
		constants.MolDeaconPatrol + ".formula.toml",
		constants.MolWitnessPatrol + ".formula.toml",
		"shiny.formula.toml",
	}
	for _, f := range formulas {
		path := filepath.Join(formulasDir, f)
		if err := os.WriteFile(path, []byte("# test"), 0644); err != nil {
			t.Fatalf("writing %s: %v", f, err)
		}
	}

	// Also create a non-formula file (should be ignored)
	if err := os.WriteFile(filepath.Join(formulasDir, ".installed.json"), []byte("{}"), 0644); err != nil {
		t.Fatalf("writing .installed.json: %v", err)
	}

	// Test
	names := getFormulaNames(tmpDir)
	if names == nil {
		t.Fatal("getFormulaNames returned nil")
	}

	expected := []string{constants.MolDeaconPatrol, constants.MolWitnessPatrol, "shiny"}
	for _, name := range expected {
		if !names[name] {
			t.Errorf("expected formula name %q not found", name)
		}
	}

	// Should not include the .installed.json file
	if names[".installed"] {
		t.Error(".installed should not be in formula names")
	}

	if len(names) != len(expected) {
		t.Errorf("got %d formula names, want %d", len(names), len(expected))
	}
}

func issueIDs(issues []*beads.Issue) []string {
	ids := make([]string, 0, len(issues))
	for _, issue := range issues {
		ids = append(ids, issue.ID)
	}
	return ids
}

func TestGetFormulaNames_NonexistentDir(t *testing.T) {
	t.Parallel()
	names := getFormulaNames("/nonexistent/path")
	if names != nil {
		t.Error("expected nil for nonexistent directory")
	}
}

func TestFilterFormulaScaffolds(t *testing.T) {
	t.Parallel()
	formulaNames := map[string]bool{
		constants.MolDeaconPatrol:  true,
		constants.MolWitnessPatrol: true,
	}

	issues := []*beads.Issue{
		{ID: constants.MolDeaconPatrol, Title: constants.MolDeaconPatrol},
		{ID: constants.MolDeaconPatrol + ".inbox-check", Title: "Handle callbacks"},
		{ID: constants.MolDeaconPatrol + ".health-scan", Title: "Check health"},
		{ID: constants.MolWitnessPatrol, Title: constants.MolWitnessPatrol},
		{ID: constants.MolWitnessPatrol + ".loop-or-exit", Title: "Loop or exit"},
		{ID: "hq-123", Title: "Real work item"},
		{ID: "hq-wisp-abc", Title: "Actual wisp"},
		{ID: "gt-456", Title: "Project issue"},
	}

	filtered := filterFormulaScaffolds(issues, formulaNames)

	// Should only have the non-scaffold issues
	if len(filtered) != 3 {
		t.Errorf("got %d filtered issues, want 3", len(filtered))
	}

	expectedIDs := map[string]bool{
		"hq-123":      true,
		"hq-wisp-abc": true,
		"gt-456":      true,
	}
	for _, issue := range filtered {
		if !expectedIDs[issue.ID] {
			t.Errorf("unexpected issue in filtered result: %s", issue.ID)
		}
	}
}

func TestFilterFormulaScaffolds_NilFormulaNames(t *testing.T) {
	t.Parallel()
	issues := []*beads.Issue{
		{ID: "hq-123", Title: "Real work"},
		{ID: constants.MolDeaconPatrol, Title: "Would be filtered"},
	}

	// With nil formula names, should return all issues unchanged
	filtered := filterFormulaScaffolds(issues, nil)
	if len(filtered) != len(issues) {
		t.Errorf("got %d issues, want %d (nil formulaNames should return all)", len(filtered), len(issues))
	}
}

func TestFilterFormulaScaffolds_EmptyFormulaNames(t *testing.T) {
	t.Parallel()
	issues := []*beads.Issue{
		{ID: "hq-123", Title: "Real work"},
		{ID: constants.MolDeaconPatrol, Title: "Would be filtered"},
	}

	// With empty formula names, should return all issues unchanged
	filtered := filterFormulaScaffolds(issues, map[string]bool{})
	if len(filtered) != len(issues) {
		t.Errorf("got %d issues, want %d (empty formulaNames should return all)", len(filtered), len(issues))
	}
}

func TestFilterFormulaScaffolds_EmptyIssues(t *testing.T) {
	t.Parallel()
	formulaNames := map[string]bool{constants.MolDeaconPatrol: true}
	filtered := filterFormulaScaffolds([]*beads.Issue{}, formulaNames)
	if len(filtered) != 0 {
		t.Errorf("got %d issues, want 0", len(filtered))
	}
}

func TestGetWispIDsUsesBdMolWispList(t *testing.T) {
	beadsPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(beadsPath, "issues.jsonl"), []byte(`{"id":"stale-jsonl-wisp"}`+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	bdPath := filepath.Join(binDir, "bd")
	bdScript := `#!/bin/sh
if [ "$1" = "mol" ] && [ "$2" = "wisp" ] && [ "$3" = "list" ] && [ "$4" = "--json" ]; then
  printf '{"wisps":[{"id":"dolt-wisp-1"},{"id":"dolt-wisp-2"}],"count":2}\n'
  exit 0
fi
exit 1
`
	if err := os.WriteFile(bdPath, []byte(bdScript), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ids := getWispIDs(beadsPath)
	if !ids["dolt-wisp-1"] || !ids["dolt-wisp-2"] {
		t.Fatalf("expected IDs from bd mol wisp list, got %#v", ids)
	}
	if ids["stale-jsonl-wisp"] {
		t.Fatalf("getWispIDs read stale issues.jsonl; got %#v", ids)
	}
}

func TestFilterFormulaScaffolds_DotInNonScaffold(t *testing.T) {
	t.Parallel()
	// Issue ID has a dot but prefix is not a formula name
	formulaNames := map[string]bool{constants.MolDeaconPatrol: true}

	issues := []*beads.Issue{
		{ID: "hq-cv.synthesis-step", Title: "Convoy synthesis"},
		{ID: "some.other.thing", Title: "Random dotted ID"},
	}

	filtered := filterFormulaScaffolds(issues, formulaNames)
	if len(filtered) != 2 {
		t.Errorf("got %d issues, want 2 (non-formula dots should not filter)", len(filtered))
	}
}

func TestFilterReadyIssuesByRoute(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0755); err != nil {
		t.Fatalf("creating town beads dir: %v", err)
	}
	routes := strings.Join([]string{
		`{"prefix":"hq-","path":"."}`,
		`{"prefix":"hq-cv-","path":"."}`,
		`{"prefix":"bds-","path":"bd_symphony/mayor/rig"}`,
		`{"prefix":"gt-","path":"gastown/mayor/rig"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("writing routes: %v", err)
	}

	issues := []*beads.Issue{
		{ID: "hq-123", Title: "town work"},
		{ID: "hq-cv-123", Title: "town convoy"},
		{ID: "bds-town-stale", Title: "wrongly-created town bds row"},
		{ID: "unknown-123", Title: "unknown route"},
	}
	filtered := filterReadyIssuesByRoute(townRoot, "town", issues)
	if got, want := issueIDs(filtered), []string{"hq-123", "hq-cv-123"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("town filtered IDs = %v, want %v", got, want)
	}

	issues = []*beads.Issue{
		{ID: "bds-123", Title: "bd_symphony work"},
		{ID: "hq-123", Title: "town work in rig result"},
		{ID: "gt-123", Title: "other rig work"},
		{ID: "unknown-123", Title: "unknown route"},
	}
	filtered = filterReadyIssuesByRoute(townRoot, "bd_symphony", issues)
	if got, want := issueIDs(filtered), []string{"bds-123"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rig filtered IDs = %v, want %v", got, want)
	}
}

// TestClassifyReadyErr_CappedIsNotAFailure is the gt-m7pq regression test for
// the om-editorial rejection's critical finding: a capped ready page
// (ErrReadyTruncated) must report capped=true so runReady's goroutines take
// the "run the filter pipeline over the returned page" branch instead of the
// "no data, hard failure" branch. It must also leave src.Error empty so the
// same source isn't double-counted as a failedSources entry in runReady's
// warning/exit-code logic.
func TestClassifyReadyErr_CappedIsNotAFailure(t *testing.T) {
	t.Parallel()
	src := ReadySource{Name: "town"}
	err := &beads.ErrReadyTruncated{Found: 100, Cap: 100, TrueCount: 373}

	capped := classifyReadyErr(&src, err)

	if !capped {
		t.Fatal("classifyReadyErr returned false for ErrReadyTruncated, want true")
	}
	if !src.Capped {
		t.Error("src.Capped = false, want true")
	}
	if src.TrueCount != 373 {
		t.Errorf("src.TrueCount = %d, want 373", src.TrueCount)
	}
	if src.Error != "" {
		t.Errorf("src.Error = %q, want empty — a capped page is not a failed source", src.Error)
	}
}

// TestClassifyReadyErr_CappedWithoutTrueCount pins the sub-case where the
// store path's unbounded re-query itself failed (ErrReadyTruncated.StoreError
// set, TrueCount left at zero): the page is still capped data, not a hard
// failure, even though the probe couldn't confirm how much more exists.
func TestClassifyReadyErr_CappedWithoutTrueCount(t *testing.T) {
	t.Parallel()
	src := ReadySource{Name: "town"}
	err := &beads.ErrReadyTruncated{Found: 100, Cap: 100, StoreError: errors.New("probe timed out")}

	capped := classifyReadyErr(&src, err)

	if !capped {
		t.Fatal("classifyReadyErr returned false for ErrReadyTruncated, want true")
	}
	if !src.Capped {
		t.Error("src.Capped = false, want true")
	}
	if src.TrueCount != 0 {
		t.Errorf("src.TrueCount = %d, want 0 (probe failed, no confirmed count)", src.TrueCount)
	}
	if src.Error != "" {
		t.Errorf("src.Error = %q, want empty — a capped page is not a failed source", src.Error)
	}
}

// TestClassifyReadyErr_HardFailureSetsError pins the other side: an error
// that is not ErrReadyTruncated means there is no usable page at all, so
// classifyReadyErr must report capped=false and set src.Error so runReady's
// failedSources check catches it.
func TestClassifyReadyErr_HardFailureSetsError(t *testing.T) {
	t.Parallel()
	src := ReadySource{Name: "town"}
	err := errors.New("bd: connection refused")

	capped := classifyReadyErr(&src, err)

	if capped {
		t.Fatal("classifyReadyErr returned true for a plain error, want false")
	}
	if src.Capped {
		t.Error("src.Capped = true, want false")
	}
	if src.Error != "bd: connection refused" {
		t.Errorf("src.Error = %q, want %q", src.Error, "bd: connection refused")
	}
}

// TestRunReadyBranch_CappedSourceKeepsIssues exercises the exact branch
// shape runReady's town and rig goroutines use
// ("if err == nil || classifyReadyErr(&src, err) { ...run filter
// pipeline... }") so a regression back to the rejected attempt's
// "if err != nil { setError } else { filter }" shape — which discarded the
// capped page entirely — fails this test.
func TestRunReadyBranch_CappedSourceKeepsIssues(t *testing.T) {
	t.Parallel()
	issues := []*beads.Issue{{ID: "gt-real", Title: "capped page row", Priority: 2}}
	err := &beads.ErrReadyTruncated{Found: 1, Cap: 100, TrueCount: 373}

	src := ReadySource{Name: "town"}
	if err == nil || classifyReadyErr(&src, err) {
		src.Issues = issues
	}

	if len(src.Issues) != 1 || src.Issues[0].ID != "gt-real" {
		t.Fatalf("capped source dropped its page: got %d issues, want [gt-real]", len(src.Issues))
	}
	if !src.Capped || src.TrueCount != 373 {
		t.Errorf("src = {Capped: %v, TrueCount: %d}, want {true, 373}", src.Capped, src.TrueCount)
	}

	var failedSources []string
	if src.Error != "" {
		failedSources = append(failedSources, src.Name)
	}
	if len(failedSources) != 0 {
		t.Errorf("failedSources = %v, want empty — a capped-but-successful source is not a failure", failedSources)
	}
}

// TestCappedNote_NeverRendersLikeACompleteBoard is the gt-m7pq regression test
// for the human-output branch: every capped source gets a note, including the
// one whose true size is unknown (TrueCount unset — the unbounded probe failed,
// or bd's envelope said truncated with no total). A capped source that renders
// exactly like a complete one is the silent under-report this whole change
// exists to kill, and "unknown" is not "none".
func TestCappedNote_NeverRendersLikeACompleteBoard(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		trueCount int
		count     int
		want      string
	}{
		{
			name:      "probed size exceeds the page",
			trueCount: 373,
			count:     100,
			want:      "capped: 373 exist",
		},
		{
			name:      "probed size unknown, page has rows",
			trueCount: 0,
			count:     100,
			want:      "capped: more than 100 exist",
		},
		{
			name:      "probed size unknown, page is empty",
			trueCount: 0,
			count:     0,
			want:      "capped: more exist",
		},
		{
			name:      "probe proved no more than the page",
			trueCount: 100,
			count:     100,
			want:      "capped: more than 100 exist",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			src := ReadySource{Name: "gastown", Capped: true, TrueCount: tt.trueCount}
			if got := cappedNote(src, tt.count); got != tt.want {
				t.Errorf("cappedNote(%d rows) = %q, want %q", tt.count, got, tt.want)
			}
		})
	}
}

// TestRunReady_AllSourcesFailedIsACommandFailure is the gt-an5b regression
// test. runReady returned the JSON encode before its failed-sources check, so a
// town whose every store failed still exited 0 and printed a zero-item report:
// byte-identical on the wire to an idle town, and the exit status agreed with
// the idle case too. /api/ready keys off that status, so it answered 200
// carrying the panel's "No ready work" for a town it could not read at all.
//
// Serial: it drives runReady through the real workspace lookup and the
// captureOutput os.Stdout swap, and runReady's output mode and rig filter are
// package variables.
func TestRunReady_AllSourcesFailedIsACommandFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}

	// A bd that always fails: every source's ready query dies, which is the
	// all-sources-failed case. The stub never reaches a real bd or Dolt.
	setupTownWithBdStub(t, "#!/bin/sh\nexit 1\n")

	oldJSON := readyJSON
	readyJSON = true
	t.Cleanup(func() { readyJSON = oldJSON })

	var err error
	out := captureOutput(func() { err = runReady(nil, nil) })

	if err == nil {
		t.Fatalf("runReady returned nil for a town whose every source failed; "+
			"stdout was %q", out)
	}
	if !strings.Contains(err.Error(), "all sources failed to load") {
		t.Errorf("runReady error = %v, want it to name the all-sources failure", err)
	}
	if got := strings.TrimSpace(out); got != "" {
		t.Errorf("runReady wrote %q on stdout; a failed command must not print a "+
			"zero-item report, which is byte-identical to what an idle town prints", got)
	}
}
