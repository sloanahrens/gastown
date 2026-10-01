package townstatus

import (
	"path/filepath"
	"sync"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
)

// discoverRigHooks finds all hook attachments for agents in a rig.
// It fetches all pinned handoff beads in a single bd call, then resolves
// each agent's hook in-memory. This replaces the previous N+1 pattern where
// each agent triggered a separate bd subprocess.
func discoverRigHooks(r *rig.Rig, crews []string) []AgentHookInfo {
	var hooks []AgentHookInfo

	// Create beads instance for the rig
	b := beads.New(r.Path)

	// Batch-fetch all handoff beads in one bd call
	allHandoffs, err := b.FindAllHandoffBeads()
	if err != nil {
		// On error, return empty hooks for all agents rather than failing
		allHandoffs = make(map[string]*beads.Issue)
	}

	// Check polecats
	for _, name := range r.Polecats {
		hooks = append(hooks, resolveHookFromMap(allHandoffs, name, r.Name+"/"+name, constants.RolePolecat))
	}

	// Check crew workers
	for _, name := range crews {
		hooks = append(hooks, resolveHookFromMap(allHandoffs, name, r.Name+"/crew/"+name, constants.RoleCrew))
	}

	return hooks
}

// resolveHookFromMap builds an AgentHookInfo from a pre-fetched map of handoff
// beads: the in-memory equivalent of one handoff lookup per agent, so a rig's
// hooks cost one bd call rather than one per agent.
func resolveHookFromMap(allHandoffs map[string]*beads.Issue, role, agentAddress, roleType string) AgentHookInfo {
	hook := AgentHookInfo{
		Agent: agentAddress,
		Role:  roleType,
	}

	handoff, ok := allHandoffs[role]
	if !ok || handoff == nil {
		return hook
	}

	attachment := beads.ParseAttachmentFields(handoff)
	if attachment != nil && attachment.AttachedMolecule != "" {
		hook.HasWork = true
		hook.Molecule = attachment.AttachedMolecule
		hook.Title = handoff.Title
	} else if handoff.Description != "" {
		hook.HasWork = true
		hook.Title = handoff.Title
	}

	return hook
}

// discoverGlobalAgents checks runtime state for town-level agents (the Mayor).
// Uses parallel fetching for performance. If skipMail is true, mail lookups are skipped.
// allSessions is a preloaded map of tmux sessions for O(1) lookup.
// allAgentBeads is a preloaded map of agent beads for O(1) lookup.
// allHookBeads is a preloaded map of hook beads for O(1) lookup.
func discoverGlobalAgents(townRoot string, allSessions map[string]bool, allAgentBeads map[string]*beads.Issue, allHookBeads map[string]*beads.Issue, mailRouter *mail.Router, skipMail bool) []AgentRuntime {
	// Get session names dynamically
	mayorSession := session.MayorSessionName()

	// Define agents to discover
	// Note: the Mayor is a town-level agent with an hq- prefix bead ID
	agentDefs := []struct {
		name    string
		address string
		session string
		role    string
		beadID  string
	}{
		{constants.RoleMayor, constants.RoleMayor + "/", mayorSession, "coordinator", beads.MayorBeadIDTown()},
	}

	// Batch-fetch mail summaries for all agents in one pair of bd calls
	// instead of one bd subprocess fan-out per agent (gt-978i).
	var mailSummaries map[string]mail.MailSummary
	if !skipMail && mailRouter != nil {
		addresses := make([]string, len(agentDefs))
		for i, d := range agentDefs {
			addresses[i] = d.address
		}
		mailSummaries, _ = mailRouter.BatchMailSummaries(addresses)
	}

	agents := make([]AgentRuntime, len(agentDefs))
	var wg sync.WaitGroup

	for i, def := range agentDefs {
		wg.Add(1)
		go func(idx int, d struct {
			name    string
			address string
			session string
			role    string
			beadID  string
		}) {
			defer wg.Done()

			agent := AgentRuntime{
				Name:    d.name,
				Address: d.address,
				Session: d.session,
				Role:    d.role,
			}

			// Check tmux session from preloaded map (O(1))
			agent.Running = allSessions[d.session]

			// Look up agent bead from preloaded map (O(1))
			if issue, ok := allAgentBeads[d.beadID]; ok {
				// Prefer database columns over description parsing
				// HookBead column is authoritative (cleared by unsling)
				agent.HookBead = issue.HookBead
				agent.State = beads.ResolveAgentState(issue.Description, issue.AgentState)
				if agent.HookBead != "" {
					agent.HasWork = true
					// Get hook title from preloaded map
					if pinnedIssue, ok := allHookBeads[agent.HookBead]; ok {
						agent.WorkTitle = pinnedIssue.Title
					}
				}
				// Parse description fields for notification level
				if fields := beads.ParseAgentFields(issue.Description); fields != nil {
					agent.NotificationLevel = fields.NotificationLevel
				}
			}

			// Apply pre-fetched mail summary (skip if --fast)
			if !skipMail {
				applyMailSummary(&agent, mailSummaries[agent.Address])
			}

			applyPauseMarker(&agent, townRoot)

			agents[idx] = agent
		}(i, def)
	}

	wg.Wait()
	return agents
}

