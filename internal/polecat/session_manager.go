// Package polecat provides polecat workspace and session management.
package polecat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/bdgate"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/polecat/spans"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/runtime"
	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/seattoken"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
)

// reportBranchRepairFailure makes a session-branch repair that did not run
// visible in two places, because neither one reaches every path that starts a
// session. The stderr warning reaches an operator running gt session start or
// gt sling. The feed event reaches an operator watching a witness-driven
// restart: that path is `gt session restart` under util.ExecRun, which keeps
// stderr only when the command exits non-zero, so a repair that fails while
// the session still starts would otherwise leave no trace at all (gt-ns8t).
func reportBranchRepairFailure(townRoot, rigName, polecat, step string, err error) {
	style.PrintWarning("polecat %s/%s: session branch repair did not run (%s): %v", rigName, polecat, step, err)
	_ = events.LogFeedTo(townRoot, events.TypePolecatBranchRepairFailed, events.ActorGt, map[string]interface{}{
		"rig":     rigName,
		"polecat": polecat,
		"step":    step,
		"error":   err.Error(),
	})
}

// Session errors
var (
	ErrSessionRunning  = errors.New("session already running")
	ErrSessionNotFound = errors.New("session not found")
	ErrIssueInvalid    = errors.New("issue not found or tombstoned")

	// ErrSandboxNotRemoved is what Stop wraps when the session is already
	// stopped but its sandbox could not be removed, or its registry record could
	// not be read. The stop itself succeeded: a caller must not retry it (the
	// retry gets ErrSessionNotFound), only report that the sandbox may remain.
	ErrSandboxNotRemoved = errors.New("session stopped, sandbox not removed")

	// ErrBaseBranchRepair reports the state the fresh-branch repair exists to
	// prevent: a worktree on the canonical base branch that the repair could
	// not move off it. Starting a session here is what mis-bases the work a
	// polecat later submits (gt-032w), so callers fail the start instead of
	// proceeding on the base branch (gt-ns8t).
	ErrBaseBranchRepair = errors.New("polecat worktree left on the base branch")
)

// SessionManager handles polecat session lifecycle.
type SessionManager struct {
	tmux sessionTmux
	rig  *rig.Rig
	// gits opens git on the polecat's worktree; its zero value opens
	// *git.Git.
	gits gitOpener
	// prefixes resolves the rig's session prefix; nil gives
	// session.DefaultPrefix.
	prefixes *session.PrefixRegistry
	// hooks route the kills and replacements through the supervisor
	// (gt-4k3fj.4.1); a nil hook goes straight to tmux.
	hooks SessionHooks
	// beadsAt opens the beads database in a resolved beads dir; nil is
	// beads.NewPinned.
	beadsAt func(beadsDir string) beads.Client
	// sandboxDeleter stands in for the linked openshell deleter in the
	// cleanup an abandoned launch runs, so a test never shells out to the
	// sandbox CLI; nil means the linked one.
	sandboxDeleter func(townRoot string, rec session.SandboxRecord) error
	// sandboxPusher stands in for the linked openshell pusher a stop runs
	// before it deletes a sandbox, so a test never shells out to the sandbox
	// CLI; nil means the linked one.
	sandboxPusher func(townRoot string, rec session.SandboxRecord) error
	// sandboxExists asks a gateway whether it has a sandbox of that name when
	// nil: a seam for the unit tier, which has no gateway.
	sandboxExists func(gateway, name string) (bool, error)
	// launchFn replaces launch when set: a seam so a test can see what Start
	// has done by the time it launches.
	launchFn func(polecat, sessionID string, opts SessionStartOptions) error
	// clk stamps a stopped seat's span end; nil is the real clock.
	clk clockwork.Clock
	// warn reports a span end that could not be written; nil is
	// style.PrintWarning.
	warn func(format string, args ...any)
	// seatTokenIssuer mints the token a tracker-rig seat does not have yet;
	// nil is the host issuer, built from the town's admin credential.
	seatTokenIssuer seattoken.Issuer
	// seatTokenTimeout bounds one token issue; zero is seatTokenIssueTimeout.
	seatTokenTimeout time.Duration
	// hostSeatIssuer builds the issuer a launch uses when seatTokenIssuer is
	// nil; nil is seattoken.NewHostIssuer, which reads the admin credential
	// through the process environment, so a test that must not depend on the
	// shell supplies its own.
	hostSeatIssuer func(townRoot string) (seattoken.Issuer, error)
}

// The two variables a seat on a tracker rig carries. The tracker client reads
// them (tracker/client.FromEnv); neither holds the token itself.
const (
	envTrackerURL       = "TRACKER_URL"
	envTrackerTokenFile = "TRACKER_TOKEN_FILE"
)

// seatTokenIssueTimeout bounds the launch's call for a seat token, so a
// tracker that accepts the connection and then stalls cannot hold a launch
// open.
const seatTokenIssueTimeout = 30 * time.Second

// SessionHooks route a SessionManager's kills through the supervisor
// (gt-4k3fj.4.1). Each nil hook kills straight through tmux.
type SessionHooks struct {
	// Stop ends a session in Stop: the supervisor's logged operator stop,
	// set by the operator verbs that end a polecat (gt down, gt session
	// stop, gt polecat nuke).
	Stop func(sessionID string) error
	// Cleanup ends a session Start just created and is abandoning because
	// its startup failed: the supervisor's unrefused, logged cleanup.
	Cleanup func(polecat, reason string) error
	// Respawn runs Start's replacement of a session whose agent exited, so
	// an e-stop, a park or a gt down in progress refuses it.
	Respawn func(polecat, reason string, run func() error) error
}

// SetHooks routes Stop's kill, Start's startup-failure kills and Start's
// replacement of a dead session through h.
func (m *SessionManager) SetHooks(h SessionHooks) {
	m.hooks = h
}

// cleanup kills a session Start created and is abandoning.
func (m *SessionManager) cleanup(polecat, sessionID, reason string) {
	if m.hooks.Cleanup != nil {
		_ = m.hooks.Cleanup(polecat, reason)
		return
	}
	_ = m.tmux.KillSessionWithProcesses(sessionID)
}

// sessionTmux is the tmux surface a SessionManager drives. *tmux.Tmux
// provides it; tests pass tmuxfake with the few calls it lacks added.
type sessionTmux interface {
	startupNudgeTmux
	sessionProbe
	NewSessionWithCommandAndEnv(name, workDir, command string, env map[string]string) error
	ListSessions() ([]string, error)
	GetSessionInfo(name string) (*tmux.SessionInfo, error)
	GetPaneID(session string) (string, error)
	SetEnvironment(session, key, value string) error
	GetEnvironment(session, key string) (string, error)
	ConfigureGasTownSession(session string, theme *tmux.Theme, rig, worker, role string) error
	SetPaneDiedHook(session, agentID string) error
	WaitForCommand(session string, excludeCommands []string, timeout time.Duration) error
	WaitForRuntimeReady(session string, rc *config.RuntimeConfig, timeout time.Duration) error
	AcceptStartupDialogs(session string) error
	CheckStartupBlocked(session string) error
	CheckSessionHealth(session string, maxInactivity time.Duration) tmux.ZombieStatus
	SendKeysRaw(session, keys string) error
	SendKeysDebounced(session, keys string, debounceMs int) error
	CapturePane(session string, lines int) (string, error)
	AttachSession(session string) error
}

var _ sessionTmux = (*tmux.Tmux)(nil)

// NewSessionManager creates a new polecat session manager for a rig;
// prefixes names its sessions.
func NewSessionManager(t *tmux.Tmux, r *rig.Rig, prefixes *session.PrefixRegistry) *SessionManager {
	m := &SessionManager{rig: r, prefixes: prefixes}
	if t != nil {
		m.tmux = t
	}
	return m
}

