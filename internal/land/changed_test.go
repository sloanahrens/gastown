package land

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

func TestChangedPackages(t *testing.T) {
	t.Parallel()
	// The tree after the change: these directories hold Go files. A testdata
	// directory holds one too once a fixture is written in Go, and the go tool
	// still does not build it (gt-f1ynu).
	live := map[string]bool{
		"internal/land":    true,
		"internal/cmd":     true,
		"internal/formula": true,
		"internal/newhome": true,
		"internal/oldhome": true, // lost one file but keeps others
		"cmd/gt":           true,

		"internal/testpolicy":                           true,
		"internal/testpolicy/testdata/docker/qualified": true,
		"internal/land/testdata/case":                   true,
		"internal/land/_build":                          true,
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
			// A copy leaves its source untouched, so only the destination's
			// package is retested.
			"copy names only the destination",
			"C075\tinternal/land/a.go\tinternal/newhome/a.go\n",
			[]string{"internal/newhome"},
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
			"A\tinternal/land/testdata/case/input.txt\n",
			[]string{"internal/land"},
		},
		{
			// The go tool skips a testdata directory, so the fixture package
			// is internal/testpolicy — not the fixture directory itself, which
			// `go test` cannot build (gt-f1ynu).
			"a go fixture under testdata maps to the owning package",
			"M\tinternal/testpolicy/testdata/docker/qualified/x_test.go\n",
			[]string{"internal/testpolicy"},
		},
		{
			"an underscored directory is a fixture directory too",
			"M\tinternal/land/_build/gen.go\n",
			[]string{"internal/land"},
		},
		{
			// A fixture with no enclosing package has no owner to name, and
			// the fixture directory itself must never be handed to `go test`,
			// which cannot build it (gt-f1ynu).
			"a testdata fixture with no enclosing package is skipped",
			"M\tplugins/foo/testdata/x.go\n",
			nil,
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
			t.Parallel()
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

// guardTriggers names, per guard package in treeWideGuards, a diff that
// trips its guard tests. TestGuardSelects fails when the table grows a
// package this list does not name, so a new guard cannot land untested.
var guardTriggers = []struct {
	pkg     string
	trigger string
}{
	{"internal/testpolicy", "M\tinternal/land/gate_test.go\n"},
	{"internal/cmdtree", "M\tinternal/templates/roles/polecat.md.tmpl\n"},
	{"internal/cmd", "M\tMakefile\n"},
	{"internal/bdgate", "M\tinternal/session/lifecycle.go\n"},
	{"internal/polecat", "M\tinternal/daemon/patrol.go\n"},
	{"internal/beads", "M\tinternal/plugin/dispatch.go\n"},
	{"internal/beads/beadsfake", "A\tinternal/newpkg/new.go\n"},
	{"internal/beadsql", "M\tinternal/daemon/patrol.go\n"},
	{"internal/config", "M\tcmd/gt/main.go\n"},
	{"internal/townconfig", "M\tcmd/gt/main.go\n"},
	{"internal/guardlint", "M\tinternal/daemon/patrol.go\n"},
	{"internal/rig", "M\tinternal/daemon/patrol.go\n"},
	{"internal/tmux", "M\tinternal/daemon/patrol.go\n"},
	{"internal/doltserver", "M\tinternal/daemon/patrol.go\n"},
	{"internal/deps", "M\tinternal/daemon/patrol.go\n"},
	{"internal/testdb", "M\tinternal/daemon/patrol.go\n"},
	{"internal/testutil", "M\tinternal/daemon/patrol.go\n"},
	{"internal/land", "M\t.om.json\n"},
}

// unguarded is a changed path no guard reads: a doc outside every guard's
// inputs, so a branch holding only this one triggers nothing.
const unguarded = "M\tdocs/testing.md\n"

func TestGuardSelects(t *testing.T) {
	t.Parallel()
	hasGo := func(string) bool { return true }

	if got := GuardSelects(unguarded, hasGo); got != nil {
		t.Errorf("GuardSelects(%q) = %v, want nil", unguarded, got)
	}
	if got := GuardSelects("", hasGo); got != nil {
		t.Errorf("GuardSelects(empty) = %v, want nil", got)
	}
	for _, tc := range guardTriggers {
		t.Run(tc.pkg, func(t *testing.T) {
			t.Parallel()
			got := GuardSelects(tc.trigger, hasGo)
			for _, s := range got {
				if s.Package == tc.pkg {
					if len(s.Tests) == 0 {
						t.Fatalf("%s triggered with no tests to run", tc.pkg)
					}
					return
				}
			}
			t.Errorf("GuardSelects(%q) = %v, want %s among them", tc.trigger, got, tc.pkg)
		})
	}
}

// TestEveryGuardIsSampled keeps the two lists in step: a guard package the
// sampler does not name has a predicate no test exercises.
func TestEveryGuardIsSampled(t *testing.T) {
	t.Parallel()
	sampled := map[string]bool{}
	for _, tc := range guardTriggers {
		sampled[tc.pkg] = true
	}
	for _, g := range treeWideGuards {
		if !sampled[g.pkg] {
			t.Errorf("treeWideGuards names %s, which guardTriggers does not sample", g.pkg)
		}
	}
}

func TestGuardSelectsRemovals(t *testing.T) {
	t.Parallel()
	hasGo := func(string) bool { return true }

	tests := []struct {
		name string
		diff string
		want string // the package that must be selected, "" for none
	}{
		{
			// A guard that reads a path directly fails when the path is gone.
			"deleting a session start file selects bdgate",
			"D\tinternal/crew/manager.go\n",
			"internal/bdgate",
		},
		{
			// The lists testpolicy ratchets go stale when a listed package's
			// last test file goes with it.
			"deleting a test file selects testpolicy",
			"D\tinternal/land/old_test.go\n",
			"internal/testpolicy",
		},
		{
			"deleting a doc selects nothing",
			"D\tdocs/old.md\n",
			"",
		},
		{
			// A guard that ratchets a baseline down fails on the rename away:
			// the file it counted no longer exists.
			"renaming away from agent prose selects cmdtree",
			"R100\tinternal/templates/roles/polecat.md.tmpl\tinternal/templates/roles/polecat.txt\n",
			"internal/cmdtree",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var pkgs []string
			for _, s := range GuardSelects(tt.diff, hasGo) {
				pkgs = append(pkgs, s.Package)
			}
			sort.Strings(pkgs)
			if tt.want == "" {
				if len(pkgs) != 0 {
					t.Errorf("GuardSelects(%q) = %v, want none", tt.diff, pkgs)
				}
				return
			}
			if !contains(pkgs, tt.want) {
				t.Errorf("GuardSelects(%q) = %v, want %s among them", tt.diff, pkgs, tt.want)
			}
		})
	}
}

func TestGuardSelectsSkipsMissingPackages(t *testing.T) {
	t.Parallel()
	hasGo := func(string) bool { return false }
	if got := GuardSelects("M\tinternal/templates/roles/polecat.md.tmpl\n", hasGo); got != nil {
		t.Errorf("GuardSelects = %v, want nil when the guard package holds no Go", got)
	}
}

func TestPackageDirHasGo(t *testing.T) {
	t.Parallel()
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

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
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
