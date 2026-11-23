package land

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// The tokens are built from parts so this file holds no token-shaped literal.
var scrubTestFamilies = map[string]string{
	"aws":           "AK" + "IA" + strings.Repeat("Q", 16),
	"google":        "AI" + "za" + strings.Repeat("b", 35),
	"slack":         "xo" + "xb-" + strings.Repeat("9", 24),
	"asia":          "AS" + "IA" + strings.Repeat("Z", 16),
	"jwt":           "ey" + "J" + strings.Repeat("a", 12) + "." + strings.Repeat("b", 12) + "." + strings.Repeat("c", 12),
	"jwt-unsecured": "ey" + "J" + strings.Repeat("a", 12) + "." + strings.Repeat("b", 12) + ".",
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
// is not part of it, even where it follows an unsecured JWT's own signature '.
func TestReviewLogScrubATokenFollowedByTokenBytes(t *testing.T) {
	t.Parallel()
	for _, tail := range []string{"+x", "-x", ".x", "/x", "=x", ":x", "z9", "key"} {
		claim := "id " + scrubTestFamilies["asia"] + tail + " end"
		if got, want := scrubClaim(claim), "id <redacted>"+tail+" end"; got != want {
			t.Errorf("scrubClaim(%q) = %q, want %q", claim, got, want)
		}
	}
	for _, family := range []string{"asia", "aws", "jwt", "jwt-unsecured"} {
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

// TestScrubTextIsScrubClaim: the export a caller outside this package records and
// shows text through is this package's scrub, not a copy of it (gt-v90ftl).
func TestScrubTextIsScrubClaim(t *testing.T) {
	t.Parallel()
	token := "ghp_" + strings.Repeat("a", 36)
	line := "fatal: could not read from https://bot:" + token + "@forgejo:3000/o/r.git"
	got := ScrubText(line)
	if want := scrubClaim(line); got != want {
		t.Errorf("ScrubText(%q) = %q, want scrubClaim's %q", line, got, want)
	}
	if strings.Contains(got, token[:12]) {
		t.Errorf("ScrubText keeps the token: %q", got)
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

// TestReviewLogScrubRecordedFindingTitleAndPath: a finding's title and path are
// scrubbed like its claim, and an ordinary pair is kept (gt-kwx3vs).
func TestReviewLogScrubRecordedFindingTitleAndPath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                string
		f                   Finding
		wantTitle, wantFile string
	}{
		{"title quotes a token", Finding{Title: "it logs " + scrubTestFamilies["aws"] + " verbatim", Path: "internal/land/note.go"}, "it logs <redacted> verbatim", "internal/land/note.go"},
		{"path holds a token after a slash", Finding{Title: "the refusal has no test", Path: "internal/land/" + scrubTestFamilies["slack"]}, "the refusal has no test", "internal/land/<redacted>"},
		{"ordinary title and path", Finding{Title: "the refusal has no test", Path: "internal/land/reviewlog.go"}, "the refusal has no test", "internal/land/reviewlog.go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if rec := reviewFindingTexts([]Finding{tc.f})[0]; rec.Title != tc.wantTitle || rec.File != tc.wantFile {
				t.Errorf("recorded title/path = %q/%q, want %q/%q", rec.Title, rec.File, tc.wantTitle, tc.wantFile)
			}
		})
	}
}

// TestReviewLogScrubHeldFindingTitleAndPath: a held finding's title and path
// reach the queue line scrubbed, and the finding refiled from it keeps the raw
// finding's fingerprint, so its bead is matched instead of filed twice (gt-kwx3vs).
func TestReviewLogScrubHeldFindingTitleAndPath(t *testing.T) {
	t.Parallel()
	rawTitle := "it logs " + scrubTestFamilies["aws"] + " verbatim"
	rawPath := "internal/land/" + scrubTestFamilies["slack"]
	logf := &ReviewsFile{Path: filepath.Join(t.TempDir(), ReviewsFileName)}
	queue := QueuedFollowup{Bead: "gt-x", Rig: "gastown", Findings: heldFindingTexts([]FollowupCandidate{{
		ID: "f-1", Severity: omSeverityMinor, Path: rawPath, Title: rawTitle, Claim: "the key reaches the log",
	}})}
	if err := logf.AppendQueue(queue); err != nil {
		t.Fatalf("AppendQueue: %v", err)
	}
	queued, err := logf.Queued()
	if err != nil || len(queued) != 1 {
		t.Fatalf("Queued = %d lines, %v, want one", len(queued), err)
	}
	if got := queued[0].Findings[0]; got.Title != "it logs <redacted> verbatim" || got.File != "internal/land/<redacted>" {
		t.Errorf("queued title/path = %q/%q, want both scrubbed", got.Title, got.File)
	}
	refiled := (&Lander{}).queuedCandidates(queued[0], nil, nil)
	if len(refiled) != 1 {
		t.Fatalf("the queue line refiled %d findings, want one", len(refiled))
	}
	want := Fingerprint(rawPath, rawTitle)
	if d := Decide(refiled[0], []ExistingFollowup{{ID: "gt-old", Labels: []string{want}}}, 0); d.Action != DedupeComment || d.Target != "gt-old" {
		t.Errorf("Decide = %+v, want the bead the raw finding was filed as", d)
	}
	if spec := bundleSpec(Work{BeadID: "gt-x"}, Result{}, time.Time{}, followupGroup{Package: "internal/land", Findings: refiled}); !slices.Contains(spec.Labels, want) {
		t.Errorf("filed labels = %v, want the raw finding's %s", spec.Labels, want)
	}
}

// TestReviewLogScrubFingerprintKeepsItsIdentity pins what the scrub in
// Fingerprint must keep (gt-kwx3vs): an ordinary title and path fingerprint
// exactly as main does, and a finding refiled from the scrubbed queue line
// fingerprints like the raw finding om raised. Without the scrub the raw and
// refiled texts hash apart, so this fails.
func TestReviewLogScrubFingerprintKeepsItsIdentity(t *testing.T) {
	t.Parallel()
	// Computed on origin/main from the same ordinary title and path.
	if got, want := Fingerprint("internal/land/reviewlog.go", "the refusal has no test"), "om-fp:f4be1f1cde2a"; got != want {
		t.Errorf("ordinary fingerprint = %s, want main's %s", got, want)
	}
	c := FollowupCandidate{Path: "internal/land/" + scrubTestFamilies["slack"], Title: "it logs " + scrubTestFamilies["aws"] + " verbatim"}
	refiled := heldFindingTexts([]FollowupCandidate{c})[0]
	if got, want := Fingerprint(refiled.File, refiled.Title), Fingerprint(c.Path, c.Title); got != want {
		t.Errorf("refiled fingerprint = %s, want the raw finding's %s", got, want)
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

// TestReviewLogScrubPlanBeadIDsNeverCarryAToken: a log row whose bead field is a
// token-shaped string is reported as the marker, a normal id is reported as it
// is, and no part of the token reaches the result (gt-6r1l6k).
func TestReviewLogScrubPlanBeadIDsNeverCarryAToken(t *testing.T) {
	t.Parallel()
	idToken := "gh" + "p_" + strings.Repeat("a", 36)
	claimToken := scrubTestFamilies["aws"]
	rows := []ReviewRecord{{BeadID: idToken}, {BeadID: "gt-1"}}
	for i := range rows {
		rows[i].FindingText = []ReviewFindingText{{ID: "f-1", Claim: "it logs " + claimToken + " verbatim"}}
	}
	logf, _ := writeScrubTestLog(t, scrubTestLogBytes(t, rows...))
	res, err := logf.ScrubPlan()
	if err != nil {
		t.Fatalf("ScrubPlan: %v", err)
	}
	if want := []string{redactedWord, "gt-1"}; !slices.Equal(res.Beads, want) {
		t.Errorf("Beads = %v, want %v", res.Beads, want)
	}
	assertNoScrubToken(t, fmt.Sprintf("%+v", res), idToken)
}

// TestReviewLogScrubRewriteApplyRewritesOnlyTheFlaggedRow: an apply rewrites the
// token-holding row's bytes alone, keeps the file's mode, and a second run finds
// nothing and leaves the log as the first run left it.
func TestReviewLogScrubRewriteApplyRewritesOnlyTheFlaggedRow(t *testing.T) {
	t.Parallel()
	token := scrubTestFamilies["aws"]
	before := scrubTestLogBytes(t, scrubTestThreeRows(token)...)
	logf, path := writeScrubTestLog(t, before)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stating the test log: %v", err)
	}
	var gotMode os.FileMode
	logf.replace = func(p string, data []byte, mode os.FileMode) error {
		gotMode = mode
		return os.WriteFile(p, data, mode)
	}
	res, err := logf.Scrub(true)
	if err != nil {
		t.Fatalf("Scrub(true): %v", err)
	}
	if res.RowsChanged != 1 || len(res.Beads) != 1 || res.Beads[0] != "gt-slit1" {
		t.Fatalf("apply reported RowsChanged=%d Beads=%v, want 1 and [gt-slit1]", res.RowsChanged, res.Beads)
	}
	assertNoScrubToken(t, fmt.Sprintf("%+v", res), token)
	if gotMode != info.Mode().Perm() {
		t.Errorf("the replace seam got mode %v, want the file's %v", gotMode, info.Mode().Perm())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the rewritten log: %v", err)
	}
	beforeLines := bytes.Split(bytes.TrimSuffix(before, []byte("\n")), []byte("\n"))
	afterLines := bytes.Split(bytes.TrimSuffix(after, []byte("\n")), []byte("\n"))
	if len(afterLines) != len(beforeLines) {
		t.Fatalf("the rewrite left %d lines, want %d", len(afterLines), len(beforeLines))
	}
	for i := 1; i < len(beforeLines); i++ {
		if !bytes.Equal(afterLines[i], beforeLines[i]) {
			t.Errorf("row %d changed to %q, want %q", i+1, afterLines[i], beforeLines[i])
		}
	}
	want := bytes.ReplaceAll(beforeLines[0], []byte(token), []byte(redactedWord))
	if !bytes.Equal(afterLines[0], want) {
		t.Errorf("the flagged row is %q, want %q", afterLines[0], want)
	}
	assertNoScrubToken(t, string(afterLines[0]), token)

	res2, err := logf.Scrub(true)
	if err != nil {
		t.Fatalf("second Scrub(true): %v", err)
	}
	if res2.RowsChanged != 0 || res2.Words != 0 || len(res2.Beads) != 0 {
		t.Errorf("the second run reported RowsChanged=%d Words=%d Beads=%v, want zero", res2.RowsChanged, res2.Words, res2.Beads)
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the log after the second run: %v", err)
	}
	if !bytes.Equal(again, after) {
		t.Error("the second run changed the log")
	}
	assertNoScrubToken(t, fmt.Sprintf("%+v", res2), token)
}

// TestReviewLogScrubRewriteKeepsEachLinesTerminator: an apply writes each line
// back with the terminator it read, so only the token's bytes change in a CRLF
// log, a log whose last line has no newline, and a log with a lone \r in a line.
func TestReviewLogScrubRewriteKeepsEachLinesTerminator(t *testing.T) {
	t.Parallel()
	token := scrubTestFamilies["aws"]
	plain := scrubTestLogBytes(t, scrubTestThreeRows(token)...)
	rows := map[string][]byte{
		"crlf":          bytes.ReplaceAll(plain, []byte("\n"), []byte("\r\n")),
		"no final":      bytes.TrimSuffix(plain, []byte("\n")),
		"lone carriage": append(slices.Clone(plain), "odd\rline\n"...),
	}
	for name, before := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			logf, path := writeScrubTestLog(t, before)
			res, err := logf.Scrub(true)
			if err != nil {
				t.Fatalf("Scrub(true): %v", err)
			}
			assertNoScrubToken(t, fmt.Sprintf("%+v", res), token)
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading the rewritten log: %v", err)
			}
			if want := bytes.ReplaceAll(before, []byte(token), []byte(redactedWord)); !bytes.Equal(after, want) {
				t.Errorf("the log is %q, want %q", after, want)
			}
		})
	}
}

