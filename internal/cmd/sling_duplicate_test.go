package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/sling"
	"github.com/steveyegge/gastown/internal/specdispatch"
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
		{
			// Every package's test binary defines TestMain, so citing it
			// names no defect; a real test beside it still counts (gt-a0gk).
			name:      "the package entrypoint is not a test reference",
			text:      "TestMain (internal/cmd/main_test.go) panics; TestSlingRetriesHookWrite is unaffected",
			wantTests: []string{"TestSlingRetriesHookWrite"},
			wantFiles: []string{"internal/cmd/main_test.go"},
		},
		{
			name:      "the wildcard spelling of the entrypoint is not one either",
			text:      "TestMain_* fails in every package",
			wantTests: nil,
		},
		{
			// The exclusion is an exact name, not a prefix: a real test that
			// happens to start with TestMain is still a signal.
			name:      "a test whose name starts with the entrypoint still counts",
			text:      "TestMaintenanceWindowExpiry fails",
			wantTests: []string{"TestMaintenanceWindowExpiry"},
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

// The overlap guard reads a plan document as context, not work (gt-4wb27):
// every child of an epic cites the plan it implements, so a shared docs/plans
// path says nothing about two children colliding. Code paths still count.
func TestExtractOverlapRefsIgnoresPlanDocuments(t *testing.T) {
	t.Parallel()
	const plan = "docs/plans/2026-10-19-epic-plan.md"
	tests := []struct {
		name      string
		text      string
		wantFiles []string
	}{
		{
			name:      "a cited plan is dropped",
			text:      "Plan: " + plan + ".",
			wantFiles: []string{},
		},
		{
			name:      "a plan beside a code path keeps the code path",
			text:      "Plan: " + plan + "; the fix lands in internal/cmd/spec.go.",
			wantFiles: []string{"internal/cmd/spec.go"},
		},
		{
			name:      "a relative spelling of the plan is dropped too",
			text:      "./" + plan,
			wantFiles: []string{},
		},
		{
			name:      "a plan in a subdirectory of docs/plans is dropped",
			text:      "Plan: docs/plans/2026-10/epic-plan.md.",
			wantFiles: []string{},
		},
		{
			name:      "a parent-relative spelling of the plan is dropped too",
			text:      "../" + plan,
			wantFiles: []string{},
		},
		{
			name:      "a sibling directory sharing the prefix text still counts",
			text:      "See docs/plans-archive/old-plan.md.",
			wantFiles: []string{"docs/plans-archive/old-plan.md"},
		},
		{
			name:      "a file named plans beside docs still counts",
			text:      "See docs/plans.md.",
			wantFiles: []string{"docs/plans.md"},
		},
		{
			name:      "a plan cited twice and a code path cited once keep only the code path",
			text:      plan + " and again " + plan + ", fix in internal/cmd/sling.go",
			wantFiles: []string{"internal/cmd/sling.go"},
		},
		{
			name:      "design docs outside docs/plans still count",
			text:      "See docs/design/2026-10-19-epic-design.md.",
			wantFiles: []string{"docs/design/2026-10-19-epic-design.md"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertStrings(t, "files", extractOverlapRefs(tt.text).Files, tt.wantFiles)
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
		if got := specdispatch.TestNamesOverlap(tt.a, tt.b); got != tt.want {
			t.Errorf("TestNamesOverlap(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

// TestSlingDuplicateCheckCatchesTonightPairs is the acceptance bar on gt-mcq:
// replaying tonight's two double dispatches against the check must flag both.
// Since gt-4k3fj.5 only live work (open, in_progress, hooked) refuses the sling;
// the same overlap with a closed bead is reported as a warning and proceeds.
func TestSlingDuplicateCheckCatchesTonightPairs(t *testing.T) {
	t.Parallel()
	t.Run("gt-80o and gt-g6b", func(t *testing.T) {
		// gt-g6b was dispatched while gt-80o was still open and was closed
		// after the fact, so the replay hands each direction the other side's
		// row as it stood at that bead's sling: a tick's pool would read a bead
		// it just dispatched as hooked (noteDispatched), not open, which is a
		// different question from this pair's. Replay in both directions:
		// whichever bead is being slung must see the other.
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
				decision := decideSlingDuplicates(tc.candidate, matches)
				wantBlocked := tc.other.Status != "closed"
				if decision.Blocked != wantBlocked {
					t.Fatalf("blocked = %v, want %v (other is %s); report was:\n%s", decision.Blocked, wantBlocked, tc.other.Status, decision.Message)
				}
				for _, want := range []string{"TestFindStrandedConvoys_MixedConvoys", "TestRunLogCrashEmitsFeedSessionDeath"} {
					if !strings.Contains(decision.Message, want) {
						t.Errorf("report should name shared test %s:\n%s", want, decision.Message)
					}
				}
				if wantBlocked && !strings.Contains(decision.Message, "--force") {
					t.Errorf("a refusal must offer the override:\n%s", decision.Message)
				}
			})
		}
	})

	t.Run("gt-3vr and gt-rl0", func(t *testing.T) {
		closed := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
		other := newDuplicateCandidate("gt-3vr", bead3vrTitle, "closed", bead3vrDesc)
		other.ClosedAt = closed

		candidate := newDuplicateCandidate("gt-rl0", beadRl0Title, "open", beadRl0Desc)
		matches := findDuplicateMatches(candidate, []duplicateCandidate{other})

		decision := decideSlingDuplicates(candidate, matches)
		if decision.Blocked {
			t.Fatalf("a CLOSED overlap must never refuse the sling (gt-4k3fj.5); report was:\n%s", decision.Message)
		}
		if !strings.Contains(decision.Message, "proceeds") {
			t.Errorf("a closed overlap should still be reported as proceeding:\n%s", decision.Message)
		}

		live := other
		live.Status = "in_progress"
		liveDecision := decideSlingDuplicates(candidate, findDuplicateMatches(
			candidate, []duplicateCandidate{live}))
		if !liveDecision.Blocked {
			t.Fatalf("the same overlap with live work must refuse; report was:\n%s", liveDecision.Message)
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
	if matches[0].Blocking(candidate) {
		t.Fatal("a shared file with no shared test must not block")
	}

	decision := decideSlingDuplicates(candidate, matches)
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

// TestSlingDuplicateIgnoresPackageFixtures replays the refusal behind gt-a0gk:
// gt-hotx and gt-a8kx were each refused against ten unrelated live beads, and
// every one of those overlaps was the name "TestMain" — Go's package
// entrypoint, which every test binary defines and which therefore distinguishes
// nothing. The shared file they do have is exactly the weak evidence the guard
// warns on rather than refuses.
func TestSlingDuplicateIgnoresPackageFixtures(t *testing.T) {
	t.Parallel()

	newPair := func(testA, testB string) (duplicateCandidate, duplicateCandidate) {
		return newDuplicateCandidate("gt-hotx", "gt done: slot wait reports a hang", "open",
				fmt.Sprintf("%s regressed in internal/cmd; the wait loop is in internal/cmd/done.go.", testA)),
			newDuplicateCandidate("gt-a8kx", "done: slot wait never re-reads the queue", "open",
				fmt.Sprintf("%s in internal/cmd; internal/cmd/done.go holds the wait.", testB))
	}

	t.Run("the entrypoint alone never blocks", func(t *testing.T) {
		candidate, other := newPair("TestMain", "TestMain")
		matches := findDuplicateMatches(candidate, []duplicateCandidate{other})
		if len(matches) != 1 {
			t.Fatalf("expected the shared file to be reported, got %+v", matches)
		}
		if matches[0].Blocking(candidate) {
			t.Fatalf("a shared TestMain must not refuse a sling: %+v", matches[0])
		}
		for _, m := range matches {
			if len(m.SharedTests) != 0 {
				t.Fatalf("shared tests = %v, want none: TestMain is a package fixture", m.SharedTests)
			}
		}
		decision := decideSlingDuplicates(candidate, matches)
		if decision.Blocked {
			t.Fatalf("distinct beads citing TestMain must pass; got refusal:\n%s", decision.Message)
		}
		if strings.Contains(decision.Message, "shared test:") {
			t.Errorf("the report must not claim a shared test:\n%s", decision.Message)
		}
	})

	t.Run("a real shared test still refuses", func(t *testing.T) {
		candidate, other := newPair("TestRunDoneSlotWait", "TestRunDoneSlotWait")
		matches := findDuplicateMatches(candidate, []duplicateCandidate{other})
		decision := decideSlingDuplicates(candidate, matches)
		if !decision.Blocked {
			t.Fatalf("the same pair sharing a real test must refuse; report was:\n%s", decision.Message)
		}
	})
}

func TestDecideSlingDuplicates(t *testing.T) {
	t.Parallel()
	candidate := newDuplicateCandidate("gt-x", "sling the thing", "open")
	if got := decideSlingDuplicates(candidate, nil); got.Message != "" || got.Blocked {
		t.Errorf("no matches should produce no report, got %+v", got)
	}

	blocking := duplicateMatch{
		Bead:        duplicateCandidate{ID: "gt-1", Status: "hooked"},
		SharedTests: []string{"TestFoo"},
	}
	warning := duplicateMatch{
		Bead:        duplicateCandidate{ID: "gt-2", Status: "open"},
		SharedFiles: []string{"internal/cmd/sling.go"},
	}

	both := decideSlingDuplicates(candidate, []duplicateMatch{warning, blocking})
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
	if got := decideSlingDuplicates(candidate, many); !strings.Contains(got.Message, "and 3 more") {
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
	if !matches[0].Blocking(candidate) {
		t.Error("TestFoo overlap should block")
	}
}

// TestCheckSlingDuplicatesSkipsBareBead locks in the cost control: a bead that
// names no tests and no files must not pay for a pool fetch at all.
func TestCheckSlingDuplicatesSkipsBareBead(t *testing.T) {
	t.Parallel()
	fetched := 0
	pools := newDuplicatePools(func(string) ([]duplicateCandidate, error) {
		fetched++
		return nil, nil
	}, noPeerHolds)

	info := &beadInfo{
		Title:       "Sling: a bead with bare prose",
		Status:      "open",
		Description: "attached_formula: mol-polecat-work\nattached_vars: [\"feature=Sling: a bead with bare prose\"]",
	}
	candidate, matches, err := pools.check(t.TempDir(), "gt-bare", info)
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

// A bead whose only file reference is a plan document has nothing left to
// compare once the plan is dropped (gt-4wb27), so it is not guarded: no
// candidate, no pool fetch. This is the intended cost of letting siblings share
// a plan, pinned so a change to it is deliberate.
func TestCheckSlingDuplicatesSkipsBeadNamingOnlyAPlan(t *testing.T) {
	t.Parallel()
	fetched := 0
	pools := newDuplicatePools(func(string) ([]duplicateCandidate, error) {
		fetched++
		return nil, nil
	}, noPeerHolds)

	info := &beadInfo{
		Title:       "Implement step 2 of the epic",
		Status:      "open",
		Description: "Plan: docs/plans/2026-10-19-epic-plan.md",
	}
	candidate, matches, err := pools.check(t.TempDir(), "gt-plan-only", info)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if candidate != nil || matches != nil {
		t.Errorf("a bead naming only a plan should skip the check, got %+v %+v", candidate, matches)
	}
	if fetched != 0 {
		t.Errorf("expected no pool fetch for a plan-only bead, got %d", fetched)
	}
}

// TestCheckSlingDuplicatesCheckErrorIsNotFatal pins the advisory contract: a bd
// failure must never refuse a sling, only surface as a check error.
func TestCheckSlingDuplicatesCheckErrorIsNotFatal(t *testing.T) {
	t.Parallel()
	pools := newDuplicatePools(func(string) ([]duplicateCandidate, error) {
		return nil, os.ErrDeadlineExceeded
	}, noPeerHolds)

	info := &beadInfo{
		Title:       "fix TestFoo in internal/cmd/sling.go",
		Status:      "open",
		Description: "TestFoo fails on main.",
	}
	candidate, matches, err := pools.check(t.TempDir(), "gt-cand", info)
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
	t.Parallel()
	fetched := 0
	pools := newDuplicatePools(func(string) ([]duplicateCandidate, error) {
		fetched++
		return nil, nil
	}, noPeerHolds)

	beadsDir := t.TempDir()
	// gt-3vr was filed first, so the ready board reaches it first; the guard
	// ranks the two the same way.
	first := ranked(newDuplicateCandidate("gt-3vr", bead3vrTitle, "open", bead3vrDesc), 1, "2026-10-15T09:00:00Z")
	second := ranked(newDuplicateCandidate("gt-rl0", beadRl0Title, "open", beadRl0Desc), 1, "2026-10-15T10:00:00Z")

	// First dispatch sees an empty pool and finds nothing.
	matches := findDuplicateMatches(first, mustPool(t, pools, beadsDir))
	if len(matches) != 0 {
		t.Fatalf("expected an empty pool, got %+v", matches)
	}

	// Its hook lands, so it joins the pool.
	pools.noteDispatched(beadsDir, first)

	// The second dispatch in the same batch now sees it.
	matches = findDuplicateMatches(second, mustPool(t, pools, beadsDir))
	if len(matches) != 1 || !matches[0].Blocking(second) {
		t.Fatalf("expected the intra-batch duplicate to be caught, got %+v", matches)
	}
	if matches[0].Bead.ID != "gt-3vr" {
		t.Errorf("matched the wrong bead: %s", matches[0].Bead.ID)
	}
	if fetched != 1 {
		t.Errorf("expected the pool to be fetched once and then reused, got %d fetches", fetched)
	}

	// Registering the same bead twice must not duplicate it.
	pools.noteDispatched(beadsDir, first)
	pool := mustPool(t, pools, beadsDir)
	if len(pool) != 1 {
		t.Errorf("expected one registered candidate, got %d", len(pool))
	}
}

// TestCheckSlingDuplicatesReadiesTheCandidate (gt-mknmb): the row the sling's
// check hands to noteDispatched carries the readiness half a bd show gave the
// sling, so it answers the dispatcher's hold question exactly as the stored
// record does — read from a row that never was, the shape lint refuses the bead
// for a description it does not have. The row is filed back as hooked once the
// hook lands (gt-dzjtz), so the next bead of the same burst is refused by the
// seat the first one holds.
func TestCheckSlingDuplicatesReadiesTheCandidate(t *testing.T) {
	t.Parallel()
	knobs := specHoldKnobs{
		MaxPriority: 2,
		ShapeGate:   specdispatch.ShapeGateWarn,
		Template:    specdispatch.Template{Sections: specdispatch.DefaultSections, Source: "built-in"},
	}
	pools := newDuplicatePools(
		func(string) ([]duplicateCandidate, error) { return nil, nil },
		func(_, _ string, peer duplicateCandidate, pool []duplicateCandidate) string {
			return specPeerHeldFrom(beadsfake.New(), knobs, peer, pool)
		},
	)

	info := &beadInfo{
		Title:       "Fix TestFoo in internal/cmd/tap_guard_dangerous.go",
		Status:      "open",
		IssueType:   "task",
		Priority:    1,
		CreatedAt:   "2026-10-15T09:00:00Z",
		Description: specTestDescription,
		Acceptance:  specTestAcceptance,
	}
	candidate, _, err := pools.check(duplicateTown(t), "gt-first", info)
	if err != nil {
		t.Fatalf("checkSlingDuplicates: %v", err)
	}
	if candidate == nil {
		t.Fatal("the bead names TestFoo, so the guard has to inspect it")
	}
	if got := specPeerHeldFrom(beadsfake.New(), knobs, *candidate, nil); got != "" {
		t.Errorf("the candidate's own row holds it from dispatch: %q, want the dispatcher's to sling", got)
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

// TestCheckSlingDuplicatesSeesOverlapInPoolDesignNotes is the acceptance bar
// this file was missing (gt-hgvu, om major on gt-wisp-j5i): the earlier
// pair-replay tests above build both sides of the comparison with
// newDuplicateCandidate, which hands the pool bead full title+description+
// design+notes text directly. That is not what production does — the pool
// comes from listDuplicateCandidates, which runs bd list, and bd list's JSON
// never carries design or notes at all. The real gt-g6b defect (see the
// fixture comment above) named its shared tests only in design and notes, so
// a check that only ever exercises fully-populated candidates can pass while
// the wired-up pipeline still misses that exact case. This test drives the
// check through the production pool fetch over a bd shaped like production:
// the pool bead's list row is silent on the shared test, and the overlap is
// recoverable only from the batched show fetchDuplicateDetails makes.
func TestCheckSlingDuplicatesSeesOverlapInPoolDesignNotes(t *testing.T) {
	t.Parallel()
	townRoot := duplicateTown(t)
	store := designNotesDuplicateStore()
	pools := newDuplicatePools(func(beadsDir string) ([]duplicateCandidate, error) {
		return fetchDuplicatePoolFrom(store, func(dir string, err error) {
			t.Errorf("enrichment of %s failed: %v", dir, err)
		}, beadsDir)
	}, noPeerHolds)

	// gt-pool1 is the older bead, so it comes first in dispatch order and its
	// overlap refuses the candidate rather than the other way round.
	info := &beadInfo{Title: "Fix TestFoo flake", Status: "open", Description: "TestFoo fails after the refactor.",
		CreatedAt: "2026-10-15T10:00:00Z"}
	candidate, matches, err := pools.check(townRoot, "gt-newbead", info)
	if err != nil {
		t.Fatalf("checkSlingDuplicates: %v", err)
	}
	if candidate == nil {
		t.Fatal("a bead naming TestFoo should be checked")
	}
	decision := decideSlingDuplicates(*candidate, matches)
	if !decision.Blocked {
		t.Fatalf("expected a refusal: the shared test lives in the pool bead's design/notes, got %+v", matches)
	}
	for _, want := range []string{"TestFoo", "gt-pool1", "--force"} {
		if !strings.Contains(decision.Message, want) {
			t.Errorf("refusal should mention %q:\n%s", want, decision.Message)
		}
	}
}

// TestCheckSlingDuplicatesIgnoresDependencyChain is the gt-xydyc production
// path: the pool bead that refused the base of a planned chain is the bead
// that depends on it, so the edges have to come out of the batched show fetch
// the pool already makes.
func TestCheckSlingDuplicatesIgnoresDependencyChain(t *testing.T) {
	t.Parallel()
	townRoot := duplicateTown(t)

	db := beadsfake.New()
	db.Seed(
		beads.Issue{ID: "gt-plan.1", Title: "plan: base step", Status: "open",
			Description: "TestSharedStep fails at the base of the plan."},
		beads.Issue{ID: "gt-plan.3", Title: "plan: prerequisite step", Status: "open",
			Description: "TestSharedStep fails in the step the base needs."},
		beads.Issue{ID: "gt-plan.7", Title: "plan: a tracked step", Status: "open",
			Description: "TestSharedStep fails in the step that tracks the base."},
		beads.Issue{ID: "gt-plan.9", Title: "plan: a step that depends on the base", Status: "hooked",
			Description: "TestSharedStep fails from this vantage point."},
		beads.Issue{ID: "gt-rival", Title: "unrelated bead", Status: "open",
			Description: "TestSharedStep fails."},
	)
	if err := db.AddDependency("gt-plan.9", "gt-plan.1"); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}
	if err := db.AddTypedDependency("gt-plan.7", "gt-plan.1", "tracks"); err != nil {
		t.Fatalf("AddTypedDependency: %v", err)
	}

	pools := newDuplicatePools(func(beadsDir string) ([]duplicateCandidate, error) {
		return fetchDuplicatePoolFrom(db, func(dir string, err error) {
			t.Errorf("enrichment of %s failed: %v", dir, err)
		}, beadsDir)
	}, noPeerHolds)

	// The pool beads carry the lower priority number, so both come first in
	// dispatch order and neither is let past on rank; the dependency chain is
	// the only thing this test varies.
	info := &beadInfo{
		Title:        "plan: base step",
		Status:       "open",
		Priority:     2,
		Description:  "TestSharedStep fails at the base of the plan.",
		Dependencies: []beads.IssueDep{{ID: "gt-plan.3", DependencyType: "blocks"}},
	}
	candidate, matches, err := pools.check(townRoot, "gt-plan.1", info)
	if err != nil {
		t.Fatalf("checkSlingDuplicates: %v", err)
	}

	var got []string
	for _, m := range matches {
		got = append(got, m.Bead.ID)
	}
	// The dependent and the prerequisite drop out of the overlap; the bead
	// outside the chain stays, and so does the one only linked by a
	// non-blocking relation (gt-plan.7 tracks the candidate).
	if strings.Join(got, ",") != "gt-plan.7,gt-rival" {
		t.Fatalf("matches = %v, want [gt-plan.7 gt-rival]", got)
	}
	decision := decideSlingDuplicates(*candidate, matches)
	if !decision.Blocked {
		t.Fatalf("the unrelated bead must still refuse the sling:\n%s", decision.Message)
	}
	if strings.Contains(decision.Message, "gt-plan.9") || strings.Contains(decision.Message, "gt-plan.3") {
		t.Errorf("the report must not count the chain as overlap:\n%s", decision.Message)
	}
}

// duplicateTown is a temp town whose gt- prefix routes to the gastown rig,
// where the duplicate check reads its pool.
func duplicateTown(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir rig .beads: %v", err)
	}
	writeGastownRoutes(t, townRoot)
	return townRoot
}

// listLikeBD is a store whose List rows lack design and notes, as bd list's
// JSON does; Show and ShowMultiple carry them. showErr, when set, fails
// ShowMultiple.
type listLikeBD struct {
	beads.Client
	showErr error
}

func (s listLikeBD) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	rows, err := s.Client.List(opts)
	for _, row := range rows {
		row.Design, row.Notes = "", ""
	}
	return rows, err
}

func (s listLikeBD) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	if s.showErr != nil {
		return nil, s.showErr
	}
	return s.Client.ShowMultiple(ids)
}

// designNotesDuplicateStore holds gt-pool1, open, whose list row is silent
// on TestFoo; only its design and notes name it. It is older than the
// candidate that shares the test, so it comes first in dispatch order.
func designNotesDuplicateStore() listLikeBD {
	db := beadsfake.New()
	db.Seed(beads.Issue{ID: "gt-pool1", Title: "Some pool bead", Status: "open",
		Description: "Unrelated prose, no test names here.",
		Design:      "Root cause: TestFoo fails intermittently under load.",
		Notes:       "Added regression coverage: TestFoo.",
		CreatedAt:   "2026-10-15T09:00:00Z"})
	return listLikeBD{Client: db}
}

// TestSchedulerSlingParamsRunDuplicateCheck is the gt-skk7 / gt-eisp2 wiring
// test. Every scheduler used to set SkipDuplicateCheck on what is actually the
// bead's FIRST dispatch, so the check never ran on those paths at all. The
// operator's 'gt sling <epic>' has no earlier checked sling, so the params it
// builds must leave the check on and carry --force, the escape hatch the
// scheduler documents. That executeSling refuses on those params is
// TestExecuteSlingDuplicateContent.
func TestSchedulerSlingParamsRunDuplicateCheck(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	epicChild := epicDispatchCandidate{ID: "gt-3vr", Title: bead3vrTitle, RigName: "testrig"}

	for _, force := range []bool{false, true} {
		for name, params := range map[string]SlingParams{
			"gt sling <epic>": epicSlingParams(epicChild, "mol-polecat-work", epicScheduleOpts{Formula: "mol-polecat-work", NoBoot: true, Force: force}, townRoot),
		} {
			if params.SkipDuplicateCheck {
				t.Errorf("%s (force=%v): SkipDuplicateCheck set on a first dispatch", name, force)
			}
			if params.Force != force {
				t.Errorf("%s: Force = %v, want %v", name, params.Force, force)
			}
		}
	}
}

// TestCapacityDispatchDuplicateRefusalIsAFailure pins the gt-eisp2 ruling: a
// duplicate refusal at the capacity dispatcher counts toward the context's
// circuit breaker instead of leaving it queued. The refusal clears itself only
// once the overlapping bead is slung or closes, with no bound on how long that
// takes, so a silent deferral would stall the bead.
func TestCapacityDispatchDuplicateRefusalIsAFailure(t *testing.T) {
	t.Parallel()
	refusal := fmt.Errorf("sling failed: %w", errors.New("refusing to sling gt-3vr: duplicate content of gt-rl0 (use --force)"))
	if why, deferred := capacityDispatchDeferral(refusal); deferred {
		t.Fatalf("a duplicate refusal must not be deferred (%q)", why)
	}
}

func newDuplicateCandidate(id, title, status string, parts ...string) duplicateCandidate {
	return duplicateCandidate{
		ID:     id,
		Title:  title,
		Status: status,
		Refs:   extractOverlapRefs(append([]string{title}, parts...)...),
	}
}

// ranked sets the dispatch-order keys the overlap guard ranks unstarted beads
// by, so a fixture states which of two open beads the ready board reaches
// first (gt-nlkba) instead of leaving it to the ID tie-break.
func ranked(c duplicateCandidate, priority int, createdAt string) duplicateCandidate {
	c.Priority = priority
	c.CreatedAt = createdAt
	return c
}

func mustPool(t *testing.T, pools *duplicatePools, beadsDir string) []duplicateCandidate {
	t.Helper()
	pool, err := pools.poolFor(beadsDir)
	if err != nil {
		t.Fatalf("duplicatePoolFor: %v", err)
	}
	return pool
}

// noPeerHolds is the resolver for a pool whose test varies something other
// than the peer-hold question: no peer is known to be held, so the guard's
// rank decides alone, exactly as it did before the holds existed.
func noPeerHolds(string, string, duplicateCandidate, []duplicateCandidate) string { return "" }

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

// TestListDuplicateCandidates_EnrichmentFailureDegradesLoudly pins the
// contract of the design/notes enrichment step (gt-vn90g, om major on
// gt-wisp-ikxn): when the batched bd show fails, the pool fetch still succeeds
// with list-only refs — a bd hiccup is not a duplicate — but the failure is
// reported, since a pool without design/notes text silently misses the gt-g6b
// class of overlap.
func TestListDuplicateCandidates_EnrichmentFailureDegradesLoudly(t *testing.T) {
	t.Parallel()
	beadsDir := t.TempDir()
	db := beadsfake.New()
	db.Seed(beads.Issue{ID: "gt-pool1", Title: "Some pool bead", Status: "open", Description: "Touches internal/cmd/sling_duplicate.go and TestFoo."})
	store := listLikeBD{Client: db, showErr: errors.New("bd show: dolt connection refused")}

	var warnedDir string
	var warnedErr error
	warn := func(beadsDir string, err error) { warnedDir, warnedErr = beadsDir, err }

	got, err := listDuplicateCandidates(store, warn, beadsDir, []string{"open"}, time.Time{})
	if err != nil {
		t.Fatalf("a failed enrichment must not fail the pool fetch: %v", err)
	}
	if len(got) != 1 || got[0].ID != "gt-pool1" {
		t.Fatalf("expected the list-only candidate gt-pool1, got %+v", got)
	}
	if got[0].Refs.Empty() {
		t.Error("list-only refs from the description should survive a failed enrichment")
	}
	if warnedErr == nil {
		t.Fatal("a failed enrichment must be reported, not swallowed")
	}
	if warnedDir != beadsDir {
		t.Errorf("warning names %q, want %q", warnedDir, beadsDir)
	}
	if !strings.Contains(warnedErr.Error(), "dolt connection refused") {
		t.Errorf("warning error should carry bd's failure: %v", warnedErr)
	}
}

// TestListDuplicateCandidates_EnrichmentSuccessIsQuiet guards the other side:
// a healthy enrichment must not emit the degraded-check warning.
func TestListDuplicateCandidates_EnrichmentSuccessIsQuiet(t *testing.T) {
	t.Parallel()
	warned := false
	warn := func(string, error) { warned = true }

	got, err := listDuplicateCandidates(designNotesDuplicateStore(), warn, t.TempDir(), []string{"open"}, time.Time{})
	if err != nil {
		t.Fatalf("listDuplicateCandidates: %v", err)
	}
	if warned {
		t.Error("a successful enrichment must not warn")
	}
	if len(got) != 1 || len(got[0].Refs.Tests) == 0 {
		t.Errorf("expected design/notes tests recovered, got %+v", got)
	}
}

// TestDuplicateOverlapRefusalByStatus: in_progress and hooked beads refuse a
// sling outright; closed (the gt-bivxl case) and parked statuses warn. An open
// bead is neither — it refuses only on rank, which
// TestOpenClusterDispatchesOneAtATime covers (gt-nlkba) — so here, where the
// candidate comes first on the board, it does not refuse.
func TestDuplicateOverlapRefusalByStatus(t *testing.T) {
	t.Parallel()
	// The overlapping bead below is numbered behind the candidate, so nothing
	// here is let past on rank and the status is the only variable.
	candidate := ranked(duplicateCandidate{ID: "gt-candidate", Status: "open"}, 1, "2026-10-15T12:00:00Z")
	for status, want := range map[string]bool{
		"open": false, "in_progress": true, "hooked": true,
		"closed": false, "pinned": false, "blocked": false, "deferred": false, "tombstone": false,
	} {
		m := duplicateMatch{Bead: ranked(duplicateCandidate{ID: "gt-z1", Status: status}, 2, "2026-10-15T12:00:00Z"), SharedTests: []string{"TestFoo"}}
		if got := m.Blocking(candidate); got != want {
			t.Errorf("status %s: Blocking() = %v, want %v", status, got, want)
		}
		if got := decideSlingDuplicates(candidate, []duplicateMatch{m}).Blocked; got != want {
			t.Errorf("status %s: decision blocked = %v, want %v", status, got, want)
		}
	}
	closedAt := time.Now().Add(-43 * time.Minute).UTC().Format(time.RFC3339)
	twoClosed := []duplicateMatch{
		{Bead: duplicateCandidate{ID: "gt-fqdj9", Status: "closed", ClosedAt: closedAt}, SharedTests: []string{"TestA"}},
		{Bead: duplicateCandidate{ID: "gt-ty2fy", Status: "closed"}, SharedTests: []string{"TestB"}},
	}
	if d := decideSlingDuplicates(ranked(duplicateCandidate{ID: "gt-bivxl", Status: "open"}, 1, "2026-10-15T12:00:00Z"), twoClosed); d.Blocked {
		t.Fatalf("gt-bivxl must not be refused over two closed beads:\n%s", d.Message)
	}
}

// TestOpenClusterDispatchesOneAtATime is the gt-nlkba acceptance bar, over the
// cluster that actually wedged: gt-hqenc, gt-79opc and gt-t7p80 are ready, all
// three name TestDangerousGuard_Integration in
// internal/cmd/tap_guard_dangerous.go, and every one of them refused each of the
// others for hours while a seat sat idle — each refusal saying it would clear
// when "the overlapping bead closes", which none of them ever would. With the
// refusal ordered, one pass of the decision per bead lets exactly one through:
// the highest priority, then the oldest. The rest are then held against work in
// flight, which is what happens today after a manual --force sling.
func TestOpenClusterDispatchesOneAtATime(t *testing.T) {
	t.Parallel()
	const shared = "TestDangerousGuard_Integration fails in internal/cmd/tap_guard_dangerous.go."

	cluster := []duplicateCandidate{
		// P1 and newer: behind gt-79opc on age alone.
		ranked(newDuplicateCandidate("gt-hqenc", "guard judges the argv after gt slot run --", "open", shared), 1, "2026-10-15T10:00:00Z"),
		// P1 and oldest: the head of the cluster.
		ranked(newDuplicateCandidate("gt-79opc", "guard blocks a nested command", "open", shared), 1, "2026-10-15T09:00:00Z"),
		// The oldest of all, but P2: priority number wins over age.
		ranked(newDuplicateCandidate("gt-t7p80", "guard tests are flaky under load", "open", shared), 2, "2026-10-15T08:00:00Z"),
	}

	// unblocked runs one pass of the decision the way the tick does: the ready
	// board decides each bead it is holding, and everything else — the work
	// already in flight — is only what those beads are compared against.
	unblocked := func(pool []duplicateCandidate) []string {
		var got []string
		for _, c := range pool {
			if c.Status != "open" {
				continue
			}
			if !decideSlingDuplicates(c, findDuplicateMatches(c, pool)).Blocked {
				got = append(got, c.ID)
			}
		}
		return got
	}

	// Every bead is open, so nothing is live: exactly one is not refused, and
	// it is the head.
	if got := unblocked(cluster); len(got) != 1 || got[0] != "gt-79opc" {
		t.Fatalf("unblocked = %v, want exactly [gt-79opc]", got)
	}

	// The head is slung: now it is work in flight, and the overlap with it
	// refuses the other two whoever they are.
	slung := append([]duplicateCandidate(nil), cluster...)
	slung[1].Status = "hooked"
	if got := unblocked(slung); len(got) != 0 {
		t.Fatalf("unblocked = %v, want none: a hooked head must hold the rest", got)
	}

	// A live member refuses every open peer in the cluster regardless of order,
	// including the one the board would have reached first.
	live := append([]duplicateCandidate(nil), cluster...)
	live[2].Status = "in_progress"
	if got := unblocked(live); len(got) != 0 {
		t.Fatalf("unblocked = %v, want none: an in_progress peer holds the opens", got)
	}
}

// TestOpenClusterDispatchesPastAHeldHead is the gt-00gee acceptance bar, over
// the case gt-nlkba's rule cannot reach: a cluster of three open beads naming
// one test, whose head the dispatcher holds, dispatches its next eligible bead
// instead of none.
//
// Ordering the refusals let exactly one bead of an all-open cluster through —
// the head — so the cluster only progressed while the dispatcher could sling
// that head. A head the dispatcher holds for a reason of its own is never slung
// at all, and ranking it first refused the other two for as long as the hold
// lasted. Every hold the dispatcher takes a bead over is one of those reasons,
// which is what extends the bar past the shape gate to the rest of them
// (gt-mknmb): a hold label, a bead routed to a person, and a bead the ready
// query never lists because a dependency of it is still open.
func TestOpenClusterDispatchesPastAHeldHead(t *testing.T) {
	t.Parallel()
	townRoot := duplicateTown(t)
	const shared = "TestDangerousGuard_Integration fails in internal/cmd/tap_guard_dangerous.go."
	shaped := specTestDescription + "\n\n" + shared
	// The head's shape is the gate's whole input: the lint refuses a bead with
	// no Gate section.
	unshaped := strings.Replace(specTestDescription, "## Gate\nmake gate", "", 1) + "\n\n" + shared

	knobs := specHoldKnobs{
		MaxPriority: 2,
		ShapeGate:   specdispatch.ShapeGateWarn,
		Template:    specdispatch.Template{Sections: specdispatch.DefaultSections, Source: "built-in"},
	}

	// head is the cluster's first bead, varied per case: the same work, held for
	// a different reason each time.
	head := func(mutate func(*beads.Issue)) beads.Issue {
		h := beads.Issue{ID: "gt-head", Title: "guard blocks a nested command", Type: "task", Status: "open",
			Priority: 1, CreatedAt: "2026-10-15T09:00:00Z", Description: shaped,
			AcceptanceCriteria: specTestAcceptance}
		if mutate != nil {
			mutate(&h)
		}
		return h
	}

	// cluster is the three beads in board order — one test, one file, oldest
	// first — with the head the case asks for.
	cluster := func(h beads.Issue, extra ...beads.Issue) *beadsfake.Fake {
		db := beadsfake.New()
		db.Seed(h)
		db.Seed(
			beads.Issue{ID: "gt-mid", Title: "guard judges the argv after `gt slot run --`", Type: "task", Status: "open",
				Priority: 1, CreatedAt: "2026-10-15T10:00:00Z", Description: shaped,
				AcceptanceCriteria: specTestAcceptance},
			beads.Issue{ID: "gt-tail", Title: "the guard's tests are flaky under load", Type: "task", Status: "open",
				Priority: 1, CreatedAt: "2026-10-15T11:00:00Z", Description: shaped,
				AcceptanceCriteria: specTestAcceptance},
		)
		db.Seed(extra...)
		return db
	}

	// dispatched runs one dispatcher pass over the cluster in board order: a
	// bead the dispatcher holds is skipped before it can spend a seat, and a
	// bead the overlap guard lets past is slung. The pool is the pass's own
	// snapshot, so a bead slung mid-pass still reads as open in it — which is
	// what the bead behind it compares against, exactly as the burst path does.
	// The dispatcher's own hold question reads the peer's row in that same pool
	// (gt-mknmb).
	dispatched := func(db *beadsfake.Fake) []string {
		pools := newDuplicatePools(
			func(beadsDir string) ([]duplicateCandidate, error) {
				return fetchDuplicatePoolFrom(db, func(dir string, err error) {
					t.Errorf("enrichment of %s failed: %v", dir, err)
				}, beadsDir)
			},
			func(_, _ string, peer duplicateCandidate, pool []duplicateCandidate) string {
				return specPeerHeldFrom(db, knobs, peer, pool)
			},
		)
		beadsDir := duplicateBeadsDir(townRoot, "gt-head")
		if beadsDir == "" {
			t.Fatal("the town routes no gt- bead to a rig, so the pass has no pool to read")
		}
		pool, err := pools.poolFor(beadsDir)
		if err != nil {
			t.Fatalf("pool: %v", err)
		}
		var slung []string
		for _, id := range []string{"gt-head", "gt-mid", "gt-tail"} {
			if specPeerHeldFrom(db, knobs, poolRow(t, pool, id), pool) != "" {
				continue
			}
			issue, err := db.Show(id)
			if err != nil {
				t.Fatalf("read %s: %v", id, err)
			}
			candidate, matches, err := pools.check(townRoot, id, beadInfoOf(issue))
			if err != nil {
				t.Fatalf("check %s: %v", id, err)
			}
			if candidate == nil {
				t.Fatalf("%s names %s, so the guard has to inspect it", id, shared)
			}
			if decideSlingDuplicates(*candidate, matches).Blocked {
				continue
			}
			slung = append(slung, id)
		}
		return slung
	}

	// The blocked head waits on an open bead of its own that shares nothing with
	// the cluster, so it is the head's rank and not the blocker that the guard
	// weighs.
	blocker := beads.Issue{ID: "gt-blocker", Title: "nightly dashboard tick is slow", Type: "task", Status: "open",
		Priority: 1, CreatedAt: "2026-10-15T08:00:00Z", Description: "The dashboard's collector runs long under load."}

	cases := []struct {
		name  string
		head  beads.Issue
		extra []beads.Issue
		link  func(*beadsfake.Fake)
		want  string
	}{
		{name: "the head's shape is one the lint refuses",
			head: head(func(h *beads.Issue) { h.Description = unshaped }), want: "gt-mid"},
		{name: "the head is one only the crew takes",
			head: head(func(h *beads.Issue) { h.Labels = []string{specdispatch.CrewOnlyLabel} }), want: "gt-mid"},
		{name: "the head is one routed to a person",
			head: head(func(h *beads.Issue) { h.Labels = []string{"needs-human"} }), want: "gt-mid"},
		{name: "the head waits on a dependency that is still open",
			head: head(nil), extra: []beads.Issue{blocker},
			link: func(db *beadsfake.Fake) {
				if err := db.AddTypedDependency("gt-head", blocker.ID, "blocks"); err != nil {
					t.Fatalf("blocking edge gt-head -> %s: %v", blocker.ID, err)
				}
			}, want: "gt-mid"},
		{name: "the head's dependency landed, so the head goes",
			head: head(nil), extra: []beads.Issue{func() beads.Issue {
				closed := blocker
				closed.Status = "closed"
				return closed
			}()},
			link: func(db *beadsfake.Fake) {
				if err := db.AddTypedDependency("gt-head", blocker.ID, "blocks"); err != nil {
					t.Fatalf("blocking edge gt-head -> %s: %v", blocker.ID, err)
				}
			}, want: "gt-head"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := cluster(tc.head, tc.extra...)
			if tc.link != nil {
				tc.link(db)
			}
			if got := dispatched(db); strings.Join(got, " ") != tc.want {
				t.Errorf("dispatched %v, want [%s]: a head the dispatcher holds must not wedge its cluster, and one it can sling still holds it", got, tc.want)
			}
		})
	}
}

// TestOpenClusterBurstKeepsRefusingANotedDispatchedPeer is the gt-bvvab
// acceptance bar: the peer this same tick has already slung must keep refusing
// the beads behind it.
//
// A burst reads one pool for the whole tick, so a bead it slung mid-tick
// reaches the beads behind it only as the row noteDispatched files back into
// that pool. The row records the hook that landed, so the peer reads as work in
// flight and sling.Blocking refuses on the seat alone, whatever the two beads'
// ranks (gt-dzjtz). Filed under its pre-sling status instead, the peer read as
// unstarted work, and the refusal fell to the dispatcher's hold question, which
// took the stale open row as dispatchable — so the next bead of the tick went
// onto a seat the first one already held (gt-mcq, gt-bvvab).
//
// The cluster is the gt-nlkba shape — two ready beads naming one test, sharing
// no path — slung in board order, the way the gt-3vr/gt-rl0 double dispatch was
// actually filed.
func TestOpenClusterBurstKeepsRefusingANotedDispatchedPeer(t *testing.T) {
	t.Parallel()
	townRoot := duplicateTown(t)
	// The overlap is the test name alone: a shared path would also let the
	// dispatcher's own path hold settle the bead before the guard saw it.
	const shared = "TestDangerousGuard_Integration is the test that fails."
	shaped := specTestDescription + "\n\n" + shared

	db := beadsfake.New()
	// The head of the cluster, and the bead behind it in the same tick.
	db.Seed(beads.Issue{ID: "gt-lead", Title: "guard blocks a nested command", Type: "task", Status: "open",
		Priority: 1, CreatedAt: "2026-10-15T09:00:00Z", Description: shaped,
		AcceptanceCriteria: specTestAcceptance})
	db.Seed(beads.Issue{ID: "gt-next", Title: "guard judges the argv after `gt slot run --`", Type: "task", Status: "open",
		Priority: 2, CreatedAt: "2026-10-15T10:00:00Z", Description: shaped,
		AcceptanceCriteria: specTestAcceptance})

	knobs := specHoldKnobs{
		MaxPriority: 2,
		ShapeGate:   specdispatch.ShapeGateWarn,
		Template:    specdispatch.Template{Sections: specdispatch.DefaultSections, Source: "built-in"},
	}
	pools := newDuplicatePools(
		func(beadsDir string) ([]duplicateCandidate, error) {
			return fetchDuplicatePoolFrom(db, func(dir string, err error) {
				t.Errorf("enrichment of %s failed: %v", dir, err)
			}, beadsDir)
		},
		func(_, _ string, peer duplicateCandidate, pool []duplicateCandidate) string {
			return specPeerHeldFrom(db, knobs, peer, pool)
		},
	)
	beadsDir := duplicateBeadsDir(townRoot, "gt-lead")
	if beadsDir == "" {
		t.Fatal("the town routes no gt- bead to a rig, so the burst has no pool to read")
	}

	// check is one bead of the burst as sling.go runs it: the guard's verdict on
	// that bead against the tick's pool, through the same resolver the tick
	// wires.
	check := func(id string) (*duplicateCandidate, []duplicateMatch) {
		t.Helper()
		issue, err := db.Show(id)
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		candidate, matches, err := pools.check(townRoot, id, beadInfoOf(issue))
		if err != nil {
			t.Fatalf("check %s: %v", id, err)
		}
		if candidate == nil {
			t.Fatalf("%s names %s, so the guard has to inspect it", id, shared)
		}
		return candidate, matches
	}

	lead, leadMatches := check("gt-lead")
	if d := decideSlingDuplicates(*lead, leadMatches); d.Blocked {
		t.Fatalf("gt-lead heads the cluster and nothing precedes it: %s", d.Message)
	}

	// The lead's sling lands: the hook write makes it work in flight, and the
	// burst files its row back into the pool as hooked (gt-dzjtz).
	status, assignee := "hooked", "gastown/polecats/x"
	if err := db.Update("gt-lead", beads.UpdateOptions{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("hook gt-lead: %v", err)
	}
	pools.noteSlingCandidateDispatched(townRoot, lead)

	// The bead behind it in the same tick shares only the test name, so nothing
	// but the lead's own state can refuse it.
	next, matches := check("gt-next")
	var peer *duplicateMatch
	for i := range matches {
		if matches[i].Bead.ID == "gt-lead" {
			peer = &matches[i]
		}
	}
	if peer == nil {
		var got []string
		for _, m := range matches {
			got = append(got, m.Bead.ID)
		}
		t.Fatalf("the pool the tick is still reading must hold gt-lead as gt-next's peer, got %v", got)
	}
	if len(peer.SharedFiles) != 0 {
		t.Fatalf("gt-next shares %v with gt-lead, so the case is no longer the test-name-only overlap", peer.SharedFiles)
	}
	if !peer.Blocking(*next) {
		t.Errorf("Blocking(gt-next) = false over the gt-lead slung earlier in this tick: a peer whose seat is already spent must keep refusing, or the burst puts two seats on one defect")
	}
}

// TestNoteDispatchedMarksCachedPeerHooked is the gt-dzjtz acceptance bar: the
// row a landed sling files back into the tick's pool records the hook, so a
// peer of the just-slung bead is weighed against work in flight rather than
// against the open row the snapshot holds.
//
// Both shapes are covered. A bead the pool already holds is the common one — a
// batch's snapshot predates every hook the batch makes — and a bead appended
// after the fetch had the same hole, since it was appended under its pre-sling
// status.
func TestNoteDispatchedMarksCachedPeerHooked(t *testing.T) {
	t.Parallel()
	beadsDir := t.TempDir()
	cached := ranked(newDuplicateCandidate("gt-cached", "guard blocks a nested command", "open",
		"TestDangerousGuard_Integration fails in internal/cmd/tap_guard_dangerous.go."), 1, "2026-10-15T09:00:00Z")
	pools := newDuplicatePools(func(string) ([]duplicateCandidate, error) {
		return []duplicateCandidate{cached}, nil
	}, noPeerHolds)

	if status := poolRow(t, mustPool(t, pools, beadsDir), "gt-cached").Status; status != "open" {
		t.Fatalf("the snapshot row for gt-cached reads %q, want open", status)
	}

	// The cached bead's sling lands after the fetch, as it does in a burst. The
	// candidate is the row the bead's own show returned, which can differ from
	// the snapshot's in more than its status.
	landed := ranked(newDuplicateCandidate("gt-cached", "guard judges the argv after `gt slot run --`", "open",
		"TestDangerousGuard_Integration fails in internal/cmd/tap_guard_dangerous.go."), 2, "2026-10-15T09:30:00Z")
	pools.noteDispatched(beadsDir, landed)
	row := poolRow(t, mustPool(t, pools, beadsDir), "gt-cached")
	if row.Status != beads.StatusHooked {
		t.Errorf("the cached peer reads %q after its sling landed, want %q: an open row is work that may not precede the candidate weighing it, so the guard lets a second seat onto the defect", row.Status, beads.StatusHooked)
	}
	// The whole row is corrected, not just its status: a snapshot field left
	// behind is as stale as the status the note repairs (gt-fvak3).
	if row.Title != landed.Title || row.Priority != landed.Priority || row.CreatedAt != landed.CreatedAt {
		t.Errorf("the cached peer reads %+v after its sling landed, want the landed row %+v: the note corrects the whole row, not only its status", row, landed)
	}

	// A bead the snapshot never held is appended once its own hook lands.
	fresh := ranked(newDuplicateCandidate("gt-fresh", "guard judges the argv after `gt slot run --`", "open",
		"TestDangerousGuard_Integration fails in internal/cmd/tap_guard_dangerous.go."), 2, "2026-10-15T10:00:00Z")
	pools.noteDispatched(beadsDir, fresh)
	pool := mustPool(t, pools, beadsDir)
	if got := poolRow(t, pool, "gt-fresh").Status; got != beads.StatusHooked {
		t.Errorf("the appended bead reads %q after its sling landed, want %q", got, beads.StatusHooked)
	}
	if len(pool) != 2 {
		t.Errorf("the pool holds %d beads, want the snapshot's one plus the appended one", len(pool))
	}
}

// TestBatchSlingRefusesPeerOfJustSlungHeldLead is the gt-dzjtz acceptance bar
// over the seat the wrong rank wastes. gt-a outranks gt-b but the dispatcher
// holds it — routed to a person — so its rank never refuses gt-b. gt-b is slung
// first, and gt-a is then the bead behind it.
//
// Against a pool row left open, gt-a's check sees unstarted work that ranks
// behind it: Blocking is false, and the batch slings gt-a onto the seat gt-b
// already holds. The row records the hook instead, so the seat alone refuses
// whatever the two beads' ranks.
func TestBatchSlingRefusesPeerOfJustSlungHeldLead(t *testing.T) {
	t.Parallel()
	townRoot := duplicateTown(t)
	const shared = "TestDangerousGuard_Integration is the test that fails."
	shaped := specTestDescription + "\n\n" + shared

	db := beadsfake.New()
	db.Seed(beads.Issue{ID: "gt-a", Title: "guard blocks a nested command", Type: "task", Status: "open",
		Priority: 1, CreatedAt: "2026-10-15T09:00:00Z", Description: shaped,
		AcceptanceCriteria: specTestAcceptance, Labels: []string{"needs-human"}})
	db.Seed(beads.Issue{ID: "gt-b", Title: "guard judges the argv after `gt slot run --`", Type: "task", Status: "open",
		Priority: 2, CreatedAt: "2026-10-15T10:00:00Z", Description: shaped,
		AcceptanceCriteria: specTestAcceptance})

	knobs := specHoldKnobs{
		MaxPriority: 2,
		ShapeGate:   specdispatch.ShapeGateWarn,
		Template:    specdispatch.Template{Sections: specdispatch.DefaultSections, Source: "built-in"},
	}
	pools := newDuplicatePools(
		func(beadsDir string) ([]duplicateCandidate, error) {
			return fetchDuplicatePoolFrom(db, func(dir string, err error) {
				t.Errorf("enrichment of %s failed: %v", dir, err)
			}, beadsDir)
		},
		func(_, _ string, peer duplicateCandidate, pool []duplicateCandidate) string {
			return specPeerHeldFrom(db, knobs, peer, pool)
		},
	)
	beadsDir := duplicateBeadsDir(townRoot, "gt-a")
	if beadsDir == "" {
		t.Fatal("the town routes no gt- bead to a rig, so the batch has no pool to read")
	}

	// check is one bead of the batch as sling.go runs it: the guard's verdict
	// against the tick's pool, through the same resolver the tick wires.
	check := func(id string) (*duplicateCandidate, []duplicateMatch) {
		t.Helper()
		issue, err := db.Show(id)
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		candidate, matches, err := pools.check(townRoot, id, beadInfoOf(issue))
		if err != nil {
			t.Fatalf("check %s: %v", id, err)
		}
		if candidate == nil {
			t.Fatalf("%s names %s, so the guard has to inspect it", id, shared)
		}
		return candidate, matches
	}

	// The case only exists while gt-a is held: dispatchable, it outranks gt-b and
	// refuses it on the rank alone.
	pool := mustPool(t, pools, beadsDir)
	if why := specPeerHeldFrom(db, knobs, poolRow(t, pool, "gt-a"), pool); why == "" {
		t.Fatal("gt-a must be held, or it refuses gt-b before the batch reaches it")
	}

	b, bMatches := check("gt-b")
	if d := decideSlingDuplicates(*b, bMatches); d.Blocked {
		t.Fatalf("gt-b must be slung: the bead ahead of it is held, so nothing dispatchable precedes it: %s", d.Message)
	}

	// gt-b's sling lands and the batch files it back into the tick's pool.
	status, assignee := "hooked", "gastown/polecats/x"
	if err := db.Update("gt-b", beads.UpdateOptions{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("hook gt-b: %v", err)
	}
	pools.noteSlingCandidateDispatched(townRoot, b)

	// gt-a is the batch's other member, checked against the same tick's pool.
	a, aMatches := check("gt-a")
	decision := decideSlingDuplicates(*a, aMatches)
	if !decision.Blocked {
		t.Fatalf("gt-a was not refused against a pool where gt-b is already hooked: the batch puts two seats on one defect")
	}
	if !strings.Contains(decision.Message, "gt-b") {
		t.Errorf("the refusal must name the bead holding the seat, gt-b:\n%s", decision.Message)
	}
}

// poolRow is the pool's own row for id: the record the overlap guard weighs a
// peer by, and the one the dispatcher's hold questions read.
func poolRow(t *testing.T, pool []duplicateCandidate, id string) duplicateCandidate {
	t.Helper()
	for _, c := range pool {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("the pool has no row for %s", id)
	return duplicateCandidate{}
}

// TestSpecPeerHeldFrom covers the rule the guard reads a peer's dispatchability
// from: the questions the dispatcher asks a candidate before it spends a seat
// (gt-00gee), read off the peer's own pool row (gt-mknmb). "" means the
// dispatcher would sling the bead, so the guard must count it; anything else is
// a hold, so the guard must not.
func TestSpecPeerHeldFrom(t *testing.T) {
	t.Parallel()
	knobs := specHoldKnobs{
		MaxPriority: 2,
		ShapeGate:   specdispatch.ShapeGateWarn,
		Template:    specdispatch.Template{Sections: specdispatch.DefaultSections, Source: "built-in"},
	}
	off := knobs
	off.ShapeGate = specdispatch.ShapeGateOff
	// resting is the town's answer for a peer whose last sling failed at
	// session start: a throttle the bead's own fields do not carry, so the
	// guard has to be told it (gt-mqa3r).
	resting := off
	resting.Backoff = func(beadID string) string {
		if beadID == "gt-peer" {
			return "startup backoff: last sling failed at session start 2m ago"
		}
		return ""
	}
	unshape := func(p *duplicateCandidate) {
		p.Ready.Description = strings.Replace(p.Ready.Description, "## Gate\nmake gate", "", 1)
	}

	// peer is the pool row the rule reads, mutated as the case asks, and childDB
	// seeds the children the container rule reads from the store.
	peer := func(mutate func(*duplicateCandidate)) duplicateCandidate {
		p := duplicateCandidate{ID: "gt-peer", Title: "Fix TestFoo", Status: "open", Priority: 1, IssueType: "task",
			Ready: sling.Readiness{Description: specTestDescription, Acceptance: specTestAcceptance}}
		if mutate != nil {
			mutate(&p)
		}
		return p
	}
	childDB := func(children ...beads.Issue) *beadsfake.Fake {
		db := beadsfake.New()
		// The store needs the peer to hang the child edges off, though the rule
		// reads only the child set from it.
		db.Seed(beads.Issue{ID: "gt-peer", Title: "Fix TestFoo", Type: "task", Status: "open", Priority: 1})
		for _, child := range children {
			db.Seed(child)
			if err := db.AddTypedDependency(child.ID, "gt-peer", "parent-child"); err != nil {
				t.Fatalf("parent edge %s -> gt-peer: %v", child.ID, err)
			}
		}
		return db
	}

	cases := []struct {
		name  string
		db    *beadsfake.Fake
		peer  duplicateCandidate
		knobs specHoldKnobs
		want  string
	}{
		{"a clean work bead is the dispatcher's", beadsfake.New(), peer(nil), knobs, ""},
		{"the failed label puts a bead out of the queue", beadsfake.New(),
			peer(func(p *duplicateCandidate) { p.Ready.Labels = []string{specdispatch.DispatchFailedLabel} }), knobs,
			"label " + specdispatch.DispatchFailedLabel},
		{"the shape gate holds a bead the lint refuses", beadsfake.New(), peer(unshape), knobs,
			"## Gate: section missing"},
		{"a waived refusal dispatches anyway", beadsfake.New(),
			peer(func(p *duplicateCandidate) { unshape(p); p.Ready.Labels = []string{specdispatch.ShapeWaivedLabel} }), knobs, ""},
		{"an off gate runs no lint", beadsfake.New(), peer(unshape), off, ""},
		{"an assigned bead is not the dispatcher's to take", beadsfake.New(),
			peer(func(p *duplicateCandidate) { p.Ready.Assignee = "gastown/polecats/x" }), knobs, "assigned to gastown/polecats/x"},
		{"a bead past the ceiling is backlog", beadsfake.New(), peer(func(p *duplicateCandidate) { p.Priority = 3 }), knobs,
			"priority P3 outside the ceiling P2"},
		{"a type the dispatcher never takes", beadsfake.New(),
			peer(func(p *duplicateCandidate) { p.IssueType = "chore" }), knobs, "type chore"},
		{"an open blocking dependency is not on the ready board", beadsfake.New(),
			peer(func(p *duplicateCandidate) { p.Ready.OpenBlockers = []string{"gt-x"} }), knobs, "blocked by gt-x"},
		// The tick skips a bead whose formula declaration it cannot read, so the
		// bead is never slung with it (gt-j5cr2). Read as slingable, such a head
		// refused every lower-ranked bead sharing its test while it stayed
		// broken and was itself never dispatched (gt-7cmjp).
		{"a formula declaration the tick cannot read holds its bead", beadsfake.New(),
			peer(func(p *duplicateCandidate) {
				p.Ready.Labels = []string{specdispatch.FormulaLabelPrefix + "mol-a", specdispatch.FormulaLabelPrefix + "mol-b"}
			}), knobs, "conflicting formula labels: mol-a, mol-b"},
		{"a peer with many open blockers names the first few", beadsfake.New(),
			peer(func(p *duplicateCandidate) { p.Ready.OpenBlockers = []string{"gt-a", "gt-b", "gt-c", "gt-d"} }), knobs,
			"blocked by gt-a, gt-b, gt-c (+1 more)"},
		{"a bead resting after a failed start is not the dispatcher's next move", beadsfake.New(),
			peer(nil), resting, "startup backoff: last sling failed at session start 2m ago"},
		{"a container waits for its open child", childDB(beads.Issue{ID: "gt-child", Status: "open"}), peer(nil), knobs,
			"container: open child gt-child"},
		{"a container whose children closed is a candidate again",
			childDB(beads.Issue{ID: "gt-child", Status: "closed"}), peer(nil), knobs, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := specPeerHeldFrom(tc.db, tc.knobs, tc.peer, nil); got != tc.want {
				t.Errorf("specPeerHeldFrom = %q, want %q", got, tc.want)
			}
		})
	}
}

// A peer resting after a failed session start is a bead the dispatcher will not
// sling for up to an hour (dispatch.StartupBackoff). Read as the cluster's next
// move, it refused every bead sharing its test for the length of its rest: on
// devops 2026-10-25 that was 180 deferred slings in twenty minutes, each with a
// charged seat and a 7-8 second attempt, and no bead of the cluster dispatched
// (gt-mqa3r). The peer-hold question answers the rest, so the guard lets the
// cluster's next bead through.
func TestOverlapGuardLetsTheClusterPastAPeerInStartupBackoff(t *testing.T) {
	t.Parallel()
	peer := duplicateCandidate{
		ID: "gt-head", Title: "host rehearsal: the red tier sweep fails", Status: "open",
		IssueType: "task", Priority: 1, CreatedAt: "2026-10-25T18:40:00Z",
		Ready: sling.Readiness{Description: specTestDescription, Acceptance: specTestAcceptance},
		Refs:  extractOverlapRefs("TestHostRehearsal fails"),
	}
	candidate := duplicateCandidate{
		ID: "gt-tail", Title: "rework: the sweep again", Status: "open",
		IssueType: "task", Priority: 2, CreatedAt: "2026-10-25T19:00:00Z",
		Refs: extractOverlapRefs("TestHostRehearsal fails"),
	}
	townRoot := duplicateTown(t)
	heldReason := func() string {
		knobs, ok := specHoldKnobsFrom(townRoot, io.Discard)
		if !ok {
			t.Fatal("the town's settings would not load")
		}
		matches := findDuplicateMatches(candidate, []duplicateCandidate{peer})
		if len(matches) != 1 {
			t.Fatalf("matches = %+v, want the peer's shared test", matches)
		}
		return specPeerHeldFrom(beadsfake.New(), knobs, matches[0].Bead, nil)
	}

	// With no failure record the head is the dispatcher's next move, so the
	// guard refuses the tail and the cluster waits for it.
	if held := heldReason(); held != "" {
		t.Fatalf("held = %q before any failed start, want the peer dispatchable", held)
	}
	matches := findDuplicateMatches(candidate, []duplicateCandidate{peer})
	if !decideSlingDuplicates(candidate, matches).Blocked {
		t.Fatal("the guard let the tail past a head the dispatcher would sling")
	}

	// A failed start parks the head. The guard has to see the rest, or it
	// keeps refusing the tail with nothing slingable in the cluster.
	if err := dispatch.RecordStartupFailure(townRoot, peer.ID, "session start failed: the sandbox image lags the installed gt"); err != nil {
		t.Fatalf("recording the startup failure: %v", err)
	}
	held := heldReason()
	if held == "" {
		t.Fatal("the peer's startup backoff is not answered: the guard reads a resting head as next")
	}
	matches = findDuplicateMatches(candidate, []duplicateCandidate{peer})
	matches[0].Bead.Held = held
	if decideSlingDuplicates(candidate, matches).Blocked {
		t.Fatalf("the guard refused the cluster's next bead while the head rested (%s)", held)
	}

	// The answer is worth exactly the rest. Once the window has run out the
	// head is the dispatcher's next move again, so the guard refuses the tail
	// on its rank as it did before the failure: the exemption must not outlive
	// the record it reads (gt-36qvt).
	aged := fmt.Sprintf(`{"bead_id":%q,"count":1,"last_failure":%q,"reason":"session start failed"}`,
		peer.ID, time.Now().Add(-dispatch.StartupBackoffBase-time.Minute).UTC().Format(time.RFC3339))
	record := filepath.Join(townRoot, ".runtime", "sling-startup-failures", peer.ID+".json")
	if err := os.WriteFile(record, []byte(aged), 0o600); err != nil {
		t.Fatalf("aging the startup-failure record: %v", err)
	}
	if held := heldReason(); held != "" {
		t.Fatalf("held = %q after the rest elapsed, want the head next again", held)
	}
	matches = findDuplicateMatches(candidate, []duplicateCandidate{peer})
	if !decideSlingDuplicates(candidate, matches).Blocked {
		t.Fatal("the guard let the tail past a head whose rest had run out")
	}

	// The answer excuses a bead with no session, never one working the defect:
	// a head that took the seat is refused as live work on the shared test
	// even while a backoff record rests it. That refusal is sling.Blocking's
	// in-flight rule, not the backoff answer outliving its window (gt-36qvt).
	if err := dispatch.RecordStartupFailure(townRoot, peer.ID, "session start failed again"); err != nil {
		t.Fatalf("recording the second startup failure: %v", err)
	}
	held = heldReason()
	if held == "" {
		t.Fatal("a fresh rest was not answered")
	}
	live := peer
	live.Status = "in_progress"
	liveMatches := findDuplicateMatches(candidate, []duplicateCandidate{live})
	liveMatches[0].Bead.Held = held
	if !decideSlingDuplicates(candidate, liveMatches).Blocked {
		t.Fatalf("the backoff answer excused live work on the same test (%s)", held)
	}
}

// A peer the dispatcher will not sling because a working seat already has its
// defect is not the cluster's next move, however eligible it is. Without that
// question the peer-hold answer lapsed as soon as a rest ended: the head was
// next again, so the guard refused its cluster while the live work that held
// the head could run for hours — a refusal outliving the rest it replaced
// (gt-36qvt).
func TestOverlapGuardLetsTheClusterPastAPeerLiveWorkHolds(t *testing.T) {
	t.Parallel()
	townRoot := duplicateTown(t)
	db := beadsfake.New()
	// The head names two tests, one of which the live seat holds. The tail
	// names only the other, so it is no duplicate of the live work — only of
	// the head the live work keeps off the board.
	db.Seed(
		beads.Issue{ID: "gt-head", Type: "task", Title: "host rehearsal: the red tier sweep fails",
			Status: "open", Priority: 1, CreatedAt: "2026-10-25T18:40:00Z",
			Description: "TestHostRehearsal and TestPeerOnly fail."},
		beads.Issue{ID: "gt-peerwork", Type: "task", Title: "the peer-only sweep",
			Status: "in_progress", Priority: 1, Assignee: "gastown/polecats/capable",
			CreatedAt: "2026-10-25T18:30:00Z", Description: "TestPeerOnly fails."},
		beads.Issue{ID: "gt-tail", Type: "task", Title: "rework: the sweep again",
			Status: "open", Priority: 2, CreatedAt: "2026-10-25T19:00:00Z",
			Description: "TestHostRehearsal fails."},
	)
	knobs := specHoldKnobs{
		MaxPriority: 3, ShapeGate: specdispatch.ShapeGateOff, Warn: io.Discard,
		Backoff: func(string) string { return "" },
	}
	pools := newDuplicatePools(
		func(beadsDir string) ([]duplicateCandidate, error) {
			return fetchDuplicatePoolFrom(db, func(dir string, err error) {
				t.Errorf("enrichment of %s failed: %v", dir, err)
			}, beadsDir)
		},
		func(_, _ string, peer duplicateCandidate, pool []duplicateCandidate) string {
			return specPeerHeldFrom(db, knobs, peer, pool)
		},
	)
	tail, err := db.Show("gt-tail")
	if err != nil {
		t.Fatalf("read gt-tail: %v", err)
	}
	candidate, matches, err := pools.check(townRoot, "gt-tail", beadInfoOf(tail))
	if err != nil {
		t.Fatalf("check gt-tail: %v", err)
	}
	if len(matches) != 1 || matches[0].Bead.ID != "gt-head" {
		t.Fatalf("matches = %+v, want the head alone", matches)
	}
	held := matches[0].Bead.Held
	if !strings.Contains(held, "gt-peerwork") {
		t.Fatalf("Held = %q, want the live seat's bead named: the head is skipped, not next", held)
	}
	if d := decideSlingDuplicates(*candidate, matches); d.Blocked {
		t.Fatalf("the guard refused the cluster behind a head live work holds (%s): %s", held, d.Message)
	}
}

// TestSpecHoldKnobsFromReadsTheTown pins the wiring behind the peer-hold rule:
// the knobs are the dispatcher's own keys, read from the town's settings
// (polecat_pool.max_priority, polecat_pool.shape_gate), and an off gate loads
// no template at all.
func TestSpecHoldKnobsFromReadsTheTown(t *testing.T) {
	t.Parallel()

	t.Run("a town that set nothing gets the dispatcher's defaults", func(t *testing.T) {
		t.Parallel()
		knobs, ok := specHoldKnobsFrom(duplicateTown(t), io.Discard)
		if !ok {
			t.Fatal("a town with no settings file still answers the knobs")
		}
		if knobs.MaxPriority != config.DefaultSeatRefillMaxPriority || knobs.ShapeGate != config.DefaultSeatRefillShapeGate {
			t.Errorf("knobs = %+v, want the ceiling and gate the ticker defaults to", knobs)
		}
		if len(knobs.Template.Sections) == 0 {
			t.Errorf("knobs = %+v, want the template the shape gate lints against", knobs)
		}
	})

	t.Run("the town's own ceiling and gate win", func(t *testing.T) {
		t.Parallel()
		townRoot := duplicateTown(t)
		writeModelsSettings(t, townRoot, `{"polecat_pool":{"max_priority":1,"shape_gate":"off"}}`)
		knobs, ok := specHoldKnobsFrom(townRoot, io.Discard)
		if !ok {
			t.Fatal("a readable settings file answers the knobs")
		}
		if knobs.MaxPriority != 1 || knobs.ShapeGate != specdispatch.ShapeGateOff {
			t.Errorf("knobs = %+v, want the town's ceiling and gate", knobs)
		}
		if len(knobs.Template.Sections) != 0 {
			t.Errorf("an off gate has no verdict to reach, so it needs no template: %+v", knobs.Template)
		}
	})
}

// TestPrecedesIsTheReadyBoardOrder pins the guard's ranking to the dispatcher's
// (specdispatch.Order): lower priority number, then earlier creation, then bead
// ID. If the two drift, the bead the guard lets through is not the one the
// board would sling first, and the cluster deadlocks again (gt-nlkba).
func TestPrecedesIsTheReadyBoardOrder(t *testing.T) {
	t.Parallel()
	rows := []struct {
		id       string
		priority int
		created  string
	}{
		{"gt-b", 2, "2026-10-15T10:00:00Z"},
		{"gt-a", 1, "2026-10-15T11:00:00Z"},
		{"gt-c", 1, "2026-10-15T09:00:00.5Z"},
		{"gt-e", 1, "2026-10-15T09:00:00Z"},
		{"gt-d", 1, "2026-10-15T09:00:00Z"}, // level with gt-e: the ID breaks it
		{"gt-f", 1, ""},                     // no creation time at all
	}

	guarded := make([]duplicateCandidate, 0, len(rows))
	board := make([]specdispatch.Spec, 0, len(rows))
	for _, r := range rows {
		guarded = append(guarded, ranked(duplicateCandidate{ID: r.id}, r.priority, r.created))
		board = append(board, specdispatch.Spec{ID: r.id, Priority: r.priority, CreatedAt: r.created})
	}
	sort.Slice(guarded, func(i, j int) bool { return sling.Precedes(guarded[i], guarded[j]) })
	specdispatch.Order(board)

	guardOrder := make([]string, 0, len(guarded))
	for _, c := range guarded {
		guardOrder = append(guardOrder, c.ID)
	}
	specOrder := make([]string, 0, len(board))
	for _, s := range board {
		specOrder = append(specOrder, s.ID)
	}

	for i := range board {
		if guarded[i].ID != board[i].ID {
			t.Fatalf("guard order %v, board order %v", guardOrder, specOrder)
		}
	}
} // TestDuplicateOverlapSkipsEpics (gt-fudap): an epic's prose enumerates its
// children's tests and files rather than naming one defect, so a shared test
// name against an epic says nothing about duplication. The seat-refill failure
// behind this bead slung task gt-idv8s, which shares TestIntegration with open
// epic gt-ik4a1, and was refused — losing the reason to cobra's usage block.
func TestDuplicateOverlapSkipsEpics(t *testing.T) {
	t.Parallel()
	candidate := newDuplicateCandidate("gt-idv8s",
		"seat-refill: fill an empty polecat seat", "open",
		"TestIntegration fails when the seat is refilled in internal/cmd.")

	epicText := []string{"gt-ik4a1", "Epic: internal/cmd test cleanup", "open",
		"TestIntegration fails when the seat is refilled in internal/cmd."}

	epic := newDuplicateCandidate(epicText[0], epicText[1], epicText[2], epicText[3:]...)
	epic.IssueType = "epic"

	matches := findDuplicateMatches(candidate, []duplicateCandidate{epic})
	if len(matches) != 0 {
		t.Fatalf("an epic must not be an overlap candidate, got %+v", matches)
	}
	if d := decideSlingDuplicates(candidate, matches); d.Blocked {
		t.Fatalf("an epic overlap must not refuse the sling:\n%s", d.Message)
	}

	// Control: the same text on a plain task still refuses, so the exclusion is
	// the epic type and not the shared test itself. The task is live work, so
	// the refusal does not depend on which of the two ranks first.
	task := epic
	task.IssueType = "task"
	task.Status = "in_progress"
	taskMatches := findDuplicateMatches(candidate, []duplicateCandidate{task})
	if len(taskMatches) != 1 || !decideSlingDuplicates(candidate, taskMatches).Blocked {
		t.Fatalf("the same overlap with a non-epic task must still refuse; matches=%+v", taskMatches)
	}
}

// TestListDuplicateCandidatesCarriesIssueType (gt-fudap): the pool must keep
// each bead's issue type, or the epic exclusion above cannot fire on real data
// — bd list carries the type and the candidate reduction dropped it.
func TestListDuplicateCandidatesCarriesIssueType(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(beads.Issue{ID: "gt-ik4a1", Title: "Epic: internal/cmd test cleanup", Status: "open",
		Type:        "epic",
		Description: "TestIntegration fails when the seat is refilled in internal/cmd."})

	got, err := listDuplicateCandidates(listLikeBD{Client: db}, func(string, error) {}, t.TempDir(), []string{"open"}, time.Time{})
	if err != nil {
		t.Fatalf("listDuplicateCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("pool = %+v, want the epic gt-ik4a1", got)
	}
	if !got[0].IsEpic() {
		t.Errorf("candidate %s IssueType = %q, want epic", got[0].ID, got[0].IssueType)
	}
}

// A plan document is filtered however the prose spells its path: repo-relative,
// home-relative or absolute. A code path named beside it still counts.
func TestExtractOverlapRefsFiltersEveryPlanPathSpelling(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		text      string
		wantFiles []string
	}{
		{"repo-relative", "docs/plans/x.md", []string{}},
		{"home-relative", "~/gt/docs/plans/x.md", []string{}},
		{"absolute", "/Users/someone/gt/docs/plans/x.md", []string{}},
		{"rig-relative", "gastown/docs/plans/2026-10/x.md", []string{}},
		{"absolute beside a code path", "/Users/someone/gt/docs/plans/x.md and internal/cmd/sling.go", []string{"internal/cmd/sling.go"}},
		{"absolute sibling directory still counts", "/Users/someone/gt/docs/plans-archive/x.md", []string{"Users/someone/gt/docs/plans-archive/x.md"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertStrings(t, "files", extractOverlapRefs(tt.text).Files, tt.wantFiles)
		})
	}
}
