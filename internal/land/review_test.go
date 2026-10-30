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
	"syscall"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

// stubOMVersion is the version every reviewer stub answers `om version` with:
// a binary that names one, so a test about argv or flags is not also a test of
// the version probe.
const stubOMVersion = "v0.9.1-3-gabc1234"

// stubVersionLine is what a stub writes for `om version`.
func stubVersionLine() string { return "om version " + stubOMVersion + "\n" }

// omSubcommand is the subcommand an argv names — "review" or "version" — and
// "" for a call that names neither.
func omSubcommand(argv []string) string {
	if len(argv) > 1 {
		return argv[1]
	}
	return ""
}

// fakeOM answers one om invocation: it writes verdict (when non-empty) to the
// --out file and exits with code. The version probe is answered too, and is
// not what gotArgv keeps.
func fakeOM(t *testing.T, verdict string, code int, runErr error, gotArgv *[]string) runFunc {
	t.Helper()
	return func(_ context.Context, _ string, _ []string, argv []string, out io.Writer) (int, error) {
		if omSubcommand(argv) == "version" {
			_, _ = io.WriteString(out, stubVersionLine())
			return 0, nil
		}
		*gotArgv = argv
		if runErr != nil {
			return -1, runErr
		}
		for i, a := range argv {
			if a == "--out" && verdict != "" {
				if err := os.WriteFile(argv[i+1], []byte(verdict), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		_, _ = io.WriteString(out, "om: reviewing\n")
		return code, nil
	}
}

// baseRepo is the landing's repository for a review test: its base commit holds
// omJSON as .om.json, or no .om.json when omJSON is "", and any read fails with
// err when err is set.
type baseRepo struct {
	Repo
	omJSON string
	err    error
}

func (b baseRepo) ShowFileAtRev(_, path string) (string, error) {
	if b.err != nil {
		return "", b.err
	}
	if path != omConfigFile || b.omJSON == "" {
		return "", git.ErrNotAtRef
	}
	return b.omJSON, nil
}

// reviewAt runs one om review of dir from base to head through reviewOnce, with
// a base repository whose .om.json is omJSON. Review carries no repository and
// fails closed, so the tests that exercise om's verdict go through here.
func reviewAt(ctx context.Context, r OMReviewer, omJSON, dir, base, head string) (Verdict, error) {
	return r.reviewOnce(ctx, ReviewInput{Dir: dir, Base: base, Head: head, Repo: baseRepo{omJSON: omJSON}})
}

func TestOMReviewerApprove(t *testing.T) {
	t.Parallel()
	var argv []string
	r := OMReviewer{OutDir: t.TempDir()}
	r.run = fakeOM(t, `{"score":0.91,"verdict":"approve","findings":[{"id":"f1","severity":"minor","path":"a.go","line":3,"title":"nit"}]}`, 0, nil, &argv)
	v, err := reviewAt(context.Background(), r, "", "/wt", "aaa", "bbb")
	if err != nil {
		t.Fatal(err)
	}
	if v.Verdict != VerdictApprove || v.Score != 0.91 || len(v.Findings) != 1 || v.Findings[0].Path != "a.go" {
		t.Fatalf("verdict = %+v", v)
	}
	got := strings.Join(argv[:len(argv)-1], " ")
	if got != "om review -C /wt --base aaa --head bbb --out" {
		t.Fatalf("argv = %q", argv)
	}
}

func TestOMReviewerRequestChanges(t *testing.T) {
	t.Parallel()
	var argv []string
	r := OMReviewer{Path: "/bin/om", OutDir: t.TempDir()}
	r.run = fakeOM(t, `{"score":0.4,"verdict":"request_changes","findings":[{"id":"f2","severity":"major","title":"bug"}]}`, 1, nil, &argv)
	v, err := reviewAt(context.Background(), r, "", "/wt", "a", "b")
	if err != nil || v.Verdict != VerdictRequestChanges || argv[0] != "/bin/om" {
		t.Fatalf("verdict = %+v, %v (argv %q)", v, err, argv)
	}
}

func TestOMReviewerFailsClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		verdict string
		code    int
		runErr  error
	}{
		{"execution error", `{"verdict":"approve"}`, 2, nil},
		{"binary missing", "", -1, errors.New("exec: om: not found")},
		{"no verdict file", "", 0, nil},
		{"malformed json", `{"verdict":`, 0, nil},
		{"unknown verdict", `{"verdict":"maybe"}`, 0, nil},
		{"exit disagrees with verdict", `{"verdict":"request_changes"}`, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var argv []string
			r := OMReviewer{OutDir: t.TempDir()}
			r.run = fakeOM(t, tc.verdict, tc.code, tc.runErr, &argv)
			if v, err := reviewAt(context.Background(), r, "", "/wt", "a", "b"); err == nil {
				t.Fatalf("Review = %+v, nil; want an error", v)
			}
		})
	}
}

