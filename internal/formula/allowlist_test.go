package formula

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Allowlist used by most matcher tests — mirrors mol-dog-reaper's declared
// command surface (gt-9iv).
var testAllowlist = []string{
	"gt reaper",
	"gt convoy check",
	"gt escalate",
	"jq",
	"echo",
}

func TestCheckCommandAllowed(t *testing.T) {
	tests := []struct {
		name    string
		command string
		wantOK  bool
	}{
		{"exact prefix", "gt reaper scan --db=beads --json", true},
		{"prefix with more tokens", "gt convoy check", true},
		{"single token entry", "jq '.candidates'", true},
		{"disallowed command", "gt dolt cleanup --force", false},
		{"disallowed similar prefix", "gt convoy list", false},
		{"prefix must match whole tokens", "gt reaperx", false},
		{"empty command", "", true},

		// Multiple segments — every segment must pass.
		{"chained allowed", "gt reaper scan --json && gt reaper reap --json", true},
		{"chained with disallowed tail", "gt reaper scan && gt dolt cleanup --force", false},
		{"semicolon disallowed", "gt reaper scan; rm -rf /tmp/x", false},
		{"pipe to allowed", "gt reaper scan --json | jq '.total'", true},
		{"pipe to disallowed", "gt reaper scan --json | tee /tmp/out", false},
		{"or-chained disallowed", "gt reaper scan || curl http://example.com", false},
		{"background disallowed", "gt reaper scan & rm -rf /tmp/x", false},

		// Quoting — operators inside quotes must not split segments.
		{"operators inside double quotes", `gt escalate -s HIGH -m "scan && cleanup failed; see log"`, true},
		{"operators inside single quotes", `gt escalate -m 'a | b ; c'`, true},
		{"disallowed hidden after quoted arg", `gt escalate -m "ok" && gt dolt cleanup`, false},

		// Env assignments and control keywords.
		{"leading env assignment", "GT_DOLT_PORT=3307 gt reaper scan", true},
		{"env assignment then disallowed", "FOO=bar gt dolt cleanup", false},
		{"if header", "if gt reaper scan --json; then echo ok; fi", true},
		{"if header disallowed body", "if true; then gt dolt cleanup; fi", false},
		{"for loop over literals", "for db in beads gastown; do gt reaper reap --db=$db; done", true},
		{"for loop disallowed body", "for db in beads; do gt dolt cleanup --db=$db; done", false},
		{"export assignment", "export GT_DOLT_PORT=3307", true},

		// Command substitution — inner commands are checked recursively.
		{"allowed substitution", `echo "$(gt reaper databases --json)"`, true},
		{"disallowed substitution", `echo "$(gt dolt cleanup --force)"`, false},
		{"nested substitution disallowed", `echo "$(echo $(gt dolt cleanup))"`, false},
		{"substitution in loop header", "for db in $(gt reaper databases); do gt reaper scan --db=$db; done", true},
		{"backticks rejected", "echo `gt reaper scan`", false},
		{"arithmetic expansion opaque", "echo $((1 + 2))", true},

		// Redirections must not confuse the splitter.
		{"stderr redirect", "gt reaper scan --json 2>&1", true},
		{"redirect to file", "gt reaper scan > /tmp/scan.json", true},

		// Subshells.
		{"subshell allowed", "(gt reaper scan && gt reaper reap)", true},
		{"subshell disallowed", "(gt dolt cleanup)", false},

		// Variable indirection cannot dodge the list.
		{"variable as command", "$CMD --force", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckCommandAllowed(tt.command, testAllowlist)
			if tt.wantOK && err != nil {
				t.Errorf("CheckCommandAllowed(%q) = %v, want allowed", tt.command, err)
			}
			if !tt.wantOK && err == nil {
				t.Errorf("CheckCommandAllowed(%q) = nil, want blocked", tt.command)
			}
		})
	}
}

func TestCheckCommandAllowedErrorNamesSegment(t *testing.T) {
	err := CheckCommandAllowed("gt reaper scan && gt dolt cleanup --force", testAllowlist)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "gt dolt cleanup") {
		t.Errorf("error should name the offending segment, got: %v", err)
	}
}

func TestMergeAllowlist(t *testing.T) {
	got := mergeAllowlist([]string{"gt reaper", "jq"}, []string{"jq", "gt escalate"})
	want := []string{"gt reaper", "jq", "gt escalate"}
	if len(got) != len(want) {
		t.Fatalf("mergeAllowlist = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mergeAllowlist[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	if out := mergeAllowlist(nil, nil); len(out) != 0 {
		t.Errorf("mergeAllowlist(nil, nil) = %v, want empty", out)
	}
}

func TestParse_CommandAllowlist(t *testing.T) {
	f, err := Parse([]byte(`
formula = "test-allowlist"
description = "Test"
type = "workflow"
command_allowlist = ["gt reaper", "gt convoy check"]

[[steps]]
id = "step1"
title = "Step 1"
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{"gt reaper", "gt convoy check"}
	if len(f.CommandAllowlist) != len(want) {
		t.Fatalf("CommandAllowlist = %v, want %v", f.CommandAllowlist, want)
	}
	for i := range want {
		if f.CommandAllowlist[i] != want[i] {
			t.Errorf("CommandAllowlist[%d] = %q, want %q", i, f.CommandAllowlist[i], want[i])
		}
	}
}

func TestValidate_CommandAllowlistEmptyEntry(t *testing.T) {
	_, err := Parse([]byte(`
formula = "test-allowlist"
type = "workflow"
command_allowlist = ["gt reaper", "  "]

[[steps]]
id = "step1"
title = "Step 1"
`))
	if err == nil || !strings.Contains(err.Error(), "command_allowlist") {
		t.Errorf("expected command_allowlist validation error, got: %v", err)
	}
}

// TestResolve_CommandAllowlistInherited verifies that command_allowlist
// entries are inherited through extends and merged with the child's (gt-9iv).
func TestResolve_CommandAllowlistInherited(t *testing.T) {
	dir := t.TempDir()
	parent := `
formula = "allowlist-parent"
type = "workflow"
command_allowlist = ["gt reaper"]

[[steps]]
id = "base"
title = "Base"
`
	if err := os.WriteFile(filepath.Join(dir, "allowlist-parent.formula.toml"), []byte(parent), 0644); err != nil {
		t.Fatal(err)
	}

	child, err := Parse([]byte(`
formula = "allowlist-child"
type = "workflow"
extends = ["allowlist-parent"]
command_allowlist = ["gt convoy check"]
`))
	if err != nil {
		t.Fatalf("Parse child: %v", err)
	}

	resolved, err := Resolve(child, []string{dir})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	want := []string{"gt reaper", "gt convoy check"}
	if len(resolved.CommandAllowlist) != len(want) {
		t.Fatalf("resolved CommandAllowlist = %v, want %v", resolved.CommandAllowlist, want)
	}
	for i := range want {
		if resolved.CommandAllowlist[i] != want[i] {
			t.Errorf("CommandAllowlist[%d] = %q, want %q", i, resolved.CommandAllowlist[i], want[i])
		}
	}
}