// polecatAgentEnvConfig is the AgentEnv input for a polecat's session. The
// resolved agent's own GOFLAGS rides along so the seat's -p cap follows what
// the agent env says instead of replacing it (a session backend applies this
// map after the agent preset env).
func polecatAgentEnvConfig(rig, polecat, townRoot, sessionID string, opts SessionStartOptions, rc *config.RuntimeConfig) config.AgentEnvConfig {
	cfg := config.AgentEnvConfig{
		Role:             "polecat",
		Rig:              rig,
		AgentName:        polecat,
		TownRoot:         townRoot,
		RuntimeConfigDir: opts.RuntimeConfigDir,
		Agent:            opts.Agent,
		SessionName:      sessionID,
	}
	if rc != nil {
		cfg.AgentGOFLAGS = rc.Env["GOFLAGS"]
	}
	return cfg
}

// SessionStartOptions configures polecat session startup.
type SessionStartOptions struct {
	// WorkDir overrides the default working directory (polecat clone dir).
	WorkDir string

	// Backend hosts the session (session.Resolve). Nil or the tmux backend
	// is today's host tmux session; the openshell backend prepares a
	// sandbox first and the session is created inside it through the
	// routed tmux client.
	Backend session.Backend

	// Trace times the sandbox phases of this start for the sling's log line.
	// Nil, or a tmux start, times nothing.
	Trace *session.SandboxTrace

	// Issue is an optional issue ID to work on.
	Issue string

	// Command overrides the default "claude" command.
	Command string

	// Account specifies the account handle to use (overrides default).
	Account string

	// RuntimeConfigDir is resolved config directory for the runtime account.
	// If set, this is injected as an environment variable.
	RuntimeConfigDir string

	// Agent is the agent override for this polecat session (e.g., "claude-haiku").
	// If set, GT_AGENT is written to the tmux session environment table so that
	// IsAgentAliveChecked and waitForPolecatReady read the correct process names.
	Agent string
}

// SessionInfo contains information about a running polecat session.
type SessionInfo struct {
	// Polecat is the polecat name.
	Polecat string `json:"polecat"`

	// SessionID is the tmux session identifier.
	SessionID string `json:"session_id"`

	// Running indicates if the session is currently active.
	Running bool `json:"running"`

	// RigName is the rig this session belongs to.
	RigName string `json:"rig_name"`

	// Attached indicates if someone is attached to the session.
	Attached bool `json:"attached,omitempty"`

	// Created is when the session was created.
	Created time.Time `json:"created,omitempty"`

	// Windows is the number of tmux windows.
	Windows int `json:"windows,omitempty"`

	// LastActivity is when the session last had activity.
	LastActivity time.Time `json:"last_activity,omitempty"`
}

// SessionName generates the tmux session name for a polecat.
// Validates that the polecat name doesn't contain the rig prefix to prevent
// double-prefix bugs (e.g., "gt-gastown_manager-gastown_manager-142").
func (m *SessionManager) SessionName(polecat string) string {
	prefix := m.prefix()
	sessionName := session.PolecatSessionName(prefix, polecat)

	// Validate session name format to detect double-prefix bugs
	if err := validateSessionName(sessionName, prefix, m.rig.Name); err != nil {
		// Log warning but don't fail - allow the session to be created
		// so we can track and clean up malformed sessions later
		fmt.Fprintf(os.Stderr, "Warning: malformed session name: %v\n", err)
	}

	return sessionName
}

// prefix returns the rig's session prefix.
func (m *SessionManager) prefix() string {
	return m.prefixes.PrefixForRig(m.rig.Name)
}

// validateSessionName checks for double-prefix session names.
// Returns an error if the session name has the rig prefix duplicated.
// Example bad name: "gt-gastown_manager-gastown_manager-142"
func validateSessionName(sessionName, rigPrefix, rigName string) error {
	// Expected format: gt-<rig>-<name>
	// Check if the name part starts with the rig prefix (indicates double-prefix bug)
	prefix := rigPrefix + "-"
	if !strings.HasPrefix(sessionName, prefix) {
		return nil // Not our rig, can't validate
	}

	namePart := strings.TrimPrefix(sessionName, prefix)

	// Check if name part starts with rig name followed by hyphen
	// This indicates overflow name included rig prefix: gt-<rig>-<rig>-N
	if strings.HasPrefix(namePart, rigName+"-") {
		return fmt.Errorf("double-prefix detected: %s (expected format: gt-%s-<name>)",
			sessionName, rigName)
	}

	return nil
}

// polecatDir returns the parent directory for a polecat.
// This is polecats/<name>/ - the polecat's home directory.
func (m *SessionManager) polecatDir(polecat string) string {
	return filepath.Join(m.rig.Path, "polecats", polecat)
}

// clonePath returns the path where the git worktree lives.
// New structure: polecats/<name>/<rigname>/ - gives LLMs recognizable repo context.
// Falls back to old structure: polecats/<name>/ for backward compatibility.
func (m *SessionManager) clonePath(polecat string) string {
	// New structure: polecats/<name>/<rigname>/
	newPath := filepath.Join(m.rig.Path, "polecats", polecat, m.rig.Name)
	if info, err := os.Stat(newPath); err == nil && info.IsDir() {
		return newPath
	}

	// Old structure: polecats/<name>/ (backward compat)
	oldPath := filepath.Join(m.rig.Path, "polecats", polecat)
	if info, err := os.Stat(oldPath); err == nil && info.IsDir() {
		// Check if this is actually a git worktree (has .git file or dir)
		gitPath := filepath.Join(oldPath, ".git")
		if _, err := os.Stat(gitPath); err == nil {
			return oldPath
		}
	}

	// Default to new structure for new polecats
	return newPath
}

// freshBranchName returns a unique branch name for a new polecat session.
// Mirrors the naming convention in Manager.buildBranchName:
//   - polecat/<name>/<issue>+<timestamp> when an issue is known
//   - polecat/<name>-<timestamp> otherwise
//
// parseFreshBranchName is the structural inverse.
func (m *SessionManager) freshBranchName(polecatName, issue string) string {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 36)
	return FormatGeneratedBranchName(polecatName, issue, ts)
}

// freshBranchMeta holds the identity decoded from a branch produced by
// freshBranchName. ok=false means the branch does not match either format.
type freshBranchMeta struct {
	polecat string
	issue   string // empty when the branch has no issue binding
	ok      bool
}

// parseFreshBranchName is the structural inverse of freshBranchName. It
// does not consult git or the filesystem; it recognizes the two formats
// the formatter emits. Used in place of substring heuristics so that
// branch-naming changes can be made in a single place.
func parseFreshBranchName(branch string) freshBranchMeta {
	meta, ok := ParseGeneratedBranchName(branch)
	if !ok {
		return freshBranchMeta{}
	}
	return freshBranchMeta{polecat: meta.Polecat, issue: meta.Issue, ok: true}
}

func (m *SessionManager) canonicalSessionStartPoint(g gitRepo) string {
	defaultBranch := ""
	rigCfg, cfgErr := rig.LoadRigConfigIfPresent(m.rig.Path)
	if cfgErr != nil {
		rig.WarnRigConfigOnce(m.rig.Path, cfgErr)
	}
	if rigCfg != nil {
		defaultBranch = rigCfg.DefaultBranch
	}
	if defaultBranch == "" {
		defaultBranch = g.RemoteDefaultBranch()
	}
	if defaultBranch == "" {
		return ""
	}
	return fmt.Sprintf("origin/%s", defaultBranch)
}

// shouldCreateFreshSessionBranch decides whether the session manager should
// replace the worktree's current branch with a new polecat branch based on
// the canonical remote base. Decisions are made from structured data —
// parseFreshBranchName output and the computed canonical branch — not from
// substring heuristics on the branch name.
func shouldCreateFreshSessionBranch(currentBranch, issue, canonicalBranch string) bool {
	meta := parseFreshBranchName(currentBranch)

	// Same-issue respawn: keep the existing polecat branch so preserved work
	// for this issue isn't discarded.
	if meta.ok && issue != "" && meta.issue == issue {
		return false
	}

	// On the canonical base branch — need a fresh polecat branch to work on.
	if canonicalBranch != "" && currentBranch == canonicalBranch {
		return true
	}

	// On some other polecat branch belonging to a different issue — fresh
	// branch is safer than inheriting unrelated preserved history.
	return issue != "" && meta.ok
}

