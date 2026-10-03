// Package townstatus holds the town's runtime status model and the code that
// gathers it. The cobra command in internal/cmd binds flags, calls Gather and
// prints (gt-638go.10).
//
// It is a leaf package, so any in-process caller can share the model; it
// imports no command and no daemon. It is separate from internal/townhealth,
// which owns the health verdict the daemon writes every tick: that package's
// Settings type is embedded in internal/config, so a status gatherer living
// there could not import config.
package townstatus

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/crew"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Options are Gather's inputs: what to read, plus the two answers that come
// from the caller rather than the town.
type Options struct {
	// TownRoot is the workspace root. Empty means find it from the working
	// directory.
	TownRoot string
	// Fast skips the beads prefetch and the mail lookups, rendering
	// runtime-only status. Gather also turns it on by itself when another
	// status run holds the detail lock.
	Fast bool
	// Registry names sessions and rig prefixes. Nil is the default registry.
	Registry *session.PrefixRegistry
	// DaemonProbe reports whether the daemon is running and its PID. The
	// daemon package owns that answer and has to stay free to import this
	// one, so the caller supplies the probe — cmd passes daemon.IsRunning.
	// Nil leaves Daemon unset.
	DaemonProbe func(townRoot string) (bool, int, error)
	// DNDProbe reads the invoking agent's notification state from townRoot,
	// nil outside an agent context. DND describes the caller, not the town,
	// so the caller supplies it — cmd passes detectCurrentDNDStatus. Nil
	// leaves DND unset.
	DNDProbe func(townRoot string) *DNDInfo
}

