package beadsfake

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// fakeControls are the Fake's exported methods that are not part of the
// beads surface: seeding, failure injection, and the recorders for the
// calls a fake can only script (SQLCSV, InitDatabase). Each is a test
// control; none may be a bd verb a consumer reaches past Client for.
var fakeControls = map[string]bool{
	"Seed":          true,
	"DropTable":     true,
	"FailWith":      true,
	"OnSQL":         true,
	"SQLStatements": true,
	"SQLCSV":        true,
	"InitDatabase":  true,
	"Inits":         true,
}

func methodNames(t reflect.Type) map[string]bool {
	names := map[string]bool{}
	for i := 0; i < t.NumMethod(); i++ {
		names[t.Method(i).Name] = true
	}
	return names
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedVerbNames is sortedKeys for the engineVerbs allowlist.
func sortedVerbNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestFakeMethodSetMatchesClient is the one-interface rule (gt-7iwy0.4): the
// shared fake implements exactly beads.Client and beads.Admin plus its
// listed test controls. A method on the fake that is on neither interface is
// a verb consumers would reach through the concrete fake; a method on the
// interfaces the fake lacks fails to compile already.
func TestFakeMethodSetMatchesClient(t *testing.T) {
	t.Parallel()
	surface := methodNames(reflect.TypeOf((*beads.Client)(nil)).Elem())
	for name := range methodNames(reflect.TypeOf((*beads.Admin)(nil)).Elem()) {
		surface[name] = true
	}
	fake := methodNames(reflect.TypeOf((*Fake)(nil)))

	var extra, missing []string
	for _, name := range sortedKeys(fake) {
		if !surface[name] && !fakeControls[name] {
			extra = append(extra, name)
		}
	}
	for _, name := range sortedKeys(surface) {
		if !fake[name] {
			missing = append(missing, name)
		}
	}
	for _, name := range sortedKeys(fakeControls) {
		if !fake[name] {
			missing = append(missing, name+" (listed control)")
		}
		if surface[name] {
			t.Errorf("%s is on the beads surface; drop it from fakeControls", name)
		}
	}
	if len(extra) > 0 {
		t.Errorf("Fake methods on neither beads.Client nor beads.Admin: %v\n"+
			"add each to the interface (with a contract case) or list it in fakeControls", extra)
	}
	if len(missing) > 0 {
		t.Errorf("methods the Fake lacks: %v", missing)
	}
}

// TestContractCoversEveryClientMethod pins the second half of the rule: a
// Client or Admin method lands with a contract case, so the fake and bd
// are held to the same behavior on it. A method counts as covered when the
// contract sources call it.
func TestContractCoversEveryClientMethod(t *testing.T) {
	t.Parallel()
	called := map[string]bool{}
	fset := token.NewFileSet()
	for _, file := range []string{"contract.go", "admin_contract.go"} {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					called[sel.Sel.Name] = true
				}
			}
			return true
		})
	}
	surface := methodNames(reflect.TypeOf((*beads.Client)(nil)).Elem())
	for name := range methodNames(reflect.TypeOf((*beads.Admin)(nil)).Elem()) {
		surface[name] = true
	}
	var uncovered []string
	for _, name := range sortedKeys(surface) {
		if !called[name] {
			uncovered = append(uncovered, name)
		}
	}
	if len(uncovered) > 0 {
		t.Errorf("Client/Admin methods no contract case calls: %v", uncovered)
	}
}

