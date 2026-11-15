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

// rootLiteral is the guard's rule for a town root spelled as a path: the root
// followed by the end of the line, or by a character that cannot continue the
// name `gt`. A letter, digit, underscore or dash continues the name, so the
// sibling paths (the landing work path and the rest) do not match; punctuation
// after the name ends it.
func rootLiteral(root string) *regexp.Regexp {
	return regexp.MustCompile(regexp.QuoteMeta(root) + `(?:[^A-Za-z0-9_-]|$)`)
}

// podRootLiteral is the rule applied to the pod town root, the root's one
// spelling, so the guard and the constant cannot drift apart.
var podRootLiteral = rootLiteral(PodTownRoot)

// spellings returns the 1-based lines of src that match literal.
func spellings(literal *regexp.Regexp, src string) []int {
	var lines []int
	for i, line := range strings.Split(src, "\n") {
		if literal.MatchString(line) {
			lines = append(lines, i+1)
		}
	}
	return lines
}

// podRootSpellings returns the 1-based lines of src that spell the pod town root
// literally.
func podRootSpellings(src string) []int {
	return spellings(podRootLiteral, src)
}

// TestPodRootLiteralScanner pins the scanner the guard runs, so a fixture the guard
// must catch, and one it must let through, are each judged as written. The rows
// spell a placeholder root, which keeps a person's account out of the tree: the
// rule holds around any root, so what they pin is the shape of the line beside
// it. The production rule is checked against PodTownRoot at the end.
func TestPodRootLiteralScanner(t *testing.T) {
	t.Parallel()
	literal := rootLiteral("/home/user/gt")
	tests := []struct {
		name string
		src  string
		want bool
	}{
		{name: "a quoted literal", src: `const root = "/home/user/gt"`, want: true},
		{name: "a path under the root", src: `dir := "/home/user/gt/rig/polecats"`, want: true},
		{name: "a literal in help text", src: "  gt town state seed --town /home/user/gt --namespace rehearsal", want: true},
		{name: "a literal at the end of a line", src: "root = /home/user/gt", want: true},
		{name: "a literal before a comma", src: `// the root defaults to /home/user/gt, then a rename moves it`, want: true},
		{name: "a literal before a sentence period", src: `// the claim mounts at /home/user/gt.`, want: true},
		{name: "a literal before a close paren", src: `x := f(/home/user/gt)`, want: true},
		{name: "a literal before a semicolon", src: `root=/home/user/gt; export GT_TOWN_ROOT`, want: true},
		{name: "a literal before a colon", src: `root: /home/user/gt`, want: true},
		{name: "a literal in single quotes", src: `run('/home/user/gt')`, want: true},
		{name: "the landing sibling", src: `LandingWorkPath = "/home/user/gt-landing.noindex"`, want: false},
		{name: "the backup sibling", src: `DoltBackupPath = "/home/user/gt-backups/dolt"`, want: false},
		{name: "the beads sibling", src: `ClaudeBeadsRoot = "/home/user/.claude/.beads"`, want: false},
		{name: "a longer name under the account", src: `x := "/home/user/gtx"`, want: false},
		{name: "a digit continues the name", src: `x := "/home/user/gt9"`, want: false},
		{name: "an underscore continues the name", src: `x := "/home/user/gt_old"`, want: false},
		{name: "the constant by name", src: `TownRootPath = manifestdata.PodTownRoot`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := len(spellings(literal, tt.src)) > 0
			if got != tt.want {
				t.Fatalf("spellings(%q) flags it = %v, want %v", tt.src, got, tt.want)
			}
		})
	}
	if got := podRootSpellings(PodTownRoot); len(got) != 1 {
		t.Fatalf("the production rule flags %d lines of the pod town root, want 1", len(got))
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
