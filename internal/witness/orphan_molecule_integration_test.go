//go:build integration

package witness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/testutil"
)

// bondRealPolecatWork bonds the real mol-polecat-work formula to a fresh bead
// with `bd mol bond … --ephemeral`, against a real bd on the test Dolt
// container, and returns the base bead and the wisp root gt sling records as
// attached_molecule.
//
// The formula is copied out of this checkout rather than restated here, so
// the molecule carries the steps and the chained `needs` a slung polecat
// gets.
func bondRealPolecatWork(t *testing.T, dir string, b *beads.Beads) (baseID, wispRootID string) {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed: cannot locate the formula to bond")
	}
	formula, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "formula", "formulas", "mol-polecat-work.formula.toml"))
	if err != nil {
		t.Fatalf("reading mol-polecat-work: %v", err)
	}
	formulaDir := filepath.Join(dir, ".beads", "formulas")
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", formulaDir, err)
	}
	if err := os.WriteFile(filepath.Join(formulaDir, "mol-polecat-work.formula.toml"), formula, 0o644); err != nil {
		t.Fatalf("writing the formula beside the database: %v", err)
	}

	base, err := b.Create(beads.CreateOptions{Title: "assigned work", Priority: 1})
	if err != nil {
		t.Fatalf("creating the base bead: %v", err)
	}

	out, err := DefaultBdCli().Exec(dir, "mol", "bond", "mol-polecat-work", base.ID, "--ephemeral", "--json",
		"--var", "issue="+base.ID, "--var", "base_branch=main",
		"--var", "setup_command=", "--var", "build_command=make build",
		"--var", "typecheck_command=", "--var", "lint_command=make lint",
		"--var", "test_command=make gate")
	if err != nil {
		t.Fatalf("bd mol bond mol-polecat-work %s --ephemeral: %v", base.ID, err)
	}
	var bond struct {
		ResultID  string            `json:"result_id"`
		IDMapping map[string]string `json:"id_mapping"`
	}
	if err := json.Unmarshal([]byte(out), &bond); err != nil {
		t.Fatalf("parsing the bond result: %v (%s)", err, out)
	}
	// bd reports the base bead as the compound molecule's result_id, while
	// the spawned root every step hangs off is the id_mapping entry. gt sling
	// records the latter as attached_molecule (sling.go, bondFormulaDirect ->
	// parseBondSpawnRootID), so that is the id the cleanup is handed.
	if bond.ResultID != base.ID {
		t.Errorf("bond result_id = %q, want the base bead %s", bond.ResultID, base.ID)
	}
	root := bond.IDMapping["mol-polecat-work"]
	if root == "" {
		t.Fatalf("bond id_mapping has no mol-polecat-work entry: %s", out)
	}
	return base.ID, root
}

// newIsolatedBeadsDB hands the test an empty bd database on the shared Dolt
// container, initialized in a directory the test owns.
func newIsolatedBeadsDB(t *testing.T) (string, *beads.Beads) {
	t.Helper()
	testutil.RequireDoltContainer(t)
	port, err := strconv.Atoi(testutil.DoltContainerPort())
	if err != nil {
		t.Fatalf("Dolt container port %q is not a number: %v", testutil.DoltContainerPort(), err)
	}
	dir := t.TempDir()
	b := beads.NewIsolatedWithPort(dir, port)
	if err := b.Init("wz"); err != nil {
		testutil.FailContainerInit(t, b, err)
	}
	return dir, b
}

// queryRows runs a read-only `bd sql` query and returns its rows as fields,
// dropping the header, the rule under it and the row-count footer.
func queryRows(t *testing.T, dir string, cols int, query string) [][]string {
	t.Helper()
	out, err := DefaultBdCli().Exec(dir, "sql", query)
	if err != nil {
		t.Fatalf("bd sql %s: %v", query, err)
	}
	var rows [][]string
	for _, line := range strings.Split(out, "\n") {
		if line == "" || strings.HasPrefix(line, "(") || strings.Contains(line, "---+") {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) != cols {
			t.Fatalf("bd sql %s row %q has %d fields, want %d", query, line, len(fields), cols)
		}
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		rows = append(rows, fields)
	}
	if len(rows) > 0 && rows[0][0] == "id" {
		rows = rows[1:] // the header row
	}
	return rows
}