// ensureCanonicalSessionBranch puts a starting session on a polecat branch
// rather than the canonical base branch, and reports why it could not.
//
// It returns ErrBaseBranchRepair when the repair was needed and the worktree
// is still on the base branch. That case is an error rather than a log line
// because nothing downstream can tell a session that opened on the base
// branch from a healthy one (gt-ns8t).
func (m *SessionManager) ensureCanonicalSessionBranch(g gitRepo, polecat string, opts SessionStartOptions) (string, error) {
	townRoot := filepath.Dir(m.rig.Path)

	currentBranch, err := g.CurrentBranch()
	if err != nil {
		// The worktree's branch is unreadable, so whether it sits on the base
		// branch is unknown. Report and start: refusing here would fail every
		// session in a worktree whose only problem is this read.
		reportBranchRepairFailure(townRoot, m.rig.Name, polecat, "read the current branch", err)
		return "", nil
	}

	startPoint := m.canonicalSessionStartPoint(g)
	if startPoint == "" {
		reportBranchRepairFailure(townRoot, m.rig.Name, polecat,
			"resolve the canonical session start point",
			errors.New("no default branch in rig config or remote"))
		return currentBranch, nil
	}
	canonicalBranch := strings.TrimPrefix(startPoint, "origin/")
	if !shouldCreateFreshSessionBranch(currentBranch, opts.Issue, canonicalBranch) {
		return currentBranch, nil
	}

	// Refresh origin refs before branching so recovered sessions start from the
	// canonical remote base instead of any preserved local polecat branch.
	if err := g.Fetch("origin"); err != nil {
		// The branch is still created, but from whatever origin/<default>
		// resolved to locally. That mis-bases the session rather than stranding
		// it on the base branch, so it is reported and the repair continues.
		reportBranchRepairFailure(townRoot, m.rig.Name, polecat, "refresh origin", err)
	}

	exists, err := g.RefExists(startPoint)
	if err != nil {
		return currentBranch, m.baseBranchRepairError(townRoot, polecat, currentBranch, "check "+startPoint, err)
	}
	if !exists {
		return currentBranch, m.baseBranchRepairError(townRoot, polecat, currentBranch,
			"resolve "+startPoint, fmt.Errorf("%s does not exist locally", startPoint))
	}

	newBranch := m.freshBranchName(polecat, opts.Issue)
	if err := g.CheckoutNewBranch(newBranch, startPoint); err != nil {
		return currentBranch, m.baseBranchRepairError(townRoot, polecat, currentBranch, "check out "+newBranch, err)
	}
	// startPoint is the canonical base, so it is the upstream this branch's work
	// merges into — the one thing git's own choice gets wrong here (gt-voz8q).
	pinBranchUpstream(g, newBranch, startPoint)

	return newBranch, nil
}

// baseBranchRepairError reports a repair that was needed, was attempted, and
// left the worktree on the base branch. The message names the branch and the
// failing step so the operator can finish the repair by hand.
func (m *SessionManager) baseBranchRepairError(townRoot, polecat, currentBranch, step string, err error) error {
	reportBranchRepairFailure(townRoot, m.rig.Name, polecat, step, err)
	return fmt.Errorf("%w: %s/%s is on %q and %s failed: %v",
		ErrBaseBranchRepair, m.rig.Name, polecat, currentBranch, step, err)
}

// HasPolecat reports whether a polecat directory exists for name — the same
// source the witness zombie detector walks when it decides which polecats it
// may recover (gt-fojqs).
func (m *SessionManager) HasPolecat(polecat string) bool {
	return m.hasPolecat(polecat)
}

// hasPolecat checks if the polecat exists in this rig.
func (m *SessionManager) hasPolecat(polecat string) bool {
	polecatPath := m.polecatDir(polecat)
	info, err := os.Stat(polecatPath)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// polecatSlot returns a unique integer slot index for this polecat based on its
// position among existing polecat directories. This enables port offsetting and
// resource isolation when multiple polecats run in parallel (GH#954).
func (m *SessionManager) polecatSlot(polecat string) int {
	polecatsDir := filepath.Join(m.rig.Path, "polecats")
	entries, err := os.ReadDir(polecatsDir)
	if err != nil {
		return 0
	}
	slot := 0
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if e.Name() == polecat {
			return slot
		}
		slot++
	}
	return slot
}

// Start creates and starts a new session for a polecat.
// ensureRuntimeWorkspace prepares workDir for an unattended agent session:
// runtime settings in the shared polecats parent directory (passed to Claude
// Code via --settings flag) and Claude's folder-trust entry for the worktree.
// Fresh polecat worktrees are never-before-trusted paths, and without the
// pre-seeded trust entry the session stalls on the folder-trust dialog
// (gt-22r, gt-yy9). Trust seeding is non-fatal — AcceptStartupDialogs remains
// the in-pane backstop.
// Every start syncs the managed settings and reports hooks:present or
// hooks:absent for the session (gt-4k3fj.8.3).
func (m *SessionManager) ensureRuntimeWorkspace(polecat, workDir, runtimeConfigDir string, runtimeConfig *config.RuntimeConfig) error {
	polecatSettingsDir := config.RoleSettingsDir("polecat", m.rig.Path)
	hooksStatus, err := runtime.SyncSessionSettings(polecatSettingsDir, workDir, "polecat")
	runtime.ReportHooks(filepath.Dir(m.rig.Path), fmt.Sprintf("%s/polecats/%s", m.rig.Name, polecat), m.SessionName(polecat), hooksStatus)
	if err != nil {
		return fmt.Errorf("ensuring runtime settings: %w", err)
	}
	runtime.SeedWorkspaceTrust(workDir, runtimeConfigDir, runtimeConfig)
	return nil
}

func (m *SessionManager) Start(polecat string, opts SessionStartOptions) error {
	// Refuse before any side effect when the gt binary's bd handshake failed.
	if err := bdgate.Require(); err != nil {
		return err
	}
	if !m.hasPolecat(polecat) {
		return fmt.Errorf("%w: %s", ErrPolecatNotFound, polecat)
	}

	sessionID := m.SessionName(polecat)

	// Check if session already exists.
	// If an existing session's pane process has died, kill the stale session
	// and proceed rather than returning ErrSessionRunning (gt-jn40ft).
	//
	// For zombie detection, use IsAgentAliveChecked directly rather than the
	// heartbeat-primary isSessionStale path. The pane process is often a
	// shell or wrapper that outlives the agent, so heartbeat-fresh + pane-PID
	// alive can hide a dead agent — wedging gt session restart with
	// ErrSessionRunning on the very zombie state recovery is meant to handle
	// (hq-k1ot / np-tt5s). A false-negative here is recoverable: Start is
	// about to (re)create the session, so killing a transiently-misclassified
	// healthy session just churns one creation cycle. Patrol-side cleanup
	// (manager.go:cleanupOrphanedDirs) intentionally keeps the conservative
	// isSessionProcessDead path to avoid killing healthy sessions during
	// transient pgrep/ps failures.
	running, err := m.tmux.HasSession(sessionID)
	if err != nil {
		return fmt.Errorf("checking session: %w", err)
	}
	if running {
		// A failed liveness query is UNKNOWN, not a zombie: refuse the start
		// rather than kill a session that may be working (gt-fcxe9.1).
		alive, aliveErr := m.tmux.IsAgentAliveChecked(sessionID)
		if aliveErr != nil {
			return fmt.Errorf("checking agent liveness in %s (not killing): %w", sessionID, aliveErr)
		}
		if alive {
			return fmt.Errorf("%w: %s", ErrSessionRunning, sessionID)
		}
		replace := func() error {
			if err := m.tmux.KillSessionWithProcesses(sessionID); err != nil {
				return fmt.Errorf("killing stale session %s: %w", sessionID, err)
			}
			return m.launch(polecat, sessionID, opts)
		}
		if m.hooks.Respawn == nil {
			return replace()
		}
		return m.hooks.Respawn(polecat, "polecat start: replace a session whose agent exited", replace)
	}
	m.dropStaleSandboxRoute(sessionID, opts)
	launch := m.launch
	if m.launchFn != nil {
		launch = m.launchFn
	}
	return launch(polecat, sessionID, opts)
}

