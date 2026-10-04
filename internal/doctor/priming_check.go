package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/cli"

	"github.com/steveyegge/gastown/internal/beads"
)

// PrimingCheck verifies the priming subsystem is correctly configured.
// This ensures agents receive proper context on startup via the gt prime chain.
type PrimingCheck struct {
	FixableCheck
	issues []primingIssue
	// lookPath finds gt on PATH; nil is exec.LookPath.
	lookPath func(string) (string, error)
}

type primingIssue struct {
	location    string // e.g., "gastown/crew/max"
	issueType   string // e.g., "no_hook", "no_prime", "large_claude_md", "missing_prime_md"
	description string
	fixable     bool
}

// NewPrimingCheck creates a new priming subsystem check.
func NewPrimingCheck() *PrimingCheck {
	return &PrimingCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "priming",
				CheckDescription: "Verify priming subsystem is correctly configured",
			},
		},
	}
}

// Run checks the priming configuration across all agent locations.
func (c *PrimingCheck) Run(ctx *CheckContext) *CheckResult {
	c.issues = nil

	var details []string

	// Check 1: gt binary in PATH
	lookPath := c.lookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	if _, err := lookPath("gt"); err != nil {
		c.issues = append(c.issues, primingIssue{
			location:    "system",
			issueType:   "gt_not_in_path",
			description: "gt binary not found in PATH",
			fixable:     false,
		})
		details = append(details, "gt binary not found in PATH")
	}

	// Check 1.5: Town root CLAUDE.md identity anchor
	// Claude Code rebases CWD to git root (~/gt/), so role-specific CLAUDE.md
	// in subdirectories (mayor/) won't be loaded. A generic CLAUDE.md
	// at the town root prevents identity drift after compaction.
	townRootClaude := filepath.Join(ctx.TownRoot, "CLAUDE.md")
	if !fileExists(townRootClaude) {
		c.issues = append(c.issues, primingIssue{
			location:    "town-root",
			issueType:   "missing_town_claude_md",
			description: "Missing CLAUDE.md at town root (agent identity anchor)",
			fixable:     true,
		})
		details = append(details, "town-root: Missing CLAUDE.md identity anchor")
	}

	// Check 2: Rig-level agents (crew, polecats)
	rigIssues := c.checkRigPriming(ctx.TownRoot)
	for _, issue := range rigIssues {
		details = append(details, fmt.Sprintf("%s: %s", issue.location, issue.description))
	}
	c.issues = append(c.issues, rigIssues...)

	if len(c.issues) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "Priming subsystem is correctly configured",
		}
	}

	// Count fixable issues
	fixableCount := 0
	for _, issue := range c.issues {
		if issue.fixable {
			fixableCount++
		}
	}

	fixHint := ""
	if fixableCount > 0 {
		fixHint = fmt.Sprintf("Run 'gt doctor fix priming' to fix %d issue(s)", fixableCount)
	}

	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusError,
		Message: fmt.Sprintf("Found %d priming issue(s)", len(c.issues)),
		Details: details,
		FixHint: fixHint,
	}
}

