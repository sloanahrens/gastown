package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The fixtures below are transcribed from the beads named in gt-mcq — the two
// real same-defect double dispatches the check exists to prevent. They are kept
// verbatim (including the wildcard spelling "TestRunPrimeExternalTools_*")
// because the acceptance test is only meaningful if it replays the text that
// actually defeated the old title-based dedupe.

const (
	// gt-80o, closed after polecat basalt fixed it.
	bead80oTitle = "internal/cmd: ~12 pre-existing test failures unmasked by gt-2nu os.Exit(1) fix"
	bead80oDesc  = `gt-2nu (merged at f523878) removed an os.Exit(1) in persistentPreRun that killed the internal/cmd test binary mid-suite on macOS dev builds, silently hiding tests after the exit point. With the binary now surviving, 'go test ./internal/cmd/' shows ~12 failures that pre-exist on main (verified: TestRunPrimeExternalTools_RunsMemoryAndMail, TestRigBeadsRootPrefersRouteResolvedRigDir, TestBdShowInvocationPinsRoutedMetadataDatabase fail identically at origin/main ecbe8aa). Full failing set: TestFindStrandedConvoys_MixedConvoys, TestRunLogCrashEmitsFeedSessionDeath, TestRunMoleculeAwaitSignalAgentBeadUsesCwdRigBeadsDirWhenBeadsDirPointsTown, TestStandaloneFormulaRigTargetAcquiresSingleAdmission, TestRunPrimeExternalTools_* (3), TestCheckPendingEscalations_BoundsSlowBdList, TestPrepareBdShowExecAnchorsRelativePathBeforeChdir`

	// gt-g6b, the same defect seen from the second vantage point. Its
	// description at sling time was bare formula metadata; the test names
	// arrived as the polecat worked, in design and notes.
	beadG6bTitle  = "Additional non-hermetic/failing internal/cmd tests found on clean main (post gt-0i5)"
	beadG6bDesign = `While fixing gt-0i5 (TestRunDoneWithRoutedIssueIgnoresCurrentRigMirror symlink mismatch), ran the full go test ./internal/cmd/... suite on a clean checkout (macOS, with CGO_CPPFLAGS/CGO_LDFLAGS set for icu4c per Makefile) and found these additional failures, unrelated to gt-0i5's fix:
  - TestFindStrandedConvoys_MixedConvoys
  - TestRunLogCrashEmitsFeedSessionDeath
  - TestRunMoleculeAwaitSignalAgentBeadUsesCwdRigBeadsDirWhenBeadsDirPointsTown
  - TestStandaloneFormulaRigTargetAcquiresSingleAdmission
  - TestRunPrimeExternalTools_RunsMemoryAndMail
  - TestRunPrimeExternalTools_BoundsSlowMailCheck
  - TestRunPrimeExternalTools_SkipsMailCheckForPatrolRoles
  - TestCheckPendingEscalations_BoundsSlowBdList
  - TestRigBeadsRootPrefersRouteResolvedRigDir
  - TestBdShowInvocationPinsRoutedMetadataDatabase
  - TestPrepareBdShowExecAnchorsRelativePathBeforeChdir`
	beadG6bNotes = `Root causes, grouped:
1. Symlink-canonicalization mismatch (7 tests: TestBdShowInvocationPinsRoutedMetadataDatabase,
TestPrepareBdShowExecAnchorsRelativePathBeforeChdir, TestBondFormulaDirectPinsTargetBeadsDir,
TestCloseConvoyPinsTownDatabaseUnderStaleEnv): same class as gt-0i5.
2. Real production bug in internal/beads/routes.go pathWithin(): resolved root via EvalSymlinks
but left the path unresolved when it did not yet exist on disk, which broke
GetRigDirForName's route-resolved lookups generally (TestRigBeadsRootPrefersRouteResolvedRigDir).`

	// gt-3vr, closed. Names both the failing test and its file in the description.
	bead3vrTitle = "cmd/gt package tests flagged as risky but not using hermetic harness"
	bead3vrDesc  = `TestHermeticHarnessEnforced (internal/testutil/hermetic_enforce_test.go) fails: github.com/steveyegge/gastown/cmd/gt has test dependencies that can reach live town state (Dolt/beads/gt/bd) but doesn't call testutil.HermeticMain in a TestMain, nor is it in hermeticExempt. Pre-existing, unrelated to gt-ro0.`

	// gt-rl0, closed — the same defect filed from the refinery gate's vantage point.
	beadRl0Title = "TestHermeticHarnessEnforced fails: cmd/gt tests reach live town state without hermetic harness"
	beadRl0Desc  = `internal/testutil's TestHermeticHarnessEnforced fails on main (verified at 1d3d8ea baseline, pre-existing before gt-ro0): package github.com/steveyegge/gastown/cmd/gt has tests that can reach live town state (Dolt/beads/gt/bd) but no TestMain calling testutil.HermeticMain. Fix: add the TestMain (WithDolt() if needed) to cmd/gt, or add a justified hermeticExempt entry in internal/testutil/hermetic_enforce_test.go. Discovered by refinery during gt-wisp-j0ht gate.`
)