// TestOMReviewerMarksAnExecutionError: exit 2 is the one om failure Land reads
// by type, to report the merged tree's size alongside it (gt-hhid7).
func TestOMReviewerMarksAnExecutionError(t *testing.T) {
	t.Parallel()
	var argv []string
	r := OMReviewer{OutDir: t.TempDir()}
	r.run = fakeOM(t, "", 2, nil, &argv)
	_, err := reviewAt(context.Background(), r, "", "/wt", "a", "b")
	if !errors.Is(err, ErrOMExecution) {
		t.Fatalf("Review error = %v, want it to wrap ErrOMExecution", err)
	}
}

// hangThenOM blocks its first `hangs` invocations until their context ends
// (a provider that accepts the request and never answers), then answers like
// fakeOM with an approve.
func hangThenOM(t *testing.T, hangs int, calls *int) runFunc {
	t.Helper()
	var argv []string
	approve := fakeOM(t, `{"score":0.9,"verdict":"approve"}`, 0, nil, &argv)
	return func(ctx context.Context, dir string, unset []string, av []string, out io.Writer) (int, error) {
		*calls++
		if *calls <= hangs {
			<-ctx.Done()
			return -1, ctx.Err()
		}
		return approve(ctx, dir, unset, av, out)
	}
}

// TestOMReviewerRunsOnce: om is never retried. A run that hangs past its
// timeout is killed and reported as ErrOMTimeout; a run that fails fast is
// reported too; either way om ran once (gt-is0ep).
func TestOMReviewerRunsOnce(t *testing.T) {
	t.Parallel()
	calls := 0
	r := OMReviewer{OutDir: t.TempDir(), Timeout: 20 * time.Millisecond}
	r.run = hangThenOM(t, 1, &calls)
	_, err := reviewAt(context.Background(), r, "", "/wt", "a", "b")
	if !errors.Is(err, ErrOMTimeout) {
		t.Fatalf("Review error = %v, want ErrOMTimeout", err)
	}
	if calls != 1 {
		t.Fatalf("om ran %d times after a timeout, want 1", calls)
	}

	calls = 0
	r.run = func(context.Context, string, []string, []string, io.Writer) (int, error) {
		calls++
		return 2, nil
	}
	if _, err := reviewAt(context.Background(), r, "", "/wt", "a", "b"); err == nil || errors.Is(err, ErrOMTimeout) {
		t.Fatalf("Review error = %v, want an execution error", err)
	}
	if calls != 1 {
		t.Fatalf("om ran %d times after an execution error, want 1", calls)
	}
}

// TestOMReviewerDoesNotRetryAVerdict: request_changes is an answer, not a
// failure; om runs once.
func TestOMReviewerDoesNotRetryAVerdict(t *testing.T) {
	t.Parallel()
	calls := 0
	var argv []string
	inner := fakeOM(t, `{"score":0.3,"verdict":"request_changes"}`, 1, nil, &argv)
	r := OMReviewer{OutDir: t.TempDir()}
	r.run = func(ctx context.Context, dir string, unset []string, av []string, out io.Writer) (int, error) {
		if omSubcommand(av) != "version" {
			calls++
		}
		return inner(ctx, dir, unset, av, out)
	}
	if v, err := reviewAt(context.Background(), r, "", "/wt", "a", "b"); err != nil || v.Verdict != VerdictRequestChanges || calls != 1 {
		t.Fatalf("Review = %+v, %v after %d runs; want request_changes from one run", v, err, calls)
	}
}

