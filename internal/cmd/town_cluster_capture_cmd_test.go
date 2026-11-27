package cmd

import (
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestClusterCaptureCommandRegistered holds the command to its place under
// townClusterCmd and to the two flags it takes.
func TestClusterCaptureCommandRegistered(t *testing.T) {
	t.Parallel()
	commands := townClusterCmd.Commands()
	i := slices.IndexFunc(commands, func(c *cobra.Command) bool { return c.Name() == "capture" })
	if i < 0 {
		t.Fatalf("townClusterCmd has no capture command")
	}
	namespace := commands[i].Flags().Lookup("namespace")
	if namespace == nil {
		t.Fatalf("capture has no --namespace flag")
	}
	if namespace.Shorthand != "n" || namespace.DefValue != "" {
		t.Errorf("--namespace is -%s with default %q, want -n and an empty default", namespace.Shorthand, namespace.DefValue)
	}
	if commands[i].Flags().Lookup("out") == nil {
		t.Errorf("capture has no --out flag")
	}
}

// TestClusterCaptureCommandRefuses drives each refusal on a fresh command, so no
// case sets a flag on the registered one.
func TestClusterCaptureCommandRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "a missing --out", args: []string{"deployment/daemon"}, want: `required flag(s) "out" not set`},
		{name: "no argument", args: []string{"--out", "capture.json"}, want: "accepts 1 arg(s), received 0"},
		{name: "two arguments", args: []string{"deployment/daemon", "deployment/dashboard", "--out", "capture.json"}, want: "accepts 1 arg(s), received 2"},
		{name: "a complete call", args: []string{"deployment/daemon", "-n", "gt-sbx", "--out", "capture.json"}, want: "capture is not wired yet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := newTownClusterCaptureCmd()
			cmd.SetArgs(tc.args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("capture %q = %v, want an error holding %q", tc.args, err, tc.want)
			}
		})
	}
}

// TestClusterCaptureCommandDeps holds the deps to getwd's directory and the
// control cluster, with the runner and the clock wired.
func TestClusterCaptureCommandDeps(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	d, err := clusterCaptureCommandDeps(func() (string, error) { return dir, nil })
	if err != nil {
		t.Fatalf("clusterCaptureCommandDeps = %v, want no error", err)
	}
	if d.workDir != dir {
		t.Errorf("workDir = %q, want getwd's %q", d.workDir, dir)
	}
	if d.cluster != clusterControl {
		t.Errorf("cluster = %q, want %q", d.cluster, clusterControl)
	}
	if d.kubectl == nil {
		t.Errorf("kubectl is nil, want the real runner")
	}
	if d.now == nil {
		t.Errorf("now is nil, want time.Now")
	}
}

// TestClusterCaptureCommandDepsRefusesUnreadableDir drives the failing getwd: its
// error is returned and no deps are built.
func TestClusterCaptureCommandDepsRefusesUnreadableDir(t *testing.T) {
	t.Parallel()
	readErr := errors.New("the working directory is gone")
	d, err := clusterCaptureCommandDeps(func() (string, error) { return "", readErr })
	if !errors.Is(err, readErr) {
		t.Fatalf("clusterCaptureCommandDeps = %v, want an error holding %v", err, readErr)
	}
	if d.kubectl != nil || d.now != nil || d.cluster != "" || d.workDir != "" {
		t.Errorf("deps = %+v, want none with the error", d)
	}
}
