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

	"github.com/steveyegge/gastown/internal/beads"
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
// version. It counts the probes and keeps the argv of every review.
type omStub struct {
	t         *testing.T
	help      string // what `review --help` prints
	fail      int    // help probes to fail with a run error before answering
	bead      string // the bead the verdict echoes; "" echoes none
	body      string // what a review writes; "" writes an approve echoing bead
	code      int    // what a review exits with; 0 is approve
	noVersion bool   // an om whose `version` call prints nothing
	probes    int
	versions  int
	prior     string     // the previous round's findings file as the review read it
	argv      []string   // the last review's argv
	argvs     [][]string // every review's argv, in the order om ran
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
	s.argvs = append(s.argvs, slices.Clone(argv))
	body := s.body
	if body == "" {
		body = `{"score":0.9,"verdict":"approve","findings":[]}`
		if s.bead != "" {
			body = fmt.Sprintf(`{"score":0.9,"verdict":"approve","bead":%q,"findings":[]}`, s.bead)
		}
	}
	for i, a := range argv {
		if a == "--out" {
			if err := os.WriteFile(argv[i+1], []byte(body), 0o600); err != nil {
				s.t.Fatal(err)
			}
		}
		// om reads the file the flag names during the review, before the
		// reviewer removes it: the content a test asserts on is what om saw.
		if a == "--prior-findings" && i+1 < len(argv) {
			data, err := os.ReadFile(argv[i+1])
			if err != nil {
				s.t.Fatal(err)
			}
			s.prior = string(data)
		}
	}
	_, _ = io.WriteString(out, "om: reviewing\n")
	return s.code, nil
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

// TestOMReviewerAlwaysPassesTheBead: the id is on the argv of every review that
// names a bead, whatever the installed om's help says. The id in om's prompt is
// the only thing usage attribution can join a review to its work on, so a
// review that quietly omits it is one the feed cannot place (gt-r3fg9).
func TestOMReviewerAlwaysPassesTheBead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		help string
	}{
		{name: "the help lists the flag", help: omHelpWithBead},
		{name: "the help does not list it", help: omHelpWithoutBead},
		{name: "om prints no help", help: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bin := omBinary(t)
			stub := &omStub{t: t, help: tc.help, bead: "gt-abc"}
			dir := t.TempDir()
			var log strings.Builder
			v, err := omStubbed(t, bin, stub).ReviewBead(context.Background(),
				ReviewInput{Repo: baseRepo{}, Dir: dir, Base: "aaa", Head: "bbb", BeadID: "gt-abc", Logf: logfTo(&log)})
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
			// The bead's flag is never a question for the help read: with no
			// acceptance text, this review has nothing left to probe for.
			if stub.probes != 0 {
				t.Errorf("probed %d times, want 0: --bead is not gated on the help", stub.probes)
			}
			if log.String() != "" {
				t.Errorf("an echoed bead logged %q", log.String())
			}
		})
	}
}

// TestOMReviewerKeepsTheBeadWhenTheOnlyFlagIsMissing: an om that takes no
// criteria still gets the bead's id — the one flag the review cannot drop.
func TestOMReviewerKeepsTheBeadWhenTheOnlyFlagIsMissing(t *testing.T) {
	t.Parallel()
	bin, dir := omBinary(t), t.TempDir()
	stub := &omStub{t: t, help: omHelpWithoutBead, bead: "gt-abc"}
	if _, err := omStubbed(t, bin, stub).ReviewBead(context.Background(), ReviewInput{Repo: baseRepo{},
		Dir: dir, Base: "aaa", Head: "bbb", BeadID: "gt-abc", CriteriaText: "- [ ] one"}); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(stub.argv, "--criteria-file") {
		t.Errorf("argv = %q carries --criteria-file the help does not list", stub.argv)
	}
	prefix := reviewPrefix(bin, dir)
	if tail := stub.argv[len(prefix):]; len(tail) != 3 || tail[1] != "--bead" || tail[2] != "gt-abc" {
		t.Errorf("argv = %q, want it to end at --bead gt-abc", stub.argv)
	}
}

// TestLandNamesEveryOMReviewForItsWorkBead: the landing's own om run carries the
// id of the bead it is landing, on the argv of a first landing (gt-r3fg9).
func TestLandNamesEveryOMReviewForItsWorkBead(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	stub := &omStub{t: t, help: omHelpWithBead, bead: f.work.BeadID}
	l := f.lander()
	l.Reviewer = omStubbed(t, omBinary(t), stub)
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}
	beadOnEveryOMArgv(t, stub, f.work.BeadID, 1)
}

// TestLandNamesEveryOMReviewForAReworkedBead: a landing om asked changes on is
// reworked by its author and lands on a later head. Every om run over that bead
// names it, so the review the feed places is the one that judged the head that
// actually landed (gt-r3fg9).
func TestLandNamesEveryOMReviewForAReworkedBead(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	stub := &omStub{t: t, bead: f.work.BeadID}
	// The first landing asks for changes; the rework is approved.
	stub.body = fmt.Sprintf(`{"score":0.3,"verdict":"request_changes","bead":%q,"findings":[]}`, f.work.BeadID)
	stub.code = 1
	l := f.lander()
	l.Reviewer = omStubbed(t, omBinary(t), stub)
	var rej *Rejection
	if _, err := l.Land(context.Background(), f.work); !errors.As(err, &rej) || rej.Kind != RejectReview {
		t.Fatalf("first Land = %T %v, want a review rejection", err, err)
	}
	// The author reworks the branch and re-submits the same bead: the label
	// gt done puts back is the whole of the next round's readiness.
	f.setBranch("fix: address the review")
	if err := f.bd.Update(f.work.BeadID, beads.UpdateOptions{
		AddLabels: []string{LabelReadyToLand}, RemoveLabels: []string{LabelRework}}); err != nil {
		t.Fatalf("re-submitting the reworked bead: %v", err)
	}
	stub.body, stub.code = "", 0
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("the reworked landing: %v", err)
	}
	beadOnEveryOMArgv(t, stub, f.work.BeadID, 2)
}

// beadOnEveryOMArgv asserts om ran exactly want times and that every run named
// bead, so a landing that drops the id fails here whatever else it does.
func beadOnEveryOMArgv(t *testing.T, stub *omStub, bead string, want int) {
	t.Helper()
	if len(stub.argvs) != want {
		t.Fatalf("om ran %d review(s), want %d: %q", len(stub.argvs), want, stub.argvs)
	}
	for i, argv := range stub.argvs {
		j := slices.Index(argv, "--bead")
		if j < 0 || j+1 >= len(argv) || argv[j+1] != bead {
			t.Errorf("review %d argv = %q, want it to carry --bead %s", i+1, argv, bead)
		}
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
		ReviewInput{Repo: baseRepo{}, Dir: t.TempDir(), Base: "a", Head: "b", BeadID: "gt-abc", Logf: logfTo(&log)})
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
		ReviewInput{Repo: baseRepo{}, Dir: t.TempDir(), Base: "a", Head: "b", BeadID: "gt-abc", Logf: logfTo(&log)})
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