// TestReviewLogScrubRewriteNothingFlaggedWritesNothing: an apply over a log with
// no token-shaped word never calls the replace seam and leaves the file as it
// is (bytes, inode, modification time), whatever its line endings.
func TestReviewLogScrubRewriteNothingFlaggedWritesNothing(t *testing.T) {
	t.Parallel()
	clean := scrubTestLogBytes(t, scrubTestThreeRows("no token here")[1:]...)
	rows := map[string][]byte{
		"lf":       clean,
		"crlf":     bytes.ReplaceAll(clean, []byte("\n"), []byte("\r\n")),
		"no final": bytes.TrimSuffix(clean, []byte("\n")),
	}
	for name, before := range rows {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			logf, path := writeScrubTestLog(t, before)
			logf.replace = func(string, []byte, os.FileMode) error {
				t.Error("the replace seam was called for a log with nothing flagged")
				return nil
			}
			infoBefore, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stating the test log: %v", err)
			}
			res, err := logf.Scrub(true)
			if err != nil {
				t.Fatalf("Scrub(true): %v", err)
			}
			if res.RowsChanged != 0 || res.Words != 0 {
				t.Errorf("reported RowsChanged=%d Words=%d, want zero", res.RowsChanged, res.Words)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading the log: %v", err)
			}
			infoAfter, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stating the log after: %v", err)
			}
			if !bytes.Equal(after, before) {
				t.Errorf("the log is %q, want %q", after, before)
			}
			if !os.SameFile(infoBefore, infoAfter) || !infoBefore.ModTime().Equal(infoAfter.ModTime()) {
				t.Error("the log's inode or modification time changed")
			}
		})
	}
}

