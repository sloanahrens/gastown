// Package templates provides embedded templates for role contexts and messages.
package templates

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/templates/commands"
)

var (
	cmdName     string
	cmdNameOnce sync.Once
)

// CmdName returns the Gas Town CLI command name.
// Defaults to "gt", but can be overridden with GT_COMMAND env var.
// This allows coexistence with other tools that use "gt" (e.g., Graphite).
func CmdName() string {
	cmdNameOnce.Do(func() {
		cmdName = os.Getenv("GT_COMMAND")
		if cmdName == "" {
			cmdName = "gt"
		}
	})
	return cmdName
}

// templateFuncs provides custom functions for templates.
var templateFuncs = template.FuncMap{
	"cmd": CmdName, // {{ cmd }} returns the CLI command name
}

//go:embed roles/*.md.tmpl messages/*.md.tmpl
var templateFS embed.FS

//go:embed launchd/*.plist systemd/*.service
var supervisorFS embed.FS

//go:embed polecat-CLAUDE.md
var polecatCLAUDEmd string

// Templates manages role and message templates.
type Templates struct {
	roleTemplates    *template.Template
	messageTemplates *template.Template
}

// RoleData contains information for rendering role contexts.
type RoleData struct {
	Role            string   // mayor, witness, refinery, polecat, crew, deacon
	RigName         string   // e.g., "greenplace"
	TownRoot        string   // e.g., "/Users/steve/ai"
	TownName        string   // e.g., "ai" - the town identifier for session names
	WorkDir         string   // current working directory
	DefaultBranch   string   // default branch for merges (e.g., "main", "develop")
	IsForkRig       bool     // true when rig config has upstream_url
	UpstreamURL     string   // redacted upstream URL for display only
	Polecat         string   // polecat name (for polecat role)
	Polecats        []string // list of polecats (for witness role)
	DogName         string   // dog name (for dog role)
	BeadsDir        string   // BEADS_DIR path
	IssuePrefix     string   // beads issue prefix
	MayorSession    string   // e.g., "gt-ai-mayor" - dynamic mayor session name
	DeaconSession   string   // e.g., "gt-ai-deacon" - dynamic deacon session name
	PatrolStepCount int      // resolved step count of mol-deacon-patrol, 0 if unresolved (deacon role only)
}

// SpawnData contains information for spawn assignment messages.
type SpawnData struct {
	Issue       string
	Title       string
	Priority    int
	Description string
	Branch      string
	RigName     string
	Polecat     string
}

// NudgeData contains information for nudge messages.
type NudgeData struct {
	Polecat    string
	Reason     string
	NudgeCount int
	MaxNudges  int
	Issue      string
	Status     string
}

// EscalationData contains information for escalation messages.
type EscalationData struct {
	Polecat     string
	Issue       string
	Reason      string
	NudgeCount  int
	LastStatus  string
	Suggestions []string
}

// HandoffData contains information for session handoff messages.
type HandoffData struct {
	Role        string
	CurrentWork string
	Status      string
	NextSteps   []string
	Notes       string
	PendingMail int
	GitBranch   string
	GitDirty    bool
}

// SupervisorData contains information for rendering supervisor templates.
type SupervisorData struct {
	GTPath   string            // Path to the gt binary
	TownRoot string            // Path to the Gas Town workspace
	Env      map[string]string // Extra environment variables from settings/daemon.env

	// ExitTimeOutSeconds is launchd's ExitTimeOut for the daemon job: how long
	// launchd waits after SIGTERM (sent by `launchctl kickstart -k`, i.e. `gt
	// daemon restart`) before it SIGKILLs the process. It must be at least as
	// large as the daemon's own worst-case graceful shutdown (see
	// daemon.ShutdownBudget) — otherwise launchd force-kills the daemon
	// mid-shutdown on every restart, which for the Dolt SQL server step in
	// particular risks corrupting its append-only journal instead of letting
	// it exit cleanly. 0 omits the key, which leaves launchd's own default
	// (20s) in effect.
	ExitTimeOutSeconds int
}

// New creates a new Templates instance.
func New() (*Templates, error) {
	t := &Templates{}

	// Parse role templates with custom functions
	roleTempl, err := template.New("").Funcs(templateFuncs).ParseFS(templateFS, "roles/*.md.tmpl")
	if err != nil {
		return nil, fmt.Errorf("parsing role templates: %w", err)
	}
	t.roleTemplates = roleTempl

	// Parse message templates with custom functions
	msgTempl, err := template.New("").Funcs(templateFuncs).ParseFS(templateFS, "messages/*.md.tmpl")
	if err != nil {
		return nil, fmt.Errorf("parsing message templates: %w", err)
	}
	t.messageTemplates = msgTempl

	return t, nil
}

