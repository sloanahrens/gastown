package podengine

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/sweeprunner"
)

const (
	// repoRoot is the tree from this package's directory.
	repoRoot = "../.."
	// engineDoc is the document this guard holds against the tree.
	engineDoc = "docs/design/pod-engine.md"
	// backendKey prefixes the document's claim about the runner backend; the
	// value is compared with the names sweeprunner's backends report.
	backendKey = "sweep-runner-backend:"
	// notCallSiteHeading starts the document's list of files that name the
	// program without running it.
	notCallSiteHeading = "## Named, not a call site"
	// packageSymbol stands for a site outside any function.
	packageSymbol = "<package>"
	// minSites is the 2026-11 inventory's size less a margin: a scanner that
	// stopped seeing call sites would otherwise pass every check below.
	minSites = 25
)

// engineSite is one place a non-test Go file under internal/ names the docker
// CLI as the program of a command it runs.
type engineSite struct {
	// File is repository-relative and slash-separated.
	File string
	// Line is where the literal sits, for a failure message.
	Line int
	// Symbol is the enclosing function or method, or packageSymbol.
	Symbol string
	// Text is that source line, whitespace-collapsed.
	Text string
}

func (s engineSite) key() string { return s.File + "\x00" + s.Symbol }

// scanDockerSites walks the non-test Go files under root/internal and returns
// the docker call sites and the files that hold the program literal at all. A
// file that cannot be parsed is an error: a scan that skips it would read an
// unreadable call site as no call site (gt-cd5rqa).
func scanDockerSites(root string) ([]engineSite, []string, error) {
	var (
		sites []engineSite
		files []string
		fset  = token.NewFileSet()
	)
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return fs.SkipDir
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
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", filepath.ToSlash(rel), err)
		}
		found, holds := walkEngineSites(filepath.ToSlash(rel), file, fset, strings.Split(string(src), "\n"))
		sites = append(sites, found...)
		if holds {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	slices.SortFunc(sites, func(a, b engineSite) int {
		if a.File != b.File {
			return strings.Compare(a.File, b.File)
		}
		return a.Line - b.Line
	})
	slices.Sort(files)
	return sites, files, nil
}

// walkEngineSites returns the sites in one parsed file and whether the file
// holds the docker program literal anywhere, the table or not.
func walkEngineSites(file string, f *ast.File, fset *token.FileSet, lines []string) ([]engineSite, bool) {
	var (
		sites []engineSite
		holds bool
	)
	var visit func(n ast.Node, parent ast.Node, symbol string)
	visit = func(n ast.Node, parent ast.Node, symbol string) {
		switch x := n.(type) {
		case *ast.BasicLit:
			holds = holds || isDockerLiteral(x)
		case *ast.FuncDecl:
			symbol = funcSymbol(x)
		}
		if lit, ok := siteLiteral(n, parent); ok {
			line := fset.Position(lit.Pos()).Line
			if symbol == "" {
				symbol = packageSymbol
			}
			sites = append(sites, engineSite{File: file, Line: line, Symbol: symbol, Text: sourceLine(lines, line)})
		}
		eachChild(n, func(c ast.Node) { visit(c, n, symbol) })
	}
	visit(f, nil, "")
	return sites, holds
}

// siteLiteral reports the node that places the docker program literal where a
// command is run: an argument of a call with a subcommand after it (or the
// program argument of exec.Command), or the first element of an argv the file
// builds.
func siteLiteral(n ast.Node, parent ast.Node) (ast.Node, bool) {
	switch x := n.(type) {
	case *ast.CallExpr:
		for i, arg := range x.Args {
			if !isDockerLiteral(arg) {
				continue
			}
			if i < len(x.Args)-1 || isExecCommand(x.Fun) {
				return arg, true
			}
		}
	case *ast.CompositeLit:
		if !isStringSlice(x) || len(x.Elts) == 0 || !isDockerLiteral(x.Elts[0]) {
			return nil, false
		}
		switch parent.(type) {
		case *ast.ValueSpec, *ast.GenDecl:
			// A package-level list names programs a build or a step needs, not
			// an argv the file runs; the document names those files in prose.
			return nil, false
		case *ast.CompositeLit:
			// An unnamed field of a table is a requirement, as the daemon step
			// audit's Needs column is.
			return nil, false
		}
		return x.Elts[0], true
	}
	return nil, false
}

