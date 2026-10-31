// Package buildflags is the one description of how a gt binary is compiled:
// the ldflags it needs to run at all, and the cgo environment a keg-only
// Homebrew icu4c needs on darwin.
//
// Both the integration test helper in internal/cmd and the tier sweep's
// one-time build take their flags from here, so the binary the sweep hands the
// tests is the binary each helper would have built itself (gt-2daks).
package buildflags

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// LDFlags is the -ldflags every gt build passes. Without BuiltProperly=1 a
// binary built on darwin refuses to run outside a test (internal/cmd/root.go).
const LDFlags = "-X github.com/steveyegge/gastown/internal/cmd.BuiltProperly=1"

// ICUCgoEnv returns the CGO_CPPFLAGS and CGO_LDFLAGS entries a gt build needs
// on this host, appended to base, or nil when it needs none.
//
// go-icu-regex, a dolt dependency, includes unicode/regex.h, and Homebrew
// installs icu4c keg-only, so the compiler does not search it: without these a
// bare `go build` dies with "unicode/regex.h file not found". The Makefile
// detects the prefix and exports the same flags for `make build` and
// `make test`; a build that runs from neither, like the test helpers' or the
// sweep's, has to ask for itself.
func ICUCgoEnv(base []string) []string {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return darwinCgoEnv(base, BrewICU4CPrefix())
}

// darwinCgoEnv is ICUCgoEnv's rule for a darwin host whose icu4c keg is
// prefix, "" when no keg holds the header. It is nil when base already carries
// either flag: a caller's own setting wins, as cmd/go gives a process value
// precedence over the value in its env file.
func darwinCgoEnv(base []string, prefix string) []string {
	if prefix == "" || envHas(base, "CGO_CPPFLAGS") || envHas(base, "CGO_LDFLAGS") {
		return nil
	}
	return []string{
		"CGO_CPPFLAGS=-I" + filepath.Join(prefix, "include"),
		"CGO_LDFLAGS=-L" + filepath.Join(prefix, "lib"),
	}
}

// envHas reports whether env sets name to a non-empty value, the way
// os.Getenv reads it: an entry with an empty value is unset.
func envHas(env []string, name string) bool {
	prefix := name + "="
	for _, entry := range env {
		if len(entry) > len(prefix) && strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

// BrewICU4CPrefix returns the icu4c prefix that holds unicode/regex.h, or ""
// when none is installed.
//
// `brew --prefix icu4c` names the formula brew currently resolves, which can
// be a version that is not installed (a clean HOME resolves icu4c to icu4c@79
// through the formula API while only icu4c@78 is installed). Only a prefix
// that holds the header is used; otherwise the installed icu4c kegs under
// brew's opt directory are searched, newest first.
func BrewICU4CPrefix() string {
	const header = "include/unicode/regex.h"
	out, err := exec.Command("brew", "--prefix", "icu4c").Output()
	if err != nil {
		return ""
	}
	if named := strings.TrimSpace(string(out)); named != "" {
		if _, statErr := os.Stat(filepath.Join(named, header)); statErr == nil {
			return named
		}
	}
	root, err := exec.Command("brew", "--prefix").Output()
	if err != nil {
		return ""
	}
	kegs, err := filepath.Glob(filepath.Join(strings.TrimSpace(string(root)), "opt", "icu4c*"))
	if err != nil {
		return ""
	}
	sort.Sort(sort.Reverse(sort.StringSlice(kegs)))
	for _, keg := range kegs {
		if _, statErr := os.Stat(filepath.Join(keg, header)); statErr == nil {
			return keg
		}
	}
	return ""
}