// RenderRole renders a role context template.
func (t *Templates) RenderRole(role string, data RoleData) (string, error) {
	templateName := role + ".md.tmpl"

	var buf bytes.Buffer
	if err := t.roleTemplates.ExecuteTemplate(&buf, templateName, data); err != nil {
		return "", fmt.Errorf("rendering role template %s: %w", templateName, err)
	}

	return buf.String(), nil
}

// RenderMessage renders a message template.
func (t *Templates) RenderMessage(name string, data interface{}) (string, error) {
	templateName := name + ".md.tmpl"

	var buf bytes.Buffer
	if err := t.messageTemplates.ExecuteTemplate(&buf, templateName, data); err != nil {
		return "", fmt.Errorf("rendering message template %s: %w", templateName, err)
	}

	return buf.String(), nil
}

// RoleNames returns the list of available role templates.
func (t *Templates) RoleNames() []string {
	return []string{"mayor", "witness", "refinery", "polecat", "crew", "deacon", "boot"}
}

// MessageNames returns the list of available message templates.
func (t *Templates) MessageNames() []string {
	return []string{"spawn", "nudge", "escalation", "handoff"}
}

// CreateMayorCLAUDEmd creates the Mayor's CLAUDE.md file at the specified directory.
// This is used by both gt install and gt doctor --fix.
//
// Returns (created bool, error) - created is false if file already exists.
// Existing files are preserved to respect user customizations.
func CreateMayorCLAUDEmd(mayorDir, townRoot, townName, mayorSession, deaconSession string) (bool, error) {
	claudePath := filepath.Join(mayorDir, "CLAUDE.md")

	// Check if file already exists - preserve user customizations
	if _, err := os.Stat(claudePath); err == nil {
		return false, nil // File exists, preserve it
	} else if !os.IsNotExist(err) {
		return false, err // Unexpected error
	}

	tmpl, err := New()
	if err != nil {
		return false, err
	}

	data := RoleData{
		Role:          "mayor",
		TownRoot:      townRoot,
		TownName:      townName,
		WorkDir:       mayorDir,
		MayorSession:  mayorSession,
		DeaconSession: deaconSession,
	}

	content, err := tmpl.RenderRole("mayor", data)
	if err != nil {
		return false, err
	}

	return true, os.WriteFile(claudePath, []byte(content), 0644)
}

// PolecatLifecycleMarker is a unique string present in the polecat CLAUDE.md
// template. Used to detect whether a CLAUDE.md file contains the Gas Town
// overlay (vs. project-specific content). If an existing CLAUDE.md lacks this
// marker, polecat lifecycle instructions are appended — the agent won't know
// to call `gt done` otherwise.
const PolecatLifecycleMarker = "IDLE POLECAT HERESY"

// CreatePolecatCLAUDEmd writes the polecat CLAUDE.md template to the worktree.
// This is the primary mechanism for polecats to learn about `gt done` and other
// lifecycle commands — the file persists across compaction and session restarts.
//
// If the worktree already has a tracked CLAUDE.md (e.g., from the rig's repo),
// polecat lifecycle instructions are written to CLAUDE.local.md instead. This
// avoids creating uncommitted changes in the tracked CLAUDE.md, which the
// gt done auto-save safety net would otherwise commit onto the polecat's branch,
// polluting the PR diff with hundreds of lines of agent context.
//
// If no CLAUDE.md exists, the full template is written to CLAUDE.md.
//
// Returns (created bool, error).
func CreatePolecatCLAUDEmd(worktreePath, rigName, polecatName string) (bool, error) {
	claudePath := filepath.Join(worktreePath, "CLAUDE.md")
	claudeLocalPath := filepath.Join(worktreePath, "CLAUDE.local.md")

	// Render the polecat template with rig/name substitutions
	content := polecatCLAUDEmd
	content = strings.ReplaceAll(content, "{{rig}}", rigName)
	content = strings.ReplaceAll(content, "{{name}}", polecatName)

	// Check if lifecycle instructions are already present in either file.
	for _, path := range []string{claudePath, claudeLocalPath} {
		if existing, err := os.ReadFile(path); err == nil {
			if strings.Contains(string(existing), PolecatLifecycleMarker) {
				return false, nil // Already has our instructions
			}
		}
	}

	// If CLAUDE.md exists (tracked repo file), write to CLAUDE.local.md instead
	// to avoid polluting the tracked file with polecat context. CLAUDE.local.md
	// is gitignored in standard rig repos and is still loaded by Claude Code.
	if _, err := os.Stat(claudePath); err == nil {
		existingLocal, readErr := os.ReadFile(claudeLocalPath)
		if readErr == nil {
			// Append to existing CLAUDE.local.md
			merged := string(existingLocal) + "\n---\n\n" + content
			return true, os.WriteFile(claudeLocalPath, []byte(merged), 0644)
		}
		// Write new CLAUDE.local.md with just polecat context
		return true, os.WriteFile(claudeLocalPath, []byte(content), 0644)
	}

	// No CLAUDE.md — write the full template there
	return true, os.WriteFile(claudePath, []byte(content), 0644)
}

