package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
)

// sandboxEndDelay is how long after the Stop hook the agent's tmux session is
// killed: long enough for the hook to return and the agent to print its summary.
const sandboxEndDelay = 10 * time.Second

// sandboxSessionEnd is what ending a finished sandbox polecat's session reads
// and does. The zero value is the real process.
type sandboxSessionEnd struct {
	// fields reads the polecat's agent bead; nil is the beads database of the
	// town (the sandbox reaches it over Dolt).
	fields func(townRoot, rig, polecat string) (*beads.AgentFields, error)
	// schedule kills the agent's tmux session after delay, detached from the
	// hook; nil starts a detached shell.
	schedule func(sessionName string, delay time.Duration) error
	// log receives the one line saying the session will end; nil is stderr.
	log io.Writer
}

// endSandboxSessionIfDone ends the agent's tmux session in a sandbox once gt done
// completed cleanly, so the sandbox's main process, which waits on that session,
// exits and the gateway deletes the sandbox. It reports whether it scheduled the
// end. A host session, a polecat that has not finished, and one that exited
// DEFERRED or ESCALATED, or still holds unpushed work, are left alone: the
// sandbox clone holds the only copy of that work.
func endSandboxSessionIfDone(e sandboxSessionEnd, getenv func(string) string, townRoot, rig, polecatName, sessionName string) bool {
	if getenv("GT_SESSION_BACKEND") != session.BackendOpenShell {
		return false
	}
	read := e.fields
	if read == nil {
		read = readAgentBeadFields
	}
	fields, err := read(townRoot, rig, polecatName)
	if err != nil || !sandboxPolecatFinishedCleanly(fields) {
		return false
	}
	schedule := e.schedule
	if schedule == nil {
		schedule = scheduleSandboxSessionKill
	}
	if err := schedule(sessionName, sandboxEndDelay); err != nil {
		w := e.log
		if w == nil {
			w = os.Stderr
		}
		fmt.Fprintf(w, "polecat-stop-check: could not schedule the end of session %s: %v\n", sessionName, err)
		return false
	}
	w := e.log
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, "polecat-stop-check: gt done completed and the branch is pushed; ending session %s in %s so the sandbox can be deleted\n", sessionName, sandboxEndDelay)
	return true
}

// sandboxPolecatFinishedCleanly is true only for a polecat whose gt done
// completed (agent_state done, hook cleared, exit COMPLETED when recorded) and
// whose own cleanup report says nothing is uncommitted, stashed or unpushed.
// Escalated and deferred exits record agent_state stuck, so they never match.
func sandboxPolecatFinishedCleanly(f *beads.AgentFields) bool {
	if f == nil {
		return false
	}
	return f.AgentState == string(beads.AgentStateDone) &&
		f.HookBead == "" &&
		f.CleanupStatus == string(polecat.CleanupClean) &&
		(f.ExitType == "" || f.ExitType == "COMPLETED")
}

func readAgentBeadFields(townRoot, rig, polecatName string) (*beads.AgentFields, error) {
	id := beads.AgentBeadIDFor(constants.RolePolecat, rig, polecatName, townRoot)
	if id == "" {
		return nil, fmt.Errorf("no agent bead id for %s/%s", rig, polecatName)
	}
	_, fields, err := beads.GetAgentBead(beads.New(townRoot), id)
	return fields, err
}

// scheduleSandboxSessionKill starts a shell, detached from this hook's process
// group, that waits delay and kills the agent's tmux session in the sandbox's
// tmux server. The session name is a positional argument, never spliced into
// the script.
func scheduleSandboxSessionKill(sessionName string, delay time.Duration) error {
	script := fmt.Sprintf(`sleep %d; tmux -L %s kill-session -t "$1"`, int(delay.Seconds()), session.SandboxTmuxSocket)
	cmd := exec.Command("sh", "-c", script, "sh", sessionName) //nolint:gosec // G204: fixed script, session name passed as an argument
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