// dropStaleSandboxRoute removes the registry record a finished sandbox left
// behind when a HOST session is about to take its name. The routed tmux client
// sends every command for a recorded name into that sandbox, so with the record
// still listed the new session's creation went to a sandbox that no longer exists
// and failed as "session not found"; the patrol reaps such records only on its
// own schedule.
//
// The record goes only when the record's own gateway affirmatively reports no
// sandbox of that name, the patrol reaper's rule. That is also why a concurrent
// sandbox Prepare is never wiped: Prepare creates the sandbox before it writes
// the record, so while a record is new the gateway has the sandbox, and the
// record stays. A gateway that cannot be asked, or a registry that cannot be
// read, leaves the record and the start proceeds with a one-line warning: the
// record only matters if it exists, and the launch fails by itself, naming the
// sandbox, if it does route into a dead one. Only the record and its route are
// dropped; a sandbox start (Prepare writes the record afresh) is left alone.
func (m *SessionManager) dropStaleSandboxRoute(sessionID string, opts SessionStartOptions) {
	if b := opts.Backend; b != nil && b.Name() != session.BackendTmux {
		return
	}
	townRoot := filepath.Dir(m.rig.Path)
	rec, found, err := session.ReadSandboxRecord(townRoot, sessionID)
	if err != nil {
		style.PrintWarning("starting %s: cannot read its sandbox record, leaving it: %v", sessionID, err)
		return
	}
	if !found {
		return
	}
	exists := m.sandboxExists
	if exists == nil {
		exists = gatewayHasSandbox
	}
	has, err := exists(rec.Gateway, sessionID)
	if err != nil {
		style.PrintWarning("starting %s: cannot ask the gateway about its sandbox record, leaving it: %v", sessionID, err)
		return
	}
	if has {
		return
	}
	if err := session.CleanupSandboxWith(nil, townRoot, sessionID); err != nil {
		style.PrintWarning("starting %s: dropping its stale sandbox record failed: %v", sessionID, err)
	}
}

// gatewayHasSandbox asks gateway ("" is the default gateway) whether it has a
// sandbox called name.
func gatewayHasSandbox(gateway, name string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return (&sandbox.CLI{Gateway: gateway}).Exists(ctx, name)
}