// ProvisionCommands creates the .claude/commands/ directory with standard slash commands.
// This ensures crew/polecat workspaces have the handoff command and other utilities
// even if the source repo doesn't have them tracked.
// If a command already exists, it is skipped (no overwrite).
func ProvisionCommands(workspacePath string) error {
	return commands.ProvisionFor(workspacePath, "claude")
}

// ProvisionCommandsFor provisions commands for a specific agent.
func ProvisionCommandsFor(workspacePath, agent string) error {
	return commands.ProvisionFor(workspacePath, agent)
}

// CommandNames returns the list of embedded slash commands.
func CommandNames() []string {
	return commands.Names()
}

// HasCommands checks if a workspace has the .claude/commands/ directory provisioned.
func HasCommands(workspacePath string) bool {
	return HasCommandsFor(workspacePath, "claude")
}

// HasCommandsFor checks if a workspace has commands provisioned for an agent.
func HasCommandsFor(workspacePath, agent string) bool {
	return len(commands.MissingFor(workspacePath, agent)) == 0
}

// MissingCommands returns the list of embedded commands missing from the workspace.
func MissingCommands(workspacePath string) []string {
	return commands.MissingFor(workspacePath, "claude")
}

// MissingCommandsFor returns missing commands for a specific agent.
func MissingCommandsFor(workspacePath, agent string) []string {
	return commands.MissingFor(workspacePath, agent)
}

// ProvisionSupervisor creates and configures supervisor files for the daemon.
// On macOS: creates and loads a launchd plist.
// On Linux: creates and enables a systemd user unit.
// Returns a message indicating what action was taken (or skipped).
//
// Reads settings/daemon.env (if present) and carries its KEY=VALUE pairs into
// the supervisor's EnvironmentVariables/Environment= so a launchd/systemd-
// spawned daemon gets the same host-specific env vars a manually-started one
// inherits from the operator's shell. GT_TOWN_ROOT is always set from
// townRoot directly and cannot be overridden by the env file.
//
// exitTimeout becomes the launchd job's ExitTimeOut (see SupervisorData); the
// caller passes daemon.ShutdownBudget so the two stay derived from the same
// real value instead of drifting independently. A zero exitTimeout leaves
// launchd's own default in effect (only meaningful on darwin).
func ProvisionSupervisor(townRoot string, exitTimeout time.Duration) (string, error) {
	data, err := supervisorData(townRoot, exitTimeout)
	if err != nil {
		return "", err
	}

	switch runtime.GOOS {
	case "darwin":
		return provisionLaunchd(data)
	case "linux":
		return provisionSystemd(data)
	default:
		return fmt.Sprintf("Supervisor auto-configuration skipped on %s (not supported yet)", runtime.GOOS), nil
	}
}

// supervisorData builds the render inputs for the town at townRoot. Both the
// provision paths and SupervisorFileContent go through it, so that a file
// rewritten from the running binary is byte-for-byte the file a fresh
// provision would write: "what is current" has to mean one thing.
func supervisorData(townRoot string, exitTimeout time.Duration) (SupervisorData, error) {
	gtPath, err := os.Executable()
	if err != nil {
		return SupervisorData{}, fmt.Errorf("finding gt executable: %w", err)
	}

	env, err := config.LoadDaemonEnv(townRoot)
	if err != nil {
		return SupervisorData{}, fmt.Errorf("loading daemon env: %w", err)
	}
	delete(env, "GT_TOWN_ROOT")

	return SupervisorData{
		GTPath:             gtPath,
		TownRoot:           townRoot,
		Env:                env,
		ExitTimeOutSeconds: int(exitTimeout / time.Second),
	}, nil
}

