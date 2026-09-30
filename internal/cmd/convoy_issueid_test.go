package cmd

import (
	"strings"
	"testing"

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
	t.Parallel()
	fx := newConvoyCLIFixture(t, convoyWriteBD(""))

	if err := fx.c.create(convoyCreateOptions{}, []string{"om-gate coverage: om", "om-1a2b", "om-3c4d", "om-5e6f"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	var tracked []string
	for _, line := range strings.Split(fx.bd.log(), "\n") {
		if strings.HasPrefix(line, "dep add ") {
			tracked = append(tracked, strings.Fields(line)[3])
		}
	}
	if got, want := strings.Join(tracked, ","), "om-1a2b,om-3c4d,om-5e6f"; got != want {
		t.Fatalf("tracked = %s, want %s", got, want)
	}
	if !strings.Contains(fx.out.String(), "om-gate coverage: om") {
		t.Fatalf("convoy name missing from output:\n%s", fx.out.String())
	}
}

// TestConvoyCreate_RejectsNonBeadIDTarget covers the other half of gt-gsky: a
// name passed where an issue belongs, after a real convoy name. Not an ID, so
// create refuses before writing the convoy instead of recording an edge no
// query can resolve.
func TestConvoyCreate_RejectsNonBeadIDTarget(t *testing.T) {
	t.Parallel()
	fx := newConvoyCLIFixture(t, convoyWriteBD(""))

	err := fx.c.create(convoyCreateOptions{}, []string{"test-convoy", "om-gate coverage: om"})
	if err == nil {
		t.Fatal("create accepted a non-bead-ID tracking target")
	}
	if !strings.Contains(err.Error(), `"om-gate coverage: om"`) {
		t.Fatalf("error should name the offending target, got: %v", err)
	}
	if strings.Contains(fx.bd.log(), "create ") {
		t.Fatalf("convoy was created before the target was refused:\n%s", fx.bd.log())
	}
}

// TestLooksLikeIssueID: a registered or legacy prefix, or a 2-3 letter
// lowercase one, reads as an issue ID; longer words and other shapes do not.
func TestLooksLikeIssueID(t *testing.T) {
	t.Parallel()
	testRegistry := session.NewPrefixRegistry()
	testRegistry.Register("nx", "nexus")
	testRegistry.Register("rpk", "nrpk")
	testRegistry.Register("longpfx", "longprefix")

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
		{"abc-max3", true},     // 3-char prefix matches heuristic
		{"abcd-four", false},   // 4-char unregistered prefix: not matched by heuristic
		{"abcde-five", false},  // 5-char prefix exceeds heuristic limit
		{"abcdef-max6", false}, // 6-char prefix exceeds heuristic limit
		{"test-plan", false},   // 4-char common word: not a false-positive
		{"gthq-deacon", true},  // legacy gthq prefix via HasKnownPrefix
		{"notvalid", false},
		{"no-hyphen-after", true}, // "no" is a 2-char lowercase prefix
		{"alpha-release", false},  // 5-char word: not a false-positive
		{"deploy-backend", false}, // 6-char word: not a false-positive
		{"A-uppercase", false},
		{"1-number", false},
		{"", false},
		{"-noprefix", false},
		{"a-tooshort", false},
		{"abcdefg-toolong", false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()
			got := looksLikeIssueIDIn(testRegistry, tc.input)
			if got != tc.want {
				t.Errorf("looksLikeIssueID(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}
