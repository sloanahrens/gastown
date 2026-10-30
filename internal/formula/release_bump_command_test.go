package formula

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/BurntSushi/toml"
)

// releaseFormulas are the two release formulas that run the bump command.
var releaseFormulas = []string{"gastown-release.formula.toml", "beads-release.formula.toml"}

// bumpCommandLine is the line a release formula runs to bump the version. The
// capture is the package path the command builds, relative to the repo root.
var bumpCommandLine = regexp.MustCompile(`(?m)^go run (\./\S+) \{\{version\}\}$`)

// deletedBumpScripts are the scripts the Go command replaced. No formula may name them.
var deletedBumpScripts = []string{
	"scripts/bump-version.sh",
	"scripts/update-nix-flake.sh",
	"scripts/lib/common.sh",
}

// repoRoot is the repository root as seen from this package's directory, which is
// where go test runs.
const repoRoot = "../.."

// releaseStep is the part of a formula step the bump check reads.
type releaseStep struct {
	ID          string `toml:"id"`
	Title       string `toml:"title"`
	Description string `toml:"description"`
}

// bumpPackageProblem returns an error when pkg, a path under root, does not hold a
// main.go whose package clause is package main. A go run of pkg cannot succeed then.
func bumpPackageProblem(root, pkg string) error {
	file := filepath.Join(root, filepath.FromSlash(pkg), "main.go")
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.PackageClauseOnly)
	if err != nil {
		return err
	}
	if f.Name.Name != "main" {
		return fmt.Errorf("package %s, want package main", f.Name.Name)
	}
	return nil
}

// TestBumpPackageProblem drives each branch of the package check against a temporary
// tree, so a directory that is missing, has no main.go, or names another package is
// refused.
func TestBumpPackageProblem(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mainGo  string // "" means no main.go is written
		wantErr bool
	}{
		{name: "main package", mainGo: "package main\n"},
		{name: "no main.go", mainGo: "", wantErr: true},
		{name: "other package", mainGo: "package tool\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := t.TempDir()
			pkg := "cmd/tool"
			dir := filepath.Join(root, filepath.FromSlash(pkg))
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.mainGo != "" {
				if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(tc.mainGo), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			err := bumpPackageProblem(root, pkg)
			if (err != nil) != tc.wantErr {
				t.Errorf("bumpPackageProblem() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestReleaseFormulasRunBumpCommand pins the bump step of each release formula to
// the Go command, and proves the package that command names exists in the repo as
// package main. A formula that still names the deleted bump-version.sh, or that
// runs a directory that is not package main, hands a releaser a step that cannot run.
func TestReleaseFormulasRunBumpCommand(t *testing.T) {
	t.Parallel()

	for _, name := range releaseFormulas {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			data, err := GetEmbeddedFormulaContent(name)
			if err != nil {
				t.Fatalf("GetEmbeddedFormulaContent(%s): %v", name, err)
			}

			var formula struct {
				Steps []releaseStep `toml:"steps"`
			}
			if _, err := toml.Decode(string(data), &formula); err != nil {
				t.Fatalf("decode %s: %v", name, err)
			}

			var bump *releaseStep
			for i := range formula.Steps {
				if formula.Steps[i].ID == "run-bump-script" {
					bump = &formula.Steps[i]
					break
				}
			}
			if bump == nil {
				t.Fatalf("%s has no run-bump-script step", name)
			}

			m := bumpCommandLine.FindStringSubmatch(bump.Description)
			if m == nil {
				t.Fatalf("%s run-bump-script does not run go run ./<package> {{version}}:\n%s", name, bump.Description)
			}
			if err := bumpPackageProblem(repoRoot, m[1]); err != nil {
				t.Errorf("%s run-bump-script runs go run %s, which is not a package main: %v", name, m[1], err)
			}
			if strings.Contains(bump.Title+bump.Description, "bump-version.sh") {
				t.Errorf("%s run-bump-script still names bump-version.sh", name)
			}
		})
	}
}

// deletedScriptHits scans every formula file under fsys matching pattern for the scripts
// the Go command replaced and returns one "<file>:<line> names deleted script <name>"
// entry per hit. It errors when the pattern matches no file, so a scan that looked at
// nothing cannot pass.
func deletedScriptHits(fsys fs.FS, pattern string) ([]string, error) {
	paths, err := fs.Glob(fsys, pattern)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no formulas match %q", pattern)
	}
	var hits []string
	for _, p := range paths {
		content, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, err
		}
		for i, line := range strings.Split(string(content), "\n") {
			for _, script := range deletedBumpScripts {
				if strings.Contains(line, script) {
					hits = append(hits, fmt.Sprintf("%s:%d names deleted script %s", p, i+1, script))
				}
			}
		}
	}
	return hits, nil
}

// TestFormulasNameNoDeletedBumpScript scans every embedded formula for the scripts
// the Go command replaced. A hit names the formula and the line.
func TestFormulasNameNoDeletedBumpScript(t *testing.T) {
	t.Parallel()

	hits, err := deletedScriptHits(formulasFS, "formulas/*.formula.toml")
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range hits {
		t.Error(hit)
	}
}

// TestDeletedScriptHitsDrivesItsFailingBranch drives the scan over formulas it builds, so
// the branch that reports a hit, the branch that finds none, and the branch that refuses
// an empty match are each exercised without depending on the real formulas.
func TestDeletedScriptHitsDrivesItsFailingBranch(t *testing.T) {
	t.Parallel()

	t.Run("a hit names the file, the line and the script", func(t *testing.T) {
		t.Parallel()
		fsys := fstest.MapFS{
			"formulas/a.formula.toml": {Data: []byte("id = \"x\"\ndescription = \"Run ./scripts/bump-version.sh {{version}}\"\n")},
			"formulas/b.formula.toml": {Data: []byte("description = \"Run go run ./cmd/bump-version {{version}}\"\n")},
		}
		hits, err := deletedScriptHits(fsys, "formulas/*.formula.toml")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"formulas/a.formula.toml:2 names deleted script scripts/bump-version.sh"}
		if !reflect.DeepEqual(hits, want) {
			t.Errorf("hits = %q, want %q", hits, want)
		}
	})

	t.Run("a clean formula reports nothing", func(t *testing.T) {
		t.Parallel()
		fsys := fstest.MapFS{"formulas/b.formula.toml": {Data: []byte("description = \"Run go run ./cmd/bump-version {{version}}\"\n")}}
		hits, err := deletedScriptHits(fsys, "formulas/*.formula.toml")
		if err != nil || len(hits) != 0 {
			t.Errorf("deletedScriptHits() = %q, %v, want no hits and no error", hits, err)
		}
	})

	t.Run("every deleted name is found", func(t *testing.T) {
		t.Parallel()
		var b strings.Builder
		for _, script := range deletedBumpScripts {
			b.WriteString("x " + script + "\n")
		}
		fsys := fstest.MapFS{"formulas/c.formula.toml": {Data: []byte(b.String())}}
		hits, err := deletedScriptHits(fsys, "formulas/*.formula.toml")
		if err != nil || len(hits) != len(deletedBumpScripts) {
			t.Errorf("deletedScriptHits() = %q, %v, want %d hits", hits, err, len(deletedBumpScripts))
		}
	})

	t.Run("a pattern that matches nothing is an error", func(t *testing.T) {
		t.Parallel()
		if _, err := deletedScriptHits(fstest.MapFS{}, "formulas/*.formula.toml"); err == nil {
			t.Error("deletedScriptHits(empty fs) returned no error, want one: a scan that looked at nothing cannot pass")
		}
	})
}
