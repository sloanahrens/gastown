package models

import (
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// modelFamilies are the model names a backend is read as, in the order a longer
// name is given up for one of them: a name carrying "sonnet", "flash" or "pro"
// is that family, whatever release prefix its vendor puts on it. The short name
// is what an operator reads, and a release number is not what they ask for when
// they ask what the town runs (gt-bj47s).
var modelFamilies = []string{"sonnet", "opus", "haiku", "flash", "pro"}

// ModelName names the model an argv runs, in the short form an operator uses
// for it. The argv is a backend — ["/<home>/.local/bin/claude", "-p",
// "--model", "sonnet"] — so the model is what its --model flag names, and an
// argv run under a wrapper that names none is read from the wrapper's own name
// ("claude-deepseek-flash" is flash). An argv that is neither shows under its
// command name, which is all the town knows about it.
func ModelName(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	name := backendModel(argv)
	if name == "" {
		name = filepath.Base(argv[0])
	}
	for _, family := range modelFamilies {
		if modelWord(name, family) {
			return family
		}
	}
	return name
}

// backendModel returns the model an argv names: the value of --model or -m, in
// either the separate or the --model= spelling. A flag with no value after it
// names nothing.
func backendModel(argv []string) string {
	for i, arg := range argv {
		switch {
		case arg == "--model" || arg == "-m":
			if i+1 < len(argv) {
				return argv[i+1]
			}
		case strings.HasPrefix(arg, "--model="):
			return strings.TrimPrefix(arg, "--model=")
		}
	}
	return ""
}

// modelWord reports whether word is one of name's own words, for a name split
// on anything that is not a letter or a digit: "flash" matches deepseek-flash
// and claude-flash-2026, and never flashback.
func modelWord(name, word string) bool {
	for _, w := range strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if w == word {
			return true
		}
	}
	return false
}

// ExpandHome resolves a leading "~/" to the home directory, which is how the
// table spells a path in om_backend. A path with no home to expand is itself.
func ExpandHome(s string) string { return expandHome(hostHome(), s) }

// expandHome is ExpandHome against home, so a test with a home of its own does
// not read the process environment (gt-jaagc).
func expandHome(home, s string) string {
	if home == "" || !strings.HasPrefix(s, "~/") {
		return s
	}
	return filepath.Join(home, s[2:])
}

// SameCommand compares two commands word for word, a leading "~/" standing for
// the home directory, so a table's "~/bin/claude" is the live "/home/x/bin/claude".
func SameCommand(a, b []string) bool { return sameCommandIn(hostHome(), a, b) }

// sameCommandIn is SameCommand against home.
func sameCommandIn(home string, a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if expandHome(home, a[i]) != expandHome(home, b[i]) {
			return false
		}
	}
	return true
}

// hostHome is the home directory the process runs under, "" when there is none.
func hostHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}
