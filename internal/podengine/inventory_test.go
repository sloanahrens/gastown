package podengine

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
	// backendKey prefixes the document's claim about the runner backend.
	backendKey = "sweep-runner-backend:"
	// notCallSiteHeading starts the document's list of files that name the
	// program without running it.
	notCallSiteHeading = "## Named, not a call site"
	// needsEngineHeading starts the document's closing list of the rows the
	// pod cannot answer, each with its bead.
	needsEngineHeading = "## Needs an engine"
	// packageSymbol stands for a site outside any function.
	packageSymbol = "<package>"
	// unresolvedProgram names a command whose program the scan cannot read.
	unresolvedProgram = "unresolved program"
	// dockerProgram is the program name the guard looks for.
	dockerProgram = "docker"
	// noBeadYet is the one value the needs-engine column takes without a bead.
	noBeadYet = "no bead yet"
	// tierSweepFile is the file whose gate rows the registry check reads.
	tierSweepFile = "internal/daemon/tier_sweep.go"
	// runnerConfigFile holds the tier sweep runner config the document cites.
	runnerConfigFile = "internal/config/daemon_config.go"
	// daemonDir is the package whose calls rule out a mac-job row.
	daemonDir = "internal/daemon"
	// minSites is the inventory's size less a margin: a scanner that stopped
	// seeing call sites would otherwise pass every check below.
	minSites = 25
)

// citeRE is a file:line citation; the guard checks each one against the tree.
var citeRE = regexp.MustCompile(`([A-Za-z0-9_./-]+\.go):([0-9]+)`)

// engineSite is one place a non-test Go file under internal/ runs the docker
// CLI.
type engineSite struct {
	// File is repository-relative and slash-separated.
	File string
	// Line is where the program literal, or the name bound to it, sits.
	Line int
	// Symbol is the enclosing function or method, or packageSymbol.
	Symbol string
	// Text is that source line, whitespace-collapsed.
	Text string
	// Unresolved marks a command whose program is a name the file binds to a
	// value that is not a literal, so the scan cannot read it.
	Unresolved bool
}

func (s engineSite) key() string { return fmt.Sprintf("%s\x00%s\x00%d", s.File, s.Symbol, s.Line) }

// bindKind is what a const, var or := binds a name to.
type bindKind int

const (
	bindOpaque  bindKind = iota // a value the scan cannot read
	bindLiteral                 // a string literal; value is its text
	bindAlias                   // another name; value is that name
)

// binding is one name's value in a scope.
type binding struct {
	kind  bindKind
	value string
}

type bindings map[string]binding

// bindingOf reads what a right-hand side binds a name to.
func bindingOf(e ast.Expr) binding {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			if v, err := strconv.Unquote(x.Value); err == nil {
				return binding{kind: bindLiteral, value: v}
			}
		}
	case *ast.Ident:
		return binding{kind: bindAlias, value: x.Name}
	}
	return binding{kind: bindOpaque}
}

// addSpecs binds the names of a const or var declaration.
func addSpecs(scope bindings, g *ast.GenDecl) {
	if g.Tok != token.CONST && g.Tok != token.VAR {
		return
	}
	for _, s := range g.Specs {
		vs, ok := s.(*ast.ValueSpec)
		if !ok {
			continue
		}
		for i, name := range vs.Names {
			switch {
			case i < len(vs.Values):
				scope[name.Name] = bindingOf(vs.Values[i])
			case len(vs.Values) == 0 && vs.Type != nil:
				// A var with a type and no value is the zero value.
				scope[name.Name] = binding{kind: bindLiteral}
			default:
				scope[name.Name] = binding{kind: bindOpaque}
			}
		}
	}
}

// addLocals binds the names a function body declares or assigns. The scan is
// flow-insensitive: a name's last binding in the body is the one it reads.
func addLocals(scope bindings, body *ast.BlockStmt) {
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.DeclStmt:
			if g, ok := x.Decl.(*ast.GenDecl); ok {
				addSpecs(scope, g)
			}
		case *ast.AssignStmt:
			if x.Tok != token.DEFINE && x.Tok != token.ASSIGN {
				return true
			}
			for i, lhs := range x.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || id.Name == "_" {
					continue
				}
				if len(x.Lhs) == len(x.Rhs) {
					scope[id.Name] = bindingOf(x.Rhs[i])
				} else {
					scope[id.Name] = binding{kind: bindOpaque}
				}
			}
		}
		return true
	})
}

// programValue reads e as a program name: the literal's text, or the text a
// name in scope is bound to, through aliases. known is false when the value
// is not a literal the scan can read; bound is false when e is not a name the
// scope binds, so a call through it is not read as a docker call.
func programValue(e ast.Expr, scope bindings) (value string, known, bound bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false, false
		}
		v, err := strconv.Unquote(x.Value)
		if err != nil {
			return "", false, false
		}
		return v, true, true
	case *ast.Ident:
		name := x.Name
		for range 8 {
			b, ok := scope[name]
			if !ok {
				return "", false, false
			}
			switch b.kind {
			case bindLiteral:
				return b.value, true, true
			case bindOpaque:
				return "", false, true
			}
			name = b.value
		}
	}
	return "", false, false
}

// siteFinder collects one file's docker call sites.
type siteFinder struct {
	file  string
	fset  *token.FileSet
	lines []string
	seen  map[token.Pos]bool
	sites []engineSite
}

func (sf *siteFinder) add(pos token.Pos, symbol string, unresolved bool) {
	if sf.seen[pos] {
		return
	}
	sf.seen[pos] = true
	line := sf.fset.Position(pos).Line
	sf.sites = append(sf.sites, engineSite{
		File: sf.file, Line: line, Symbol: symbol, Text: sourceLine(sf.lines, line), Unresolved: unresolved,
	})
}

