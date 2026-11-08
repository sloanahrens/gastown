package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/parser"
	"go/printer"
	"go/token"
	"hash"
	"os"
	"path/filepath"
	"regexp"
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
		// The stamp column is empty on the seed rows, and a trailing empty field
		// is invisible in an editor, so a row may end at the bead column and the
		// missing stamp reads as empty.
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
		env["GT_ROLE"] = "gastown/crew/operator"
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

// guardCorpusNamedPath matches one fixture path a command names: a placeholder
// and the run of characters after it up to a separator or quote. It matches
// wherever the placeholder sits in a token (`--out={town}/x`, `"{home}/y"`).
var guardCorpusNamedPath = regexp.MustCompile(`\{(?:town|home)\}[^\s'"\x60;&|()<>]*`)

// missingNamedPaths returns each {town} or {home} path the command names that
// the fixture does not build as a directory. A path that cannot be stat'd is
// missing too, so an unreadable fixture fails the check instead of passing it.
func (f guardCorpusFixture) missingNamedPaths(command string) []string {
	var missing []string
	for _, ref := range guardCorpusNamedPath.FindAllString(command, -1) {
		path := f.expand(ref)
		if st, err := os.Stat(path); err != nil || !st.IsDir() {
			missing = append(missing, path)
		}
	}
	return missing
}

// TestGuardCorpusNamedPathsExist checks that every {town} or {home} path a row
// names is a directory in the fixture. A row whose path the fixture never
// builds is judged against a path that does not exist, so its verdict says
// nothing about the directory the row names.
func TestGuardCorpusNamedPathsExist(t *testing.T) {
	t.Parallel()
	fixture := newGuardCorpusFixture(t)
	for _, row := range loadGuardCorpus(t) {
		for _, path := range fixture.missingNamedPaths(row.command) {
			t.Errorf("%s:%d: row names %s, which the fixture does not build as a directory", guardCorpusPath, row.line, path)
		}
	}
}

// TestMissingNamedPathsReportsUnbuiltPaths drives the refusal branch of the
// check: a named path the fixture does not build is reported, wherever the
// placeholder sits in the command, and a built one is not.
func TestMissingNamedPathsReportsUnbuiltPaths(t *testing.T) {
	t.Parallel()
	fixture := newGuardCorpusFixture(t)
	tests := []struct {
		name    string
		command string
		want    []string
	}{
		{name: "no placeholder", command: "ls -la", want: nil},
		{name: "built rig at token start", command: "find {town}/gastown -name x", want: nil},
		{name: "built polecat at token start", command: "cd {town}/gastown/polecats/topaz/gastown", want: nil},
		{name: "built home mid-token", command: "--cache={home}", want: nil},
		{name: "built path quoted", command: `find "{town}/gastown/.repo.git" -name x`, want: nil},
		{name: "unbuilt rig at token start", command: "find {town}/nope -name x", want: []string{"/nope"}},
		{name: "unbuilt rig mid-token", command: "tar --out={town}/gastown/missing", want: []string{"/gastown/missing"}},
		{name: "unbuilt home quoted", command: `cp x "{home}/absent"`, want: []string{"/absent"}},
		{name: "unbuilt path followed by semicolon", command: "cd {home}/gone; ls", want: []string{"/gone"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fixture.missingNamedPaths(tt.command)
			if len(got) != len(tt.want) {
				t.Fatalf("missingNamedPaths(%q) = %v, want %d path(s) ending in %v", tt.command, got, len(tt.want), tt.want)
			}
			for i, suffix := range tt.want {
				if !strings.HasSuffix(got[i], suffix) {
					t.Errorf("missingNamedPaths(%q)[%d] = %q, want a path ending in %q", tt.command, i, got[i], suffix)
				}
			}
		})
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

// The freeze rule. The command guards share one stamp: the sha256 of the
// normalized non-test source of every guard file (tap_guard.go and
// tap_guard_*.go, found by a glob of the guard directory, so a new guard file
// is covered without a list to update) and of the parser every command guard
// reads commands through. A change to any of them moves the stamp, and the
// corpus must then carry a row stamped with the new one. The stamp comes from
// the source, not from git, so it holds offline and in a clean clone.

const (
	// guardFreezeDir holds the guard files the stamp covers.
	guardFreezeDir = "."

	// guardParserDir holds the parser whose non-test sources are part of the
	// stamp.
	guardParserDir = "../shellparse"
)

// guardFreezeGuards are the guards each of which must carry a row with the
// current stamp.
var guardFreezeGuards = []string{corpusContainerSuite, corpusDangerous, corpusTestScope}

// isGuardSource reports whether a file name is one of the guard files the stamp
// covers. Test files are excluded by the caller.
func isGuardSource(name string) bool {
	return name == "tap_guard.go" || strings.HasPrefix(name, "tap_guard_")
}

// freezeSources lists the non-test .go files in dir whose names match, sorted
// by name. A dir that cannot be read is an error.
func freezeSources(dir string, match func(name string) bool) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var paths []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || !match(name) {
			continue
		}
		paths = append(paths, filepath.Join(dir, name))
	}
	return paths, nil
}

