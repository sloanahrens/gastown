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
