package storypoints

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// bannedImports are the paths the core package may not import. hmhints and
// os/exec bring hm's process exec into the daemon's import closure. Code that
// asks hm for neighbors lives in the nearest package instead.
var bannedImports = map[string]bool{
	"github.com/steveyegge/gastown/internal/hmhints": true,
	"os/exec": true,
	"github.com/steveyegge/gastown/internal/storypoints/nearest": true,
}

// forbiddenImports parses the non-test .go files directly in dir and reports
// each banned import as "file: path". It also returns how many files it
// parsed, so a directory with no source is an error, not a clean pass.
func forbiddenImports(dir string) (violations []string, scanned int, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			return nil, 0, err
		}
		scanned++
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return nil, 0, err
			}
			if bannedImports[path] {
				violations = append(violations, name+": "+path)
			}
		}
	}
	if scanned == 0 {
		return nil, 0, fmt.Errorf("no non-test .go files in %s", dir)
	}
	return violations, scanned, nil
}

func TestCoreImportsAreClean(t *testing.T) {
	t.Parallel()
	violations, _, err := forbiddenImports(".")
	if err != nil {
		t.Fatalf("scan the core package: %v", err)
	}
	for _, v := range violations {
		t.Errorf("core package imports %s; keep hm and process exec out of it", v)
	}
}

func TestForbiddenImportsRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		files          map[string]string
		missing        bool
		wantViolations []string
		wantScanned    int
		wantErr        bool
	}{
		{
			name:           "hmhints import is flagged",
			files:          map[string]string{"a.go": "package p\n\nimport \"github.com/steveyegge/gastown/internal/hmhints\"\n"},
			wantViolations: []string{"a.go: github.com/steveyegge/gastown/internal/hmhints"},
			wantScanned:    1,
		},
		{
			name:           "os/exec import is flagged",
			files:          map[string]string{"a.go": "package p\n\nimport \"os/exec\"\n"},
			wantViolations: []string{"a.go: os/exec"},
			wantScanned:    1,
		},
		{
			name:           "nearest import is flagged",
			files:          map[string]string{"a.go": "package p\n\nimport \"github.com/steveyegge/gastown/internal/storypoints/nearest\"\n"},
			wantViolations: []string{"a.go: github.com/steveyegge/gastown/internal/storypoints/nearest"},
			wantScanned:    1,
		},
		{
			name: "every banned import in one file is reported",
			files: map[string]string{"a.go": "package p\n\nimport (\n" +
				"\t\"os/exec\"\n\t\"github.com/steveyegge/gastown/internal/hmhints\"\n)\n"},
			wantViolations: []string{
				"a.go: os/exec",
				"a.go: github.com/steveyegge/gastown/internal/hmhints",
			},
			wantScanned: 1,
		},
		{
			name:        "clean import passes",
			files:       map[string]string{"a.go": "package p\n\nimport \"os\"\n"},
			wantScanned: 1,
		},
		{
			name: "test files are not scanned",
			files: map[string]string{
				"a.go":      "package p\n\nimport \"os\"\n",
				"a_test.go": "package p\n\nimport \"os/exec\"\n",
			},
			wantScanned: 1,
		},
		{
			name:    "empty directory is refused",
			files:   map[string]string{},
			wantErr: true,
		},
		{
			name:    "missing directory is refused",
			missing: true,
			wantErr: true,
		},
		{
			name:    "file that does not parse is refused",
			files:   map[string]string{"a.go": "package p\n\nimport (\n"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "pkg")
			if !tt.missing {
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				for name, src := range tt.files {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			violations, scanned, err := forbiddenImports(dir)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("forbiddenImports = %q, %d, nil; want an error", violations, scanned)
				}
				return
			}
			if err != nil {
				t.Fatalf("forbiddenImports: %v", err)
			}
			if !slices.Equal(violations, tt.wantViolations) {
				t.Fatalf("violations = %q, want %q", violations, tt.wantViolations)
			}
			if scanned != tt.wantScanned {
				t.Fatalf("scanned = %d, want %d", scanned, tt.wantScanned)
			}
		})
	}
}