// engineVerbs is the third and last part of the one-interface rule
// (gt-7iwy0.4): the *Beads methods that are neither Client nor Admin. Each is
// a bd verb or an engine-level operation a Client cannot express — a write
// onto the store's own actor, a raw `bd sql` read, a database-level
// migration — or a *Beads-only fast path over the preload snapshots. The
// reason field records which, and who still calls it, so that deleting one is
// a decision someone makes on the evidence rather than by grep.
//
// A method outside Client ∪ Admin ∪ engineVerbs is a domain helper that
// belongs on a free function over Client (client_helpers.go and friends), so
// the fake can run it too. Adding one here is the exception, not the fix.
var engineVerbs = map[string]string{
	"ActingAs":                     "scopes the store's actor for the writes that follow (internal/done's crew path)",
	"Blocked":                      "reports whether a bead is blocked; schedulerrun's blockedReader asks the *Beads",
	"Bond":                         "runs gt's bond formula for a bead (internal/cmd's sling path)",
	"ClearAgentActiveMRIfMatches":  "clears an agent bead's active_mr while it still names the expected MR",
	"ContainerUnavailable":         "classifies a bd failure as a container/database outage (internal/testutil)",
	"Cook":                         "cooks a formula through the engine (formula cook, doctor's overlay check)",
	"CreateAgentBead":              "creates an agent bead in its canonical database (doctor's agent-bead check)",
	"CreateProbeBead":              "creates an ephemeral, non-dispatchable probe bead; internal/beads' probe test is its caller",
	"DeleteLegacyAgentBead":        "removes a legacy agent bead (polecat identity reconcile)",
	"DemoteToWisp":                 "moves an ephemeral-flagged issue into the wisps table (doctor's misclassified-wisp repair)",
	"FindMRForBranchAny":           "finds a merge-request bead for a branch in any status (polecat recovery)",
	"ForAgentBead":                 "hands back a Client scoped to the agent-bead store, the agent write helpers' entry point",
	"FormulaShow":                  "prints a formula's body through the engine (sling formula)",
	"GetAgentBead":                 "reads an agent bead and its parsed fields, from the preload snapshot when warm",
	"GetAgentBeadInStoreOnly":      "reads an agent bead without the town-database fallback (identity reconcile)",
	"Init":                         "initializes a bd database in a directory; integration-test harnesses call it",
	"InitDatabase":                 "creates the Dolt database a rig's bead store lives in",
	"IsBeadsRepo":                  "reports whether the working directory holds a beads database",
	"ListAgentBeads":               "lists agent beads, from the preload snapshot when warm",
	"ListAgentBeadsFromWisps":      "lists the agent beads that live in the wisps table",
	"ListMergeRequests":            "lists merge-request beads from the wisp cache (internal/cmd's polecatMRLister)",
	"ListWispIDs":                  "lists wisp ids for the orphan-agent sweep (doctor)",
	"LogDetachAudit":               "appends a detach entry to the store's audit log (detachAuditLogger)",
	"MigrateRepoID":                "moves a repo's beads to the prefix's canonical database (rig manager)",
	"PreloadAgentBeads":            "warms the agent-bead snapshot; internal/beads' preload cache test is its caller",
	"PreloadBeads":                 "warms the wisps and issues snapshots from one bd sql read",
	"PreloadMergeRequests":         "warms the merge-request cache for a fleet-wide branch scan",
	"PurgeClosedEphemeral":         "deletes closed wisps past a grace period",
	"RecordReassignment":           "records a reassignment in the issue's notes (polecat reuse)",
	"ReopenUnassigned":             "returns an in_progress, unassigned issue to open (doctor repair)",
	"Run":                          "runs raw bd argv; the escape hatch, its sole caller internal/cmd's prime checklist integration test",
	"SQLCSV":                       "runs one `bd sql --json` query and returns its rows",
	"SetIssuePrefix":               "sets the database's issue prefix",
	"UpdateAgentCleanupStatus":     "one-field agent write built on UpdateAgentDescriptionFields",
	"UpdateAgentDescriptionFields": "read-modify-writes an agent bead's description fields under its lock",
	"Wisp":                         "creates a molecule wisp through the engine (sling formula)",
	"WithTimeout":                  "returns a plain wrapper whose bd calls are killed after d",
}

// TestBeadsMethodSetIsClientAdminAndEngine is the *Beads half of the
// one-interface rule: the concrete store exposes Client and Admin plus the
// named engine verbs above, and nothing else. Any other exported method is a
// wrapper whose free version already exists — move its callers to the free
// function and delete it, rather than teaching consumers a second name for
// the same operation.
func TestBeadsMethodSetIsClientAdminAndEngine(t *testing.T) {
	t.Parallel()
	surface := methodNames(reflect.TypeOf((*beads.Client)(nil)).Elem())
	for name := range methodNames(reflect.TypeOf((*beads.Admin)(nil)).Elem()) {
		surface[name] = true
	}
	methods := methodNames(reflect.TypeOf((*beads.Beads)(nil)))

	var unlisted []string
	for _, name := range sortedKeys(methods) {
		if !surface[name] && engineVerbs[name] == "" {
			unlisted = append(unlisted, name)
		}
	}
	if len(unlisted) > 0 {
		t.Errorf("*Beads methods on neither Client, Admin nor engineVerbs: %v\n"+
			"call the free function over Client instead of adding an engine verb", unlisted)
	}

	var missing, redundant []string
	for _, name := range sortedVerbNames(engineVerbs) {
		switch {
		case surface[name]:
			redundant = append(redundant, name)
		case !methods[name]:
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("engineVerbs entries *Beads no longer has: %v\n"+
			"drop them, and say in the commit why the last caller went", missing)
	}
	if len(redundant) > 0 {
		t.Errorf("engineVerbs entries that are on Client or Admin: %v\n"+
			"drop them from engineVerbs; the interface list is where they belong", redundant)
	}
}
