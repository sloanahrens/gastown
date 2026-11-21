package land

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// The tokens are built from parts so this file holds no token-shaped literal.
var scrubTestFamilies = map[string]string{
	"aws":    "AK" + "IA" + strings.Repeat("Q", 16),
	"google": "AI" + "za" + strings.Repeat("b", 35),
	"slack":  "xo" + "xb-" + strings.Repeat("9", 24),
	"asia":   "AS" + "IA" + strings.Repeat("Z", 16),
}

// scrubTestWrappers are the forms a token is quoted in: each is a format with the
// token at %s, and the want is the same format with the marker in its place.
var scrubTestWrappers = map[string]string{
	"bare": "%s", "backtick": "`%s`", "square": "[%s]", "angle": "<%s>", "star": "*%s*",
	"brace": "{%s}", "paren": "(%s)", "dquote": `"%s"`, "squote": "'%s'",
	"equals": "KEY_NAME=%s", "colon": "name:%s", "slash": "dir/%s", "hyphen": "NAME-%s",
	"query": "https://host/x?a=1&token=%s", "newline": `a\n%s`, "percent": "token%%3D%s",
}

// TestReviewLogScrubEveryFamilyInEveryWrapper: a token is replaced and the text
// around it kept, whatever the token is wrapped or joined with.
func TestReviewLogScrubEveryFamilyInEveryWrapper(t *testing.T) {
	t.Parallel()
	for family, token := range scrubTestFamilies {
		for name, wrap := range scrubTestWrappers {
			t.Run(family+"/"+name, func(t *testing.T) {
				t.Parallel()
				claim := "it logs " + fmt.Sprintf(wrap, token) + " verbatim"
				want := "it logs " + fmt.Sprintf(wrap, redactedWord) + " verbatim"
				if got := scrubClaim(claim); got != want {
					t.Errorf("scrubClaim(%q) = %q, want %q", claim, got, want)
				}
			})
		}
	}
}

// TestReviewLogScrubKeepsWhatIsNoSecret: prose, a hash, a UUID, an import path, a
// URL and a path with a line number come out unchanged, including words that
// contain a token prefix inside them (risk-assessment-framework holds sk-).
func TestReviewLogScrubKeepsWhatIsNoSecret(t *testing.T) {
	t.Parallel()
	for _, claim := range []string{
		"the retry counter never resets, see risk-assessment-framework and task-based-approaches",
		"commit 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b and id 123e4567-e89b-12d3-a456-426614174000",
		"import github.com/steveyegge/gastown/internal/land at https://example.com/docs/retry-and-timeouts",
		"see internal/land/reviewlog.go:180; the ASIA-PACIFIC-EAST-1 region name stays",
	} {
		if got := scrubClaim(claim); got != claim {
			t.Errorf("scrubClaim(%q) = %q, want it unchanged", claim, got)
		}
	}
}

// TestReviewLogScrubATokenFollowedByTokenBytes: the ASIA shape is whole-value, so a
// key id followed by token bytes ('+x', '-x', '.x', '/x', '=x', ':x') is cut at
// the joiner, or ('z9', 'key') where its upper-case bytes end, and replaced with the tail kept; a prefix token takes such a tail
// with it (more redaction, never less); a sentence's '.' or ':' after any token
// is not part of it.
func TestReviewLogScrubATokenFollowedByTokenBytes(t *testing.T) {
	t.Parallel()
	for _, tail := range []string{"+x", "-x", ".x", "/x", "=x", ":x", "z9", "key"} {
		claim := "id " + scrubTestFamilies["asia"] + tail + " end"
		if got, want := scrubClaim(claim), "id <redacted>"+tail+" end"; got != want {
			t.Errorf("scrubClaim(%q) = %q, want %q", claim, got, want)
		}
	}
	for _, family := range []string{"asia", "aws"} {
		for _, tail := range []string{".", ":"} {
			claim := "id " + scrubTestFamilies[family] + tail + " end"
			if got, want := scrubClaim(claim), "id <redacted>"+tail+" end"; got != want {
				t.Errorf("scrubClaim(%q) = %q, want %q", claim, got, want)
			}
		}
	}
}