// guardSourceHash is the stamp of the command guards: the sha256 of the
// normalized source of every guard file in guardDir, then of every non-test
// .go file in parserDir. A comment or a change of layout leaves it alone; a
// changed identifier or literal, a new guard file, or a changed parser moves
// it. A source that cannot be read or parsed is an error, never a stamp.
func guardSourceHash(guardDir, parserDir string) (string, error) {
	guards, err := freezeSources(guardDir, isGuardSource)
	if err != nil {
		return "", err
	}
	if len(guards) == 0 {
		return "", fmt.Errorf("guard dir %s holds no tap_guard source", guardDir)
	}
	parser, err := freezeSources(parserDir, func(string) bool { return true })
	if err != nil {
		return "", fmt.Errorf("parser: %w", err)
	}
	if len(parser) == 0 {
		return "", fmt.Errorf("parser dir %s holds no non-test .go source", parserDir)
	}
	h := sha256.New()
	for _, path := range append(guards, parser...) {
		if err := hashNormalizedGo(h, path); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashNormalizedGo writes the base name of path and its normalized source to h.
// The source is parsed without comments and printed through an empty FileSet,
// which resolves no source lines: the printer lays the tree out from its
// structure, so the layout of the source (and the blank line a removed comment
// leaves) does not reach the stamp. Printing through the parse's own FileSet
// would bring that layout back.
func hashNormalizedGo(h hash.Hash, path string) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), path, src, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	var out bytes.Buffer
	cfg := printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 8}
	if err := cfg.Fprint(&out, token.NewFileSet(), file); err != nil {
		return fmt.Errorf("print %s: %w", path, err)
	}
	fmt.Fprintf(h, "%s\x00", filepath.Base(path))
	h.Write(out.Bytes())
	return nil
}

// guardFreezeFailure returns the freeze-rule failure for guard: empty when a
// corpus row carries the stamp of the guard files in guardDir, otherwise a
// message naming the guard, the stamp to add and the file to add the row to.
// An unknown guard name or an unreadable source is an error.
func guardFreezeFailure(rows []guardCorpusRow, guard, guardDir, parserDir string) (string, error) {
	known := false
	for _, g := range guardFreezeGuards {
		known = known || g == guard
	}
	if !known {
		return "", fmt.Errorf("unknown guard %q", guard)
	}
	stamp, err := guardSourceHash(guardDir, parserDir)
	if err != nil {
		return "", fmt.Errorf("stamp %s: %w", guard, err)
	}
	for _, row := range rows {
		if row.guard == guard && row.stamp == stamp {
			return "", nil
		}
	}
	return fmt.Sprintf("guard %s: no corpus row carries its current stamp %s; a tap_guard file or the parser changed. "+
		"Add a row for the case you fixed to %s with stamp %s and the bead that made the change in the bead column",
		guard, stamp, guardCorpusPath, stamp), nil
}

// TestGuardChangeAddsCorpusRow holds the freeze rule on the tree: each command
// guard's current stamp is carried by at least one corpus row.
func TestGuardChangeAddsCorpusRow(t *testing.T) {
	t.Parallel()
	rows := loadGuardCorpus(t)
	for _, guard := range guardFreezeGuards {
		t.Run(guard, func(t *testing.T) {
			msg, err := guardFreezeFailure(rows, guard, guardFreezeDir, guardParserDir)
			if err != nil {
				t.Fatal(err)
			}
			if msg != "" {
				t.Error(msg)
			}
		})
	}
}

