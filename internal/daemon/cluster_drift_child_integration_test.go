//go:build integration

package daemon

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
)

// The tests in this file start real children, so they are in the integration
// tier: the unit tier uses fakes (cluster_drift_child_test.go).

// TestIntegrationDriftChildStandIn is the child TestIntegrationStartDriftChildRealProcess runs. It
// does nothing in an ordinary run; when startDriftChild starts this test binary
// with a mode after "--", it acts on it: "exit N" exits with status N and
// "kill" kills itself with SIGKILL.
func TestIntegrationDriftChildStandIn(t *testing.T) {
	switch flag.Arg(0) {
	case "exit":
		code, _ := strconv.Atoi(flag.Arg(1))
		syscall.Exit(code)
	case "kill":
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
	}
}

// TestIntegrationStartDriftChildRealProcess: what startDriftChild returns for a real
// child. A non-zero exit, including 255, is the child's answer with no error; a
// child killed by a signal is told apart from it by -1 and an error; a binary
// that does not exist is -1 and the start error.
func TestIntegrationStartDriftChildRealProcess(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	mode := func(args ...string) []string {
		return append([]string{"-test.run=^TestIntegrationDriftChildStandIn$", "--"}, args...)
	}
	tests := []struct {
		name    string
		exe     string
		args    []string
		want    int
		wantErr bool
	}{
		{"exit 3", self, mode("exit", "3"), 3, false},
		{"exit 255", self, mode("exit", "255"), 255, false},
		{"killed by a signal", self, mode("kill"), -1, true},
		{"missing binary", filepath.Join(t.TempDir(), "no-such-binary"), nil, -1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, exit, err := startDriftChild(context.Background(), tt.exe, tt.args, 64)
			if exit != tt.want || (err != nil) != tt.wantErr {
				t.Errorf("startDriftChild = exit %d, err %v; want exit %d, error %v", exit, err, tt.want, tt.wantErr)
			}
		})
	}
}
