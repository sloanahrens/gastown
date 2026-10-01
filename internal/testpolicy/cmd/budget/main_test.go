package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/steveyegge/gastown/internal/testpolicy"
)

// TestSummaryLines_KeepsOnlyPackageSummaries feeds summaryLines go test text
// in pieces that split lines, as a pipe delivers it, and checks that only the
// "ok" and "FAIL\t<pkg>" lines the wall check reads come out.
func TestSummaryLines_KeepsOnlyPackageSummaries(t *testing.T) {
	t.Parallel()
	var got strings.Builder
	s := &summaryLines{w: &got}
	text := "--- FAIL: TestX (1.00s)\n    x_test.go:1: ok then\nFAIL\nFAIL\tgithub.com/steveyegge/gastown/internal/a\t2.1s\nok  \tgithub.com/steveyegge/gastown/internal/b\t16.0s\n?   \tgithub.com/steveyegge/gastown/internal/c\t[no test files]\n"
	for i := 0; i < len(text); i += 7 {
		end := min(i+7, len(text))
		if _, err := s.Write([]byte(text[i:end])); err != nil {
			t.Fatal(err)
		}
	}
	want := "FAIL\tgithub.com/steveyegge/gastown/internal/a\t2.1s\nok  \tgithub.com/steveyegge/gastown/internal/b\t16.0s\n"
	if got.String() != want {
		t.Fatalf("summaryLines kept %q, want %q", got.String(), want)
	}
}