func TestExtractContentRefs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		text      string
		wantTests []string
		wantFiles []string
	}{
		{
			name:      "go test names and dotted paths",
			text:      "TestHermeticHarnessEnforced fails in internal/testutil/hermetic_enforce_test.go",
			wantTests: []string{"TestHermeticHarnessEnforced"},
			wantFiles: []string{"internal/testutil/hermetic_enforce_test.go"},
		},
		{
			name:      "wildcard test spelling is captured whole",
			text:      "TestRunPrimeExternalTools_* (3) fail",
			wantTests: []string{"TestRunPrimeExternalTools_"},
		},
		{
			name:      "bare filename with a code extension counts",
			text:      "the retry lives in sling.go",
			wantFiles: []string{"sling.go"},
		},
		{
			name: "urls are not repo paths",
			text: "see https://github.com/steveyegge/gastown/blob/main/internal/cmd/sling.go and " +
				"docs at github.com/steveyegge/gastown/docs/policy.md",
		},
		{
			name:      "directories are not file references",
			text:      "the failures are in internal/cmd and cmd/gt and ./internal/testutil/",
			wantTests: nil,
			wantFiles: nil,
		},
		{
			name: "versions and dotted prose are not paths",
			text: "bd version 1.2.2, released 6.30pm, see e.g. the yaml front matter",
		},
		{
			name:      "leading relative prefix normalizes away",
			text:      "patch ./internal/cmd/sling_helpers.go and ../gastown/internal/cmd/close.go",
			wantFiles: []string{"gastown/internal/cmd/close.go", "internal/cmd/sling_helpers.go"},
		},
		{
			name:      "trailing punctuation does not break a path",
			text:      "fixed in internal/cmd/sling.go, then internal/beads/routes.go;",
			wantFiles: []string{"internal/beads/routes.go", "internal/cmd/sling.go"},
		},
		{
			name:      "prose test word is not a test name",
			text:      "Testing the tester's tested tests",
			wantTests: nil,
		},
		{
			// The full stop belongs to the path token, so it survives
			// tokenization and has to be trimmed before the extension check.
			name:      "a path ending a sentence keeps its file",
			text:      "the harness lives in internal/testutil/hermetic_enforce_test.go.",
			wantFiles: []string{"internal/testutil/hermetic_enforce_test.go"},
		},
		{
			name:      "possessive prose does not invent a path",
			text:      "internal/testutil's TestHermeticHarnessEnforced fails",
			wantTests: []string{"TestHermeticHarnessEnforced"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refs := extractContentRefs(tt.text)
			assertStrings(t, "tests", refs.Tests, tt.wantTests)
			assertStrings(t, "files", refs.Files, tt.wantFiles)
		})
	}
}