// launch creates the polecat's session and starts its agent. A launch that
// fails after Prepare removes the sandbox that Prepare made; see the deferred
// cleanup below.
func (m *SessionManager) launch(polecat, sessionID string, opts SessionStartOptions) (err error) {
	// Determine working directory
	workDir := opts.WorkDir
	if workDir == "" {
		workDir = m.clonePath(polecat)
	}

	// Validate issue exists and isn't tombstoned BEFORE creating session.
	// This prevents CPU spin loops from agents retrying work on invalid issues.
	if opts.Issue != "" {
		if err := m.validateIssue(opts.Issue, workDir); err != nil {
			return err
		}
	}

	// Resolve through the role-aware resolver so --settings and
	// --append-system-prompt-file are both attached (gt-lpb0).
	// An explicit --agent override still resolves that agent's own config
	// rather than the default role agent, so WaitForRuntimeReady polls with
	// the right readiness strategy for the agent that actually starts (gt-1j3m).
	townRoot := filepath.Dir(m.rig.Path)
	runtimeConfig, err := config.ResolveRoleAgentConfigWithOverride("polecat", townRoot, m.rig.Path, opts.Agent, polecat)
	if err != nil {
		return fmt.Errorf("resolving agent config for %s: %w", opts.Agent, err)
	}

	// Prepare the workspace (settings, folder trust) before the session starts.
	if err := m.ensureRuntimeWorkspace(polecat, workDir, opts.RuntimeConfigDir, runtimeConfig); err != nil {
		return err
	}

	// Build startup command with beacon for predecessor discovery. Claude's
	// SessionStart hook runs gt prime, and the beacon rides in as the CLI prompt.
	address := session.BeaconRecipient("polecat", polecat, m.rig.Name)
	beacon := session.FormatStartupBeacon(session.BeaconConfig{
		Recipient: address,
		Sender:    "witness",
		Topic:     "assigned",
		MolID:     opts.Issue,
	})
	startupNudgeContent := runtime.StartupNudgeContent()

	// Compute environment vars BEFORE creating the session so they can be
	// passed to tmux via -e flags. Setting env via SetEnvironment after the
	// pane starts only affects newly spawned panes — the running pane (and
	// any subprocess Claude spawns, e.g. bd) keeps its original env (gt-neycp).
	//
	// GT_BRANCH and GT_POLECAT_PATH are critical for gt done's nuked-worktree
	// fallback: when the polecat's cwd is deleted before gt done finishes,
	// these env vars allow branch detection and path resolution without a
	// working directory.
	polecatGitBranch := ""
	if g := m.gits.Open(workDir); g != nil {
		branch, err := m.ensureCanonicalSessionBranch(g, polecat, opts)
		if err != nil {
			// Before any tmux session exists, so nothing is left to clean up.
			return err
		}
		polecatGitBranch = branch
	}
	envVars := config.AgentEnv(polecatAgentEnvConfig(m.rig.Name, polecat, townRoot, sessionID, opts, runtimeConfig))
	// AgentEnv already sets GT_ROLE, GT_RIG, GT_POLECAT, BD_ACTOR,
	// BD_DOLT_AUTO_COMMIT, etc. Layer in polecat-session-specific vars.
	envVars["GT_POLECAT_PATH"] = workDir
	envVars["GT_TOWN_ROOT"] = townRoot
	envVars["POLECAT_SLOT"] = fmt.Sprintf("%d", m.polecatSlot(polecat))
	envVars["GT_PROCESS_NAMES"] = strings.Join(config.AgentRegistryFor(townRoot, m.rig.Path).ResolveProcessNames(runtimeConfig.ResolvedAgent, runtimeConfig.Command, runtimeConfig.Args...), ",")
	if polecatGitBranch != "" {
		envVars["GT_BRANCH"] = polecatGitBranch
	}
	// AgentEnv only emits GT_AGENT when opts.Agent is non-empty (explicit override).
	// Fallback for the no-override path so the tmux session table has GT_AGENT
	// for show-environment lookups.
	if _, hasGTAgent := envVars["GT_AGENT"]; !hasGTAgent && runtimeConfig.ResolvedAgent != "" {
		envVars["GT_AGENT"] = runtimeConfig.ResolvedAgent
	}
	// Account config dir env (CLAUDE_CONFIG_DIR) for the selected account.
	if runtimeConfig.Session != nil && runtimeConfig.Session.ConfigDirEnv != "" && opts.RuntimeConfigDir != "" {
		envVars[runtimeConfig.Session.ConfigDirEnv] = opts.RuntimeConfigDir
	}
	// A seat on a tracker rig carries the tracker endpoint and the path to its
	// own token file, so the bd it runs opens its rig's beads. Both names ride
	// in envVars with the rest, so the tmux -e environment and the export
	// prefix carry them. A Dolt rig gets nothing here and its environment is
	// what it was.
	// A sandbox seat is skipped: the token file's path is a host path the
	// sandbox cannot open, and how a sandbox seat reaches the tracker is its own
	// delivery.
	if b := opts.Backend; b == nil || b.Name() == session.BackendTmux {
		if err := m.addTrackerSeatEnv(envVars, townRoot, polecat); err != nil {
			return err
		}
	}

	// Create session with command and env vars via -e flags so the initial
	// shell — and Claude's subprocesses (notably bd) — inherit them from the start.
	// See: https://github.com/anthropics/gastown/issues/280 (race condition fix)
	// Where the session runs. The tmux backend runs it in the host worktree;
	// the openshell backend prepares a sandbox (clone, skeleton, registry
	// record) and hands back the in-sandbox work dir plus the env the agent
	// command must carry there. After Prepare the tmux client routes this
	// session's commands into the sandbox (session.sandbox_routing).
	sessionWorkDir := workDir
	var hostTownRoot, sandboxRoot string // set when a backend moves the session into a sandbox
	envCfg := config.AgentEnvConfig{
		Role:        "polecat",
		Rig:         m.rig.Name,
		AgentName:   polecat,
		TownRoot:    townRoot,
		SessionName: sessionID,
	}
	if b := opts.Backend; b != nil && b.Name() != session.BackendTmux {
		fh, ok := b.(forgejoHoster)
		if !ok {
			return fmt.Errorf("preparing %s session for %s: the backend names no forgejo host to clone from", b.Name(), polecat)
		}
		// A separate error: the deferred cleanup below reads the named
		// result, which this block must not shadow.
		repoURL, repoErr := m.sandboxRepoURL(fh, townRoot, polecat, polecatGitBranch)
		if repoErr != nil {
			return fmt.Errorf("preparing %s session for %s: %w", b.Name(), polecat, repoErr)
		}
		prep, perr := b.Prepare(context.Background(), session.SpawnSpec{
			TownRoot:    townRoot,
			Rig:         m.rig.Name,
			Polecat:     polecat,
			Session:     sessionID,
			Branch:      polecatGitBranch,
			RepoURL:     repoURL,
			HostWorkDir: workDir,
			WorkBeadID:  opts.Issue,
			Env:         envVars,
			Trace:       opts.Trace,
		})
		if perr != nil {
			return fmt.Errorf("preparing %s session for %s: %w", b.Name(), polecat, perr)
		}
		// A launch that fails after Prepare leaves a running sandbox whose
		// registry record counts as a live session, so the seat looks occupied
		// and no one gets to it: Stop returns ErrSessionNotFound when the tmux
		// session never came up, so the kill path's cleanup never runs. This
		// drops what this launch made, sandbox and record (gt-8a1ql).
		//
		// A panic unwinds through this defer with the named result still nil,
		// so it is recovered, the sandbox dropped, and the same value panicked
		// again. A launch that fails after this returns is out of reach: the
		// patrol's orphan cleanup owns that sandbox.
		defer func() {
			p := recover()
			if err == nil && p == nil {
				return
			}
			if cerr := session.CleanupSandboxWith(sandboxDeleterOrLinked(m.sandboxDeleter), townRoot, sessionID); cerr != nil {
				if p != nil {
					// A panic leaves no result to carry the failure out.
					style.PrintWarning("cleaning up %s session for %s: %v", b.Name(), polecat, cerr)
				} else {
					err = errors.Join(err, fmt.Errorf("cleaning up %s session for %s: %w", b.Name(), polecat, cerr))
				}
			}
			if p != nil {
				panic(p)
			}
		}()
		if prep.WorkDir != "" {
			sessionWorkDir = prep.WorkDir
			// The startup command names files under the host town root
			// (--settings, --append-system-prompt-file, GT_SYSTEM_PROMPT_FILE);
			// inside the sandbox they live under its own town root.
			root, rerr := sandboxTownRoot(townRoot, workDir, prep.WorkDir)
			if rerr != nil {
				return fmt.Errorf("preparing %s session for %s: %w", b.Name(), polecat, rerr)
			}
			hostTownRoot, sandboxRoot = townRoot, root
			// envVars was built from host paths too (GIT_CEILING_DIRECTORIES,
			// GT_ROOT, ...); the session environment names none of them.
			rewriteTownRootInPathEnv(envVars, hostTownRoot, sandboxRoot)
		}
		// The same overrides reach the agent two ways on purpose: in the
		// startup command (EnvOverride, so the agent process and what it
		// spawns see them) and in the tmux session environment (envVars,
		// so later panes and exec'd commands see them too).
		envCfg.EnvOverride = prep.Env
		envCfg.OmitSecretPrefix = prep.OmitSecretPrefix
		for k, v := range prep.Env {
			if v == "" {
				delete(envVars, k)
			} else {
				envVars[k] = v
			}
		}
	}

	command := opts.Command
	if command == "" {
		// A separate error: the deferred cleanup above reads the named
		// result, which this block must not shadow.
		var buildErr error
		command, buildErr = config.BuildStartupCommandFromConfig(envCfg, m.rig.Path, beacon, opts.Agent)
		if buildErr != nil {
			return fmt.Errorf("building startup command: %w", buildErr)
		}
		command = session.RewriteTownRoot(command, hostTownRoot, sandboxRoot)
	}

	if err := m.tmux.NewSessionWithCommandAndEnv(sessionID, sessionWorkDir, command, envVars); err != nil {
		return fmt.Errorf("creating session: %w", err)
	}

	// Record agent's pane_id for ZFC-compliant liveness checks (gt-qmsx).
	// Declared pane identity replaces process-tree inference in IsRuntimeRunning
	// and FindAgentPane. Legacy sessions without GT_PANE_ID fall back to scanning.
	if paneID, err := m.tmux.GetPaneID(sessionID); err == nil {
		_ = m.tmux.SetEnvironment(sessionID, "GT_PANE_ID", paneID)
	}

	// Hook the issue to the polecat if provided via --issue flag
	if opts.Issue != "" {
		agentID := fmt.Sprintf("%s/polecats/%s", m.rig.Name, polecat)
		if err := m.hookIssue(opts.Issue, agentID, workDir); err != nil {
			style.PrintWarning("could not hook issue %s: %v", opts.Issue, err)
		}
	}

	// Apply theme (non-fatal)
	theme := tmux.ResolveSessionTheme(townRoot, m.rig.Name, "polecat", polecat)
	_ = m.tmux.ConfigureGasTownSession(sessionID, theme, m.rig.Name, polecat, "polecat")

	// Set pane-died hook for crash detection (non-fatal)
	agentID := fmt.Sprintf("%s/%s", m.rig.Name, polecat)
	_ = m.tmux.SetPaneDiedHook(sessionID, agentID)

	// Wait for Claude to start (non-fatal)
	_ = m.tmux.WaitForCommand(sessionID, constants.SupportedShells, constants.ClaudeStartTimeout)

	// Accept startup dialogs (workspace trust + bypass permissions) if they appear
	_ = m.tmux.AcceptStartupDialogs(sessionID)
	if err := m.tmux.CheckStartupBlocked(sessionID); err != nil {
		m.cleanup(polecat, sessionID, "polecat start: startup blocked")
		return fmt.Errorf("startup blocked: %w", err)
	}

	// Wait for runtime to be fully ready at the prompt (not just started).
	// Uses prompt-based polling for agents with ReadyPromptPrefix (e.g., Claude "❯ "),
	// falling back to ReadyDelayMs sleep for agents without prompt detection.
	_ = m.tmux.WaitForRuntimeReady(sessionID, runtimeConfig, constants.ClaudeStartTimeout)
	if err := m.tmux.CheckStartupBlocked(sessionID); err != nil {
		m.cleanup(polecat, sessionID, "polecat start: startup blocked")
		return fmt.Errorf("startup blocked: %w", err)
	}

	// Verify beacon delivery for hook+prompt agents (Mode A, hi-y44).
	// Fresh spawns may show the Claude Code splash screen with the CLI beacon
	// pre-filled but not auto-submitted. If the agent is still idle after startup,
	// re-deliver the work instructions via nudge to kick it into action.
	// Runs asynchronously: verifyStartupNudgeDelivery sleeps before checking, so a
	// synchronous call would add ~25s to every successful polecat startup on the
	// common gt sling path. Non-fatal: the witness zombie patrol handles unrecovered stalls.
	go m.verifyStartupNudgeDelivery(sessionID, runtimeConfig, startupNudgeContent)

	// Verify session survived startup - if the command crashed, the session may have died.
	// Without this check, Start() would return success even if the pane died during initialization.
	running, err := m.tmux.HasSession(sessionID)
	if err != nil {
		return fmt.Errorf("verifying session: %w", err)
	}
	if !running {
		return fmt.Errorf("session %s died during startup (agent command may have failed)", sessionID)
	}
	// AgentUnknown (a failed liveness query) is not evidence the startup
	// failed; only a confirmed unhealthy status kills the new session.
	if status := m.tmux.CheckSessionHealth(sessionID, 0); status != tmux.SessionHealthy && status != tmux.AgentUnknown {
		m.cleanup(polecat, sessionID, "polecat start: unhealthy during startup")
		return fmt.Errorf("session %s unhealthy during startup: %s", sessionID, status)
	}

	// Validate GT_AGENT is set. Without GT_AGENT, IsAgentAliveChecked cannot
	// resolve the session's preset (process names, Escape policy) and witness
	// patrol may auto-nuke a live polecat. Fail fast.
	gtAgent, _ := m.tmux.GetEnvironment(sessionID, "GT_AGENT")
	if gtAgent == "" {
		m.cleanup(polecat, sessionID, "polecat start: GT_AGENT not set")
		return fmt.Errorf("GT_AGENT not set in session %s (command=%q); "+
			"witness patrol will misidentify this polecat as a zombie and auto-nuke it. "+
			"Ensure RuntimeConfig.ResolvedAgent is set during agent config resolution",
			sessionID, runtimeConfig.Command)
	}

	// Track PID for defense-in-depth orphan cleanup (non-fatal). It needs the
	// real tmux; a test's fake tmux skips it.
	if real, ok := m.tmux.(*tmux.Tmux); ok {
		_ = session.TrackSessionPID(townRoot, sessionID, real)
	}

	// Touch initial heartbeat so liveness detection works from the start (gt-qjtq).
	// Subsequent touches happen on every gt command via persistentPreRun.
	TouchSessionHeartbeat(townRoot, sessionID)

	return nil
}

