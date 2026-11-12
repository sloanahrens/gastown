package forgejocut

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

// execGuardRoots are the directories whose production files may start a child
// process. A child started anywhere else would not be built by a scrubbed
// constructor, so it could be handed the admin token.
var execGuardRoots = []string{
	"cmd/forgejo-cutover",
	"internal/forgejocut",
	"internal/forgejoresync",
	"internal/forgejoprobe",
}

// scrubbedConstructors are the only functions in execGuardRoots that may start a
// child process (see startSeams). Each keeps the admin token variables out
// of the child's environment, and each has a test that turns red when that is
// taken out: execRunner.command's callers set the scrubbed environment before
// the child starts, gitCommandIn sets it, and execGit.Run sets it. Keys are
// file:function, and a method is keyed Type.method.
var scrubbedConstructors = map[string]bool{
	"cmd/forgejo-cutover/main.go:execRunner.command": true,
	"internal/forgejoresync/refs.go:gitCommandIn":    true,
	"internal/forgejoprobe/git.go:execGit.Run":       true,
}

// TestRunChildrenBuiltOnlyByScrubbedConstructors walks the cutover's production
// files and fails on any exec.Command or exec.CommandContext outside the scrubbed
// constructors, or on a listed constructor that no longer makes such a call. The
// walk must see the files it checks, or a walk that found nothing would pass.
func TestRunChildrenBuiltOnlyByScrubbedConstructors(t *testing.T) {
	t.Parallel()
	for _, problem := range execGuardProblems(os.DirFS(filepath.Join("..", "..")), execGuardRoots, scrubbedConstructors) {
		t.Error(problem)
	}
}

