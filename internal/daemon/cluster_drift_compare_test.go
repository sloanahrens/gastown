package daemon

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeDriftStart is an injected start that records its one call and answers
// with fixed output, so the compare is tested with no process (gt-jokegc).
type fakeDriftStart struct {
	stdout []byte
	stderr []byte
	exit   int
	runErr error

	called bool
	ctx    context.Context
	exe    string
	args   []string
	limit  int
}

func (f *fakeDriftStart) start(ctx context.Context, exe string, args []string, limit int) ([]byte, []byte, int, error) {
	f.called, f.ctx, f.exe, f.args, f.limit = true, ctx, exe, args, limit
	return f.stdout, f.stderr, f.exit, f.runErr
}

// TestClusterDriftCompareCallsStartWithTheResolvedBinary: the compare runs the
// binary exe resolved, with the args town cluster drift and a 64 KiB limit,
// passes the caller's context through, and returns the three lines of an exit 1
// answer (gt-jokegc).
func TestClusterDriftCompareCallsStartWithTheResolvedBinary(t *testing.T) {
	t.Parallel()
	three := []string{
		"deployment/gt-daemon: spec.replicas fingerprint a1b2 != c3d4",
		"deployment/gt-daemon: metadata.labels fingerprint a1b2 != e5f6",
		"cronjob/tracker-backup: spec.schedule fingerprint a1b2 != g7h8",
	}
	fake := &fakeDriftStart{stdout: []byte(strings.Join(three, "\n") + "\n"), exit: 1}
	exe := func() (string, error) { return "/opt/gt/bin/gt", nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got, err := newClusterDriftCompare(exe, fake.start)(ctx)

	if err != nil || !reflect.DeepEqual(got.Differing, three) {
		t.Fatalf("compare = %+v, %v; want the three lines and no error", got, err)
	}
	if fake.exe != "/opt/gt/bin/gt" || !reflect.DeepEqual(fake.args, []string{"town", "cluster", "drift"}) || fake.limit != 64<<10 {
		t.Errorf("start(%q, %q, %d), want the resolved binary, town cluster drift and 64 KiB", fake.exe, fake.args, fake.limit)
	}
	cancel()
	if fake.ctx.Err() != context.Canceled {
		t.Error("start did not receive a context derived from the caller's")
	}
}

// TestClusterDriftCompareBoundsStartWithDeadline: the context start receives
// carries a deadline clusterDriftRunBound after the compare began, read from the
// test's own clock on either side of the call, with no sleep (gt-ime6b8).
func TestClusterDriftCompareBoundsStartWithDeadline(t *testing.T) {
	t.Parallel()
	fake := &fakeDriftStart{exit: 0}
	exe := func() (string, error) { return "/opt/gt/bin/gt", nil }

	before := time.Now()
	_, err := newClusterDriftCompare(exe, fake.start)(context.Background())
	after := time.Now()

	deadline, ok := fake.ctx.Deadline()
	if err != nil || !fake.called || !ok {
		t.Fatalf("compare err = %v, called = %t, deadline set = %t; want a run with a deadline", err, fake.called, ok)
	}
	if lo, hi := before.Add(60*time.Second), after.Add(60*time.Second); deadline.Before(lo) || deadline.After(hi) {
		t.Errorf("deadline = %v, want between %v and %v (60 s after the compare began)", deadline, lo, hi)
	}
}

// TestClusterDriftCompareOverrunEndsAsError: a start that returns the context's
// deadline error makes the compare an error with no result, the overrun being
// unreadable and never a clear. The caller's context is already past its
// deadline, so the context that start receives reports DeadlineExceeded with no
// sleep (gt-ime6b8).
func TestClusterDriftCompareOverrunEndsAsError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancel()
	called := false
	start := func(runCtx context.Context, exe string, args []string, limit int) ([]byte, []byte, int, error) {
		called = true
		return nil, nil, -1, runCtx.Err()
	}
	exe := func() (string, error) { return "/opt/gt/bin/gt", nil }

	got, err := newClusterDriftCompare(exe, start)(ctx)

	if !called {
		t.Fatal("start was not called")
	}
	if !errors.Is(err, context.DeadlineExceeded) || len(got.Differing) != 0 {
		t.Errorf("compare = %+v, %v; want no result and an error wrapping DeadlineExceeded", got, err)
	}
}

