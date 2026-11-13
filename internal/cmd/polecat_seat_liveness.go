package cmd

import (
	"strings"

	"github.com/steveyegge/gastown/internal/polecat"
)

// liveSeatCounter answers the cap's question about the polecat directory name:
// does it hold a live seat? True counts the directory against the cap.
type liveSeatCounter func(polecatName string) bool

// newLiveSeatCounter builds the cap's seat question for one rig. mgr is the
// polecat manager the sling already opened; the sling path never passes nil
// (openSlingRig fails instead), and a nil manager counts the seat rather than
// panicking. The agent-bead read goes through mgr, so the sling's open memo
// answers a bead the seat listing already read (gt-atlmw2); with no memo open
// it reads bd as before. agentBeadID names the directory's agent bead,
// sessionName its session (which is also its sandbox's name), and probe tells
// a sandbox seat from a host seat and asks the gateway whether that sandbox
// still exists.
//
// A directory counts unless every read agrees it is a sandbox seat whose
// sandbox is gone: no agent bead, an unreadable bead, a seat the probe cannot
// classify, and a gateway that did not answer all count. Only the gateway's
// affirmative "gone" (polecat.SandboxSeatGitState's live source) does not: a
// sandbox seat's directory is a seed worktree the sandbox never writes to, so
// nothing live is left behind it (gt-xlazq).
func newLiveSeatCounter(mgr *polecat.Manager, agentBeadID func(polecatName string) string,
	sessionName func(polecatName string) string, probe sandboxSeatProbe) liveSeatCounter {
	return func(name string) bool {
		if mgr == nil || probe == nil {
			return true
		}
		_, fields, err := mgr.ReadAgentBead(agentBeadID(name))
		if err != nil || fields == nil {
			return true
		}
		state, sandboxed := probe(strings.TrimSpace(fields.Agent), sessionName(name))
		if !sandboxed {
			return true // a host seat is its directory
		}
		return state.Source != polecat.GitStateSourceLive
	}
}
