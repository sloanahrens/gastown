package cmd

import (
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