// writeFreezeDir writes each synthetic file into a fresh directory and returns
// it. The freeze tests read nothing from the tree.
func writeFreezeDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// TestGuardSourceHashIgnoresCommentsAndFormatting pins what the stamp covers.
// The sources are synthetic: the test reads nothing from the tree.
func TestGuardSourceHashIgnoresCommentsAndFormatting(t *testing.T) {
	t.Parallel()
	const base = `package cmd

func guardLabel(n int) string {
	if n > 1 {
		return "many"
	}
	return "one"
}
`
	parserDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(parserDir, "parse.go"), []byte("package shellparse\n\nfunc parse() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := func(src string) string {
		t.Helper()
		hash, err := guardSourceHash(writeFreezeDir(t, map[string]string{"tap_guard_fixture.go": src}), parserDir)
		if err != nil {
			t.Fatal(err)
		}
		return hash
	}
	want := stamp(base)
	tests := []struct {
		name string
		src  string
		same bool
	}{
		{name: "comment above the func", src: strings.Replace(base, "func guardLabel", "// guardLabel labels n.\nfunc guardLabel", 1), same: true},
		{name: "comment inside the body", src: strings.Replace(base, "\treturn \"one\"", "\t// the default label\n\treturn \"one\"", 1), same: true},
		{name: "comment between statements", src: strings.Replace(base, "\t}\n\treturn", "\t}\n\n\t// fall through\n\treturn", 1), same: true},
		{name: "extra blank line", src: strings.Replace(base, "\tif n > 1 {", "\n\n\tif n > 1 {", 1), same: true},
		{name: "one-line layout", src: "package cmd\n\nfunc guardLabel(n int) string{if n>1{return \"many\"};return \"one\"}\n", same: true},
		{name: "changed string literal", src: strings.Replace(base, `"many"`, `"several"`, 1), same: false},
		{name: "changed identifier", src: strings.Replace(strings.Replace(base, "n int", "count int", 1), "if n > 1", "if count > 1", 1), same: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stamp(tt.src) == want; got != tt.same {
				t.Errorf("stamp unchanged = %v, want %v", got, tt.same)
			}
		})
	}
}

// TestGuardSourceHashCoversParserSource checks that the parser's non-test
// sources are part of the stamp, and its test files are not.
func TestGuardSourceHashCoversParserSource(t *testing.T) {
	t.Parallel()
	const guardSrc = "package cmd\n\nfunc guardLabel() string { return \"one\" }\n"
	tests := []struct {
		name   string
		parser string
		same   bool
	}{
		{name: "comment in the parser", parser: "package shellparse\n\n// parse reads a line.\nfunc parse() {}\n", same: true},
		{name: "changed parser literal", parser: "package shellparse\n\nfunc parse() { _ = \"x\" }\n", same: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			guardDir := writeFreezeDir(t, map[string]string{"tap_guard_fixture.go": guardSrc})
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "parse.go"), []byte("package shellparse\n\nfunc parse() {}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			before, err := guardSourceHash(guardDir, dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "parse.go"), []byte(tt.parser), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "parse_test.go"), []byte("package shellparse\n\nfunc TestX() { _ = 1 }\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			after, err := guardSourceHash(guardDir, dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := before == after; got != tt.same {
				t.Errorf("stamp unchanged = %v, want %v", got, tt.same)
			}
		})
	}
}

// TestGuardChangeWithoutRowFails is the freeze rule's refusal: a literal
// changed in a guard's source, with the corpus unchanged, is reported as the
// guard, with the stamp to add. The source is synthetic, so the test does not
// depend on any literal in the production guards.
func TestGuardChangeWithoutRowFails(t *testing.T) {
	t.Parallel()
	const src = "package cmd\n\nfunc guardMessage() string { return \"Hard reset discards all uncommitted changes irreversibly\" }\n"
	const changed = "package cmd\n\nfunc guardMessage() string { return \"Hard reset discards every uncommitted change\" }\n"
	original, err := guardSourceHash(writeFreezeDir(t, map[string]string{"tap_guard_dangerous.go": src}), guardParserDir)
	if err != nil {
		t.Fatal(err)
	}
	guardDir := writeFreezeDir(t, map[string]string{"tap_guard_dangerous.go": changed})
	stamp, err := guardSourceHash(guardDir, guardParserDir)
	if err != nil {
		t.Fatal(err)
	}
	if stamp == original {
		t.Fatalf("changed literal left the stamp at %s", stamp)
	}
	msg, err := guardFreezeFailure(loadGuardCorpus(t), corpusDangerous, guardDir, guardParserDir)
	if err != nil {
		t.Fatal(err)
	}
	if msg == "" {
		t.Fatal("changed guard source passed the freeze rule with the corpus unchanged")
	}
	for _, want := range []string{corpusDangerous, stamp, guardCorpusPath} {
		if !strings.Contains(msg, want) {
			t.Errorf("freeze failure %q does not contain %q", msg, want)
		}
	}
}

