package beads

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"
)

// libraryFreeFiles are production files that reached beads through the
// in-process library and now go through bd (gt-7iwy0.2). Paths are relative
// to this package. gt-7iwy0.3 deletes the library; until then this keeps a
// migrated site from growing a store call back.
var libraryFreeFiles = []string{
	"beads_agent.go",
	"../cmd/tracking_relations.go",
	"../cmd/daemon_dispatch.go",
}

func TestMigratedFilesImportNoBeadsLibrary(t *testing.T) {
	t.Parallel()
	for _, rel := range libraryFreeFiles {
		f, err := parser.ParseFile(token.NewFileSet(), filepath.FromSlash(rel), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if path == "github.com/steveyegge/beads" || filepath.Dir(path) == "github.com/steveyegge/beads" {
				t.Errorf("%s imports %s; it must reach beads through bd (gt-7iwy0.2)", rel, path)
			}
		}
	}
}