// addTrackerSeatEnv gives a seat on a tracker rig the endpoint and the path to
// its own token file, issuing the token on the seat's first launch and reusing
// the file on a restart. The token value never enters the environment: the
// seat is handed the file's path, and the tracker client reads it from there.
//
// A rig on Dolt gets neither variable, so a Dolt rig's seat environment is
// byte for byte what it was. An unreadable route table, a backend the table
// does not define, or an endpoint that does not resolve is an error, never
// "no rig on the tracker": that would start the seat with no beads store it
// could open.
func (m *SessionManager) addTrackerSeatEnv(envVars map[string]string, townRoot, polecat string) error {
	seat := intent.Seat{Rig: m.rig.Name, Role: "polecats", Name: polecat}
	store, err := rigBackend(townRoot, seat.Rig)
	if err != nil {
		return fmt.Errorf("the tracker environment of seat %s: %w", seat, err)
	}
	if store == beads.BackendDolt {
		return nil
	}
	if store != beads.BackendTracker {
		return fmt.Errorf("the tracker environment of seat %s: the route table names the backend %q for rig %s, which is neither %q nor %q",
			seat, store, seat.Rig, beads.BackendDolt, beads.BackendTracker)
	}
	endpoint, err := config.ResolveTrackerEndpoint(townRoot)
	if err != nil {
		return fmt.Errorf("the tracker environment of seat %s: %w", seat, err)
	}
	// The seat's name becomes part of a file name under the town's runtime
	// directory, so it must be one safe path segment before anything is written.
	if err := CheckSandboxSeatNames(seat.Rig, polecat); err != nil {
		return fmt.Errorf("the tracker token of seat %s: %w", seat, err)
	}
	issuer := m.seatTokenIssuer
	if issuer == nil {
		// The host issuer needs the town's admin credential, which a restart that
		// finds the seat's token file never uses: it is built only when the
		// token is actually issued.
		issuer = &lazySeatIssuer{build: func() (seattoken.Issuer, error) {
			if m.hostSeatIssuer != nil {
				return m.hostSeatIssuer(townRoot)
			}
			host, err := seattoken.NewHostIssuer(townRoot)
			if err != nil {
				return nil, err
			}
			return host, nil
		}}
	}
	timeout := m.seatTokenTimeout
	if timeout <= 0 {
		timeout = seatTokenIssueTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	path, err := seattoken.New(townRoot).Ensure(ctx, seat, issuer)
	if err != nil {
		// seattoken's errors name the seat and never the token; the wrap
		// keeps the seat in the message a failed launch shows an operator.
		return fmt.Errorf("the tracker token of seat %s: %w", seat, err)
	}
	envVars[envTrackerURL] = endpoint.URL()
	envVars[envTrackerTokenFile] = path
	return nil
}

// lazySeatIssuer builds its issuer on the first Issue, so Ensure, which asks for
// a token only when the seat has none, needs the issuer's credential only then.
type lazySeatIssuer struct {
	build  func() (seattoken.Issuer, error)
	once   sync.Once
	issuer seattoken.Issuer
	err    error
}

func (l *lazySeatIssuer) Issue(ctx context.Context, actor string) (string, error) {
	l.once.Do(func() { l.issuer, l.err = l.build() })
	if l.err != nil {
		return "", l.err
	}
	return l.issuer.Issue(ctx, actor)
}

// rigBackend reports the backend the route table names for rig: every route
// whose path begins with the rig's name is read, so a second route for the rig
// is not lost behind the first. A rig the table does not name is on Dolt.
// Routes that name rig and disagree are an error, because the store that holds
// its beads must have one answer.
func rigBackend(townRoot, rig string) (string, error) {
	routes, err := beads.LoadRoutes(filepath.Join(townRoot, ".beads"))
	if err != nil {
		return "", fmt.Errorf("reading the route table: %w", err)
	}
	name := ""
	for _, r := range routes {
		if strings.SplitN(r.Path, "/", 2)[0] != rig {
			continue
		}
		n := r.BackendName()
		if name != "" && n != name {
			return "", fmt.Errorf("the routes for rig %s name both %q and %q", rig, name, n)
		}
		name = n
	}
	if name == "" {
		return beads.BackendDolt, nil
	}
	return name, nil
}

// isSessionStale checks if a tmux session's pane process has died.
// A stale session exists in tmux but its main process (the agent) is no longer running.
// This happens when the agent crashes during startup but tmux keeps the dead pane.
// Delegates to isSessionProcessDead to avoid duplicating process-check logic (gt-qgzj1h).
func (m *SessionManager) isSessionStale(sessionID string) bool {
	var probe sessionProbe
	if m.tmux != nil {
		probe = m.tmux
	}
	return isSessionProcessDead(probe, sessionID, filepath.Dir(m.rig.Path))
}

// Stop terminates a polecat session and deletes the sandbox it ran in. When
// the session stopped but the sandbox could not be removed, the error wraps
// ErrSandboxNotRemoved.
func (m *SessionManager) Stop(polecat string, force bool) error {
	sessionID := m.SessionName(polecat)

	running, err := m.tmux.HasSession(sessionID)
	if err != nil {
		return fmt.Errorf("checking session: %w", err)
	}
	if !running {
		return ErrSessionNotFound
	}

	// Try graceful shutdown first
	if !force {
		_ = m.tmux.SendKeysRaw(sessionID, "C-c")
		if real, ok := m.tmux.(*tmux.Tmux); ok {
			session.WaitForSessionExit(real, sessionID, constants.GracefulShutdownTimeout)
		}
	}

	if m.hooks.Stop != nil {
		if err := m.hooks.Stop(sessionID); err != nil {
			return fmt.Errorf("killing session: %w", err)
		}
		m.endSeatSpan(polecat)
		return m.removeSandbox(sessionID)
	}
	// Use KillSessionWithProcesses to ensure all descendant processes are killed.
	// This prevents orphan bash processes from Claude's Bash tool surviving session termination.
	if err := m.tmux.KillSessionWithProcesses(sessionID); err != nil {
		return fmt.Errorf("killing session: %w", err)
	}
	m.endSeatSpan(polecat)

	return m.removeSandbox(sessionID)
}

// endSeatSpan closes the attempt the stopped seat left open. The seat reported
// no end of its own — a sandboxed one cannot, since it writes the log of the
// sandbox's town rather than this one — and a stopping session is the last
// moment the host sees it (gt-8yk78).
//
// The close reads only the span store: a stop runs in gt down and gt polecat
// nuke, which must not wait on Dolt, so the store answers which bead the seat
// held from its own logs (spans.Store.EndSeatOpen).
//
// The close is a lower bound: a stop ends the session, not the work, and a
// seat resumed on the same bead continues the attempt with no second start, so
// the time after the stop is missing from the log rather than wrong in it.
// Nothing here fails the stop the caller asked for: the span is a measurement,
// and a log that cannot be read costs the measurement, not the stop.
func (m *SessionManager) endSeatSpan(polecat string) {
	if m.rig == nil || m.rig.Path == "" || m.rig.Name == "" || polecat == "" {
		return
	}
	seat := m.rig.Name + "/" + polecat
	// A rig lives directly under the town: the span store is the town's.
	store := spans.New(spans.DefaultDir(filepath.Dir(m.rig.Path)), nil)
	if _, _, err := store.EndSeatOpen(seat, spans.ReasonStopped, m.clock().Now()); err != nil {
		m.warnSpan("could not end the polecat span of %s: %v", seat, err)
	}
}

// warnSpan reports an end that could not be written; nil is style.PrintWarning.
// A caller that keeps a log (the daemon) can watch for a store it cannot read.
func (m *SessionManager) warnSpan(format string, args ...any) {
	if m.warn != nil {
		m.warn(format, args...)
		return
	}
	style.PrintWarning(format, args...)
}

// clock is the clock a stopped seat's span end is stamped with; nil is the
// real clock.
func (m *SessionManager) clock() clockwork.Clock {
	if m.clk != nil {
		return m.clk
	}
	return clockwork.NewRealClock()
}

// removeSandbox pushes the work a stopped sandboxed session committed, then
// deletes the sandbox it ran in and its registry record. A host session has no
// record and nothing happens. Left behind, the sandbox stays Ready and the seat
// keeps looking occupied.
//
// The push comes first because the sandbox's clone is the only copy of commits
// the seat never pushed (gt-hwpfq). A push that is refused keeps the sandbox and
// its record, so those commits survive for the operator to recover.
//
// A registry that cannot be read is not a host session: it fails with
// ErrSandboxNotRemoved and names the lookup, apart from a deletion failure.
func (m *SessionManager) removeSandbox(sessionID string) error {
	if m.rig.Path == "" {
		return nil
	}
	return RemoveStoppedSandbox(filepath.Dir(m.rig.Path), sessionID, m.sandboxPusher, m.sandboxDeleter)
}

// RemoveStoppedSandbox is removeSandbox for a session that something other than
// a SessionManager stopped, such as the daemon's overrun stop, which kills
// through the supervisor. pusher and deleter nil are the linked openshell hooks.
// It is the same push-then-delete: a refused push keeps the sandbox and its
// record, and the error wraps ErrSandboxNotRemoved.
func RemoveStoppedSandbox(townRoot, sessionID string, pusher, deleter func(townRoot string, rec session.SandboxRecord) error) error {
	rec, sandboxed, err := session.ReadSandboxRecord(townRoot, sessionID)
	if err != nil {
		return fmt.Errorf("%w: cannot tell whether %s ran in a sandbox: %w", ErrSandboxNotRemoved, sessionID, err)
	}
	if !sandboxed {
		return nil
	}
	// A sandbox already gone has no commits left to lose, so it is not a refused
	// push; the delete below finds nothing to remove either.
	if err := sandboxPusherOrLinked(pusher, session.SandboxPusher)(townRoot, rec); err != nil && !errors.Is(err, session.ErrNothingToPush) {
		return fmt.Errorf("%w: keeping its sandbox so its commits survive, their push was refused: %w", ErrSandboxNotRemoved, err)
	}
	if err := session.CleanupSandboxWith(sandboxDeleterOrLinked(deleter), townRoot, sessionID); err != nil {
		return fmt.Errorf("%w: removing its sandbox: %w", ErrSandboxNotRemoved, err)
	}
	return nil
}

// errNoSandboxPusher is the refusal a stop gets when no pusher is linked.
var errNoSandboxPusher = errors.New("no sandbox pusher is linked into this binary")

// sandboxPusherOrLinked is the pusher a stop runs: the one a test injected, else
// the linked openshell pusher. With neither, the push is refused, so the sandbox
// is kept rather than deleted with its commits.
func sandboxPusherOrLinked(pusher, linked func(townRoot string, rec session.SandboxRecord) error) func(townRoot string, rec session.SandboxRecord) error {
	if pusher != nil {
		return pusher
	}
	if linked != nil {
		return linked
	}
	return func(string, session.SandboxRecord) error {
		return errNoSandboxPusher
	}
}

// IsRunning checks if a polecat session is active and healthy.
// Checks both tmux session existence AND agent process liveness to avoid
// reporting zombie sessions (tmux alive but Claude dead) as "running".
func (m *SessionManager) IsRunning(polecat string) (bool, error) {
	sessionID := m.SessionName(polecat)
	status := m.tmux.CheckSessionHealth(sessionID, 0)
	return status.CountsAsRunning(), nil
}

// Status returns detailed status for a polecat session.
func (m *SessionManager) Status(polecat string) (*SessionInfo, error) {
	sessionID := m.SessionName(polecat)

	running, err := m.tmux.HasSession(sessionID)
	if err != nil {
		return nil, fmt.Errorf("checking session: %w", err)
	}

	info := &SessionInfo{
		Polecat:   polecat,
		SessionID: sessionID,
		Running:   running,
		RigName:   m.rig.Name,
	}

	if !running {
		return info, nil
	}

	tmuxInfo, err := m.tmux.GetSessionInfo(sessionID)
	if err != nil {
		return info, nil
	}

	info.Attached = tmuxInfo.Attached
	info.Windows = tmuxInfo.Windows

	if tmuxInfo.Created != "" {
		info.Created = parseSessionCreatedTime(tmuxInfo.Created)
	}

	if tmuxInfo.Activity != "" {
		var activityUnix int64
		if _, err := fmt.Sscanf(tmuxInfo.Activity, "%d", &activityUnix); err == nil && activityUnix > 0 {
			info.LastActivity = time.Unix(activityUnix, 0)
		}
	}

	return info, nil
}

// parseSessionCreatedTime parses a tmux session-created timestamp as produced by
// Tmux.GetSessionInfo. That string is built from a Unix timestamp via
// time.Unix(...).Format(...), which renders in the Local zone but drops the zone
// info from the string. It MUST be parsed back with time.ParseInLocation against
// time.Local (not time.Parse, which defaults to UTC) or the recovered instant is
// off by the machine's UTC offset — see gt-jy5.
func parseSessionCreatedTime(created string) time.Time {
	return parseSessionCreatedTimeIn(created, time.Local)
}

// parseSessionCreatedTimeIn is parseSessionCreatedTime for a machine whose
// local zone is loc.
func parseSessionCreatedTimeIn(created string, loc *time.Location) time.Time {
	formats := []string{
		"2006-01-02 15:04:05",
		"Mon Jan 2 15:04:05 2006",
		"Mon Jan _2 15:04:05 2006",
		time.ANSIC,
		time.UnixDate,
	}
	for _, format := range formats {
		if t, err := time.ParseInLocation(format, created, loc); err == nil {
			return t
		}
	}
	return time.Time{}
}

// List returns information about all sessions for this rig.
// This includes polecats, witness, refinery, and crew sessions.
// Use ListPolecats() to get only polecat sessions.
func (m *SessionManager) List() ([]SessionInfo, error) {
	sessions, err := m.tmux.ListSessions()
	if err != nil {
		return nil, err
	}

	prefix := m.prefix() + "-"
	var infos []SessionInfo

	for _, sessionID := range sessions {
		if !strings.HasPrefix(sessionID, prefix) {
			continue
		}

		polecat := strings.TrimPrefix(sessionID, prefix)
		infos = append(infos, SessionInfo{
			Polecat:   polecat,
			SessionID: sessionID,
			Running:   true,
			RigName:   m.rig.Name,
		})
	}

	return infos, nil
}

// ListPolecats returns information only about polecat sessions for this rig.
// Filters out witness, refinery, and crew sessions.
func (m *SessionManager) ListPolecats() ([]SessionInfo, error) {
	infos, err := m.List()
	if err != nil {
		return nil, err
	}

	var filtered []SessionInfo
	for _, info := range infos {
		// Skip non-polecat sessions
		if info.Polecat == "witness" || info.Polecat == "refinery" || strings.HasPrefix(info.Polecat, "crew-") {
			continue
		}
		filtered = append(filtered, info)
	}

	return filtered, nil
}

// Attach attaches to a polecat session.
func (m *SessionManager) Attach(polecat string) error {
	sessionID := m.SessionName(polecat)

	running, err := m.tmux.HasSession(sessionID)
	if err != nil {
		return fmt.Errorf("checking session: %w", err)
	}
	if !running {
		return ErrSessionNotFound
	}

	return m.tmux.AttachSession(sessionID)
}

// Capture returns the recent output from a polecat session.
func (m *SessionManager) Capture(polecat string, lines int) (string, error) {
	sessionID := m.SessionName(polecat)

	running, err := m.tmux.HasSession(sessionID)
	if err != nil {
		return "", fmt.Errorf("checking session: %w", err)
	}
	if !running {
		return "", ErrSessionNotFound
	}

	return m.tmux.CapturePane(sessionID, lines)
}

// CaptureSession returns the recent output from a session by raw session ID.
func (m *SessionManager) CaptureSession(sessionID string, lines int) (string, error) {
	running, err := m.tmux.HasSession(sessionID)
	if err != nil {
		return "", fmt.Errorf("checking session: %w", err)
	}
	if !running {
		return "", ErrSessionNotFound
	}

	return m.tmux.CapturePane(sessionID, lines)
}

// Inject sends a message to a polecat session.
func (m *SessionManager) Inject(polecat, message string) error {
	sessionID := m.SessionName(polecat)

	running, err := m.tmux.HasSession(sessionID)
	if err != nil {
		return fmt.Errorf("checking session: %w", err)
	}
	if !running {
		return ErrSessionNotFound
	}

	debounceMs := 200 + (len(message)/1024)*100
	if debounceMs > 1500 {
		debounceMs = 1500
	}

	return m.tmux.SendKeysDebounced(sessionID, message, debounceMs)
}

// StopAll terminates all polecat sessions for this rig.
func (m *SessionManager) StopAll(force bool) error {
	infos, err := m.ListPolecats()
	if err != nil {
		return err
	}

	var errs []error
	for _, info := range infos {
		if err := m.Stop(info.Polecat, force); err != nil {
			errs = append(errs, fmt.Errorf("stopping %s: %w", info.Polecat, err))
		}
	}

	return errors.Join(errs...)
}

// resolveHookWorkDir is the workspace directory bd commands on a given issue
// run from: the rig that issue's prefix routes to, or fallbackDir when the
// prefix names no rig. This enables cross-rig beads resolution via
// routes.jsonl. This is the core fix for GitHub issue #1056.
func (m *SessionManager) resolveHookWorkDir(issueID, fallbackDir string) string {
	townRoot := filepath.Dir(m.rig.Path)
	return beads.ResolveHookDir(townRoot, issueID, fallbackDir)
}

// validateIssue checks that an issue exists and is not in a terminal state.
// This must be called before starting a session to avoid CPU spin loops
// from agents retrying work on invalid issues.
func (m *SessionManager) validateIssue(issueID, workDir string) error {
	issue, err := m.beadsFor(issueID, workDir).Show(issueID)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrIssueInvalid, issueID)
	}
	if beads.IssueStatus(issue.Status).IsTerminal() {
		return fmt.Errorf("%w: %s has terminal status %s", ErrIssueInvalid, issueID, issue.Status)
	}
	return nil
}