// TestSummaryLines_FlushKeepsLastLineWithoutNewline covers a summary line at
// the end of the stream with no trailing newline: Flush must pass it on.
func TestSummaryLines_FlushKeepsLastLineWithoutNewline(t *testing.T) {
	t.Parallel()
	var got strings.Builder
	s := &summaryLines{w: &got}
	if _, err := s.Write([]byte("noise\nok  \tgithub.com/steveyegge/gastown/internal/b\t1.5s")); err != nil {
		t.Fatal(err)
	}
	if got.String() != "" {
		t.Fatalf("summaryLines emitted %q before Flush, want nothing", got.String())
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if want := "ok  \tgithub.com/steveyegge/gastown/internal/b\t1.5s\n"; got.String() != want {
		t.Fatalf("after Flush summaryLines kept %q, want %q", got.String(), want)
	}
}

// TestSummaryLines_CapsPartialLine checks that an over-long line without a
// newline is dropped instead of growing the buffer, and that the next line
// is read normally.
func TestSummaryLines_CapsPartialLine(t *testing.T) {
	t.Parallel()
	var got strings.Builder
	s := &summaryLines{w: &got}
	long := []byte("ok " + strings.Repeat("x", 1024))
	for i := 0; i < 2*maxSummaryLine/len(long)+2; i++ {
		if _, err := s.Write(long); err != nil {
			t.Fatal(err)
		}
		if len(s.partial) > maxSummaryLine {
			t.Fatalf("partial line grew to %d bytes, over the %d cap", len(s.partial), maxSummaryLine)
		}
	}
	if _, err := s.Write([]byte("\nok  \tgithub.com/steveyegge/gastown/internal/c\t2s\n")); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if want := "ok  \tgithub.com/steveyegge/gastown/internal/c\t2s\n"; got.String() != want {
		t.Fatalf("summaryLines kept %q, want only %q", got.String(), want)
	}
}

// TestCheckFastTier_FailsClosedOnEmptyProbe: packages ran but the probe saw
// no summary line, so the check must fail rather than read silence as fast.
func TestCheckFastTier_FailsClosedOnEmptyProbe(t *testing.T) {
	t.Parallel()
	var summary bytes.Buffer
	if code := checkFastTier(&summaryLines{w: &summary}, &summary, 3, false); code == 0 {
		t.Fatal("checkFastTier passed a run of 3 packages whose output had no summary line; want it to fail closed")
	}
}

// TestCheckFastTier_FailsClosedOnUnreadableTime: a summary line whose time
// cannot be parsed fails the check.
func TestCheckFastTier_FailsClosedOnUnreadableTime(t *testing.T) {
	t.Parallel()
	var summary bytes.Buffer
	probe := &summaryLines{w: &summary}
	_, _ = probe.Write([]byte("ok  \tgithub.com/steveyegge/gastown/internal/a\t1.0s\nok  \tgithub.com/steveyegge/gastown/internal/b\tsoon\n"))
	if code := checkFastTier(probe, &summary, 2, false); code == 0 {
		t.Fatal("checkFastTier passed a summary line with an unreadable time; want it to fail closed")
	}
}

// TestCheckFastTier_Verdicts: fast packages pass, an all-cached run passes,
// and a package over the limit passes by default (make gate: wall time depends
// on host load, and a landing must not be refused for contention, gt-z7qtk)
// but fails under strictWall (make tier-check).
func TestCheckFastTier_Verdicts(t *testing.T) {
	t.Parallel()
	over := (testpolicy.FastTierMaxWall + time.Second).String()
	for _, tc := range []struct {
		name, text string
		strict     bool
		want       int
	}{
		{"fast", "ok  \tgithub.com/steveyegge/gastown/internal/a\t1.0s\n", false, 0},
		{"all cached", "ok  \tgithub.com/steveyegge/gastown/internal/a\t(cached)\n", false, 0},
		{"wall overrun does not fail the gate", "ok  \tgithub.com/steveyegge/gastown/internal/a\t" + over + "\n", false, 0},
		{"wall overrun fails tier-check", "ok  \tgithub.com/steveyegge/gastown/internal/a\t" + over + "\n", true, 1},
		{"fast passes tier-check", "ok  \tgithub.com/steveyegge/gastown/internal/a\t1.0s\n", true, 0},
	} {
		var summary bytes.Buffer
		probe := &summaryLines{w: &summary}
		_, _ = probe.Write([]byte(tc.text))
		if got := checkFastTier(probe, &summary, 1, tc.strict); got != tc.want {
			t.Errorf("%s: checkFastTier = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestReportOver checks that a CPU overrun fails only an enforced run, says
// so when it is reported only, and that a missing measurement always fails.
func TestReportOver(t *testing.T) {
	t.Parallel()
	cpu := testpolicy.Overrun{Package: "internal/a", CPU: testpolicy.CPUTime{User: 11 * time.Second}}
	unmeasured := testpolicy.Overrun{Package: "internal/b", Unmeasured: true}
	for _, tc := range []struct {
		name    string
		over    []testpolicy.Overrun
		enforce bool
		fails   bool
		line    string
	}{
		{"enforced", []testpolicy.Overrun{cpu}, true, true, "BUDGET: internal/a used 11s user CPU"},
		{"loaded host", []testpolicy.Overrun{cpu}, false, false, "BUDGET (reported only, load 30.0 >= 16 CPUs; GATE_STRICT_BUDGET=1 enforces): internal/a"},
		{"unmeasured on loaded host", []testpolicy.Overrun{unmeasured}, false, true, "BUDGET: internal/b passed but its CPU time was not recorded"},
	} {
		var out bytes.Buffer
		if got := reportOver(&out, tc.over, 10*time.Second, tc.enforce, "load 30.0 >= 16 CPUs"); got != tc.fails {
			t.Errorf("%s: reportOver = %v, want %v", tc.name, got, tc.fails)
		}
		if !strings.Contains(out.String(), tc.line) {
			t.Errorf("%s: output %q, want it to contain %q", tc.name, out.String(), tc.line)
		}
	}
}

// fakeHalf is a half that writes fixed output and returns a fixed result,
// after waiting for wait (when set) to close; cancelling ctx ends the wait
// with code -1. A test whose halves never unblock deadlocks, which
// synctest.Test reports as a failure instead of hanging.
func fakeHalf(stdout, stderr string, code int, err error, wait <-chan struct{}) half {
	return func(ctx context.Context, out, errOut io.Writer) (int, error) {
		if wait != nil {
			select {
			case <-wait:
			case <-ctx.Done():
				return -1, nil
			}
		}
		_, _ = io.WriteString(out, stdout)
		_, _ = io.WriteString(errOut, stderr)
		return code, err
	}
}

// TestRunHalves_RunsBothAtOnce checks the halves overlap: each waits for the
// other to start, so running them one after the other, in either order,
// deadlocks.
func TestRunHalves_RunsBothAtOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		judgedStarted, cachedStarted := make(chan struct{}), make(chan struct{})
		meet := func(mine, theirs chan struct{}) half {
			return func(ctx context.Context, out, errOut io.Writer) (int, error) {
				close(mine)
				<-theirs
				return 0, nil
			}
		}
		var out, errOut bytes.Buffer
		code, err := runHalves(meet(judgedStarted, cachedStarted), meet(cachedStarted, judgedStarted), &out, &errOut)
		if err != nil || code != 0 {
			t.Fatalf("runHalves = %d, %v; want 0, nil", code, err)
		}
	})
}

// TestRunHalves_CachedOutputComesAfterJudged checks the cached half's output,
// though it finishes first, is written whole after the judged half's.
func TestRunHalves_CachedOutputComesAfterJudged(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		cachedDone := make(chan struct{})
		cached := func(ctx context.Context, out, errOut io.Writer) (int, error) {
			defer close(cachedDone)
			_, _ = io.WriteString(out, "ok cached a\nok cached b\n")
			_, _ = io.WriteString(errOut, "cached took\n")
			return 0, nil
		}
		var out, errOut bytes.Buffer
		code, err := runHalves(fakeHalf("ok judged\n", "judged took\n", 0, nil, cachedDone), cached, &out, &errOut)
		if err != nil || code != 0 {
			t.Fatalf("runHalves = %d, %v; want 0, nil", code, err)
		}
		if want := "ok judged\nok cached a\nok cached b\n"; out.String() != want {
			t.Errorf("stdout = %q, want %q", out.String(), want)
		}
		if want := "judged took\ncached took\n"; errOut.String() != want {
			t.Errorf("stderr = %q, want %q", errOut.String(), want)
		}
	})
}

