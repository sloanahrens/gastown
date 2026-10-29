package land

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// omHelpWithCriteria is the help text of an om that knows both flags.
const omHelpWithCriteria = `Usage:
  om review [flags]

Flags:
  --base string             the base of the range
  --bead string             the work bead the diff belongs to
  --criteria-file path      file of acceptance criteria
  --head string             the head of the range
  --out string              where the verdict goes
`

// omHelpWithoutCriteria is an om that knows the bead and not the criteria.
const omHelpWithoutCriteria = `Usage:
  om review [flags]

Flags:
  --base string   the base of the range
  --bead string   the work bead the diff belongs to
  --head string   the head of the range
`

// criteriaStub answers the help probe, the version probe and the review,
// reading the --criteria-file and the --context-file while the review runs —
// the only moment their contents are supposed to exist.
type criteriaStub struct {
	t         *testing.T
	help      string
	probeFail int // help probes to fail with a run error before answering
	probes    int
	versions  int
	argv      []string
	critPath  string
	text      string
	mode      os.FileMode // the criteria file's permissions as the run saw them
	sawFlag   bool
	ctxPath   string      // the --context-file's path as the review saw it
	ctxText   string      // what that file held during the run
	ctxMode   os.FileMode // its permissions
	sawCtx    bool
	hold      bool  // block until the attempt's deadline passes
	fail      error // the review's own failure
}

func (s *criteriaStub) run(ctx context.Context, _ string, _ []string, argv []string, out io.Writer) (int, error) {
	if omSubcommand(argv) == "version" {
		s.versions++
		_, _ = io.WriteString(out, stubVersionLine())
		return 0, nil
	}
	if slices.Contains(argv, "--help") {
		s.probes++
		if s.probes <= s.probeFail {
			return -1, errors.New("exec: om: not found")
		}
		_, _ = io.WriteString(out, s.help)
		return 2, nil
	}
	s.argv = argv
	if i := slices.Index(argv, "--criteria-file"); i >= 0 {
		s.sawFlag, s.critPath = true, argv[i+1]
		b, err := os.ReadFile(s.critPath) //nolint:gosec // G304: the path this stub was handed
		if err != nil {
			s.t.Errorf("reading the criteria file during the run: %v", err)
		}
		s.text = string(b)
		if fi, err := os.Stat(s.critPath); err == nil {
			s.mode = fi.Mode().Perm()
		}
	}
	if i := slices.Index(argv, "--context-file"); i >= 0 {
		s.sawCtx, s.ctxPath = true, argv[i+1]
		b, err := os.ReadFile(s.ctxPath) //nolint:gosec // G304: the path this stub was handed
		if err != nil {
			s.t.Errorf("reading the context file during the run: %v", err)
		}
		s.ctxText = string(b)
		if fi, err := os.Stat(s.ctxPath); err == nil {
			s.ctxMode = fi.Mode().Perm()
		}
	}
	switch {
	case s.hold:
		<-ctx.Done()
		return -1, ctx.Err()
	case s.fail != nil:
		return -1, s.fail
	}
	// The verdict echoes the bead om was told, so these tests carry no warning
	// about a missing echo; the reviewer's own warnings are another file's
	// business.
	body := `{"score":0.9,"verdict":"approve","findings":[]}`
	if i := slices.Index(argv, "--bead"); i >= 0 {
		body = fmt.Sprintf(`{"score":0.9,"verdict":"approve","bead":%q,"findings":[]}`, argv[i+1])
	}
	if i := slices.Index(argv, "--out"); i >= 0 {
		if err := os.WriteFile(argv[i+1], []byte(body), 0o600); err != nil {
			s.t.Fatal(err)
		}
	}
	return 0, nil
}

func criteriaStubbed(t *testing.T, bin string, stub *criteriaStub) OMReviewer {
	t.Helper()
	r := OMReviewer{Path: bin, OutDir: t.TempDir()}
	r.run = stub.run
	return r
}

// gone asserts the criteria file the stub saw is removed.
func (s *criteriaStub) gone(t *testing.T) {
	t.Helper()
	if !s.sawFlag {
		t.Fatal("the review never carried --criteria-file")
	}
	if _, err := os.Stat(s.critPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("criteria file %s still there after the run (err %v)", s.critPath, err)
	}
}

