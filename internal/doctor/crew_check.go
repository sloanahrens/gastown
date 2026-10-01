package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CrewStateCheck validates crew worker state.json files for completeness.
// Empty or incomplete state.json files cause "can't find pane/session" errors.
type CrewStateCheck struct {
	FixableCheck
	invalidCrews []invalidCrew // Cached during Run for use in Fix
}

type invalidCrew struct {
	path      string
	stateFile string
	rigName   string
	crewName  string
	issue     string
}

// NewCrewStateCheck creates a new crew state check.
func NewCrewStateCheck() *CrewStateCheck {
	return &CrewStateCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "crew-state",
				CheckDescription: "Validate crew worker state.json files",
				CheckCategory:    CategoryCleanup,
			},
		},
	}
}

// Run checks all crew state.json files for completeness.
func (c *CrewStateCheck) Run(ctx *CheckContext) *CheckResult {
	c.invalidCrews = nil

	crewDirs := c.findAllCrewDirs(ctx.TownRoot)
	if len(crewDirs) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No crew workspaces found",
		}
	}

	var validCount int
	var details []string

	for _, cd := range crewDirs {
		stateFile := filepath.Join(cd.path, "state.json")

		// Check if state.json exists
		data, err := os.ReadFile(stateFile)
		if err != nil {
			if os.IsNotExist(err) {
				// Missing state file is OK - code will use defaults
				validCount++
				continue
			}
			// Other errors are problems
			issue := fmt.Sprintf("cannot read state.json: %v", err)
			c.invalidCrews = append(c.invalidCrews, invalidCrew{
				path:      cd.path,
				stateFile: stateFile,
				rigName:   cd.rigName,
				crewName:  cd.crewName,
				issue:     issue,
			})
			details = append(details, fmt.Sprintf("%s/%s: %s", cd.rigName, cd.crewName, issue))
			continue
		}

		// Parse state.json
		var state struct {
			Name      string `json:"name"`
			Rig       string `json:"rig"`
			ClonePath string `json:"clone_path"`
		}
		if err := json.Unmarshal(data, &state); err != nil {
			issue := "invalid JSON in state.json"
			c.invalidCrews = append(c.invalidCrews, invalidCrew{
				path:      cd.path,
				stateFile: stateFile,
				rigName:   cd.rigName,
				crewName:  cd.crewName,
				issue:     issue,
			})
			details = append(details, fmt.Sprintf("%s/%s: %s", cd.rigName, cd.crewName, issue))
			continue
		}

		// Check for empty/incomplete state
		var issues []string
		if state.Name == "" {
			issues = append(issues, "missing name")
		}
		if state.Rig == "" {
			issues = append(issues, "missing rig")
		}
		if state.ClonePath == "" {
			issues = append(issues, "missing clone_path")
		}

		if len(issues) > 0 {
			issue := strings.Join(issues, ", ")
			c.invalidCrews = append(c.invalidCrews, invalidCrew{
				path:      cd.path,
				stateFile: stateFile,
				rigName:   cd.rigName,
				crewName:  cd.crewName,
				issue:     issue,
			})
			details = append(details, fmt.Sprintf("%s/%s: %s", cd.rigName, cd.crewName, issue))
		} else {
			validCount++
		}
	}

	if len(c.invalidCrews) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: fmt.Sprintf("All %d crew state files valid", validCount),
		}
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d crew workspace(s) with invalid state.json", len(c.invalidCrews)),
		Details: details,
		FixHint: "Run 'gt doctor fix crew-state' to regenerate state files",
	}
}

// Fix regenerates invalid state.json files with correct values.
func (c *CrewStateCheck) Fix(ctx *CheckContext) error {
	if len(c.invalidCrews) == 0 {
		return nil
	}

	var lastErr error
	for _, ic := range c.invalidCrews {
		state := map[string]interface{}{
			"name":       ic.crewName,
			"rig":        ic.rigName,
			"clone_path": ic.path,
			"branch":     "main",
			"created_at": time.Now().Format(time.RFC3339),
			"updated_at": time.Now().Format(time.RFC3339),
		}

		data, err := json.MarshalIndent(state, "", "  ")
		if err != nil {
			lastErr = fmt.Errorf("%s/%s: %w", ic.rigName, ic.crewName, err)
			continue
		}

		if err := os.WriteFile(ic.stateFile, data, 0644); err != nil {
			lastErr = fmt.Errorf("%s/%s: %w", ic.rigName, ic.crewName, err)
			continue
		}
	}

	return lastErr
}

type crewDir struct {
	path     string
	rigName  string
	crewName string
}

// findAllCrewDirs finds all crew directories in the workspace.
func (c *CrewStateCheck) findAllCrewDirs(townRoot string) []crewDir {
	var dirs []crewDir

	entries, err := os.ReadDir(townRoot)
	if err != nil {
		return dirs
	}

	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || entry.Name() == "mayor" {
			continue
		}

		rigName := entry.Name()
		crewPath := filepath.Join(townRoot, rigName, "crew")

		crewEntries, err := os.ReadDir(crewPath)
		if err != nil {
			continue
		}

		for _, crew := range crewEntries {
			if !crew.IsDir() || strings.HasPrefix(crew.Name(), ".") {
				continue
			}
			dirs = append(dirs, crewDir{
				path:     filepath.Join(crewPath, crew.Name()),
				rigName:  rigName,
				crewName: crew.Name(),
			})
		}
	}

	return dirs
}

// crewCloneDirs returns the crew member directories under crewDir, sorted.
// A crew member is the clone `gt crew add` makes, so dot-dirs (the shared
// crew/.claude) are skipped, and so is a linked git worktree (.git is a
// file): operators keep throwaway worktrees of a crew clone beside it
// (crew/sloan-<topic>), and those are not crew members.
func crewCloneDirs(crewDir string) []string {
	entries, err := os.ReadDir(crewDir)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path := filepath.Join(crewDir, entry.Name())
		if info, err := os.Stat(filepath.Join(path, ".git")); err == nil && !info.IsDir() {
			continue
		}
		dirs = append(dirs, path)
	}
	return dirs
}