// beadsFor opens the database issueID resolves to from fallbackDir. The
// database is named by path, so a BEADS_DIR the session manager's own process
// inherited cannot retarget the validateIssue read or the hookIssue write
// (gt-9hou3).
func (m *SessionManager) beadsFor(issueID, fallbackDir string) beads.Client {
	beadsDir := beads.ResolveBeadsDir(m.resolveHookWorkDir(issueID, fallbackDir))
	if m.beadsAt != nil {
		return m.beadsAt(beadsDir)
	}
	return beads.NewPinned(beadsDir)
}

// verifyStartupNudgeDelivery checks if the polecat started working after the
// startup nudge and retries the nudge if the agent is truly idle.
// This fixes the Mode B race condition (GH#1379) where the startup nudge arrives
// before Claude Code is ready, causing the polecat to sit idle.
//
// Uses IsIdle (not IsAtPrompt) to distinguish "idle at prompt" from "busy
// processing". IsIdle checks for a busy indicator (see hasBusyIndicator) in
// Claude's status bar — if present, the agent is actively working even though
// the ❯ prompt may still be visible in the pane. This prevents the false-
// positive retries that interrupted Claude mid-processing (GH#3031).
//
// Non-fatal: if verification fails or times out, the session is left running.
// The witness zombie patrol will eventually detect and handle truly idle polecats.
func (m *SessionManager) verifyStartupNudgeDelivery(sessionID string, rc *config.RuntimeConfig, retryContent string) {
	verifyStartupNudge(m.tmux, clockwork.NewRealClock(), filepath.Dir(m.rig.Path), sessionID, rc, retryContent)
}

