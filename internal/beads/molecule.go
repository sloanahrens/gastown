// Package beads molecule support - composable workflow templates.
package beads

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MoleculeStep represents a parsed step from a molecule definition.
type MoleculeStep struct {
	Ref          string         // Step reference (from "## Step: <ref>")
	Title        string         // Step title (first non-empty line or ref)
	Instructions string         // Prose instructions for this step
	Needs        []string       // Step refs this step depends on
	WaitsFor     []string       // Dynamic wait conditions (e.g., "all-children")
	Tier         string         // Optional tier hint: haiku, sonnet, opus
	Type         string         // Step type: "task" (default), "wait", etc.
	Backoff      *BackoffConfig // Backoff configuration for wait-type steps
}

// BackoffConfig defines exponential backoff parameters for wait-type steps.
// Used by patrol agents to implement cost-saving await-signal patterns.
type BackoffConfig struct {
	Base       string // Base interval (e.g., "30s")
	Multiplier int    // Multiplier for exponential growth (default: 2)
	Max        string // Maximum interval cap (e.g., "10m")
}

// stepHeaderRegex matches "## Step: <ref>" with optional whitespace.
var stepHeaderRegex = regexp.MustCompile(`(?i)^##\s*Step:\s*(\S+)\s*$`)

// needsLineRegex matches "Needs: step1, step2, ..." lines.
var needsLineRegex = regexp.MustCompile(`(?i)^Needs:\s*(.+)$`)

// tierLineRegex matches "Tier: haiku|sonnet|opus" lines.
var tierLineRegex = regexp.MustCompile(`(?i)^Tier:\s*(haiku|sonnet|opus)\s*$`)

// waitsForLineRegex matches "WaitsFor: condition1, condition2, ..." lines.
// Common conditions: "all-children" (fanout gate for dynamically bonded children)
var waitsForLineRegex = regexp.MustCompile(`(?i)^WaitsFor:\s*(.+)$`)

// typeLineRegex matches "Type: task|wait|..." lines.
// Common types: "task" (default), "wait" (await-signal with backoff)
var typeLineRegex = regexp.MustCompile(`(?i)^Type:\s*(\w+)\s*$`)

// backoffLineRegex matches "Backoff: base=30s, multiplier=2, max=10m" lines.
// Parses backoff configuration for wait-type steps.
var backoffLineRegex = regexp.MustCompile(`(?i)^Backoff:\s*(.+)$`)

// ParseMoleculeSteps extracts step definitions from a molecule's description.
//
// The expected format is:
//
//	## Step: <ref>
//	<prose instructions>
//	Needs: <step>, <step>  # optional
//	Tier: haiku|sonnet|opus  # optional
//	Type: task|wait  # optional, default is "task"
//	Backoff: base=30s, multiplier=2, max=10m  # optional, for wait-type steps
//
// Returns an empty slice if no steps are found.
func ParseMoleculeSteps(description string) ([]MoleculeStep, error) {
	if description == "" {
		return nil, nil
	}

	lines := strings.Split(description, "\n")
	var steps []MoleculeStep
	var currentStep *MoleculeStep
	var contentLines []string

	// Helper to finalize current step
	finalizeStep := func() {
		if currentStep == nil {
			return
		}

		// Process content lines to extract Needs/Tier and build instructions
		var instructionLines []string
		for _, line := range contentLines {
			trimmed := strings.TrimSpace(line)

			// Check for Needs: line
			if matches := needsLineRegex.FindStringSubmatch(trimmed); matches != nil {
				deps := strings.Split(matches[1], ",")
				for _, dep := range deps {
					dep = strings.TrimSpace(dep)
					if dep != "" {
						currentStep.Needs = append(currentStep.Needs, dep)
					}
				}
				continue
			}

			// Check for Tier: line
			if matches := tierLineRegex.FindStringSubmatch(trimmed); matches != nil {
				currentStep.Tier = strings.ToLower(matches[1])
				continue
			}

			// Check for WaitsFor: line
			if matches := waitsForLineRegex.FindStringSubmatch(trimmed); matches != nil {
				conditions := strings.Split(matches[1], ",")
				for _, cond := range conditions {
					cond = strings.TrimSpace(cond)
					if cond != "" {
						currentStep.WaitsFor = append(currentStep.WaitsFor, cond)
					}
				}
				continue
			}

			// Check for Type: line
			if matches := typeLineRegex.FindStringSubmatch(trimmed); matches != nil {
				currentStep.Type = strings.ToLower(matches[1])
				continue
			}

			// Check for Backoff: line
			if matches := backoffLineRegex.FindStringSubmatch(trimmed); matches != nil {
				currentStep.Backoff = parseBackoffConfig(matches[1])
				continue
			}

			// Regular instruction line
			instructionLines = append(instructionLines, line)
		}

		// Build instructions, trimming leading/trailing blank lines
		currentStep.Instructions = strings.TrimSpace(strings.Join(instructionLines, "\n"))

		// Set title from first non-empty line of instructions, or use ref
		if currentStep.Instructions != "" {
			firstLine := strings.SplitN(currentStep.Instructions, "\n", 2)[0]
			currentStep.Title = strings.TrimSpace(firstLine)
		}
		if currentStep.Title == "" {
			currentStep.Title = currentStep.Ref
		}

		steps = append(steps, *currentStep)
		currentStep = nil
		contentLines = nil
	}

	for _, line := range lines {
		// Check for step header
		if matches := stepHeaderRegex.FindStringSubmatch(line); matches != nil {
			// Finalize previous step if any
			finalizeStep()

			// Start new step
			currentStep = &MoleculeStep{
				Ref: matches[1],
			}
			contentLines = nil
			continue
		}

		// Accumulate content lines if we're in a step
		if currentStep != nil {
			contentLines = append(contentLines, line)
		}
	}

	// Finalize last step
	finalizeStep()

	return steps, nil
}

