package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The characterization corpus for the three command guards. Each row records
// one command, the session context it is judged in, and the verdict the guard
// answers today. The corpus exists so that a change to a guard has to show
// itself against recorded behaviour: the row that moves is named, and the
// change either updates it on purpose or is the regression.
//
// It records behaviour, not intent. A row whose verdict is wrong is kept as it
// is (and noted on the bead that added it) until a bead changes the guard; the
// corpus is the before-picture that change is measured against.

const (
	guardCorpusPath = "testdata/guard_corpus.tsv"

	// corpusContainerSuite, corpusDangerous and corpusTestScope are the
	// guard column's vocabulary; each names one classifier.
	corpusContainerSuite = "container_suite"
	corpusDangerous      = "dangerous"
	corpusTestScope      = "test_scope"

	// corpusAllow and corpusRefuse are the verdict column's vocabulary.
	corpusAllow  = "allow"
	corpusRefuse = "refuse"

	// corpusPolecat, corpusRefinery, corpusCrew and corpusOther are the
	// context column's vocabulary — the session the guard runs in, built by
	// guardCorpusFixture.process.
	corpusPolecat  = "polecat"
	corpusRefinery = "refinery"
	corpusCrew     = "crew"
	corpusOther    = "other"
)

// guardCorpusColumns is the TSV header, in order.
var guardCorpusColumns = []string{"guard", "context", "command", "verdict", "reason_key", "bead", "stamp"}

// guardCorpusRow is one parsed row; line is the file line it came from, so a
// failure names the row to edit.
type guardCorpusRow struct {
	guard     string
	context   string
	command   string
	verdict   string
	reasonKey string
	bead      string
	stamp     string
	line      int
}

func (r guardCorpusRow) name() string {
	return fmt.Sprintf("%s/%s/line:%d", r.guard, r.context, r.line)
}