// TestOMReviewerPassesTheCriteriaFile: with the flag supported and acceptance
// text on the bead, the argv names a file that holds exactly that text, and the
// file is gone once the review is over.
func TestOMReviewerPassesTheCriteriaFile(t *testing.T) {
	t.Parallel()
	const criteria = "- [x] one\n- [ ] two\n\nprose about three\n"
	bin, dir := omBinary(t), t.TempDir()
	stub := &criteriaStub{t: t, help: omHelpWithCriteria}
	r := criteriaStubbed(t, bin, stub)
	var log strings.Builder
	v, err := r.ReviewBead(context.Background(), ReviewInput{Repo: baseRepo{},
		Dir: dir, Base: "aaa", Head: "bbb", BeadID: "gt-abc", CriteriaText: criteria, Logf: logfTo(&log)})
	if err != nil {
		t.Fatal(err)
	}
	if v.Verdict != VerdictApprove {
		t.Errorf("verdict = %+v, want an approve", v)
	}
	prefix := reviewPrefix(bin, dir)
	if !slices.Equal(stub.argv[:len(prefix)], prefix) {
		t.Errorf("argv = %q, want it to start with %q", stub.argv, prefix)
	}
	// The out file's path, then the two flags in the order they are added.
	rest := stub.argv[len(prefix):]
	if len(rest) != 5 || rest[1] != "--bead" || rest[2] != "gt-abc" ||
		rest[3] != "--criteria-file" || rest[4] != stub.critPath {
		t.Errorf("argv = %q, want --bead and --criteria-file after the out file", stub.argv)
	}
	if stub.text != criteria {
		t.Errorf("criteria file held %q during the run, want %q", stub.text, criteria)
	}
	if stub.mode != 0o600 {
		t.Errorf("criteria file mode = %v, want 0600", stub.mode)
	}
	if got := filepath.Dir(stub.critPath); got != r.OutDir {
		t.Errorf("criteria file lives in %s, want the verdict directory %s", got, r.OutDir)
	}
	stub.gone(t)
	if log.String() != "" {
		t.Errorf("a review with criteria logged %q", log.String())
	}
}

// TestOMReviewerProbesTheHelpOncePerBinary: whether om takes the criteria flag
// is a property of the installed binary, so one help read answers it however
// many landings ask.
func TestOMReviewerProbesTheHelpOncePerBinary(t *testing.T) {
	t.Parallel()
	bin := omBinary(t)
	stub := &criteriaStub{t: t, help: omHelpWithCriteria}
	r := criteriaStubbed(t, bin, stub)
	for i := 0; i < 3; i++ {
		if _, err := r.ReviewBead(context.Background(), ReviewInput{Repo: baseRepo{},
			Dir: t.TempDir(), Base: "a", Head: "b", BeadID: "gt-abc", CriteriaText: "- [ ] one"}); err != nil {
			t.Fatal(err)
		}
	}
	if stub.probes != 1 {
		t.Errorf("probed %d times, want 1: the flags are a property of the binary", stub.probes)
	}
}