// parseBackoffConfig parses a backoff configuration string.
// Expected format: "base=30s, multiplier=2, max=10m"
// Returns nil if parsing fails.
func parseBackoffConfig(configStr string) *BackoffConfig {
	cfg := &BackoffConfig{
		Multiplier: 2, // Default multiplier
	}

	// Split by comma and parse key=value pairs
	parts := strings.Split(configStr, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		// Split by = to get key and value
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}

		key := strings.TrimSpace(strings.ToLower(kv[0]))
		value := strings.TrimSpace(kv[1])

		switch key {
		case "base":
			cfg.Base = value
		case "multiplier":
			if m, err := strconv.Atoi(value); err == nil {
				cfg.Multiplier = m
			}
		case "max":
			cfg.Max = value
		}
	}

	// Return nil if no base was specified (incomplete config)
	if cfg.Base == "" {
		return nil
	}

	return cfg
}

// ValidateMolecule checks if an issue is a valid molecule definition.
// Returns an error describing the problem, or nil if valid.
//
// Note: This function only validates the old format (embedded markdown steps).
// It reports "no steps defined" for new format molecules (with child issues)
// since it cannot access child issues without a Beads client.
func ValidateMolecule(mol *Issue) error {
	if mol == nil {
		return fmt.Errorf("molecule is nil")
	}

	if mol.Type != "molecule" {
		return fmt.Errorf("issue type is %q, expected molecule", mol.Type)
	}

	steps, err := ParseMoleculeSteps(mol.Description)
	if err != nil {
		return fmt.Errorf("parsing steps: %w", err)
	}

	if len(steps) == 0 {
		return fmt.Errorf("molecule has no steps defined")
	}

	// Build step map for reference validation
	stepMap := make(map[string]bool)
	for _, step := range steps {
		if step.Ref == "" {
			return fmt.Errorf("step has empty ref")
		}
		if stepMap[step.Ref] {
			return fmt.Errorf("duplicate step ref: %s", step.Ref)
		}
		stepMap[step.Ref] = true
	}

	// Validate Needs references
	for _, step := range steps {
		for _, need := range step.Needs {
			if !stepMap[need] {
				return fmt.Errorf("step %q depends on unknown step %q", step.Ref, need)
			}
			if need == step.Ref {
				return fmt.Errorf("step %q has self-dependency", step.Ref)
			}
		}
	}

	// Detect cycles in dependency graph
	if err := detectCycles(steps); err != nil {
		return err
	}

	return nil
}

// detectCycles checks for circular dependencies in the step graph using DFS.
// Returns an error describing the cycle if one is found.
func detectCycles(steps []MoleculeStep) error {
	// Build adjacency list: step -> steps it depends on
	deps := make(map[string][]string)
	for _, step := range steps {
		deps[step.Ref] = step.Needs
	}

	// Track visit state: 0 = unvisited, 1 = visiting (in stack), 2 = visited
	state := make(map[string]int)

	// DFS from each node to find cycles
	var path []string
	var dfs func(node string) error

	dfs = func(node string) error {
		if state[node] == 2 {
			return nil // Already fully processed
		}
		if state[node] == 1 {
			// Found a back edge - there's a cycle
			// Build cycle path for error message
			cycleStart := -1
			for i, n := range path {
				if n == node {
					cycleStart = i
					break
				}
			}
			cycle := append(path[cycleStart:], node)
			return fmt.Errorf("cycle detected in step dependencies: %s", formatCycle(cycle))
		}

		state[node] = 1 // Mark as visiting
		path = append(path, node)

		for _, dep := range deps[node] {
			if err := dfs(dep); err != nil {
				return err
			}
		}

		path = path[:len(path)-1] // Pop from path
		state[node] = 2           // Mark as visited
		return nil
	}

	for _, step := range steps {
		if state[step.Ref] == 0 {
			if err := dfs(step.Ref); err != nil {
				return err
			}
		}
	}

	return nil
}

// formatCycle formats a cycle path as "a -> b -> c -> a".
func formatCycle(cycle []string) string {
	return strings.Join(cycle, " -> ")
}