// TestReviewLogScrubRewriteTokenAtEveryOffsetAroundTheLineLimit: an oversize
// line is scanned in one pass, so a token is replaced at every offset from just
// under the limit to just past it, for four families.
func TestReviewLogScrubRewriteTokenAtEveryOffsetAroundTheLineLimit(t *testing.T) {
	t.Parallel()
	const limit = 100
	families := map[string]string{
		"sk":   "s" + "k-" + "abcdefghijklmnop1234",
		"ghp":  "gh" + "p_" + strings.Repeat("a", 20),
		"akia": scrubTestFamilies["aws"],
		"xoxb": scrubTestFamilies["slack"],
	}
	for family, token := range families {
		t.Run(family, func(t *testing.T) {
			t.Parallel()
			for pad := limit - 2; pad <= limit+len(token)+2; pad++ {
				line := strings.Repeat("x", pad) + " " + token + " tail\n"
				logf, path := writeScrubTestLog(t, []byte(line))
				logf.lineLimit = limit
				res, err := logf.Scrub(true)
				if err != nil {
					t.Fatalf("pad %d: Scrub(true): %v", pad, err)
				}
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("pad %d: reading the log: %v", pad, err)
				}
				if res.RowsChanged != 1 || res.Oversize != 1 {
					t.Errorf("pad %d: reported RowsChanged=%d Oversize=%d, want 1 and 1", pad, res.RowsChanged, res.Oversize)
				}
				assertNoScrubToken(t, string(after), token)
				if want := strings.Repeat("x", pad) + " " + redactedWord + " tail\n"; string(after) != want {
					t.Errorf("pad %d: the line is %q, want %q", pad, after, want)
				}
			}
		})
	}
}

