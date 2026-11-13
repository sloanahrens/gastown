// Package templates provides embedded templates for role contexts.
package templates

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/steveyegge/gastown/internal/cli"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/templates/commands"
)

// templateFuncs provides custom functions for templates.
var templateFuncs = template.FuncMap{
	"cmd": cli.Name, // {{ cmd }} returns the CLI command name (GT_COMMAND, else gt)
}

//go:embed roles/*.md.tmpl
var templateFS embed.FS

//go:embed systemd/*.service
var supervisorFS embed.FS

//go:embed polecat-CLAUDE.md
var polecatCLAUDEmd string

// Templates manages role templates.
type Templates struct {
	roleTemplates *template.Template
}

// RoleData contains information for rendering role contexts.
type RoleData struct {
	Role          string // polecat, crew
	RigName       string // e.g., "greenplace"
	TownRoot      string // e.g., "/Users/user/ai"
	TownName      string // e.g., "ai" - the town identifier for session names
	WorkDir       string // current working directory
	DefaultBranch string // default branch for merges (e.g., "main", "develop")
	IsForkRig     bool   // true when rig config has upstream_url
	UpstreamURL   string // redacted upstream URL for display only
	Polecat       string // polecat name (for polecat role)
	BeadsDir      string // BEADS_DIR path
	IssuePrefix   string // beads issue prefix
}

// SupervisorData contains information for rendering supervisor templates.
type SupervisorData struct {
	GTPath   string            // Path to the gt binary
	TownRoot string            // Path to the Gas Town workspace
	Env      map[string]string // Extra environment variables from settings/daemon.env
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

	return t, nil
}

// RenderRole renders a role context template.
func (t *Templates) RenderRole(role string, data RoleData) (string, error) {
	return t.render(role, data)
}

func (t *Templates) render(role string, data any) (string, error) {
	templateName := role + ".md.tmpl"

	var buf bytes.Buffer
	if err := t.roleTemplates.ExecuteTemplate(&buf, templateName, data); err != nil {
		return "", fmt.Errorf("rendering role template %s: %w", templateName, err)
	}

	return buf.String(), nil
}

// RoleNames returns the list of available role templates.
func (t *Templates) RoleNames() []string {
	return []string{"polecat", "crew"}
}

// PolecatLifecycleMarker is a unique string present in the polecat CLAUDE.md
// template, and the one statement of the overlay rule: a root CLAUDE.md that
// carries it is the rig-written Gas Town overlay, not repo source. gt done
// unstages such a file, the submission path strips it from the branch, and the
// uncommitted-work check (internal/git hasOverlayCLAUDE) ignores it. If an
// existing CLAUDE.md lacks the marker, polecat lifecycle instructions are
// appended: the agent won't know to call `gt done` otherwise.
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
// A command on disk that differs from the embedded template is replaced; the
// binary is canonical (gt-4czhp).
func ProvisionCommands(workspacePath string) error {
	return commands.Provision(workspacePath)
}

// CommandNames returns the list of embedded slash commands.
func CommandNames() []string {
	return commands.Names()
}

// MissingCommands returns the list of embedded commands missing from the workspace.
func MissingCommands(workspacePath string) []string {
	return commands.Missing(workspacePath)
}

// StaleCommands returns the list of commands present in the workspace whose
// body no longer matches the embedded template (gt-w9afa).
func StaleCommands(workspacePath string) []string {
	return commands.Stale(workspacePath)
}

// ProvisionSupervisor creates and configures supervisor files for the daemon.
// On Linux: creates and enables a systemd user unit.
// On macOS: writes nothing. gt no longer provisions a launchd job; a macOS town
// runs its daemon in the container.
// Returns a message indicating what action was taken (or skipped).
//
// Reads settings/daemon.env (if present) and carries its KEY=VALUE pairs into
// the supervisor's Environment= so a systemd-spawned daemon gets the same
// host-specific env vars a manually-started one inherits from the operator's
// shell. GT_TOWN_ROOT is always set from townRoot directly and cannot be
// overridden by the env file.
func ProvisionSupervisor(townRoot string) (string, error) {
	return realHost.provisionSupervisor(townRoot)
}

