package beads

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/steveyegge/gastown/internal/util"
)

// Wisp cleanup helpers shared by the daemon dispatch loop (internal/daemon)
// and `gt dog done` (internal/cmd). Both need to answer the same two
// questions — "which hooked beads are formula wisps?" and "close this wisp and
// everything under it" — and both must shell out under the same bd
// subprocess environment policy, so the logic lives here instead of being
// forked per package and drifting (gt-da2x).

// FormulaWispIDs returns the IDs of beads carrying attached_formula metadata
// — the formula molecule wisps `gt sling` attaches to an agent's hook bead —
// in the order given. Hooked ephemeral beads without that metadata are
// excluded: they are unrelated hooked work, and force-closing them would
// destroy it.
func FormulaWispIDs(issues []*Issue) []string {
	var ids []string
	for _, issue := range issues {
		if isFormulaWisp(issue) {
			ids = append(ids, issue.ID)
		}
	}
	return ids
}

// FirstFormulaWisp returns the first bead carrying attached_formula metadata,
// or nil if none does. See FormulaWispIDs.
func FirstFormulaWisp(issues []*Issue) *Issue {
	for _, issue := range issues {
		if isFormulaWisp(issue) {
			return issue
		}
	}
	return nil
}

// isFormulaWisp is the single attached-formula predicate behind
// FormulaWispIDs and FirstFormulaWisp, so the daemon and cmd paths cannot
// drift on what counts as a wisp.
func isFormulaWisp(issue *Issue) bool {
	fields := ParseAttachmentFields(issue)
	return fields != nil && fields.AttachedFormula != ""
}

// WispStep is one bead in a wisp tree: the wisp root or one of its
// descendants.
type WispStep struct {
	ID     string
	Status string
}

// WispTree returns rootID followed by every descendant in breadth-first order
// (the root at index 0, deepest level last), each with the status the read
// reported. The root's status is left empty: callers only build a tree for a
// wisp they have just seen open, so it needs no re-read to be known open.
//
// Children are read with `bd show <id> --children --json`, not the obvious
// `bd children` (an alias for `bd list --parent`): the latter walks only the
// persistent dependencies table and silently misses ephemeral wisp children,
// whose parent-child edges live in the separate wisp_dependencies table
// (gt-43t7). Finding no children that way is indistinguishable from a
// finished wisp, which is what makes it a dangerous answer here.
//
// A failed child read aborts rather than truncating the tree: callers close
// what this returns, so a partial answer would strand wisps.
func WispTree(ctx context.Context, dir string, env []string, rootID string) ([]WispStep, error) {
	seen := map[string]bool{rootID: true}
	tree := []WispStep{{ID: rootID}}
	for i := 0; i < len(tree); i++ {
		children, err := wispChildren(ctx, dir, env, tree[i].ID)
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			if child == nil || seen[child.ID] {
				continue
			}
			seen[child.ID] = true
			tree = append(tree, WispStep{ID: child.ID, Status: child.Status})
		}
	}
	return tree, nil
}

// wispChildren reads one level of a wisp tree.
func wispChildren(ctx context.Context, dir string, env []string, parentID string) ([]*Issue, error) {
	cmd := exec.CommandContext(ctx, "bd", "show", parentID, "--children", "--json") //nolint:gosec // G204: args are constructed internally
	cmd.Dir = dir
	cmd.Env = env
	util.SetDetachedProcessGroup(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("bd show %s --children: %w: %s", parentID, err, msg)
		}
		return nil, fmt.Errorf("bd show %s --children: %w", parentID, err)
	}
	return parseChildrenJSON(stdout.Bytes())
}

// CloseWispTree force-closes every step of tree that is not already closed,
// deepest level first, using the given mutation environment. Returns the
// number of beads force-closed.
//
// Children are closed before their parents so no bead is ever closed while a
// child of it survives (gt-7lx3). WispTree is breadth-first, so reversing its
// order is enough to get that: every child precedes its parent in the queue.
func CloseWispTree(ctx context.Context, dir string, env []string, reason string, tree []WispStep) (int, error) {
	var ids []string
	for _, step := range tree {
		if step.Status != string(StatusClosed) {
			ids = append(ids, step.ID)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
		ids[i], ids[j] = ids[j], ids[i]
	}

	args := append([]string{"close"}, ids...)
	args = append(args, "--force", "--reason", reason)
	cmd := exec.CommandContext(ctx, "bd", args...) //nolint:gosec // G204: args are constructed internally
	cmd.Dir = dir
	cmd.Env = env
	util.SetDetachedProcessGroup(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return 0, fmt.Errorf("bd close %s: %w: %s", strings.Join(ids, " "), err, msg)
		}
		return 0, fmt.Errorf("bd close %s: %w", strings.Join(ids, " "), err)
	}
	return len(ids), nil
}