// scrubEscapedRow is one row's raw line whose claim holds token with one byte
// written as a JSON escape: it decodes to the token, and the raw line does not
// hold it.
func scrubEscapedRow(t *testing.T, bead, token string) []byte {
	t.Helper()
	line := scrubTestLogBytes(t, ReviewRecord{BeadID: bead, Rig: "gastown", Verdict: VerdictApprove,
		FindingText: []ReviewFindingText{{ID: "f-1", Claim: "it logs " + token}}})
	mid := len(token) / 2
	esc := fmt.Sprintf(`\u%04x`, token[mid])
	return bytes.Replace(line, []byte(token), []byte(token[:mid]+esc+token[mid+1:]), 1)
}

// TestReviewLogScrubSkippedRowsAreCountedListedAndUnchanged: a row whose flagged
// word the raw line does not hold is counted, listed by a safe label and left
// byte for byte, under plan and apply, beside a row the scrub does rewrite.
func TestReviewLogScrubSkippedRowsAreCountedListedAndUnchanged(t *testing.T) {
	t.Parallel()
	token := scrubTestFamilies["aws"]
	idToken := scrubTestFamilies["slack"]
	rewritable := scrubTestLogBytes(t, scrubTestThreeRows(token)[0])
	rows := []struct {
		name  string
		bead  string
		label string
	}{
		{"bead id", "gt-esc1", "gt-esc1"},
		{"no bead field", "", "line 2"},
		{"token-shaped bead id", idToken, redactedWord},
	}
	for _, row := range rows {
		for _, apply := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s apply=%v", row.name, apply), func(t *testing.T) {
				t.Parallel()
				before := append(slices.Clone(rewritable), scrubEscapedRow(t, row.bead, token)...)
				logf, path := writeScrubTestLog(t, before)
				res, err := logf.Scrub(apply)
				if err != nil {
					t.Fatalf("Scrub(%v): %v", apply, err)
				}
				if res.Skipped != 1 || !slices.Equal(res.SkippedBeads, []string{row.label}) {
					t.Errorf("Skipped=%d SkippedBeads=%v, want 1 and [%s]", res.Skipped, res.SkippedBeads, row.label)
				}
				if res.RowsChanged != 1 || res.Rows != 2 {
					t.Errorf("RowsChanged=%d Rows=%d, want 1 and 2: the skipped row is read, not changed", res.RowsChanged, res.Rows)
				}
				assertNoScrubToken(t, fmt.Sprintf("%+v", res), token)
				assertNoScrubToken(t, fmt.Sprintf("%+v", res), idToken)
				after, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("reading the log: %v", err)
				}
				want := before
				if apply {
					want = bytes.Replace(before, []byte(token), []byte(redactedWord), 1)
				}
				if !bytes.Equal(after, want) {
					t.Errorf("the log is %q, want %q", after, want)
				}
			})
		}
	}
}