// eachChild calls fn for n's direct children.
func eachChild(n ast.Node, fn func(ast.Node)) {
	ast.Inspect(n, func(c ast.Node) bool {
		if c == nil {
			return false
		}
		if c == n {
			return true
		}
		fn(c)
		return false
	})
}

func isDockerLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING && lit.Value == `"docker"`
}

func isStringSlice(cl *ast.CompositeLit) bool {
	arr, ok := cl.Type.(*ast.ArrayType)
	if !ok || arr.Len != nil {
		return false
	}
	id, ok := arr.Elt.(*ast.Ident)
	return ok && id.Name == "string"
}

func isExecCommand(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "exec" {
		return false
	}
	return sel.Sel.Name == "Command" || sel.Sel.Name == "CommandContext"
}

// funcSymbol names the function, with its receiver's type for a method.
func funcSymbol(f *ast.FuncDecl) string {
	if f.Recv == nil || len(f.Recv.List) == 0 {
		return f.Name.Name
	}
	t := f.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + f.Name.Name
	}
	return f.Name.Name
}

func sourceLine(lines []string, n int) string {
	if n < 1 || n > len(lines) {
		return ""
	}
	return strings.Join(strings.Fields(lines[n-1]), " ")
}

// engineRow is one row of the document's inventory table.
type engineRow struct {
	Line        int
	Site        string
	Symbol      string
	Asks        string
	Disposition string
	Evidence    string
}

// engineDocContent is the document's machine-read part.
type engineDocContent struct {
	Rows []engineRow
	// NamedFiles are the files the not-a-call-site list names.
	NamedFiles []string
	// Backend is the runner backend the document claims.
	Backend string
}

// parseEngineDoc reads the table, the not-a-call-site list and the backend
// claim. A table row that is not five cells is an error: a row the parser
// half-reads would drop the site it classifies.
func parseEngineDoc(path string) (*engineDocContent, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	doc := &engineDocContent{}
	inTable, inNotCall := false, false
	for i, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			inTable = false
			inNotCall = strings.HasPrefix(trimmed, notCallSiteHeading)
			continue
		}
		if strings.HasPrefix(trimmed, backendKey) {
			doc.Backend = strings.TrimSpace(strings.TrimPrefix(trimmed, backendKey))
			continue
		}
		if inNotCall {
			if name, ok := bulletFile(trimmed); ok {
				doc.NamedFiles = append(doc.NamedFiles, name)
			}
			continue
		}
		if !strings.HasPrefix(trimmed, "|") {
			continue
		}
		cells := tableCells(trimmed)
		if cells[0] == "Site" {
			inTable = true
			continue
		}
		if strings.HasPrefix(cells[0], "---") || !inTable {
			continue
		}
		if len(cells) != 5 {
			return nil, fmt.Errorf("%s:%d: the row has %d cells, want 5 (Site, Symbol, Asks the engine for, Disposition, Evidence)", path, i+1, len(cells))
		}
		doc.Rows = append(doc.Rows, engineRow{
			Line:        i + 1,
			Site:        cells[0],
			Symbol:      cells[1],
			Asks:        cells[2],
			Disposition: cells[3],
			Evidence:    cells[4],
		})
	}
	return doc, nil
}

var bulletFileRE = regexp.MustCompile("^- `(internal/[^`]+)`")

func bulletFile(line string) (string, bool) {
	m := bulletFileRE.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return m[1], true
}

func tableCells(line string) []string {
	parts := strings.Split(strings.Trim(line, "|"), "|")
	cells := make([]string, 0, len(parts))
	for _, p := range parts {
		cells = append(cells, strings.Trim(strings.TrimSpace(p), "`"))
	}
	return cells
}

// compareInventory reports every call site the table does not classify and
// every row that names no site in the tree.
func compareInventory(sites []engineSite, rows []engineRow) []string {
	rowCount, siteCount := map[string]int{}, map[string]int{}
	for _, r := range rows {
		rowCount[r.Site+"\x00"+r.Symbol]++
	}
	var problems []string
	for _, s := range sites {
		siteCount[s.key()]++
		if siteCount[s.key()] > rowCount[s.key()] {
			problems = append(problems, fmt.Sprintf("%s:%d: %s runs the docker CLI and %s has no row for it: %s", s.File, s.Line, s.Symbol, engineDoc, s.Text))
		}
	}
	used := map[string]int{}
	for _, r := range rows {
		key := r.Site + "\x00" + r.Symbol
		used[key]++
		if used[key] > siteCount[key] {
			problems = append(problems, fmt.Sprintf("row %d (%s %s): names no docker CLI call site in the tree", r.Line, r.Site, r.Symbol))
		}
	}
	return problems
}