// Gather reads the town's runtime status: the registered rigs and their
// agents, the town-level agents, and the daemon, Dolt, tmux, gate-slot and
// health probes beside them. Everything it returns is data; printing is the
// caller's job.
func Gather(opts Options) (TownStatus, error) {
	townRoot := opts.TownRoot
	if townRoot == "" {
		found, err := workspace.FindFromCwdOrError()
		if err != nil {
			return TownStatus{}, fmt.Errorf("not in a Gas Town workspace: %w", err)
		}
		townRoot = found
	}
	reg := opts.Registry
	if reg == nil {
		reg = session.DefaultRegistry()
	}

	fast := opts.Fast
	skipBeadsPrefetch := false
	if !fast {
		if release, ok := tryDetailLock(townRoot); ok {
			defer release()
		} else {
			fast = true
			skipBeadsPrefetch = true
		}
	}

	// Load town config
	townConfigPath := constants.MayorTownPath(townRoot)
	townConfig, err := config.LoadTownConfig(townConfigPath)
	if err != nil {
		// Try to continue without config
		townConfig = &config.TownConfig{Name: filepath.Base(townRoot)}
	}

	// Load rigs config
	rigsConfigPath := constants.MayorRigsPath(townRoot)
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		// Empty config if file doesn't exist
		rigsConfig = &config.RigsConfig{Rigs: make(map[string]config.RigEntry)}
	}

	// Load town settings for agent display info
	townSettings, _ := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot))

	// Create rig manager
	g := git.NewGit(townRoot)
	mgr := rig.NewManager(townRoot, rigsConfig, g)

	// Create tmux instance for runtime checks
	t := tmux.NewTmux()

	// Pre-fetch all tmux sessions and verify agent liveness for O(1) lookup.
	// A Gas Town session is only considered "running" if the agent process is
	// alive inside it, not merely if the tmux session exists. This prevents
	// zombie sessions (tmux alive, agent dead) from showing as running.
	// See: gt-bd6i3
	allSessions := make(map[string]bool)
	var livenessUnknown []string
	if sessions, err := t.ListSessions(); err == nil {
		var sessionMu sync.Mutex
		var sessionWg sync.WaitGroup
		for _, s := range sessions {
			if reg.IsKnownSession(s) {
				sessionWg.Add(1)
				go func(name string) {
					defer sessionWg.Done()
					// A failed liveness query is unknown, not dead: show
					// the session as present rather than hide it (gt-fcxe9.1).
					alive, aliveErr := t.IsAgentAliveChecked(name)
					sessionMu.Lock()
					if aliveErr != nil {
						alive = true
						livenessUnknown = append(livenessUnknown, name)
					}
					allSessions[name] = alive
					sessionMu.Unlock()
				}(s)
			} else {
				allSessions[s] = true
			}
		}
		sessionWg.Wait()
	}

	// Discover rigs
	rigs, err := mgr.DiscoverRigs()
	if err != nil {
		return TownStatus{}, fmt.Errorf("discovering rigs: %w", err)
	}

	// Pre-fetch agent beads across all rig-specific beads DBs. If another status
	// process already holds the detail lock, skip this Dolt-heavy section and
	// render runtime-only status instead of amplifying the query storm.
	allAgentBeads := make(map[string]*beads.Issue)
	allHookBeads := make(map[string]*beads.Issue)
	var beadsMu sync.Mutex // Protects allAgentBeads and allHookBeads

	// Helper to safely merge beads into the shared maps
	mergeAgentBeads := func(beadsMap map[string]*beads.Issue) {
		beadsMu.Lock()
		for id, issue := range beadsMap {
			allAgentBeads[id] = issue
		}
		beadsMu.Unlock()
	}
	mergeHookBeads := func(beadsMap map[string]*beads.Issue) {
		beadsMu.Lock()
		for id, issue := range beadsMap {
			allHookBeads[id] = issue
		}
		beadsMu.Unlock()
	}

	if !skipBeadsPrefetch {
		var beadsWg sync.WaitGroup

		// Fetch town-level agent beads (the Mayor) from town beads
		townBeadsPath := beads.GetTownBeadsPath(townRoot)
		beadsWg.Add(1)
		go func() {
			defer beadsWg.Done()
			townBeadsClient := beads.New(townBeadsPath)
			townAgentBeads, _ := beads.ListAgentBeads(townBeadsClient)
			mergeAgentBeads(townAgentBeads)

			// Fetch hook beads from town beads
			var townHookIDs []string
			for _, issue := range townAgentBeads {
				hookID := issue.HookBead
				if hookID == "" {
					fields := beads.ParseAgentFields(issue.Description)
					if fields != nil {
						hookID = fields.HookBead
					}
				}
				if hookID != "" {
					townHookIDs = append(townHookIDs, hookID)
				}
			}
			if len(townHookIDs) > 0 {
				townHookBeads, _ := townBeadsClient.ShowMultiple(townHookIDs)
				mergeHookBeads(townHookBeads)
			}
		}()

		// Fetch rig-level agent beads in parallel
		for _, r := range rigs {
			beadsWg.Add(1)
			go func(r *rig.Rig) {
				defer beadsWg.Done()
				rigBeadsPath := filepath.Join(r.Path, "mayor", "rig")
				rigBeads := beads.New(rigBeadsPath)
				rigAgentBeads, _ := beads.ListAgentBeads(rigBeads)
				if rigAgentBeads == nil {
					return
				}
				mergeAgentBeads(rigAgentBeads)

				var hookIDs []string
				for _, issue := range rigAgentBeads {
					// Use the HookBead field from the database column; fall back for legacy beads.
					hookID := issue.HookBead
					if hookID == "" {
						fields := beads.ParseAgentFields(issue.Description)
						if fields != nil {
							hookID = fields.HookBead
						}
					}
					if hookID != "" {
						hookIDs = append(hookIDs, hookID)
					}
				}

				if len(hookIDs) == 0 {
					return
				}
				hookBeads, _ := rigBeads.ShowMultiple(hookIDs)
				mergeHookBeads(hookBeads)
			}(r)
		}

		beadsWg.Wait()
	}

	// Create mail router for inbox lookups
	mailRouter := mail.NewRouter(townRoot, reg)

	// Load overseer config
	var overseerInfo *OverseerInfo
	if overseerConfig, err := config.LoadOrDetectOverseer(townRoot); err == nil && overseerConfig != nil {
		overseerInfo = &OverseerInfo{
			Name:     overseerConfig.Name,
			Email:    overseerConfig.Email,
			Username: overseerConfig.Username,
			Source:   overseerConfig.Source,
		}
		// Get overseer mail count (skip in --fast mode)
		if !fast {
			if mailbox, err := mailRouter.GetMailbox("overseer"); err == nil {
				_, unread, _ := mailbox.Count()
				overseerInfo.UnreadMail = unread
			}
		}
	}

	// Build status - parallel fetch global agents and rigs
	var dnd *DNDInfo
	if opts.DNDProbe != nil {
		dnd = opts.DNDProbe(townRoot)
	}
	status := TownStatus{
		Name:     townConfig.Name,
		Location: townRoot,
		Overseer: overseerInfo,
		DND:      dnd,
		Rigs:     make([]RigStatus, len(rigs)),
	}
	status.HealthLines, _, status.Health = HealthView(townRoot, time.Now())

	// Daemon status
	if opts.DaemonProbe != nil {
		if daemonRunning, daemonPid, err := opts.DaemonProbe(townRoot); err == nil {
			status.Daemon = &ServiceInfo{Running: daemonRunning, PID: daemonPid}
		}
	}

	// Dolt status
	doltCfg := doltserver.DefaultConfig(townRoot)
	if doltCfg.IsRemote() {
		status.Dolt = &DoltInfo{Remote: true, Port: doltCfg.Port}
	} else {
		doltRunning, doltPid, _ := doltserver.IsRunning(townRoot)
		port := doltCfg.Port
		if doltRunning {
			// Report the port the running server was started on, which
			// can differ from the configured one until Dolt restarts.
			if state, err := doltserver.LoadState(townRoot); err == nil && state.Port > 0 {
				port = state.Port
			}
		}
		doltInfo := &DoltInfo{
			Running: doltRunning,
			PID:     doltPid,
			Port:    port,
			DataDir: doltCfg.DataDir,
		}
		// Check if port is held by another town's Dolt
		if !doltRunning {
			if conflictPid, conflictDir := doltserver.CheckPortConflict(townRoot); conflictPid > 0 {
				doltInfo.PortConflict = true
				doltInfo.ConflictOwner = conflictDir
			}
		}
		status.Dolt = doltInfo
	}
	if !fast && (status.Dolt.Remote || status.Dolt.Running) {
		readDoltCommitMeter(status.Dolt, townRoot, doltserver.CommitsLastDay)
	}

	// Tmux status
	socket := tmux.GetDefaultSocket()
	socketLabel := "default"
	if socket != "" {
		socketLabel = socket
	}
	tmuxInfo := &TmuxInfo{
		Socket:       socketLabel,
		SessionCount: len(allSessions),
		Running:      len(allSessions) > 0,
	}
	// Resolve socket path: /tmp/tmux-<UID>/<socket>
	tmuxInfo.SocketPath = filepath.Join(tmux.SocketDir(), socketLabel)
	if _, err := os.Stat(tmuxInfo.SocketPath); err == nil {
		tmuxInfo.Running = true
		tmuxInfo.PID = tmux.NewTmux().ServerPID()
	}
	status.Tmux = tmuxInfo

	// Container-suite gate slot: only show a line when something is
	// actually holding it (gt-bcsq). Best-effort — a lock-read failure
	// shouldn't break 'gt status'.
	status.Slot = gateSlotHolder(townRoot)

	sort.Strings(livenessUnknown)
	status.LivenessUnknown = livenessUnknown

	var wg sync.WaitGroup

	// Fetch global agents in parallel with rig discovery
	wg.Add(1)
	go func() {
		defer wg.Done()
		status.Agents = discoverGlobalAgents(townRoot, allSessions, allAgentBeads, allHookBeads, mailRouter, fast)
	}()

	// Process all rigs in parallel
	rigActiveHooks := make([]int, len(rigs)) // Track hooks per rig for thread safety
	for i, r := range rigs {
		wg.Add(1)
		go func(idx int, r *rig.Rig) {
			defer wg.Done()

			rs := RigStatus{
				Name:         r.Name,
				Polecats:     r.Polecats,
				PolecatCount: len(r.Polecats),
			}

			// Count crew workers
			crewGit := git.NewGit(r.Path)
			crewMgr := crew.NewManager(r, crewGit, reg)
			if workers, err := crewMgr.List(); err == nil {
				for _, w := range workers {
					rs.Crews = append(rs.Crews, w.Name)
				}
				rs.CrewCount = len(workers)
			}

			// Run hooks and agents discovery concurrently within this rig.
			// Each was previously sequential; now they overlap since they use
			// independent bd/beads calls.
			var rigWg sync.WaitGroup

			// Discover hooks for all agents in this rig
			// In --fast mode, skip expensive handoff bead lookups. Hook info comes from
			// preloaded agent beads via discoverRigAgents instead.
			if !fast {
				rigWg.Add(1)
				go func() {
					defer rigWg.Done()
					rs.Hooks = discoverRigHooks(r, rs.Crews)
				}()
			}

			// Discover runtime state for all agents in this rig
			// (uses preloaded maps, so it's fast — but run concurrently with hooks)
			rigWg.Add(1)
			go func() {
				defer rigWg.Done()
				rs.Agents = discoverRigAgents(reg, allSessions, r, rs.Crews, allAgentBeads, allHookBeads, mailRouter, fast)
			}()

			rigWg.Wait()

			activeHooks := 0
			for _, hook := range rs.Hooks {
				if hook.HasWork {
					activeHooks++
				}
			}
			rigActiveHooks[idx] = activeHooks

			status.Rigs[idx] = rs
		}(i, r)
	}

	wg.Wait()

	// Enrich agents with runtime info — inspect actual running processes
	for i := range status.Agents {
		a := &status.Agents[i]
		alias, info := resolveAgentDisplay(townSettings, a.Role, a.Session, a.Running)
		a.AgentAlias = alias
		a.AgentInfo = info
	}
	for i := range status.Rigs {
		for j := range status.Rigs[i].Agents {
			a := &status.Rigs[i].Agents[j]
			alias, info := resolveAgentDisplay(townSettings, a.Role, a.Session, a.Running)
			a.AgentAlias = alias
			a.AgentInfo = info
		}
	}

	// Aggregate summary (after parallel work completes)
	for i, rs := range status.Rigs {
		status.Summary.PolecatCount += rs.PolecatCount
		status.Summary.CrewCount += rs.CrewCount
		status.Summary.ActiveHooks += rigActiveHooks[i]
	}
	status.Summary.RigCount = len(rigs)

	return status, nil
}

// CountRunningAgents returns how many agents in s have a running session,
// counting the town-level agents and every rig's.
func CountRunningAgents(s TownStatus) int {
	count := 0
	for _, a := range s.Agents {
		if a.Running {
			count++
		}
	}
	for _, r := range s.Rigs {
		for _, a := range r.Agents {
			if a.Running {
				count++
			}
		}
	}
	return count
}
