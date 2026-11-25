package land

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
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
