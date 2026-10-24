package cmd

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
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
	// log receives the one line saying what happened; nil is stderr.
	log io.Writer
}

// logf writes one polecat-stop-check line to e.log.
func (e sandboxSessionEnd) logf(format string, args ...any) {
	w := e.log
	if w == nil {
		w = os.Stderr
	}
	fmt.Fprintf(w, "polecat-stop-check: "+format+"\n", args...)
}

// endSandboxSessionIfDone ends the agent's tmux session in a sandbox once gt done
// completed cleanly, so the sandbox's main process, which waits on that session,
// exits and the gateway deletes the sandbox. It reports whether it scheduled the
// end. A host session, a polecat that has not finished, or one that still holds
// unpushed work, is left alone: the sandbox clone holds the only copy of that
// work. A sandbox session it leaves is logged with the reason, so a sandbox that
// stays up can be explained.
func endSandboxSessionIfDone(e sandboxSessionEnd, getenv func(string) string, townRoot, rig, polecatName, sessionName string) bool {
	if getenv("GT_SESSION_BACKEND") != session.BackendOpenShell {
		return false
	}
	read := e.fields
	if read == nil {
		read = readAgentBeadFields
	}
	fields, err := read(townRoot, rig, polecatName)
	if err != nil {
		e.logf("leaving session %s: could not read the agent bead of %s/%s: %v", sessionName, rig, polecatName, err)
		return false
	}
	if why := sandboxPolecatNotFinished(fields); why != "" {
		e.logf("leaving session %s: %s", sessionName, why)
		return false
	}
	schedule := e.schedule
	if schedule == nil {
		schedule = scheduleSandboxSessionKill
	}
	if err := schedule(sessionName, sandboxEndDelay); err != nil {
		e.logf("could not schedule the end of session %s: %v", sessionName, err)
		return false
	}
	e.logf("gt done completed and the branch is pushed; ending session %s in %s so the sandbox can be deleted", sessionName, sandboxEndDelay)
	return true
}

// finalStuckExit reports whether f is the record gt done writes for a final
// DEFERRED or ESCALATED exit. The work bead stays hooked for the resume, so the
// daemon never deletes the sandbox; ending the session is what frees the seat
// (gt-e0pq6).
func finalStuckExit(f *beads.AgentFields) bool {
	return f.AgentState == string(beads.AgentStateStuck) && (f.ExitType == "DEFERRED" || f.ExitType == "ESCALATED")
}

// sandboxPolecatNotFinished returns why a polecat's session may not end, or ""
// when it may: its gt done finished (completed, or a final DEFERRED or ESCALATED
// exit) with the bead unhooked and the cleanup report clean.
func sandboxPolecatNotFinished(f *beads.AgentFields) string {
	switch {
	case f == nil:
		return "no agent bead"
	case f.AgentState != string(beads.AgentStateDone) && !finalStuckExit(f):
		return fmt.Sprintf("agent_state is %q, not done", f.AgentState)
	case f.HookBead != "":
		return fmt.Sprintf("hook_bead %s is still set", f.HookBead)
	case f.CleanupStatus != string(polecat.CleanupClean):
		return fmt.Sprintf("cleanup_status is %q, not clean, and the sandbox clone may hold the only copy of the work", f.CleanupStatus)
	case !finalStuckExit(f) && f.ExitType != "" && f.ExitType != "COMPLETED":
		return fmt.Sprintf("exit_type is %s, not COMPLETED", f.ExitType)
	}
	return ""
}

func readAgentBeadFields(townRoot, rig, polecatName string) (*beads.AgentFields, error) {
	id := beads.AgentBeadIDFor(constants.RolePolecat, rig, polecatName, townRoot)
	if id == "" {
		return nil, fmt.Errorf("no agent bead id for %s/%s", rig, polecatName)
	}
	_, fields, err := beads.GetAgentBead(beads.New(townRoot), id)
	return fields, err
}

// sandboxKillScript waits "$2" seconds, then kills session "$1" in the sandbox's
// tmux server. Both are positional arguments, never spliced into the script.
const sandboxKillScript = `sleep "$2"; tmux -L ` + session.SandboxTmuxSocket + ` kill-session -t "$1"`

// sandboxKillCommand is the shell that ends sessionName after delay.
func sandboxKillCommand(sessionName string, delay time.Duration) *exec.Cmd {
	secs := strconv.FormatFloat(delay.Seconds(), 'f', -1, 64)
	cmd := exec.Command("sh", "-c", sandboxKillScript, "sh", sessionName, secs) //nolint:gosec // G204: fixed script, values passed as arguments
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}                        // its own session, so the hook's exit does not take it down
	return cmd
}

// scheduleSandboxSessionKill starts that shell detached from this hook and does
// not wait for it.
func scheduleSandboxSessionKill(sessionName string, delay time.Duration) error {
	cmd := sandboxKillCommand(sessionName, delay)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