// TestOMReviewerRecordsTheVersionOnTheVerdict: the verdict names the om behind
// it, read from the binary rather than from om's verdict file (gt-uebmq).
func TestOMReviewerRecordsTheVersionOnTheVerdict(t *testing.T) {
	t.Parallel()
	stub := &omStub{t: t, help: omHelpWithBead, bead: "gt-abc"}
	v, err := omStubbed(t, omBinary(t), stub).ReviewBead(context.Background(),
		ReviewInput{Repo: baseRepo{}, Dir: t.TempDir(), Base: "aaa", Head: "bbb", BeadID: "gt-abc"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Version != stubOMVersion {
		t.Errorf("version = %q, want %q", v.Version, stubOMVersion)
	}
	if stub.versions != 1 {
		t.Errorf("version probes = %d, want 1", stub.versions)
	}
}

// TestOMReviewerRecordsTheVersionOfAReviewThatFailed: a review that produced no
// verdict is exactly when the binary is worth naming, so the version is read
// either way (gt-uebmq).
func TestOMReviewerRecordsTheVersionOfAReviewThatFailed(t *testing.T) {
	t.Parallel()
	stub := &criteriaStub{t: t, help: omHelpWithCriteria, fail: errors.New("exec: om: not found")}
	v, err := criteriaStubbed(t, omBinary(t), stub).ReviewBead(context.Background(),
		ReviewInput{Repo: baseRepo{}, Dir: t.TempDir(), Base: "aaa", Head: "bbb", BeadID: "gt-abc"})
	if err == nil {
		t.Fatal("a failed review returned no error")
	}
	if v.Version != stubOMVersion {
		t.Errorf("version = %q, want %q", v.Version, stubOMVersion)
	}
}

// TestOMReviewerProbesTheVersionOncePerBinary: the version is a property of the
// installed binary, so later verdicts in this process read the cache the help
// probe shares (gt-uebmq).
func TestOMReviewerProbesTheVersionOncePerBinary(t *testing.T) {
	t.Parallel()
	stub := &omStub{t: t, help: omHelpWithBead, bead: "gt-abc"}
	r := omStubbed(t, omBinary(t), stub)
	for i := 0; i < 3; i++ {
		if _, err := r.ReviewBead(context.Background(),
			ReviewInput{Repo: baseRepo{}, Dir: t.TempDir(), Base: "aaa", Head: "bbb", BeadID: "gt-abc"}); err != nil {
			t.Fatal(err)
		}
	}
	if stub.versions != 1 {
		t.Errorf("version probes = %d, want 1: an installed om keeps its version", stub.versions)
	}
}

// TestOMReviewerRestartsAnExecTheKernelCalledBusy: a start the kernel refuses
// with ETXTBSY is a moment, not a verdict, so the om exec is started again and
// an om that was just written -- the fake om of the contract tests, an om a
// deploy renamed into place -- still names its version (gt-0pt32).
func TestOMReviewerRestartsAnExecTheKernelCalledBusy(t *testing.T) {
	t.Parallel()
	var calls int
	r := OMReviewer{Path: filepath.Join(t.TempDir(), "om"), OutDir: t.TempDir()}
	r.run = func(_ context.Context, _ string, _ []string, argv []string, out io.Writer) (int, error) {
		calls++
		if calls == 1 {
			return -1, fmt.Errorf("fork/exec %s: %w", argv[0], syscall.ETXTBSY)
		}
		_, _ = io.WriteString(out, stubVersionLine())
		return 0, nil
	}
	if got := r.omVersion(context.Background(), ReviewInput{Repo: baseRepo{}, Dir: t.TempDir()}); got != stubOMVersion {
		t.Fatalf("version = %q, want %q after one busy start", got, stubOMVersion)
	}
	if calls != 2 {
		t.Fatalf("om was started %d times, want 2: one refused as busy, one that answered", calls)
	}
}

// TestOMReviewerBoundsTheRestarts: an om that stays busy is still a failure,
// with the reason in the warning, rather than a review held open (gt-0pt32).
func TestOMReviewerBoundsTheRestarts(t *testing.T) {
	t.Parallel()
	var calls int
	r := OMReviewer{Path: filepath.Join(t.TempDir(), "om"), OutDir: t.TempDir()}
	r.run = func(context.Context, string, []string, []string, io.Writer) (int, error) {
		calls++
		return -1, fmt.Errorf("fork/exec: %w", syscall.ETXTBSY)
	}
	var log strings.Builder
	got := r.omVersion(context.Background(), ReviewInput{Repo: baseRepo{}, Dir: t.TempDir(), Logf: logfTo(&log)})
	if got != omVersionUnknown {
		t.Fatalf("version = %q, want %q: a busy om has none to report", got, omVersionUnknown)
	}
	if calls != omBusyAttempts {
		t.Fatalf("om was started %d times, want the bound %d", calls, omBusyAttempts)
	}
	if !strings.Contains(log.String(), "text file busy") {
		t.Errorf("log = %q, want the reason the probe read no version", log.String())
	}
}

// TestOMReviewerRestartsNothingElse: only a busy executable earns a second
// start; any other run error is the answer the first attempt already gave
// (gt-0pt32).
func TestOMReviewerRestartsNothingElse(t *testing.T) {
	t.Parallel()
	var calls int
	r := OMReviewer{Path: filepath.Join(t.TempDir(), "om"), OutDir: t.TempDir()}
	r.run = func(context.Context, string, []string, []string, io.Writer) (int, error) {
		calls++
		return -1, fmt.Errorf("fork/exec: %w", syscall.ENOENT)
	}
	if got := r.omVersion(context.Background(), ReviewInput{Repo: baseRepo{}, Dir: t.TempDir()}); got != omVersionUnknown {
		t.Fatalf("version = %q, want %q", got, omVersionUnknown)
	}
	if calls != 1 {
		t.Fatalf("om was started %d times, want 1: only a busy executable is worth a second start", calls)
	}
}

// TestOMReviewerRecordsAnUnreadableVersionAsUnknown: an om that names no
// version is still a reviewer, and the verdict says so (gt-uebmq).
func TestOMReviewerRecordsAnUnreadableVersionAsUnknown(t *testing.T) {
	t.Parallel()
	stub := &omStub{t: t, help: omHelpWithBead, bead: "gt-abc", noVersion: true}
	var log strings.Builder
	v, err := omStubbed(t, omBinary(t), stub).ReviewBead(context.Background(),
		ReviewInput{Repo: baseRepo{}, Dir: t.TempDir(), Base: "aaa", Head: "bbb", BeadID: "gt-abc", Logf: logfTo(&log)})
	if err != nil {
		t.Fatalf("ReviewBead: %v; a version-less om is still a reviewer", err)
	}
	if v.Version != omVersionUnknown {
		t.Errorf("version = %q, want %q", v.Version, omVersionUnknown)
	}
	if !strings.Contains(log.String(), "could not read the version") {
		t.Errorf("log = %q, want the unreadable version named", log.String())
	}
}

// TestOMReviewerRecordsTheVersionOfTheOMThatRanTheReview: an install that swaps
// the om binary while a review runs must not rename the reviewer, so the
// verdict carries the version read before the review, not the one the path
// answers after it (gt-f3da3).
func TestOMReviewerRecordsTheVersionOfTheOMThatRanTheReview(t *testing.T) {
	t.Parallel()
	bin := omBinary(t)
	const swappedVersion = "v1.0.0-1-gfeed456"
	current := stubOMVersion
	stub := &omStub{t: t, help: omHelpWithBead, bead: "gt-abc"}
	r := omStubbed(t, bin, stub)
	r.run = func(ctx context.Context, dir string, env []string, argv []string, out io.Writer) (int, error) {
		if omSubcommand(argv) == "version" {
			_, _ = io.WriteString(out, "om version "+current+"\n")
			return 0, nil
		}
		code, err := stub.run(ctx, dir, env, argv, out)
		if omSubcommand(argv) == "review" && !slices.Contains(argv, "--help") {
			// The om installer renames the new build over the path mid-review.
			current = swappedVersion
			if werr := os.WriteFile(bin, []byte("a newer, larger om binary\n"), 0o600); werr != nil {
				t.Error(werr)
			}
		}
		return code, err
	}
	v, err := r.ReviewBead(context.Background(),
		ReviewInput{Repo: baseRepo{}, Dir: t.TempDir(), Base: "aaa", Head: "bbb", BeadID: "gt-abc"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Version != stubOMVersion {
		t.Errorf("version = %q, want %q: the om that ran the review, not the one installed after it", v.Version, stubOMVersion)
	}
}

// TestParseOMVersion: the version is the sha on `om version`'s own line, and
// an output without one names no version.
func TestParseOMVersion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		out  string
		want string
	}{
		{"the version line", "om version 9f8e7d6\n", "9f8e7d6"},
		{"a line after other output", "warning: no .om.json\nom version 9f8e7d6\n", "9f8e7d6"},
		{"a version line with none", "om version\n", ""},
		{"another tool's version", "om 0.2.0\n", ""},
		{"no output", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := parseOMVersion(tc.out); got != tc.want {
				t.Errorf("parseOMVersion(%q) = %q, want %q", tc.out, got, tc.want)
			}
		})
	}
}
