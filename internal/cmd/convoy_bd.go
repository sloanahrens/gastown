package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/convoy"
	"github.com/steveyegge/gastown/internal/workspace"
)

// bd JSON helpers the convoy commands share. They outlived gt mountain's
// staging code, which was deleted with it (gt-638go.2, gt-31vjc).

// bdShowResult matches the JSON output of `bd show <id> --json`.
type bdShowResult struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Status      string   `json:"status"`
	IssueType   string   `json:"issue_type"`
	Labels      []string `json:"labels"`
	Description string   `json:"description"`
}

func runBdJSONForBead(beadID string, args ...string) ([]byte, error) {
	return beads.RunBdJSON(resolveBeadDir(beadID), args...)
}

// bdShow runs `bd show <id> --json` and returns the parsed bead info.
// Returns error if bd exits non-zero or returns no results.
func bdShow(beadID string) (*bdShowResult, error) {
	out, err := runBdJSONForBead(beadID, "show", beadID, "--json")
	if err != nil {
		return nil, fmt.Errorf("bd show %s: %w", beadID, err)
	}

	var results []bdShowResult
	if err := json.Unmarshal(out, &results); err != nil {
		return nil, fmt.Errorf("bd show %s: parse JSON: %w (raw: %s)", beadID, err, out)
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("bd show %s: no results", beadID)
	}

	return &results[0], nil
}

// bdListChildren runs `bd list --parent=<id> --json` and returns child beads.
// bd list is CWD-sensitive — it only searches the beads database in the current
// directory. We resolve the correct .beads directory from the bead's prefix via
// routes.jsonl so this works regardless of the caller's working directory.
//
// When the `--parent` index returns no rows, we fall back to a direct query
// against the dependencies table (parent-child links) and resolve each child
// via bdShow. This handles the case (GH #3700) where the index used by
// `bd list --parent` doesn't see children that were added via `bd dep add ...
// --type=parent-child`. The deps table is authoritative.
func bdListChildren(parentID string) ([]bdShowResult, error) {
	out, err := runBdJSONForBead(parentID, "list", "--parent="+parentID, "--json")
	if err != nil {
		return nil, fmt.Errorf("bd list --parent=%s: %w", parentID, err)
	}

	// Handle empty output (no children) — try the deps-table fallback first.
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "[]" {
		return bdListChildrenViaDeps(parentID)
	}

	var results []bdShowResult
	if err := json.Unmarshal(out, &results); err != nil {
		return nil, fmt.Errorf("bd list --parent=%s: parse JSON: %w (raw: %s)", parentID, err, out)
	}

	return results, nil
}

// bdListChildrenViaDeps resolves children by querying the dependencies table
// directly for parent-child links, then loading each child via bdShow.
//
// Used as a fallback when `bd list --parent=<id>` returns empty even though
// children exist (GH #3700). Returns nil (not an error) when the prefix can't
// be resolved or no parent-child deps exist.
func bdListChildrenViaDeps(parentID string) ([]bdShowResult, error) {
	beadsDir := beadsDirForID(parentID)
	if beadsDir == "" {
		// Can't resolve the rig; nothing more we can do.
		return nil, nil
	}

	// Production data stores parent-child as a typed dependency target where
	// issue_id=parent. "down" returns target rows for the epic's children.
	childIDs, err := convoy.DepListRawIDs(beadsDir, parentID, "down", "parent-child")
	if err != nil {
		return nil, nil // best-effort — caller still gets the empty primary result
	}
	if len(childIDs) == 0 {
		return nil, nil
	}

	results := make([]bdShowResult, 0, len(childIDs))
	for _, id := range childIDs {
		child, err := bdShow(id)
		if err != nil || child == nil {
			continue
		}
		results = append(results, *child)
	}
	return results, nil
}

// beadsDirForID resolves the .beads directory that owns a given bead ID by
// looking up its prefix in routes.jsonl. This is needed for CWD-sensitive bd
// commands like `bd list --parent=` which only search the local database.
// Returns empty string if the prefix or workspace cannot be resolved.
func beadsDirForID(beadID string) string {
	prefix := beads.ExtractPrefix(beadID)
	if prefix == "" {
		return ""
	}
	townRoot, err := workspace.FindFromCwd()
	if err != nil {
		return ""
	}
	rigPath := beads.GetRigPathForPrefix(townRoot, prefix)
	if rigPath == "" {
		return ""
	}
	return beads.ResolveBeadsDir(rigPath)
}
