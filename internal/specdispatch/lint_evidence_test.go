package specdispatch

import (
	"strings"
	"testing"
)

// evidenceBoxSpec is goodSpec whose acceptance field is one box, so only that
// box and the Evidence section the test passes decide the refusal.
func evidenceBoxSpec(box string) Spec {
	s := goodSpec()
	s.Acceptance = "- [ ] " + box
	return s
}

// The refusal names the fixture path and what to do about it: run the rule on
// that fixture and paste the command and the output tail.
func TestSpecLintEvidenceRefuses(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, box, fixture string }{
		{"zero findings phrase", "zero findings on internal/specdispatch/testdata/ok.yaml", "internal/specdispatch/testdata/ok.yaml"},
		{"none of phrase", "none of the cases under internal/specdispatch/testdata/ apply", "internal/specdispatch/testdata"},
		{"no words on phrase", "no stray files on internal/specdispatch/testdata/", "internal/specdispatch/testdata"},
		{"passes on phrase", "the rule passes on internal/specdispatch/testdata/ok.yaml", "internal/specdispatch/testdata/ok.yaml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := evidenceBoxSpec(tc.box)
			r, blocked := evidenceRefusal(s, Sections(s.Description))
			if !blocked {
				t.Fatalf("evidenceRefusal(%q) = no refusal, want a refusal", tc.box)
			}
			if r.Field != "## Evidence" {
				t.Errorf("Field = %q, want %q", r.Field, "## Evidence")
			}
			for _, want := range []string{tc.fixture, "command", "output tail"} {
				if !strings.Contains(r.Reason, want) {
					t.Errorf("Reason = %q, want it to name %q", r.Reason, want)
				}
			}
		})
	}
}

// An internal/ path whose basename names a fixture is a fixture path with no
// testdata segment, and must not be skipped because it starts with internal/
// (the om major on the first attempt).
func TestSpecLintEvidenceInternalBasename(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, box string
		want      bool
	}{
		{"internal basename fixture", "zero findings on internal/specdispatch/fixture_ok.json", true},
		{"deploy path", "zero findings on deploy/specdispatch/values.yaml", true},
		{"plain internal path", "zero findings on internal/specdispatch/ok.json", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := evidenceBoxSpec(tc.box)
			r, blocked := evidenceRefusal(s, Sections(s.Description))
			if blocked != tc.want {
				t.Fatalf("evidenceRefusal(%q) blocked = %v (%q), want %v", tc.box, blocked, r.Reason, tc.want)
			}
			if tc.want && r.Field != "## Evidence" {
				t.Errorf("Field = %q, want %q", r.Field, "## Evidence")
			}
		})
	}
}

// A fixture with no zero claim, and a zero claim with no fixture path, are the
// two halves of the rule that must each stand alone.
func TestSpecLintEvidenceAllows(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, box string }{
		{"fixture without a zero claim", "the test reads internal/specdispatch/testdata/ok.json"},
		{"zero findings without a fixture path", "zero findings on internal/specdispatch/ok.json"},
		{"none of without a fixture path", "none of the lint cases are refused"},
		{"no words on without a fixture path", "no stray files on internal/specdispatch/ok.json"},
		{"passes on without a fixture path", "the gate passes on main"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := evidenceBoxSpec(tc.box)
			if r, blocked := evidenceRefusal(s, Sections(s.Description)); blocked {
				t.Fatalf("evidenceRefusal(%q) refused: %q", tc.box, r.Reason)
			}
		})
	}
}

// A usable fenced block holds at least one non-blank line between its markers;
// a lone fence marker and an empty pair show no command, so the refusal stands.
func TestSpecLintEvidenceFence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, section string
		blocked       bool
	}{
		{"content fenced block", "```\ngo test ./internal/specdispatch\nok\n```", false},
		{"dollar line", "$ go test ./internal/specdispatch\nok", false},
		{"empty section", "", true},
		{"lone fence marker", "```", true},
		{"empty fenced block", "```\n```", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := evidenceBoxSpec("zero findings on internal/specdispatch/testdata/ok.yaml")
			r, blocked := evidenceRefusal(s, map[string]string{"evidence": tc.section})
			if blocked != tc.blocked {
				t.Fatalf("evidenceRefusal(section %q) blocked = %v (%q), want %v", tc.section, blocked, r.Reason, tc.blocked)
			}
		})
	}
}

// A bead created before EvidenceRequiredFrom is grandfathered: the rule did not
// exist when it was filed, so it is not refused. A bead created at or after the
// cutoff is checked, and so is a bead whose CreatedAt is empty or unparseable
// (both parse to the zero time, which is not before the cutoff) (gt-qhrsa4).
func TestSpecLintEvidenceGrandfather(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		createdAt string
		blocked   bool
	}{
		{"before cutoff", "2026-11-19T23:59:59Z", false},
		{"at cutoff", "2026-11-20T00:00:00Z", true},
		{"after cutoff", "2026-11-20T00:00:01Z", true},
		{"empty CreatedAt", "", true},
		{"unparseable CreatedAt", "not a time", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := evidenceBoxSpec("zero findings on internal/specdispatch/testdata/ok.yaml")
			s.CreatedAt = tc.createdAt
			r, blocked := evidenceRefusal(s, Sections(s.Description))
			if blocked != tc.blocked {
				t.Fatalf("evidenceRefusal(CreatedAt %q) blocked = %v (%q), want %v", tc.createdAt, blocked, r.Reason, tc.blocked)
			}
		})
	}
}

// A usable Evidence section suppresses the refusal; a section that shows no
// command does not.
func TestSpecLintEvidenceSection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, section string
		want          bool
	}{
		{"fenced block", "```\ngo test ./internal/specdispatch -run TestOK\nok\n```", false},
		{"dollar line", "$ go test ./internal/specdispatch -run TestOK\nok", false},
		{"empty section", "", true},
		{"prose only", "I checked it and it was fine.", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := evidenceBoxSpec("zero findings on internal/specdispatch/testdata/ok.yaml")
			r, blocked := evidenceRefusal(s, map[string]string{"evidence": tc.section})
			if blocked != tc.want {
				t.Fatalf("evidenceRefusal(section %q) blocked = %v (%q), want %v", tc.section, blocked, r.Reason, tc.want)
			}
		})
	}
}
