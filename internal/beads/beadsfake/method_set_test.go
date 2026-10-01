package beadsfake

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// fakeControls are the Fake's exported methods that are not part of the
// beads surface: seeding, failure injection, and the recorders for the
// calls a fake can only script (SQLCSV, InitDatabase). Each is a test
// control; none may be a bd verb a consumer reaches past Client for.
var fakeControls = map[string]bool{
	"Seed":          true,
	"DropTable":     true,
	"FailWith":      true,
	"OnSQL":         true,
	"SQLStatements": true,
	"SQLCSV":        true,
	"InitDatabase":  true,
	"Inits":         true,
}

func methodNames(t reflect.Type) map[string]bool {
	names := map[string]bool{}
	for i := 0; i < t.NumMethod(); i++ {
		names[t.Method(i).Name] = true
	}
	return names
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestFakeMethodSetMatchesClient is the one-interface rule (gt-7iwy0.4): the
// shared fake implements exactly beads.Client and beads.Admin plus its
// listed test controls. A method on the fake that is on neither interface is
// a verb consumers would reach through the concrete fake; a method on the
// interfaces the fake lacks fails to compile already.
func TestFakeMethodSetMatchesClient(t *testing.T) {
	t.Parallel()
	surface := methodNames(reflect.TypeOf((*beads.Client)(nil)).Elem())
	for name := range methodNames(reflect.TypeOf((*beads.Admin)(nil)).Elem()) {
		surface[name] = true
	}
	fake := methodNames(reflect.TypeOf((*Fake)(nil)))

	var extra, missing []string
	for _, name := range sortedKeys(fake) {
		if !surface[name] && !fakeControls[name] {
			extra = append(extra, name)
		}
	}
	for _, name := range sortedKeys(surface) {
		if !fake[name] {
			missing = append(missing, name)
		}
	}
	for _, name := range sortedKeys(fakeControls) {
		if !fake[name] {
			missing = append(missing, name+" (listed control)")
		}
		if surface[name] {
			t.Errorf("%s is on the beads surface; drop it from fakeControls", name)
		}
	}
	if len(extra) > 0 {
		t.Errorf("Fake methods on neither beads.Client nor beads.Admin: %v\n"+
			"add each to the interface (with a contract case) or list it in fakeControls", extra)
	}
	if len(missing) > 0 {
		t.Errorf("methods the Fake lacks: %v", missing)
	}
}

// TestContractCoversEveryClientMethod pins the second half of the rule: a
// Client or Admin method lands with a contract case, so the fake and bd
// are held to the same behavior on it. A method counts as covered when the
// contract sources call it.
func TestContractCoversEveryClientMethod(t *testing.T) {
	t.Parallel()
	called := map[string]bool{}
	fset := token.NewFileSet()
	for _, file := range []string{"contract.go", "admin_contract.go"} {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					called[sel.Sel.Name] = true
				}
			}
			return true
		})
	}
	surface := methodNames(reflect.TypeOf((*beads.Client)(nil)).Elem())
	for name := range methodNames(reflect.TypeOf((*beads.Admin)(nil)).Elem()) {
		surface[name] = true
	}
	var uncovered []string
	for _, name := range sortedKeys(surface) {
		if !called[name] {
			uncovered = append(uncovered, name)
		}
	}
	if len(uncovered) > 0 {
		t.Errorf("Client/Admin methods no contract case calls: %v", uncovered)
	}
}