func TestTestNamesOverlap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		a, b string
		want bool
	}{
		{"TestFoo", "TestFoo", true},
		{"TestRunPrimeExternalTools_", "TestRunPrimeExternalTools_BoundsSlowMailCheck", true},
		{"TestRunPrimeExternalTools_BoundsSlowMailCheck", "TestRunPrimeExternalTools_RunsMemoryAndMail", false},
		{"TestFoo", "TestFooBar", false},
		{"TestFooBar", "TestFoo", false},
		{"TestFoo", "TestBar", false},
	}
	for _, tt := range tests {
		if got := testNamesOverlap(tt.a, tt.b); got != tt.want {
			t.Errorf("testNamesOverlap(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

// TestSlingDuplicateCheckCatchesTonightPairs is the acceptance bar on gt-mcq:
// replaying tonight's two double dispatches against the check must flag both.
func TestSlingDuplicateCheckCatchesTonightPairs(t *testing.T) {
	t.Parallel()
	t.Run("gt-80o and gt-g6b", func(t *testing.T) {
		// gt-g6b was dispatched while gt-80o was still open, so the pool holds
		// the closed-after-the-fact side. Replay in both directions: whichever
		// bead is being slung must see the other.
		cases := []struct {
			name      string
			candidate duplicateCandidate
			other     duplicateCandidate
		}{
			{
				name: "slung second",
				candidate: newDuplicateCandidate("gt-g6b", beadG6bTitle, "closed",
					beadG6bDesign, beadG6bNotes),
				other: newDuplicateCandidate("gt-80o", bead80oTitle, "open", bead80oDesc),
			},
			{
				name: "slung first",
				candidate: newDuplicateCandidate("gt-80o", bead80oTitle, "open",
					bead80oDesc),
				other: newDuplicateCandidate("gt-g6b", beadG6bTitle, "closed",
					beadG6bDesign, beadG6bNotes),
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				matches := findDuplicateMatches(tc.candidate, []duplicateCandidate{tc.other})
				decision := decideSlingDuplicates(tc.candidate.ID, matches)
				if !decision.Blocked {
					t.Fatalf("expected the pair to be refused; report was:\n%s", decision.Message)
				}
				for _, want := range []string{"TestFindStrandedConvoys_MixedConvoys", "TestRunLogCrashEmitsFeedSessionDeath"} {
					if !strings.Contains(decision.Message, want) {
						t.Errorf("report should name shared test %s:\n%s", want, decision.Message)
					}
				}
				if !strings.Contains(decision.Message, "--force") {
					t.Errorf("a refusal must offer the override:\n%s", decision.Message)
				}
			})
		}
	})

	t.Run("gt-3vr and gt-rl0", func(t *testing.T) {
		closed := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
		other := newDuplicateCandidate("gt-3vr", bead3vrTitle, "closed", bead3vrDesc)
		other.ClosedAt = closed

		matches := findDuplicateMatches(
			newDuplicateCandidate("gt-rl0", beadRl0Title, "open", beadRl0Desc),
			[]duplicateCandidate{other})

		decision := decideSlingDuplicates("gt-rl0", matches)
		if !decision.Blocked {
			t.Fatalf("expected the pair to be refused; report was:\n%s", decision.Message)
		}
		for _, want := range []string{
			"TestHermeticHarnessEnforced",
			"internal/testutil/hermetic_enforce_test.go",
			"gt-3vr",
		} {
			if !strings.Contains(decision.Message, want) {
				t.Errorf("report should mention %q:\n%s", want, decision.Message)
			}
		}
	})
}

// TestSlingDuplicateNegativeControl is the other half of the acceptance bar: a
// genuinely distinct bead that happens to share a file, but shares no test,
// must pass with a warning rather than being refused.
func TestSlingDuplicateNegativeControl(t *testing.T) {
	t.Parallel()
	existing := newDuplicateCandidate("gt-aaaa", "gt sling retries the hook write on lock contention", "open",
		"The retry loop lives in internal/cmd/sling.go and needs a bounded backoff.",
		"Regression test: TestSlingRetryAfterLockTimeout.")

	candidate := newDuplicateCandidate("gt-bbbb", "gt sling help text omits the duplicate guard", "open",
		"Add the content duplicate guard section to the help text in internal/cmd/sling.go.",
		"Regression test: TestSlingHelpRendersDuplicateGuard.")

	matches := findDuplicateMatches(candidate, []duplicateCandidate{existing})
	if len(matches) == 0 {
		t.Fatal("expected a file-only overlap to be reported")
	}
	if matches[0].Blocking() {
		t.Fatal("a shared file with no shared test must not block")
	}

	decision := decideSlingDuplicates(candidate.ID, matches)
	if decision.Blocked {
		t.Fatalf("distinct bead sharing one file must pass; got refusal:\n%s", decision.Message)
	}
	if decision.Message == "" {
		t.Fatal("a file-only overlap should still be reported")
	}
	for _, want := range []string{"internal/cmd/sling.go", "gt-aaaa", "proceeds"} {
		if !strings.Contains(decision.Message, want) {
			t.Errorf("warning should mention %q:\n%s", want, decision.Message)
		}
	}
}

func TestDecideSlingDuplicates(t *testing.T) {
	t.Parallel()
	if got := decideSlingDuplicates("gt-x", nil); got.Message != "" || got.Blocked {
		t.Errorf("no matches should produce no report, got %+v", got)
	}

	blocking := duplicateMatch{
		Bead:        duplicateCandidate{ID: "gt-1", Status: "closed", ClosedAt: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)},
		SharedTests: []string{"TestFoo"},
	}
	warning := duplicateMatch{
		Bead:        duplicateCandidate{ID: "gt-2", Status: "open"},
		SharedFiles: []string{"internal/cmd/sling.go"},
	}

	both := decideSlingDuplicates("gt-x", []duplicateMatch{warning, blocking})
	if !both.Blocked {
		t.Error("a test-name overlap must block even alongside a file-only overlap")
	}
	if strings.Contains(both.Message, "proceeds") {
		t.Errorf("a blocking report must not claim the sling proceeds:\n%s", both.Message)
	}

	// The count of matches is capped so a bead naming a whole failing suite
	// cannot produce an unbounded report.
	many := make([]duplicateMatch, 0, duplicateMatchLimit+3)
	for i := 0; i < duplicateMatchLimit+3; i++ {
		many = append(many, duplicateMatch{
			Bead:        duplicateCandidate{ID: "gt-many", Status: "open"},
			SharedFiles: []string{"internal/cmd/sling.go"},
		})
	}
	if got := decideSlingDuplicates("gt-x", many); !strings.Contains(got.Message, "and 3 more") {
		t.Errorf("expected the report to summarize the remainder:\n%s", got.Message)
	}
}

func TestFindDuplicateMatchesSkipsSelfAndEmpty(t *testing.T) {
	t.Parallel()
	candidate := newDuplicateCandidate("gt-x", "fix TestFoo", "open", "in internal/cmd/sling.go")
	pool := []duplicateCandidate{
		candidate, // must not match itself
		newDuplicateCandidate("gt-empty", "no references at all", "open"),
		newDuplicateCandidate("gt-match", "also TestFoo", "open"),
	}
	matches := findDuplicateMatches(candidate, pool)
	if len(matches) != 1 {
		t.Fatalf("expected exactly one match, got %d: %+v", len(matches), matches)
	}
	if matches[0].Bead.ID != "gt-match" {
		t.Errorf("matched the wrong bead: %s", matches[0].Bead.ID)
	}
	if !matches[0].Blocking() {
		t.Error("TestFoo overlap should block")
	}
}

// TestCheckSlingDuplicatesSkipsBareBead locks in the cost control: a bead that
// names no tests and no files must not pay for a pool fetch at all.
func TestCheckSlingDuplicatesSkipsBareBead(t *testing.T) {
	resetDuplicatePoolCache()
	t.Cleanup(resetDuplicatePoolCache)

	fetched := 0
	restore := swapFetchDuplicatePoolFn(func(string) ([]duplicateCandidate, error) {
		fetched++
		return nil, nil
	})
	defer restore()

	info := &beadInfo{
		Title:       "Sling: a bead with bare prose",
		Status:      "open",
		Description: "attached_formula: mol-polecat-work\nattached_vars: [\"feature=Sling: a bead with bare prose\"]",
	}
	candidate, matches, err := checkSlingDuplicates(t.TempDir(), "gt-bare", info)
	if err != nil {
		t.Fatalf("checkSlingDuplicates: %v", err)
	}
	if candidate != nil || matches != nil {
		t.Errorf("a bead with nothing to match on should skip the check, got %+v %+v", candidate, matches)
	}
	if fetched != 0 {
		t.Errorf("expected no pool fetch for a bead with no references, got %d", fetched)
	}
}

// TestCheckSlingDuplicatesCheckErrorIsNotFatal pins the advisory contract: a bd
// failure must never refuse a sling, only surface as a check error.
func TestCheckSlingDuplicatesCheckErrorIsNotFatal(t *testing.T) {
	resetDuplicatePoolCache()
	t.Cleanup(resetDuplicatePoolCache)

	restore := swapFetchDuplicatePoolFn(func(string) ([]duplicateCandidate, error) {
		return nil, os.ErrDeadlineExceeded
	})
	defer restore()

	info := &beadInfo{
		Title:       "fix TestFoo in internal/cmd/sling.go",
		Status:      "open",
		Description: "TestFoo fails on main.",
	}
	candidate, matches, err := checkSlingDuplicates(t.TempDir(), "gt-cand", info)
	if err == nil {
		t.Fatal("expected the pool error to be reported")
	}
	if candidate == nil {
		t.Fatal("the candidate should still be returned so the caller can register it")
	}
	if len(matches) != 0 {
		t.Errorf("a failed pool read cannot produce matches, got %+v", matches)
	}
}

// TestDuplicateIntraBatchDetection covers the pattern behind gt-3vr/gt-rl0: the
// mayor batch-slung the pair together, so neither was in the pool snapshot when
// the other was checked. Registering a dispatched bead makes the second see the
// first.
func TestDuplicateIntraBatchDetection(t *testing.T) {
	resetDuplicatePoolCache()
	t.Cleanup(resetDuplicatePoolCache)

	fetched := 0
	restore := swapFetchDuplicatePoolFn(func(string) ([]duplicateCandidate, error) {
		fetched++
		return nil, nil
	})
	defer restore()

	beadsDir := t.TempDir()
	first := newDuplicateCandidate("gt-3vr", bead3vrTitle, "open", bead3vrDesc)
	second := newDuplicateCandidate("gt-rl0", beadRl0Title, "open", beadRl0Desc)

	// First dispatch sees an empty pool and finds nothing.
	matches := findDuplicateMatches(first, mustPool(t, beadsDir))
	if len(matches) != 0 {
		t.Fatalf("expected an empty pool, got %+v", matches)
	}

	// Its hook lands, so it joins the pool.
	noteDispatchedCandidate(beadsDir, first)

	// The second dispatch in the same batch now sees it.
	matches = findDuplicateMatches(second, mustPool(t, beadsDir))
	if len(matches) != 1 || !matches[0].Blocking() {
		t.Fatalf("expected the intra-batch duplicate to be caught, got %+v", matches)
	}
	if matches[0].Bead.ID != "gt-3vr" {
		t.Errorf("matched the wrong bead: %s", matches[0].Bead.ID)
	}
	if fetched != 1 {
		t.Errorf("expected the pool to be fetched once and then reused, got %d fetches", fetched)
	}

	// Registering the same bead twice must not duplicate it.
	noteDispatchedCandidate(beadsDir, first)
	pool := mustPool(t, beadsDir)
	if len(pool) != 1 {
		t.Errorf("expected one registered candidate, got %d", len(pool))
	}
}

// TestNoteSlingCandidateDispatchedNilIsSafe guards the call sites, which pass
// through a possibly-nil candidate when the check was skipped.
func TestNoteSlingCandidateDispatchedNilIsSafe(t *testing.T) {
	t.Parallel()
	resetDuplicatePoolCache()
	t.Cleanup(resetDuplicatePoolCache)
	noteSlingCandidateDispatched(t.TempDir(), nil)
}

// TestExecuteSling_RefusesDuplicateContent exercises the wiring: executeSling
// must refuse before spawning when the bead's named tests already appear on
// open or recently-closed work in the rig.
func TestExecuteSling_RefusesDuplicateContent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	townRoot := t.TempDir()
	writeDuplicateBDStub(t, townRoot)
	t.Setenv("PATH", filepath.Join(townRoot, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))

	params := SlingParams{
		BeadID:   "gt-3vr",
		RigName:  "testrig",
		TownRoot: townRoot,
	}

	result, err := executeSling(params)
	if err == nil {
		t.Fatal("expected executeSling to refuse beads whose content duplicates existing work")
	}
	if result == nil || result.ErrMsg != errSlingDuplicateContent.Error() {
		t.Errorf("expected ErrMsg=%q, got %+v", errSlingDuplicateContent.Error(), result)
	}
	for _, want := range []string{"TestHermeticHarnessEnforced", "gt-rl0", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should mention %q: %v", want, err)
		}
	}
}

