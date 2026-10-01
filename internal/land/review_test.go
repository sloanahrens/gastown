package land

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeOM answers one om invocation: it writes verdict (when non-empty) to the
// --out file and exits with code.
func fakeOM(t *testing.T, verdict string, code int, runErr error, gotArgv *[]string) runFunc {
	t.Helper()
	return func(_ context.Context, _ string, _ []string, argv []string, out io.Writer) (int, error) {
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

func TestOMReviewerApprove(t *testing.T) {
	t.Parallel()
	var argv []string
	r := OMReviewer{OutDir: t.TempDir()}
	r.run = fakeOM(t, `{"score":0.91,"verdict":"approve","findings":[{"id":"f1","severity":"minor","path":"a.go","line":3,"title":"nit"}]}`, 0, nil, &argv)
	v, err := r.Review(context.Background(), "/wt", "aaa", "bbb")
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
	v, err := r.Review(context.Background(), "/wt", "a", "b")
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
			if v, err := r.Review(context.Background(), "/wt", "a", "b"); err == nil {
				t.Fatalf("Review = %+v, nil; want an error", v)
			}
		})
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

// TestOMReviewerRetriesAStalledRun: a run that hangs past its attempt bound is
// killed and om runs again; the second run's verdict is the review (gt-b4w3y).
func TestOMReviewerRetriesAStalledRun(t *testing.T) {
	t.Parallel()
	calls := 0
	r := OMReviewer{OutDir: t.TempDir(), Timeout: 20 * time.Millisecond}
	r.run = hangThenOM(t, 1, &calls)
	v, err := r.Review(context.Background(), "/wt", "a", "b")
	if err != nil || v.Verdict != VerdictApprove {
		t.Fatalf("Review = %+v, %v; want approve after one retry", v, err)
	}
	if calls != 2 {
		t.Fatalf("om ran %d times, want 2", calls)
	}
}

// TestOMReviewerGivesUpAfterAttempts: two stalls are reported, naming both
// attempts, and om is not run a third time.
func TestOMReviewerGivesUpAfterAttempts(t *testing.T) {
	t.Parallel()
	calls := 0
	r := OMReviewer{OutDir: t.TempDir(), Timeout: 20 * time.Millisecond}
	r.run = hangThenOM(t, 3, &calls)
	_, err := r.Review(context.Background(), "/wt", "a", "b")
	if err == nil {
		t.Fatal("Review succeeded; want an error after two stalled attempts")
	}
	if calls != DefaultOMAttempts {
		t.Fatalf("om ran %d times, want %d", calls, DefaultOMAttempts)
	}
	for _, want := range []string{"attempt 1:", "attempt 2:", "deadline exceeded"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
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
		calls++
		return inner(ctx, dir, unset, av, out)
	}
	if v, err := r.Review(context.Background(), "/wt", "a", "b"); err != nil || v.Verdict != VerdictRequestChanges || calls != 1 {
		t.Fatalf("Review = %+v, %v after %d runs; want request_changes from one run", v, err, calls)
	}
}