// TestReviewLogScrubSkippedWordFoundOnlyInsideALongerWord: a word whose only
// plain copy sits inside a longer flagged word is missing once that one is
// replaced, so the row is skipped, not reported clean.
func TestReviewLogScrubSkippedWordFoundOnlyInsideALongerWord(t *testing.T) {
	t.Parallel()
	short := scrubTestFamilies["aws"]
	long := short + "zzz"
	rec := ReviewRecord{BeadID: "gt-esc2", Rig: "gastown", Verdict: VerdictApprove, FindingText: []ReviewFindingText{
		{ID: "f-1", Claim: "a " + long}, {ID: "f-2", Claim: "b " + short}}}
	raw := bytes.Replace(scrubTestLogBytes(t, rec), []byte("b "+short), []byte(`b \u0041`+short[1:]), 1)
	logf, _ := writeScrubTestLog(t, raw)
	res, err := logf.ScrubPlan()
	if err != nil {
		t.Fatalf("ScrubPlan: %v", err)
	}
	if res.Skipped != 1 || !slices.Equal(res.SkippedBeads, []string{"gt-esc2"}) {
		t.Errorf("Skipped=%d SkippedBeads=%v, want 1 and [gt-esc2]", res.Skipped, res.SkippedBeads)
	}
}

// TestReviewLogScrubSkippedListsNothingForARewrittenLog: an oversize line is
// rewritten by the raw scan, not skipped, and a log with neither reports empty
// lists.
func TestReviewLogScrubSkippedListsNothingForARewrittenLog(t *testing.T) {
	t.Parallel()
	token := scrubTestFamilies["aws"]
	big := strings.Repeat("x", 200) + " " + token
	for name, log := range map[string][]byte{
		"oversize": []byte(big + "\n"),
		"clean":    scrubTestLogBytes(t, scrubTestThreeRows("none")[1:]...),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			logf, _ := writeScrubTestLog(t, log)
			logf.lineLimit = 100
			res, err := logf.ScrubPlan()
			if err != nil {
				t.Fatalf("ScrubPlan: %v", err)
			}
			if res.Skipped != 0 || len(res.SkippedBeads) != 0 {
				t.Errorf("Skipped=%d SkippedBeads=%v, want zero and empty", res.Skipped, res.SkippedBeads)
			}
		})
	}
}