// TestGuardSourceHashCoversEveryGuardFile checks that a new tap_guard file
// moves the stamp, so a guard added to the directory cannot change a verdict
// without a row, while a test file under the same prefix does not.
func TestGuardSourceHashCoversEveryGuardFile(t *testing.T) {
	t.Parallel()
	const guardSrc = "package cmd\n\nfunc guardLabel() string { return \"one\" }\n"
	const extra = "package cmd\n\nfunc guardExtra() string { return \"two\" }\n"
	parserDir := writeFreezeDir(t, map[string]string{"parse.go": "package shellparse\n\nfunc parse() {}\n"})
	base := map[string]string{"tap_guard_fixture.go": guardSrc}
	before, err := guardSourceHash(writeFreezeDir(t, base), parserDir)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		extra string
		same  bool
	}{
		{name: "extra guard file", extra: "tap_guard_extra.go", same: false},
		{name: "extra guard file at the base name", extra: "tap_guard.go", same: false},
		{name: "extra guard test file", extra: "tap_guard_extra_test.go", same: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := map[string]string{tt.extra: extra}
			for name, src := range base {
				files[name] = src
			}
			after, err := guardSourceHash(writeFreezeDir(t, files), parserDir)
			if err != nil {
				t.Fatal(err)
			}
			if got := before == after; got != tt.same {
				t.Errorf("stamp unchanged = %v, want %v", got, tt.same)
			}
		})
	}
}

// TestGuardSourceHashRefusesUnreadableSource checks the refusal branches: a
// source that cannot be read or parsed, or a parser with no source, is an
// error, never a stamp.
func TestGuardSourceHashRefusesUnreadableSource(t *testing.T) {
	t.Parallel()
	parserDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(parserDir, "parse.go"), []byte("package shellparse\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testsOnly := t.TempDir()
	if err := os.WriteFile(filepath.Join(testsOnly, "parse_test.go"), []byte("package shellparse\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	valid := writeFreezeDir(t, map[string]string{"tap_guard_fixture.go": "package cmd\n"})
	tests := []struct {
		name      string
		guardDir  string
		parserDir string
	}{
		{name: "missing guard dir", guardDir: filepath.Join(t.TempDir(), "absent"), parserDir: parserDir},
		{name: "unparsable guard file", guardDir: writeFreezeDir(t, map[string]string{"tap_guard_fixture.go": "package cmd\nfunc ("}), parserDir: parserDir},
		{name: "guard dir with no tap_guard file", guardDir: writeFreezeDir(t, map[string]string{"helper.go": "package cmd\n"}), parserDir: parserDir},
		{name: "missing parser dir", guardDir: valid, parserDir: filepath.Join(t.TempDir(), "absent")},
		{name: "parser dir with only test files", guardDir: valid, parserDir: testsOnly},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if hash, err := guardSourceHash(tt.guardDir, tt.parserDir); err == nil {
				t.Errorf("guardSourceHash = %q, want an error", hash)
			}
		})
	}
}

// TestGuardFreezeFailureRefusesUnreadableSource checks that an unreadable
// source is an error from the freeze check, not an empty failure that reads as
// a stamped guard.
func TestGuardFreezeFailureRefusesUnreadableSource(t *testing.T) {
	t.Parallel()
	msg, err := guardFreezeFailure(nil, corpusDangerous, filepath.Join(t.TempDir(), "absent"), guardParserDir)
	if err == nil {
		t.Errorf("guardFreezeFailure = %q, want an error", msg)
	}
}

// TestGuardFreezeFailureRefusesUnknownGuard checks that a guard name outside
// the freeze's guard list is an error, not a stamp that no row can carry.
func TestGuardFreezeFailureRefusesUnknownGuard(t *testing.T) {
	t.Parallel()
	msg, err := guardFreezeFailure(nil, "nope", guardFreezeDir, guardParserDir)
	if err == nil {
		t.Errorf("guardFreezeFailure(nope) = %q, want an error", msg)
	}
}
