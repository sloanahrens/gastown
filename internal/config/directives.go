package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
// The role's files are read only for a known role (IsKnownRole), so a name no
// role has renders nothing (gt-r3jbi). Returns empty string if no directive
// files exist.
//
// Invalid or unreadable paths are treated as absent (no error).
func LoadRoleDirective(role, townRoot, rigName string) string {
	sources := RoleDirectiveSources(role, townRoot, rigName)
	parts := make([]string, 0, len(sources))
	for _, path := range sources {
		text, _ := readDirectiveFile(path)
		parts = append(parts, strings.TrimSpace(stripChangelog(text)))
	}
	return strings.Join(parts, "\n")
}

// RoleDirectiveSources returns the directive files that render for role and
// hold content, in resolution order. It is what lets a caller name the files
// it is actually showing rather than guessing from the layout.
func RoleDirectiveSources(role, townRoot, rigName string) []string {
	var sources []string
	for _, path := range directivePaths(role, townRoot, rigName) {
		if text, _ := readDirectiveFile(path); strings.TrimSpace(text) != "" {
			sources = append(sources, path)
		}
	}
	return sources
}

// RoleDirective is one directive file that renders for a role: its scope (the
// word "town", or the rig's name), its file stem ("_common" or the role), the
// file's path, and its full text, unchanged.
type RoleDirective struct {
	Scope string
	Name  string
	Path  string
	Text  string
}

// ReadRoleDirectives returns every directive file that renders for role, in
// resolution order, with its full text. A file that does not exist is absent,
// and so is one that holds only whitespace, as RoleDirectiveSources treats it.
// A file that exists and cannot be read is an error: a caller that renders a
// prompt from these must not silently drop a policy (fail closed).
func ReadRoleDirectives(role, townRoot, rigName string) ([]RoleDirective, error) {
	var out []RoleDirective
	for _, path := range directivePaths(role, townRoot, rigName) {
		text, err := readDirectiveFile(path)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		out = append(out, RoleDirective{
			Scope: directiveScope(path, townRoot),
			Name:  strings.TrimSuffix(filepath.Base(path), ".md"),
			Path:  path,
			Text:  stripChangelog(text),
		})
	}
	return out, nil
}

// directiveScope names the scope of a directive file: "town" for one under
// <townRoot>/directives, otherwise the rig directory it sits in.
func directiveScope(path, townRoot string) string {
	if strings.HasPrefix(path, CentralDirectiveDir(townRoot)+string(filepath.Separator)) {
		return "town"
	}
	rel, err := filepath.Rel(townRoot, path)
	if err != nil {
		return ""
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) >= 3 {
		return parts[0]
	}
	return "town"
}

// stripChangelog removes a leading "## Changelog" block: the editors' record at
// the top of a content file, which is not directive text and so stays out of
// every polecat's prompt. The block runs from that heading to the next "## "
// heading, or to the end of the file. A file that does not open with the block
// is returned unchanged.
func stripChangelog(text string) string {
	trimmed := strings.TrimLeft(text, " \t\r\n")
	if !strings.HasPrefix(trimmed, "## Changelog") {
		return text
	}
	lines := strings.SplitAfter(trimmed, "\n")
	for i := 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") || strings.HasPrefix(lines[i], "##\t") || lines[i] == "##\n" {
			return strings.Join(lines[i:], "")
		}
	}
	return ""
}

// readDirectiveFile is the one reader of a directive file: the system-prompt
// file, the hook and directive show all read through it, so they cannot drift.
// It returns the file's text as written. A file that does not exist is "" with
// no error. Any other read failure is returned, so a caller that renders a
// prompt fails closed; a caller that only displays (the hook, directive show)
// ignores the error and treats the file as absent, as it always did.
func readDirectiveFile(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is from trusted config
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("directive %s: %w", path, err)
	}
	return string(data), nil
}

// directivePaths returns the directive files that render for role, broadest
// first: for each name (_common, then the role), the central layer and then the
// rig layer. The role's file is read only for a known role, so a name no role
// has reaches no agent, which is what gt directive list and gt doctor report
// (gt-r3jbi). A layer's file is the new path when that file exists, and otherwise
// the legacy path when that exists, which is warned about once per process
// (gt-oz73l). Asking for the shared file yields it once rather than twice.
func directivePaths(role, townRoot, rigName string) []string {
	names := []string{SharedDirectiveName}
	if role != SharedDirectiveName && IsKnownRole(role) {
		names = append(names, role)
	}
	var paths []string
	for _, name := range names {
		paths = append(paths, layerDirectivePath(CentralDirectiveDir(townRoot), filepath.Join(townRoot, "directives"), name))
		if rigName != "" {
			paths = append(paths, layerDirectivePath(RigDirectiveDir(townRoot, rigName), filepath.Join(townRoot, rigName, "directives"), name))
		}
	}
	return paths
}

// DirectivePaths returns the directive files that render for role, broadest
// first, as the reader resolves each layer: the new path when that file exists,
// the legacy path otherwise. Callers that name the files a prompt draws from use
// it, so none of them builds a path of its own (gt-80rnu).
func DirectivePaths(role, townRoot, rigName string) []string {
	return directivePaths(role, townRoot, rigName)
}

