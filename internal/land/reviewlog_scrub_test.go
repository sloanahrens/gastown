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

// scrubTestFamilies are the token families the rule's prefix list names, one for
// each provider the log saw: AWS, Google and Slack.
var scrubTestFamilies = map[string]string{"aws": awsKey, "google": googleKey, "slack": slackKey}

// TestReviewLogScrubJoinedTokens: a token joined to a key name, a label or a path
// by '=', ':' or '/' is one run of bytes that does not start like a token, so each
// part of the run is checked as well as the whole. The joined text is kept and
// only the token is replaced.
func TestReviewLogScrubJoinedTokens(t *testing.T) {
	t.Parallel()
	for family, token := range scrubTestFamilies {
		for _, joiner := range []string{"=", ":", "/"} {
			t.Run(family+joiner, func(t *testing.T) {
				t.Parallel()
				claim := "it logs KEY_NAME" + joiner + token + " verbatim"
				want := "it logs KEY_NAME" + joiner + "<redacted> verbatim"
				if got := scrubClaim(claim); got != want {
					t.Errorf("scrubClaim(%q) = %q, want %q", claim, got, want)
				}
			})
		}
	}
}

// TestReviewLogScrubTokenHoldingAJoiner: a token whose own bytes include a joiner
// is replaced whole by the whole-run check, not left with its tail after the
// first joiner (base64 secrets hold '/').
func TestReviewLogScrubTokenHoldingAJoiner(t *testing.T) {
	t.Parallel()
	token := "s" + "k-" + strings.Repeat("a", 8) + "/" + strings.Repeat("b", 8)
	if got, want := scrubClaim("the key "+token+" leaks"), "the key <redacted> leaks"; got != want {
		t.Errorf("scrubClaim = %q, want %q", got, want)
	}
}

// scrubTestSeparator is every byte the scrub splits on, words and joiners alike.
func scrubTestSeparator(r rune) bool {
	return r < 256 && (claimSeparator(byte(r)) || claimJoiner(byte(r)))
}

// TestReviewLogScrubBoundHoldsNoSecret: a token of each family straddles the
// bound, so a scrub after boundedClaim would keep its head. Every part of the
// recorded claim is held to the rule alone.
func TestReviewLogScrubBoundHoldsNoSecret(t *testing.T) {
	t.Parallel()
	for family, token := range scrubTestFamilies {
		t.Run(family, func(t *testing.T) {
			t.Parallel()
			claim := strings.Repeat("x", maxRecordedClaimBytes-9) + " k=" + token + " trailing words"
			got := reviewFindingTexts([]Finding{{Detail: claim}})[0].Claim
			if !strings.Contains(got, truncatedClaim) {
				t.Fatalf("the claim was not cut at the bound, so the test does not reach it:\n%q", got)
			}
			for _, part := range strings.FieldsFunc(got, scrubTestSeparator) {
				if config.LooksLikeSecret("", part) {
					t.Errorf("the recorded claim keeps %q", part)
				}
			}
		})
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
