package land

import (
	"context"
	"encoding/json"
	"errors"
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
