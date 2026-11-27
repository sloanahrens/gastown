package cmd

import (
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestClusterApplyCommandRegistered holds the command to its place under
// townClusterCmd: apply, with the four string flags the apply takes.
func TestClusterApplyCommandRegistered(t *testing.T) {
	t.Parallel()
	commands := townClusterCmd.Commands()
	i := slices.IndexFunc(commands, func(c *cobra.Command) bool { return c.Name() == "apply" })
	if i < 0 {
		t.Fatalf("townClusterCmd has no apply command")
	}
	for _, name := range []string{"plan", "approval", "policy-kubeconfig", "apply-kubeconfig"} {
		flag := commands[i].Flags().Lookup(name)
		if flag == nil {
			t.Errorf("apply has no --%s flag", name)
			continue
		}
		if flag.Value.Type() != "string" {
			t.Errorf("apply --%s is a %s flag, want string", name, flag.Value.Type())
		}
	}
}

// TestClusterApplyCommandRefuses drives each refusal on a fresh command, so no
// case sets a flag on the registered one.
func TestClusterApplyCommandRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "a missing --plan", args: []string{"--approval", "approval.json"}, want: `required flag(s) "plan" not set`},
		{name: "a missing --approval", args: []string{"--plan", "plan"}, want: `required flag(s) "approval" not set`},
		{name: "both required flags missing", args: []string{}, want: `required flag(s) "approval", "plan" not set`},
		{name: "a complete call", args: []string{"--plan", "plan", "--approval", "approval.json", "--policy-kubeconfig", "policy", "--apply-kubeconfig", "apply"}, want: "apply is not wired yet, so nothing was applied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := newTownClusterApplyCmd()
			cmd.SetArgs(tc.args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("apply %q = %v, want an error holding %q", tc.args, err, tc.want)
			}
		})
	}
}
