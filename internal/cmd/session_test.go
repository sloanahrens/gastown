package cmd

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

func TestSessionInfoJSONOutput(t *testing.T) {
	t.Parallel()
	info := &polecat.SessionInfo{
		Polecat:   "alpha",
		SessionID: "gt-alpha",
		Running:   true,
		RigName:   "gastown",
		Attached:  false,
		Created:   time.Date(2026, 2, 20, 10, 0, 0, 0, time.UTC),
		Windows:   1,
	}

	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		t.Fatalf("json.MarshalIndent failed: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if parsed["polecat"] != "alpha" {
		t.Errorf("polecat = %v, want alpha", parsed["polecat"])
	}
	if parsed["session_id"] != "gt-alpha" {
		t.Errorf("session_id = %v, want gt-alpha", parsed["session_id"])
	}
	if parsed["running"] != true {
		t.Errorf("running = %v, want true", parsed["running"])
	}
	if parsed["rig_name"] != "gastown" {
		t.Errorf("rig_name = %v, want gastown", parsed["rig_name"])
	}
}

func TestSessionStatusCmdJSONFlagWiring(t *testing.T) {
	t.Parallel()
	// Verify --json flag is registered on the session status command.
	// This catches regressions where flag binding is accidentally removed,
	// which would silently break formulas that depend on --json output.
	f := sessionStatusCmd.Flags().Lookup("json")
	if f == nil {
		t.Fatal("session status command missing --json flag")
	}
	if f.DefValue != "false" {
		t.Errorf("--json default = %q, want \"false\"", f.DefValue)
	}
}

func TestSessionHealthCmdFlagWiring(t *testing.T) {
	t.Parallel()
	if sessionCmd.Commands() == nil {
		t.Fatal("session command has no subcommands")
	}

	f := sessionHealthCmd.Flags().Lookup("json")
	if f == nil {
		t.Fatal("session health command missing --json flag")
	}
	if f.DefValue != "false" {
		t.Errorf("--json default = %q, want \"false\"", f.DefValue)
	}

	f = sessionHealthCmd.Flags().Lookup("max-inactivity")
	if f == nil {
		t.Fatal("session health command missing --max-inactivity flag")
	}
	if f.DefValue != "0s" {
		t.Errorf("--max-inactivity default = %q, want \"0s\"", f.DefValue)
	}
}

func TestSessionHealthReportJSONContract(t *testing.T) {
	t.Parallel()
	report := newSessionHealthReport("gt-vault", tmux.AgentDead, 30*time.Minute)
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	if parsed["session"] != "gt-vault" {
		t.Errorf("session = %v, want gt-vault", parsed["session"])
	}
	if parsed["status"] != "agent-dead" {
		t.Errorf("status = %v, want agent-dead", parsed["status"])
	}
	if parsed["healthy"] != false {
		t.Errorf("healthy = %v, want false", parsed["healthy"])
	}
	if parsed["zombie"] != true {
		t.Errorf("zombie = %v, want true", parsed["zombie"])
	}
	if parsed["max_inactivity_seconds"] != float64(1800) {
		t.Errorf("max_inactivity_seconds = %v, want 1800", parsed["max_inactivity_seconds"])
	}
}

// TestRunSessionHealthJSONSessionDead is also the no-regression guard for
// gt-3uyr: a well-formed session name that does not exist must KEEP reporting
// session-dead with exit 0. Callers ask that question on purpose —
// plugins/stuck-agent-dog/run.sh restarts a polecat on a session-dead verdict
// and treats a non-zero exit as "health unavailable". Only an argument that can
// never name a session (a rig/name address) is rejected.
func TestRunSessionHealthJSONSessionDead(t *testing.T) {
	oldJSON := sessionHealthJSON
	oldMaxInactivity := sessionHealthMaxInactivity
	oldStdout := os.Stdout
	t.Cleanup(func() {
		sessionHealthJSON = oldJSON
		sessionHealthMaxInactivity = oldMaxInactivity
		os.Stdout = oldStdout
	})

	sessionHealthJSON = true
	sessionHealthMaxInactivity = 0
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe failed: %v", err)
	}
	os.Stdout = w

	err = runSessionHealth(sessionHealthCmd, []string{"gt-session-health-test-nonexistent"})
	if closeErr := w.Close(); closeErr != nil {
		t.Fatalf("closing pipe writer: %v", closeErr)
	}
	os.Stdout = oldStdout
	if err != nil {
		t.Fatalf("runSessionHealth failed: %v", err)
	}

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading stdout pipe: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("json.Unmarshal failed: %v\noutput: %s", err, string(data))
	}
	if parsed["session"] != "gt-session-health-test-nonexistent" {
		t.Errorf("session = %v, want gt-session-health-test-nonexistent", parsed["session"])
	}
	if parsed["status"] != "session-dead" {
		t.Errorf("status = %v, want session-dead", parsed["status"])
	}
	if parsed["healthy"] != false {
		t.Errorf("healthy = %v, want false", parsed["healthy"])
	}
	if parsed["zombie"] != false {
		t.Errorf("zombie = %v, want false", parsed["zombie"])
	}
}

