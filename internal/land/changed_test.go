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
		{"one modified go file", "M\x00internal/land/gate.go\x00", []string{"internal/land"}},
		{"test file counts", "M\x00internal/land/gate_test.go\x00", []string{"internal/land"}},
		{
			"two files one package, sorted and deduplicated",
			"M\x00internal/land/gate.go\x00A\x00internal/cmd/done.go\x00M\x00internal/land/changed.go\x00",
			[]string{"internal/cmd", "internal/land"},
		},
		{
			"rename across packages names both sides",
			"R095\x00internal/oldhome/a.go\x00internal/newhome/a.go\x00",
			[]string{"internal/newhome", "internal/oldhome"},
		},
		{
			"rename within a package",
			"R100\x00internal/land/a.go\x00internal/land/b.go\x00",
			[]string{"internal/land"},
		},
		{
			// A copy leaves its source untouched, so only the destination's
			// package is retested.
			"copy names only the destination",
			"C075\x00internal/land/a.go\x00internal/newhome/a.go\x00",
			[]string{"internal/newhome"},
		},
		{
			"deleted package is dropped",
			"D\x00internal/gone/x.go\x00D\x00internal/gone/x_test.go\x00",
			nil,
		},
		{
			"deleted file in a surviving package is kept",
			"D\x00internal/oldhome/b.go\x00",
			[]string{"internal/oldhome"},
		},
		{
			"rename out of a deleted package keeps only the new side",
			"R100\x00internal/gone/a.go\x00internal/newhome/a.go\x00",
			[]string{"internal/newhome"},
		},
		{"markdown names nothing", "M\x00docs/testing.md\x00M\x00README.md\x00", nil},
		{"makefile and scripts name nothing", "M\x00Makefile\x00A\x00scripts/x.sh\x00", nil},
		{
			"testdata maps to the owning package",
			"A\x00internal/land/testdata/case/input.txt\x00",
			[]string{"internal/land"},
		},
		{
			// The go tool skips a testdata directory, so the fixture package
			// is internal/testpolicy — not the fixture directory itself, which
			// `go test` cannot build (gt-f1ynu).
			"a go fixture under testdata maps to the owning package",
			"M\x00internal/testpolicy/testdata/docker/qualified/x_test.go\x00",
			[]string{"internal/testpolicy"},
		},
		{
			"an underscored directory is a fixture directory too",
			"M\x00internal/land/_build/gen.go\x00",
			[]string{"internal/land"},
		},
		{
			// A fixture with no enclosing package has no owner to name, and
			// the fixture directory itself must never be handed to `go test`,
			// which cannot build it (gt-f1ynu).
			"a testdata fixture with no enclosing package is skipped",
			"M\x00plugins/foo/testdata/x.go\x00",
			nil,
		},
		{
			"embedded file maps to the owning package",
			"M\x00internal/formula/formulas/mol-x.formula.toml\x00",
			[]string{"internal/formula"},
		},
		{
			"non-go file in a directory under no package",
			"M\x00plugins/foo/run.sh\x00",
			nil,
		},
		{"go.mod names nothing", "M\x00go.mod\x00M\x00go.sum\x00", nil},
		{"blank fields and a dangling status are skipped", "\x00M\x00internal/cmd/x.go\x00M", []string{"internal/cmd"}},
		{"dot-slash paths are cleaned", "M\x00./internal/cmd/x.go\x00", []string{"internal/cmd"}},
		{
			// git -z writes the name as it is: the non-ASCII byte is not
			// quoted to octal, so the .go suffix survives to name the package.
			"non-ASCII added file names its package",
			"A\x00internal/cmd/café_test.go\x00",
			[]string{"internal/cmd"},
		},
		{
			"tab and quote in a name are not separators",
			"M\x00internal/cmd/tab\tname\"q\".go\x00",
			[]string{"internal/cmd"},
		},
		{
			// The record is cut short: its path never arrived, so it names
			// nothing rather than the "." an empty path cleans to.
			"a truncated rename record names nothing",
			"R100\x00internal/cmd/a.go\x00",
			nil,
		},
		{
			"a truncated record after a whole one keeps the whole one",
			"M\x00internal/cmd/x.go\x00R100\x00internal/cmd/a.go\x00",
			[]string{"internal/cmd"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ChangedPackages(tt.diff, hasGo)
			if err != nil {
				t.Fatalf("ChangedPackages: %v", err)
			}
			if len(got) == 0 && len(tt.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ChangedPackages = %v, want %v", got, tt.want)
			}
		})
	}
}

