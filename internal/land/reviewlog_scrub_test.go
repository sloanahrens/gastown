package land

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// TestReviewLogScrubATokenBesideAScheme: the rule refuses any value containing
// "://", so a token followed by a joiner and a URL, or by "://" itself, is cut at
// the "://": the longest cut the rule accepts, so no byte of a token that holds a
// joiner of its own survives (the scheme word before "://" may go with it); the
// ASIA shape, which the rule takes whole, falls back to the joiner cut and keeps
// the scheme word. The URL's own tail stays.
func TestReviewLogScrubATokenBesideAScheme(t *testing.T) {
	t.Parallel()
	aws, sk, ghp := scrubTestFamilies["aws"], "s"+"k-"+"abcdefghijklmnop1234", "gh"+"p_"+strings.Repeat("a", 20)
	for claim, want := range map[string]string{
		aws + "/https://x.test":                  "<redacted>://x.test",
		sk + "/https://x.test/path":              "<redacted>://x.test/path",
		ghp + ":https://x":                       "<redacted>://x",
		aws + "://x":                             "<redacted>://x",
		scrubTestFamilies["asia"] + "/https://x": "<redacted>/https://x",
		"see (" + sk + "://host)":                "see (<redacted>://host)",
		"sk-" + strings.Repeat("a", 6) + "/" + strings.Repeat("b", 12) + "://x": "<redacted>://x",
	} {
		if got := scrubBounded(t, claim); got != want {
			t.Errorf("scrubClaim(%q) = %q, want %q", claim, got, want)
		}
	}
}

// TestReviewLogScrubATokenWithAnInnerJoinerBesideAScheme: a token whose own body
// holds a joiner, followed by each form of scheme, loses every byte: no 12-byte
// window of it is in the output. A cut at the joiner before the scheme word would
// land inside the token and leave its tail.
func TestReviewLogScrubATokenWithAnInnerJoinerBesideAScheme(t *testing.T) {
	t.Parallel()
	token := "github" + "_pat_" + "11ABCDEFG0abcdefghijkl_mnopqrstuvwxyz"
	for _, shape := range []string{"%s://x", "(%s://host)", "%s/https://x", "%s:https://x", "k=%s://x", "%s.https://x", "%s-https://x"} {
		claim := fmt.Sprintf(shape, token)
		got := scrubBounded(t, claim)
		for i := 0; i+12 <= len(token); i++ {
			if strings.Contains(got, token[i:i+12]) {
				t.Errorf("scrubClaim(%q) = %q keeps the credential bytes %q", claim, got, token[i:i+12])
				break
			}
		}
	}
}

// TestReviewLogScrubATokenPastTheRunCapLeavesNoTail: a token longer than
// maxTokenRun is replaced through the whole contiguous run of token bytes, not
// only its first maxTokenRun: the rest of it is credential bytes too.
func TestReviewLogScrubATokenPastTheRunCapLeavesNoTail(t *testing.T) {
	t.Parallel()
	for _, n := range []int{maxTokenRun + 88, 5000} {
		claim := "key gh" + "p_" + strings.Repeat("a", n) + " end"
		if got, want := scrubBounded(t, claim), "key <redacted> end"; got != want {
			t.Errorf("a %d-byte token: scrubClaim = %.40q, want %q", n, got, want)
		}
	}
	million := scrubBounded(t, strings.Repeat("a-", 500000))
	if len(million) > scrubInputBound {
		t.Errorf("a million bytes returned %d, more than the %d read", len(million), scrubInputBound)
	}
}

// scrubTestThreeRows is a log of three rows: one whose finding text quotes the
// token, one quoting prose, and a count-only verdict.
func scrubTestThreeRows(token string) []ReviewRecord {
	return []ReviewRecord{
		{BeadID: "gt-slit1", Rig: "gastown", Verdict: VerdictApprove, Score: 0.9,
			FindingText: []ReviewFindingText{{ID: "f-1", Title: "the key leaks", Claim: "it logs `" + token + "` verbatim"}}},
		{BeadID: "gt-slit2", Rig: "gastown", Verdict: VerdictApprove, Score: 0.8,
			FindingText: []ReviewFindingText{{ID: "f-1", Claim: "the retry counter never resets"}}},
		{BeadID: "gt-slit3", Rig: "gastown", Verdict: VerdictRequestChanges, Score: 0.3, Findings: 2},
	}
}

// scrubTestLogBytes encodes rows as the log's own lines.
func scrubTestLogBytes(t *testing.T, recs ...ReviewRecord) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, rec := range recs {
		line, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("encoding a test row: %v", err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// writeScrubTestLog writes a log and returns a ReviewsFile over it.
func writeScrubTestLog(t *testing.T, data []byte) (*ReviewsFile, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), ReviewsFileName)
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatalf("writing the test log: %v", err)
	}
	return &ReviewsFile{Path: path}, path
}

// assertNoScrubToken fails if text holds the token or a run of its bytes long
// enough to be a value: a report and an error name rows, never a value.
func assertNoScrubToken(t *testing.T, text, token string) {
	t.Helper()
	if strings.Contains(text, token) {
		t.Fatal("the text holds the planted token")
	}
	if n := len(token) / 2; n >= 8 && strings.Contains(text, token[:n]) {
		t.Fatalf("the text holds the first %d bytes of the planted token", n)
	}
}

