package daemonimage

import (
	"path/filepath"
	"testing"
)

// derivedRoot is the module root the fixture's go list lines are under.
const derivedRoot = "/src/devops"

// derivedGoList is the captured output of `go list -deps -f '{{.Dir}}'` for the
// devops tools in a fixture module. It mixes the module's own directories with
// the standard library, a module in the module cache, another checkout, and a
// sibling whose path has the root as a string prefix but not as a directory.
// pkg/patrol appears twice.
const derivedGoList = `/usr/local/go/src/fmt
/src/devops/internal/ledgerdb
/home/u/go/pkg/mod/github.com/x/y@v1.2.0/errs
/src/devops/pkg/patrol
/src/devops/cmd/reporter
/src/devops/pkg/patrol
/src/devops
/src/devopsx/pkg/patrol
/src/other/pkg/tool
/src/devops/cmd/cloudcheck
/usr/local/go/src/internal/fmtsort
`

func TestDerivedInputPathsFromGoListOutput(t *testing.T) {
	t.Parallel()
	wantDirs(t, derivedInputPaths(derivedGoList, derivedRoot),
		[]string{"internal/ledgerdb", "pkg/patrol", "cmd/reporter", ".", "cmd/cloudcheck"})
}

func TestDerivedInputsDropOtherModulesAndStdlib(t *testing.T) {
	t.Parallel()
	for _, dir := range derivedInputPaths(derivedGoList, derivedRoot) {
		if !filepath.IsLocal(dir) {
			t.Errorf("derived %q is outside the module: stdlib or another module was kept", dir)
		}
	}
}