// TestClusterDriftCompareRefusals: a failed binary lookup never starts the
// child; a child that did not run or exited with no answer is an error with no
// result; exit 0 is in step with no error (gt-jokegc).
func TestClusterDriftCompareRefusals(t *testing.T) {
	t.Parallel()
	resolve := func() (string, error) { return "/opt/gt/bin/gt", nil }
	tests := []struct {
		name      string
		exe       func() (string, error)
		fake      fakeDriftStart
		wantStart bool
		wantErr   bool
	}{
		{name: "lookup fails, child never starts", exe: func() (string, error) { return "", errors.New("no $PATH") }, wantErr: true},
		{name: "child did not run", exe: resolve, fake: fakeDriftStart{exit: -1, runErr: errors.New("fork/exec: no such file")}, wantStart: true, wantErr: true},
		{name: "exit 3 is no answer", exe: resolve, fake: fakeDriftStart{exit: 3, stdout: []byte("x\n")}, wantStart: true, wantErr: true},
		{name: "exit 0 is in step", exe: resolve, fake: fakeDriftStart{exit: 0}, wantStart: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := tt.fake
			got, err := newClusterDriftCompare(tt.exe, fake.start)(context.Background())
			if fake.called != tt.wantStart {
				t.Errorf("start called = %t, want %t", fake.called, tt.wantStart)
			}
			if (err != nil) != tt.wantErr || len(got.Differing) != 0 {
				t.Errorf("compare = %+v, %v; want an error = %t with no result", got, err, tt.wantErr)
			}
		})
	}
}

// TestClusterDriftCompareNamesTheMissingReadGrant: an exit 2 answer whose stderr
// says the daemon was forbidden the cluster, or a binary the run needs was not
// found, is an error with no result that names the missing drift read grant
// and keeps the command's first stderr line (gt-udasjo).
func TestClusterDriftCompareNamesTheMissingReadGrant(t *testing.T) {
	t.Parallel()
	const cannotSay = "gt town cluster drift exited 2, which cannot say whether the cluster is in step: "
	const grant = "the daemon cannot read the control cluster yet (the drift read grant is not applied): "
	tests := []struct {
		name   string
		stderr string
		want   string
	}{
		{name: "forbidden", stderr: "forbidden: User \"system:serviceaccount:gt:daemon\" cannot list pods", want: grant + cannotSay + "forbidden: User \"system:serviceaccount:gt:daemon\" cannot list pods"},
		{name: "kubectl capitalised Forbidden", stderr: "Error from server (Forbidden): pods is forbidden", want: grant + cannotSay + "Error from server (Forbidden): pods is forbidden"},
		{name: "executable file not found", stderr: "exec: \"kubectl\": executable file not found in $PATH", want: grant + cannotSay + "exec: \"kubectl\": executable file not found in $PATH"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeDriftStart{stderr: []byte(tt.stderr + "\n"), exit: 2}
			exe := func() (string, error) { return "/opt/gt/bin/gt", nil }

			got, err := newClusterDriftCompare(exe, fake.start)(context.Background())

			if err == nil || err.Error() != tt.want {
				t.Fatalf("compare error = %v, want %q", err, tt.want)
			}
			if len(got.Differing) != 0 {
				t.Errorf("compare result = %+v, want no result beside the error", got)
			}
		})
	}
}

// TestClusterDriftCompareOtherErrorsPassThrough: an exit 2 answer with no grant
// words keeps the parse's own text, and the recorded-flags answer is still
// errNoRecordedRender, not named as a grant problem (gt-udasjo).
func TestClusterDriftCompareOtherErrorsPassThrough(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		stderr     string
		wantText   string
		wantRecord bool
	}{
		{name: "unrelated exit 2 message", stderr: "disk full while writing the report", wantText: "gt town cluster drift exited 2, which cannot say whether the cluster is in step: disk full while writing the report"},
		{name: "recorded-flags answer", stderr: "no live render has recorded its flags", wantText: "gt town cluster drift exited 2, which cannot say whether the cluster is in step: no live render has recorded its flags: " + errNoRecordedRender.Error(), wantRecord: true},
		{name: "recorded-flags answer whose stderr also says forbidden", stderr: "forbidden: no live render has recorded its flags", wantText: "gt town cluster drift exited 2, which cannot say whether the cluster is in step: forbidden: no live render has recorded its flags: " + errNoRecordedRender.Error(), wantRecord: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeDriftStart{stderr: []byte(tt.stderr + "\n"), exit: 2}
			exe := func() (string, error) { return "/opt/gt/bin/gt", nil }

			got, err := newClusterDriftCompare(exe, fake.start)(context.Background())

			if err == nil || err.Error() != tt.wantText {
				t.Fatalf("compare error = %v, want %q", err, tt.wantText)
			}
			if errors.Is(err, errNoRecordedRender) != tt.wantRecord {
				t.Errorf("errNoRecordedRender = %t, want %t", errors.Is(err, errNoRecordedRender), tt.wantRecord)
			}
			if len(got.Differing) != 0 {
				t.Errorf("compare result = %+v, want no result beside the error", got)
			}
		})
	}
}
