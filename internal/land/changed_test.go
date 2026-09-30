package land

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestChangedPackages(t *testing.T) {
	// The tree after the change: these directories hold Go files.
	live := map[string]bool{
		"internal/land":       true,
		"internal/cmd":        true,
		"internal/formula":    true,
		"internal/newhome":    true,
		"internal/oldhome":    true, // lost one file but keeps others
		"internal/testpolicy": true,
		"cmd/gt":              true,
	}
	hasGo := func(dir string) bool { return live[dir] }

	tests := []struct {
		name string
		diff string
		want []string
	}{
		{"empty diff", "", nil},
		{"one modified go file", "M\tinternal/land/gate.go\n", []string{"internal/land"}},
		{"test file counts", "M\tinternal/land/gate_test.go\n", []string{"internal/land"}},
		{
			"two files one package, sorted and deduplicated",
			"M\tinternal/land/gate.go\nA\tinternal/cmd/done.go\nM\tinternal/land/changed.go\n",
			[]string{"internal/cmd", "internal/land"},
		},
		{
			"rename across packages names both sides",
			"R095\tinternal/oldhome/a.go\tinternal/newhome/a.go\n",
			[]string{"internal/newhome", "internal/oldhome"},
		},
		{
			"rename within a package",
			"R100\tinternal/land/a.go\tinternal/land/b.go\n",
			[]string{"internal/land"},
		},
		{
			"deleted package is dropped",
			"D\tinternal/gone/x.go\nD\tinternal/gone/x_test.go\n",
			nil,
		},
		{
			"deleted file in a surviving package is kept",
			"D\tinternal/oldhome/b.go\n",
			[]string{"internal/oldhome"},
		},
		{
			"rename out of a deleted package keeps only the new side",
			"R100\tinternal/gone/a.go\tinternal/newhome/a.go\n",
			[]string{"internal/newhome"},
		},
		{"markdown names nothing", "M\tdocs/testing.md\nM\tREADME.md\n", nil},
		{"makefile and scripts name nothing", "M\tMakefile\nA\tscripts/x.sh\n", nil},
		{
			"testdata maps to the owning package",
			"A\tinternal/testpolicy/testdata/case/input.txt\n",
			[]string{"internal/testpolicy"},
		},
		{
			"embedded file maps to the owning package",
			"M\tinternal/formula/formulas/mol-x.formula.toml\n",
			[]string{"internal/formula"},
		},
		{
			"non-go file in a directory under no package",
			"M\tplugins/foo/run.sh\n",
			nil,
		},
		{"go.mod names nothing", "M\tgo.mod\nM\tgo.sum\n", nil},
		{"malformed and blank lines are skipped", "\nM\n  \nM\tinternal/cmd/x.go\n", []string{"internal/cmd"}},
		{"crlf line endings", "M\tinternal/cmd/x.go\r\n", []string{"internal/cmd"}},
		{"dot-slash paths are cleaned", "M\t./internal/cmd/x.go\n", []string{"internal/cmd"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ChangedPackages(tt.diff, hasGo)
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ChangedPackages = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPackageDirHasGo(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "pkg/a.go", "package pkg\n")
	writeFile(t, root, "docs/readme.md", "x\n")
	writeFile(t, root, "nested/deep/b.go", "package deep\n")

	for dir, want := range map[string]bool{
		"pkg":      true,
		"docs":     false,
		"nested":   false, // only a subdirectory holds Go
		"missing":  false,
		"pkg/a.go": false, // a file, not a directory
	} {
		if got := PackageDirHasGo(root, dir); got != want {
			t.Errorf("PackageDirHasGo(%q) = %v, want %v", dir, got, want)
		}
	}
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
