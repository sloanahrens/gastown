package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/doltserver"
)

// DoltRemoteLeftoversCheck warns while any Dolt remote state from before
// ADR 0002 is still on disk: a remote registered on a Dolt database, a
// git-remote-cache directory, or a sync.remote line in a rig's (or the
// town's) .beads/config.yaml.
//
// gastown no longer pushes or pulls Dolt remotes, so none of this is used.
// It is still worth removing: a registered remote is what any stray
// DOLT_PUSH or `bd dolt push` would push to, and the cache alone was 456 MB
// of the gt database. The check is read-only and never connects to Dolt: it
// reads each database's .dolt/repo_state.json, which is where Dolt records
// its remotes, so it works whether or not the server is up.
type DoltRemoteLeftoversCheck struct {
	BaseCheck
}

// NewDoltRemoteLeftoversCheck creates the Dolt remote leftovers check.
func NewDoltRemoteLeftoversCheck() *DoltRemoteLeftoversCheck {
	return &DoltRemoteLeftoversCheck{
		BaseCheck: BaseCheck{
			CheckName:        "dolt-remote-leftovers",
			CheckDescription: "Warn on Dolt remotes, git-remote-cache dirs and sync.remote left from remote sync",
			CheckCategory:    CategoryInfrastructure,
		},
	}
}

// doltRemoteLeftoversFixHint names the operator procedure. The check never
// removes anything itself: removing a remote is a write to a live database.
const doltRemoteLeftoversFixHint = "Dolt remote sync was removed (ADR 0002). Follow \"Removing Dolt remotes\" in " +
	"docs/design/dolt-storage.md: CALL DOLT_REMOTE('remove', ...) per database, delete .dolt/git-remote-cache, " +
	"and delete sync.remote from .beads/config.yaml."

// Run scans the Dolt data directory and every routed beads config.
func (c *DoltRemoteLeftoversCheck) Run(ctx *CheckContext) *CheckResult {
	var details []string

	dataDir := doltserver.DefaultConfig(ctx.TownRoot).DataDir
	details = append(details, scanDoltDataDirForRemotes(dataDir)...)
	details = append(details, scanBeadsConfigsForSyncRemote(ctx.TownRoot)...)

	if len(details) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No Dolt remotes, git-remote-cache dirs or sync.remote settings found",
		}
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d leftover(s) from Dolt remote sync, which is no longer used", len(details)),
		Details: details,
		FixHint: doltRemoteLeftoversFixHint,
	}
}

// scanDoltDataDirForRemotes reports every database under dataDir that has a
// remote in its repo_state.json or a git-remote-cache directory. A missing
// data directory means no databases. A file it cannot read or parse is
// reported, never read as "no remote".
func scanDoltDataDirForRemotes(dataDir string) []string {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return []string{fmt.Sprintf("%s: cannot list Dolt data dir: %v", dataDir, err)}
	}

	var details []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		db := entry.Name()
		doltDir := filepath.Join(dataDir, db, ".dolt")
		if _, err := os.Stat(doltDir); err != nil {
			continue // not a Dolt database
		}

		remotes, err := readDoltRepoStateRemotes(filepath.Join(doltDir, "repo_state.json"))
		switch {
		case err != nil:
			details = append(details, fmt.Sprintf("%s: cannot read remotes from repo_state.json: %v", db, err))
		case len(remotes) > 0:
			details = append(details, fmt.Sprintf("%s: Dolt remote(s) %s", db, strings.Join(remotes, ", ")))
		}

		cache := filepath.Join(doltDir, "git-remote-cache")
		if info, err := os.Stat(cache); err == nil && info.IsDir() {
			details = append(details, fmt.Sprintf("%s: %s exists", db, cache))
		}
	}
	return details
}

// readDoltRepoStateRemotes returns the "name=url" of each remote recorded in
// a Dolt repo_state.json, sorted by name. A missing file means no remotes.
func readDoltRepoStateRemotes(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var state struct {
		Remotes map[string]struct {
			URL string `json:"url"`
		} `json:"remotes"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	var remotes []string
	for name, r := range state.Remotes {
		remotes = append(remotes, name+"="+r.URL)
	}
	sort.Strings(remotes)
	return remotes, nil
}

// scanBeadsConfigsForSyncRemote reports each routed beads config.yaml (the
// town's own and every rig's) that still sets sync.remote.
func scanBeadsConfigsForSyncRemote(townRoot string) []string {
	routes, err := beads.LoadRoutes(filepath.Join(townRoot, ".beads"))
	if err != nil {
		return []string{fmt.Sprintf("cannot load routes.jsonl to find rig configs: %v", err)}
	}

	paths := map[string]bool{".": true} // the town's own .beads
	for _, r := range routes {
		if r.Path != "" {
			paths[r.Path] = true
		}
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)

	var details []string
	for _, p := range sorted {
		configPath := filepath.Join(townRoot, p, ".beads", "config.yaml")
		if remote, ok := readSyncRemote(configPath); ok {
			details = append(details, fmt.Sprintf("%s: sync.remote %s", configPath, remote))
		}
	}
	return details
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
		value = strings.Trim(value, `"'`)
		if value == "" {
			return "", false
		}
		return value, true
	}
	return "", false
}