// TestRunExecGuardRefusals drives each branch of the guard over an in-memory tree,
// so a refusal is shown to be reported and a clean tree to pass, without reading the
// repository.
func TestRunExecGuardRefusals(t *testing.T) {
	t.Parallel()
	const (
		clean = "package run\n\nfunc other() int { return 1 }\n"
		// The constructor's name is the one the allowed map lists.
		runnerSrc = "package run\n\nimport \"os/exec\"\n\ntype runner struct{}\n\n" +
			"func (r runner) start() *exec.Cmd { return exec.CommandContext(nil, \"git\") }\n"
	)
	allowed := map[string]bool{"internal/run/run.go:runner.start": true}
	cases := []struct {
		name    string
		tree    fs.FS
		roots   []string
		allowed map[string]bool
		want    string
		wantNil bool
	}{
		{
			name: "an exec.Command in an unlisted function is reported",
			tree: fstest.MapFS{"internal/run/run.go": {Data: []byte(
				"package run\n\nimport \"os/exec\"\n\nfunc other() { _ = exec.Command(\"git\") }\n")}},
			want: "outside a scrubbed constructor (internal/run/run.go:other)",
		},
		{
			name: "an exec.CommandContext in an unlisted function is reported",
			tree: fstest.MapFS{"internal/run/run.go": {Data: []byte(
				"package run\n\nimport \"os/exec\"\n\nfunc other() { _ = exec.CommandContext(nil, \"git\") }\n")}},
			want: "outside a scrubbed constructor",
		},
		{
			name: "an exec call under an import alias is reported",
			tree: fstest.MapFS{"internal/run/run.go": {Data: []byte(
				"package run\n\nimport osexec \"os/exec\"\n\nfunc other() { _ = osexec.Command(\"git\") }\n")}},
			want: "outside a scrubbed constructor",
		},
		{
			name: "an exec call in a package-level variable is reported",
			tree: fstest.MapFS{"internal/run/run.go": {Data: []byte(
				"package run\n\nimport \"os/exec\"\n\nvar cmd = exec.Command(\"git\")\n")}},
			want: "outside a scrubbed constructor (internal/run/run.go)",
		},
		{
			name: "an exec.Command taken as a function value is reported",
			tree: fstest.MapFS{"internal/run/run.go": {Data: []byte(
				"package run\n\nimport \"os/exec\"\n\nfunc other() { run := exec.Command; _ = run }\n")}},
			want: "outside a scrubbed constructor (internal/run/run.go:other)",
		},
		{
			name: "an exec.Cmd literal is reported, since it starts a child with no Command call",
			tree: fstest.MapFS{"internal/run/run.go": {Data: []byte(
				"package run\n\nimport \"os/exec\"\n\nfunc other() { _ = &exec.Cmd{Path: \"/bin/git\"} }\n")}},
			want: "outside a scrubbed constructor (internal/run/run.go:other)",
		},
		{
			name: "new of exec.Cmd is reported",
			tree: fstest.MapFS{"internal/run/run.go": {Data: []byte(
				"package run\n\nimport \"os/exec\"\n\nfunc other() { _ = new(exec.Cmd) }\n")}},
			want: "outside a scrubbed constructor (internal/run/run.go:other)",
		},
		{
			name: "os.StartProcess is reported",
			tree: fstest.MapFS{"internal/run/run.go": {Data: []byte(
				"package run\n\nimport \"os\"\n\nfunc other() { _, _ = os.StartProcess(\"/bin/git\", nil, nil) }\n")}},
			want: "outside a scrubbed constructor (internal/run/run.go:other)",
		},
		{
			name: "a syscall.ForkExec under an import alias is reported",
			tree: fstest.MapFS{"internal/run/run.go": {Data: []byte(
				"package run\n\nimport sc \"syscall\"\n\nfunc other() { _, _ = sc.ForkExec(\"/bin/git\", nil, nil) }\n")}},
			want: "outside a scrubbed constructor (internal/run/run.go:other)",
		},
		{
			name: "a dot import of os/exec is reported at the import, since its names hide from the selector check",
			tree: fstest.MapFS{"internal/run/run.go": {Data: []byte(
				"package run\n\nimport . \"os/exec\"\n\nfunc other() { _ = Command(\"git\") }\n")}},
			want: "outside a scrubbed constructor (internal/run/run.go)",
		},
		{
			name: "the exec.Cmd type named in a signature and os.Args are not starts",
			tree: fstest.MapFS{"internal/run/run.go": {Data: []byte(
				"package run\n\nimport (\n\t\"os\"\n\t\"os/exec\"\n)\n\n" +
					"func other(cmd *exec.Cmd) []string { _ = os.Args; return cmd.Args }\n")}},
			wantNil: true,
		},
		{
			name:    "an exec call in a listed constructor passes",
			tree:    fstest.MapFS{"internal/run/run.go": {Data: []byte(runnerSrc)}},
			allowed: allowed,
			wantNil: true,
		},
		{
			name: "a listed method on a generic receiver passes, its type parameters dropped",
			tree: fstest.MapFS{"internal/run/run.go": {Data: []byte(
				"package run\n\nimport \"os/exec\"\n\ntype runner[T any] struct{}\n\n" +
					"func (r *runner[T]) start() *exec.Cmd { return exec.Command(\"git\") }\n")}},
			allowed: allowed,
			wantNil: true,
		},
		{
			name: "a method on a selector receiver is not matched by the plain function's allowed entry",
			tree: fstest.MapFS{"internal/run/run.go": {Data: []byte(
				"package run\n\nimport \"os/exec\"\n\n" +
					"func start() *exec.Cmd { return exec.Command(\"git\") }\n\n" +
					"func (r other.T) start() *exec.Cmd { return exec.Command(\"git\") }\n")}},
			allowed: map[string]bool{"internal/run/run.go:start": true},
			want:    "outside a scrubbed constructor (internal/run/run.go:?.start)",
		},
		{
			name: "a Command call on another package is not an exec call",
			tree: fstest.MapFS{"internal/run/run.go": {Data: []byte(
				"package run\n\nimport \"example.com/fakeexec\"\n\nfunc other() { _ = fakeexec.Command(\"git\") }\n")}},
			wantNil: true,
		},
		{
			name: "an exec call in a test file is not checked",
			tree: fstest.MapFS{
				"internal/run/run.go": {Data: []byte(clean)},
				"internal/run/run_test.go": {Data: []byte(
					"package run\n\nimport \"os/exec\"\n\nfunc helper() { _ = exec.Command(\"git\") }\n")},
			},
			wantNil: true,
		},
		{
			name:    "a listed constructor that makes no exec call is reported as stale",
			tree:    fstest.MapFS{"internal/run/run.go": {Data: []byte(clean)}},
			allowed: allowed,
			want:    "internal/run/run.go:runner.start is listed as a scrubbed constructor but makes no exec call",
		},
		{
			name: "a tree with no production files is refused, so the guard cannot pass on nothing",
			tree: fstest.MapFS{"internal/run/run_test.go": {Data: []byte(clean)}},
			want: "no production files",
		},
		{
			name:  "a root the walk cannot find is refused",
			tree:  fstest.MapFS{"internal/run/run.go": {Data: []byte(clean)}},
			roots: []string{"internal/missing"},
			want:  "walk the tree",
		},
		{
			name: "a production file that cannot be read is refused, not passed as clean",
			tree: execUnreadable{fstest.MapFS{"internal/run/run.go": {Data: []byte(clean)}}, "internal/run/run.go"},
			want: "walk the tree",
		},
		{
			name: "a production file that does not parse is refused",
			tree: fstest.MapFS{"internal/run/run.go": {Data: []byte("package run\n\nimport (\n")}},
			want: "walk the tree",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			roots := tc.roots
			if roots == nil {
				roots = []string{"internal/run"}
			}
			problems := execGuardProblems(tc.tree, roots, tc.allowed)
			if tc.wantNil {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none", problems)
				}
				return
			}
			if len(problems) != 1 || !strings.Contains(problems[0], tc.want) {
				t.Fatalf("problems = %v, want one naming %q", problems, tc.want)
			}
		})
	}
}