// TestReviewLogScrubPlanDryRunReportsAndLeavesTheFile: a plan reports the
// one changed row by bead id and word count and leaves the log byte for byte.
func TestReviewLogScrubPlanDryRunReportsAndLeavesTheFile(t *testing.T) {
	t.Parallel()
	token := scrubTestFamilies["aws"]
	before := scrubTestLogBytes(t, scrubTestThreeRows(token)...)
	logf, path := writeScrubTestLog(t, before)
	res, err := logf.ScrubPlan()
	if err != nil {
		t.Fatalf("ScrubPlan: %v", err)
	}
	if res.Rows != 3 || res.RowsChanged != 1 {
		t.Errorf("plan reported Rows=%d RowsChanged=%d, want 3 and 1", res.Rows, res.RowsChanged)
	}
	if res.Words != 1 {
		t.Errorf("plan reported Words=%d, want 1", res.Words)
	}
	if len(res.Beads) != 1 || res.Beads[0] != "gt-slit1" {
		t.Errorf("plan reported Beads=%v, want [gt-slit1]", res.Beads)
	}
	if res.Oversize != 0 || res.Skipped != 0 || len(res.SkippedBeads) != 0 {
		t.Errorf("plan reported Oversize=%d Skipped=%d SkippedBeads=%v, want zero", res.Oversize, res.Skipped, res.SkippedBeads)
	}
	assertNoScrubToken(t, fmt.Sprintf("%+v", res), token)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the log after the plan: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Error("the plan changed the log")
	}
}

// TestReviewLogScrubPlanCountsWhatItCannotDecode: a line that is not a JSON
// object and one past the byte bound are copied through untouched and counted
// while the scrub rewrites a row beside them.
func TestReviewLogScrubPlanCountsWhatItCannotDecode(t *testing.T) {
	t.Parallel()
	token := scrubTestFamilies["aws"]
	longLine := strings.Repeat("y", 600)
	before := append(scrubTestLogBytes(t, scrubTestThreeRows(token)...), []byte("not a JSON row\nnull\n"+longLine+"\n")...)
	logf, path := writeScrubTestLog(t, before)
	logf.lineLimit = 512
	res, err := logf.ScrubPlan()
	if err != nil {
		t.Fatalf("ScrubPlan: %v", err)
	}
	if res.Rows != 3 || res.RowsChanged != 1 || res.Oversize != 1 {
		t.Errorf("Rows=%d RowsChanged=%d Oversize=%d, want 3, 1 and 1", res.Rows, res.RowsChanged, res.Oversize)
	}
	assertNoScrubToken(t, fmt.Sprintf("%+v", res), token)
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the log after the plan: %v", err)
	}
	if !bytes.Equal(after, before) {
		t.Error("the plan changed the log")
	}
}

// TestReviewLogScrubPlanMissingLogIsNoOp: a missing log is nothing to scrub,
// and the plan creates no file.
func TestReviewLogScrubPlanMissingLogIsNoOp(t *testing.T) {
	t.Parallel()
	logf := &ReviewsFile{Path: filepath.Join(t.TempDir(), ReviewsFileName)}
	res, err := logf.ScrubPlan()
	if err != nil {
		t.Fatalf("ScrubPlan on a missing log: %v", err)
	}
	if res.Rows != 0 || res.RowsChanged != 0 || len(res.Beads) != 0 {
		t.Errorf("a missing log reported Rows=%d RowsChanged=%d Beads=%v, want zero", res.Rows, res.RowsChanged, res.Beads)
	}
	if _, err := os.Stat(logf.Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the scrub touched a missing log: stat error %v", err)
	}
}

// TestReviewLogScrubPlanErrorsNameTheFileOnly: a log that cannot be opened and
// one that cannot be read return an error that names the file and holds none of
// a row's text.
func TestReviewLogScrubPlanErrorsNameTheFileOnly(t *testing.T) {
	t.Parallel()
	token := scrubTestFamilies["aws"]
	for name, mkPath := range map[string]func(t *testing.T) string{
		"a path under a regular file cannot be opened": func(t *testing.T) string {
			file := filepath.Join(t.TempDir(), "plain")
			if err := os.WriteFile(file, scrubTestLogBytes(t, scrubTestThreeRows(token)...), 0o640); err != nil {
				t.Fatalf("writing the plain file: %v", err)
			}
			return filepath.Join(file, ReviewsFileName)
		},
		"a directory opens and cannot be read": func(t *testing.T) string { return t.TempDir() },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := mkPath(t)
			res, err := (&ReviewsFile{Path: path}).ScrubPlan()
			if err == nil {
				t.Fatalf("ScrubPlan on %s returned no error", name)
			}
			if res.Rows != 0 || res.RowsChanged != 0 {
				t.Errorf("a failed plan reported Rows=%d RowsChanged=%d, want zero", res.Rows, res.RowsChanged)
			}
			assertNoScrubToken(t, err.Error(), token)
		})
	}
}