// TestReviewLogScrubRewriteReplaceFailureKeepsTheLog: a failing replace returns
// its error, which names no value, and leaves the original log.
func TestReviewLogScrubRewriteReplaceFailureKeepsTheLog(t *testing.T) {
	t.Parallel()
	token := scrubTestFamilies["aws"]
	before := scrubTestLogBytes(t, scrubTestThreeRows(token)...)
	logf, path := writeScrubTestLog(t, before)
	logf.replace = func(string, []byte, os.FileMode) error { return errors.New("the disk is full") }
	_, err := logf.Scrub(true)
	if err == nil {
		t.Fatal("Scrub(true) with a failing replace returned no error")
	}
	if !strings.Contains(err.Error(), "the disk is full") {
		t.Errorf("the error does not name the replace failure: %v", err)
	}
	assertNoScrubToken(t, err.Error(), token)
	after, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("reading the log after the failed replace: %v", rerr)
	}
	if !bytes.Equal(after, before) {
		t.Error("a replace failure changed the log")
	}
}

// TestReviewLogScrubRewriteCopiesWhatItCannotDecode: a line that is not a JSON
// object and one past the byte bound are copied through untouched and counted
// while the scrub rewrites a row beside them.
func TestReviewLogScrubRewriteCopiesWhatItCannotDecode(t *testing.T) {
	t.Parallel()
	token := scrubTestFamilies["aws"]
	longLine := strings.Repeat("y", 600)
	before := append(scrubTestLogBytes(t, scrubTestThreeRows(token)...), []byte("not a JSON row\nnull\n"+longLine+"\n")...)
	logf, path := writeScrubTestLog(t, before)
	logf.lineLimit = 512
	res, err := logf.Scrub(true)
	if err != nil {
		t.Fatalf("Scrub(true): %v", err)
	}
	if res.Rows != 3 || res.RowsChanged != 1 {
		t.Errorf("Rows=%d RowsChanged=%d, want 3 and 1", res.Rows, res.RowsChanged)
	}
	if res.Oversize != 1 {
		t.Errorf("Oversize = %d, want 1", res.Oversize)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the rewritten log: %v", err)
	}
	beforeLines := bytes.Split(bytes.TrimSuffix(before, []byte("\n")), []byte("\n"))
	afterLines := bytes.Split(bytes.TrimSuffix(after, []byte("\n")), []byte("\n"))
	if len(afterLines) != len(beforeLines) {
		t.Fatalf("the rewrite left %d lines, want %d", len(afterLines), len(beforeLines))
	}
	for i := 1; i < len(beforeLines); i++ {
		if !bytes.Equal(afterLines[i], beforeLines[i]) {
			t.Errorf("line %d changed to %q, want the same bytes", i+1, afterLines[i])
		}
	}
}

