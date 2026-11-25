package land

import "strings"

// podEnginePinned reports whether the pod-engine doc pins the repository path:
// the doc cites it as path:line, or names it bare in a table cell. The match is
// on the whole path, so a longer path that ends with it, or a test file of it,
// is not pinned, and the empty path is never pinned (gt-63t22c). The doc's text
// is the input; the function reads no file.
func podEnginePinned(doc, path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	return podEngineCitesLine(doc, path) || podEngineBareCell(doc, path)
}

// podEngineCitesLine reports whether the doc has the path, at a token start,
// followed by a colon and a line number.
func podEngineCitesLine(doc, path string) bool {
	for start := 0; start <= len(doc); {
		j := strings.Index(doc[start:], path)
		if j < 0 {
			return false
		}
		i := start + j
		end := i + len(path)
		if (i == 0 || !podPathByte(doc[i-1])) && podLineSuffix(doc[end:]) {
			return true
		}
		start = i + 1
	}
	return false
}

// podEngineBareCell reports whether a table row has a cell that is the path
// alone, with or without backticks.
func podEngineBareCell(doc, path string) bool {
	for _, line := range strings.Split(doc, "\n") {
		row := strings.TrimSpace(line)
		if !strings.HasPrefix(row, "|") {
			continue
		}
		for _, cell := range strings.Split(row, "|") {
			if strings.Trim(strings.TrimSpace(cell), "`") == path {
				return true
			}
		}
	}
	return false
}

// podLineSuffix reports whether s starts with a colon and a line number.
func podLineSuffix(s string) bool {
	return len(s) >= 2 && s[0] == ':' && s[1] >= '0' && s[1] <= '9'
}

// podPathByte reports whether b can be part of a repository path, so a match
// that starts after one is inside a longer path.
func podPathByte(b byte) bool {
	return b == '/' || b == '.' || b == '_' || b == '-' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}
