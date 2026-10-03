// Package formularefs finds references to formulas a retirement deleted:
// code-review, design, mol-plan-review and mol-prd-review went with the convoy
// formula type (gt-gzhin.5); mol-convoy-feed and mol-convoy-cleanup went with
// the gt convoy commands and internal/convoy (gt-gzhin.7); and
// mol-idea-to-plan went on its own, for invoking gt sling flags that do not
// exist (gt-6lm4o). A shipped formula or role template that still names one
// hands an agent a command that cannot run, so the guard tests in
// internal/formula and internal/templates scan their embedded trees through
// this package.
package formularefs

import (
	"io/fs"
	"regexp"
	"strings"
)

// Removed lists the formulas a retirement deleted: the convoy formula type's
// (gt-gzhin.5), the two convoy-workflow formulas its last slice carried out
// (gt-gzhin.7), and mol-idea-to-plan (gt-6lm4o).
var Removed = []string{"code-review", "design", "mol-plan-review", "mol-prd-review", "mol-convoy-feed", "mol-convoy-cleanup", "mol-idea-to-plan"}

// uniqueRemoved reports whether name belongs to the deleted formulas alone, so
// that it is a reference wherever it stands. "design" is an ordinary English
// noun and shiny's step id, and "code-review" names a skill, so those two
// count only where a run verb or a file name makes the formula the subject.
func uniqueRemoved(name string) bool {
	return strings.HasPrefix(name, "mol-")
}

// invocation matches a run of a formula — `gt formula run <name>`, `gt sling
// <name>`, `--on <name>`, `--formula <name>` — with the name bare or quoted.
func invocation(name string) *regexp.Regexp {
	verbs := `(?:formula\s+(?:run|show|pour|cook)|sling|--on|--formula)`
	quotes := "[\"'`]?"
	return regexp.MustCompile(`(?i)` + verbs + `\s+` + quotes + regexp.QuoteMeta(name) + `\b`)
}

// fileRef matches the formula's file name. The leading boundary keeps
// mol-polecat-code-review.formula.toml from matching code-review.
func fileRef(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])` + regexp.QuoteMeta(name) + `\.formula\.toml`)
}

// standalone matches the name as a whole token, so a survivor whose own name
// ends in it (mol-polecat-code-review) is not flagged.
func standalone(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?:^|[^A-Za-z0-9_-])` + regexp.QuoteMeta(name) + `(?:[^A-Za-z0-9_-]|$)`)
}

// References returns one line per deleted formula text refers to.
func References(text string) []string {
	var out []string
	for _, name := range Removed {
		switch {
		case invocation(name).MatchString(text):
			out = append(out, "invokes removed formula "+name)
		case fileRef(name).MatchString(text):
			out = append(out, "names removed formula file "+name+".formula.toml")
		case uniqueRemoved(name) && standalone(name).MatchString(text):
			out = append(out, "names removed formula "+name)
		}
	}
	return out
}

// ScanFS returns one "<file>: <reference>" line per deleted formula referenced
// by any file directly under dir in fsys.
func ScanFS(fsys fs.FS, dir string) ([]string, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		data, err := fs.ReadFile(fsys, dir+"/"+entry.Name())
		if err != nil {
			return nil, err
		}
		for _, ref := range References(string(data)) {
			out = append(out, entry.Name()+": "+ref)
		}
	}
	return out, nil
}
