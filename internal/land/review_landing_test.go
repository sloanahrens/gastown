package land

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The three om outcomes a landing must route (gt-v4ssj.2): approve lands with
// the real verdict recorded (TestLandMergesGatesPushesAndRecords),
// request_changes is rework carrying om's text
// (TestLandRequestChangesRejectsWithFindings), and an om execution error
// lands with "error:<reason>" recorded (below).

func TestLandOMExecutionErrorLandsWithErrorVerdict(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.review.fn = func(string) (Verdict, error) {
		return Verdict{}, errors.New("om review did not run: exec: \"om\": not found")
	}
	l := f.lander()
	l.ReviewErrorLands = true
	res, err := l.Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v; an om execution error must not block the landing", err)
	}
	if f.originMain() != res.LandedCommit {
		t.Fatal("origin/main is not the landed commit")
	}
	var rec LandingRecord
	if err := json.Unmarshal([]byte(f.landingLines()[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rec.OMVerdict, VerdictErrorPrefix) || !strings.Contains(rec.OMVerdict, "not found") {
		t.Fatalf("om_verdict = %q; want error:<reason>", rec.OMVerdict)
	}
	if b := f.bead(); b.Status != "closed" || !strings.Contains(b.Notes, "om_verdict: error:") {
		t.Fatalf("bead status %s notes:\n%s", b.Status, b.Notes)
	}
}

// omStderr is the shape om's own output had in the incident (gt-hhid7): a long
// config warning about the ignored .om.json backend first, the reason it gave
// no verdict last. omError wraps it the way OMReviewer reports an execution
// error.
func omStderr() string {
	return `om: warning: config: "backend" in /private/var/folders/dx/ccj87p8d14l8cs64cnp691pm0000gn/T/gt-landing-501/gastown/wt/.om.json is ignored — the reviewer backend is operator-config only` +
		"\nthe reviewer backend claude-deepseek-flash exited 1 in 0 seconds"
}

func omError(stderr string) error {
	return fmt.Errorf("%w: exited 2: %s", ErrOMExecution, stderr)
}

// TestReviewErrorReasonKeepsBothEnds: the bound still applies, but it now cuts
// the middle, so the reason keeps the command that failed and the backend's own
// stderr. The head-only cut it replaces kept the config warning and threw the
// cause away (gt-hhid7).
func TestReviewErrorReasonKeepsBothEnds(t *testing.T) {
	t.Parallel()
	stderr := strings.Repeat("warning: filler path segment/", 40) + "the reviewer backend claude-deepseek-flash exited 1 in 0 seconds"
	got := reviewErrorReason(omError(stderr))
	if n := len([]rune(got)); n > reviewErrorReasonMax {
		t.Errorf("reason is %d runes, want at most %d: %q", n, reviewErrorReasonMax, got)
	}
	if !strings.Contains(got, "om review execution error") {
		t.Errorf("reason lost its head: %q", got)
	}
	if !strings.Contains(got, "exited 1 in 0 seconds") {
		t.Errorf("reason lost the backend's cause: %q", got)
	}
	if short := "om review execution error: exited 2"; reviewErrorReason(errors.New(short)) != short {
		t.Errorf("a reason under the bound was rewritten: %q", reviewErrorReason(errors.New(short)))
	}
}

// TestLandRejectionKeepsTheTailOfALongOMError: the rejection note a human reads
// carries the backend's own failure, not only the leading config warning.
func TestLandRejectionKeepsTheTailOfALongOMError(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.review.fn = func(string) (Verdict, error) { return Verdict{}, omError(omStderr()) }
	l := f.lander()
	l.ReviewErrorRejects = true
	_, err := l.Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectReview, LabelNeedsHuman)
	if !strings.Contains(rej.Reason, "the reviewer backend claude-deepseek-flash exited 1 in 0 seconds") {
		t.Errorf("rejection reason lost the backend's cause:\n%s", rej.Reason)
	}
	if !strings.Contains(f.bead().Notes, "the reviewer backend claude-deepseek-flash exited 1 in 0 seconds") {
		t.Errorf("bead note lost the backend's cause:\n%s", f.bead().Notes)
	}
}

