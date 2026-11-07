package gtwrap

import (
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

func TestPlanSelectsTheToolByInvokedName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"gt", []string{"status", "--line"}, []string{"gt", "status", "--line"}},
		{"bd", []string{"show", "gt-abc"}, []string{"bd", "show", "gt-abc"}},
		{"om", []string{"review", "--json"}, []string{"om", "review", "--json"}},
		{"hm", nil, []string{"hm"}},
		// An argument holding a space is one argument and stays one, since the
		// container receives argv and never a shell line to split.
		{"gt", []string{"bead", "note", "the retry never resets"}, []string{"gt", "bead", "note", "the retry never resets"}},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/"+strings.Join(tt.args, " "), func(t *testing.T) {
			t.Parallel()
			call, err := Plan(tt.name, tt.args, nil, "/town", false)
			if err != nil {
				t.Fatalf("Plan(%q) returned an error: %v", tt.name, err)
			}
			if !slices.Equal(call.Argv, tt.want) {
				t.Fatalf("Plan(%q) ran %q, want %q", tt.name, call.Argv, tt.want)
			}
		})
	}
}

func TestPlanRefusesANameItWasNotInstalledAs(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"bash", "gt-wrap", "", "/usr/local/bin/gt", "GT"} {
		call, err := Plan(name, nil, nil, "/town", false)
		if err == nil {
			t.Fatalf("Plan(%q) returned no error; it ran %q", name, call.Argv)
		}
		if !strings.Contains(err.Error(), "gt, bd, om and hm") {
			t.Fatalf("Plan(%q) error %q does not name the four tools", name, err)
		}
		if call.Argv != nil {
			t.Fatalf("Plan(%q) returned a call to run %q with its error", name, call.Argv)
		}
	}
}

func TestPlanForwardsOnlyTheWrapperEnvironment(t *testing.T) {
	t.Parallel()
	env := []string{
		"HOME=/Users/operator",
		"AWS_SECRET_ACCESS_KEY=secret",
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"GT_TOWN=/town",
		"BEADS_DOLT_SERVER_HOST=127.0.0.1",
		"BD_DEBUG_ROUTING=1",
		"CLAUDE_CONFIG_DIR=/Users/operator/.claude",
		"TERM=xterm-256color",
		"COLUMNS=120",
		"LINES=40",
		// A longer name that merely starts with the letters of an allowed one.
		"GTFOO=no",
		"TERM_PROGRAM=Apple_Terminal",
		// An entry with no "=" names no variable and cannot be forwarded.
		"GT_NO_EQUALS",
	}
	want := []string{
		"GT_TOWN=/town",
		"BEADS_DOLT_SERVER_HOST=127.0.0.1",
		"BD_DEBUG_ROUTING=1",
		"CLAUDE_CONFIG_DIR=/Users/operator/.claude",
		"TERM=xterm-256color",
		"COLUMNS=120",
		"LINES=40",
	}
	call, err := Plan("gt", []string{"status"}, env, "/town", false)
	if err != nil {
		t.Fatalf("Plan returned an error: %v", err)
	}
	if !slices.Equal(call.Env, want) {
		t.Fatalf("Plan forwarded %q, want %q", call.Env, want)
	}
}

func TestPlanNeverForwardsACredentialName(t *testing.T) {
	t.Parallel()
	credentials := config.UnforwardedCredentialEnvVars()
	// An empty list would screen nothing and pass every case below, so it is a failure.
	if len(credentials) == 0 {
		t.Fatal("config.UnforwardedCredentialEnvVars() names no credential")
	}
	env := []string{"CLAUDE_CONFIG_DIR=/Users/operator/.claude"}
	for _, name := range credentials {
		env = append(env, name+"=secret")
	}
	call, err := Plan("gt", []string{"status"}, env, "/town", false)
	if err != nil {
		t.Fatalf("Plan returned an error: %v", err)
	}
	want := []string{"CLAUDE_CONFIG_DIR=/Users/operator/.claude"}
	if !slices.Equal(call.Env, want) {
		t.Fatalf("Plan forwarded %q, want only the ordinary CLAUDE_ name %q", call.Env, want)
	}
}

// The OAuth token is in the CLAUDE_ family, so the prefix alone would forward it.
// This names it directly, so the refusal holds whatever the credential list holds.
func TestPlanRefusesTheOAuthTokenInTheClaudeFamily(t *testing.T) {
	t.Parallel()
	env := []string{
		"CLAUDE_CODE_OAUTH_TOKEN=secret",
		"CLAUDE_CONFIG_DIR=/Users/operator/.claude",
	}
	call, err := Plan("gt", []string{"status"}, env, "/town", false)
	if err != nil {
		t.Fatalf("Plan returned an error: %v", err)
	}
	want := []string{"CLAUDE_CONFIG_DIR=/Users/operator/.claude"}
	if !slices.Equal(call.Env, want) {
		t.Fatalf("Plan forwarded %q, want only the ordinary CLAUDE_ name %q", call.Env, want)
	}
}

func TestPlanCarriesTheCallersDirectoryAndTerminal(t *testing.T) {
	t.Parallel()
	for _, isTTY := range []bool{false, true} {
		call, err := Plan("gt", []string{"status"}, nil, "/town/rig", isTTY)
		if err != nil {
			t.Fatalf("Plan returned an error: %v", err)
		}
		if call.Dir != "/town/rig" {
			t.Fatalf("Plan ran in %q, want the caller's directory %q", call.Dir, "/town/rig")
		}
		if call.TTY != isTTY {
			t.Fatalf("Plan recorded TTY %t, want %t", call.TTY, isTTY)
		}
	}
}
