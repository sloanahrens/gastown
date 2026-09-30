package land

import (
	"context"
	"strings"
	"testing"
)

func TestAttributionLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		msg  string
		want bool
	}{
		{"feat: x\n\nCo-Authored-By: Claude Opus <noreply@anthropic.com>", true},
		{"feat: x\n\nco-authored-by: Anthropic Bot <bot@example.com>", true},
		{"feat: x\n\nSigned-off-by: Claude <c@example.com>", true},
		{"feat: x\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)", true},
		{"feat: x\n\nGenerated with Claude", true},
		{"feat: x\n\nsome 🤖 badge", true},
		// Product mentions are not attribution (2026-09-30 false positives).
		{"fix: Claude Code hooks fire on Bash(*)", false},
		{"docs: explain the anthropic API key rotation", false},
		{"feat: x\n\nCo-Authored-By: Jane Doe <jane@example.com>", false},
		{"feat: x\n\nSigned-off-by: Sloan Ahrens <s@example.com>", false},
		{"feat: generated with go generate", false},
	} {
		if got := AttributionLine(tc.msg) != ""; got != tc.want {
			t.Errorf("AttributionLine(%q) found=%v, want %v", tc.msg, got, tc.want)
		}
	}
}

func TestLandRefusesAttributionTrailerAsRework(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.setBranch("feat: add b\n\nCo-Authored-By: Claude Opus <noreply@anthropic.com>")
	l := f.lander()
	l.RangeChecks = []RangeCheck{AttributionCheck}
	_, err := l.Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectPolicy, LabelRework)
	if !strings.Contains(rej.Reason, "attribution") {
		t.Errorf("reason %q does not name the attribution", rej.Reason)
	}
	if len(f.gate.dirs) != 0 {
		t.Error("an attributed range was gated")
	}
}

func TestLandAllowsProductMention(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.setBranch("fix: Claude Code hooks fire on every Bash call")
	l := f.lander()
	l.RangeChecks = []RangeCheck{AttributionCheck}
	// The same landing also proves a disabled review lands as "skipped".
	l.Reviewer = SkipReviewer{}
	res, err := l.Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if got := f.originMain(); got != res.LandedCommit {
		t.Errorf("origin/main = %s, want %s", got, res.LandedCommit)
	}
	if lines := f.landingLines(); len(lines) != 1 || !strings.Contains(lines[0], `"om_verdict":"skipped"`) {
		t.Errorf("landing record: %q", lines)
	}
}