// TestLandExecutionErrorOnAHugeDiffNamesTheSize: an om execution error on a
// merged tree past the stated line bound is reported as the diff being too
// large for om. The reader's next move (an overseer review) is not the rework a
// bare execution error asks for, and the size was the part of the incident only
// a hand-run of om revealed (gt-hhid7).
func TestLandExecutionErrorOnAHugeDiffNamesTheSize(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	// A branch diff past the default bound, so the test exercises the real
	// default rather than a configured one.
	f.work.Head = f.git.Commit(t, f.origin, fixtureBranch, "feat: add a large b",
		map[string]string{"b.txt": strings.Repeat("work\n", DefaultOMDiffTooLargeLines+10)})
	f.review.fn = func(string) (Verdict, error) { return Verdict{}, omError(omStderr()) }
	l := f.lander()
	l.ReviewErrorRejects = true
	_, err := l.Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectReview, LabelNeedsHuman)
	if !strings.Contains(rej.Reason, "diff too large for om; overseer review needed") {
		t.Errorf("reason = %q; want it to name the size", rej.Reason)
	}
	if !strings.Contains(rej.Reason, "past the "+fmt.Sprint(DefaultOMDiffTooLargeLines)+"-line bound") {
		t.Errorf("reason = %q; want the bound stated", rej.Reason)
	}
	if f.originMain() != f.base {
		t.Error("origin/main moved: an unreviewed head must never land")
	}
}

// TestLandExecutionErrorUnderTheBoundStaysBare: the same execution error on an
// ordinary tree names no size, so the phrase keeps meaning something.
func TestLandExecutionErrorUnderTheBoundStaysBare(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.review.fn = func(string) (Verdict, error) { return Verdict{}, omError(omStderr()) }
	l := f.lander()
	l.ReviewErrorRejects = true
	_, err := l.Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectReview, LabelNeedsHuman)
	if strings.Contains(rej.Reason, "diff too large for om") {
		t.Errorf("reason = %q; a small diff must not be called too large", rej.Reason)
	}
}

// omFailsThenApprove is an om that exits with an execution error on its first
// run and approves on the second: the flake of gt-q241r (a trailing comma in
// the reviewer's JSON) whose hand retry passed.
func omFailsThenApprove(f *landFixture) {
	n := 0
	f.review.fn = func(string) (Verdict, error) {
		n++
		if n == 1 {
			return Verdict{}, omError(omStderr())
		}
		return Verdict{Verdict: VerdictApprove, Score: 0.9}, nil
	}
}

// TestLandRetriesOMReviewOnceAfterAnExecutionError: one malformed om generation
// must not cost an escalation and a hand review. The retry runs on the same
// merged tree, the landing succeeds on the second verdict, and both the log and
// the stages line say it was a retry so a flaky reviewer stays visible
// (gt-q241r).
func TestLandRetriesOMReviewOnceAfterAnExecutionError(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	omFailsThenApprove(f)
	l := f.lander()
	var log strings.Builder
	l.Out = &log
	res, err := l.Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v; a retried execution error must not stop the landing", err)
	}
	if len(f.review.calls) != 2 {
		t.Fatalf("review calls = %d, want 2 (the error and one retry)", len(f.review.calls))
	}
	if f.review.calls[0] != f.review.calls[1] {
		t.Errorf("the retry reviewed %v, not the same tree and range as %v", f.review.calls[1], f.review.calls[0])
	}
	if got := f.originMain(); got != res.LandedCommit {
		t.Errorf("origin/main is %s, want the landed commit", got)
	}
	for _, want := range []string{"retrying once", "(retried)"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log is missing %q:\n%s", want, log.String())
		}
	}
}

// TestLandRetriesOMReviewOnceThenRejects: a second execution error is handled
// as today — the landing stops with the second error, not the first, and om is
// not run a third time.
func TestLandRetriesOMReviewOnceThenRejects(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	n := new(int)
	f.review.fn = func(string) (Verdict, error) {
		*n++
		return Verdict{}, omError(fmt.Sprintf("attempt %d: the backend exited 1", *n))
	}
	l := f.lander()
	l.ReviewErrorRejects = true
	_, err := l.Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectReview, LabelNeedsHuman)
	if len(f.review.calls) != 2 {
		t.Fatalf("review calls = %d, want 2 (the error and exactly one retry)", len(f.review.calls))
	}
	if !strings.Contains(rej.Reason, "attempt 2") {
		t.Errorf("reason = %q; want the second attempt's error", rej.Reason)
	}
}