// SupervisorFileContent renders the supervisor file this binary would install
// for the town at townRoot under the supervisor of the given kind
// ("launchd"/"systemd"), from the same data ProvisionSupervisor renders with.
// ok is false for a kind this build does not write — including the empty kind
// a host with no supported supervisor reports.
//
// It answers "what does this binary think the file should say", which
// SupervisorFileRepair needs to ask, and which a caller can use to see the
// difference a provision would make without installing anything.
func SupervisorFileContent(kind, townRoot string, exitTimeout time.Duration) (content string, ok bool, err error) {
	switch kind {
	case "launchd", "systemd":
	default:
		return "", false, nil
	}

	data, err := supervisorData(townRoot, exitTimeout)
	if err != nil {
		return "", false, err
	}

	if kind == "launchd" {
		content, err = renderLaunchdPlist(data)
	} else {
		content, err = renderSystemdUnit(data)
	}
	if err != nil {
		return "", false, err
	}
	return content, true, nil
}

// SupervisorFileRepair returns the content that should replace the supervisor
// file at path, and whether there is a repair to make at all (gt-x872).
//
// The file at path describes a job run out of townRoot; the caller has already
// established that it is this town's. A repair is a rewrite of the ONE value
// in it that is compiled into the binary that wrote it — launchd's
// ExitTimeOut, which comes from daemon.ShutdownBudget. It goes stale on a
// binary upgrade and nowhere else, and it is not cosmetic: a job whose
// ExitTimeOut is shorter than the daemon's own graceful shutdown gets its
// daemon SIGKILLed mid-shutdown on every restart, which for the Dolt SQL
// server step in particular risks its journal. That value has to follow the
// binary, so a file written by an older one has to be repairable without
// re-provisioning the whole job.
//
// Anything else that differs means the file is not this binary's rendering of
// this town — a different gt path, a different town, a job someone wrote by
// hand, an env file that changed since it was written — and none of it is
// repaired here. Repointing a launchd job at whichever gt happens to be
// running is a reconfiguration, not a repair; `gt daemon enable-supervisor`
// is the explicit way to re-provision a file from scratch.
//
// On Linux this is always ("", false, nil): the systemd unit renders no value
// from the binary's own constants, so there is nothing for a binary upgrade to
// leave behind.
//
// It only renders. Installing the content and making the running job read it
// are the caller's, and separate: a service manager caches a job definition
// when it loads it, so a rewritten file reaches a loaded job only through an
// unload and a load.
func SupervisorFileRepair(path, kind, townRoot string, exitTimeout time.Duration) (content string, repair bool, err error) {
	if kind != "launchd" {
		return "", false, nil
	}
	isFor, err := SupervisorFileIsFor(path, townRoot)
	if err != nil || !isFor {
		return "", false, err
	}

	installed, err := os.ReadFile(path) //nolint:gosec // fixed per-user path from this package
	if err != nil {
		return "", false, fmt.Errorf("reading supervisor file %s: %w", path, err)
	}

	current, ok, err := SupervisorFileContent(kind, townRoot, exitTimeout)
	if err != nil || !ok {
		return "", false, err
	}

	if string(installed) == current {
		return "", false, nil
	}
	if withExitTimeOutStripped(string(installed)) != withExitTimeOutStripped(current) {
		return "", false, nil
	}
	return current, true, nil
}

// withExitTimeOutStripped removes the ExitTimeOut key and its integer from a
// rendered plist, so that two plists differing only in that value compare
// equal. Matching is line-based against what renderLaunchdPlist produces —
// the key on its own line, the value on the next — so a file formatted some
// other way (the key and value on one line) keeps its difference and is not
// treated as a repairable one.
func withExitTimeOutStripped(plist string) string {
	lines := strings.Split(plist, "\n")
	kept := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "<key>ExitTimeOut</key>" {
			i++ // the value line goes with the key
			continue
		}
		kept = append(kept, lines[i])
	}
	return strings.Join(kept, "\n")
}

