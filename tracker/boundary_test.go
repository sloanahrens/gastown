package tracker

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	gtModule      = "github.com/steveyegge/gastown"
	trackerModule = gtModule + "/tracker"
)

// forbiddenImports returns the imports in src that reach gastown code outside
// the tracker module. The match is path-aware: gastown-other is not gastown.
func forbiddenImports(t *testing.T, filename, src string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filename, src, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	var bad []string
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			t.Fatalf("%s: unquote import %s: %v", filename, imp.Path.Value, err)
		}
		inGt := path == gtModule || strings.HasPrefix(path, gtModule+"/")
		inTracker := path == trackerModule || strings.HasPrefix(path, trackerModule+"/")
		if inGt && !inTracker {
			bad = append(bad, path)
		}
	}
	return bad
}

func TestForbiddenImports(t *testing.T) {
	t.Parallel() // unit tier, in-process, microseconds per case
	cases := []struct {
		name string
		src  string
		want []string
	}{
		{"standard library", `package p; import "net/http"`, nil},
		{"third party", `package p; import "github.com/jackc/pgx/v5"`, nil},
		{"own module root", `package p; import "github.com/steveyegge/gastown/tracker"`, nil},
		{"own module subpackage", `package p; import "github.com/steveyegge/gastown/tracker/api"`, nil},
		{"gt internal", `package p; import "github.com/steveyegge/gastown/internal/beads"`, []string{"github.com/steveyegge/gastown/internal/beads"}},
		{"gt root", `package p; import "github.com/steveyegge/gastown"`, []string{"github.com/steveyegge/gastown"}},
		{"gt cmd", `package p; import "github.com/steveyegge/gastown/cmd/gt"`, []string{"github.com/steveyegge/gastown/cmd/gt"}},
		{"look-alike module", `package p; import "github.com/steveyegge/gastown-other/x"`, nil},
		{"tracker look-alike inside gt", `package p; import "github.com/steveyegge/gastown/trackerx"`, []string{"github.com/steveyegge/gastown/trackerx"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := forbiddenImports(t, "x.go", c.src)
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("forbiddenImports = %v, want %v", got, c.want)
			}
		})
	}
}

// TestTrackerImportsNothingFromGt walks every Go file in the module, tests
// included, and fails on any import of gastown code outside tracker/. This is
// the boundary that keeps the module extractable (design spec, decision 3).
func TestTrackerImportsNothingFromGt(t *testing.T) {
	t.Parallel() // unit tier: parses imports only, reads the module's own files
	var violations []string
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if err != nil {
			t.Errorf("parse %s: %v", p, err)
			return nil
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if bad := forbiddenImports(t, p, "package p\nimport "+strconv.Quote(path)); len(bad) > 0 {
				violations = append(violations, p+" imports "+bad[0])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk tracker module: %v", err)
	}
	if len(violations) > 0 {
		t.Errorf("tracker/ must import nothing from gt internals; remove:\n  %s", strings.Join(violations, "\n  "))
	}
}