// TestLandDoesNotRetryANonExecutionOMError: a timeout is not a malformed
// generation, and a second run would only spend the om budget on it.
func TestLandDoesNotRetryANonExecutionOMError(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.review.fn = func(string) (Verdict, error) {
		return Verdict{}, fmt.Errorf("%w after 5m", ErrOMTimeout)
	}
	l := f.lander()
	l.ReviewErrorRejects = true
	_, err := l.Land(context.Background(), f.work)
	f.assertRejected(t, err, RejectReview, LabelNeedsHuman)
	if len(f.review.calls) != 1 {
		t.Errorf("review calls = %d, want 1: a timeout is not retried", len(f.review.calls))
	}
}

// TestLandDoesNotRetryAnOversizeOMError: an execution error on a tree past the
// size bound is deterministic, so retrying it only spends the om budget.
func TestLandDoesNotRetryAnOversizeOMError(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.work.Head = f.git.Commit(t, f.origin, fixtureBranch, "feat: add a large b",
		map[string]string{"b.txt": strings.Repeat("work\n", DefaultOMDiffTooLargeLines+10)})
	f.review.fn = func(string) (Verdict, error) { return Verdict{}, omError(omStderr()) }
	l := f.lander()
	l.ReviewErrorRejects = true
	_, err := l.Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectReview, LabelNeedsHuman)
	if len(f.review.calls) != 1 {
		t.Errorf("review calls = %d, want 1: an oversize execution error is deterministic", len(f.review.calls))
	}
	if !strings.Contains(rej.Reason, "diff too large for om") {
		t.Errorf("reason = %q; want it to name the size", rej.Reason)
	}
}

// TestLandDoesNotRetryARequestChangesVerdict: om judged the diff, it simply
// refused it. That verdict is final and goes back as rework.
func TestLandDoesNotRetryARequestChangesVerdict(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.review.fn = func(string) (Verdict, error) {
		return Verdict{Verdict: VerdictRequestChanges, Score: 0.4, Findings: []Finding{{ID: "f1", Title: "bad"}}}, nil
	}
	l := f.lander()
	_, err := l.Land(context.Background(), f.work)
	f.assertRejected(t, err, RejectReview, LabelRework)
	if len(f.review.calls) != 1 {
		t.Errorf("review calls = %d, want 1: a request for changes is a verdict, not an error", len(f.review.calls))
	}
}

func TestOMReviewerAppliesTheRigThreshold(t *testing.T) {
	t.Parallel()
	tree := t.TempDir()
	if err := os.WriteFile(filepath.Join(tree, ".om.json"), []byte(`{"threshold":0.6}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var argv []string
	r := OMReviewer{OutDir: t.TempDir()}
	r.run = fakeOM(t, `{"score":0.55,"verdict":"approve","summary":"mostly fine","findings":[]}`, 0, nil, &argv)
	v, err := r.Review(context.Background(), tree, "a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if v.Verdict != VerdictRequestChanges || !strings.Contains(v.Summary, "below") || !strings.Contains(v.Summary, "mostly fine") {
		t.Fatalf("verdict %+v; a score under the threshold is a request for changes", v)
	}
	r.run = fakeOM(t, `{"score":0.6,"verdict":"approve","findings":[]}`, 0, nil, &argv)
	if v, err := r.Review(context.Background(), tree, "a", "b"); err != nil || v.Verdict != VerdictApprove {
		t.Fatalf("at the threshold: %+v %v", v, err)
	}
}

func TestOMReviewerStripsClaudeConfigDir(t *testing.T) {
	t.Parallel()
	var env []string
	r := OMReviewer{OutDir: t.TempDir()}
	r.run = func(_ context.Context, _ string, e []string, argv []string, _ io.Writer) (int, error) {
		env = e
		for i, a := range argv {
			if a == "--out" {
				_ = os.WriteFile(argv[i+1], []byte(`{"score":0.9,"verdict":"approve","findings":[]}`), 0o600)
			}
		}
		return 0, nil
	}
	if _, err := r.Review(context.Background(), t.TempDir(), "a", "b"); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env, "CLAUDE_CONFIG_DIR") {
		t.Fatalf("env %v does not unset CLAUDE_CONFIG_DIR", env)
	}
	got := mergeEnv([]string{"A=1", "CLAUDE_CONFIG_DIR=/x", "B=2"}, env)
	if slices.ContainsFunc(got, func(kv string) bool { return strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR") }) || len(got) != 2 {
		t.Fatalf("mergeEnv kept CLAUDE_CONFIG_DIR: %v", got)
	}
}