// DirectiveEditPath is the file gt directive edit writes for a layer: the new
// path of the central layer (townLevel, or no rig) or of the rig's own layer.
// An edit of the central layer is a change to the gastown repo's checkout, which
// lands through the pipeline (gt-80rnu).
func DirectiveEditPath(role, townRoot, rigName string, townLevel bool) string {
	if townLevel || rigName == "" {
		return filepath.Join(CentralDirectiveDir(townRoot), role+".md")
	}
	return filepath.Join(RigDirectiveDir(townRoot, rigName), role+".md")
}

// IsTownDirective reports whether path is a file of the town-level layer, the
// central one or the legacy town directory (gt-80rnu).
func IsTownDirective(path, townRoot string) bool {
	sep := string(filepath.Separator)
	return strings.HasPrefix(path, CentralDirectiveDir(townRoot)+sep) ||
		strings.HasPrefix(path, filepath.Join(townRoot, "directives")+sep)
}

// DirectiveTreeRels lists, relative to the town root, the directive trees a
// sandbox copies for rig: the central and rig layers, and the legacy directories
// the reader still falls back to until they are removed (gt-80rnu).
func DirectiveTreeRels(rig string) []string {
	rels := []string{
		filepath.ToSlash(filepath.Join("gastown", "mayor", "rig", "directives")),
		filepath.ToSlash(filepath.Join(rig, "mayor", "rig", ".gastown", "directives")),
		"directives",
	}
	if rig != "" {
		rels = append(rels, rig+"/directives")
	}
	return rels
}

// CentralDirectiveDir is the central directive layer: the gastown repo's
// canonical checkout, read for every rig (gt-oz73l).
func CentralDirectiveDir(townRoot string) string {
	return filepath.Join(townRoot, "gastown", "mayor", "rig", "directives")
}

// RigDirectiveDir is a rig's own directive layer, in the rig's canonical
// checkout (gt-oz73l).
func RigDirectiveDir(townRoot, rig string) string {
	return filepath.Join(townRoot, rig, "mayor", "rig", ".gastown", "directives")
}

// layerDirectivePath is the file for name in one layer. The new directory's file
// wins whenever it exists. Otherwise the legacy file is read when it exists, and
// the new path (absent) is returned when neither does.
func layerDirectivePath(newDir, legacyDir, name string) string {
	newPath := filepath.Join(newDir, name+".md")
	if directiveFileExists(newPath) {
		return newPath
	}
	legacy := filepath.Join(legacyDir, name+".md")
	if directiveFileExists(legacy) {
		warnLegacyDirective(legacy)
		return legacy
	}
	return newPath
}

// directiveFileExists reports whether a directive file is there to read. Any
// stat failure other than absence counts as there, so the read reports it and a
// caller fails closed.
func directiveFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !errors.Is(err, fs.ErrNotExist)
}

// legacyDirectiveWarned is each legacy path already named this process.
var legacyDirectiveWarned sync.Map

// warnLegacyDirective names a legacy directive file once per process, so a
// prompt rendered on every spawn does not repeat the line.
func warnLegacyDirective(path string) {
	if _, done := legacyDirectiveWarned.LoadOrStore(path, true); done {
		return
	}
	log.Printf("directive %s is at a legacy path; move it under the gastown repo's directives (gt-oz73l)", path)
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

	townFiles, err := scanDirectiveLayer(CentralDirectiveDir(townRoot), filepath.Join(townRoot, "directives"), "town")
	if err != nil {
		return nil, err
	}
	files = append(files, townFiles...)

	rigs, err := rigNamesToScan(townRoot, rigName)
	if err != nil {
		return nil, err
	}
	for _, rig := range rigs {
		rigFiles, err := scanDirectiveLayer(RigDirectiveDir(townRoot, rig), filepath.Join(townRoot, rig, "directives"), rig)
		if err != nil {
			return nil, err
		}
		files = append(files, rigFiles...)
	}

	return files, nil
}

// scanDirectiveLayer lists one layer the way the reader resolves it: the files in
// the new directory, and the legacy files whose name the new directory does not
// have (gt-80rnu).
func scanDirectiveLayer(newDir, legacyDir, scope string) ([]DirectiveFile, error) {
	newFiles, err := readDirectiveDir(newDir, scope)
	if err != nil {
		return nil, err
	}
	legacyFiles, err := readDirectiveDir(legacyDir, scope)
	if err != nil {
		return nil, err
	}
	have := make(map[string]bool, len(newFiles))
	for _, f := range newFiles {
		have[f.Role] = true
	}
	out := newFiles
	for _, f := range legacyFiles {
		if !have[f.Role] {
			out = append(out, f)
		}
	}
	return out, nil
}

// IsTownLevelDir reports whether a top-level town directory is town state
// rather than a rig: the mayor and deacon agent directories, plus the hidden
// and plugin directories. Rig detection and the directive scan both call it,
// so they agree on which directories are rigs.
func IsTownLevelDir(name string) bool {
	switch name {
	case "mayor", "deacon", ".beads", ".claude", ".git", "plugins":
		return true
	}
	return false
}

// rigNamesToScan returns the single named rig, or every rig directory under
// townRoot that carries a config.json and is not a town-level directory. A
// named town-level directory is not a rig, so it yields no rigs.
func rigNamesToScan(townRoot, rigName string) ([]string, error) {
	if rigName != "" {
		if IsTownLevelDir(rigName) {
			return nil, nil
		}
		return []string{rigName}, nil
	}

	entries, err := os.ReadDir(townRoot)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", townRoot, err)
	}
	var rigs []string
	for _, d := range entries {
		if !d.IsDir() || IsTownLevelDir(d.Name()) {
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