// TestReviewLogScrubRewriteScansWhatItCannotDecode: a line past the bound and a
// line that is not an object are scanned over their raw bytes, so a token in
// either is replaced and every other byte kept; the plan counts them as changed
// the same way, Oversize still counts the oversize line, and a line with no
// token stays byte for byte (gt-9h1asy, om c1f06be4c163).
func TestReviewLogScrubRewriteScansWhatItCannotDecode(t *testing.T) {
	t.Parallel()
	token := scrubTestFamilies["aws"]
	tokens := map[string]string{"oversize": strings.Repeat("y", 300) + " " + token + " " + strings.Repeat("z", 300),
		"not an object": "not a JSON row " + token + " tail"}
	lines := [][]byte{
		scrubTestLogBytes(t, scrubTestThreeRows(token)[0]),
		[]byte(tokens["oversize"] + "\n"), []byte(tokens["not an object"] + "\n"),
		[]byte("a plain line with no token\n"), []byte(strings.Repeat("q", 600) + "\n"),
	}
	before := bytes.Join(lines, nil)
	plan, _ := writeScrubTestLog(t, before)
	plan.lineLimit = 512
	pres, err := plan.ScrubPlan()
	if err != nil {
		t.Fatalf("ScrubPlan: %v", err)
	}
	logf, path := writeScrubTestLog(t, before)
	logf.lineLimit = 512
	res, err := logf.Scrub(true)
	if err != nil {
		t.Fatalf("Scrub(true): %v", err)
	}
	for label, r := range map[string]ScrubResult{"plan": pres, "apply": res} {
		if r.Rows != 1 || r.RowsChanged != 3 || r.Words != 3 || r.Oversize != 2 {
			t.Errorf("%s: Rows=%d RowsChanged=%d Words=%d Oversize=%d, want 1, 3, 3 and 2", label, r.Rows, r.RowsChanged, r.Words, r.Oversize)
		}
		assertNoScrubToken(t, fmt.Sprintf("%+v", r), token)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the rewritten log: %v", err)
	}
	if want := bytes.ReplaceAll(before, []byte(token), []byte(redactedWord)); !bytes.Equal(after, want) {
		t.Errorf("the rewrite changed more than the token runs:\n got: %q\nwant: %q", after, want)
	}
}

// TestReviewLogScrubRewriteScansAMillionByteLine: an oversize line of a million
// bytes, dense with prefixes, is scanned in full and its token replaced, in time
// bounded by the line, not by its square (gt-9h1asy).
func TestReviewLogScrubRewriteScansAMillionByteLine(t *testing.T) {
	t.Parallel()
	token := scrubTestFamilies["aws"]
	line := strings.Repeat("ASIAa-", 170000) + " " + token + " end"
	logf, path := writeScrubTestLog(t, []byte(line+"\n"))
	logf.lineLimit = 512
	res, err := logf.Scrub(true)
	if err != nil {
		t.Fatalf("Scrub(true): %v", err)
	}
	if res.Oversize != 1 || res.RowsChanged != 1 || res.Words != 1 {
		t.Errorf("Oversize=%d RowsChanged=%d Words=%d, want 1, 1 and 1", res.Oversize, res.RowsChanged, res.Words)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the rewritten log: %v", err)
	}
	if bytes.Contains(after, []byte(token)) {
		t.Error("the million-byte line still holds the token")
	}
	assertNoScrubToken(t, fmt.Sprintf("%+v", res), token)
}

