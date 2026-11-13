package land

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// importFixtureModule is the module the fixture graph names; its Dir is the
// root the selection is run from.
const importFixtureModule = "example.com/m"

// importFixtureRoot is the module directory the fixture graph reports.
const importFixtureRoot = "/fixture"

// importRecord is the part of one `go list -json` record the selection reads.
type importRecord struct {
	ImportPath string
	Dir        string
	Imports    []string
	Module     *importModule
}

type importModule struct {
	Path string
	Dir  string
	Main bool
}

// importGraphList renders the `go list -deps -test -json ./...` stream of a
// module whose packages import the directories imports names. Each record
// carries its package's direct imports, as go list reports them.
func importGraphList(t *testing.T, imports map[string][]string) []byte {
	t.Helper()
	var out []byte
	for dir, imps := range imports {
		rec := importRecord{
			ImportPath: importFixtureModule + "/" + dir,
			Dir:        importFixtureRoot + "/" + dir,
			Module:     &importModule{Path: importFixtureModule, Dir: importFixtureRoot, Main: true},
		}
		for _, imp := range imps {
			rec.Imports = append(rec.Imports, importFixtureModule+"/"+imp)
		}
		b, err := json.Marshal(rec)
		if err != nil {
			t.Fatalf("marshal fixture record: %v", err)
		}
		out = append(out, b...)
		out = append(out, '\n')
	}
	return out
}

// importRunner answers the selection's commands from canned output: a clean
// tree at head, no untracked Go files, and the graph or its error for go list.
// It starts no process.
type importRunner struct {
	list    []byte
	listErr error
}

func (r importRunner) run(argv []string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty argv")
	}
	switch argv[0] {
	case "go":
		return r.list, r.listErr
	case "git":
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected command %q", argv)
}

// importFixture is a small module: rig is imported by townstate and by the
// heavy internal/cmd; townview reaches rig only through townstate; tools
// imports nothing in the module.
var importFixture = map[string][]string{
	"internal/rig":       nil,
	"internal/townstate": {"internal/rig"},
	"internal/townview":  {"internal/townstate"},
	"internal/cmd":       {"internal/rig"},
	"internal/polecat":   {"internal/townstate"},
	"internal/tools":     nil,
}

func TestSelectImportersAddsDirectImportersAndLeavesHeavyOnesOut(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		changed  []string
		wantAdd  []string
		wantLeft []string
	}{
		{
			name:     "a leaf's direct importers, heavy one left out",
			changed:  []string{"internal/rig"},
			wantAdd:  []string{"internal/townstate"},
			wantLeft: []string{"internal/cmd"},
		},
		{
			name:     "the direct importers of a middle package, not of its importers",
			changed:  []string{"internal/townstate"},
			wantAdd:  []string{"internal/townview"},
			wantLeft: []string{"internal/polecat"},
		},
		{
			name:    "a package nothing imports adds nothing",
			changed: []string{"internal/tools"},
		},
		{
			name:     "changed packages are not added as importers of each other",
			changed:  []string{"internal/rig", "internal/townstate"},
			wantAdd:  []string{"internal/townview"},
			wantLeft: []string{"internal/cmd", "internal/polecat"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := importRunner{list: importGraphList(t, importFixture)}
			sel, err := SelectImporters(r.run, importFixtureRoot, "HEAD", tc.changed)
			if err != nil {
				t.Fatalf("SelectImporters: %v", err)
			}
			if !slices.Equal(sel.Add, tc.wantAdd) {
				t.Errorf("Add = %v, want %v", sel.Add, tc.wantAdd)
			}
			var left []string
			for _, l := range sel.LeftOut {
				left = append(left, l.Dir)
				if l.Reason == "" {
					t.Errorf("LeftOut %s has no reason", l.Dir)
				}
			}
			if !slices.Equal(left, tc.wantLeft) {
				t.Errorf("LeftOut = %v, want %v", left, tc.wantLeft)
			}
		})
	}
}

// TestSelectImportersFailsWithTheCause: a graph that cannot be read or does not
// hold a changed package fails the selection, and names the cause, so presubmit
// never passes on the changed packages alone.
func TestSelectImportersFailsWithTheCause(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		changed []string
		r       importRunner
		cause   string
	}{
		{
			name:    "go list fails",
			changed: []string{"internal/rig"},
			r:       importRunner{listErr: errors.New("exit status 1")},
			cause:   "go list",
		},
		{
			name:    "go list reports a package error",
			changed: []string{"internal/rig"},
			r: importRunner{list: []byte(`{"ImportPath": "example.com/m/internal/rig", "Dir": "/fixture/internal/rig",` +
				` "Module": {"Path": "example.com/m", "Dir": "/fixture", "Main": true},` +
				` "Error": {"Err": "build constraints exclude all Go files"}}`)},
			cause: "build constraints exclude all Go files",
		},
		{
			name:    "changed package is not in the graph",
			changed: []string{"internal/gone"},
			r:       importRunner{list: importGraphList(t, importFixture)},
			cause:   "internal/gone",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sel, err := SelectImporters(tc.r.run, importFixtureRoot, "HEAD", tc.changed)
			if err == nil {
				t.Fatalf("SelectImporters = %+v, want an error naming %q", sel, tc.cause)
			}
			if !strings.Contains(err.Error(), tc.cause) {
				t.Errorf("error = %q, want it to name %q", err, tc.cause)
			}
			if len(sel.Add) != 0 || len(sel.LeftOut) != 0 {
				t.Errorf("SelectImporters = %+v with an error, want nothing selected", sel)
			}
		})
	}
}