// applyPauseMarker checks the pause marker file layer for this agent
// (gt-ahik) and records the reason on the AgentRuntime so the status
// line can show [paused (reason)]. File layer only — cheap, no Dolt.
func applyPauseMarker(agent *AgentRuntime, townRoot string) {
	rigName, role, name, ok := MarkerTriple(agent.Address)
	if !ok {
		return
	}
	if st := agentpause.PausedByState(townRoot, rigName, role, name); st != nil {
		agent.Paused = true
		agent.PausedReason = st.Reason
	}
}

// MarkerTriple maps an agent status address to the (rig, role, name)
// used by the pause marker path, and reports whether the address names a
// marker-backed agent at all.
//
// It goes through session.ParseAddressWithRegistry rather than splitting the
// string (with no registry: the triple carries no prefix), so
// every address form the rest of the system uses resolves to the same marker
// the pauser wrote: "rig/name" and "rig/polecats/name" (polecat),
// "rig/crew/name" — and the town-level "mayor/", whose marker lives at
// .runtime/agents/<role>.json,
// so they have an EMPTY rig rather than no marker (gt-wisp-6ajo).
func MarkerTriple(address string) (rig, role, name string, ok bool) {
	id, err := session.ParseAddressWithRegistry(address, nil)
	if err != nil {
		return "", "", "", false
	}
	return id.Rig, string(id.Role), id.Name, true
}

// applyMailSummary copies a pre-fetched batch mail summary onto an agent.
// Summaries come from Router.BatchMailSummaries, fetched once per set of
// agents rather than once per agent (gt-978i).
func applyMailSummary(agent *AgentRuntime, summary mail.MailSummary) {
	agent.UnreadMail = summary.UnreadCount
	agent.FirstSubject = summary.FirstSubject
}

// detectCurrentDNDStatus returns DND status for the currently resolved role context.

type agentDef struct {
	name    string
	address string
	session string
	role    string
	beadID  string
}

// discoverRigAgents checks runtime state for all agents in a rig.
// Uses parallel fetching for performance. If skipMail is true, mail lookups are skipped.
// allSessions is a preloaded map of tmux sessions for O(1) lookup.
// allAgentBeads is a preloaded map of agent beads for O(1) lookup.
// allHookBeads is a preloaded map of hook beads for O(1) lookup.
func discoverRigAgents(reg *session.PrefixRegistry, allSessions map[string]bool, r *rig.Rig, crews []string, allAgentBeads map[string]*beads.Issue, allHookBeads map[string]*beads.Issue, mailRouter *mail.Router, skipMail bool) []AgentRuntime {
	// Build list of all agents to discover
	var defs []agentDef
	townRoot := filepath.Dir(r.Path)
	prefix := beads.GetPrefixForRig(townRoot, r.Name)

	// Polecats
	for _, name := range r.Polecats {
		defs = append(defs, agentDef{
			name:    name,
			address: r.Name + "/" + name,
			session: session.PolecatSessionName(reg.PrefixForRig(r.Name), name),
			role:    constants.RolePolecat,
			beadID:  beads.PolecatBeadIDWithPrefix(prefix, r.Name, name),
		})
	}

	// Crew
	for _, name := range crews {
		defs = append(defs, agentDef{
			name:    name,
			address: r.Name + "/crew/" + name,
			session: session.CrewSessionName(reg.PrefixForRig(r.Name), name),
			role:    constants.RoleCrew,
			beadID:  beads.CrewBeadIDWithPrefix(prefix, r.Name, name),
		})
	}

	if len(defs) == 0 {
		return nil
	}

	// Batch-fetch mail summaries for all agents in this rig in one pair of
	// bd calls instead of one bd subprocess fan-out per agent (gt-978i).
	var mailSummaries map[string]mail.MailSummary
	if !skipMail && mailRouter != nil {
		addresses := make([]string, len(defs))
		for i, d := range defs {
			addresses[i] = d.address
		}
		mailSummaries, _ = mailRouter.BatchMailSummaries(addresses)
	}

	// Fetch all agents in parallel
	agents := make([]AgentRuntime, len(defs))
	var wg sync.WaitGroup

	for i, def := range defs {
		wg.Add(1)
		go func(idx int, d agentDef) {
			defer wg.Done()

			agent := AgentRuntime{
				Name:    d.name,
				Address: d.address,
				Session: d.session,
				Role:    d.role,
			}

			// Check tmux session from preloaded map (O(1))
			agent.Running = allSessions[d.session]

			// Look up agent bead from preloaded map (O(1))
			if issue, ok := allAgentBeads[d.beadID]; ok {
				// Prefer database columns over description parsing
				// HookBead column is authoritative (cleared by unsling)
				agent.HookBead = issue.HookBead
				agent.State = beads.ResolveAgentState(issue.Description, issue.AgentState)
				if agent.HookBead != "" {
					agent.HasWork = true
					// Get hook title from preloaded map
					if pinnedIssue, ok := allHookBeads[agent.HookBead]; ok {
						agent.WorkTitle = pinnedIssue.Title
					}
				}
				// Parse description fields for notification level
				if fields := beads.ParseAgentFields(issue.Description); fields != nil {
					agent.NotificationLevel = fields.NotificationLevel
				}
			}

			// Apply pre-fetched mail summary (skip if --fast)
			if !skipMail {
				applyMailSummary(&agent, mailSummaries[agent.Address])
			}

			applyPauseMarker(&agent, townRoot)

			agents[idx] = agent
		}(i, def)
	}

	wg.Wait()
	return agents
}
