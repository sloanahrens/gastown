package forgejocut

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The shapes the case table and the shell test it maps are read by. The script's
// assertions are a check call at the start of a line and the exit-status test at
// the head of a case; the table's rows are a line number in backticks and the Go
// test names the assertion maps to.
var (
	shellCheckLine = regexp.MustCompile(`^\s*check "`)
	shellExitLine  = regexp.MustCompile(`^if \[ "\$rc" `)
	tableRowLine   = regexp.MustCompile("^- `([0-9]+)`")
	tableTestName  = regexp.MustCompile(`Test[A-Z][A-Za-z0-9_]*`)
	goTestFunc     = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]*)\(`)
)

// TestCaseTableAccountsForEveryShellAssertion holds case-table.md to the shell
// test it replaces: the script executes the 157 assertions the table states, the
// table has one row for each, keyed by the line the assertion sits on, every row
// names at least one Go test, and every named test exists in this package or the
// command. A new shell assertion, a lost row, a renamed test or a stale row turns
// it red. The table and the script are deleted together (gt-tpg0p.52), so this
// check lives exactly as long as the mapping it verifies.
func TestCaseTableAccountsForEveryShellAssertion(t *testing.T) {
	t.Parallel()
	const scriptPath = "../../scripts/forgejo-cutover_test.sh"
	assertions := map[int]bool{}
	for i, line := range strings.Split(readRepoFile(t, scriptPath), "\n") {
		if shellCheckLine.MatchString(line) || shellExitLine.MatchString(line) {
			assertions[i+1] = true
		}
	}
	if len(assertions) != 157 {
		t.Fatalf("%s executes %d assertions, want the 157 the table states", scriptPath, len(assertions))
	}

	rows := map[int][]string{}
	for i, line := range strings.Split(readRepoFile(t, "case-table.md"), "\n") {
		match := tableRowLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		lineNumber, err := strconv.Atoi(match[1])
		if err != nil {
			t.Fatalf("case-table.md:%d names %q, which is not a line number", i+1, match[1])
		}
		rows[lineNumber] = append(rows[lineNumber], tableTestName.FindAllString(line, -1)...)
	}
	if len(rows) != len(assertions) {
		t.Errorf("case-table.md has %d rows, want one for each of the script's %d assertions", len(rows), len(assertions))
	}
	for lineNumber := range assertions {
		if _, ok := rows[lineNumber]; !ok {
			t.Errorf("the assertion at %s:%d has no case-table row", scriptPath, lineNumber)
		}
	}
	for lineNumber, names := range rows {
		if !assertions[lineNumber] {
			t.Errorf("case-table.md maps %s:%d, which executes no assertion", scriptPath, lineNumber)
		}
		if len(names) == 0 {
			t.Errorf("case-table.md maps %s:%d to no Go test", scriptPath, lineNumber)
		}
	}

	defined := definedGoTests(t, ".", "../../cmd/forgejo-cutover")
	for lineNumber, names := range rows {
		for _, name := range names {
			if !defined[name] {
				t.Errorf("case-table.md maps %s:%d to %s, which no test defines", scriptPath, lineNumber, name)
			}
		}
	}
}

// readRepoFile reads a file under the worktree, failing the test when it cannot
// be read: a mapping checked against a missing file would pass as empty.
func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// definedGoTests is the test functions the named package directories define.
func definedGoTests(t *testing.T, dirs ...string) map[string]bool {
	t.Helper()
	defined := map[string]bool{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", filepath.Join(dir, entry.Name()), err)
			}
			for _, match := range goTestFunc.FindAllStringSubmatch(string(data), -1) {
				defined[match[1]] = true
			}
		}
	}
	return defined
}
