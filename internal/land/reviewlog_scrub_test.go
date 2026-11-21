package land

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// The tokens are built from parts so this file holds no token-shaped literal.
var (
	awsKey    = "AK" + "IA" + strings.Repeat("Q", 16)
	googleKey = "AI" + "za" + strings.Repeat("b", 35)
	slackKey  = "xo" + "xb-" + strings.Repeat("9", 24)
)

func TestReviewLogScrubReplacesTokenShapedWords(t *testing.T) {
	t.Parallel()
	claim := "the config holds " + awsKey + " and " + googleKey + ", plus " + slackKey + " (leaked)"
	want := "the config holds <redacted> and <redacted>, plus <redacted> (leaked)"
	if got := scrubClaim(claim); got != want {
		t.Errorf("scrubClaim(%q) = %q, want %q", claim, got, want)
	}
}

func TestReviewLogScrubKeepsProseURLAndPath(t *testing.T) {
	t.Parallel()
	claim := "see https://example.com/docs/retry-and-timeouts and internal/land/reviewlog.go:180; the retry counter never resets"
	if got := scrubClaim(claim); got != claim {
		t.Errorf("scrubClaim(%q) = %q, want it unchanged", claim, got)
	}
}

// TestReviewLogScrubBoundHoldsNoSecret: the token straddles the bound, so a
// scrub after boundedClaim would keep its head.
func TestReviewLogScrubBoundHoldsNoSecret(t *testing.T) {
	t.Parallel()
	claim := strings.Repeat("x", maxRecordedClaimBytes-9) + " " + awsKey + " trailing words"
	got := reviewFindingTexts([]Finding{{Detail: claim}})[0].Claim
	if !strings.Contains(got, truncatedClaim) {
		t.Fatalf("the claim was not cut at the bound, so the test does not reach it:\n%q", got)
	}
	for _, word := range strings.FieldsFunc(got, func(r rune) bool { return r < 256 && claimSeparator(byte(r)) }) {
		if config.LooksLikeSecret("", word) || strings.Contains(word, "AK"+"IA") {
			t.Errorf("the recorded claim keeps %q", word)
		}
	}
}

func TestReviewLogScrubRecordedFindingText(t *testing.T) {
	t.Parallel()
	findings := []Finding{{
		ID: "f-1", Severity: omSeverityMajor, Path: "internal/land/note.go", Line: 12,
		Title:  "the refusal has no test",
		Detail: "the branch returns nil for " + awsKey + " instead of refusing",
	}}
	rec := reviewFindingTexts(findings)
	if len(rec) != 1 {
		t.Fatalf("reviewFindingTexts kept %d findings, want 1", len(rec))
	}
	if want := "the branch returns nil for <redacted> instead of refusing"; rec[0].Claim != want {
		t.Errorf("recorded claim = %q, want %q", rec[0].Claim, want)
	}
}

// TestReviewLogScrubHeldFinding: a held finding's claim reaches the log through a queue line.
func TestReviewLogScrubHeldFinding(t *testing.T) {
	t.Parallel()
	held := heldFindingTexts([]FollowupCandidate{{Title: "name the error", Claim: "the token " + slackKey + " is logged"}})
	if len(held) != 1 {
		t.Fatalf("heldFindingTexts kept %d findings, want 1", len(held))
	}
	if want := "the token <redacted> is logged"; held[0].Claim != want {
		t.Errorf("queued claim = %q, want %q", held[0].Claim, want)
	}
}
