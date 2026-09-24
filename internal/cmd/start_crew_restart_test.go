package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/tmux"
)

// restartProbe is the tmux shim used by the crew-restart tests below. It
// answers the liveness queries startOrRestartCrewMember makes without a tmux
// server, and logs every invocation so a test can assert what was sent.
type restartProbe struct {
	logPath string
}

// shimTmux replaces tmux on PATH for the duration of the test. The shim
// reports a session that exists (has-session) whose pane runs a shell
// (display-message/list-panes) and whose session env holds nothing
// (show-environment), so IsAgentAlive reports the agent as exited — the
// restart branch. Every invocation is appended to the returned log.
func shimTmux(t *testing.T) *restartProbe {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("tmux shim is POSIX-only; tmux itself does not run on Windows")
	}

	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "tmux.log")

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString(`printf '%s\n' "$*" >> "` + logPath + `"` + "\n")
	b.WriteString("sub=\"\"\n")
	b.WriteString("for a in \"$@\"; do\n")
	b.WriteString("  case \"$a\" in\n")
	b.WriteString("    has-session|show-environment|display-message|list-panes|send-keys|set-environment|capture-pane) sub=$a; break;;\n")
	b.WriteString("  esac\n")
	b.WriteString("done\n")
	b.WriteString("case \"$sub\" in\n")
	b.WriteString("  has-session) exit 0 ;;\n")
	// No session environment: an unset key reports "unknown variable", which
	// getEnvironmentOptional reads as absent rather than as an error.
	b.WriteString("  show-environment) echo \"unknown variable\" >&2; exit 1 ;;\n")
	b.WriteString("  display-message)\n")
	b.WriteString("    for a in \"$@\"; do\n")
	b.WriteString("      case \"$a\" in\n")
	b.WriteString("        *pane_current_command*) echo bash; exit 0 ;;\n")
	b.WriteString("        *pane_pid*) echo 999999; exit 0 ;;\n")
	b.WriteString("      esac\n")
	b.WriteString("    done\n")
	b.WriteString("    exit 0 ;;\n")
	b.WriteString("  list-panes) printf 'bash\\t999999\\n'; exit 0 ;;\n")
	b.WriteString("esac\n")
	b.WriteString("exit 0\n")

	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(b.String()), 0o755); err != nil {
		t.Fatalf("write tmux shim: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &restartProbe{logPath: logPath}
}

// invocations returns the logged tmux argv lines.
func (p *restartProbe) invocations(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(p.logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read shim log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// sentKeys reports whether the shim saw a send-keys invocation.
func (p *restartProbe) sentKeys(t *testing.T) bool {
	t.Helper()
	for _, line := range p.invocations(t) {
		if strings.Contains(line, "send-keys") {
			return true
		}
	}
	return false
}

// restartFixture writes a town whose crew worker resolves to an agent whose
// env references a ${VAR}, so the startup command cannot be built unless that
// variable is set.
func restartFixture(t *testing.T, crewName, unsetVar string) (r *rig.Rig, townRoot string) {
	t.Helper()

	townRoot = t.TempDir()
	rigPath := filepath.Join(townRoot, "testrig")

	settings := config.NewRigSettings()
	settings.WorkerAgents = map[string]string{crewName: "proxied-agent"}
	settings.Agents = map[string]*config.RuntimeConfig{
		"proxied-agent": {
			Command: "claude",
			Env:     map[string]string{"ANTHROPIC_AUTH_TOKEN": "${" + unsetVar + "}"},
		},
	}
	if err := config.SaveRigSettings(config.RigSettingsPath(rigPath), settings); err != nil {
		t.Fatalf("SaveRigSettings: %v", err)
	}

	return &rig.Rig{Name: "testrig", Path: rigPath}, townRoot
}

// When the resolved agent's env references an unset variable, the restart must
// be held: no command is typed into the live pane, and the reason is reported
// naming the variable (gt-yih1, gt-wisp-jsm). The earlier attempt at this
// returned a message claiming a fallback that never happened.
func TestStartOrRestartCrewMember_HoldsRestartWithUnsetEnvReference(t *testing.T) {
	probe := shimTmux(t)
	const unsetVar = "GT_TEST_CREW_UNSET_TOKEN"
	t.Setenv(unsetVar, "")

	r, townRoot := restartFixture(t, "alice", unsetVar)
	tm := tmux.NewTmuxWithSocket("gt-test-crew-restart-held")

	msg, started := startOrRestartCrewMember(tm, r, "alice", townRoot)

	if started {
		t.Errorf("started = true for a held restart: %q", msg)
	}
	for _, want := range []string{"restart held", "testrig/alice", unsetVar, "session preserved"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
	if strings.Contains(msg, "falling back") {
		t.Errorf("message %q promises a fallback that does not happen", msg)
	}

	// The load-bearing property: the broken command never reaches the pane.
	if probe.sentKeys(t) {
		t.Errorf("a command was typed into the held session; invocations: %v", probe.invocations(t))
	}
	// Guard against a vacuous pass: the session-exists branch really ran.
	if logged := probe.invocations(t); !strings.Contains(strings.Join(logged, "\n"), "has-session") {
		t.Errorf("shim saw no has-session call, so the restart branch never ran: %v", logged)
	}
}

// The counterpart: with the referenced variable set, the same fixture restarts
// the agent in place. This pins that the hold above is the guard firing and not
// a fixture that fails to build a command at all.
func TestStartOrRestartCrewMember_RestartsWhenEnvResolves(t *testing.T) {
	probe := shimTmux(t)
	const setVar = "GT_TEST_CREW_SET_TOKEN"
	t.Setenv(setVar, "live-token")

	r, townRoot := restartFixture(t, "alice", setVar)
	tm := tmux.NewTmuxWithSocket("gt-test-crew-restart-ok")

	msg, started := startOrRestartCrewMember(tm, r, "alice", townRoot)

	if !started {
		t.Fatalf("started = false with the variable set: %q", msg)
	}
	if !strings.Contains(msg, "agent restarted") {
		t.Errorf("message %q does not report a restart", msg)
	}
	if !probe.sentKeys(t) {
		t.Errorf("no send-keys invocation; a restart typed nothing: %v", probe.invocations(t))
	}
}