// scanBody records the sites under n, read against scope. An argv list is a
// site only inside a function: a package-level list names a program a build
// or a step needs, and the document names those files in prose.
func (sf *siteFinder) scanBody(n ast.Node, scope bindings, symbol string, inFunc bool) {
	ast.Inspect(n, func(c ast.Node) bool {
		switch x := c.(type) {
		case *ast.CallExpr:
			sf.scanCall(x, scope, symbol)
		case *ast.CompositeLit:
			if !inFunc || len(x.Elts) == 0 || !isArgvLiteral(x) {
				break
			}
			if v, known, _ := programValue(x.Elts[0], scope); known && v == dockerProgram {
				sf.add(x.Elts[0].Pos(), symbol, false)
			}
		}
		return true
	})
}

// scanCall records a command whose program is docker, and a call that passes
// the docker literal ahead of its subcommand (a wrapper such as
// `h.probe("docker", "inspect", ...)`).
func (sf *siteFinder) scanCall(x *ast.CallExpr, scope bindings, symbol string) {
	if idx := execProgramIndex(x.Fun); idx >= 0 && idx < len(x.Args) {
		arg := x.Args[idx]
		v, known, bound := programValue(arg, scope)
		switch {
		case known && v == dockerProgram:
			sf.add(arg.Pos(), symbol, false)
		case !known && bound:
			sf.add(arg.Pos(), symbol, true)
		}
	}
	for i, arg := range x.Args {
		if i < len(x.Args)-1 && isDockerLiteral(arg) {
			sf.add(arg.Pos(), symbol, false)
		}
	}
}

// walkEngineSites returns the sites in one parsed file and whether the file
// holds the docker program literal anywhere, the table or not.
func walkEngineSites(file string, f *ast.File, fset *token.FileSet, lines []string) ([]engineSite, bool) {
	sf := &siteFinder{file: file, fset: fset, lines: lines, seen: map[token.Pos]bool{}}
	pkg := bindings{}
	for _, d := range f.Decls {
		if g, ok := d.(*ast.GenDecl); ok {
			addSpecs(pkg, g)
		}
	}
	for _, d := range f.Decls {
		switch x := d.(type) {
		case *ast.FuncDecl:
			if x.Body == nil {
				continue
			}
			scope := maps.Clone(pkg)
			addLocals(scope, x.Body)
			sf.scanBody(x.Body, scope, funcSymbol(x), true)
		case *ast.GenDecl:
			sf.scanBody(x, pkg, packageSymbol, false)
		}
	}
	holds := false
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && isDockerLiteral(lit) {
			holds = true
		}
		return !holds
	})
	return sf.sites, holds
}

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

// isDockerLiteral reports whether e is the string literal "docker".
func isDockerLiteral(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING && lit.Value == `"docker"`
}

// isArgvLiteral reports whether cl is a string slice or array, or an element
// of one whose type the enclosing literal supplies.
func isArgvLiteral(cl *ast.CompositeLit) bool {
	if cl.Type == nil {
		return true
	}
	arr, ok := cl.Type.(*ast.ArrayType)
	if !ok {
		return false
	}
	id, ok := arr.Elt.(*ast.Ident)
	return ok && id.Name == "string"
}

// execProgramIndex is the position of the program argument in an os/exec
// constructor, or -1 for any other function.
func execProgramIndex(fun ast.Expr) int {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return -1
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "exec" {
		return -1
	}
	switch sel.Sel.Name {
	case "Command":
		return 0
	case "CommandContext":
		return 1
	}
	return -1
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

// normalize collapses whitespace, so a quoted source line matches however the
// file is indented.
func normalize(s string) string { return strings.Join(strings.Fields(s), " ") }

func sourceLine(lines []string, n int) string {
	if n < 1 || n > len(lines) {
		return ""
	}
	return normalize(lines[n-1])
}

// engineRow is one row of the document's inventory table.
type engineRow struct {
	// DocLine is the row's line in the document, for failure messages.
	DocLine int
	// File and Symbol name the function that runs the call.
	File   string
	Symbol string
	// Line is the source line of the call in File; 0 when the cell is blank.
	Line int
	// Source is the quoted text on that line.
	Source      string
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
	// NeedsEngine is the closing section's text, read against the needs-engine rows.
	NeedsEngine string
	// Text is the whole document.
	Text string
}

// parseEngineDoc reads the table, the not-a-call-site list, the backend claim
// and the closing section. A table row that is not seven cells is an error: a
// row the parser half-reads would drop the cell it classifies.
func parseEngineDoc(path string) (*engineDocContent, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	doc := &engineDocContent{Text: string(src)}
	section := ""
	var needs []string
	for i, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			section = trimmed
			continue
		}
		if section == needsEngineHeading {
			needs = append(needs, line)
		}
		if strings.HasPrefix(trimmed, backendKey) {
			doc.Backend = strings.TrimSpace(strings.TrimPrefix(trimmed, backendKey))
			continue
		}
		if section == notCallSiteHeading {
			if name, ok := bulletFile(trimmed); ok {
				doc.NamedFiles = append(doc.NamedFiles, name)
			}
			continue
		}
		if !strings.HasPrefix(trimmed, "|") || !strings.HasPrefix(section, "## The inventory") {
			continue
		}
		cells := tableCells(trimmed)
		if cells[0] == "File" || strings.HasPrefix(cells[0], "---") {
			continue
		}
		if len(cells) != 7 {
			return nil, fmt.Errorf("%s:%d: the row has %d cells, want 7 (File, Function, Line, Source, Asks the engine for, Disposition, Evidence)", path, i+1, len(cells))
		}
		line, _ := strconv.Atoi(cells[2])
		doc.Rows = append(doc.Rows, engineRow{
			DocLine: i + 1, File: cells[0], Symbol: cells[1], Line: line, Source: cells[3],
			Asks: cells[4], Disposition: cells[5], Evidence: cells[6],
		})
	}
	doc.NeedsEngine = strings.Join(needs, "\n")
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