// TestReviewLogScrubTokenHoldingAJoiner: a token whose own bytes include '/', as a
// base64 secret does, is replaced whole and not left with its tail.
func TestReviewLogScrubTokenHoldingAJoiner(t *testing.T) {
	t.Parallel()
	token := "s" + "k-" + strings.Repeat("a", 8) + "/" + strings.Repeat("b", 8)
	if got, want := scrubClaim("the key "+token+" leaks"), "the key <redacted> leaks"; got != want {
		t.Errorf("scrubClaim = %q, want %q", got, want)
	}
}

// TestReviewLogScrubTheCutHoldsNoTokenAndNoMarkerPart: a token straddling the
// bound leaves none of itself, a marker straddling it is not split, and the
// recorded claim is a fixed point of the scrub (the rule alone finds nothing in it).
func TestReviewLogScrubTheCutHoldsNoTokenAndNoMarkerPart(t *testing.T) {
	t.Parallel()
	for family, token := range scrubTestFamilies {
		t.Run(family, func(t *testing.T) {
			t.Parallel()
			for back := 0; back <= len(redactedWord)+2; back++ {
				claim := strings.Repeat("x", maxRecordedClaimBytes-back) + " `" + token + "` trailing words"
				got := reviewFindingTexts([]Finding{{Detail: claim}})[0].Claim
				if !strings.Contains(got, truncatedClaim) {
					t.Fatalf("back %d: the claim was not cut at the bound:\n%q", back, got)
				}
				body := strings.TrimSuffix(got, truncatedClaim)
				if scrubClaim(body) != body {
					t.Errorf("back %d: the recorded claim keeps a token-shaped run", back)
				}
				if i := strings.LastIndex(body, "<"); i >= 0 && body[i:] != redactedWord && strings.HasPrefix(redactedWord, body[i:]) {
					t.Errorf("back %d: the cut split a marker: %q", back, body[i:])
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
		Detail: "the branch returns nil for `" + scrubTestFamilies["aws"] + "` instead of refusing",
	}}
	rec := reviewFindingTexts(findings)
	if len(rec) != 1 {
		t.Fatalf("reviewFindingTexts kept %d findings, want 1", len(rec))
	}
	if want := "the branch returns nil for `<redacted>` instead of refusing"; rec[0].Claim != want {
		t.Errorf("recorded claim = %q, want %q", rec[0].Claim, want)
	}
}

// TestReviewLogScrubHeldFinding: a held finding's claim reaches the log through a queue line.
func TestReviewLogScrubHeldFinding(t *testing.T) {
	t.Parallel()
	held := heldFindingTexts([]FollowupCandidate{{Title: "name the error", Claim: "the token [" + scrubTestFamilies["slack"] + "] is logged"}})
	if len(held) != 1 {
		t.Fatalf("heldFindingTexts kept %d findings, want 1", len(held))
	}
	if want := "the token [<redacted>] is logged"; held[0].Claim != want {
		t.Errorf("queued claim = %q, want %q", held[0].Claim, want)
	}
}

// scrubTestPrefixes are the rule's own prefixes, read from it, so a prefix added
// to the rule is tested without a row for it.
func scrubTestPrefixes(distinctive bool) []string {
	var out []string
	for _, p := range config.SecretValuePrefixes() {
		if (len(p) >= distinctivePrefixLen) == distinctive {
			out = append(out, p)
		}
	}
	return out
}

// TestReviewLogScrubDistinctivePrefixesAreFoundGlued: a prefix too long to occur
// in an ordinary word (AKIA, AIza, xoxb-, ghp_ ...) starts a token wherever it is,
// glued to a letter, a digit or an underscore before it.
func TestReviewLogScrubDistinctivePrefixesAreFoundGlued(t *testing.T) {
	t.Parallel()
	prefixes := append(scrubTestPrefixes(true), "AS"+"IA")
	if len(prefixes) < 5 {
		t.Fatalf("only %d distinctive prefixes: the rule's list was not read", len(prefixes))
	}
	for _, p := range prefixes {
		token := p + strings.Repeat("Q", 16)
		for _, before := range []string{"k", "Z", "7", "_"} {
			claim := "pre" + before + token + " post"
			if got, want := scrubClaim(claim), "pre"+before+"<redacted> post"; got != want {
				t.Errorf("scrubClaim(%q) = %q, want %q", claim, got, want)
			}
		}
	}
}

// TestReviewLogScrubAmbiguousPrefixesKeepTheGlueRule: a short prefix (sk-, sk_)
// occurs inside ordinary words, so it starts a token only after a byte that is
// not a letter or digit; glued to a letter it is left, which is the one residual.
func TestReviewLogScrubAmbiguousPrefixesKeepTheGlueRule(t *testing.T) {
	t.Parallel()
	prefixes := scrubTestPrefixes(false)
	if len(prefixes) == 0 {
		t.Fatal("no ambiguous prefix: the rule's list was not read")
	}
	for _, p := range prefixes {
		inWord := "the ri" + p + "assessment-framework stays"
		if got := scrubClaim(inWord); got != inWord {
			t.Errorf("scrubClaim(%q) = %q, want it unchanged", inWord, got)
		}
		token := p + strings.Repeat("q", 16)
		if got, want := scrubClaim("key "+token+" end"), "key <redacted> end"; got != want {
			t.Errorf("an unglued %q token: scrubClaim = %q, want %q", p, got, want)
		}
	}
}

// scrubBounded runs the scrub and fails the test if it does not return within
// the test's bound, so a regression that goes quadratic or worse fails and does
// not hang the suite.
func scrubBounded(t *testing.T, claim string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan string, 1)
	go func() { done <- scrubClaim(claim) }()
	select {
	case got := <-done:
		return got
	case <-ctx.Done():
		t.Fatalf("scrubClaim did not return within the bound on a %d-byte claim", len(claim))
		return ""
	}
}

// TestReviewLogScrubLongRunsAreBounded: a long claim made of one run with many
// joiner bytes (a path, a base64 blob, a dashed hex dump, an id list), including
// runs that start with the ASIA shape at every few bytes, is scrubbed in bounded
// time: the cost is fixed by scrubInputBound and maxTokenRun, not by the claim.
func TestReviewLogScrubLongRunsAreBounded(t *testing.T) {
	t.Parallel()
	for _, unit := range []string{"a-", "a_", "a.", "a/", "a+", "a=", "a:", "ASIAa-", "ASIA-", "ASIAQ/"} {
		t.Run(unit, func(t *testing.T) {
			t.Parallel()
			claim := strings.Repeat(unit, 20000)
			got := scrubBounded(t, claim)
			if len(got) > scrubInputBound {
				t.Errorf("scrubClaim returned %d bytes of a %d-byte claim, more than the %d it reads", len(got), len(claim), scrubInputBound)
			}
		})
	}
}

// TestReviewLogScrubATokenRunIsTriedUpToMaxTokenRun: no real token is longer than
// maxTokenRun, so a longer run is taken in pieces: the first piece is replaced
// and the rest, which starts no prefix, is kept.
func TestReviewLogScrubATokenRunIsTriedUpToMaxTokenRun(t *testing.T) {
	t.Parallel()
	claim := scrubTestFamilies["aws"] + strings.Repeat("Q", maxTokenRun)
	got := scrubBounded(t, claim)
	if want := redactedWord + strings.Repeat("Q", len(claim)-maxTokenRun); got != want {
		t.Errorf("scrubClaim of a %d-byte run kept %d bytes after the marker, want %d", len(claim), len(got)-len(redactedWord), len(claim)-maxTokenRun)
	}
}
