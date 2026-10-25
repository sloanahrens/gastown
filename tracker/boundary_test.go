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

// forbiddenImports returns the import paths that reach gastown code outside
// the tracker module. The match is path-aware: gastown-other is not gastown.
func forbiddenImports(paths []string) []string {
	var bad []string
	for _, path := range paths {
		inGt := path == gtModule || strings.HasPrefix(path, gtModule+"/")
		inTracker := path == trackerModule || strings.HasPrefix(path, trackerModule+"/")
		if inGt && !inTracker {
			bad = append(bad, path)
		}
	}
	return bad
}

func TestForbiddenImports(t *testing.T) {
	t.Parallel() // unit tier, pure function, microseconds per case
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"standard library", "net/http", false},
		{"third party", "github.com/jackc/pgx/v5", false},
		{"own module root", "github.com/steveyegge/gastown/tracker", false},
		{"own module subpackage", "github.com/steveyegge/gastown/tracker/api", false},
		{"gt internal", "github.com/steveyegge/gastown/internal/beads", true},
		{"gt root", "github.com/steveyegge/gastown", true},
		{"gt cmd", "github.com/steveyegge/gastown/cmd/gt", true},
		{"look-alike module", "github.com/steveyegge/gastown-other/x", false},
		{"tracker look-alike inside gt", "github.com/steveyegge/gastown/trackerx", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := forbiddenImports([]string{c.path})
			if (len(got) == 1) != c.want {
				t.Errorf("forbiddenImports(%q) = %v, want forbidden=%v", c.path, got, c.want)
			}
		})
	}
}

// TestTrackerImportsNothingFromGt walks every Go file in the module, tests
// included, and fails on any import of gastown code outside tracker/. This is
// the boundary that keeps the module extractable.
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
		var paths []string
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Errorf("%s: unquote import %s: %v", p, imp.Path.Value, err)
				continue
			}
			paths = append(paths, path)
		}
		for _, bad := range forbiddenImports(paths) {
			violations = append(violations, p+" imports "+bad)
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