// tableCells splits a markdown row on its unescaped pipes and strips the
// backticks around each cell.
func tableCells(line string) []string {
	const escaped = "\x00"
	body := strings.ReplaceAll(strings.Trim(line, "|"), `\|`, escaped)
	parts := strings.Split(body, "|")
	cells := make([]string, 0, len(parts))
	for _, p := range parts {
		cells = append(cells, strings.Trim(strings.TrimSpace(strings.ReplaceAll(p, escaped, "|")), "`"))
	}
	return cells
}

// handWrittenRows are rows for a call the scan cannot read, because the call
// runs a program it takes as a parameter (gt-cesfam). The row still cites the
// line that runs the program, which the citation check reads, and the closing
// section lists it. Keyed by file, function and line.
var handWrittenRows = map[string]bool{
	"internal/doltserver/host.go\x00host.probe\x00238": true,
}

// compareInventory reports every call site the table does not classify and
// every row that names no call at its line, except a hand-written row.
func compareInventory(sites []engineSite, rows []engineRow) []string {
	rowKey := map[string]bool{}
	for _, r := range rows {
		rowKey[fmt.Sprintf("%s\x00%s\x00%d", r.File, r.Symbol, r.Line)] = true
	}
	siteKey := map[string]bool{}
	var problems []string
	for _, s := range sites {
		siteKey[s.key()] = true
		if rowKey[s.key()] {
			continue
		}
		what := "runs the docker CLI"
		if s.Unresolved {
			what = "runs a command whose program is " + unresolvedProgram
		}
		problems = append(problems, fmt.Sprintf("%s:%d: %s %s and %s has no row for it: %s", s.File, s.Line, s.Symbol, what, engineDoc, s.Text))
	}
	for _, r := range rows {
		key := fmt.Sprintf("%s\x00%s\x00%d", r.File, r.Symbol, r.Line)
		if r.Line != 0 && !siteKey[key] && !handWrittenRows[key] {
			problems = append(problems, fmt.Sprintf("row %d (%s %s line %d): names no docker CLI call at that line", r.DocLine, r.File, r.Symbol, r.Line))
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
		known[r.File] = true
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

// lineOf returns the normalized text of line n of root/file, or an error when
// the file or the line is not there.
func lineOf(root, file string, n int) (string, error) {
	src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
	if err != nil {
		return "", fmt.Errorf("cites %s, which cannot be read: %w", file, err)
	}
	lines := strings.Split(string(src), "\n")
	if n < 1 || n > len(lines) {
		return "", fmt.Errorf("cites %s:%d, but the file has %d lines", file, n, len(lines))
	}
	return normalize(lines[n-1]), nil
}

// checkRowCitations holds each row's file, line and quoted text against the
// tree: a row whose line is blank, missing, or no longer holds the text fails
// naming the row.
func checkRowCitations(root string, rows []engineRow) []string {
	var problems []string
	for _, r := range rows {
		if r.Line == 0 {
			problems = append(problems, fmt.Sprintf("row %d (%s %s): has no line column value", r.DocLine, r.File, r.Symbol))
			continue
		}
		text, err := lineOf(root, r.File, r.Line)
		if err != nil {
			problems = append(problems, fmt.Sprintf("row %d (%s %s): %v", r.DocLine, r.File, r.Symbol, err))
			continue
		}
		if r.Source == "" || !strings.Contains(text, normalize(r.Source)) {
			problems = append(problems, fmt.Sprintf("row %d (%s %s): %s:%d does not hold the quoted text %q; it reads %q", r.DocLine, r.File, r.Symbol, r.File, r.Line, r.Source, text))
		}
	}
	return problems
}

// checkCitations returns every file:line cited in text and checks each one
// against the tree.
func checkCitations(root string, text string) (cites []string, problems []string) {
	for _, m := range citeRE.FindAllStringSubmatch(text, -1) {
		n, _ := strconv.Atoi(m[2])
		if _, err := lineOf(root, m[1], n); err != nil {
			problems = append(problems, err.Error())
		}
		cites = append(cites, m[1]+":"+m[2])
	}
	return cites, problems
}

var beadIDRE = regexp.MustCompile(`^[a-z][a-z0-9]*-[a-z0-9]{2,}(\.[a-z0-9]+)*$`)

// checkDispositions holds each row's disposition to the four-part grammar, its
// citations to the tree, and the mac-job rule: a call a daemon-package file
// makes can never be mac-job.
func checkDispositions(root string, rows []engineRow) []string {
	daemonCalls, problems := calledNames(filepath.Join(root, daemonDir))
	for _, r := range rows {
		where := fmt.Sprintf("row %d (%s %s)", r.DocLine, r.File, r.Symbol)
		fields := strings.Fields(r.Disposition)
		if len(fields) == 0 {
			problems = append(problems, where+": has a blank disposition; want kube-api, mac-job, container-off <step>, gated <file:line> when <condition>, degrades <file:line> <effect>, or needs-engine <bead>")
			continue
		}
		switch fields[0] {
		case "kube-api":
			if len(fields) != 1 {
				problems = append(problems, where+": kube-api takes no argument")
			}
			problems = append(problems, citationProblems(root, where, r.Evidence, "kube-api names the Job renderer line it rests on")...)
		case "mac-job":
			if len(fields) != 1 {
				problems = append(problems, where+": mac-job takes no argument")
			}
			if strings.HasPrefix(r.File, daemonDir+"/") {
				problems = append(problems, where+": a call in the daemon package can never be mac-job")
			}
			if name := calledName(r.Symbol); daemonCalls[name] {
				problems = append(problems, where+": internal/daemon calls "+name+", so the call is not made only by a command or Mac job")
			}
			problems = append(problems, citationProblems(root, where, r.Evidence, "mac-job names the entry point it is reached from")...)
		case "container-off":
			if len(fields) != 2 {
				problems = append(problems, where+": container-off wants the daemon.container_off name it is listed under")
			} else if !config.DaemonStepKnown(fields[1]) {
				problems = append(problems, fmt.Sprintf("%s: container-off names %q, which the daemon step audit does not know", where, fields[1]))
			}
		case "gated", "degrades":
			problems = append(problems, gateProblems(root, where, fields)...)
		case "needs-engine":
			switch {
			case strings.Join(fields[1:], " ") == noBeadYet:
			case len(fields) != 2:
				problems = append(problems, where+": needs-engine wants the bead id that supplies the engine, or \"no bead yet\"")
			case !beadIDRE.MatchString(fields[1]):
				problems = append(problems, fmt.Sprintf("%s: needs-engine names %q, which is not a bead id", where, fields[1]))
			}
		case "deferred", "unknown":
			problems = append(problems, fmt.Sprintf("%s: %q is not a disposition; the row classifies the call", where, fields[0]))
		default:
			problems = append(problems, fmt.Sprintf("%s: %q is not one of kube-api, mac-job, container-off, gated, degrades, needs-engine", where, fields[0]))
		}
	}
	return problems
}

// gateProblems checks a gated or degrades disposition: a file:line citation
// that exists, then the condition (gated) or the effect (degrades) in words.
// A gated condition is text the cited line must hold, so a gate that moved or
// changed fails here. A degrades effect is a static claim the guard does not
// read; the document says so.
func gateProblems(root, where string, fields []string) []string {
	if len(fields) < 3 {
		return []string{fmt.Sprintf("%s: %s wants a file:line citation and the %s in words", where, fields[0], map[string]string{"gated": "condition", "degrades": "effect"}[fields[0]])}
	}
	m := citeRE.FindStringSubmatch(fields[1])
	if m == nil || m[0] != fields[1] {
		return []string{fmt.Sprintf("%s: %s names %q, which is not a file:line citation", where, fields[0], fields[1])}
	}
	n, _ := strconv.Atoi(m[2])
	text, err := lineOf(root, m[1], n)
	if err != nil {
		return []string{where + ": " + err.Error()}
	}
	if fields[0] != "gated" {
		return nil
	}
	if fields[2] != "when" || len(fields) < 4 {
		return []string{where + `: gated wants "when" and the condition in words after the citation`}
	}
	condition := strings.Join(fields[3:], " ")
	if !strings.Contains(text, condition) {
		return []string{fmt.Sprintf("%s: %s reads %q, which does not hold the gate's condition %q", where, fields[1], text, condition)}
	}
	return nil
}

// citationProblems requires at least one file:line citation in evidence and
// checks each one against the tree.
func citationProblems(root, where, evidence, want string) []string {
	cites, problems := checkCitations(root, evidence)
	if len(cites) == 0 {
		problems = append(problems, fmt.Sprintf("the evidence cites no file:line, and %s", want))
	}
	for i := range problems {
		problems[i] = where + ": " + problems[i]
	}
	return problems
}

// calledName is the function name a row's symbol calls by: the method name for
// Type.Method, or the function name.
func calledName(symbol string) string {
	_, name, ok := strings.Cut(symbol, ".")
	if !ok {
		return symbol
	}
	return name
}

// calledNames returns every name the non-test Go files directly in dir call,
// as an identifier or as a selector's name. The check is by name, so it reads
// interface dispatch too; it does not follow a call through another package.
func calledNames(dir string) (map[string]bool, []string) {
	names := map[string]bool{}
	var problems []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return names, []string{fmt.Sprintf("the daemon package %s cannot be read, so the mac-job rule cannot be checked: %v", dir, err)}
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s cannot be parsed, so the calls it makes are not seen by the mac-job rule: %v", e.Name(), err))
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				names[fn.Name] = true
			case *ast.SelectorExpr:
				names[fn.Sel.Name] = true
			}
			return true
		})
	}
	return names, problems
}

