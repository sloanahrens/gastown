package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// SharedDirectiveName is the reserved directive filename that renders for every
// role. Policy that is not role-specific — host rules, testing norms — belongs
// here rather than duplicated into each role's file (R2). The leading
// underscore keeps it clear of every role name in AllRoles.
const SharedDirectiveName = "_common"

// IsKnownRole reports whether name is a role that directives can target.
func IsKnownRole(name string) bool {
	for _, r := range AllRoles() {
		if r == name {
			return true
		}
	}
	return false
}

// LoadRoleDirective loads role directive content from the directive file layout.
// Resolution order:
//  1. Town-level shared: <townRoot>/directives/_common.md
//  2. Rig-level shared:  <townRoot>/<rigName>/directives/_common.md
//  3. Town-level role:   <townRoot>/directives/<role>.md
//  4. Rig-level role:    <townRoot>/<rigName>/directives/<role>.md
//
// Every existing file is concatenated in that order, newline-separated, so
// shared policy is read first and the most specific file has the last word.
// Returns empty string if no directive files exist.
//
// Invalid or unreadable paths are treated as absent (no error).
func LoadRoleDirective(role, townRoot, rigName string) string {
	sources := RoleDirectiveSources(role, townRoot, rigName)
	parts := make([]string, 0, len(sources))
	for _, path := range sources {
		parts = append(parts, readDirectiveContent(path))
	}
	return strings.Join(parts, "\n")
}

// RoleDirectiveSources returns the directive files that render for role and
// hold content, in resolution order. It is what lets a caller name the files
// it is actually showing rather than guessing from the layout.
func RoleDirectiveSources(role, townRoot, rigName string) []string {
	var sources []string
	for _, path := range directivePaths(role, townRoot, rigName) {
		if readDirectiveContent(path) != "" {
			sources = append(sources, path)
		}
	}
	return sources
}

func readDirectiveContent(path string) string {
	content, err := os.ReadFile(path) //nolint:gosec // G304: path is from trusted config
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(content))
}

// directivePaths returns the directive files that render for role, broadest
// first. Asking for the shared file yields it once rather than twice.
func directivePaths(role, townRoot, rigName string) []string {
	paths := []string{filepath.Join(townRoot, "directives", SharedDirectiveName+".md")}
	if rigName != "" {
		paths = append(paths, filepath.Join(townRoot, rigName, "directives", SharedDirectiveName+".md"))
	}
	if role == SharedDirectiveName {
		return paths
	}
	paths = append(paths, filepath.Join(townRoot, "directives", role+".md"))
	if rigName != "" {
		paths = append(paths, filepath.Join(townRoot, rigName, "directives", role+".md"))
	}
	return paths
}

// DirectiveFile is one <name>.md file under a directives/ directory.
type DirectiveFile struct {
	Scope string // "town", or the name of the rig it belongs to
	Role  string // file stem, e.g. "polecat" or "_common"
	Path  string // absolute path to the file
}

// Unused reports whether the file is never rendered: a name that is neither a
// known role nor the shared name reaches no agent at all.
func (f DirectiveFile) Unused() bool {
	return f.Role != SharedDirectiveName && !IsKnownRole(f.Role)
}

// ScanDirectiveFiles lists the *.md files in the town directives directory and
// in the named rig's. An empty rigName scans every rig directory that holds a
// config.json, which is what the town-wide callers want.
//
// A missing directives directory yields no files and no error; any other read
// failure is returned, so a caller that cannot inspect the layout can say so
// instead of reporting a clean scan.
func ScanDirectiveFiles(townRoot, rigName string) ([]DirectiveFile, error) {
	var files []DirectiveFile

	townFiles, err := readDirectiveDir(filepath.Join(townRoot, "directives"), "town")
	if err != nil {
		return nil, err
	}
	files = append(files, townFiles...)

	rigs, err := rigNamesToScan(townRoot, rigName)
	if err != nil {
		return nil, err
	}
	for _, rig := range rigs {
		rigFiles, err := readDirectiveDir(filepath.Join(townRoot, rig, "directives"), rig)
		if err != nil {
			return nil, err
		}
		files = append(files, rigFiles...)
	}

	return files, nil
}

// rigNamesToScan returns the single named rig, or every rig directory under
// townRoot that carries a config.json.
func rigNamesToScan(townRoot, rigName string) ([]string, error) {
	if rigName != "" {
		return []string{rigName}, nil
	}

	entries, err := os.ReadDir(townRoot)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", townRoot, err)
	}
	var rigs []string
	for _, d := range entries {
		if !d.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(townRoot, d.Name(), "config.json")); err != nil {
			continue
		}
		rigs = append(rigs, d.Name())
	}
	return rigs, nil
}

// readDirectiveDir returns the .md files in dir tagged with scope. A directory
// that does not exist is an empty result, not a failure.
func readDirectiveDir(dir, scope string) ([]DirectiveFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}

	var files []DirectiveFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		files = append(files, DirectiveFile{
			Scope: scope,
			Role:  strings.TrimSuffix(e.Name(), ".md"),
			Path:  filepath.Join(dir, e.Name()),
		})
	}
	return files, nil
}