// execUnreadable is a tree whose Open fails for one path, so a file the guard
// cannot read is an error, not a file it can pass over.
type execUnreadable struct {
	tree fstest.MapFS
	path string
}

func (u execUnreadable) Open(name string) (fs.File, error) {
	if name == u.path {
		return nil, fs.ErrPermission
	}
	return u.tree.Open(name)
}

// execSite is one process start: the function that makes it (file:function, or the
// file alone outside any function) and where.
type execSite struct {
	key string
	pos string
}

// execGuardProblems returns what the guard finds under roots in fsys: a walk that
// failed, a walk that saw no production file, an exec call outside the allowed
// functions, or an allowed function that makes no exec call. It returns none for a
// tree that passes.
func execGuardProblems(fsys fs.FS, roots []string, allowed map[string]bool) []string {
	checked, sites, err := execCallSites(fsys, roots)
	switch {
	case err != nil:
		return []string{fmt.Sprintf("walk the tree: %v", err)}
	case checked == 0:
		return []string{"the walk found no production files to check"}
	}
	var problems []string
	made := map[string]bool{}
	for _, s := range sites {
		made[s.key] = true
		if !allowed[s.key] {
			problems = append(problems, fmt.Sprintf("%s starts a process outside a scrubbed constructor (%s)", s.pos, s.key))
		}
	}
	for _, key := range slices.Sorted(maps.Keys(allowed)) {
		if !made[key] {
			problems = append(problems, fmt.Sprintf("%s is listed as a scrubbed constructor but makes no exec call", key))
		}
	}
	return problems
}

