package land

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// isolatedApprove is the verdict every isolated review in these rows reaches.
const isolatedApprove = `{"score":0.91,"verdict":"approve","findings":[]}`

// reviewIsolated runs one ReviewBead through c, a copy Isolated returned, with om
// answered by the fake that records argv in argv. It returns the verdict file om
// was given as --out, which the fake writes.
func reviewIsolated(t *testing.T, c Reviewer, argv *[]string, in ReviewInput) (string, Verdict, error) {
	t.Helper()
	br, ok := c.(BeadReviewer)
	if !ok {
		t.Fatalf("%T is not a BeadReviewer", c)
	}
	in.Dir, in.Base, in.Head, in.Repo = "/wt", "aaa", "bbb", baseRepo{}
	v, err := br.ReviewBead(context.Background(), in)
	if err != nil {
		return "", v, err
	}
	i := slices.Index(*argv, "--out")
	if i < 0 || i+1 >= len(*argv) {
		t.Fatalf("om was not given --out: %v", *argv)
	}
	return (*argv)[i+1], v, nil
}

// recordingEnv answers om's version probe and its review, and records the env
// each call was given by subcommand, so a row can read what the child sees.
func recordingEnv(t *testing.T, envs map[string][]string) runFunc {
	t.Helper()
	return func(_ context.Context, _ string, env, argv []string, out io.Writer) (int, error) {
		envs[omSubcommand(argv)] = slices.Clone(env)
		if omSubcommand(argv) == "version" {
			_, _ = io.WriteString(out, stubVersionLine())
			return 0, nil
		}
		for i, a := range argv {
			if a == "--out" {
				if err := os.WriteFile(argv[i+1], []byte(isolatedApprove), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		return 0, nil
	}
}

// TestVerdictReuseIsolatedSetsTMPDIR: om keeps its raw replies in os.TempDir()
// when its caller names none, so the review call of an isolated copy is given
// TMPDIR=tmp; omUnsetEnv still rides the same env as an unset entry, so om's
// CLAUDE_CONFIG_DIR is still stripped.
func TestVerdictReuseIsolatedSetsTMPDIR(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	envs := map[string][]string{}
	r := OMReviewer{OutDir: t.TempDir()}
	r.run = recordingEnv(t, envs)
	c, ok := r.Isolated(tmp).(BeadReviewer)
	if !ok {
		t.Fatalf("%T is not a BeadReviewer", c)
	}
	in := ReviewInput{BeadID: "gt-1e7u58"}
	in.Dir, in.Base, in.Head, in.Repo = "/wt", "aaa", "bbb", baseRepo{}
	if _, err := c.ReviewBead(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	env, ok := envs["review"]
	if !ok {
		t.Fatalf("om was not run for the review; %d om call(s) recorded", len(envs))
	}
	if want := "TMPDIR=" + tmp; !slices.Contains(env, want) {
		t.Fatalf("review env's %d entries hold no TMPDIR for the isolated tmp", len(env))
	}
	if !slices.Contains(env, "CLAUDE_CONFIG_DIR") {
		t.Fatalf("review env's %d entries hold no CLAUDE_CONFIG_DIR unset entry", len(env))
	}
}

// TestVerdictReuseNonIsolatedEnvUnchanged: an Isolated copy for a non-empty tmp
// adds TMPDIR; a reviewer Isolated did not make hands its review exactly
// omUnsetEnv, so a pool's TMPDIR never reaches the landing's own om and points
// its raw replies at another review's directory.
func TestVerdictReuseNonIsolatedEnvUnchanged(t *testing.T) {
	t.Parallel()
	envs := map[string][]string{}
	r := OMReviewer{OutDir: t.TempDir()}
	r.run = recordingEnv(t, envs)
	in := ReviewInput{BeadID: "gt-pibum9"}
	in.Dir, in.Base, in.Head, in.Repo = "/wt", "aaa", "bbb", baseRepo{}
	if _, err := r.ReviewBead(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	got, ok := envs["review"]
	if !ok {
		t.Fatalf("om was not run for the review; %d om call(s) recorded", len(envs))
	}
	if !slices.Equal(got, omUnsetEnv) {
		t.Fatalf("review env has %d entries, want exactly omUnsetEnv's %d", len(got), len(omUnsetEnv))
	}
	for _, e := range got {
		if strings.HasPrefix(e, "TMPDIR=") {
			t.Fatalf("a reviewer Isolated did not make gave its review an entry for TMPDIR")
		}
	}
}

// filesUnder lists every file under root, so a row can tell whether a review
// wrote anything into a directory the landing owns.
func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestVerdictReuseIsolatedLeavesTheReceiver: the receiver keeps its verdict
// directory, and a review through the copy writes its verdict file under tmp/out.
func TestVerdictReuseIsolatedLeavesTheReceiver(t *testing.T) {
	t.Parallel()
	landing, tmp := t.TempDir(), t.TempDir()
	var argv []string
	r := OMReviewer{OutDir: landing}
	r.run = fakeOM(t, isolatedApprove, 0, nil, &argv)
	var _ Isolator = r
	c := r.Isolated(tmp)
	if r.OutDir != landing {
		t.Fatalf("receiver OutDir = %q, want it unchanged at %q", r.OutDir, landing)
	}
	path, v, err := reviewIsolated(t, c, &argv, ReviewInput{})
	if err != nil {
		t.Fatal(err)
	}
	if v.Verdict != VerdictApprove {
		t.Fatalf("verdict = %q, want %q", v.Verdict, VerdictApprove)
	}
	if got, want := filepath.Dir(path), filepath.Join(tmp, "out"); got != want {
		t.Fatalf("verdict file written under %q, want %q", got, want)
	}
	if files := filesUnder(t, landing); len(files) != 0 {
		t.Fatalf("a review through the copy wrote into the receiver's directory: %v", files)
	}
}

// TestIsolatedReviewWritesNoOutputToTheLanding: the copy drops the receiver's
// output writer, so om's output of a pool review never reaches the landing's log.
func TestIsolatedReviewWritesNoOutputToTheLanding(t *testing.T) {
	t.Parallel()
	var landingLog bytes.Buffer
	var argv []string
	r := OMReviewer{OutDir: t.TempDir(), Out: &landingLog}
	r.run = fakeOM(t, isolatedApprove, 0, nil, &argv)
	if _, _, err := reviewIsolated(t, r.Isolated(t.TempDir()), &argv, ReviewInput{}); err != nil {
		t.Fatal(err)
	}
	if landingLog.Len() != 0 {
		t.Fatalf("the landing's writer received %q from a pool review", landingLog.String())
	}
}

// TestIsolatedReviewKeepsTheLandingsVerdictStore: the copy drops the receiver's
// verdict store, so a pool review of a bead writes no record into the landing's store.
func TestIsolatedReviewKeepsTheLandingsVerdictStore(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	var argv []string
	r := OMReviewer{OutDir: t.TempDir(), Verdicts: &VerdictStore{TownRoot: townRoot, Rig: "gastown"}}
	r.run = fakeOM(t, isolatedApprove, 0, nil, &argv)
	if _, _, err := reviewIsolated(t, r.Isolated(t.TempDir()), &argv, ReviewInput{BeadID: "gt-ozk4zj"}); err != nil {
		t.Fatal(err)
	}
	if files := filesUnder(t, townRoot); len(files) != 0 {
		t.Fatalf("a pool review wrote %d record(s) into the landing's verdict store: %v", len(files), files)
	}
}

// TestIsolatedEmptyTmpRefusesTheReview: an empty tmp leaves the copy with no
// verdict directory, so the review is refused before om runs.
func TestIsolatedEmptyTmpRefusesTheReview(t *testing.T) {
	t.Parallel()
	var argv []string
	r := OMReviewer{OutDir: t.TempDir()}
	r.run = fakeOM(t, isolatedApprove, 0, nil, &argv)
	c, ok := r.Isolated("").(OMReviewer)
	if !ok {
		t.Fatal("the isolated copy is not an OMReviewer")
	}
	if c.OutDir != "" {
		t.Fatalf("copy OutDir = %q for an empty tmp, want none", c.OutDir)
	}
	if _, _, err := reviewIsolated(t, c, &argv, ReviewInput{}); err == nil {
		t.Fatal("a review with no tmp succeeded, want a refusal")
	}
	if argv != nil {
		t.Fatalf("om ran for a refused review: %v", argv)
	}
}

// envTMPDIR is the directory the TMPDIR entry of env names, and whether env
// holds one. The fake om of TestVerdictReuseIsolatedReadsItsOwnRawDir reads it
// the way om reads os.TempDir(), so the row writes its raw reply where the
// isolated copy's own om would.
func envTMPDIR(env []string) (string, bool) {
	for _, e := range env {
		if dir, ok := strings.CutPrefix(e, "TMPDIR="); ok {
			return dir, true
		}
	}
	return "", false
}

// omEnvCall is one om invocation a test recorded: the argv it was given and a
// copy of the env, so a row can tell what each child saw after the run.
type omEnvCall struct {
	argv []string
	env  []string
}

// omCallName is what a recorded call asked om to do, so a row names the call it
// reads: "version" and "review --help" for the probes and "review" for the
// review itself.
func omCallName(argv []string) string {
	if slices.Contains(argv, "--help") {
		return "review --help"
	}
	return omSubcommand(argv)
}

// recordOMEnv answers om's version probe and its review, and appends every call
// with its env to calls, so a row can read what the probes and the review were
// each given.
func recordOMEnv(t *testing.T, calls *[]omEnvCall) runFunc {
	t.Helper()
	return func(_ context.Context, _ string, env, argv []string, out io.Writer) (int, error) {
		*calls = append(*calls, omEnvCall{argv: slices.Clone(argv), env: slices.Clone(env)})
		switch omCallName(argv) {
		case "version":
			_, _ = io.WriteString(out, stubVersionLine())
			return 0, nil
		case "review --help":
			_, _ = io.WriteString(out, "Usage: om review\n")
			return 0, nil
		}
		for i, a := range argv {
			if a == "--out" {
				if err := os.WriteFile(argv[i+1], []byte(isolatedApprove), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		return 0, nil
	}
}

// TestVerdictReuseIsolatedProbesGetTMPDIR: the copy's version and help probes
// run through the same withEnv-wrapped runner as its review, so they see
// TMPDIR=tmp too (gt-1h6tvh). A probe left on the landing's environment would
// keep om's raw replies in os.TempDir(), where every other review on the host
// keeps its own.
func TestVerdictReuseIsolatedProbesGetTMPDIR(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	var calls []omEnvCall
	r := OMReviewer{OutDir: t.TempDir()}
	r.run = recordOMEnv(t, &calls)
	c, ok := r.Isolated(tmp).(BeadReviewer)
	if !ok {
		t.Fatalf("%T is not a BeadReviewer", r.Isolated(tmp))
	}
	// CriteriaText makes the review ask the help probe as well as the version
	// probe, so one run covers both probes and the review.
	in := ReviewInput{BeadID: "gt-1h6tvh", CriteriaText: "- [ ] the change is wired"}
	in.Dir, in.Base, in.Head, in.Repo = "/wt", "aaa", "bbb", baseRepo{}
	if _, err := c.ReviewBead(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	want := "TMPDIR=" + tmp
	for _, name := range []string{"version", "review --help", "review"} {
		i := slices.IndexFunc(calls, func(c omEnvCall) bool { return omCallName(c.argv) == name })
		if i < 0 {
			t.Fatalf("om was not called as %q; %d call(s) recorded", name, len(calls))
		}
		if !slices.Contains(calls[i].env, want) {
			t.Fatalf("the %s call's %d env entries hold no TMPDIR for the isolated tmp", name, len(calls[i].env))
		}
	}
}

// TestVerdictReuseIsolatedOriginalHasNoTMPDIR: Isolated wraps only the copy's
// runner, so a review through the receiver after the copy is made gives its om
// and its probes exactly omUnsetEnv, with no TMPDIR entry (gt-1h6tvh). A
// landing's om that inherited a pool's TMPDIR would keep its raw replies in
// another review's directory.
func TestVerdictReuseIsolatedOriginalHasNoTMPDIR(t *testing.T) {
	t.Parallel()
	var calls []omEnvCall
	r := OMReviewer{OutDir: t.TempDir()}
	r.run = recordOMEnv(t, &calls)
	r.Isolated(t.TempDir())
	in := ReviewInput{BeadID: "gt-1h6tvh", CriteriaText: "- [ ] the change is wired"}
	in.Dir, in.Base, in.Head, in.Repo = "/wt", "aaa", "bbb", baseRepo{}
	if _, err := r.ReviewBead(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if len(calls) == 0 {
		t.Fatal("om was not run for the receiver's review")
	}
	for _, c := range calls {
		name := omCallName(c.argv)
		if !slices.Equal(c.env, omUnsetEnv) {
			t.Fatalf("the receiver's %s call has %d env entries, want exactly omUnsetEnv's %d", name, len(c.env), len(omUnsetEnv))
		}
		for _, e := range c.env {
			if strings.HasPrefix(e, "TMPDIR=") {
				t.Fatalf("the receiver's %s call was given a TMPDIR entry after Isolated made a copy", name)
			}
		}
	}
}

// omProbeFile writes an empty file in a fresh directory and returns its path: a
// stand-in for one row's installed om, so the version that row probes is cached
// under an identity its own test directory makes unique.
func omProbeFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "om")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestVerdictReuseOMVersionUsesTheProbe: OMVersion answers with the version
// ReviewBead stamps on Verdict.Version, because it asks the same cached probe,
// so a caller deciding whether a stored verdict may be reused reads the version
// that review would record (gt-jtdmp4). A probe that fails reports
// omVersionUnknown, as the verdict does.
func TestVerdictReuseOMVersionUsesTheProbe(t *testing.T) {
	t.Parallel()
	// input builds a row's ReviewInput the one way a landing review's inputs are
	// assembled, which is the construction TestReviewInputGuard allows.
	input := func() ReviewInput {
		return BuildReviewInput(&beads.Issue{ID: "gt-jtdmp4"}, nil, nil, "", "", "aaa", "bbb", "", "/wt", baseRepo{})
	}

	t.Run("the version is the one the review stamps", func(t *testing.T) {
		t.Parallel()
		var argv []string
		r := OMReviewer{OutDir: t.TempDir(), Path: omProbeFile(t)}
		r.run = fakeOM(t, isolatedApprove, 0, nil, &argv)
		in := input()
		v, err := r.ReviewBead(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if v.Version != stubOMVersion {
			t.Fatalf("the review stamped version %q, want the stub's %q", v.Version, stubOMVersion)
		}
		if got := r.OMVersion(context.Background(), in); got != v.Version {
			t.Fatalf("OMVersion = %q, want the version the review stamped, %q", got, v.Version)
		}
	})

	t.Run("a failing probe reports unknown", func(t *testing.T) {
		t.Parallel()
		r := OMReviewer{OutDir: t.TempDir(), Path: omProbeFile(t)}
		r.run = func(_ context.Context, _ string, _ []string, argv []string, _ io.Writer) (int, error) {
			if omSubcommand(argv) == "version" {
				return 1, nil
			}
			return 0, nil
		}
		if got := r.OMVersion(context.Background(), input()); got != omVersionUnknown {
			t.Fatalf("OMVersion = %q, want %q for a probe that exits with no version line", got, omVersionUnknown)
		}
	})
}

// TestVerdictReuseIsolatedReadsItsOwnRawDir: a pool review that exits without a
// verdict quotes the raw reply its own om saved under tmp, never the one the
// receiver's RawDir points at, so a failure never reports another review's reply
// (gt-hnbpyp). The fake writes its reply into the directory its TMPDIR entry
// names, as om does, so the row exercises the invocation path rather than a
// layout the test assumes.
func TestVerdictReuseIsolatedReadsItsOwnRawDir(t *testing.T) {
	t.Parallel()
	own, other := t.TempDir(), t.TempDir()
	at := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	writeRaw(t, other, "raw-stdout.txt", "another review's reply", at.Add(time.Second))
	r := OMReviewer{
		OutDir: t.TempDir(),
		RawDir: func() string { return other },
		now:    func() time.Time { return at },
	}
	r.run = func(_ context.Context, _ string, env, _ []string, out io.Writer) (int, error) {
		dir, ok := envTMPDIR(env)
		if !ok {
			t.Fatal("the isolated review was given no TMPDIR entry")
		}
		writeRaw(t, dir, "raw-stdout.txt", "this review's reply", at.Add(time.Second))
		_, _ = io.WriteString(out, "om: reviewing\n")
		return 2, nil
	}
	c, ok := r.Isolated(own).(OMReviewer)
	if !ok {
		t.Fatalf("%T is not an OMReviewer", r.Isolated(own))
	}
	_, err := reviewAt(context.Background(), c, "", t.TempDir(), "aaa", "bbb")
	if !errors.Is(err, ErrOMExecution) {
		t.Fatalf("error = %v, want it to wrap ErrOMExecution", err)
	}
	if got := err.Error(); !strings.Contains(got, ": raw reply: this review's reply") {
		t.Fatalf("error does not quote the reply under tmp: %s", got)
	}
	if got := err.Error(); strings.Contains(got, "another review's reply") {
		t.Fatalf("error quotes another review's reply: %s", got)
	}
}
