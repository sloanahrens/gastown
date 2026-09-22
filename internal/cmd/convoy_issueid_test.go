package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/session"
)

// TestConvoyCreate_ProseNameStaysAName reproduces the reported convoy
// (gt-gsky), whose title was itself recorded as a tracked issue.
//
// `gt convoy create "om-gate coverage: om" om-a om-b om-c` is name-then-issues,
// but looksLikeIssueID fires on the name (it opens with the 2-letter "om"
// prefix). That folded the name into the tracked set, where the cross-rig
// resolver turned it into external:om:om-gate coverage: om — an edge nothing
// can resolve, which held the convoy open forever.
func TestConvoyCreate_ProseNameStaysAName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows - shell stubs")
	}

	townRoot, _ := makeRoutingTownWorkspace(t)
	chdirConvoyTest(t, townRoot)

	beadsDir := filepath.Join(townRoot, ".beads")
	_ = os.WriteFile(filepath.Join(beadsDir, ".gt-types-configured"), []byte(beads.TypeConfigSentinelValue()), 0644)
	_ = os.WriteFile(filepath.Join(beadsDir, ".gt-statuses-configured"), []byte("staged_ready,staged_warnings"), 0644)

	var tracked []string
	oldAddTracking := addTrackingRelationFn
	addTrackingRelationFn = func(townRoot, convoyID, issueID string) error {
		tracked = append(tracked, issueID)
		return nil
	}
	t.Cleanup(func() { addTrackingRelationFn = oldAddTracking })

	scriptBody := `
case "$1" in
  create)
    echo '[{"id":"hq-cv-test"}]'
    ;;
  init|config)
    exit 0
    ;;
  *)
    echo '[]'
    ;;
esac
`
	writeRoutingBdStub(t, scriptBody)

	out, err := captureConvoyStdoutErr(t, func() error {
		return runConvoyCreate(nil, []string{"om-gate coverage: om", "om-1a2b", "om-3c4d", "om-5e6f"})
	})
	if err != nil {
		t.Fatalf("runConvoyCreate: %v", err)
	}

	wantTracked := []string{"om-1a2b", "om-3c4d", "om-5e6f"}
	if len(tracked) != len(wantTracked) {
		t.Fatalf("tracked = %v, want %v", tracked, wantTracked)
	}
	for i, id := range wantTracked {
		if tracked[i] != id {
			t.Fatalf("tracked = %v, want %v", tracked, wantTracked)
		}
	}

	if !strings.Contains(out, "om-gate coverage: om") {
		t.Fatalf("convoy name missing from output:\n%s", out)
	}
}

// TestConvoyCreate_RejectsNonBeadIDTarget covers the other half of gt-gsky: a
// name passed where an issue belongs, after a real convoy name. Not an ID, so
// create refuses before writing the convoy instead of recording an edge no
// query can resolve.
func TestConvoyCreate_RejectsNonBeadIDTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on windows - shell stubs")
	}

	townRoot, _ := makeRoutingTownWorkspace(t)
	chdirConvoyTest(t, townRoot)

	// Any bd call reaching create fails the test: the validation must refuse
	// before the convoy is created.
	scriptBody := `
case "$1" in
  create)
    echo "bd create must not be called" >&2
    exit 1
    ;;
  init|config)
    exit 0
    ;;
  *)
    echo '[]'
    ;;
esac
`
	writeRoutingBdStub(t, scriptBody)

	_, err := captureConvoyStdoutErr(t, func() error {
		return runConvoyCreate(nil, []string{"test-convoy", "om-gate coverage: om"})
	})
	if err == nil {
		t.Fatal("runConvoyCreate accepted a non-bead-ID tracking target")
	}
	if !strings.Contains(err.Error(), `"om-gate coverage: om"`) {
		t.Fatalf("error should name the offending target, got: %v", err)
	}
}

func TestLooksLikeIssueID(t *testing.T) {
	originalRegistry := session.DefaultRegistry()
	t.Cleanup(func() { session.SetDefaultRegistry(originalRegistry) })

	testRegistry := session.NewPrefixRegistry()
	testRegistry.Register("nx", "nexus")
	testRegistry.Register("rpk", "nrpk")
	testRegistry.Register("longpfx", "longprefix")
	session.SetDefaultRegistry(testRegistry)

	tests := []struct {
		input string
		want  bool
	}{
		{"gt-abc123", true},
		{"bd-xyz789", true},
		{"hq-mayor", true},
		{"nx-def456", true},
		{"rpk-ghi012", true},
		{"longpfx-jkl345", true},
		{"nv-short", true},
		{"ab-min", true},
		{"abc-max3", true},            // 3-char prefix matches heuristic
		{"abcd-four", false},          // 4-char unregistered prefix: not matched by heuristic
		{"abcde-five", false},         // 5-char prefix exceeds heuristic limit
		{"abcdef-max6", false},        // 6-char prefix exceeds heuristic limit
		{"test-plan", false},          // 4-char common word: not a false-positive
		{"gthq-deacon", true},         // legacy gthq prefix via HasKnownPrefix
		{"notvalid", false},
		{"no-hyphen-after", true},     // "no" is a 2-char lowercase prefix
		{"alpha-release", false},      // 5-char word: not a false-positive
		{"deploy-backend", false},     // 6-char word: not a false-positive
		{"A-uppercase", false},
		{"1-number", false},
		{"", false},
		{"-noprefix", false},
		{"a-tooshort", false},
		{"abcdefg-toolong", false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := looksLikeIssueID(tc.input)
			if got != tc.want {
				t.Errorf("looksLikeIssueID(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}