// provisionSupervisor is ProvisionSupervisor for the host h describes.
func (h supervisorHost) provisionSupervisor(townRoot string) (string, error) {
	data, err := supervisorData(townRoot)
	if err != nil {
		return "", err
	}

	switch h.goos {
	case "linux":
		return provisionSystemd(data)
	default:
		return fmt.Sprintf("Supervisor auto-configuration skipped on %s (not supported yet)", h.goos), nil
	}
}

// supervisorData builds the render inputs for the town at townRoot. Both the
// provision paths and SupervisorFileContent go through it, so that a file
// rewritten from the running binary is byte-for-byte the file a fresh
// provision would write: "what is current" has to mean one thing.
func supervisorData(townRoot string) (SupervisorData, error) {
	gtPath, err := os.Executable()
	if err != nil {
		return SupervisorData{}, fmt.Errorf("finding gt executable: %w", err)
	}

	env, err := config.LoadDaemonEnv(townRoot)
	if err != nil {
		return SupervisorData{}, fmt.Errorf("loading daemon env: %w", err)
	}
	delete(env, "GT_TOWN_ROOT")
	// A token in daemon.env stays there: the supervisor file is not a secret
	// store, and spawn reads the token from daemon.env itself (gt-y3pgh.5).
	for k, v := range env {
		if config.LooksLikeSecret(k, v) {
			delete(env, k)
		}
	}

	return SupervisorData{
		GTPath:   gtPath,
		TownRoot: townRoot,
		Env:      env,
	}, nil
}

// SupervisorFileContent renders the supervisor file this binary would install
// for the town at townRoot under the supervisor of the given kind ("systemd"),
// from the same data ProvisionSupervisor renders with. ok is false for a kind
// this build does not write — the launchd kind, which gt no longer writes, and
// the empty kind a host with no supported supervisor reports.
//
// It answers "what does this binary think the file should say", which a caller
// can use to see the difference a provision would make without installing
// anything.
func SupervisorFileContent(kind, townRoot string) (content string, ok bool, err error) {
	if kind != "systemd" {
		return "", false, nil
	}

	data, err := supervisorData(townRoot)
	if err != nil {
		return "", false, err
	}

	content, err = renderSystemdUnit(data)
	if err != nil {
		return "", false, err
	}
	return content, true, nil
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

// SystemdUnitPath returns the path where the systemd user unit is (or would
// be) installed on Linux.
func SystemdUnitPath() (string, error) {
	return realHost.systemdUnitPath()
}

func (h supervisorHost) systemdUnitPath() (string, error) {
	dataHome := h.getenv("XDG_DATA_HOME")
	if dataHome == "" {
		homeDir, err := h.homeDir()
		if err != nil {
			return "", fmt.Errorf("finding home directory: %w", err)
		}
		dataHome = filepath.Join(homeDir, ".local", "share")
	}
	return filepath.Join(dataHome, "systemd", "user", "gastown-daemon.service"), nil
}

// SupervisorStatus reports which external supervisor is configured for the
// daemon on this host, by checking for the presence of the systemd user unit
// (Linux). Returns "systemd" or "none"; a macOS host has no supervisor file
// any longer. Never errors — a path resolution failure is treated as "none".
// This is the file-presence reading; SupervisorStatusLine is the one that
// says what the job is doing (gt-sq9e).
func SupervisorStatus() string {
	return realHost.status()
}

func (h supervisorHost) status() string {
	path, kind := h.filePath()
	if path == "" || kind == "" {
		return "none"
	}
	if _, err := os.Stat(path); err == nil {
		return kind
	}
	return "none"
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