// compareNamedFiles reports a file that holds the docker program literal and
// that neither the table nor the not-a-call-site list names, and a listed file
// that no longer holds one.
func compareNamedFiles(files []string, rows []engineRow, named []string) []string {
	known := map[string]bool{}
	for _, n := range named {
		known[n] = true
	}
	for _, r := range rows {
		known[r.Site] = true
	}
	holds := map[string]bool{}
	var problems []string
	for _, f := range files {
		holds[f] = true
		if !known[f] {
			problems = append(problems, fmt.Sprintf("%s holds the docker program literal and %s names it neither in its table nor in its not-a-call-site list", f, engineDoc))
		}
	}
	for _, n := range named {
		if !holds[n] {
			problems = append(problems, fmt.Sprintf("the not-a-call-site list names %s, which holds no docker literal", n))
		}
	}
	return problems
}

var beadIDRE = regexp.MustCompile(`^[a-z][a-z0-9]*-[a-z0-9]{2,}(\.[a-z0-9]+)*$`)

// dispositionProblem is "" for one of the four dispositions and the reason the
// value is refused otherwise.
func dispositionProblem(raw string) string {
	fields := strings.Fields(raw)
	switch {
	case len(fields) == 0:
		return "has a blank disposition; want one of kube-api, mac-job, container-off, needs-engine <bead>"
	case fields[0] == "kube-api" || fields[0] == "mac-job":
		if len(fields) != 1 {
			return fmt.Sprintf("%s takes no argument", fields[0])
		}
		return ""
	case fields[0] == "container-off":
		if len(fields) != 2 {
			return "container-off wants the daemon.container_off name it is listed under"
		}
		if !config.DaemonStepKnown(fields[1]) {
			return fmt.Sprintf("container-off names %q, which the daemon step audit does not know", fields[1])
		}
		return ""
	case fields[0] == "needs-engine":
		if len(fields) != 2 {
			return "needs-engine wants the bead id that supplies the engine"
		}
		if !beadIDRE.MatchString(fields[1]) {
			return fmt.Sprintf("needs-engine names %q, which is not a bead id", fields[1])
		}
		return ""
	}
	return fmt.Sprintf("%q is not one of kube-api, mac-job, container-off, needs-engine <bead>", fields[0])
}

func checkDispositions(rows []engineRow) []string {
	var problems []string
	for _, r := range rows {
		if problem := dispositionProblem(r.Disposition); problem != "" {
			problems = append(problems, fmt.Sprintf("row %d (%s %s): %s", r.Line, r.Site, r.Symbol, problem))
		}
	}
	return problems
}

