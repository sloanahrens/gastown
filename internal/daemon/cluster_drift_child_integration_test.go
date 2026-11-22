//go:build integration

package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// The tests in this file start real children, so they are in the integration
// tier: the unit tier uses fakes (cluster_drift_child_test.go). The children are
// /bin/sh one-liners, not this test binary re-executed: a re-executed test
// binary would run the package TestMain first, which starts a Dolt container
// before the stand-in body runs (om finding on gt-e8ol9r).

// TestIntegrationStartDriftChildRealProcess: what startDriftChild returns for a real
// child. A non-zero exit, including 255, is the child's answer with no error; a
// child killed by a signal is told apart from it by -1 and an error; a binary
// that does not exist is -1 and the start error.
func TestIntegrationStartDriftChildRealProcess(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		exe     string
		args    []string
		want    int
		wantErr bool
	}{
		{"exit 3", "/bin/sh", []string{"-c", "exit 3"}, 3, false},
		{"exit 255", "/bin/sh", []string{"-c", "exit 255"}, 255, false},
		{"killed by a signal", "/bin/sh", []string{"-c", "kill -9 $$"}, -1, true},
		{"missing binary", filepath.Join(t.TempDir(), "no-such-binary"), nil, -1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// A generous bound so a child that never exits fails the run instead of
			// hanging it; the deadline is the context's, never read as the child's exit.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, _, exit, err := startDriftChild(ctx, tt.exe, tt.args, 64)
			if exit != tt.want || (err != nil) != tt.wantErr {
				t.Errorf("startDriftChild = exit %d, err %v; want exit %d, error %v", exit, err, tt.want, tt.wantErr)
			}
		})
	}
}
