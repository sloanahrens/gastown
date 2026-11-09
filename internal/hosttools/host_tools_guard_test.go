package hosttools

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain runs the tests under unittier.Main, which fails the run when a
// goroutine outlives the tests.
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// hostToolsAllowPath names the ratchet file listing every non-test Go file that
// still names a host-only tool in a string literal.
const hostToolsAllowPath = "internal/hosttools/testdata/host-tools-allow.txt"

// hostToolsAllowClass is the only allowlist class. An entry stays until the
// deletion area its reason names removes the use.
const hostToolsAllowClass = "pending-deletion"

// hostTools are the Mac-only tools the gt-in-container design deletes. No commit
// may add a use of one of them to the gt module.
var hostTools = []string{"launchctl", "cmux", "osascript", "diskutil"}

// hostToolPattern matches a tool as a whole word, so an identifier such as
// launchctlx is not a use of launchctl.
var hostToolPattern = regexp.MustCompile(`\b(` + strings.Join(hostTools, "|") + `)\b`)

// skippedScanDirs are the directories the scan does not descend into. No other
// directory is skipped by name: a skip by a name a new package could take is a
// way around the ratchet.
var skippedScanDirs = map[string]bool{
	".git":         true,
	"vendor":       true,
	"node_modules": true,
	"testdata":     true,
}

// hostToolKey identifies one use: a file and the tool it names. The allowlist
// keys on this pair, so an edit that moves the use within the file keeps its
// entry.
type hostToolKey struct {
	file string
	tool string
}

// hostToolHit is a tool one file names: the number of string literals that name
// it, and the line of the first one.
type hostToolHit struct {
	hostToolKey
	line  int
	count int
}

// hostToolEntry is one allowlist line. count is the number of string literals
// in the file that may name the tool; a new literal fails the ratchet, and so
// does a count the file no longer reaches.
type hostToolEntry struct {
	hostToolKey
	count  int
	reason string
	line   int
}

// TestNoNewHostToolFile is the ratchet: a non-test file that names a host tool
// in a string literal fails here, naming the file and the tool, unless the
// allowlist lists that file for that tool.
func TestNoNewHostToolFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFixture(t, root, "pkg/run.go", "package pkg\n\nimport \"os/exec\"\n\nfunc run() error {\n\treturn exec.Command(\"launchctl\", \"list\").Run()\n}\n")
	allowPath := writeFixture(t, root, "allow.txt", "# no entries\n")

	err := checkHostToolsAt(root, allowPath)
	if err == nil {
		t.Fatal("checkHostToolsAt = nil, want a refusal naming pkg/run.go and launchctl")
	}
	for _, want := range []string{"pkg/run.go:6", "launchctl"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("checkHostToolsAt error = %q, want it to contain %q", err, want)
		}
	}
}

// TestStaleHostToolEntryIsAnError keeps the list a record of what remains: an
// entry whose file no longer names its tool fails until its line is deleted, so
// each deletion ratchets the list down.
func TestStaleHostToolEntryIsAnError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFixture(t, root, "pkg/run.go", "package pkg\n\nfunc run() {}\n")
	allowPath := writeFixture(t, root, "allow.txt", "pkg/run.go\tlaunchctl\t1\tpending-deletion\tgt power: the launchd job the power states load\n")

	err := checkHostToolsAt(root, allowPath)
	if err == nil {
		t.Fatal("checkHostToolsAt = nil, want a stale-entry refusal")
	}
	if !strings.Contains(err.Error(), "delete the line") || !strings.Contains(err.Error(), "pkg/run.go launchctl") {
		t.Fatalf("checkHostToolsAt error = %q, want it to say to delete the line for pkg/run.go launchctl", err)
	}
}