// TestExecuteSling_ForceBypassesDuplicateCheck pins the documented escape hatch.
func TestExecuteSling_ForceBypassesDuplicateCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	townRoot := t.TempDir()
	writeDuplicateBDStub(t, townRoot)
	t.Setenv("PATH", filepath.Join(townRoot, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))

	params := SlingParams{
		BeadID:   "gt-3vr",
		RigName:  "testrig",
		TownRoot: townRoot,
		Force:    true,
	}

	// Beyond the check, --force continues into dispatch, which fails here for
	// lack of a real rig — the point is only that the refusal did not fire.
	_, err := executeSling(params)
	if err != nil && strings.Contains(err.Error(), "TestHermeticHarnessEnforced") {
		t.Fatalf("--force must bypass the duplicate check: %v", err)
	}
}

// TestExecuteSling_RefusesDuplicateHiddenInPoolDesignNotes is the acceptance
// bar this file was missing (gt-hgvu, om major on gt-wisp-j5i): the earlier
// pair-replay tests above build both sides of the comparison with
// newDuplicateCandidate, which hands the pool bead full title+description+
// design+notes text directly. That is not what production does — the pool
// comes from listDuplicateCandidates, which runs bd list, and bd list's JSON
// never carries design or notes at all. The real gt-g6b defect (see the
// fixture comment above) named its shared tests only in design and notes, so
// a check that only ever exercises fully-populated candidates can pass while
// the wired-up pipeline still misses that exact case. This test drives
// executeSling end to end through a bd stub shaped like production: the pool
// bead's `bd list` row is silent on the shared test, and the overlap is
// recoverable only from a separate `bd show` of that bead — the batched call
// fetchDuplicateFullText makes.
func TestExecuteSling_RefusesDuplicateHiddenInPoolDesignNotes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	townRoot := t.TempDir()
	writeDesignNotesDuplicateBDStub(t, townRoot)
	t.Setenv("PATH", filepath.Join(townRoot, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))

	params := SlingParams{
		BeadID:   "gt-newbead",
		RigName:  "testrig",
		TownRoot: townRoot,
	}

	result, err := executeSling(params)
	if err == nil {
		t.Fatal("expected executeSling to refuse: the shared test lives in the pool bead's design/notes")
	}
	if result == nil || result.ErrMsg != errSlingDuplicateContent.Error() {
		t.Errorf("expected ErrMsg=%q, got %+v", errSlingDuplicateContent.Error(), result)
	}
	for _, want := range []string{"TestFoo", "gt-pool1", "--force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal should mention %q: %v", want, err)
		}
	}
}

