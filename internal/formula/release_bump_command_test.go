package formula

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

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
// runs a package that does not build, hands a releaser a step that cannot run.
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

// TestFormulasNameNoDeletedBumpScript scans every embedded formula for the scripts
// the Go command replaced. A hit names the formula and the line.
func TestFormulasNameNoDeletedBumpScript(t *testing.T) {
	t.Parallel()

	paths, err := fs.Glob(formulasFS, "formulas/*.formula.toml")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no embedded formulas found")
	}
	for _, p := range paths {
		content, err := formulasFS.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(content), "\n") {
			for _, script := range deletedBumpScripts {
				if strings.Contains(line, script) {
					t.Errorf("%s:%d names deleted script %s", p, i+1, script)
				}
			}
		}
	}
}