// gt-3uyr: a rig/name address is an argument error, not a dead session.
func TestSessionHealthArgErrorRejectsRigNameAddress(t *testing.T) {
	t.Parallel()

	for _, addr := range []string{"gastown/witness", "gastown/polecats/granite", "om-witness/crew/bob"} {
		err := sessionHealthArgError(addr)
		if err == nil {
			t.Errorf("sessionHealthArgError(%q) = nil, want error", addr)
			continue
		}
		msg := err.Error()
		// Registry-independent assertions: reachable regardless of which rigs
		// the prefix registry happens to know in this process.
		if !strings.Contains(msg, "tmux session name") {
			t.Errorf("sessionHealthArgError(%q) message %q does not name the expected argument form", addr, msg)
		}
		if !strings.Contains(msg, "gt session status "+addr) {
			t.Errorf("sessionHealthArgError(%q) message %q does not point at the rig/name subcommand", addr, msg)
		}
	}
}

// Not parallel: swaps the package-level prefix registry.
func TestSessionHealthArgErrorSuggestsSessionNameForKnownRig(t *testing.T) {
	prev := session.DefaultRegistry()
	t.Cleanup(func() { session.SetDefaultRegistry(prev) })

	reg := session.NewPrefixRegistry()
	reg.Register("gt", "gastown")
	session.SetDefaultRegistry(reg)

	err := sessionHealthArgError("gastown/witness")
	if err == nil {
		t.Fatal("sessionHealthArgError(gastown/witness) = nil, want error")
	}
	if !strings.Contains(err.Error(), "gt session health gt-witness") {
		t.Errorf("error %q does not offer the resolved tmux session name", err)
	}

	// An unregistered rig has no derivable prefix — PrefixFor falls back to the
	// default, so a guess would be plausible but wrong. Offer nothing.
	err = sessionHealthArgError("nosuchrig/witness")
	if err == nil {
		t.Fatal("sessionHealthArgError(nosuchrig/witness) = nil, want error")
	}
	if strings.Contains(err.Error(), "Did you mean") {
		t.Errorf("error %q guesses a session name for an unknown rig", err)
	}
}

func TestSessionHealthArgErrorRejectsEmpty(t *testing.T) {
	t.Parallel()

	for _, arg := range []string{"", "   "} {
		if err := sessionHealthArgError(arg); err == nil {
			t.Errorf("sessionHealthArgError(%q) = nil, want error", arg)
		}
	}
}

func TestSessionHealthArgErrorAcceptsSessionNames(t *testing.T) {
	t.Parallel()

	// Names that cannot be resolved by rig/name parsing must pass through
	// untouched: whether they exist is tmux's question, not this validator's.
	for _, name := range []string{"gt-witness", "gt-session-health-test-nonexistent", "hq-mayor", "gt-crew-bob"} {
		if err := sessionHealthArgError(name); err != nil {
			t.Errorf("sessionHealthArgError(%q) = %v, want nil", name, err)
		}
	}
}

// The wiring, not just the validator: runSessionHealth must return the
// argument error before probing tmux, in both output modes.
func TestRunSessionHealthRejectsRigNameAddress(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		name := "text"
		if asJSON {
			name = "json"
		}
		t.Run(name, func(t *testing.T) {
			oldJSON := sessionHealthJSON
			oldStdout := os.Stdout
			t.Cleanup(func() {
				sessionHealthJSON = oldJSON
				os.Stdout = oldStdout
			})
			sessionHealthJSON = asJSON

			r, w, err := os.Pipe()
			if err != nil {
				t.Fatalf("os.Pipe failed: %v", err)
			}
			os.Stdout = w

			err = runSessionHealth(sessionHealthCmd, []string{"gastown/witness"})
			if closeErr := w.Close(); closeErr != nil {
				t.Fatalf("closing pipe writer: %v", closeErr)
			}
			os.Stdout = oldStdout

			if err == nil {
				t.Fatal("runSessionHealth(gastown/witness) = nil, want error")
			}
			if !strings.Contains(err.Error(), "tmux session name") {
				t.Errorf("error %q does not explain the expected argument form", err)
			}

			// The defect was the emitted payload, not just the exit code: a
			// session-dead/healthy:false report must not be printed at all.
			out, readErr := io.ReadAll(r)
			if readErr != nil {
				t.Fatalf("reading stdout pipe: %v", readErr)
			}
			if len(out) != 0 {
				t.Errorf("runSessionHealth(gastown/witness) wrote %q to stdout, want nothing", string(out))
			}
		})
	}
}

func TestSessionInfoJSONOutputNotRunning(t *testing.T) {
	t.Parallel()
	info := &polecat.SessionInfo{
		Polecat:   "beta",
		SessionID: "gt-beta",
		Running:   false,
		RigName:   "testrig",
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if parsed["running"] != false {
		t.Errorf("running = %v, want false", parsed["running"])
	}
}