// Output without -z separates fields with tabs and ends lines with newlines,
// and git quotes a non-ASCII name in it. The NUL reader cannot find the paths
// in that, so it must refuse the output, not select nothing.
func TestChangedPackagesRejectsOutputWithoutNUL(t *testing.T) {
	t.Parallel()
	hasGo := func(string) bool { return true }
	for name, diff := range map[string]string{
		"quoted non-ASCII added file": "A\tinternal/cmd/caf\\303\\251_test.go\n",
		"plain modified file":         "M\tinternal/land/gate.go\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got, err := ChangedPackages(diff, hasGo); err == nil {
				t.Errorf("ChangedPackages(%q) = %v, want an error", diff, got)
			}
			if _, err := GuardSelects(diff, hasGo); err == nil {
				t.Errorf("GuardSelects(%q) returned no error", diff)
			}
			if _, err := ChangedPaths(diff); err == nil {
				t.Errorf("ChangedPaths(%q) returned no error", diff)
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
	{"internal/testpolicy", "M\x00internal/land/gate_test.go\x00"},
	{"internal/cmdtree", "M\x00internal/templates/roles/polecat.md.tmpl\x00"},
	{"internal/cmd", "M\x00Makefile\x00"},
	{"internal/bdgate", "M\x00internal/session/lifecycle.go\x00"},
	{"internal/polecat", "M\x00internal/daemon/patrol.go\x00"},
	{"internal/beads", "M\x00internal/plugin/dispatch.go\x00"},
	{"internal/beads/beadsfake", "A\x00internal/newpkg/new.go\x00"},
	{"internal/beadsql", "M\x00internal/daemon/patrol.go\x00"},
	{"internal/config", "M\x00cmd/gt/main.go\x00"},
	{"internal/townconfig", "M\x00cmd/gt/main.go\x00"},
	{"internal/guardlint", "M\x00internal/daemon/patrol.go\x00"},
	{"internal/rig", "M\x00internal/daemon/patrol.go\x00"},
	{"internal/tmux", "M\x00internal/daemon/patrol.go\x00"},
	{"internal/doltserver", "M\x00internal/daemon/patrol.go\x00"},
	{"internal/deps", "M\x00internal/daemon/patrol.go\x00"},
	{"internal/testdb", "M\x00internal/daemon/patrol.go\x00"},
	{"internal/testutil", "M\x00internal/daemon/patrol.go\x00"},
	{"internal/land", "M\x00.om.json\x00"},
	{"internal/session", "M\x00deploy/polecat/policy.yaml\x00"},
}

// unguarded is a changed path no guard reads: a doc outside every guard's
// inputs, so a branch holding only this one triggers nothing.
const unguarded = "M\x00docs/testing.md\x00"

func TestChangedPaths(t *testing.T) {
	t.Parallel()
	got, err := ChangedPaths("R100\x00internal/a\x00internal/b\x00D\x00internal/gone.go\x00C075\x00x/src.go\x00x/dst.go\x00M\x00./y.go\x00")
	if err != nil {
		t.Fatalf("ChangedPaths: %v", err)
	}
	// A copy's source is named too: the tree-wide check reads both sides.
	want := []string{"internal/a", "internal/b", "internal/gone.go", "x/src.go", "x/dst.go", "y.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ChangedPaths = %q, want %q", got, want)
	}
}

func TestGuardSelects(t *testing.T) {
	t.Parallel()
	hasGo := func(string) bool { return true }

	if got, err := GuardSelects(unguarded, hasGo); err != nil || got != nil {
		t.Errorf("GuardSelects(%q) = %v, %v, want nil", unguarded, got, err)
	}
	if got, err := GuardSelects("", hasGo); err != nil || got != nil {
		t.Errorf("GuardSelects(empty) = %v, %v, want nil", got, err)
	}
	for _, tc := range guardTriggers {
		t.Run(tc.pkg, func(t *testing.T) {
			t.Parallel()
			got, err := GuardSelects(tc.trigger, hasGo)
			if err != nil {
				t.Fatalf("GuardSelects(%q): %v", tc.trigger, err)
			}
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
			"D\x00internal/crew/manager.go\x00",
			"internal/bdgate",
		},
		{
			// The lists testpolicy ratchets go stale when a listed package's
			// last test file goes with it.
			"deleting a test file selects testpolicy",
			"D\x00internal/land/old_test.go\x00",
			"internal/testpolicy",
		},
		{
			// The session policy tests read the file, so its removal fails them.
			"deleting the polecat policy selects session",
			"D\x00deploy/polecat/policy.yaml\x00",
			"internal/session",
		},
		{
			"deleting a doc selects nothing",
			"D\x00docs/old.md\x00",
			"",
		},
		{
			// A guard that ratchets a baseline down fails on the rename away:
			// the file it counted no longer exists.
			"renaming away from agent prose selects cmdtree",
			"R100\x00internal/templates/roles/polecat.md.tmpl\x00internal/templates/roles/polecat.txt\x00",
			"internal/cmdtree",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sel, err := GuardSelects(tt.diff, hasGo)
			if err != nil {
				t.Fatalf("GuardSelects(%q): %v", tt.diff, err)
			}
			var pkgs []string
			for _, s := range sel {
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
	if got, err := GuardSelects("M\x00internal/templates/roles/polecat.md.tmpl\x00", hasGo); err != nil || got != nil {
		t.Errorf("GuardSelects = %v, %v, want nil when the guard package holds no Go", got, err)
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
