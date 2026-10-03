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
