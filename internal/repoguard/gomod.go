package repoguard

import (
	"regexp"
	"strings"
)

// goModReplaceDirective matches a line that opens a replace directive: after
// any indent, the keyword, then whitespace (a one-line replace) or an opening
// parenthesis (a block). The indent is allowed because Go's modfile parser
// honors a top-level replace set off by spaces or a tab, and a column-0 match
// would let it through.
var goModReplaceDirective = regexp.MustCompile(`^[[:space:]]*replace([[:space:]]|\()`)

// trackerReplace is the one replace go.mod may carry: the in-repo tracker
// module cannot be fetched by version, so a checkout installs it from the tree
// (ADR 0006).
const trackerReplace = "replace github.com/steveyegge/gastown/tracker => ./tracker"

// CheckGoModReplaces returns the lines of goMod that open a replace directive
// other than the tracker module's, which only a whole-line match admits.
func CheckGoModReplaces(goMod string) []string {
	var offending []string
	for _, line := range strings.Split(goMod, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if goModReplaceDirective.MatchString(line) && strings.TrimSpace(line) != trackerReplace {
			offending = append(offending, line)
		}
	}
	return offending
}
