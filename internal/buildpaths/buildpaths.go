// Package buildpaths decides which repository paths change what `make build`
// produces, so the staleness check a user sees and the daemon's rebuild_gt job
// agree on whether a commit is due an install (gt-p62ku).
package buildpaths

import "strings"

// embedDirs are the package directories carrying //go:embed directives:
// internal/cmdtree (bd-command-tree.json), internal/config (roles/*.toml),
// internal/formula (formulas/*.formula.toml) and internal/templates (roles/,
// launchd/, systemd/, bodies/, townroot/claude.md, polecat-CLAUDE.md). What
// they embed is compiled into the binary, so a change anywhere under one is
// binary-affecting even when the file itself looks like documentation.
var embedDirs = []string{
	"internal/cmdtree/",
	"internal/config/",
	"internal/formula/",
	"internal/templates/",
}

// AffectsBuild reports whether changing repo-relative path p can change what
// `make build` produces. Documentation under docs/, markdown files, test
// sources and testdata cannot; anything under an embedDirs directory except a
// test source does, since the binary carries its contents.
func AffectsBuild(p string) bool {
	if !docOrTestPath(p) {
		return true
	}
	if strings.HasSuffix(p, "_test.go") {
		return false
	}
	for _, dir := range embedDirs {
		if strings.HasPrefix(p, dir) {
			return true
		}
	}
	return false
}

// docOrTestPath matches the documented non-binary shapes: docs/**, **/*.md,
// **/*_test.go and **/testdata/**.
func docOrTestPath(p string) bool {
	switch {
	case strings.HasPrefix(p, "docs/"):
		return true
	case strings.HasSuffix(p, ".md"):
		return true
	case strings.HasSuffix(p, "_test.go"):
		return true
	case p == "testdata", strings.HasPrefix(p, "testdata/"), strings.Contains(p, "/testdata/"):
		return true
	}
	return false
}
