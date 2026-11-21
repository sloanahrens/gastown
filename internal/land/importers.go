package land

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/affected"
)

// ImporterSelection is the direct importers presubmit adds to the packages a
// branch changed (gt-cwkzxk).
type ImporterSelection struct {
	// Add holds the importers presubmit tests: repo-relative directories, sorted.
	Add []string
	// LeftOut holds the heavy importers presubmit does not test, each with why.
	LeftOut []LeftOut
}

// LeftOut is a direct importer presubmit names and does not run.
type LeftOut struct {
	Dir    string
	Reason string
}

// heavyImporters are the direct importers presubmit leaves out of its test run
// because they are too slow to run in full: SelectImporters reports a match in
// LeftOut, and presubmit prints it with this reason so the missed tests get run
// narrowed with -run. An entry needs a measured full-run time and the reason
// (gt-cfc6wh).
var heavyImporters = map[string]string{}

// SelectImporters returns the direct importers of the changed package
// directories, split into the ones presubmit tests and the heavy ones it leaves
// out. changed holds repo-relative directories and wd is the module root the
// graph is read from, with head the commit it must describe (see
// affected.DirectImporters). A graph that cannot be read fails the selection,
// with the cause: presubmit must not test the changed packages alone and pass.
func SelectImporters(run affected.Runner, wd, head string, changed []string) (ImporterSelection, error) {
	return selectImporters(run, wd, head, changed, heavyImporters)
}

// selectImporters is SelectImporters over an explicit heavy map, so a test
// drives the left-out path without swapping the package variable.
func selectImporters(run affected.Runner, wd, head string, changed []string, heavy map[string]string) (ImporterSelection, error) {
	importers, err := affected.DirectImporters(run, wd, head, changed)
	if err != nil {
		return ImporterSelection{}, fmt.Errorf("direct importers of the changed packages: %w", err)
	}
	var sel ImporterSelection
	for _, dir := range importers {
		if reason, isHeavy := heavy[dir]; isHeavy {
			sel.LeftOut = append(sel.LeftOut, LeftOut{Dir: dir, Reason: reason})
			continue
		}
		sel.Add = append(sel.Add, dir)
	}
	return sel, nil
}