// startupNudgeTmux is the tmux surface verifyStartupNudge drives. *tmux.Tmux
// provides it.
type startupNudgeTmux interface {
	HasSession(name string) (bool, error)
	IsIdle(session string) bool
	NudgeSession(session, message string) error
}

var _ startupNudgeTmux = (*tmux.Tmux)(nil)

// verifyStartupNudge is verifyStartupNudgeDelivery against tm, waiting on
// clk, with thresholds read from townRoot's operational config.
func verifyStartupNudge(tm startupNudgeTmux, clk clockwork.Clock, townRoot, sessionID string, rc *config.RuntimeConfig, retryContent string) {
	// Only verify for agents with prompt detection. Without ReadyPromptPrefix,
	// we can't distinguish "idle at prompt" from "busy processing".
	if rc == nil || rc.Tmux == nil || rc.Tmux.ReadyPromptPrefix == "" {
		return
	}

	// Use configurable thresholds from operational config so operators can tune
	// via settings/config.json without rebuilding. Both fall back to compiled-in
	// defaults when no config is present. (Re-wired after revert of #3100.)
	opCfg := config.LoadOperationalConfig(townRoot)
	sessionCfg := opCfg.GetSessionConfig()
	verifyDelay := sessionCfg.StartupNudgeVerifyDelayD()
	maxRetries := sessionCfg.StartupNudgeMaxRetriesV()

	if strings.TrimSpace(retryContent) == "" {
		retryContent = runtime.StartupNudgeContent()
	}

	for attempt := 1; attempt <= maxRetries; attempt++ {
		// Wait for the agent to process the nudge before checking.
		clk.Sleep(verifyDelay)

		// Check if session is still alive
		running, err := tm.HasSession(sessionID)
		if err != nil || !running {
			return // Session died, nothing to verify
		}

		// Use IsIdle instead of IsAtPrompt: IsIdle checks for the "esc to
		// interrupt" busy indicator. If Claude is processing (loading context,
		// running tools, generating a response), the status bar shows the busy
		// indicator and IsIdle returns false — even though ❯ may still be
		// visible in the pane from before Claude started output.
		if !tm.IsIdle(sessionID) {
			return // Agent is busy — nudge was received and is being processed
		}

		// Agent is truly idle (no busy indicator, prompt visible) — nudge was likely lost. Retry.
		fmt.Fprintf(os.Stderr, "[startup-nudge] attempt %d/%d: agent %s idle at prompt, retrying nudge\n",
			attempt, maxRetries, sessionID)
		if err := tm.NudgeSession(sessionID, retryContent); err != nil {
			fmt.Fprintf(os.Stderr, "[startup-nudge] retry nudge failed for %s: %v\n", sessionID, err)
			return
		}
	}

	// If we exhausted retries and the agent is still idle, log a warning.
	// The witness zombie patrol will handle this case.
	if tm.IsIdle(sessionID) {
		fmt.Fprintf(os.Stderr, "[startup-nudge] WARNING: agent %s still idle after %d nudge retries\n",
			sessionID, maxRetries)
	}
}

// hookIssue pins an issue to a polecat's hook.
func (m *SessionManager) hookIssue(issueID, agentID, workDir string) error {
	status := string(beads.StatusHooked)
	if err := m.beadsFor(issueID, workDir).Update(issueID, beads.UpdateOptions{Status: &status, Assignee: &agentID}); err != nil {
		return fmt.Errorf("bd update failed: %w", err)
	}
	fmt.Printf("✓ Hooked issue %s to %s\n", issueID, agentID)
	return nil
}