// writeDesignNotesDuplicateBDStub stands up a fake town for
// TestExecuteSling_RefusesDuplicateHiddenInPoolDesignNotes: the candidate
// gt-newbead names TestFoo in its description; the pool bead gt-pool1 names
// TestFoo only in design and notes, which its `bd list` row cannot carry —
// only a `bd show gt-pool1` reveals it, exactly as fetchDuplicateFullText
// fetches it in production.
func writeDesignNotesDuplicateBDStub(t *testing.T, townRoot string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	rigDir := filepath.Join(townRoot, "gastown", "mayor", "rig")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir rig .beads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"),
		[]byte(`{"prefix":"gt-","path":"gastown/mayor/rig"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	candShowPath := filepath.Join(townRoot, "cand-show.json")
	poolShowPath := filepath.Join(townRoot, "pool-show.json")
	activePath := filepath.Join(townRoot, "active.json")
	closedPath := filepath.Join(townRoot, "closed.json")

	writeJSON := func(path string, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	writeJSON(candShowPath, `[{"id":"gt-newbead","title":"Fix TestFoo flake","status":"open",`+
		`"assignee":"","description":"TestFoo fails after the refactor."}]`)
	writeJSON(closedPath, `[]`)
	writeJSON(activePath, `[{"id":"gt-pool1","title":"Some pool bead","status":"open",`+
		`"description":"Unrelated prose, no test names here.","close_reason":""}]`)
	writeJSON(poolShowPath, `[{"id":"gt-pool1","title":"Some pool bead","status":"open",`+
		`"description":"Unrelated prose, no test names here.",`+
		`"design":"Root cause: TestFoo fails intermittently under load.",`+
		`"notes":"Added regression coverage: TestFoo."}]`)

	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	script := `#!/bin/sh
set -eu
if [ "${1:-}" = "--allow-stale" ] && [ "${2:-}" = "version" ]; then
  echo "bd test"
  exit 0
fi
while [ "$#" -gt 0 ]; do
  case "$1" in
    --allow-stale) shift ;;
    *) break ;;
  esac