// inventoryProblems runs the comparisons and the citation and disposition
// checks over root's tree and document.
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
	problems := compareInventory(sites, doc.Rows)
	problems = append(problems, compareNamedFiles(files, doc.Rows, doc.NamedFiles)...)
	problems = append(problems, checkRowCitations(root, doc.Rows)...)
	return append(problems, checkDispositions(root, doc.Rows)...)
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
	// The mac-job rule reads internal/daemon and refuses a tree without it, so a
	// fixture that names no file there still carries the package, with no calls.
	daemon := filepath.Join(root, filepath.FromSlash(daemonDir))
	if _, err := os.Stat(daemon); err != nil {
		if err := os.MkdirAll(daemon, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(daemon, "doc.go"), []byte("package daemon\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// renderEngineDoc writes the document the parser reads, with the given rows.
func renderEngineDoc(rows ...engineRow) string {
	var b strings.Builder
	b.WriteString("# pod engine\n\n## The inventory\n\n| File | Function | Line | Source | Asks the engine for | Disposition | Evidence |\n| --- | --- | --- | --- | --- | --- | --- |\n")
	for _, r := range rows {
		line := ""
		if r.Line != 0 {
			line = strconv.Itoa(r.Line)
		}
		fmt.Fprintf(&b, "| `%s` | `%s` | %s | `%s` | %s | %s | %s |\n", r.File, r.Symbol, line, r.Source, r.Asks, r.Disposition, r.Evidence)
	}
	return b.String()
}

// TestEveryEngineSiteHasARowPerFunction holds the tree and the table equal:
// every docker call site has a row in its own function, every row has a site,
// and every file that holds the program literal is named (gt-cd5rqa).
func TestEveryEngineSiteHasARowPerFunction(t *testing.T) {
	t.Parallel()
	t.Run("the tree", func(t *testing.T) {
		t.Parallel()
		sites, _, err := scanDockerSites(repoRoot)
		if err != nil {
			t.Fatal(err)
		}
		if len(sites) < minSites {
			t.Fatalf("the scan found %d docker call sites (floor %d); the scanner has stopped seeing them", len(sites), minSites)
		}
		for _, p := range inventoryProblems(t, repoRoot) {
			t.Error(p)
		}
		for _, f := range []string{
			"internal/slot/gate.go",
			"internal/daemon/tier_sweep.go",
			"internal/doltserver/doltserver.go",
			"internal/daemon/attention_host.go",
			"internal/session/backend_openshell.go",
			"internal/diskmaint/registry/gc.go",
			"internal/daemon/disk_maintenance_disks.go",
			"internal/daemon/dolt_version_check.go",
			"internal/templates/container_state.go",
		} {
			if !slices.ContainsFunc(sites, func(s engineSite) bool { return s.File == f }) {
				t.Errorf("%s: the scan found no docker call site, and the bead's inventory names it", f)
			}
		}
	})

	// Each shape the bead names fails with the file and the function when its
	// row is absent, and passes when the row is there.
	for _, tc := range []struct {
		name   string
		src    string
		symbol string
		line   int
		source string
	}{
		{
			name:   "a function-local var slice",
			src:    "package x\n\nimport \"os/exec\"\n\nfunc Run() error {\n\targv := []string{\"docker\", \"ps\"}\n\treturn exec.Command(argv[0], argv[1:]...).Run()\n}\n",
			symbol: "Run", line: 6, source: `argv := []string{"docker", "ps"}`,
		},
		{
			name:   "a nested composite literal",
			src:    "package x\n\nfunc Argvs() [][]string {\n\treturn [][]string{{\"docker\", \"ps\"}}\n}\n",
			symbol: "Argvs", line: 4, source: `return [][]string{{"docker", "ps"}}`,
		},
		{
			name:   "a program held in a const",
			src:    "package x\n\nimport \"os/exec\"\n\nconst program = \"docker\"\n\nfunc Run() error {\n\treturn exec.Command(program, \"ps\").Run()\n}\n",
			symbol: "Run", line: 8, source: `return exec.Command(program, "ps").Run()`,
		},
		{
			name:   "a program held in a variable",
			src:    "package x\n\nimport \"os/exec\"\n\nfunc Run() error {\n\tprog := \"docker\"\n\treturn exec.CommandContext(nil, prog, \"ps\").Run()\n}\n",
			symbol: "Run", line: 7, source: `return exec.CommandContext(nil, prog, "ps").Run()`,
		},
		{
			name:   "a return value",
			src:    "package x\n\nfunc Argv() []string {\n\treturn []string{\"docker\", \"info\"}\n}\n",
			symbol: "Argv", line: 4, source: `return []string{"docker", "info"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := fixtureTree(t, map[string]string{
				"internal/x/x.go": tc.src,
				engineDoc:         renderEngineDoc(),
			})
			problems := inventoryProblems(t, root)
			want := fmt.Sprintf("internal/x/x.go:%d", tc.line)
			if len(problems) == 0 {
				t.Fatalf("the comparison passed a function with a docker call site and no row")
			}
			if !strings.Contains(problems[0], want) || !strings.Contains(problems[0], tc.symbol) {
				t.Fatalf("the failure does not name the file, line and function %s: %q", want, problems[0])
			}
			root = fixtureTree(t, map[string]string{
				"internal/x/x.go": tc.src,
				engineDoc: renderEngineDoc(engineRow{
					File: "internal/x/x.go", Symbol: tc.symbol, Line: tc.line, Source: tc.source,
					Asks: "the call", Disposition: "mac-job", Evidence: "`internal/x/x.go:" + strconv.Itoa(tc.line) + "`",
				}),
			})
			if problems := inventoryProblems(t, root); len(problems) != 0 {
				t.Fatalf("a function with its row was refused: %v", problems)
			}
		})
	}

	t.Run("a second function in a file that already has a row fails the same way", func(t *testing.T) {
		t.Parallel()
		src := "package x\n\nimport \"os/exec\"\n\nfunc First() error {\n\treturn exec.Command(\"docker\", \"ps\").Run()\n}\n\nfunc Second() error {\n\treturn exec.Command(\"docker\", \"info\").Run()\n}\n"
		root := fixtureTree(t, map[string]string{
			"internal/x/x.go": src,
			engineDoc: renderEngineDoc(engineRow{
				File: "internal/x/x.go", Symbol: "First", Line: 6, Source: `return exec.Command("docker", "ps").Run()`,
				Asks: "ps", Disposition: "mac-job", Evidence: "`internal/x/x.go:6`",
			}),
		})
		problems := inventoryProblems(t, root)
		if len(problems) != 1 || !strings.Contains(problems[0], "internal/x/x.go:10") || !strings.Contains(problems[0], "Second") {
			t.Fatalf("the second function's site was not named: %v", problems)
		}
	})

	t.Run("an unresolved program fails naming the site", func(t *testing.T) {
		t.Parallel()
		root := fixtureTree(t, map[string]string{
			"internal/x/x.go": "package x\n\nimport \"os/exec\"\n\nfunc pick() string { return \"docker\" }\n\nfunc Run() error {\n\tp := pick()\n\treturn exec.Command(p, \"ps\").Run()\n}\n",
			engineDoc:         renderEngineDoc(),
		})
		problems := inventoryProblems(t, root)
		if len(problems) == 0 || !strings.Contains(problems[0], unresolvedProgram) || !strings.Contains(problems[0], "internal/x/x.go:9") {
			t.Fatalf("an unresolved program was not named: %v", problems)
		}
	})

	t.Run("a row for a removed site fails naming the row", func(t *testing.T) {
		t.Parallel()
		root := fixtureTree(t, map[string]string{
			"internal/x/x.go": "package x\n\nfunc Run() error {\n\treturn nil\n}\n",
			engineDoc: renderEngineDoc(engineRow{
				File: "internal/x/x.go", Symbol: "Run", Line: 4, Source: "return nil",
				Asks: "nothing", Disposition: "mac-job", Evidence: "`internal/x/x.go:4`",
			}),
		})
		problems := inventoryProblems(t, root)
		if len(problems) == 0 || !strings.Contains(problems[0], "row 7") || !strings.Contains(problems[0], "internal/x/x.go Run") {
			t.Fatalf("the failure does not name the row: %v", problems)
		}
	})

	t.Run("a file naming docker with no row fails naming the file", func(t *testing.T) {
		t.Parallel()
		root := fixtureTree(t, map[string]string{
			"internal/y/y.go": "package y\n\nconst Program = \"docker\"\n",
			engineDoc:         renderEngineDoc(),
		})
		problems := inventoryProblems(t, root)
		if len(problems) == 0 || !strings.Contains(problems[0], "internal/y/y.go") {
			t.Fatalf("the comparison passed a file that holds the program literal and no mention: %v", problems)
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

// TestEveryRowCitesALineThatHoldsItsText checks each row's file, line and
// quoted text against the tree, and each gate or effect citation the rows
// carry. A row whose file, line or text is wrong fails naming the row, and a
// row with no line value fails (gt-cd5rqa).
func TestEveryRowCitesALineThatHoldsItsText(t *testing.T) {
	t.Parallel()
	t.Run("the tree", func(t *testing.T) {
		t.Parallel()
		doc, err := parseEngineDoc(filepath.Join(repoRoot, engineDoc))
		if err != nil {
			t.Fatal(err)
		}
		if len(doc.Rows) == 0 {
			t.Fatalf("%s has no inventory rows", engineDoc)
		}
		for _, p := range checkRowCitations(repoRoot, doc.Rows) {
			t.Error(p)
		}
		for _, r := range doc.Rows {
			if r.Evidence == "" || r.Asks == "" {
				t.Errorf("row %d (%s %s): says nothing the call asks for, or gives no evidence", r.DocLine, r.File, r.Symbol)
			}
		}
	})

	for _, tc := range []struct {
		name string
		row  engineRow
		want string
	}{
		{
			name: "no line value",
			row:  engineRow{File: "internal/x/x.go", Symbol: "Run", Source: `exec.Command("docker", "ps")`, Asks: "ps", Disposition: "mac-job", Evidence: "`internal/x/x.go:3`"},
			want: "has no line column value",
		},
		{
			name: "a line that does not hold the quoted text",
			row:  engineRow{File: "internal/x/x.go", Symbol: "Run", Line: 3, Source: `exec.Command("git", "ps")`, Asks: "ps", Disposition: "mac-job", Evidence: "`internal/x/x.go:3`"},
			want: "does not hold the quoted text",
		},
		{
			name: "a line past the end of the file",
			row:  engineRow{File: "internal/x/x.go", Symbol: "Run", Line: 99, Source: `exec.Command("docker", "ps")`, Asks: "ps", Disposition: "mac-job", Evidence: "`internal/x/x.go:3`"},
			want: "the file has",
		},
		{
			name: "a file that is not in the tree",
			row:  engineRow{File: "internal/x/gone.go", Symbol: "Run", Line: 3, Source: `exec.Command("docker", "ps")`, Asks: "ps", Disposition: "mac-job", Evidence: "`internal/x/x.go:3`"},
			want: "cannot be read",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := fixtureTree(t, map[string]string{
				"internal/x/x.go": "package x\n\nfunc Run() {\n\texec.Command(\"docker\", \"ps\")\n}\n",
				engineDoc:         renderEngineDoc(tc.row),
			})
			doc, err := parseEngineDoc(filepath.Join(root, engineDoc))
			if err != nil {
				t.Fatal(err)
			}
			problems := checkRowCitations(root, doc.Rows)
			if len(problems) != 1 || !strings.Contains(problems[0], tc.want) || !strings.Contains(problems[0], "row 7") {
				t.Fatalf("want one problem naming row 7 with %q, got %v", tc.want, problems)
			}
		})
	}
}

// TestDispositionRules holds the disposition grammar: the six dispositions,
// their citations, the mac-job rule against the daemon package, and the
// needs-engine rows listed in the closing section (gt-cd5rqa).
func TestDispositionRules(t *testing.T) {
	t.Parallel()
	t.Run("the tree", func(t *testing.T) {
		t.Parallel()
		doc, err := parseEngineDoc(filepath.Join(repoRoot, engineDoc))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range checkDispositions(repoRoot, doc.Rows) {
			t.Error(p)
		}
		for _, r := range doc.Rows {
			if !strings.HasPrefix(r.Disposition, "needs-engine") {
				continue
			}
			bead := strings.TrimPrefix(r.Disposition, "needs-engine ")
			if !strings.Contains(doc.NeedsEngine, r.Symbol) || !strings.Contains(doc.NeedsEngine, bead) {
				t.Errorf("row %d (%s %s) needs an engine (%s), and the closing section %q does not list it", r.DocLine, r.File, r.Symbol, bead, needsEngineHeading)
			}
		}
	})

	for _, tc := range []struct {
		name        string
		disposition string
		evidence    string
		want        string
	}{
		{name: "blank", disposition: "", evidence: "`internal/x/x.go:3`", want: "blank"},
		{name: "deferred", disposition: "deferred", evidence: "`internal/x/x.go:3`", want: `"deferred"`},
		{name: "unknown", disposition: "unknown", evidence: "`internal/x/x.go:3`", want: `"unknown"`},
		{name: "needs-engine without a bead", disposition: "needs-engine", evidence: "`internal/x/x.go:3`", want: "bead id"},
		{name: "needs-engine with a word", disposition: "needs-engine later", evidence: "`internal/x/x.go:3`", want: "not a bead id"},
		{name: "container-off with an unknown step", disposition: "container-off mystery-step", evidence: "`internal/x/x.go:3`", want: "audit"},
		{name: "gated without a citation", disposition: "gated when the flag is set", evidence: "`internal/x/x.go:3`", want: "file:line citation"},
		{name: "gated without a condition", disposition: "gated internal/x/x.go:3", evidence: "`internal/x/x.go:3`", want: "condition in words"},
		{name: "gated citing a line past the end", disposition: "gated internal/x/x.go:99 when X", evidence: "`internal/x/x.go:3`", want: "the file has"},
		{name: "gated on a line that does not hold its condition", disposition: `gated internal/x/x.go:4 when prog == "git"`, evidence: "`internal/x/x.go:3`", want: "does not hold the gate's condition"},
		{name: "gated without the word when", disposition: `gated internal/x/x.go:4 prog == "docker"`, evidence: "`internal/x/x.go:3`", want: `wants "when"`},
		{name: "degrades without an effect", disposition: "degrades internal/x/x.go:3", evidence: "`internal/x/x.go:3`", want: "effect in words"},
		{name: "mac-job without an entry point", disposition: "mac-job", evidence: "no citation here", want: "cites no file:line"},
		{name: "mac-job with an argument", disposition: "mac-job later", evidence: "`internal/x/x.go:3`", want: "takes no argument"},
		{name: "mac-job for a function the daemon calls", disposition: "mac-job", evidence: "`internal/x/x.go:3`", want: "internal/daemon calls Run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := map[string]string{
				"internal/x/x.go": "package x\n\nfunc Run() {\n\texec.Command(\"docker\", \"ps\")\n}\n",
				engineDoc:         renderEngineDoc(engineRow{File: "internal/x/x.go", Symbol: "Run", Line: 4, Source: `exec.Command("docker", "ps")`, Asks: "ps", Disposition: tc.disposition, Evidence: tc.evidence}),
			}
			if tc.name == "mac-job for a function the daemon calls" {
				files["internal/daemon/use.go"] = "package daemon\n\nfunc use() {\n\tRun()\n}\n"
			}
			root := fixtureTree(t, files)
			doc, err := parseEngineDoc(filepath.Join(root, engineDoc))
			if err != nil {
				t.Fatal(err)
			}
			problems := checkDispositions(root, doc.Rows)
			if len(problems) != 1 {
				t.Fatalf("checkDispositions gave %d problems, want 1: %v", len(problems), problems)
			}
			if !strings.HasPrefix(problems[0], "row 7 (") {
				t.Fatalf("the failure does not name the row: %q", problems[0])
			}
			if !strings.Contains(problems[0], tc.want) {
				t.Fatalf("the failure does not say why: %q, want %q", problems[0], tc.want)
			}
		})
	}

	t.Run("a daemon-package call can never be mac-job", func(t *testing.T) {
		t.Parallel()
		root := fixtureTree(t, map[string]string{
			"internal/daemon/d.go": "package daemon\n\nfunc Run() {\n\texec.Command(\"docker\", \"ps\")\n}\n",
			engineDoc:              renderEngineDoc(engineRow{File: "internal/daemon/d.go", Symbol: "Run", Line: 4, Source: `exec.Command("docker", "ps")`, Asks: "ps", Disposition: "mac-job", Evidence: "`internal/daemon/d.go:4`"}),
		})
		doc, err := parseEngineDoc(filepath.Join(root, engineDoc))
		if err != nil {
			t.Fatal(err)
		}
		problems := checkDispositions(root, doc.Rows)
		if len(problems) != 1 || !strings.Contains(problems[0], "can never be mac-job") {
			t.Fatalf("a daemon-package row was accepted as mac-job: %v", problems)
		}
	})
}

// TestTierSweepGateIsTheRegistrySetting checks the document's tier sweep rows
// against the code: the key the document cites is a field of the runner config,
// the gate line the rows cite holds the condition they quote, and the runner
// backend the document claims is the one sweeprunner reports (gt-cd5rqa).
func TestTierSweepGateIsTheRegistrySetting(t *testing.T) {
	t.Parallel()
	doc, err := parseEngineDoc(filepath.Join(repoRoot, engineDoc))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("the cited key is a field of the runner config", func(t *testing.T) {
		t.Parallel()
		key := regexp.MustCompile(`daemon\.patrols\.tier_sweep\.runner\.([a-z_]+)`).FindStringSubmatch(doc.Text)
		if key == nil {
			t.Fatalf("%s cites no daemon.patrols.tier_sweep.runner key", engineDoc)
		}
		field, err := jsonFieldName(filepath.Join(repoRoot, runnerConfigFile), "TierSweepRunnerConfig", key[1])
		if err != nil {
			t.Fatal(err)
		}
		if field != "ImageRegistry" {
			t.Fatalf("%s cites %s, which the runner config names %s", engineDoc, key[1], field)
		}
	})

	t.Run("the gate line holds the condition the rows quote", func(t *testing.T) {
		t.Parallel()
		gated := 0
		for _, r := range doc.Rows {
			if r.File != tierSweepFile || !strings.HasPrefix(r.Disposition, "gated ") {
				continue
			}
			fields := strings.Fields(r.Disposition)
			_, condition, ok := strings.Cut(r.Disposition, " when ")
			if !ok {
				t.Errorf("row %d (%s %s): the gate row quotes no condition after \"when\"", r.DocLine, r.File, r.Symbol)
				continue
			}
			gated++
			cite := citeRE.FindStringSubmatch(fields[1])
			if cite == nil || cite[1] != tierSweepFile {
				t.Errorf("row %d (%s %s): the gate is cited to %q, not to %s", r.DocLine, r.File, r.Symbol, fields[1], tierSweepFile)
				continue
			}
			n, _ := strconv.Atoi(cite[2])
			text, err := lineOf(repoRoot, tierSweepFile, n)
			if err != nil {
				t.Errorf("row %d (%s %s): %v", r.DocLine, r.File, r.Symbol, err)
				continue
			}
			if !strings.Contains(text, normalize(condition)) {
				t.Errorf("row %d (%s %s): %s:%d reads %q, which does not hold the condition %q", r.DocLine, r.File, r.Symbol, tierSweepFile, n, text, condition)
			}
		}
		if gated == 0 {
			t.Fatalf("%s has no tier sweep row gated on the registry setting", engineDoc)
		}
	})

	t.Run("the claimed backend is the one sweeprunner reports", func(t *testing.T) {
		t.Parallel()
		names := []string{(sweeprunner.Docker{}).Name(), (sweeprunner.Kubernetes{}).Name()}
		if !slices.Contains(names, doc.Backend) {
			t.Fatalf("%s claims the runner backend %q, which is not one of sweeprunner's backend names %v", engineDoc, doc.Backend, names)
		}
		if got := config.TierSweepRunnerKubernetes; got != doc.Backend {
			t.Fatalf("%s claims the runner backend %q, and config.TierSweepRunnerKubernetes is %q", engineDoc, doc.Backend, got)
		}
	})
}

// jsonFieldName returns the Go name of the field of structName whose json tag
// is key, read from the file.
func jsonFieldName(path, structName, key string) (string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return "", err
	}
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, s := range g.Specs {
			ts, ok := s.(*ast.TypeSpec)
			if !ok || ts.Name.Name != structName {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			for _, field := range st.Fields.List {
				if field.Tag == nil || len(field.Names) == 0 {
					continue
				}
				tag, err := strconv.Unquote(field.Tag.Value)
				if err != nil {
					continue
				}
				if jsonTag(tag) == key {
					return field.Names[0].Name, nil
				}
			}
		}
	}
	return "", fmt.Errorf("%s has no field tagged json:%q in %s", path, key, structName)
}

// jsonTag returns the json tag's name, without its options.
func jsonTag(tag string) string {
	const prefix = `json:"`
	i := strings.Index(tag, prefix)
	if i < 0 {
		return ""
	}
	rest := tag[i+len(prefix):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	name, _, _ := strings.Cut(rest[:end], ",")
	return name
}

// The mac-job rule reads the calls internal/daemon makes. A daemon package the
// check cannot read, or a file in it that does not parse, is a problem of its
// own: the rule would otherwise pass for want of anything to compare against.
func TestDispositionsFailClosedWhenTheDaemonPackageCannotBeRead(t *testing.T) {
	t.Parallel()
	row := engineRow{File: "internal/x/x.go", Symbol: "Run", Line: 4, Source: `exec.Command("docker", "ps")`, Asks: "ps", Disposition: "kube-api", Evidence: "`internal/x/x.go:3`"}
	files := map[string]string{
		"internal/x/x.go": "package x\n\nfunc Run() {\n\texec.Command(\"docker\", \"ps\")\n}\n",
		engineDoc:         renderEngineDoc(row),
	}
	for _, tc := range []struct {
		name   string
		break_ func(t *testing.T, root string)
		want   string
	}{
		{name: "the daemon package is missing", break_: func(t *testing.T, root string) {
			if err := os.RemoveAll(filepath.Join(root, daemonDir)); err != nil {
				t.Fatal(err)
			}
		}, want: "cannot be read"},
		{name: "a daemon file does not parse", break_: func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, daemonDir, "broken.go"), []byte("package daemon\n\nfunc {\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, want: "cannot be parsed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := fixtureTree(t, files)
			tc.break_(t, root)
			doc, err := parseEngineDoc(filepath.Join(root, engineDoc))
			if err != nil {
				t.Fatal(err)
			}
			problems := checkDispositions(root, doc.Rows)
			if len(problems) != 1 || !strings.Contains(problems[0], tc.want) {
				t.Fatalf("checkDispositions = %v, want one problem containing %q", problems, tc.want)
			}
		})
	}
}
