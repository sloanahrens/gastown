// Package docslint is the Go port of scripts/docs-lint.sh: the tier lists the
// doc audit slices from, and the checks that run over them. The rules the
// checks enforce are docs/writing-for-agents.md.
package docslint

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// FS is the checkout as the tiers see it. Every path is repo-relative and
// slash-separated, never prefixed "./". Exists reports a regular file, so a
// directory path is not a file the tiers list.
//
// WalkFiles reports the files under dir in no particular order. A dir that
// does not exist is an empty list rather than an error, because a docs-only
// tree is a legitimate checkout and an empty glob is an empty tier (gt-et39).
// A dir it cannot read is an error, never a shorter list: a tier that dropped
// an unreadable subtree would report a clean tree it did not check.
type FS interface {
	WalkFiles(dir string) ([]string, error)
	ReadFile(path string) ([]byte, error)
	Exists(path string) bool
	// PathExists reports whether p exists as a file or a directory. A link may
	// point at a directory, so the dead-link check asks this rather than Exists.
	PathExists(path string) bool
}

// The tier names `--list` accepts.
const (
	TierAgentFacing = "agent-facing"
	TierReference   = "reference"
	TierHistorical  = "historical"
	TierGo          = "go"
)

// The agent-facing files at fixed paths, each included only when it exists.
var agentFacingPaths = []string{
	"AGENTS.md",
	"internal/templates/polecat-CLAUDE.md",
	"docs/HOOKS.md",
}

// The agent-facing files found by pattern. The pattern's directory is carried
// with it so WalkFiles is asked for one subtree, not the whole repo.
var agentFacingGlobs = []struct{ dir, pattern string }{
	{"plugins", "plugins/*/plugin.md"},
	{"internal/formula/formulas", "internal/formula/formulas/*.formula.toml"},
}

// ceilingExempt names the formulas the word ceiling skips, each for a reason
// recorded on a bead (gt-nj23.10). Each is a multi-step workflow an agent
// receives one step at a time, so no single read is the whole file. A new entry
// needs its reason on a bead; the list only shrinks.
var ceilingExempt = map[string]string{
	"internal/formula/formulas/mol-polecat-work.formula.toml":          "the polecat work loop, delivered one step at a time",
	"internal/formula/formulas/mol-polecat-work-monorepo.formula.toml": "the same loop for monorepos",
}

// AgentFacing is the tier the writing rules are addressed to: every instruction
// an agent is handed at startup or on a step.
func AgentFacing(fsys FS) ([]string, error) {
	out := make([]string, 0, len(agentFacingPaths)+4)
	for _, p := range agentFacingPaths {
		if fsys.Exists(p) {
			out = append(out, p)
		}
	}
	for _, g := range agentFacingGlobs {
		matched, err := matching(fsys, g.dir, g.pattern)
		if err != nil {
			return nil, err
		}
		out = append(out, matched...)
	}
	return sortedUnique(out), nil
}

// CeilingTier is AgentFacing less the exempt formulas: the files the word
// ceiling is measured over.
func CeilingTier(fsys FS) ([]string, error) {
	agentFacing, err := AgentFacing(fsys)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range agentFacing {
		if _, exempt := ceilingExempt[p]; !exempt {
			out = append(out, p)
		}
	}
	return out, nil
}

// IsHistorical reports whether p records past work rather than instructing a
// reader today. docs/plans and docs/research are historical throughout; a
// docs/design file is historical only when its first line of prose declares it
// so, which keeps a live design doc in the reference tier.
func IsHistorical(fsys FS, p string) (bool, error) {
	switch {
	case strings.HasPrefix(p, "docs/plans/"), strings.HasPrefix(p, "docs/research/"):
		return true, nil
	case strings.HasPrefix(p, "docs/design/"):
		line, found, err := firstNonBlankLine(fsys, p)
		if err != nil {
			return false, err
		}
		// An indented header is the same declaration, so the leading space of
		// a nested quote does not move the file into the reference tier.
		return found && strings.HasPrefix(strings.TrimLeft(line, " \t"), "> Status:"), nil
	}
	return false, nil
}

// Historical is the historical tier: the .md files under docs/plans,
// docs/research and docs/design that IsHistorical accepts.
func Historical(fsys FS) ([]string, error) {
	var out []string
	for _, dir := range []string{"docs/plans", "docs/research", "docs/design"} {
		files, err := fsys.WalkFiles(dir)
		if err != nil {
			return nil, err
		}
		for _, p := range files {
			if !strings.HasSuffix(p, ".md") {
				continue
			}
			historical, err := IsHistorical(fsys, p)
			if err != nil {
				return nil, err
			}
			if historical {
				out = append(out, p)
			}
		}
	}
	return sortedUnique(out), nil
}

