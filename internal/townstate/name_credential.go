package townstate

import (
	"encoding/json"
	"strings"
)

// nameWords turns a path into words the detector can judge: a token hides in one
// segment of a name, joined to its extension by a dot or its directory by a slash,
// and the detector's own word split does not break on either.
func nameWords(r rune) rune {
	if r == '/' || r == '\\' || r == '.' {
		return ' '
	}
	return r
}

// NameHoldsCredential reports whether a file name carries a credential-shaped
// value. The name, split into its path segments and extension, goes through the
// detector the export runs over file content (FindCredentials) as a JSON string,
// because that detector reads a token-shaped word in a string value: the same rules
// judge a name as judge the bytes it names, and no second pattern is kept here. The
// nightly backup's log line withholds a name this reports (gt-ea3yjo).
func NameHoldsCredential(name string) bool {
	quoted, err := json.Marshal(strings.Map(nameWords, name))
	if err != nil {
		return false
	}
	return len(FindCredentials(quoted)) > 0
}
