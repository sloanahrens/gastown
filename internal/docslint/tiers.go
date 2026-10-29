// Package docslint is the Go port of scripts/docs-lint.sh: the tier lists the
// doc audit slices from, and the checks that run over them. The rules the
// checks enforce are docs/writing-for-agents.md.
package docslint

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// FS is the checkout as the tiers see it. Every path is repo-relative and
// slash-separated, never prefixed "./".
//
// WalkFiles reports the files under dir in no particular order; a dir that does
// not exist is an empty list rather than an error, because a docs-only tree is
// a legitimate checkout and an empty glob is an empty tier (gt-et39).
type FS interface {
	WalkFiles(dir string) []string
	ReadFile(path string) ([]byte, error)
	Exists(path string) bool
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
func AgentFacing(fsys FS) []string {
	out := make([]string, 0, len(agentFacingPaths)+4)
	for _, p := range agentFacingPaths {
		if fsys.Exists(p) {
			out = append(out, p)
		}
	}
	for _, g := range agentFacingGlobs {
		out = append(out, matching(fsys, g.dir, g.pattern)...)
	}
	return sortedUnique(out)
}

// CeilingTier is AgentFacing less the exempt formulas: the files the word
// ceiling is measured over.
func CeilingTier(fsys FS) []string {
	var out []string
	for _, p := range AgentFacing(fsys) {
		if _, exempt := ceilingExempt[p]; !exempt {
			out = append(out, p)
		}
	}
	return out
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
		return found && strings.HasPrefix(line, "> Status:"), nil
	}
	return false, nil
}

// Historical is the historical tier: the .md files under docs/plans,
// docs/research and docs/design that IsHistorical accepts.
func Historical(fsys FS) ([]string, error) {
	var out []string
	for _, dir := range []string{"docs/plans", "docs/research", "docs/design"} {
		for _, p := range fsys.WalkFiles(dir) {
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
	for _, p := range fsys.WalkFiles("docs") {
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
func Go(fsys FS) []string {
	var out []string
	for _, dir := range []string{"cmd", "internal"} {
		for _, p := range fsys.WalkFiles(dir) {
			if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
				out = append(out, p)
			}
		}
	}
	return sortedUnique(out)
}

// List is the tier `--list` prints. An unknown name is refused rather than read
// as an empty tier, which a caller would take for a clean tree.
func List(fsys FS, tier string) ([]string, error) {
	switch tier {
	case TierAgentFacing:
		return AgentFacing(fsys), nil
	case TierReference:
		return Reference(fsys)
	case TierHistorical:
		return Historical(fsys)
	case TierGo:
		return Go(fsys), nil
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
func matching(fsys FS, dir, pattern string) []string {
	var out []string
	for _, p := range fsys.WalkFiles(dir) {
		// The pattern is a constant in this file, so the only error path is a
		// malformed one, which cannot arise.
		if ok, _ := path.Match(pattern, p); ok {
			out = append(out, p)
		}
	}
	return out
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
}

// Exists reports whether p is present under Root.
func (o OSFiles) Exists(p string) bool {
	_, err := os.Stat(o.abspath(p))
	return err == nil
}

// ReadFile reads p under Root.
func (o OSFiles) ReadFile(p string) ([]byte, error) {
	return os.ReadFile(o.abspath(p))
}

// WalkFiles reports the files under dir. A dir that does not exist is empty,
// which is the docs-only tree's answer for the go tier (gt-et39). A subtree the
// process cannot read is skipped rather than stopping the walk, matching the
// script's `find ... 2>/dev/null`.
func (o OSFiles) WalkFiles(dir string) []string {
	var out []string
	_ = filepath.WalkDir(o.abspath(dir), func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(o.Root, p)
		if err != nil {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	return out
}

func (o OSFiles) abspath(p string) string {
	return filepath.Join(o.Root, filepath.FromSlash(p))
}