// TestRunHalves_ExitCode checks either half failing fails the run, the
// judged half's code winning when both fail, and a missing half.
func TestRunHalves_ExitCode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		judged, cached half
		want           int
	}{
		{"both pass", fakeHalf("", "", 0, nil, nil), fakeHalf("", "", 0, nil, nil), 0},
		{"judged fails", fakeHalf("", "", 1, nil, nil), fakeHalf("", "", 0, nil, nil), 1},
		{"cached fails", fakeHalf("", "", 0, nil, nil), fakeHalf("", "", 1, nil, nil), 1},
		{"both fail, judged code wins", fakeHalf("", "", 2, nil, nil), fakeHalf("", "", 1, nil, nil), 2},
		{"judged signalled", fakeHalf("", "", -1, nil, nil), fakeHalf("", "", 1, nil, nil), -1},
		{"no cached half", fakeHalf("", "", 1, nil, nil), nil, 1},
		{"no judged half", nil, fakeHalf("", "", 1, nil, nil), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			code, err := runHalves(tc.judged, tc.cached, &out, &errOut)
			if err != nil || code != tc.want {
				t.Fatalf("runHalves = %d, %v; want %d, nil", code, err, tc.want)
			}
		})
	}
}

// TestRunHalves_JudgedErrorCancelsCached checks a runner failure in the
// judged half stops the cached half (it would wait forever otherwise) and is
// returned.
func TestRunHalves_JudgedErrorCancelsCached(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		never := make(chan struct{})
		var out, errOut bytes.Buffer
		_, err := runHalves(fakeHalf("", "", 0, errors.New("pipe broke"), nil), fakeHalf("", "", 0, nil, never), &out, &errOut)
		if err == nil || !strings.Contains(err.Error(), "pipe broke") {
			t.Fatalf("runHalves error = %v, want the judged half's error alone", err)
		}
	})
}

// TestRunHalves_CachedErrorReturned checks a runner failure in the cached
// half fails the run even when every package passed.
func TestRunHalves_CachedErrorReturned(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	_, err := runHalves(fakeHalf("", "", 0, nil, nil), fakeHalf("", "", 0, errors.New("go not found"), nil), &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "go not found") {
		t.Fatalf("runHalves error = %v, want the cached half's error", err)
	}
}

func TestParallelism(t *testing.T) {
	t.Parallel()
	def := runtime.GOMAXPROCS(0)
	cases := []struct {
		before, after []string
		want          int
	}{
		{nil, nil, def},
		{[]string{"-timeout", "20m"}, nil, def},
		{[]string{"-p", "4"}, nil, 4},
		{[]string{"-p=6"}, nil, 6},
		{[]string{"-p", "4"}, []string{"--p", "3"}, 3},
		{[]string{"-p", "x"}, nil, def},
		{[]string{"-parallel", "2"}, nil, def},
	}
	for _, tc := range cases {
		if got := parallelism(tc.before, tc.after); got != tc.want {
			t.Errorf("parallelism(%q, %q) = %d, want %d", tc.before, tc.after, got, tc.want)
		}
	}
}

func TestSplitParallelism(t *testing.T) {
	t.Parallel()
	cases := []struct{ procs, cached, judgedP, cachedP int }{
		{24, 2, 22, 2},
		{4, 2, 2, 2},
		{3, 2, 2, 1},
		{2, 2, 1, 1},
		{1, 2, 1, 1},
		{24, 30, 12, 12},
	}
	for _, tc := range cases {
		j, c := splitParallelism(tc.procs, tc.cached)
		if j != tc.judgedP || c != tc.cachedP {
			t.Errorf("splitParallelism(%d, %d) = %d, %d; want %d, %d", tc.procs, tc.cached, j, c, tc.judgedP, tc.cachedP)
		}
	}
}
