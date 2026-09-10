package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// SyncRemoteOwnerCheck verifies that each rig's .beads/config.yaml sync.remote
// (when set) names the same GitHub owner as the rig's git origin remote.
//
// bd's push-adoption path (adoptGitOriginRemoteForPush) can route a Dolt push
// to whatever sync.remote names. A stale value left over from a fork clone —
// e.g. sync.remote still pointing at the upstream owner instead of the fork's
// origin owner — silently routes Dolt pushes to the wrong repo. (gt-15ry)
type SyncRemoteOwnerCheck struct {
	BaseCheck
}

// NewSyncRemoteOwnerCheck creates a new sync.remote owner check.
func NewSyncRemoteOwnerCheck() *SyncRemoteOwnerCheck {
	return &SyncRemoteOwnerCheck{
		BaseCheck: BaseCheck{
			CheckName:        "sync-remote-owner",
			CheckDescription: "Verify .beads/config.yaml sync.remote owner matches git origin owner",
			CheckCategory:    CategoryRig,
		},
	}
}

// Run checks every rig's sync.remote (if configured) against its git origin.
func (c *SyncRemoteOwnerCheck) Run(ctx *CheckContext) *CheckResult {
	townBeadsDir := filepath.Join(ctx.TownRoot, ".beads")
	routes, err := beads.LoadRoutes(townBeadsDir)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "Could not load routes.jsonl",
		}
	}

	// Build unique rig list from routes, same pattern as IdleTimeoutCheck.
	rigSet := make(map[string]string) // rigName -> beadsPath (relative to town root)
	for _, r := range routes {
		parts := strings.Split(r.Path, "/")
		if len(parts) >= 1 && parts[0] != "." {
			rigName := parts[0]
			if _, exists := rigSet[rigName]; !exists {
				rigSet[rigName] = r.Path
			}
		}
	}

	if len(rigSet) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No rigs to check",
		}
	}

	var mismatches []string
	var checked int

	for rigName, beadsPath := range rigSet {
		rigPath := filepath.Join(ctx.TownRoot, beadsPath)
		configPath := filepath.Join(rigPath, ".beads", "config.yaml")

		remote, ok := readSyncRemote(configPath)
		if !ok {
			continue // sync.remote not set — nothing to check
		}

		remoteOwner, err := githubOwnerFromURL(remote)
		if err != nil {
			continue // not a recognizable GitHub URL — not this check's business
		}

		originOwner, err := gitOriginOwner(rigPath)
		if err != nil {
			continue // no git repo / no origin here — nothing to compare against
		}

		checked++
		if !strings.EqualFold(remoteOwner, originOwner) {
			mismatches = append(mismatches, fmt.Sprintf(
				"%s: sync.remote owner %q != git origin owner %q", rigName, remoteOwner, originOwner))
		}
	}

	if len(mismatches) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("sync.remote owner matches git origin for all %d checked rig(s)", checked),
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusError,
		Message: fmt.Sprintf("%d rig(s) have sync.remote pointing at a different GitHub owner than git origin", len(mismatches)),
		Details: mismatches,
		FixHint: "A mismatched sync.remote can route 'bd dolt push' to the wrong repo (e.g. upstream instead of a fork). Update .beads/config.yaml's sync.remote to match git origin.",
	}
}

// readSyncRemote reads the sync.remote value from a beads config.yaml, if
// present and non-empty. Deliberately line-oriented (not a full YAML parse)
// to match how bd itself treats this file — see beadsConfigHasSyncRemote in
// internal/rig/manager.go.
func readSyncRemote(configPath string) (string, bool) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || !strings.HasPrefix(trimmed, "sync.remote:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(trimmed, "sync.remote:"))
		value = strings.Trim(value, `"`)
		if value == "" {
			return "", false
		}
		return value, true
	}
	return "", false
}

// githubOwnerFromURL extracts the GitHub owner (org or user) from a git
// remote URL, tolerating bd's "git+" scheme prefix used in sync.remote
// (e.g. "git+https://github.com/owner/repo.git").
func githubOwnerFromURL(raw string) (string, error) {
	url := strings.TrimPrefix(strings.TrimSpace(raw), "git+")
	url = strings.TrimSuffix(url, ".git")

	switch {
	case strings.HasPrefix(url, "git@github.com:"):
		url = strings.TrimPrefix(url, "git@github.com:")
	case strings.Contains(url, "github.com/"):
		_, after, _ := strings.Cut(url, "github.com/")
		url = after
	default:
		return "", fmt.Errorf("not a recognizable GitHub URL: %q", raw)
	}

	owner, _, _ := strings.Cut(strings.Trim(url, "/"), "/")
	if owner == "" {
		return "", fmt.Errorf("could not extract owner from URL: %q", raw)
	}
	return owner, nil
}

// gitOriginOwner returns the GitHub owner of rigPath's git origin remote.
func gitOriginOwner(rigPath string) (string, error) {
	cmd := exec.Command("git", "remote", "get-url", "origin")
	cmd.Dir = rigPath
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return githubOwnerFromURL(strings.TrimSpace(string(out)))
}
