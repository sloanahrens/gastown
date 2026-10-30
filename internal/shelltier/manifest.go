// Package shelltier lists the shell tests the post-land shell tier runs, the
// paths whose change can move each verdict, and the guard that keeps that list
// and the tracked tests in step (gt-u560q.1).
package shelltier

import (
	"fmt"
	"slices"
	"strings"
)

// Mode is how the runner schedules a shell test.
type Mode int

const (
	// Serial runs the test alone, one after another with the others.
	Serial Mode = iota
	// Parallel lets the test share a pool with other parallel tests.
	Parallel
)

// Entry is one shell test in the manifest.
type Entry struct {
	// Path is the repo-relative path of the test script.
	Path string
	// Triggers are the repo paths whose change can move the verdict; empty means DefaultTriggers.
	Triggers []string
	Mode     Mode
	// StubsGo is the exemption and its reason in one field: "none: ..." when the
	// script names no go command, "stub: file:line ..." when a fake go is defined
	// there. "go: ..." declares that the script runs the real go, so it runs on Go
	// changes. Empty is not a declaration, and it also runs on Go changes.
	StubsGo string
}

// DefaultTriggers are the paths a test runs on when its entry declares none.
var DefaultTriggers = []string{"scripts/", "Makefile"}

// GoTriggers are the Go tree paths that run every entry without a StubsGo exemption.
var GoTriggers = []string{"go.mod", "go.sum", "cmd/", "internal/", "plugins/"}

// TriggersOrDefault returns the paths whose change runs this test. It fails
// closed: an entry whose StubsGo is not a reason also runs on every Go change,
// because a script the guard cannot classify must run more often, not less.
func (e Entry) TriggersOrDefault() []string {
	triggers := e.Triggers
	if len(triggers) == 0 {
		triggers = DefaultTriggers
	}
	out := slices.Clone(triggers)
	if !hasReason(e.StubsGo) {
		out = append(out, GoTriggers...)
	}
	return out
}

// hasReason reports whether s is a StubsGo reason: a "none:" or "stub:" prefix
// followed by text. A bare prefix, or any other text, is not a reason.
func hasReason(s string) bool {
	for _, prefix := range []string{"none:", "stub:"} {
		if rest, ok := strings.CutPrefix(s, prefix); ok && strings.TrimSpace(rest) != "" {
			return true
		}
	}
	return false
}

// hasGoDeclaration reports whether s declares that the script runs the real go:
// a "go:" prefix followed by text. Such an entry is not exempt, so it runs on
// every Go change. A bare prefix, or any other text, is not a declaration.
func hasGoDeclaration(s string) bool {
	rest, ok := strings.CutPrefix(s, "go:")
	return ok && strings.TrimSpace(rest) != ""
}

// Manifest lists every shell test in the order the runner runs them: the
// scripts/test-makefile.sh order first, then the tests the shell tier adds.
func Manifest() []Entry {
	return []Entry{
		{Path: "scripts/check-install-path_test.sh", StubsGo: "none: names no go command"},
		{Path: "scripts/polecat-go-floor_test.sh", StubsGo: "stub: scripts/polecat-go-floor_test.sh:66 defines fake GO_BIN binaries"},
		{Path: "scripts/install-local_test.sh", StubsGo: "none: names no go command"},
		{Path: "scripts/deep-review_test.sh", StubsGo: "none: go test appears only in fixture text"},
		{Path: "scripts/flake-sweep_test.sh", StubsGo: "stub: scripts/flake-sweep_test.sh:25 puts a fake go on PATH"},
		{Path: "scripts/gate-vs-load_test.sh", StubsGo: "none: names no go command"},
		{Path: "scripts/forgejo-provision_test.sh", StubsGo: "none: names no go command"},
		{Path: "scripts/forgejo-probe_test.sh", StubsGo: "none: names no go command"},
		{Path: "scripts/forgejo-resync_test.sh", StubsGo: "none: names no go command"},
		{Path: "scripts/forgejo-cutover_test.sh", StubsGo: "none: names no go command"},
		{Path: "deploy/k8s/kind/kind_test.sh", StubsGo: "none: names no go command"},
		{Path: "scripts/resource-sampler_test.sh", StubsGo: "none: go test appears only in a ps fixture line"},
		{Path: "scripts/makefile-gate_test.sh", StubsGo: "go: the shell-test list case runs the real shelltier through go run (gt-chlve)"},
		{Path: ".githooks/pre-push_test.sh", StubsGo: "none: names no go command"},
		{Path: "scripts/post-land-shell_test.sh", StubsGo: "stub: scripts/post-land-shell_test.sh puts a fake go on PATH"},
		{Path: "scripts/town_metrics_test.sh", StubsGo: "none: names no go command"},
	}
}

// Lister returns the repo paths of the tracked shell tests.
type Lister func() ([]string, error)

// Runner executes a command and returns its standard output.
type Runner func(name string, args ...string) ([]byte, error)

// GitLister lists the tracked shell tests with git, leaving out fixtures under a
// testdata directory, which are data for a test and not tests of their own.
func GitLister(run Runner) Lister {
	return func() ([]string, error) {
		out, err := run("git", "ls-files", "*_test.sh")
		if err != nil {
			return nil, fmt.Errorf("git ls-files: %w", err)
		}
		var paths []string
		for _, p := range strings.Split(string(out), "\n") {
			if p == "" || strings.Contains("/"+p, "/testdata/") {
				continue
			}
			paths = append(paths, p)
		}
		return paths, nil
	}
}

// CheckCovered returns an error naming each tracked shell test that has no entry
// in the manifest, so a new test cannot land without one.
func CheckCovered(list Lister, entries []Entry) error {
	tracked, err := list()
	if err != nil {
		return fmt.Errorf("list tracked shell tests: %w", err)
	}
	if len(tracked) == 0 {
		return fmt.Errorf("no tracked shell tests listed, so coverage cannot be shown")
	}
	listed := make(map[string]bool, len(entries))
	for _, e := range entries {
		listed[e.Path] = true
	}
	var missing []string
	for _, p := range tracked {
		if !listed[p] {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("tracked shell tests missing from the manifest: %s", strings.Join(missing, ", "))
	}
	return nil
}

// CheckExist returns an error naming each manifest entry that exists reports
// absent, so a removed or renamed script leaves the manifest in the same change.
func CheckExist(entries []Entry, exists func(path string) bool) error {
	var missing []string
	for _, e := range entries {
		if !exists(e.Path) {
			missing = append(missing, e.Path)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("manifest entries with no script on disk: %s", strings.Join(missing, ", "))
	}
	return nil
}