// TestEveryEngineSiteHasADisposition holds the tree and the document's table
// equal: every docker call site has a row, every row has a site, and every file
// that holds the program literal is named (gt-cd5rqa).
func TestEveryEngineSiteHasADisposition(t *testing.T) {
	t.Parallel()
	t.Run("the tree", func(t *testing.T) {
		t.Parallel()
		sites, files, err := scanDockerSites(repoRoot)
		if err != nil {
			t.Fatal(err)
		}
		if len(sites) < minSites {
			t.Fatalf("the scan found %d docker call sites (floor %d); the scanner has stopped seeing them", len(sites), minSites)
		}
		doc, err := parseEngineDoc(filepath.Join(repoRoot, engineDoc))
		if err != nil {
			t.Fatal(err)
		}
		problems := append(compareInventory(sites, doc.Rows), compareNamedFiles(files, doc.Rows, doc.NamedFiles)...)
		for _, p := range problems {
			t.Error(p)
		}
		for _, r := range doc.Rows {
			if r.Asks == "" || r.Evidence == "" {
				t.Errorf("row %d (%s %s): says nothing the call asks for, or cites no line; every row states both", r.Line, r.Site, r.Symbol)
			}
		}
		// The bead's inventory: these files carry the daemon's engine calls.
		scanned := map[string]bool{}
		for _, s := range sites {
			scanned[s.File] = true
		}
		for _, f := range []string{
			"internal/slot/gate.go",
			"internal/daemon/tier_sweep.go",
			"internal/doltserver/doltserver.go",
			"internal/daemon/attention_host.go",
			"internal/session/backend_openshell.go",
			"internal/diskmaint/registry/gc.go",
		} {
			if !scanned[f] {
				t.Errorf("%s: the scan found no docker call site, and the bead's inventory names it", f)
			}
		}
	})

	t.Run("a call site with no row fails naming file and line", func(t *testing.T) {
		t.Parallel()
		root := fixtureTree(t, map[string]string{
			"internal/x/x.go": "package x\n\nimport \"os/exec\"\n\nfunc Run() error {\n\treturn exec.Command(\"docker\", \"ps\").Run()\n}\n",
			engineDoc:         renderEngineDoc(),
		})
		problems := inventoryProblems(t, root)
		if len(problems) == 0 {
			t.Fatal("the comparison passed a tree whose only docker call site has no row")
		}
		if !strings.Contains(problems[0], "internal/x/x.go:6") {
			t.Fatalf("the failure does not name the file and line: %q", problems[0])
		}
	})

	t.Run("a row for a removed site fails naming the row", func(t *testing.T) {
		t.Parallel()
		root := fixtureTree(t, map[string]string{
			"internal/x/x.go": "package x\n\nfunc Run() error {\n\treturn nil\n}\n",
			engineDoc: renderEngineDoc(engineRow{
				Site: "internal/x/x.go", Symbol: "Run", Asks: "nothing", Disposition: "mac-job", Evidence: "exec.Command(\"docker\", \"ps\")",
			}),
		})
		problems := inventoryProblems(t, root)
		if len(problems) == 0 {
			t.Fatal("the comparison passed a row whose site is gone")
		}
		if !strings.Contains(problems[0], "row 5") || !strings.Contains(problems[0], "internal/x/x.go Run") {
			t.Fatalf("the failure does not name the row: %q", problems[0])
		}
	})

	t.Run("a file naming docker with no row fails naming the file", func(t *testing.T) {
		t.Parallel()
		root := fixtureTree(t, map[string]string{
			"internal/y/y.go": "package y\n\nconst Program = \"docker\"\n",
			engineDoc:         renderEngineDoc(),
		})
		problems := inventoryProblems(t, root)
		if len(problems) == 0 {
			t.Fatal("the comparison passed a file that holds the program literal and no mention")
		}
		if !strings.Contains(problems[0], "internal/y/y.go") {
			t.Fatalf("the failure does not name the file: %q", problems[0])
		}
	})

	t.Run("a file that does not parse fails the scan", func(t *testing.T) {
		t.Parallel()
		root := fixtureTree(t, map[string]string{
			"internal/x/x.go": "package x\n\nfunc (\n",
			engineDoc:         renderEngineDoc(),
		})
		if _, _, err := scanDockerSites(root); err == nil {
			t.Fatal("the scan read an unparseable file as a file with no call site")
		}
	})

	t.Run("a row that is not five cells fails naming the line", func(t *testing.T) {
		t.Parallel()
		root := fixtureTree(t, map[string]string{
			engineDoc: "# pod engine\n\n| Site | Symbol | Asks the engine for | Disposition | Evidence |\n| --- | --- | --- | --- | --- |\n| `internal/x/x.go` | `Run` | ps | `mac-job` |\n",
		})
		_, err := parseEngineDoc(filepath.Join(root, engineDoc))
		if err == nil {
			t.Fatal("the parser accepted a four-cell row and its Evidence cell would have been lost")
		}
		if !strings.Contains(err.Error(), ":5:") {
			t.Fatalf("the failure does not name the line: %v", err)
		}
	})

	t.Run("a test file is not scanned", func(t *testing.T) {
		t.Parallel()
		root := fixtureTree(t, map[string]string{
			"internal/x/x_test.go": "package x\n\nimport (\n\t\"os/exec\"\n\t\"testing\"\n)\n\nfunc TestRun(t *testing.T) {\n\t_ = exec.Command(\"docker\", \"ps\")\n}\n",
			engineDoc:              renderEngineDoc(),
		})
		if problems := inventoryProblems(t, root); len(problems) != 0 {
			t.Fatalf("a test file's docker call was classified: %v", problems)
		}
	})
}