// Reference is the reference tier: README.md and every .md under docs that is
// neither agent-facing nor historical. docs/HOOKS.md is left out because it is
// agent-facing, and no file belongs to two tiers.
func Reference(fsys FS) ([]string, error) {
	var out []string
	if fsys.Exists("README.md") {
		out = append(out, "README.md")
	}
	files, err := fsys.WalkFiles("docs")
	if err != nil {
		return nil, err
	}
	for _, p := range files {
		if !strings.HasSuffix(p, ".md") || p == "docs/HOOKS.md" {
			continue
		}
		historical, err := IsHistorical(fsys, p)
		if err != nil {
			return nil, err
		}
		if !historical {
			out = append(out, p)
		}
	}
	return sortedUnique(out), nil
}

// Go is the go tier: the non-test .go files under cmd and internal. A missing
// directory is an empty tier, so a docs-only tree still lists.
func Go(fsys FS) ([]string, error) {
	var out []string
	for _, dir := range []string{"cmd", "internal"} {
		files, err := fsys.WalkFiles(dir)
		if err != nil {
			return nil, err
		}
		for _, p := range files {
			if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
				out = append(out, p)
			}
		}
	}
	return sortedUnique(out), nil
}

// List is the tier `--list` prints. An unknown name is refused rather than read
// as an empty tier, which a caller would take for a clean tree.
func List(fsys FS, tier string) ([]string, error) {
	switch tier {
	case TierAgentFacing:
		return AgentFacing(fsys)
	case TierReference:
		return Reference(fsys)
	case TierHistorical:
		return Historical(fsys)
	case TierGo:
		return Go(fsys)
	}
	return nil, fmt.Errorf("docs-lint: unknown tier '%s'", tier)
}

// firstNonBlankLine reads the first line of p that is not all whitespace,
// skipping the blank lines a file may open with.
func firstNonBlankLine(fsys FS, p string) (string, bool, error) {
	data, err := fsys.ReadFile(p)
	if err != nil {
		// A design doc that cannot be read is not a live one, so a read failure
		// must not fall back to the reference tier: the caller would report a
		// file none of the checks ever looked at.
		return "", false, fmt.Errorf("docs-lint: reading %s: %w", p, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			return line, true, nil
		}
	}
	return "", false, nil
}

// matching returns the files under dir that pattern matches. A pattern matching
// nothing contributes nothing: the tiers are allowed to be empty (gt-et39).
func matching(fsys FS, dir, pattern string) ([]string, error) {
	files, err := fsys.WalkFiles(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range files {
		// The pattern is a constant in this file, so the only error path is a
		// malformed one, which cannot arise.
		if ok, _ := path.Match(pattern, p); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// sortedUnique orders paths bytewise, which is the order the shell tiers print
// under LC_ALL=C, and drops repeats.
func sortedUnique(paths []string) []string {
	sort.Strings(paths)
	out := make([]string, 0, len(paths))
	for i, p := range paths {
		if i == 0 || p != paths[i-1] {
			out = append(out, p)
		}
	}
	return out
}

// OSFiles is FS over a checkout on disk; Root is the directory the repo-relative
// paths are resolved against.
type OSFiles struct {
	Root string
	// walk is filepath.WalkDir. A unit test replaces it to reach the branches a
	// real tree takes only through a directory it may not read, which the unit
	// tier cannot build (docs/testing.md).
	walk func(root string, fn fs.WalkDirFunc) error
}

// Exists reports whether p is a regular file under Root.
func (o OSFiles) Exists(p string) bool {
	info, err := os.Stat(o.abspath(p))
	return err == nil && info.Mode().IsRegular()
}

// PathExists reports whether p exists under Root, as a file or a directory.
func (o OSFiles) PathExists(p string) bool {
	_, err := os.Stat(o.abspath(p))
	return err == nil
}

// ReadFile reads p under Root.
func (o OSFiles) ReadFile(p string) ([]byte, error) {
	return os.ReadFile(o.abspath(p))
}

// WalkFiles reports the files under dir, or an empty list when dir does not
// exist, which is a docs-only tree's answer for the go tier (gt-et39). Any
// other walk failure is returned: the script's `find ... 2>/dev/null` dropped
// an unreadable subtree silently, and a gate cannot.
func (o OSFiles) WalkFiles(dir string) ([]string, error) {
	root := o.abspath(dir)
	walk := o.walk
	if walk == nil {
		walk = filepath.WalkDir
	}
	var out []string
	err := walk(root, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			// Only an absent root is an empty tier; a subtree that cannot be
			// read stops the walk, so a caller never reads a partial list as a
			// complete one (gt-et39).
			if p == root && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, under := strings.CutPrefix(p, root+string(filepath.Separator))
		if !under {
			// The walk root is itself a file, so it is the whole list.
			rel = ""
		}
		out = append(out, path.Join(dir, filepath.ToSlash(rel)))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("docs-lint: walking %s: %w", dir, err)
	}
	return out, nil
}

func (o OSFiles) abspath(p string) string {
	return filepath.Join(o.Root, filepath.FromSlash(p))
}