done
cmd="${1:-}"
case "$cmd" in
  show)
    case " $* " in
      *" gt-newbead "*) cat "$BD_DUPE_CAND_SHOW_FILE" ;;
      *) cat "$BD_DUPE_POOL_SHOW_FILE" ;;
    esac
    ;;
  list)
    case "$*" in
      *--closed-after=*) cat "$BD_DUPE_CLOSED_FILE" ;;
      *) cat "$BD_DUPE_ACTIVE_FILE" ;;
    esac
    ;;
  version) echo "bd test" ;;
esac
exit 0
`
	writeBDStub(t, binDir, script, "")
	t.Setenv("BD_DUPE_CAND_SHOW_FILE", candShowPath)
	t.Setenv("BD_DUPE_POOL_SHOW_FILE", poolShowPath)
	t.Setenv("BD_DUPE_ACTIVE_FILE", activePath)
	t.Setenv("BD_DUPE_CLOSED_FILE", closedPath)
}

// TestConvoyAndEpicDispatchRunDuplicateCheck is the gt-skk7 wiring test. Both
// manual schedulers set SkipDuplicateCheck on what is actually the bead's FIRST
// dispatch — the operator's 'gt sling <convoy|epic>', with no earlier checked
// sling — so the check never ran on those paths at all. A convoy or epic is in
// fact the shape the check exists for: a batch of beads dispatched together are
// the same-defect pair that no title dedupe sees (gt-mcq).
func TestConvoyAndEpicDispatchRunDuplicateCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows")
	}
	townRoot := t.TempDir()
	writeDuplicateBDStub(t, townRoot)
	t.Setenv("PATH", filepath.Join(townRoot, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))

	job := convoyDispatchJob{
		candidate: convoyCandidate{ID: "gt-3vr", Title: bead3vrTitle, RigName: "testrig"},
		agent:     "deepseek-flash",
	}
	// --no-boot keeps the run off the rig's agents, as the daemon passes it.
	convoyOpts := convoyScheduleOpts{Formula: "mol-polecat-work", NoBoot: true}
	epicOpts := epicScheduleOpts{Formula: "mol-polecat-work", NoBoot: true}

	paths := []struct {
		name   string
		params func() SlingParams
	}{
		{
			name:   "gt sling <convoy> -> runConvoySlingByID",
			params: func() SlingParams { return convoySlingParams(job, convoyOpts, townRoot) },
		},
		{
			name: "gt sling <epic> -> runEpicSlingByID",
			params: func() SlingParams {
				child := epicDispatchCandidate{ID: "gt-3vr", Title: bead3vrTitle, RigName: "testrig"}
				return epicSlingParams(child, "mol-polecat-work", epicOpts, townRoot)
			},
		},
	}

	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			resetDuplicatePoolCache()
			t.Cleanup(resetDuplicatePoolCache)

			result, err := executeSling(path.params())
			if err == nil {
				t.Fatal("expected a refusal: gt-3vr names the test gt-rl0 already owns")
			}
			if result == nil || result.ErrMsg != errSlingDuplicateContent.Error() {
				t.Errorf("expected ErrMsg=%q, got %+v", errSlingDuplicateContent.Error(), result)
			}
			for _, want := range []string{"TestHermeticHarnessEnforced", "gt-rl0", "--force"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal should mention %q: %v", want, err)
				}
			}

			// --force is the escape hatch these schedulers already document, so
			// it must reach the check through their own params. Past the check
			// the dispatch fails for want of a real rig; only the refusal
			// matters here.
			forced := path.params()
			forced.Force = true
			if _, err := executeSling(forced); err != nil && strings.Contains(err.Error(), "TestHermeticHarnessEnforced") {
				t.Fatalf("--force must bypass the duplicate check: %v", err)
			}
		})
	}
}

// writeDuplicateBDStub stands up a fake town whose bd answers `show` for gt-3vr
// and `list` for gt-rl0, the pair from gt-mcq.
func writeDuplicateBDStub(t *testing.T, townRoot string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	rigDir := filepath.Join(townRoot, "gastown", "mayor", "rig")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir rig .beads: %v", err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"),
		[]byte(`{"prefix":"gt-","path":"gastown/mayor/rig"}`+"\n"), 0o644); err != nil {
		t.Fatalf("write routes: %v", err)
	}

	showPath := filepath.Join(townRoot, "show.json")
	activePath := filepath.Join(townRoot, "active.json")
	closedPath := filepath.Join(townRoot, "closed.json")

	writeJSON := func(path string, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	writeJSON(showPath, `[{"id":"gt-3vr","title":`+jsonString(bead3vrTitle)+`,"status":"open","assignee":"","description":`+jsonString(bead3vrDesc)+`}]`)
	writeJSON(closedPath, `[]`)
	writeJSON(activePath, `[{"id":"gt-rl0","title":`+jsonString(beadRl0Title)+`,"status":"open","description":`+jsonString(beadRl0Desc)+`,"close_reason":""}]`)

	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	script := `#!/bin/sh