// wispStatuses returns the status of every wisp in the database, read
// straight from the wisps table: bd's own listings hide the ephemeral plane
// by default. Each test owns a database created for it alone, so every row
// it holds belongs to that test's one molecule.
func wispStatuses(t *testing.T, dir string) map[string]string {
	t.Helper()
	statuses := map[string]string{}
	for _, row := range queryRows(t, dir, 2, "select id, status from wisps order by id") {
		statuses[row[0]] = row[1]
	}
	return statuses
}

// wispParentChildTargets returns the depends-on id of every parent-child
// edge in the wisp dependency table.
func wispParentChildTargets(t *testing.T, dir string) []string {
	t.Helper()
	var targets []string
	rows := queryRows(t, dir, 3, "select issue_id, coalesce(depends_on_issue_id, depends_on_wisp_id), type from wisp_dependencies")
	for _, row := range rows {
		if row[2] == "parent-child" {
			targets = append(targets, row[1])
		}
	}
	return targets
}

func contains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestIntegrationOrphanCleanupClosesBondedEphemeralMolecule: the witness's
// orphan cleanup over a real `bd mol bond mol-polecat-work <bead>
// --ephemeral` closes all eight step wisps and their root.
//
// The bead that asked for this read the bond's result_id as the molecule
// root and found `bd list --parent=<base bead>` answering []. That [] is the
// graph, not a blind spot: bd links the wisp root to the base bead with a
// `blocks` edge, so the base bead has no parent-child children at all. The
// eight steps hang off the wisp root by parent-child, in wisp_dependencies,
// and both reads this package could use find them there (gt-22hdp.36).
func TestIntegrationOrphanCleanupClosesBondedEphemeralMolecule(t *testing.T) {
	dir, b := newIsolatedBeadsDB(t)
	base, root := bondRealPolecatWork(t, dir, b)
	bd := DefaultBdCli()

	// The base bead is not a parent of anything: the bond edge is `blocks`,
	// so reading its children answers [] whatever argv is used. Asserting
	// the shape here is what keeps a later reader from re-filing it.
	listed, err := bd.Exec(dir, "list", "--parent="+base, "--json")
	if err != nil {
		t.Fatalf("bd list --parent=%s: %v", base, err)
	}
	if strings.TrimSpace(listed) != "[]" {
		t.Errorf("bd list --parent=%s = %s, want []: the base bead's child is a `blocks` edge, not a parent-child one", base, listed)
	}
	if parents := wispParentChildTargets(t, dir); contains(parents, base) {
		t.Errorf("a parent-child edge in wisp_dependencies names %s, so the [] above is a real blind spot rather than the graph shape", base)
	}

	if before := wispStatuses(t, dir); len(before) != 9 {
		t.Fatalf("molecule %s has %d wisps, want 9 (root + 8 steps): %v", root, len(before), before)
	}
	open, err := openDescendantIDsViaCLI(bd, dir, root)
	if err != nil {
		t.Fatalf("openDescendantIDsViaCLI(%s): %v", root, err)
	}
	if len(open) != 8 {
		t.Fatalf("open descendants of %s = %v, want the 8 step wisps", root, open)
	}

	if _, err := closeMoleculeWithDescendants(bd, dir, root); err != nil {
		t.Fatalf("closeMoleculeWithDescendants(%s): %v", root, err)
	}

	var stuck []string
	for id, status := range wispStatuses(t, dir) {
		if status != string(beads.StatusClosed) {
			stuck = append(stuck, id+" ("+status+")")
		}
	}
	if len(stuck) > 0 {
		t.Errorf("cleanup of %s left %d wisp(s) open: %s", root, len(stuck), strings.Join(stuck, ", "))
	}
}
