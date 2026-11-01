package shelltier

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/tiertrigger"
)

// repoRoot is the worktree this test file sits in, so the guards read the
// scripts the manifest lists rather than a copy of them.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller did not report this test file's path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
}

// repoPathPrefixes are the repo trees and file a shell test names. A script
// that reads one of them must run when a path under it changes.
var repoPathPrefixes = []string{"scripts/", "deploy/", "plugins/", ".githooks/", "internal/", "Makefile"}

// scanRepoPaths returns the repo-relative paths text names, sorted and unique.
// A script writes them bare, in quotes, or after a variable such as $ROOT/ or
// $SCRIPT_DIR/, and every form is a dependency its triggers must cover.
func scanRepoPaths(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, prefix := range repoPathPrefixes {
		for i := 0; ; {
			j := strings.Index(text[i:], prefix)
			if j < 0 {
				break
			}
			start := i + j
			i = start + 1
			if start > 0 && isPathWordByte(text[start-1]) {
				continue // a prefix inside a longer name is not a repo path
			}
			end := start + len(prefix)
			for end < len(text) && isPathByte(text[end]) {
				end++
			}
			if p := text[start:end]; !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	slices.Sort(out)
	return out
}

// isPathWordByte reports whether b continues a word. The separator / is not
// one, so $ROOT/scripts/x.sh and https://host/scripts/x.sh both name a path.
func isPathWordByte(b byte) bool {
	return b == '_' || b == '-' || b == '.' ||
		('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z') || ('0' <= b && b <= '9')
}

// isPathByte reports whether b continues a repo-relative path.
func isPathByte(b byte) bool {
	return isPathWordByte(b) || b == '/'
}

// uncoveredRepoPaths returns the paths text names that the entry's triggers do
// not run it for. A path that is named but not covered would let a change to
// it land without running this script (test-engineering rule 6).
func uncoveredRepoPaths(e Entry, text string) []string {
	triggers := e.TriggersOrDefault()
	var out []string
	for _, p := range scanRepoPaths(text) {
		if !tiertrigger.Runs(triggers, []string{p}, false) {
			out = append(out, p)
		}
	}
	return out
}

func TestEveryReferencedRepoPathIsATrigger(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	read := func(rel string) (string, error) {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		return string(b), err
	}
	for _, e := range Manifest() {
		t.Run(e.Path, func(t *testing.T) {
			t.Parallel()
			text, err := read(e.Path)
			if err != nil {
				t.Fatalf("read %s: %v", e.Path, err)
			}
			for _, p := range uncoveredRepoPaths(e, text) {
				t.Errorf("%s names %s, but its triggers %q do not run it when %s changes",
					e.Path, p, e.TriggersOrDefault(), p)
			}
		})
	}
}

func TestScanFindsPathsInQuotesAndVariables(t *testing.T) {
	t.Parallel()
	text := `set -euo pipefail
SRC="$SCRIPT_DIR/../scripts/install-local.sh"
lib="$ROOT/scripts/lib/log.sh"
. "$REPO/.githooks/pre-push"
python3 "$ROOT/plugins/polecat-image/build.py"
cp deploy/k8s/kind/values/kind.yaml "$TMP/"
go test ./internal/shelltier
make -C "$ROOT" -f Makefile
echo 'not a path: myscripts/x.sh'
`
	want := []string{
		".githooks/pre-push",
		"Makefile",
		"deploy/k8s/kind/values/kind.yaml",
		"internal/shelltier",
		"plugins/polecat-image/build.py",
		"scripts/install-local.sh",
		"scripts/lib/log.sh",
	}
	if got := scanRepoPaths(text); !slices.Equal(got, want) {
		t.Fatalf("scanRepoPaths() = %q, want %q", got, want)
	}
}

// goCommandRE matches a go tool or make install in command position: at the
// start of a line, after a command separator, or after a shell keyword that
// introduces one. A go mention in a comment or a fixture string is not one.
var goCommandRE = regexp.MustCompile(`(?m)(?:^|[;&|({]|\$\(|\b(?:then|do|else|if|while|until|elif)\b[[:space:]]*)[[:space:]]*(go[[:space:]]+(?:build|run|test|vet|install)|make[[:space:]]+install)\b`)

// goCommand returns the first go or make install command text runs, or "".
func goCommand(text string) string {
	if m := goCommandRE.FindStringSubmatch(text); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// stubCitationRE matches the file a stub: reason cites, with an optional line.
var stubCitationRE = regexp.MustCompile(`^([A-Za-z0-9._/-]+)(?::([0-9]+))?$`)

// stubCitation returns the file a stub: reason cites and its line, if any.
func stubCitation(reason string) (file string, line int, ok bool) {
	rest, found := strings.CutPrefix(reason, "stub:")
	if !found {
		return "", 0, false
	}
	for _, word := range strings.Fields(rest) {
		m := stubCitationRE.FindStringSubmatch(strings.Trim(word, "(),;."))
		if m == nil || !strings.Contains(m[1], "/") {
			continue
		}
		if m[2] == "" {
			return m[1], 0, true
		}
		n, err := strconv.Atoi(m[2])
		if err != nil {
			continue
		}
		return m[1], n, true
	}
	return "", 0, false
}

// fakeGoRE matches a fake go a script defines: a go (or go-1.27) executable
// written under a temporary bin directory, or a go() shell function.
var fakeGoRE = regexp.MustCompile(`\$[A-Za-z_][A-Za-z0-9_]*/go(?:[.-][0-9][0-9.]*)?\b|\bgo[[:space:]]*\([[:space:]]*\)`)

// goExemptionProblem returns why the entry's StubsGo exemption is not
// supported by its script text and the files the reason cites, or "". A
// none: exemption claims the script runs no go, and a stub: exemption names
// where the script's fake go is defined; either claim that the text does not
// support makes the script run on Go changes, so the guard refuses it.
func goExemptionProblem(e Entry, text string, files func(string) (string, error)) string {
	switch {
	case e.StubsGo == "":
		return "" // no exemption: the entry already runs on every Go change
	case strings.HasPrefix(e.StubsGo, "none:"):
		if cmd := goCommand(text); cmd != "" {
			return fmt.Sprintf("%s carries %q but runs %q", e.Path, e.StubsGo, cmd)
		}
		return ""
	case strings.HasPrefix(e.StubsGo, "stub:"):
	case strings.HasPrefix(e.StubsGo, "go:") && hasGoDeclaration(e.StubsGo):
		return ""
	default:
		return fmt.Sprintf("%s carries %q, which is no exemption the guard can check", e.Path, e.StubsGo)
	}
	file, line, ok := stubCitation(e.StubsGo)
	if !ok {
		return fmt.Sprintf("%s carries %q, and the reason cites no file:line", e.Path, e.StubsGo)
	}
	if clean := path.Clean(file); path.IsAbs(file) || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Sprintf("%s carries %q, and %s is not a path under the repo root", e.Path, e.StubsGo, file)
	}
	evidence, err := files(file)
	if err != nil {
		return fmt.Sprintf("%s carries %q, but %s cannot be read: %v", e.Path, e.StubsGo, file, err)
	}
	if line > 0 {
		lines := strings.Split(evidence, "\n")
		if line > len(lines) {
			return fmt.Sprintf("%s carries %q, but %s has only %d lines", e.Path, e.StubsGo, file, len(lines))
		}
		evidence = lines[line-1]
	}
	if !fakeGoRE.MatchString(evidence) {
		return fmt.Sprintf("%s carries %q, but %s does not define a fake go", e.Path, e.StubsGo, file)
	}
	return ""
}

func TestEveryStubsGoExemptionIsSupportedByTheScript(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	readFile := func(rel string) (string, error) {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	fixtureFiles := func(m map[string]string) func(string) (string, error) {
		return func(rel string) (string, error) {
			text, ok := m[rel]
			if !ok {
				return "", os.ErrNotExist
			}
			return text, nil
		}
	}
	const stubScript = "set -euo pipefail\nprintf 'echo go\\n' > \"$BIN/go\"\n"
	tests := []struct {
		name  string
		entry Entry
		text  string
		files map[string]string
		want  string
	}{
		{
			"a none: script that mentions go only in a grep pattern passes",
			Entry{Path: "scripts/plain_test.sh", StubsGo: "none: names no go command"},
			"set -euo pipefail\ngrep -q 'go build' README.md\n",
			nil, "",
		},
		{
			"a none: script that runs go build is named",
			Entry{Path: "scripts/build_test.sh", StubsGo: "none: names no go command"},
			"set -euo pipefail\ngo build ./...\n",
			nil, "scripts/build_test.sh",
		},
		{
			"a none: script that runs make install after a separator is named",
			Entry{Path: "scripts/install_test.sh", StubsGo: "none: names no go command"},
			"set -euo pipefail\nmake lint && make install\n",
			nil, "make install",
		},
		{
			"a stub: reason citing the line that defines the fake go passes",
			Entry{Path: "scripts/stub_test.sh", StubsGo: "stub: scripts/stub_test.sh:2 puts a fake go on PATH"},
			stubScript,
			map[string]string{"scripts/stub_test.sh": stubScript}, "",
		},
		{
			"a stub: reason citing a line with no fake go fails",
			Entry{Path: "scripts/stub_test.sh", StubsGo: "stub: scripts/stub_test.sh:1 puts a fake go on PATH"},
			stubScript,
			map[string]string{"scripts/stub_test.sh": stubScript}, "scripts/stub_test.sh",
		},
		{
			"a stub: reason citing a line past the end of the file fails",
			Entry{Path: "scripts/stub_test.sh", StubsGo: "stub: scripts/stub_test.sh:9 puts a fake go on PATH"},
			stubScript,
			map[string]string{"scripts/stub_test.sh": stubScript}, "scripts/stub_test.sh has only 3 lines",
		},
		{
			"a stub: reason citing a file that does not exist fails",
			Entry{Path: "scripts/stub_test.sh", StubsGo: "stub: scripts/absent_test.sh puts a fake go on PATH"},
			"", nil, "scripts/absent_test.sh",
		},
		{
			"a stub: reason that cites no file fails",
			Entry{Path: "scripts/stub_test.sh", StubsGo: "stub: puts a fake go on PATH"},
			"", nil, "cites no file",
		},
		{
			"a stub: reason citing a path outside the repo fails",
			Entry{Path: "scripts/stub_test.sh", StubsGo: "stub: /etc/passwd puts a fake go on PATH"},
			"", nil, "not a path under the repo root",
		},
		{
			"an exemption the guard cannot read fails",
			Entry{Path: "scripts/stub_test.sh", StubsGo: "bogus"},
			"", nil, "no exemption the guard can check",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			files := fixtureFiles(tc.files)
			got := goExemptionProblem(tc.entry, tc.text, files)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("goExemptionProblem() = %q, want no problem", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("goExemptionProblem() = %q, want one naming %q", got, tc.want)
			}
		})
	}
	for _, e := range Manifest() {
		if e.StubsGo == "" {
			continue
		}
		t.Run("manifest/"+e.Path, func(t *testing.T) {
			t.Parallel()
			text, err := readFile(e.Path)
			if err != nil {
				t.Fatalf("read %s: %v", e.Path, err)
			}
			if p := goExemptionProblem(e, text, readFile); p != "" {
				t.Fatal(p)
			}
		})
	}
}
