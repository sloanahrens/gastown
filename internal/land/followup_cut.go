package land

import (
	"path"
	"strings"
)

// followupCutKey is the source file a follow-up finding's work belongs to. A
// test file shares the key of the source file it tests, so spec_test.go and
// spec.go both key to spec.go; every other path is its own key. An empty path,
// or one that cleans to the current directory, has no key and returns "".
func followupCutKey(file string) string {
	clean := path.Clean(strings.TrimSpace(file))
	if clean == "." {
		return ""
	}
	if source, ok := strings.CutSuffix(clean, "_test.go"); ok {
		return source + ".go"
	}
	return clean
}