// execCallSites parses the non-test Go files under each root and returns how many
// it parsed and every process start among them.
func execCallSites(fsys fs.FS, roots []string) (checked int, sites []execSite, err error) {
	fset := token.NewFileSet()
	for _, root := range roots {
		err = fs.WalkDir(fsys, root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := fs.ReadFile(fsys, path)
			if err != nil {
				return err
			}
			file, err := parser.ParseFile(fset, path, src, 0)
			if err != nil {
				return err
			}
			checked++
			sites = append(sites, execCallsIn(fset, path, file)...)
			return nil
		})
		if err != nil {
			return checked, sites, err
		}
	}
	return checked, sites, nil
}

// startSeams are the packages a child process is started through, and the
// selectors of each that start one. os/exec's Command and CommandContext build the
// child that the scrubbed constructors build; its Cmd type, written as a literal or
// with new, builds a child with no Command call; os.StartProcess and the syscall
// starts launch one with no exec.Cmd at all.
var startSeams = map[string][]string{
	"os/exec": {"Command", "CommandContext"},
	"os":      {"StartProcess"},
	"syscall": {"StartProcess", "ForkExec"},
}

// execCallsIn lists the process starts in one file, by the top-level declaration
// that makes each: a reference to a starting selector, call or not, so a function
// value is caught too; a literal or new of exec.Cmd; and a dot import of a seam,
// which hides its names from the selector check, so the import itself is the site.
// A file that imports none of the seams has none.
func execCallsIn(fset *token.FileSet, path string, file *ast.File) []execSite {
	names := startNames(file)
	var sites []execSite
	for _, imp := range file.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); imp.Name != nil && imp.Name.Name == "." {
			if _, seam := startSeams[p]; seam {
				sites = append(sites, execSite{key: path, pos: fset.Position(imp.Pos()).String()})
			}
		}
	}
	for _, decl := range file.Decls {
		key := path
		if fn, ok := decl.(*ast.FuncDecl); ok {
			key = path + ":" + funcKeyName(fn)
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			if startsProcess(n, names) {
				sites = append(sites, execSite{key: key, pos: fset.Position(n.Pos()).String()})
			}
			return true
		})
	}
	return sites
}

// startNames maps each seam file imports under a name to that name. A blank or dot
// import has none: a blank import names nothing, and a dot import is a site.
func startNames(file *ast.File) map[string]string {
	names := map[string]string{}
	for _, imp := range file.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		if _, seam := startSeams[p]; !seam {
			continue
		}
		switch {
		case imp.Name == nil:
			names[p] = p[strings.LastIndex(p, "/")+1:]
		case imp.Name.Name != "_" && imp.Name.Name != ".":
			names[p] = imp.Name.Name
		}
	}
	return names
}

// startsProcess reports whether one node starts a child process: a selector that a
// seam exports for that purpose, a composite literal of exec.Cmd, or new of it.
func startsProcess(n ast.Node, names map[string]string) bool {
	switch x := n.(type) {
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		if !ok {
			return false
		}
		for p, local := range names {
			if local == pkg.Name && slices.Contains(startSeams[p], x.Sel.Name) {
				return true
			}
		}
	case *ast.CompositeLit:
		return isExecCmd(x.Type, names)
	case *ast.CallExpr:
		id, ok := x.Fun.(*ast.Ident)
		return ok && id.Name == "new" && len(x.Args) == 1 && isExecCmd(x.Args[0], names)
	}
	return false
}

// isExecCmd reports whether expr names os/exec's Cmd type.
func isExecCmd(expr ast.Expr, names map[string]string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == names["os/exec"] && sel.Sel.Name == "Cmd"
}

// funcKeyName names a function for the allowed map: its name, or Type.name for a
// method, with a generic receiver's type parameters dropped. A receiver it cannot
// name is keyed "?.name", which no allowed entry carries, so its call is reported
// rather than passed.
func funcKeyName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	switch g := recv.(type) {
	case *ast.IndexExpr:
		recv = g.X
	case *ast.IndexListExpr:
		recv = g.X
	}
	if id, ok := recv.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return "?." + fn.Name.Name
}