// TestOMReviewerReprobesAReinstalledOM: an om replaced under a running daemon is
// a different binary, whatever the path says, so its flags are read again.
func TestOMReviewerReprobesAReinstalledOM(t *testing.T) {
	t.Parallel()
	bin := omBinary(t)
	stub := &criteriaStub{t: t, help: omHelpWithCriteria}
	r := criteriaStubbed(t, bin, stub)
	in := func() ReviewInput {
		return ReviewInput{Repo: baseRepo{}, Dir: t.TempDir(), Base: "a", Head: "b", BeadID: "gt-abc", CriteriaText: "- [ ] one"}
	}
	if _, err := r.ReviewBead(context.Background(), in()); err != nil {
		t.Fatal(err)
	}
	// The upgrade: same path, a binary of another size, so a new identity.
	if err := os.WriteFile(bin, []byte("nominal om, reinstalled\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReviewBead(context.Background(), in()); err != nil {
		t.Fatal(err)
	}
	if stub.probes != 2 {
		t.Errorf("probed %d times, want 2: a reinstalled om is probed again", stub.probes)
	}
}

// TestOMReviewerRetriesAFailedProbe: a probe that cannot run is not an answer,
// so the criteria flag is left off, the failure is logged, and the next review
// probes again. The bead's id is on the argv either way (gt-r3fg9): the flags
// the probe decides are never the one the review must carry.
func TestOMReviewerRetriesAFailedProbe(t *testing.T) {
	t.Parallel()
	bin := omBinary(t)
	stub := &criteriaStub{t: t, help: omHelpWithCriteria, probeFail: 1}
	r := criteriaStubbed(t, bin, stub)
	var log strings.Builder
	in := func() ReviewInput {
		return ReviewInput{Repo: baseRepo{}, Dir: t.TempDir(), Base: "a", Head: "b", BeadID: "gt-abc",
			CriteriaText: "- [ ] one", Logf: logfTo(&log)}
	}
	if _, err := r.ReviewBead(context.Background(), in()); err != nil {
		t.Fatal(err)
	}
	if stub.sawFlag {
		t.Errorf("argv = %q carries --criteria-file after a failed probe", stub.argv)
	}
	if !slices.Contains(stub.argv, "--bead") {
		t.Errorf("argv = %q, want the bead's id whatever the probe answered", stub.argv)
	}
	if !strings.Contains(log.String(), "could not probe") {
		t.Errorf("log = %q, want the failed probe named", log.String())
	}
	if _, err := r.ReviewBead(context.Background(), in()); err != nil {
		t.Fatal(err)
	}
	if stub.probes != 2 {
		t.Errorf("probed %d times, want 2: a failed probe is not cached", stub.probes)
	}
	if !stub.sawFlag {
		t.Errorf("argv = %q, want --criteria-file once the probe answers", stub.argv)
	}
}

// TestOMReviewerRetriesAProbeWithNoHelpText: an om that exits without printing
// help is as unprobed as one that failed, so the criteria flag stays off and the
// probe is retried.
func TestOMReviewerRetriesAProbeWithNoHelpText(t *testing.T) {
	t.Parallel()
	bin := omBinary(t)
	stub := &criteriaStub{t: t, help: ""}
	r := criteriaStubbed(t, bin, stub)
	var log strings.Builder
	in := func() ReviewInput {
		return ReviewInput{Repo: baseRepo{}, Dir: t.TempDir(), Base: "a", Head: "b", BeadID: "gt-abc",
			CriteriaText: "- [ ] one", Logf: logfTo(&log)}
	}
	for i := 0; i < 2; i++ {
		if _, err := r.ReviewBead(context.Background(), in()); err != nil {
			t.Fatal(err)
		}
	}
	if stub.sawFlag {
		t.Errorf("argv = %q carries --criteria-file with no help text to justify it", stub.argv)
	}
	if stub.probes != 2 {
		t.Errorf("probed %d times, want 2: a help-less probe is not cached", stub.probes)
	}
	if !strings.Contains(log.String(), "no output") {
		t.Errorf("log = %q, want the missing help named", log.String())
	}
}

// TestOMReviewerRemovesTheCriteriaFileOnErrorAndTimeout: the file is a temp
// file on every path out of the attempt, not only a successful one.
func TestOMReviewerRemovesTheCriteriaFileOnErrorAndTimeout(t *testing.T) {
	t.Parallel()
	t.Run("an om that failed to run", func(t *testing.T) {
		t.Parallel()
		bin := omBinary(t)
		stub := &criteriaStub{t: t, help: omHelpWithCriteria, fail: errors.New("exec: om: not found")}
		if _, err := criteriaStubbed(t, bin, stub).ReviewBead(context.Background(), ReviewInput{Repo: baseRepo{},
			Dir: t.TempDir(), Base: "a", Head: "b", CriteriaText: "- [ ] one"}); err == nil {
			t.Fatal("a failed run returned no error")
		}
		stub.gone(t)
	})
	t.Run("an om that timed out", func(t *testing.T) {
		t.Parallel()
		bin := omBinary(t)
		stub := &criteriaStub{t: t, help: omHelpWithCriteria, hold: true}
		r := criteriaStubbed(t, bin, stub)
		r.Timeout = 50 * time.Millisecond
		_, err := r.ReviewBead(context.Background(), ReviewInput{Repo: baseRepo{},
			Dir: t.TempDir(), Base: "a", Head: "b", CriteriaText: "- [ ] one"})
		if !errors.Is(err, ErrOMTimeout) {
			t.Fatalf("err = %v, want a timeout", err)
		}
		stub.gone(t)
	})
}

// TestOMReviewerOmitsCriteriaTheHelpDoesNotList: an om without the flag is
// called with the argv it had before the flag existed.
func TestOMReviewerOmitsCriteriaTheHelpDoesNotList(t *testing.T) {
	t.Parallel()
	bin, dir := omBinary(t), t.TempDir()
	stub := &criteriaStub{t: t, help: omHelpWithoutCriteria}
	if _, err := criteriaStubbed(t, bin, stub).ReviewBead(context.Background(), ReviewInput{Repo: baseRepo{},
		Dir: dir, Base: "aaa", Head: "bbb", BeadID: "gt-abc", CriteriaText: "- [ ] one"}); err != nil {
		t.Fatal(err)
	}
	if stub.sawFlag {
		t.Errorf("argv = %q carries --criteria-file the help does not list", stub.argv)
	}
	// The out file's path is the only name a run picks for itself, so the argv
	// is checked by its shape: the review's own prefix, that path, --bead.
	prefix := reviewPrefix(bin, dir)
	if !slices.Equal(stub.argv[:len(prefix)], prefix) {
		t.Errorf("argv = %q, want it to start with %q", stub.argv, prefix)
	}
	if tail := stub.argv[len(prefix):]; len(tail) != 3 || tail[1] != "--bead" || tail[2] != "gt-abc" {
		t.Errorf("argv = %q, want it to end at --bead gt-abc", stub.argv)
	}
}

// TestOMReviewerOmitsCriteriaWithoutText: a bead with no acceptance text logs
// nothing and is reviewed as before — there is no file to write.
func TestOMReviewerOmitsCriteriaWithoutText(t *testing.T) {
	t.Parallel()
	bin := omBinary(t)
	stub := &criteriaStub{t: t, help: omHelpWithCriteria}
	var log strings.Builder
	if _, err := criteriaStubbed(t, bin, stub).ReviewBead(context.Background(), ReviewInput{Repo: baseRepo{},
		Dir: t.TempDir(), Base: "aaa", Head: "bbb", BeadID: "gt-abc", Logf: logfTo(&log)}); err != nil {
		t.Fatal(err)
	}
	if stub.sawFlag {
		t.Errorf("argv = %q carries --criteria-file with no text to put in it", stub.argv)
	}
	if log.String() != "" {
		t.Errorf("an absent section logged %q", log.String())
	}
}

// TestOMReviewerRunsWithoutCriteriaWhenTheWriteFails: the file is a help to om,
// so a write that fails costs a log line and nothing else.
func TestOMReviewerRunsWithoutCriteriaWhenTheWriteFails(t *testing.T) {
	t.Parallel()
	bin := omBinary(t)
	stub := &criteriaStub{t: t, help: omHelpWithCriteria}
	r := criteriaStubbed(t, bin, stub)
	r.criteriaWriter = func(ReviewInput) (string, error) { return "", errors.New("no space left on device") }
	var log strings.Builder
	v, err := r.ReviewBead(context.Background(), ReviewInput{Repo: baseRepo{},
		Dir: t.TempDir(), Base: "aaa", Head: "bbb", BeadID: "gt-abc", CriteriaText: "- [ ] one", Logf: logfTo(&log)})
	if err != nil {
		t.Fatalf("ReviewBead: %v; an unwritable criteria file must not fail the review", err)
	}
	if v.Verdict != VerdictApprove {
		t.Errorf("verdict = %+v, want the approve om returned", v)
	}
	if stub.sawFlag {
		t.Errorf("argv = %q carries --criteria-file that was never written", stub.argv)
	}
	if got := log.String(); !strings.Contains(got, "could not write the acceptance criteria file") ||
		!strings.Contains(got, "no space left on device") {
		t.Errorf("log = %q, want one line naming the failed write", got)
	}
}

// TestLandGivesTheReviewerTheAcceptanceText: the landing hands the reviewer the
// text of the bead it is landing, from the description section the dispatcher
// writes when the bead's own field is empty (gt-yl7n4). The section shapes the
// reader must handle are covered where the reader lives, in internal/beads.
func TestLandGivesTheReviewerTheAcceptanceText(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	// Both ticked: an unchecked box in the section becomes a policy rejection
	// (gt-1mrji), covered by TestPolicyRejectionCountsDescriptionAcceptance.
	const criteria = "- [x] one\n- [x] two"
	f.bd.Seed(beads.Issue{ID: "gt-abc", Title: "add b", Status: "hooked", Type: "task",
		Description: "## Goal\n\nadd b\n\n## Acceptance\n\n" + criteria + "\n\n## Out of scope\n\nnothing\n",
		Labels:      []string{LabelReadyToLand}, Notes: FormatReadyNote(f.work)})
	rev := &recordBeadReviewer{verdict: Verdict{Verdict: VerdictApprove, Score: 0.9}}
	l := f.lander()
	l.Reviewer = rev
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}
	if len(rev.inputs) != 1 {
		t.Fatalf("reviewer calls = %d, want 1", len(rev.inputs))
	}
	if got := rev.inputs[0].CriteriaText; got != criteria {
		t.Errorf("ReviewInput.CriteriaText = %q, want %q", got, criteria)
	}
}