set -eu
if [ "${1:-}" = "--allow-stale" ] && [ "${2:-}" = "version" ]; then
  echo "bd test"
  exit 0
fi
while [ "$#" -gt 0 ]; do
  case "$1" in
    --allow-stale) shift ;;
    *) break ;;
  esac
done
cmd="${1:-}"
case "$cmd" in
  show) cat "$BD_DUPE_SHOW_FILE" ;;
  list)
    case "$*" in
      *--closed-after=*) cat "$BD_DUPE_CLOSED_FILE" ;;
      *) cat "$BD_DUPE_ACTIVE_FILE" ;;
    esac
    ;;
  version) echo "bd test" ;;
esac
exit 0
`
	writeBDStub(t, binDir, script, "")
	t.Setenv("BD_DUPE_SHOW_FILE", showPath)
	t.Setenv("BD_DUPE_ACTIVE_FILE", activePath)
	t.Setenv("BD_DUPE_CLOSED_FILE", closedPath)
}

func newDuplicateCandidate(id, title, status string, parts ...string) duplicateCandidate {
	return duplicateCandidate{
		ID:     id,
		Title:  title,
		Status: status,
		Refs:   extractContentRefs(append([]string{title}, parts...)...),
	}
}

func mustPool(t *testing.T, beadsDir string) []duplicateCandidate {
	t.Helper()
	pool, err := duplicatePoolFor(beadsDir)
	if err != nil {
		t.Fatalf("duplicatePoolFor: %v", err)
	}
	return pool
}

func swapFetchDuplicatePoolFn(fn func(string) ([]duplicateCandidate, error)) func() {
	previous := fetchDuplicatePoolFn
	fetchDuplicatePoolFn = fn
	return func() { fetchDuplicatePoolFn = previous }
}

func assertStrings(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", label, got, want)
		}
	}
}

// jsonString renders s as a JSON string literal for the stub fixtures.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