// TestHostToolAllowlistRules refuses every line the format does not allow: a
// wildcard path, a blank reason, any class but pending-deletion, a name that is
// not a host tool, a count that is not a positive integer, a duplicate, and a
// line that is not five fields.
func TestHostToolAllowlistRules(t *testing.T) {
	t.Parallel()

	const reason = "gt power: the power states load the launchd job"
	cases := []struct {
		name  string
		line  string
		extra string
		want  string
	}{
		{
			name: "a wildcard path is refused",
			line: "pkg/*.go\tlaunchctl\t1\tpending-deletion\t" + reason,
			want: "wildcard",
		},
		{
			name: "a blank reason is refused",
			line: "pkg/run.go\tlaunchctl\t1\tpending-deletion\t",
			want: "has no reason",
		},
		{
			name: "a whitespace-only reason is refused",
			line: "pkg/run.go\tlaunchctl\t1\tpending-deletion\t   ",
			want: "has no reason",
		},
		{
			name: "a class other than pending-deletion is refused",
			line: "pkg/run.go\tlaunchctl\t1\treal\t" + reason,
			want: `class "real" is not pending-deletion`,
		},
		{
			name: "a name that is not a host tool is refused",
			line: "pkg/run.go\tlsof\t1\tpending-deletion\t" + reason,
			want: `"lsof" is not one of the four host tools`,
		},
		{
			name: "a zero count is refused",
			line: "pkg/run.go\tlaunchctl\t0\tpending-deletion\t" + reason,
			want: `count "0" for pkg/run.go launchctl is not a positive integer`,
		},
		{
			name: "a count that is not a number is refused",
			line: "pkg/run.go\tlaunchctl\tmany\tpending-deletion\t" + reason,
			want: `count "many" for pkg/run.go launchctl is not a positive integer`,
		},
		{
			name: "a blank count is refused",
			line: "pkg/run.go\tlaunchctl\t\tpending-deletion\t" + reason,
			want: `count "" for pkg/run.go launchctl is not a positive integer`,
		},
		{
			name:  "a duplicate use is refused",
			line:  "pkg/run.go\tlaunchctl\t1\tpending-deletion\t" + reason,
			extra: "pkg/run.go\tlaunchctl\t1\tpending-deletion\tsecond " + reason,
			want:  "duplicate entry for pkg/run.go launchctl",
		},
		{
			name: "a line without five tab-separated fields is refused",
			line: "pkg/run.go launchctl 1 pending-deletion " + reason,
			want: "want path<TAB>tool<TAB>count<TAB>class<TAB>reason",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeFixture(t, root, "pkg/run.go", "package pkg\n\nimport \"os/exec\"\n\nfunc run() error {\n\treturn exec.Command(\"launchctl\", \"list\").Run()\n}\n")
			content := tc.line + "\n"
			if tc.extra != "" {
				content += tc.extra + "\n"
			}
			allowPath := writeFixture(t, root, "allow.txt", content)

			err := checkHostToolsAt(root, allowPath)
			if err == nil {
				t.Fatalf("checkHostToolsAt = nil, want the %q refusal", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("checkHostToolsAt error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestHostToolCountIsEnforced holds a listed file to its line's count. A new
// literal that names the tool fails even though the file is listed, a count the
// file no longer reaches fails as stale, and an exact match passes.
func TestHostToolCountIsEnforced(t *testing.T) {
	t.Parallel()

	const reason = "gt power: the power states load the launchd job"
	const oneLiteral = "package pkg\n\nfunc load() string {\n\treturn \"launchctl bootstrap\"\n}\n"
	const twoLiterals = "package pkg\n\nfunc load() string {\n\treturn \"launchctl bootstrap\"\n}\n\nfunc unload() string {\n\treturn \"launchctl bootout\"\n}\n"
	cases := []struct {
		name   string
		source string
		count  string
		wants  []string
	}{
		{
			name:   "an extra literal in a listed file fails",
			source: twoLiterals,
			count:  "1",
			wants:  []string{"pkg/run.go:4 names launchctl in 2 literals", "allow.txt allows 1"},
		},
		{
			name:   "a count the file no longer reaches fails as stale",
			source: oneLiteral,
			count:  "2",
			wants:  []string{"allow.txt:1 pkg/run.go launchctl allows 2, the file names it in 1 literals; lower the count to 1"},
		},
		{
			name:   "an exact match passes",
			source: twoLiterals,
			count:  "2",
		},
		{
			name:   "a literal that names the tool twice counts once",
			source: "package pkg\n\nfunc run() string {\n\treturn \"launchctl list; launchctl print\"\n}\n",
			count:  "1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeFixture(t, root, "pkg/run.go", tc.source)
			allowPath := writeFixture(t, root, "allow.txt", "pkg/run.go\tlaunchctl\t"+tc.count+"\tpending-deletion\t"+reason+"\n")

			err := checkHostToolsAt(root, allowPath)
			if len(tc.wants) == 0 {
				if err != nil {
					t.Fatalf("checkHostToolsAt = %v, want no refusal", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkHostToolsAt = nil, want a refusal containing %q", tc.wants)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("checkHostToolsAt error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

// TestHostToolHitsNotCounted fixes what the scan does not count: a comment, a
// test file, a testdata or vendor tree, and a longer identifier that merely
// contains a tool name. None of them needs an allowlist line.
func TestHostToolHitsNotCounted(t *testing.T) {
	t.Parallel()

	const literal = "package pkg\n\nfunc run() string {\n\treturn \"launchctl\"\n}\n"
	cases := []struct {
		name   string
		path   string
		source string
	}{
		{
			name:   "a comment naming a tool is not a use",
			path:   "pkg/run.go",
			source: "package pkg\n\n// launchctl is the host agent's job, deleted with the host agent.\nfunc run() {}\n",
		},
		{
			name:   "a test file naming a tool is not counted",
			path:   "pkg/run_test.go",
			source: literal,
		},
		{
			name:   "a testdata directory is not scanned",
			path:   "pkg/testdata/fixture.go",
			source: literal,
		},
		{
			name:   "a vendor directory is not scanned",
			path:   "vendor/example.com/x/x.go",
			source: literal,
		},
		{
			name:   "a node_modules directory is not scanned",
			path:   "node_modules/x/x.go",
			source: literal,
		},
		{
			name:   "a .git directory is not scanned",
			path:   ".git/hooks/x.go",
			source: literal,
		},
		{
			name:   "an identifier that contains a tool name as a prefix is not a use",
			path:   "pkg/run.go",
			source: "package pkg\n\nfunc run() string {\n\treturn \"launchctlx\"\n}\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeFixture(t, root, tc.path, tc.source)
			allowPath := writeFixture(t, root, "allow.txt", "# no entries\n")

			if err := checkHostToolsAt(root, allowPath); err != nil {
				t.Fatalf("checkHostToolsAt = %v, want no refusal", err)
			}
		})
	}
}

// TestHostToolsScanRootIsNotSkippedByName keeps the skip list from passing a
// tree whose root directory has a skipped name: the skip applies below the root.
func TestHostToolsScanRootIsNotSkippedByName(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "vendor")
	writeFixture(t, root, "pkg/run.go", "package pkg\n\nimport \"os/exec\"\n\nfunc run() error {\n\treturn exec.Command(\"launchctl\").Run()\n}\n")
	allowPath := writeFixture(t, root, "allow.txt", "# no entries\n")

	err := checkHostToolsAt(root, allowPath)
	if err == nil || !strings.Contains(err.Error(), "pkg/run.go:6 names launchctl") {
		t.Fatalf("checkHostToolsAt = %v, want a refusal naming pkg/run.go:6 launchctl under a root named vendor", err)
	}
}

// TestHostToolsUnreadableAllowlistIsAnError keeps a missing, unreadable or
// unparsable allowlist from passing as an empty list: the error names the file.
func TestHostToolsUnreadableAllowlistIsAnError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		setup func(t *testing.T, root string) string
	}{
		{
			name: "a missing allowlist names the file",
			setup: func(t *testing.T, root string) string {
				return filepath.Join(root, "host-tools-allow.txt")
			},
		},
		{
			name: "an allowlist that is a directory names the file",
			setup: func(t *testing.T, root string) string {
				dir := filepath.Join(root, "host-tools-allow.txt")
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatalf("mkdir %s: %v", dir, err)
				}
				return dir
			},
		},
		{
			name: "an unparsable allowlist names the file",
			setup: func(t *testing.T, root string) string {
				return writeFixture(t, root, "host-tools-allow.txt", "pkg/run.go launchctl pending-deletion gt power\n")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeFixture(t, root, "pkg/run.go", "package pkg\n\nfunc run() {}\n")
			allowPath := tc.setup(t, root)

			err := checkHostToolsAt(root, allowPath)
			if err == nil {
				t.Fatal("checkHostToolsAt = nil, want an error naming the allowlist file")
			}
			if !strings.Contains(err.Error(), "host-tools-allow.txt") {
				t.Fatalf("checkHostToolsAt error = %q, want it to name host-tools-allow.txt", err)
			}
		})
	}
}

// TestHostToolsUnparsableSourceIsAnError keeps a Go file the parser rejects from
// being skipped: the scan fails and names the file.
func TestHostToolsUnparsableSourceIsAnError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFixture(t, root, "pkg/broken.go", "package pkg\n\nfunc run( {\n")
	allowPath := writeFixture(t, root, "allow.txt", "# no entries\n")

	err := checkHostToolsAt(root, allowPath)
	if err == nil {
		t.Fatal("checkHostToolsAt = nil, want an error naming pkg/broken.go")
	}
	if !strings.Contains(err.Error(), "pkg/broken.go") {
		t.Fatalf("checkHostToolsAt error = %q, want it to name pkg/broken.go", err)
	}
}

// TestRepoHostTools is the gate on the real tree: every host-tool string literal
// in the gt module is listed in the allowlist, and every listed use still exists.
func TestRepoHostTools(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	if err := checkHostToolsAt(root, filepath.Join(root, hostToolsAllowPath)); err != nil {
		t.Fatal(err)
	}
}

// checkHostToolsAt fails for a host-tool string literal in a non-test Go file
// under root that the allowlist does not cover, for a file that names a tool more
// often than its line allows, and for an allowlist line that names fewer than it
// allows or nothing. The allowlist is loaded first, so a broken file never reads
// as an empty exemption list.
func checkHostToolsAt(root, allowPath string) error {
	entries, err := loadHostToolAllowlist(allowPath)
	if err != nil {
		return err
	}
	hits, err := hostToolHits(root)
	if err != nil {
		return err
	}
	if err := unlistedHostToolHits(hits, entries, allowPath); err != nil {
		return err
	}
	return staleHostToolEntries(entries, hits, allowPath)
}

// hostToolHits scans every non-test Go file under root and returns one hit per
// file and tool, counting the literals that name it. It fails on a directory or
// parse error rather than reporting a clean scan.
func hostToolHits(root string) ([]hostToolHit, error) {
	var hits []hostToolHit
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && skippedScanDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		found, err := hostToolHitsInFile(path, filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		hits = append(hits, found...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning %s: %w", root, err)
	}
	return hits, nil
}

// hostToolHitsInFile parses one file and returns the host tools its string
// literals name, with the number of literals that name each. A literal that
// names a tool twice still counts once. Comments are not string literals, so
// they never count.
func hostToolHitsInFile(path, rel string) ([]hostToolHit, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", rel, err)
	}
	var hits []hostToolHit
	index := map[string]int{}
	var unquoteErr error
	ast.Inspect(file, func(n ast.Node) bool {
		if unquoteErr != nil {
			return false
		}
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			unquoteErr = fmt.Errorf("%s:%d: %w", rel, fset.Position(lit.Pos()).Line, err)
			return false
		}
		named := map[string]bool{}
		for _, m := range hostToolPattern.FindAllStringSubmatch(value, -1) {
			tool := m[1]
			if named[tool] {
				continue
			}
			named[tool] = true
			if i, ok := index[tool]; ok {
				hits[i].count++
				continue
			}
			index[tool] = len(hits)
			hits = append(hits, hostToolHit{
				hostToolKey: hostToolKey{file: rel, tool: tool},
				line:        fset.Position(lit.Pos()).Line,
				count:       1,
			})
		}
		return true
	})
	if unquoteErr != nil {
		return nil, unquoteErr
	}
	return hits, nil
}

// loadHostToolAllowlist parses the allowlist. Each line is
// path<TAB>tool<TAB>count<TAB>class<TAB>reason; blank lines and # comments are
// skipped.
func loadHostToolAllowlist(path string) ([]hostToolEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read host-tool allowlist %s: %w", path, err)
	}
	var entries []hostToolEntry
	seen := map[hostToolKey]int{}
	for i, raw := range strings.Split(string(data), "\n") {
		text := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(text) == "" || strings.HasPrefix(strings.TrimSpace(text), "#") {
			continue
		}
		where := fmt.Sprintf("%s:%d", path, i+1)
		fields := strings.Split(text, "\t")
		if len(fields) != 5 {
			return nil, fmt.Errorf("%s: want path<TAB>tool<TAB>count<TAB>class<TAB>reason, got %q", where, text)
		}
		file := strings.TrimSpace(fields[0])
		tool := strings.TrimSpace(fields[1])
		countText := strings.TrimSpace(fields[2])
		class := strings.TrimSpace(fields[3])
		reason := strings.TrimSpace(fields[4])
		if file == "" {
			return nil, fmt.Errorf("%s: entry has no path", where)
		}
		if strings.ContainsAny(file, "*?[") {
			return nil, fmt.Errorf("%s: wildcard path %q; each entry names one file", where, file)
		}
		if !isHostTool(tool) {
			return nil, fmt.Errorf("%s: %q is not one of the four host tools (%s)", where, tool, strings.Join(hostTools, ", "))
		}
		count, err := strconv.Atoi(countText)
		if err != nil || count < 1 {
			return nil, fmt.Errorf("%s: count %q for %s %s is not a positive integer; a file that names the tool in no literal has no line", where, countText, file, tool)
		}
		if class != hostToolsAllowClass {
			return nil, fmt.Errorf("%s: class %q is not pending-deletion; that is the only class", where, class)
		}
		if reason == "" {
			return nil, fmt.Errorf("%s: entry for %s %s has no reason", where, file, tool)
		}
		key := hostToolKey{file: file, tool: tool}
		if first, dup := seen[key]; dup {
			return nil, fmt.Errorf("%s: duplicate entry for %s %s (first at line %d)", where, file, tool, first)
		}
		seen[key] = i + 1
		entries = append(entries, hostToolEntry{hostToolKey: key, count: count, reason: reason, line: i + 1})
	}
	return entries, nil
}

// unlistedHostToolHits reports the uses the allowlist does not excuse: a file and
// tool with no line, and a file that names its tool in more literals than its
// line allows. Each message names the file, the tool and the allowlist in use.
func unlistedHostToolHits(hits []hostToolHit, entries []hostToolEntry, allowPath string) error {
	allowed := map[hostToolKey]int{}
	for _, entry := range entries {
		allowed[entry.hostToolKey] = entry.count
	}
	var b strings.Builder
	for _, hit := range hits {
		limit, listed := allowed[hit.hostToolKey]
		switch {
		case !listed:
			fmt.Fprintf(&b, "  %s:%d names %s\n", hit.file, hit.line, hit.tool)
		case hit.count > limit:
			fmt.Fprintf(&b, "  %s:%d names %s in %d literals; %s allows %d\n", hit.file, hit.line, hit.tool, hit.count, allowPath, limit)
		}
	}
	if b.Len() == 0 {
		return nil
	}
	return errors.New("non-test code names a host-only tool in a string literal and " + allowPath + " does not allow that many. The gt-in-container design deletes these tools, so do not add a use; remove it instead:\n" + b.String())
}

// staleHostToolEntries reports the allowlist entries that allow more than the
// tree holds: a file that no longer names its tool loses its line, and a count
// above the literals the file still has is lowered to match.
func staleHostToolEntries(entries []hostToolEntry, hits []hostToolHit, allowPath string) error {
	live := map[hostToolKey]int{}
	for _, hit := range hits {
		live[hit.hostToolKey] = hit.count
	}
	var b strings.Builder
	for _, entry := range entries {
		have := live[entry.hostToolKey]
		switch {
		case have == 0:
			fmt.Fprintf(&b, "  %s:%d %s %s names no literal; delete the line\n", allowPath, entry.line, entry.file, entry.tool)
		case have < entry.count:
			fmt.Fprintf(&b, "  %s:%d %s %s allows %d, the file names it in %d literals; lower the count to %d\n", allowPath, entry.line, entry.file, entry.tool, entry.count, have, have)
		}
	}
	if b.Len() == 0 {
		return nil
	}
	return errors.New("allowlisted uses are stale in " + allowPath + ": delete a line whose file no longer names its tool, and lower a count the file no longer reaches, so the list only shrinks:\n" + b.String())
}

// isHostTool reports whether name is one of the four host tools.
func isHostTool(name string) bool {
	for _, tool := range hostTools {
		if name == tool {
			return true
		}
	}
	return false
}

// writeFixture writes one file under root, creating its directory, and returns
// its path.
func writeFixture(t *testing.T, root, name, content string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// repoRoot returns the directory above this package that holds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