// loadGuardCorpus reads and validates the corpus file. The file is
// tab-separated with no field quoting: the command column is a Go-quoted
// string literal, so a tab or a newline inside a command arrives as its
// escape and never splits the record. A malformed line fails the test instead
// of being skipped — a skipped row is a verdict nobody checks, which is the
// one thing this corpus exists to prevent.
func loadGuardCorpus(t *testing.T) []guardCorpusRow {
	t.Helper()
	data, err := os.ReadFile(guardCorpusPath)
	if err != nil {
		t.Fatalf("read %s: %v", guardCorpusPath, err)
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || strings.TrimSuffix(lines[0], "\r") != strings.Join(guardCorpusColumns, "\t") {
		t.Fatalf("%s: first line must be the header %q", guardCorpusPath, strings.Join(guardCorpusColumns, "\t"))
	}
	var rows []guardCorpusRow
	for i, line := range lines[1:] {
		lineNo := i + 2 // 1-based, past the header
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		// The stamp column is empty until the freeze-rule bead fills it, and a
		// trailing empty field is invisible in an editor, so a row may end at
		// the bead column and the missing stamp reads as empty.
		if len(fields) == len(guardCorpusColumns)-1 {
			fields = append(fields, "")
		}
		if len(fields) != len(guardCorpusColumns) {
			t.Fatalf("%s:%d: %d fields, want %d", guardCorpusPath, lineNo, len(fields), len(guardCorpusColumns))
		}
		command, err := strconv.Unquote(fields[2])
		if err != nil {
			t.Fatalf("%s:%d: command column is not a Go-quoted string: %v", guardCorpusPath, lineNo, err)
		}
		rows = append(rows, guardCorpusRow{
			guard:     fields[0],
			context:   fields[1],
			command:   command,
			verdict:   fields[3],
			reasonKey: fields[4],
			bead:      fields[5],
			stamp:     fields[6],
			line:      lineNo,
		})
	}
	if len(rows) == 0 {
		t.Fatalf("%s: no rows", guardCorpusPath)
	}
	return rows
}

// guardCorpusFixture is the scratch session the rows are judged in: a Go
// module worktree (fakeModule, the helper the existing guard tests build
// their trees with), a home directory and a town root, all under t.TempDir().
// The town root also holds the rig directories the rows name.
// Nothing here reads the live host: the guards reach their answer through the
// same guardProcess injection their own tests use.
type guardCorpusFixture struct {
	module string
	home   string
	town   string
}

func newGuardCorpusFixture(t *testing.T) guardCorpusFixture {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	town := filepath.Join(t.TempDir(), "town")
	if err := os.MkdirAll(town, 0o755); err != nil {
		t.Fatalf("mkdir town: %v", err)
	}
	// The rig the dangerous rows name: a rig root with its polecats/
	// aggregate, one polecat worktree and the bare repo. Built so each row is
	// judged against a directory that exists, as it is in a real town.
	for _, rel := range []string{"gastown/polecats/topaz/gastown", "gastown/.repo.git"} {
		if err := os.MkdirAll(filepath.Join(town, rel), 0o755); err != nil {
			t.Fatalf("mkdir town/%s: %v", rel, err)
		}
	}
	return guardCorpusFixture{
		// The neutral "rig" layout carries no "polecats/" or "crew/" element,
		// so a guard that reads the cwd to decide the session (isPolecatContext)
		// is answered by the context's own role markers, not by the fixture.
		module: fakeModule(t, "rig"),
		home:   home,
		town:   town,
	}
}

// expand replaces the fixture placeholders a command may carry. The fixture
// paths contain no shell metacharacters (t.TempDir builds them from the test
// name), so a plain substitution keeps the command one shell word.
func (f guardCorpusFixture) expand(command string) string {
	return strings.NewReplacer("{town}", f.town, "{home}", f.home).Replace(command)
}

// process builds the session for a context. The role markers are the ones a
// spawn carries: GT_ROLE alone for crew, GT_ROLE and GT_POLECAT for a polecat,
// GT_REFINERY and GT_ROLE for the refinery, and nothing for "other" (a session
// outside the town). Every context runs from the same scratch Go module, so a
// row's verdict turns on its command and its role, not on where the fixture
// happened to sit.
func (f guardCorpusFixture) process(t *testing.T, context string) guardProcess {
	t.Helper()
	env := map[string]string{"HOME": f.home}
	switch context {
	case corpusPolecat:
		env["GT_ROLE"] = "gastown/polecats/topaz"
		env["GT_POLECAT"] = "topaz"
	case corpusRefinery:
		env["GT_REFINERY"] = "1"
		env["GT_ROLE"] = "gastown/refinery"
	case corpusCrew:
		env["GT_ROLE"] = "gastown/crew/sloan"
	case corpusOther:
		// no role markers
	default:
		t.Fatalf("unknown context %q", context)
	}
	return fakeGuardProcess(env, f.module)
}

// judge runs the row's guard as the production hook runs that classifier and
// returns the verdict and the reason it printed.
func (f guardCorpusFixture) judge(t *testing.T, row guardCorpusRow) (verdict, reason string) {
	t.Helper()
	proc := f.process(t, row.context)
	command := f.expand(row.command)
	// Each classifier returns its reason plus the evidence the block banner
	// prints; the corpus judges the verdict and the reason only.
	switch row.guard {
	case corpusContainerSuite:
		// The hook reads the container opt-in from the session's environment
		// (tapGuardContainerSuite passes proc.getenv(dockerTestsEnv)), so the
		// corpus does too.
		reason, _ = evaluateContainerSuiteCommand(proc, command, proc.getenv(dockerTestsEnv))
	case corpusDangerous:
		reason, _ = evaluateDangerousCommand(command, 0, guardSession{proc: proc, townRoot: f.town})
	case corpusTestScope:
		reason, _ = evaluatePolecatTestScope(proc, command)
	default:
		t.Fatalf("unknown guard %q", row.guard)
	}
	if reason == "" {
		return corpusAllow, ""
	}
	return corpusRefuse, reason
}

// TestGuardCorpusMatchesCurrentVerdicts runs every row through its guard. A
// row whose verdict or refusal reason has moved fails with the row named, so
// a guard change cannot pass silently: it has to reconcile the corpus.
func TestGuardCorpusMatchesCurrentVerdicts(t *testing.T) {
	t.Parallel()
	rows := loadGuardCorpus(t)
	fixture := newGuardCorpusFixture(t)
	for _, row := range rows {
		t.Run(row.name(), func(t *testing.T) {
			verdict, reason := fixture.judge(t, row)
			if verdict != row.verdict {
				t.Errorf("%s:%d [%s in %s] %s: verdict %s (reason %q), want %s",
					guardCorpusPath, row.line, row.guard, row.context, row.command, verdict, reason, row.verdict)
				return
			}
			if row.verdict == corpusRefuse && !strings.Contains(reason, row.reasonKey) {
				t.Errorf("%s:%d [%s in %s] %s: refusal reason %q does not contain reason_key %q",
					guardCorpusPath, row.line, row.guard, row.context, row.command, reason, row.reasonKey)
			}
		})
	}
}

// TestGuardCorpusNamedPathsExist checks that every {town} or {home} path a row
// names is a directory in the fixture. A row whose path the fixture never
// builds is judged against a path that does not exist, so its verdict says
// nothing about the directory the row names.
func TestGuardCorpusNamedPathsExist(t *testing.T) {
	t.Parallel()
	fixture := newGuardCorpusFixture(t)
	for _, row := range loadGuardCorpus(t) {
		for _, token := range strings.Fields(row.command) {
			if !strings.HasPrefix(token, "{town}") && !strings.HasPrefix(token, "{home}") {
				continue
			}
			path := fixture.expand(token)
			if st, err := os.Stat(path); err != nil || !st.IsDir() {
				t.Errorf("%s:%d: row names %s, which the fixture does not build as a directory", guardCorpusPath, row.line, path)
			}
		}
	}
}

// TestGuardCorpusShape checks the corpus itself: every guard has a broad
// enough sample in both directions, no row is repeated, and the columns say
// what the loader assumes. The floors keep a corpus that has quietly lost a
// guard (or every refusal row) from passing.
func TestGuardCorpusShape(t *testing.T) {
	t.Parallel()
	const (
		minRowsPerGuard = 40
		minAllow        = 10
		minRefuse       = 10
	)
	guards := map[string]bool{corpusContainerSuite: true, corpusDangerous: true, corpusTestScope: true}
	contexts := map[string]bool{corpusPolecat: true, corpusRefinery: true, corpusCrew: true, corpusOther: true}

	type tally struct{ allow, refuse int }
	counts := map[string]*tally{}
	for guard := range guards {
		counts[guard] = &tally{}
	}
	seen := map[string]int{}
	for _, row := range loadGuardCorpus(t) {
		if !guards[row.guard] {
			t.Errorf("%s:%d: unknown guard %q", guardCorpusPath, row.line, row.guard)
			continue
		}
		if !contexts[row.context] {
			t.Errorf("%s:%d: unknown context %q", guardCorpusPath, row.line, row.context)
			continue
		}
		if row.bead == "" {
			t.Errorf("%s:%d: empty bead column; a row names the bead that added it", guardCorpusPath, row.line)
		}
		switch row.verdict {
		case corpusAllow:
			counts[row.guard].allow++
			if row.reasonKey != "" {
				t.Errorf("%s:%d: allow row carries reason_key %q; only a refusal has a reason", guardCorpusPath, row.line, row.reasonKey)
			}
		case corpusRefuse:
			counts[row.guard].refuse++
			if row.reasonKey == "" {
				t.Errorf("%s:%d: refuse row has no reason_key", guardCorpusPath, row.line)
			}
		default:
			t.Errorf("%s:%d: unknown verdict %q", guardCorpusPath, row.line, row.verdict)
		}
		key := row.guard + "\x00" + row.context + "\x00" + row.command
		if prev, ok := seen[key]; ok {
			t.Errorf("%s:%d: duplicate of line %d (same guard, context and command)", guardCorpusPath, row.line, prev)
		}
		seen[key] = row.line
	}
	for guard, c := range counts {
		if total := c.allow + c.refuse; total < minRowsPerGuard {
			t.Errorf("%s has %d rows, want at least %d", guard, total, minRowsPerGuard)
		}
		if c.allow < minAllow {
			t.Errorf("%s has %d allow rows, want at least %d", guard, c.allow, minAllow)
		}
		if c.refuse < minRefuse {
			t.Errorf("%s has %d refuse rows, want at least %d", guard, c.refuse, minRefuse)
		}
	}
}
