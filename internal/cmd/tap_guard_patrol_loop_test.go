package cmd

import (
	"encoding/json"
	"testing"
)

func TestMatchesBdMolPourPatrol(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"pour patrol suffix", "bd mol pour mol-witness-patrol", true},
		{"pour deacon patrol", "bd mol pour mol-deacon-patrol", true},
		{"pour refinery patrol", "bd mol pour mol-refinery-patrol", true},
		{"pour with flags before formula", "bd mol pour --var x=1 mol-witness-patrol", true},
		{"pour inside compound command", "cd ~/gt && bd mol pour mol-witness-patrol", true},
		{"wisp is allowed", "bd mol wisp mol-witness-patrol", false},
		{"pour of non-patrol formula", "bd mol pour mol-polecat-work", false},
		{"unrelated command", "echo hello", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesBdMolPourPatrol(shellTokenize(tt.command))
			if got != tt.want {
				t.Errorf("matchesBdMolPourPatrol(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}

func TestMatchesForSeqLoop(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"for seq", "for i in $(seq 1 5); do gt patrol run; done", true},
		{"for without seq", "for p in 1 2; do echo $p; done", false},
		{"seq without for", "seq 1 5", false},
		{"unrelated", "echo hello", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesForSeqLoop(shellTokenize(tt.command))
			if got != tt.want {
				t.Errorf("matchesForSeqLoop(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}

func TestMatchesOpenEndedWhileLoop(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"while true", "while true; do gt patrol run; done", true},
		{"while colon", "while :; do gt patrol run; done", true},
		{"while condition", "while [ -f /tmp/x ]; do sleep 1; done", false},
		{"unrelated", "echo hello", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesOpenEndedWhileLoop(shellTokenize(tt.command))
			if got != tt.want {
				t.Errorf("matchesOpenEndedWhileLoop(%q) = %v, want %v", tt.command, got, tt.want)
			}
		})
	}
}

func TestIsDeaconRole(t *testing.T) {
	tests := []struct {
		name     string
		gtDeacon string
		gtRole   string
		want     bool
	}{
		{"GT_DEACON set", "1", "", true},
		{"GT_ROLE deacon", "", "deacon", true},
		{"GT_ROLE witness", "", "gastown/witness", false},
		{"neither set", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GT_DEACON", tt.gtDeacon)
			t.Setenv("GT_ROLE", tt.gtRole)
			if got := isDeaconRole(); got != tt.want {
				t.Errorf("isDeaconRole() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRunTapGuardPatrolLoop_UnresolvableCompoundCommandsAllowed reproduces
// gt-qqfy's exact false-positive shapes: compound commands containing an
// argument-position substitution or expansion that Claude Code's "if" glob
// evaluator cannot statically resolve, which made every leading-* if-glob
// (Bash(*bd mol pour*patrol*), Bash(*for *seq*), etc.) match regardless of
// content. Since patrol-loop is now registered ungated and inspects the real
// command itself, none of these unrelated shapes should ever block — for
// every role, including deacon.
func TestRunTapGuardPatrolLoop_UnresolvableCompoundCommandsAllowed(t *testing.T) {
	commands := []string{
		`echo $(true)`,
		`echo "$(true)"`,
		`x=ab; echo "len ${#x}"`,
		`x=$(true); echo ok`,
		`cd ~/gt && S=/tmp; x=$(cat $S/f 2>/dev/null); head -1 $S/f 2>/dev/null | cut -c1-1; echo end`,
		`for p in 1 2; do echo $p; done; echo "rss=$(ps -o rss= -p $$)"`,
	}
	for _, role := range []string{"", "deacon", "gastown/witness", "gastown/refinery"} {
		t.Run(role, func(t *testing.T) {
			t.Setenv("GT_ROLE", role)
			t.Setenv("GT_DEACON", "")
			for _, command := range commands {
				hookInput := `{"tool_name":"Bash","tool_input":{"command":` + jsonQuote(command) + `}}`
				var err error
				withStdin(t, hookInput, func() {
					err = runTapGuardPatrolLoop(tapGuardPatrolLoopCmd, nil)
				})
				if err != nil {
					t.Errorf("role %q: expected command %q to be allowed, got error: %v", role, command, err)
				}
			}
		})
	}
}

func TestRunTapGuardPatrolLoop_BlocksPourOfPatrolFormula(t *testing.T) {
	t.Setenv("GT_ROLE", "gastown/witness")

	hookInput := `{"tool_name":"Bash","tool_input":{"command":"bd mol pour mol-witness-patrol"}}`
	var err error
	withStdin(t, hookInput, func() {
		err = runTapGuardPatrolLoop(tapGuardPatrolLoopCmd, nil)
	})
	if err == nil {
		t.Error("expected bd mol pour of a patrol formula to be blocked, got nil error")
	}
}

func TestRunTapGuardPatrolLoop_AllowsWispOfPatrolFormula(t *testing.T) {
	t.Setenv("GT_ROLE", "gastown/witness")

	hookInput := `{"tool_name":"Bash","tool_input":{"command":"bd mol wisp mol-witness-patrol"}}`
	var err error
	withStdin(t, hookInput, func() {
		err = runTapGuardPatrolLoop(tapGuardPatrolLoopCmd, nil)
	})
	if err != nil {
		t.Errorf("expected bd mol wisp to be allowed, got error: %v", err)
	}
}

func TestRunTapGuardPatrolLoop_DeaconOnlyLoopChecks(t *testing.T) {
	loopCommands := []string{
		`for i in $(seq 1 5); do gt patrol run; done`,
		`while true; do gt patrol run; done`,
		`while :; do gt patrol run; done`,
	}
	for _, command := range loopCommands {
		t.Run(command, func(t *testing.T) {
			t.Run("deacon blocked", func(t *testing.T) {
				t.Setenv("GT_ROLE", "deacon")
				hookInput := `{"tool_name":"Bash","tool_input":{"command":` + jsonQuote(command) + `}}`
				var err error
				withStdin(t, hookInput, func() {
					err = runTapGuardPatrolLoop(tapGuardPatrolLoopCmd, nil)
				})
				if err == nil {
					t.Errorf("expected deacon loop command %q to be blocked, got nil error", command)
				}
			})

			t.Run("witness allowed", func(t *testing.T) {
				t.Setenv("GT_ROLE", "gastown/witness")
				hookInput := `{"tool_name":"Bash","tool_input":{"command":` + jsonQuote(command) + `}}`
				var err error
				withStdin(t, hookInput, func() {
					err = runTapGuardPatrolLoop(tapGuardPatrolLoopCmd, nil)
				})
				if err != nil {
					t.Errorf("expected witness loop command %q to be allowed (deacon-only check), got error: %v", command, err)
				}
			})
		})
	}
}

// jsonQuote produces a JSON string literal (with surrounding quotes) for use
// building hand-written hook-input JSON in tests.
func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
