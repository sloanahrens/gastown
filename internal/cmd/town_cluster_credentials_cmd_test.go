package cmd

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// TestClusterCredentialsCommandName holds the constructed command to the name credentials.
func TestClusterCredentialsCommandName(t *testing.T) {
	t.Parallel()
	if got := newTownClusterCredentialsCmd(nil).Name(); got != "credentials" {
		t.Errorf("Name() = %q, want %q", got, "credentials")
	}
}

// TestClusterCredentialsCommandFlags holds each of the three string flags to an empty default.
func TestClusterCredentialsCommandFlags(t *testing.T) {
	t.Parallel()
	cmd := newTownClusterCredentialsCmd(nil)
	for _, name := range []string{"cluster", "role", "out"} {
		f := cmd.Flags().Lookup(name)
		if f == nil {
			t.Errorf("credentials has no --%s flag", name)
			continue
		}
		if f.Value.Type() != "string" {
			t.Errorf("--%s type = %q, want string", name, f.Value.Type())
		}
		if f.DefValue != "" {
			t.Errorf("--%s default = %q, want empty", name, f.DefValue)
		}
	}
}

// runClusterCredentialsCommandWith executes a fresh command over run with args and returns the error,
// with its output discarded so usage text does not reach the test log.
func runClusterCredentialsCommandWith(t *testing.T, run func(context.Context, []string) ([]byte, error), args []string) error {
	t.Helper()
	cmd := newTownClusterCredentialsCmd(run)
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	return cmd.Execute()
}

// TestClusterCredentialsCommandCall holds a complete call to one run of the runner with the request's
// argv, and a call missing the role or the out path to credentialRequest's refusal with the runner
// never called.
func TestClusterCredentialsCommandCall(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "kubeconfig")
	for _, row := range []struct {
		args  []string
		want  string
		calls int
	}{
		{[]string{"--cluster", "gt-sbx", "--role", "read", "--out", out}, "kubeadm printed no kubeconfig", 1},
		{[]string{"--cluster", "gt-sbx", "--out", out}, "a missing role", 0},
		{[]string{"--cluster", "gt-sbx", "--role", "read"}, "a missing out", 0},
	} {
		var argv [][]string
		run := func(_ context.Context, a []string) ([]byte, error) {
			argv = append(argv, a)
			return nil, errors.New("kubeadm failed")
		}
		if err := runClusterCredentialsCommandWith(t, run, row.args); err == nil || !strings.Contains(err.Error(), row.want) {
			t.Fatalf("credentials %v error = %v, want it to name %q", row.args, err, row.want)
		}
		if len(argv) != row.calls {
			t.Fatalf("credentials %v called the runner %d times, want %d", row.args, len(argv), row.calls)
		}
		if row.calls == 1 && !strings.Contains(strings.Join(argv[0], " "), "gt-deployer-read") {
			t.Errorf("runner argv = %v, want it to name gt-deployer-read", argv[0])
		}
	}
}

// TestClusterCredentialsCommandRefusesPositional holds a call with a positional argument to a refusal
// made before RunE, so credentialRequest's refusal is not what the call returns.
func TestClusterCredentialsCommandRefusesPositional(t *testing.T) {
	t.Parallel()
	run := func(context.Context, []string) ([]byte, error) { return nil, errors.New("the runner was called") }
	err := runClusterCredentialsCommandWith(t, run, []string{"apply"})
	if err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("credentials with a positional argument = %v, want cobra's unknown command error", err)
	}
}
