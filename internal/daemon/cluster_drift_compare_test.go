package daemon

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// fakeDriftStart is an injected start that records its one call and answers
// with fixed output, so the compare is tested with no process (gt-jokegc).
type fakeDriftStart struct {
	stdout []byte
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
	return f.stdout, nil, f.exit, f.runErr
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
	if fake.ctx != ctx {
		t.Error("start did not receive the caller's context")
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