// TestScrubRefusesAChangedLogBetweenReadAndReplace: another process (the landing
// worker) may append to the log after the scrub read it. A replace then would drop
// that line, so the scrub compares the log's size and modification time with what
// it read and refuses when either moved (gt-l9c9gy).
func TestScrubRefusesAChangedLogBetweenReadAndReplace(t *testing.T) {
	t.Parallel()
	token := scrubTestFamilies["aws"]
	for name, change := range map[string]func(t *testing.T, path string) []byte{
		"a line appended": func(t *testing.T, path string) []byte {
			extra := []byte(`{"bead_id":"gt-late","verdict":"approve"}` + "\n")
			fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatalf("opening the log to append: %v", err)
			}
			defer fh.Close()
			if _, err := fh.Write(extra); err != nil {
				t.Fatalf("appending: %v", err)
			}
			return extra
		},
		"a line appended with the mtime put back": func(t *testing.T, path string) []byte {
			st, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			extra := []byte(`{"bead_id":"gt-late","verdict":"approve"}` + "\n")
			fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatalf("opening the log to append: %v", err)
			}
			if _, err := fh.Write(extra); err != nil {
				t.Fatalf("appending: %v", err)
			}
			if err := fh.Close(); err != nil {
				t.Fatalf("closing: %v", err)
			}
			if err := os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil {
				t.Fatalf("putting the mtime back: %v", err)
			}
			return extra
		},
		"the mtime moved": func(t *testing.T, path string) []byte {
			later := time.Now().Add(time.Hour)
			if err := os.Chtimes(path, later, later); err != nil {
				t.Fatalf("moving the mtime: %v", err)
			}
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			before := scrubTestLogBytes(t, scrubTestThreeRows(token)...)
			logf, path := writeScrubTestLog(t, before)
			var extra []byte
			logf.beforeReplace = func() { extra = change(t, path) }
			replaced := 0
			logf.replace = func(string, []byte, os.FileMode) error { replaced++; return nil }

			_, err := logf.Scrub(true)

			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "changed") {
				t.Fatalf("Scrub(true) returned %v, want an error naming the path and that the log changed", err)
			}
			assertNoScrubToken(t, err.Error(), token)
			if replaced != 0 {
				t.Errorf("replace was called %d time(s) over a log that changed", replaced)
			}
			after, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatalf("reading the log: %v", rerr)
			}
			if !bytes.Equal(after, append(append([]byte{}, before...), extra...)) {
				t.Errorf("the log is not as its writer left it")
			}
		})
	}
}

// TestScrubReplacesAnUnchangedLog: a log nobody touched since the read is replaced
// as before.
func TestScrubReplacesAnUnchangedLog(t *testing.T) {
	t.Parallel()
	token := scrubTestFamilies["aws"]
	logf, path := writeScrubTestLog(t, scrubTestLogBytes(t, scrubTestThreeRows(token)...))
	logf.beforeReplace = func() {}

	if _, err := logf.Scrub(true); err != nil {
		t.Fatalf("Scrub(true) over an unchanged log: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the log: %v", err)
	}
	assertNoScrubToken(t, string(after), token)
}

// TestScrubRefusesALogThatVanishedBeforeTheReplace: a log that cannot be checked
// again is not replaced either; the error names the path.
func TestScrubRefusesALogThatVanishedBeforeTheReplace(t *testing.T) {
	t.Parallel()
	token := scrubTestFamilies["aws"]
	logf, path := writeScrubTestLog(t, scrubTestLogBytes(t, scrubTestThreeRows(token)...))
	logf.beforeReplace = func() {
		if err := os.Remove(path); err != nil {
			t.Fatalf("removing the log: %v", err)
		}
	}
	replaced := 0
	logf.replace = func(string, []byte, os.FileMode) error { replaced++; return nil }

	_, err := logf.Scrub(true)

	if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "checking it before the replace") {
		t.Fatalf("Scrub(true) over a vanished log returned %v, want an error naming the path", err)
	}
	if replaced != 0 {
		t.Errorf("replace was called %d time(s) over a log that could not be checked", replaced)
	}
}
