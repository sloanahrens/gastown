package cmd

import (
	"io"
	"strings"
	"testing"
)

// TestClusterCredentialsCommandName holds the constructed command to the name credentials.
func TestClusterCredentialsCommandName(t *testing.T) {
	t.Parallel()
	if got := newTownClusterCredentialsCmd().Name(); got != "credentials" {
		t.Errorf("Name() = %q, want %q", got, "credentials")
	}
}

// TestClusterCredentialsCommandFlags holds each of the three string flags to an empty default.
func TestClusterCredentialsCommandFlags(t *testing.T) {
	t.Parallel()
	cmd := newTownClusterCredentialsCmd()
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

// runClusterCredentialsCommandWith executes a fresh command with args and returns the error, with its
// output discarded so usage text does not reach the test log.
func runClusterCredentialsCommandWith(t *testing.T, args []string) error {
	t.Helper()
	cmd := newTownClusterCredentialsCmd()
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	return cmd.Execute()
}

// TestClusterCredentialsCommandRefusesCall holds a call with no argument to the not-wired refusal.
func TestClusterCredentialsCommandRefusesCall(t *testing.T) {
	t.Parallel()
	err := runClusterCredentialsCommandWith(t, []string{})
	if err == nil {
		t.Fatalf("credentials with no argument = nil, want the not-wired error")
	}
	if !strings.Contains(err.Error(), "not wired yet") {
		t.Errorf("credentials error = %q, want it to name %q", err, "not wired yet")
	}
}

// TestClusterCredentialsCommandRefusesPositional holds a call with a positional argument to a refusal
// made before RunE, so the not-wired error is not what the call returns.
func TestClusterCredentialsCommandRefusesPositional(t *testing.T) {
	t.Parallel()
	err := runClusterCredentialsCommandWith(t, []string{"apply"})
	if err == nil {
		t.Fatalf("credentials with a positional argument = nil, want an error")
	}
	if strings.Contains(err.Error(), "not wired yet") {
		t.Errorf("credentials with a positional argument reached RunE: %q", err)
	}
}