// TestDispositionIsOneOfTheFour refuses a row that names no place where the
// pod answers the call (gt-cd5rqa).
func TestDispositionIsOneOfTheFour(t *testing.T) {
	t.Parallel()
	t.Run("the tree", func(t *testing.T) {
		t.Parallel()
		doc, err := parseEngineDoc(filepath.Join(repoRoot, engineDoc))
		if err != nil {
			t.Fatal(err)
		}
		if len(doc.Rows) == 0 {
			t.Fatalf("%s has no table rows", engineDoc)
		}
		for _, p := range checkDispositions(doc.Rows) {
			t.Error(p)
		}
	})
	for _, tc := range []struct {
		name        string
		disposition string
		want        string
	}{
		{name: "blank", disposition: "", want: "blank"},
		{name: "deferred", disposition: "deferred", want: `"deferred"`},
		{name: "needs-engine without a bead", disposition: "needs-engine", want: "bead id"},
		{name: "needs-engine with a word", disposition: "needs-engine later", want: "not a bead id"},
		{name: "container-off with an unknown step", disposition: "container-off mystery-step", want: "audit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := fixtureTree(t, map[string]string{
				engineDoc: renderEngineDoc(engineRow{Site: "internal/x/x.go", Symbol: "Run", Asks: "ps", Disposition: tc.disposition, Evidence: "exec.Command(\"docker\", \"ps\")"}),
			})
			doc, err := parseEngineDoc(filepath.Join(root, engineDoc))
			if err != nil {
				t.Fatal(err)
			}
			problems := checkDispositions(doc.Rows)
			if len(problems) != 1 {
				t.Fatalf("checkDispositions gave %d problems, want 1: %v", len(problems), problems)
			}
			if !strings.HasPrefix(problems[0], "row 5 (") {
				t.Fatalf("the failure does not name the row: %q", problems[0])
			}
			if !strings.Contains(problems[0], tc.want) {
				t.Fatalf("the failure does not say why: %q, want %q", problems[0], tc.want)
			}
		})
	}
}

// TestTierSweepRowsMatchTheRunnerBackend checks the document's claim about the
// sweep's backend against the names sweeprunner's backends report, not against
// the prose around them (gt-cd5rqa).
func TestTierSweepRowsMatchTheRunnerBackend(t *testing.T) {
	t.Parallel()
	doc, err := parseEngineDoc(filepath.Join(repoRoot, engineDoc))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{(sweeprunner.Docker{}).Name(), (sweeprunner.Kubernetes{}).Name()}
	if !slices.Contains(names, doc.Backend) {
		t.Fatalf("%s claims the runner backend %q, which is not one of sweeprunner's backend names %v", engineDoc, doc.Backend, names)
	}
	kubernetes := (sweeprunner.Kubernetes{}).Name()
	if doc.Backend != kubernetes {
		t.Fatalf("%s claims the runner backend %q, and the live setting runs the sweep as a Job on the %s backend", engineDoc, doc.Backend, kubernetes)
	}
	if got := config.TierSweepRunnerKubernetes; got != kubernetes {
		t.Fatalf("config.TierSweepRunnerKubernetes is %q and sweeprunner's Kubernetes backend reports %q", got, kubernetes)
	}
	rows := 0
	for _, r := range doc.Rows {
		if r.Site != "internal/daemon/tier_sweep.go" {
			continue
		}
		rows++
		if fields := strings.Fields(r.Disposition); len(fields) == 0 || fields[0] != "kube-api" {
			t.Errorf("row %d (%s %s): the %s backend runs the sweep as a Job, so the pod answers this site as kube-api; the row says %q", r.Line, r.Site, r.Symbol, doc.Backend, r.Disposition)
		}
	}
	if rows == 0 {
		t.Fatalf("%s has no row for internal/daemon/tier_sweep.go", engineDoc)
	}
}

// inventoryProblems runs the two comparisons over root's tree and document.
func inventoryProblems(t *testing.T, root string) []string {
	t.Helper()
	sites, files, err := scanDockerSites(root)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parseEngineDoc(filepath.Join(root, engineDoc))
	if err != nil {
		t.Fatal(err)
	}
	return append(compareInventory(sites, doc.Rows), compareNamedFiles(files, doc.Rows, doc.NamedFiles)...)
}

// fixtureTree writes files under a fresh directory and returns its root.
func fixtureTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// renderEngineDoc writes the table the parser reads, with the given rows.
func renderEngineDoc(rows ...engineRow) string {
	var b strings.Builder
	b.WriteString("# pod engine\n\n| Site | Symbol | Asks the engine for | Disposition | Evidence |\n| --- | --- | --- | --- | --- |\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| `%s` | `%s` | %s | `%s` | %s |\n", r.Site, r.Symbol, r.Asks, r.Disposition, r.Evidence)
	}
	return b.String()
}