// renderLaunchdPlist renders the launchd plist template for the given data.
func renderLaunchdPlist(data SupervisorData) (string, error) {
	templateContent, err := supervisorFS.ReadFile("launchd/com.gastown.daemon.plist")
	if err != nil {
		return "", fmt.Errorf("reading launchd template: %w", err)
	}

	tmpl, err := template.New("launchd").Parse(string(templateContent))
	if err != nil {
		return "", fmt.Errorf("parsing launchd template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("rendering launchd template: %w", err)
	}

	return buf.String(), nil
}

// renderSystemdUnit renders the systemd user unit template for the given data.
func renderSystemdUnit(data SupervisorData) (string, error) {
	templateContent, err := supervisorFS.ReadFile("systemd/gastown-daemon.service")
	if err != nil {
		return "", fmt.Errorf("reading systemd template: %w", err)
	}

	tmpl, err := template.New("systemd").Parse(string(templateContent))
	if err != nil {
		return "", fmt.Errorf("parsing systemd template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("rendering systemd template: %w", err)
	}

	return buf.String(), nil
}

// LaunchdPlistPath returns the path where the launchd plist is (or would be)
// installed on macOS.
func LaunchdPlistPath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding home directory: %w", err)
	}
	return filepath.Join(homeDir, "Library", "LaunchAgents", "com.gastown.daemon.plist"), nil
}

// SystemdUnitPath returns the path where the systemd user unit is (or would
// be) installed on Linux.
func SystemdUnitPath() (string, error) {
	dataHome := os.Getenv("XDG_DATA_HOME")
	if dataHome == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("finding home directory: %w", err)
		}
		dataHome = filepath.Join(homeDir, ".local", "share")
	}
	return filepath.Join(dataHome, "systemd", "user", "gastown-daemon.service"), nil
}

// SupervisorStatus reports which external supervisor is configured for the
// daemon on this host, by checking for the presence of the launchd plist
// (macOS) or systemd user unit (Linux). Returns "launchd", "systemd", or
// "none". Never errors — a path resolution failure is treated as "none".
// This is the file-presence reading; SupervisorStatusLine is the one that
// says what the job is doing (gt-sq9e).
func SupervisorStatus() string {
	path, kind := SupervisorFilePath()
	if path == "" || kind == "" {
		return "none"
	}
	if _, err := os.Stat(path); err == nil {
		return kind
	}
	return "none"
}

// provisionLaunchd creates and loads a launchd plist on macOS.
func provisionLaunchd(data SupervisorData) (string, error) {
	plistPath, err := LaunchdPlistPath()
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(plistPath), 0755); err != nil {
		return "", fmt.Errorf("creating LaunchAgents directory: %w", err)
	}

	content, err := renderLaunchdPlist(data)
	if err != nil {
		return "", err
	}

	// Write plist file
	if err := os.WriteFile(plistPath, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("writing plist file: %w", err)
	}

	// Unload if already loaded (ignore errors)
	_ = exec.Command("launchctl", "unload", plistPath).Run()

	// Load the service
	if output, err := exec.Command("launchctl", "load", plistPath).CombinedOutput(); err != nil {
		return "", fmt.Errorf("loading launchd service: %s", string(output))
	}

	return "Created and loaded launchd service: com.gastown.daemon", nil
}

// provisionSystemd creates and enables a systemd user unit on Linux.
func provisionSystemd(data SupervisorData) (string, error) {
	servicePath, err := SystemdUnitPath()
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(servicePath), 0755); err != nil {
		return "", fmt.Errorf("creating systemd user directory: %w", err)
	}

	content, err := renderSystemdUnit(data)
	if err != nil {
		return "", err
	}

	// Write service file
	if err := os.WriteFile(servicePath, []byte(content), 0644); err != nil {
		return "", fmt.Errorf("writing service file: %w", err)
	}

	// Reload systemd daemon
	if output, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return "", fmt.Errorf("reloading systemd: %s", string(output))
	}

	// Enable the service
	if output, err := exec.Command("systemctl", "--user", "enable", "gastown-daemon.service").CombinedOutput(); err != nil {
		return "", fmt.Errorf("enabling systemd service: %s", string(output))
	}

	// Start the service
	if output, err := exec.Command("systemctl", "--user", "start", "gastown-daemon.service").CombinedOutput(); err != nil {
		return "", fmt.Errorf("starting systemd service: %s", string(output))
	}

	return "Created and enabled systemd user service: gastown-daemon.service", nil
}