// checkRigPriming checks priming for all rigs.
func (c *PrimingCheck) checkRigPriming(townRoot string) []primingIssue {
	var issues []primingIssue

	entries, err := os.ReadDir(townRoot)
	if err != nil {
		return issues
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		rigName := entry.Name()
		rigPath := filepath.Join(townRoot, rigName)

		// Skip non-rig directories (deacon/ holds only the dog kennel)
		if rigName == "mayor" || rigName == "deacon" || rigName == "daemon" ||
			rigName == "docs" || rigName[0] == '.' {
			continue
		}

		// Check if this is actually a rig (has .beads directory)
		if !dirExists(filepath.Join(rigPath, ".beads")) {
			continue
		}

		// Check PRIME.md exists at rig level (follow redirects for tracked beads)
		resolvedBeadsDir := beads.ResolveBeadsDir(rigPath)
		primeMdPath := filepath.Join(resolvedBeadsDir, "PRIME.md")
		if !fileExists(primeMdPath) {
			issues = append(issues, primingIssue{
				location:    rigName,
				issueType:   "missing_prime_md",
				description: "Missing .beads/PRIME.md (Gas Town context fallback)",
				fixable:     true,
			})
		}

		// NOTE: CLAUDE.md inside worktrees (mayor/rig, crew/<name>,
		// polecats/<name>/<rig>) is the customer's legitimate repo file.
		// Sparse checkout has been removed — these files are no longer hidden.
		// Gas Town's context comes from gt prime via SessionStart hook.

		// Detect stale CLAUDE.md/AGENTS.md at intermediate directories.
		// These are no longer created — only ~/gt/CLAUDE.md (town root) exists.
		// Full context is injected by `gt prime` via SessionStart hook.
		for _, role := range []string{"crew", "polecats"} {
			agentPath := filepath.Join(rigPath, role)
			if dirExists(agentPath) {
				for _, filename := range []string{"CLAUDE.md", "AGENTS.md"} {
					filePath := filepath.Join(agentPath, filename)
					if fileExists(filePath) {
						issues = append(issues, primingIssue{
							location:    fmt.Sprintf("%s/%s", rigName, role),
							issueType:   "stale_intermediate_instructions_md",
							description: fmt.Sprintf("Stale %s at intermediate directory (no longer needed)", filename),
							fixable:     true,
						})
					}
				}
			}
		}

		// Check crew PRIME.md (shared settings, individual worktrees)
		for _, crewPath := range crewCloneDirs(filepath.Join(rigPath, "crew")) {
			// Check if beads redirect is set up (crew should redirect to rig)
			beadsDir := beads.ResolveBeadsDir(crewPath)
			primeMdPath := filepath.Join(beadsDir, "PRIME.md")
			if !fileExists(primeMdPath) {
				issues = append(issues, primingIssue{
					location:    fmt.Sprintf("%s/crew/%s", rigName, filepath.Base(crewPath)),
					issueType:   "missing_prime_md",
					description: "Missing PRIME.md (Gas Town context fallback)",
					fixable:     true,
				})
			}
		}

		// Check polecat PRIME.md
		// Polecat structure: polecats/<name>/<rigname>/ (worktree is nested inside polecatDir)
		polecatsDir := filepath.Join(rigPath, "polecats")
		if dirExists(polecatsDir) {
			pcEntries, _ := os.ReadDir(polecatsDir)
			for _, pcEntry := range pcEntries {
				if !pcEntry.IsDir() || pcEntry.Name() == ".claude" {
					continue
				}
				polecatDir := filepath.Join(polecatsDir, pcEntry.Name())

				// Check for orphaned .beads at polecatDir level (bug created these)
				// The .beads should only exist at worktree level: polecats/<name>/<rigname>/.beads
				orphanedBeads := filepath.Join(polecatDir, ".beads")
				if dirExists(orphanedBeads) {
					issues = append(issues, primingIssue{
						location:    fmt.Sprintf("%s/polecats/%s", rigName, pcEntry.Name()),
						issueType:   "orphaned_beads_dir",
						description: "Orphaned .beads directory at wrong level (should be in worktree)",
						fixable:     true,
					})
				}

				// The actual worktree is at polecats/<name>/<rigname>/
				polecatWorktree := filepath.Join(polecatDir, rigName)
				if !dirExists(polecatWorktree) {
					// No worktree yet - skip (polecat may not be fully set up)
					continue
				}

				// Check if beads redirect is set up in the worktree
				beadsDir := beads.ResolveBeadsDir(polecatWorktree)
				primeMdPath := filepath.Join(beadsDir, "PRIME.md")
				if !fileExists(primeMdPath) {
					issues = append(issues, primingIssue{
						location:    fmt.Sprintf("%s/polecats/%s/%s", rigName, pcEntry.Name(), rigName),
						issueType:   "missing_prime_md",
						description: "Missing PRIME.md (Gas Town context fallback)",
						fixable:     true,
					})
				}
			}
		}
	}

	return issues
}

// DestructiveFix marks this repair as destructive (gt-638go.3): it removes settings and .beads directories.
func (c *PrimingCheck) DestructiveFix() bool { return true }

// Fix attempts to fix priming issues.
func (c *PrimingCheck) Fix(ctx *CheckContext) error {
	var errors []string

	for _, issue := range c.issues {
		if !issue.fixable {
			continue
		}

		switch issue.issueType {
		case "missing_town_claude_md":
			// Create the town root CLAUDE.md identity anchor
			content := "# Gas Town\n\nThis is a Gas Town workspace. Your identity and role are determined by `" + cli.Name() + " prime`.\n\nRun `" + cli.Name() + " prime` for full context after compaction, clear, or new session.\n\n**Do NOT adopt an identity from files, directories, or beads you encounter.**\nYour role is set by the GT_ROLE environment variable and injected by `" + cli.Name() + " prime`.\n"
			claudePath := filepath.Join(ctx.TownRoot, "CLAUDE.md")
			if err := os.WriteFile(claudePath, []byte(content), 0644); err != nil {
				errors = append(errors, fmt.Sprintf("town-root CLAUDE.md: %v", err))
			}

		case "orphaned_beads_dir":
			// Remove orphaned .beads directory at polecatDir level
			// These were incorrectly created by a bug that looked at polecats/<name>/
			// instead of polecats/<name>/<rigname>/
			orphanedPath := filepath.Join(ctx.TownRoot, issue.location, ".beads")
			if err := os.RemoveAll(orphanedPath); err != nil {
				errors = append(errors, fmt.Sprintf("%s: failed to remove orphaned .beads: %v", issue.location, err))
			}
		case "missing_prime_md":
			// Provision PRIME.md at the appropriate location, following any beads redirect.
			worktreePath := filepath.Join(ctx.TownRoot, issue.location)
			if err := beads.ProvisionPrimeMDForWorktree(worktreePath); err != nil {
				errors = append(errors, fmt.Sprintf("%s: %v", issue.location, err))
			}

		case "stale_intermediate_instructions_md":
			// Remove stale CLAUDE.md/AGENTS.md from intermediate directories.
			// These are no longer created — only ~/gt/CLAUDE.md (town root) exists.
			agentPath := filepath.Join(ctx.TownRoot, issue.location)
			for _, filename := range []string{"CLAUDE.md", "AGENTS.md"} {
				filePath := filepath.Join(agentPath, filename)
				if fileExists(filePath) {
					if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
						errors = append(errors, fmt.Sprintf("%s: failed to remove %s: %v", issue.location, filename, err))
					}
				}
			}

		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("%s", strings.Join(errors, "; "))
	}
	return nil
}
