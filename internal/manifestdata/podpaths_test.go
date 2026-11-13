package manifestdata

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// podRootLiteral matches the pod town root spelled as a path: followed by the end
// of the path, a quote, a backtick, a space, or a slash that starts a child path.
// The sibling paths (/Users/sloan/gt-landing.noindex and the rest) continue with a
// letter or a dash, so they do not match.
var podRootLiteral = regexp.MustCompile("/Users/sloan/gt(?:[/\"` \\t]|$)")

// podRootSpellings returns the 1-based lines of src that spell the pod town root
// literally.
func podRootSpellings(src string) []int {
	var lines []int
	for i, line := range strings.Split(src, "\n") {
		if podRootLiteral.MatchString(line) {
			lines = append(lines, i+1)
		}
	}
	return lines
}

// TestPodRootLiteralScanner pins the scanner the guard runs, so a fixture the guard
// must catch, and one it must let through, are each judged as written.
func TestPodRootLiteralScanner(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		src  string
		want bool
	}{
		{name: "a quoted literal", src: `const root = "/Users/sloan/gt"`, want: true},
		{name: "a path under the root", src: `dir := "/Users/sloan/gt/rig/polecats"`, want: true},
		{name: "a literal in help text", src: "  gt town state seed --town /Users/sloan/gt --namespace rehearsal", want: true},
		{name: "a literal at the end of a line", src: "root = /Users/sloan/gt", want: true},
		{name: "the landing sibling", src: `LandingWorkPath = "/Users/sloan/gt-landing.noindex"`, want: false},
		{name: "the backup sibling", src: `DoltBackupPath = "/Users/sloan/gt-backups/dolt"`, want: false},
		{name: "the beads sibling", src: `ClaudeBeadsRoot = "/Users/sloan/.claude/.beads"`, want: false},
		{name: "a longer name under the account", src: `x := "/Users/sloan/gtx"`, want: false},
		{name: "the constant by name", src: `TownRootPath = manifestdata.PodTownRoot`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := len(podRootSpellings(tt.src)) > 0
			if got != tt.want {
				t.Fatalf("podRootSpellings(%q) flags it = %v, want %v", tt.src, got, tt.want)
			}
		})
	}
}

// TestNoLiteralPodTownRootOutsideTheConstant: no non-test Go file under deploy/,
// internal/ or cmd/ spells the pod town root literally, so a change of the root is
// one constant. The walk must read deploy/k8s/daemon/daemon.go, or an empty walk
// would pass.
func TestNoLiteralPodTownRootOutsideTheConstant(t *testing.T) {
	t.Parallel()
	repo := filepath.Join(packageDir(t), "..", "..")
	const constantFile = "internal/manifestdata/podpaths.go"
	var scanned []string
	var findings []string
	for _, top := range []string{"deploy", "internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(repo, top), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(repo, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			if rel == constantFile {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned = append(scanned, rel)
			for _, line := range podRootSpellings(string(data)) {
				findings = append(findings, rel+":"+strconv.Itoa(line))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", top, err)
		}
	}
	if !slices.Contains(scanned, "deploy/k8s/daemon/daemon.go") {
		t.Fatalf("the walk read %d files and not deploy/k8s/daemon/daemon.go, so it passed for the wrong reason", len(scanned))
	}
	if len(findings) != 0 {
		t.Fatalf("the pod town root is spelled literally outside %s: %s", constantFile, strings.Join(findings, ", "))
	}
}
