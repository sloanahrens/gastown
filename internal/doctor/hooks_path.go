package doctor

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/git"
)

// hooksPathGap is a clone whose core.hooksPath names no directory: unset, or a
// value that dangles. The all-rigs and per-rig hooks checks both report gaps
// through it, so they judge a clone the same way.
type hooksPathGap struct {
	clone      string   // the clone the value is read in
	value      string   // the configured value; empty when unset
	configFile string   // the config file the value is read from and written to
	sharers    []string // linked worktrees that read the same config
}

// readHooksPath judges clone's core.hooksPath. It returns nil when the value
// names a directory, wherever that is, and the gap otherwise. An error means the
// value could not be read, so the clone is not judged at all: a failed read is
// not taken for a clean or unset value.
func readHooksPath(ctx *CheckContext, clone string) (*hooksPathGap, error) {
	repo := ctx.git(clone)
	value, err := repo.ConfigGet("core.hooksPath")
	if err != nil {
		return nil, fmt.Errorf("reading core.hooksPath: %w", err)
	}
	named, err := git.HooksPathTargetExists(clone, value)
	if err != nil {
		return nil, err
	}
	if named {
		return nil, nil
	}

	commonDir, err := repo.CommonDir()
	if err != nil {
		return nil, fmt.Errorf("locating config: %w", err)
	}
	worktrees, err := repo.WorktreeList()
	if err != nil {
		return nil, fmt.Errorf("listing worktrees: %w", err)
	}
	gap := &hooksPathGap{
		clone:      clone,
		value:      strings.TrimSpace(value),
		configFile: filepath.Join(commonDir, "config"),
	}
	// git lists the main worktree first; every entry after it shares its config.
	for i := 1; i < len(worktrees); i++ {
		gap.sharers = append(gap.sharers, worktrees[i].Path)
	}
	return gap, nil
}

// hooksPathDetail names a gap for a check's details. An unset clone is named by
// its path alone. A dangling value also names the value, the config it comes
// from, and the linked worktrees that share that config.
func hooksPathDetail(rel string, gap hooksPathGap) string {
	if gap.value == "" {
		return rel
	}
	detail := fmt.Sprintf("%s (core.hooksPath=%s names no directory; config %s", rel, gap.value, gap.configFile)
	if len(gap.sharers) > 0 {
		detail += "; shared with " + strings.Join(gap.sharers, ", ")
	}
	return detail + ")"
}

// fixHooksPathGaps writes .githooks into each gap's clone. A linked worktree
// shares its clone's config, so the one write covers every worktree that reads
// it; the checks never list a linked worktree as a clone of its own.
func fixHooksPathGaps(ctx *CheckContext, gaps []hooksPathGap) error {
	for _, gap := range gaps {
		if err := ctx.git(gap.clone).ConfigSet("core.hooksPath", ".githooks"); err != nil {
			return fmt.Errorf("failed to configure hooks for %s: %w", gap.clone, err)
		}
	}
	return nil
}
