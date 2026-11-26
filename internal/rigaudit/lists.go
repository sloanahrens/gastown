// Package rigaudit audits the town config's lists of rig names.
package rigaudit

import (
	"fmt"
	"slices"
	"strings"
)

// NamedList is a list of strings found in a config document, with the path
// that leads to it.
type NamedList struct {
	Path  string
	Names []string
}

// RigLists returns every non-empty list of strings in a decoded JSON document,
// sorted by Path. A list whose elements are not all strings is skipped, but
// its elements are still walked.
func RigLists(config any) []NamedList {
	var out []NamedList
	walkLists(config, "", &out)
	slices.SortFunc(out, func(a, b NamedList) int {
		return strings.Compare(a.Path, b.Path)
	})
	return out
}

// walkLists appends each string list under v to out, with paths rooted at path.
func walkLists(v any, path string, out *[]NamedList) {
	switch v := v.(type) {
	case map[string]any:
		for key, child := range v {
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			walkLists(child, childPath, out)
		}
	case []any:
		if names, ok := stringsOnly(v); ok && len(names) > 0 {
			*out = append(*out, NamedList{Path: path, Names: names})
		}
		for i, child := range v {
			walkLists(child, fmt.Sprintf("%s[%d]", path, i), out)
		}
	}
}

// stringsOnly returns the elements of list as strings, and false if any
// element is not a string.
func stringsOnly(list []any) ([]string, bool) {
	names := make([]string, 0, len(list))
	for _, e := range list {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		names = append(names, s)
	}
	return names, true
}

// RequiredLists are the config paths of the daemon's per-rig lists. A rig that
// is unparked but missing from one of them half-works, so a gap there is required.
var RequiredLists = []string{
	"daemon.patrols.landing_worker.rigs",
	"daemon.patrols.patrol_scan.rigs",
	"daemon.patrols.patrol_scan.worktree_cleanup.rigs",
}

// Problem is one finding from a config check. Kind is "required" or "info".
type Problem struct {
	Kind string
	Text string
}

// CheckLists reports each list in config that names a known rig but lacks an
// unparked rig, and each RequiredLists path that no list naming a known rig
// holds. A list that names no known rig is ignored. Problems are sorted by
// Kind, then Text.
func CheckLists(config any, unparked, known []string) []Problem {
	var problems []Problem
	seen := map[string]bool{}
	for _, list := range RigLists(config) {
		if !slices.ContainsFunc(list.Names, func(n string) bool { return slices.Contains(known, n) }) {
			continue
		}
		seen[list.Path] = true
		var missing []string
		for _, rig := range unparked {
			if !slices.Contains(list.Names, rig) {
				missing = append(missing, rig)
			}
		}
		if len(missing) == 0 {
			continue
		}
		slices.Sort(missing)
		kind := "info"
		if slices.Contains(RequiredLists, list.Path) {
			kind = "required"
		}
		problems = append(problems, Problem{kind, fmt.Sprintf("%s lacks %s", list.Path, strings.Join(slices.Compact(missing), ", "))})
	}
	for _, path := range RequiredLists {
		if !seen[path] {
			problems = append(problems, Problem{"required", path + " is absent or names no known rig"})
		}
	}
	slices.SortFunc(problems, func(a, b Problem) int {
		if c := strings.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		return strings.Compare(a.Text, b.Text)
	})
	return problems
}
