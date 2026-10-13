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

// omHelpWithBead is the help text of an om that knows the work bead: usage om
// prints to stderr, the flags one per line.
const omHelpWithBead = `Usage:
  om review [flags]

Flags:
  --base string   the base of the range
  --bead string   the work bead the diff belongs to
  --head string   the head of the range
`

const omHelpWithoutBead = `Usage:
  om review [flags]

Flags:
  --base string   the base of the range
  --head string   the head of the range
`

// omBinary writes a stand-in for an installed om, so the help probe has a file
// whose identity it can key on. The run seam never executes it.
func omBinary(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "om")
	if err := os.WriteFile(path, []byte("nominal om\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// logfTo is a ReviewInput.Logf a test can read back.
func logfTo(w io.Writer) func(format string, args ...any) {
	return func(format string, args ...any) {
		_, _ = fmt.Fprintf(w, "%s\n", fmt.Sprintf(format, args...))
	}
}

// omStub answers every call om sees: the `review --help` probe with help, the
// review itself with an approve verdict echoing Bead, and `om version` with a
// version. It counts the probes and keeps the review's argv.
type omStub struct {
	t         *testing.T
	help      string // what `review --help` prints
	fail      int    // help probes to fail with a run error before answering
	bead      string // the bead the verdict echoes; "" echoes none
	noVersion bool   // an om whose `version` call prints nothing
	probes    int
	versions  int
	argv      []string
}

func (s *omStub) run(_ context.Context, _ string, _ []string, argv []string, out io.Writer) (int, error) {
	if omSubcommand(argv) == "version" {
		s.versions++
		if !s.noVersion {
			_, _ = io.WriteString(out, stubVersionLine())
		}
		return 0, nil
	}
	if slices.Contains(argv, "--help") {
		s.probes++
		if s.probes <= s.fail {
			return -1, errors.New("exec: om: not found")
		}
		// om prints usage to stderr and exits 2, which is why the exit code
		// decides nothing.
		_, _ = io.WriteString(out, s.help)
		return 2, nil
	}
	s.argv = argv
	body := `{"score":0.9,"verdict":"approve","findings":[]}`
	if s.bead != "" {
		body = fmt.Sprintf(`{"score":0.9,"verdict":"approve","bead":%q,"findings":[]}`, s.bead)
	}
	for i, a := range argv {
		if a == "--out" {
			if err := os.WriteFile(argv[i+1], []byte(body), 0o600); err != nil {
				s.t.Fatal(err)
			}
		}
	}
	_, _ = io.WriteString(out, "om: reviewing\n")
	return 0, nil
}

func omStubbed(t *testing.T, bin string, stub *omStub) OMReviewer {
	t.Helper()
	r := OMReviewer{Path: bin, OutDir: t.TempDir()}
	r.run = stub.run
	return r
}

// reviewPrefix is the argv every review carries before the out file: the shape
// a landing has run since before --bead existed.
func reviewPrefix(bin, dir string) []string {
	return []string{bin, "review", "-C", dir, "--base", "aaa", "--head", "bbb", "--out"}
}

// TestOMReviewerPassesTheBeadWhenTheHelpListsIt: the flag rides the argv, after
// the range and the out file, and the echoed id comes back on the verdict.
func TestOMReviewerPassesTheBeadWhenTheHelpListsIt(t *testing.T) {
	t.Parallel()
	bin := omBinary(t)
	stub := &omStub{t: t, help: omHelpWithBead, bead: "gt-abc"}
	dir := t.TempDir()
	var log strings.Builder
	v, err := omStubbed(t, bin, stub).ReviewBead(context.Background(),
		ReviewInput{Dir: dir, Base: "aaa", Head: "bbb", BeadID: "gt-abc", Logf: logfTo(&log)})
	if err != nil {
		t.Fatal(err)
	}
	if v.Verdict != VerdictApprove || v.Bead != "gt-abc" {
		t.Errorf("verdict = %+v, want an approve echoing gt-abc", v)
	}
	prefix := reviewPrefix(bin, dir)
	if !slices.Equal(stub.argv[:len(prefix)], prefix) {
		t.Errorf("argv = %q, want it to start with %q", stub.argv, prefix)
	}
	if rest := stub.argv[len(prefix):]; len(rest) != 3 || rest[1] != "--bead" || rest[2] != "gt-abc" {
		t.Errorf("argv = %q, want --bead gt-abc after the out file", stub.argv)
	}
	if stub.probes != 1 {
		t.Errorf("probed %d times, want 1", stub.probes)
	}
	if log.String() != "" {
		t.Errorf("an echoed bead logged %q", log.String())
	}
}

// TestOMReviewerOmitsTheBeadWhenTheHelpDoesNotListIt: an om without the flag is
// called with the argv it has always been called with.
func TestOMReviewerOmitsTheBeadWhenTheHelpDoesNotListIt(t *testing.T) {
	t.Parallel()
	bin := omBinary(t)
	stub := &omStub{t: t, help: omHelpWithoutBead, bead: "gt-abc"}
	dir := t.TempDir()
	if _, err := omStubbed(t, bin, stub).ReviewBead(context.Background(),
		ReviewInput{Dir: dir, Base: "aaa", Head: "bbb", BeadID: "gt-abc"}); err != nil {
		t.Fatal(err)
	}
	prefix := reviewPrefix(bin, dir)
	if len(stub.argv) != len(prefix)+1 || !slices.Equal(stub.argv[:len(prefix)], prefix) {
		t.Errorf("argv = %q, want the %d-argument argv of a review with no bead", stub.argv, len(prefix)+1)
	}
}

// TestOMReviewerProbesOncePerBinary: the answer is a property of the installed
// binary, so it is read once however many landings ask for it.
func TestOMReviewerProbesOncePerBinary(t *testing.T) {
	t.Parallel()
	bin := omBinary(t)
	stub := &omStub{t: t, help: omHelpWithBead, bead: "gt-abc"}
	r := omStubbed(t, bin, stub)
	for i := 0; i < 3; i++ {
		if _, err := r.ReviewBead(context.Background(),
			ReviewInput{Dir: t.TempDir(), Base: "a", Head: "b", BeadID: "gt-abc"}); err != nil {
			t.Fatal(err)
		}
	}
	if stub.probes != 1 {
		t.Errorf("probed %d times, want 1 per binary identity", stub.probes)
	}
}

// TestOMReviewerReprobesAReinstalledOM: an om replaced under a running daemon is
// a different binary, whatever the path says.
func TestOMReviewerReprobesAReinstalledOM(t *testing.T) {
	t.Parallel()
	bin := omBinary(t)
	stub := &omStub{t: t, help: omHelpWithBead, bead: "gt-abc"}
	r := omStubbed(t, bin, stub)
	in := func() ReviewInput {
		return ReviewInput{Dir: t.TempDir(), Base: "a", Head: "b", BeadID: "gt-abc"}
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
// so the flag is left off, the failure is logged, and the next review probes
// again.
func TestOMReviewerRetriesAFailedProbe(t *testing.T) {
	t.Parallel()
	bin := omBinary(t)
	stub := &omStub{t: t, help: omHelpWithBead, bead: "gt-abc", fail: 1}
	r := omStubbed(t, bin, stub)
	var log strings.Builder
	in := func() ReviewInput {
		return ReviewInput{Dir: t.TempDir(), Base: "a", Head: "b", BeadID: "gt-abc", Logf: logfTo(&log)}
	}
	if _, err := r.ReviewBead(context.Background(), in()); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(stub.argv, "--bead") {
		t.Errorf("argv = %q carries --bead after a failed probe", stub.argv)
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
	if !slices.Contains(stub.argv, "--bead") {
		t.Errorf("argv = %q, want --bead once the probe answers", stub.argv)
	}
}

// TestOMReviewerRetriesAProbeWithNoHelpText: an om that exits without printing
// help is as unprobed as one that failed, so the flag stays off and the probe is
// retried.
func TestOMReviewerRetriesAProbeWithNoHelpText(t *testing.T) {
	t.Parallel()
	bin := omBinary(t)
	stub := &omStub{t: t, help: "", bead: "gt-abc"}
	r := omStubbed(t, bin, stub)
	var log strings.Builder
	in := func() ReviewInput {
		return ReviewInput{Dir: t.TempDir(), Base: "a", Head: "b", BeadID: "gt-abc", Logf: logfTo(&log)}
	}
	for i := 0; i < 2; i++ {
		if _, err := r.ReviewBead(context.Background(), in()); err != nil {
			t.Fatal(err)
		}
	}
	if slices.Contains(stub.argv, "--bead") {
		t.Errorf("argv = %q carries --bead with no help text to justify it", stub.argv)
	}
	if stub.probes != 2 {
		t.Errorf("probed %d times, want 2: a help-less probe is not cached", stub.probes)
	}
	if !strings.Contains(log.String(), "no output") {
		t.Errorf("log = %q, want the missing help named", log.String())
	}
}

// TestOMReviewerLogsABeadMismatch: a verdict naming another bead is a warning
// for a reader, never a change to the verdict the landing records.
func TestOMReviewerLogsABeadMismatch(t *testing.T) {
	t.Parallel()
	bin := omBinary(t)
	stub := &omStub{t: t, help: omHelpWithBead, bead: "gt-other"}
	var log strings.Builder
	v, err := omStubbed(t, bin, stub).ReviewBead(context.Background(),
		ReviewInput{Dir: t.TempDir(), Base: "a", Head: "b", BeadID: "gt-abc", Logf: logfTo(&log)})
	if err != nil {
		t.Fatalf("ReviewBead: %v; a mismatch must not fail the landing", err)
	}
	if v.Verdict != VerdictApprove || v.Score != 0.9 || v.Bead != "gt-other" {
		t.Errorf("verdict = %+v, want it returned unchanged", v)
	}
	for _, want := range []string{"WARNING", "gt-other", "gt-abc"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log = %q, want it to carry %q", log.String(), want)
		}
	}
}

// TestOMReviewerLogsAMissingEcho: an om that was told the bead and echoed none
// is logged too, and nothing else changes.
func TestOMReviewerLogsAMissingEcho(t *testing.T) {
	t.Parallel()
	bin := omBinary(t)
	stub := &omStub{t: t, help: omHelpWithBead}
	var log strings.Builder
	v, err := omStubbed(t, bin, stub).ReviewBead(context.Background(),
		ReviewInput{Dir: t.TempDir(), Base: "a", Head: "b", BeadID: "gt-abc", Logf: logfTo(&log)})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(stub.argv, "--bead") {
		t.Errorf("argv = %q, want --bead passed", stub.argv)
	}
	if v.Verdict != VerdictApprove || v.Bead != "" {
		t.Errorf("verdict = %+v, want an approve with no bead", v)
	}
	if !strings.Contains(log.String(), "did not echo") {
		t.Errorf("log = %q, want the missing echo named", log.String())
	}
}

// TestVerdictDecodesWithAndWithoutBead: an om that predates the flag and one
// that carries it both decode.
func TestVerdictDecodesWithAndWithoutBead(t *testing.T) {
	t.Parallel()
	var with, without Verdict
	if err := json.Unmarshal([]byte(`{"verdict":"approve","score":0.9,"bead":"gt-abc","findings":[]}`), &with); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"verdict":"approve","score":0.9,"findings":[]}`), &without); err != nil {
		t.Fatal(err)
	}
	if with.Bead != "gt-abc" || without.Bead != "" {
		t.Errorf("bead = %q and %q, want gt-abc and empty", with.Bead, without.Bead)
	}
}

// TestOMHelpListsFlagMatchesWholeWords: a flag listing is a word, so a help text
// that names --bead-id or --beads does not put --bead on an argv om would
// reject.
func TestOMHelpListsFlagMatchesWholeWords(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		help string
		want bool
	}{
		{"Flags:\n  --bead string   the work bead\n", true},
		{"Flags:\n  --bead=string   the work bead\n", true},
		{"Flags:\n  --bead, -b string   the work bead\n", true},
		{"Flags:\n  --bead-id string   the work bead's id\n", false},
		{"Flags:\n  --beads string   every bead\n", false},
		{"Flags:\n  --no-bead\n", false},
		{"Flags:\n  --head string\n", false},
	} {
		if got := omHelpListsFlag(tc.help, "--bead"); got != tc.want {
			t.Errorf("omHelpListsFlag(%.40q) = %v, want %v", tc.help, got, tc.want)
		}
	}
}

// recordBeadReviewer implements both halves of the reviewer contract: Land
// takes the BeadReviewer half, and the Review half is there so the fallback is
// one deleted line away from this test.
type recordBeadReviewer struct {
	verdict Verdict
	err     error
	inputs  []ReviewInput
}

func (r *recordBeadReviewer) Review(context.Context, string, string, string) (Verdict, error) {
	return r.verdict, r.err
}

func (r *recordBeadReviewer) ReviewBead(_ context.Context, in ReviewInput) (Verdict, error) {
	r.inputs = append(r.inputs, in)
	return r.verdict, r.err
}

// TestLandNamesTheWorkBeadToTheReviewer: the landing gives the reviewer the bead
// it is landing, the range it merged and the log writer its warnings belong in
// (gt-pwvv4).
func TestLandNamesTheWorkBeadToTheReviewer(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	rev := &recordBeadReviewer{verdict: Verdict{Verdict: VerdictApprove, Score: 0.9}}
	l := f.lander()
	l.Reviewer = rev
	var log strings.Builder
	l.Out = &log
	res, err := l.Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if len(rev.inputs) != 1 {
		t.Fatalf("reviewer calls = %d, want 1", len(rev.inputs))
	}
	in := rev.inputs[0]
	if in.BeadID != f.work.BeadID || in.Base != f.base || in.Head != res.LandedCommit {
		t.Errorf("ReviewInput = %+v, want bead %s, base %s and head %s", in, f.work.BeadID, f.base, res.LandedCommit)
	}
	if in.Dir == "" {
		t.Error("ReviewInput.Dir is empty: the reviewer was told no tree")
	}
	if in.Logf == nil {
		t.Fatal("ReviewInput.Logf is nil: a reviewer warning would go nowhere")
	}
	in.Logf("WARNING a reviewer line")
	if !strings.Contains(log.String(), "WARNING a reviewer line") {
		t.Errorf("log = %q, want the reviewer's warning in the landing log", log.String())
	}
}
