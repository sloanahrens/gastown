package land

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/land/policy"
)

// omHelpWithRoundFlags is the help an om that takes the context flags prints: the
// brief, policy, rounds and round flags beside the bead flag (gt-3l5qy).
const omHelpWithRoundFlags = `Usage:
  om review [flags]

Flags:
  --base string         the base of the range
  --bead string         the work bead the diff belongs to
  --bead-file path      the work-item file
  --head string         the head of the range
  --policy-file path    the town-policy file
  --round int           the landing attempt this review belongs to
  --rounds-file path    the rounds ledger, JSON
`

// contextOM is a fake om for the review argv: it answers the version and help
// probes, and on a review it reads back the contents of each context file the
// argv names, as om does, while the file still exists.
type contextOM struct {
	t     *testing.T
	help  string
	argv  []string
	files map[string]string
}

func (s *contextOM) run(_ context.Context, _ string, _ []string, argv []string, out io.Writer) (int, error) {
	if omSubcommand(argv) == "version" {
		_, _ = io.WriteString(out, stubVersionLine())
		return 0, nil
	}
	if slices.Contains(argv, "--help") {
		_, _ = io.WriteString(out, s.help)
		return 2, nil
	}
	s.argv = slices.Clone(argv)
	s.files = map[string]string{}
	for i, a := range argv {
		switch a {
		case "--bead-file", "--policy-file", "--rounds-file":
			data, err := os.ReadFile(argv[i+1])
			if err != nil {
				s.t.Fatalf("om cannot read %s: %v", a, err)
			}
			s.files[a] = string(data)
		case "--out":
			if err := os.WriteFile(argv[i+1], []byte(`{"score":0.9,"verdict":"approve","findings":[]}`), 0o600); err != nil {
				s.t.Fatal(err)
			}
		}
	}
	_, _ = io.WriteString(out, "om: reviewing\n")
	return 0, nil
}

// argvFlagValue is the argv word after flag, and whether the flag is there at all.
func argvFlagValue(argv []string, flag string) (string, bool) {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1], true
		}
	}
	return "", false
}

// The landing's review hands om the work-item file, the policy, the rounds
// ledger and the round number, read back here as om reads them. This runs the
// real landing path with a fake om, so it covers the call site that builds the
// review input (gt-3l5qy).
func TestLandHandsOMTheBriefPolicyRoundsAndRound(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	stub := &contextOM{t: t, help: omHelpWithRoundFlags}
	l := f.lander()
	l.Reviewer = OMReviewer{OutDir: t.TempDir(), run: stub.run}
	l.RoundsDir = t.TempDir()
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}
	if stub.argv == nil {
		t.Fatal("om was never run")
	}
	brief, ok := argvFlagValue(stub.argv, "--bead-file")
	if !ok || !strings.HasPrefix(stub.files["--bead-file"], "# Work item") {
		t.Errorf("--bead-file = %q (present %v), want a work-item file om reads back", brief, ok)
	}
	if got := stub.files["--policy-file"]; got != policy.PolicyBlock() {
		t.Errorf("--policy-file content differs from policy.PolicyBlock()")
	}
	if got := stub.files["--rounds-file"]; got != "[]" {
		t.Errorf("--rounds-file = %q, want the empty ledger of a first review", got)
	}
	round, ok := argvFlagValue(stub.argv, "--round")
	if n, err := strconv.Atoi(round); !ok || err != nil || n < 1 {
		t.Errorf("--round = %q (present %v), want the landing attempt", round, ok)
	}
}

// An om whose help lacks the flags is run with today's argv, and a warning names
// each flag it does not take (gt-3l5qy).
func TestReviewBeadWarnsNamingEachFlagAnOlderOMLacks(t *testing.T) {
	t.Parallel()
	stub := &contextOM{t: t, help: omHelpWithBead}
	var warnings []string
	r := OMReviewer{OutDir: t.TempDir(), run: stub.run}
	_, err := r.ReviewBead(context.Background(), ReviewInput{Repo: baseRepo{},
		Dir: t.TempDir(), Base: "aaa", Head: "bbb", BeadID: "gt-abc",
		BriefText: "# Work item\n", PolicyText: "policy", RoundsText: "[]", Round: 2,
		Logf: func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) },
	})
	if err != nil {
		t.Fatalf("ReviewBead: %v", err)
	}
	for _, flag := range []string{"--bead-file", "--policy-file", "--rounds-file", "--round"} {
		if slices.Contains(stub.argv, flag) {
			t.Errorf("argv carries %s, which the old om does not take: %v", flag, stub.argv)
		}
		if !slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, flag) }) {
			t.Errorf("no warning names %s; warnings: %q", flag, warnings)
		}
	}
}
