package cmd

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/session"
)

// The sling bd-call counter (gt-h3bka5): a spec sling's trace charges each bd
// call to the step that spends it, and the note line prints the counts. The
// tests below drive a real sling through the engine with the production bd
// call sites kept and only the mechanisms that reach outside the process
// faked, so the counts come from the code a sling actually runs.

// slingCallTown lays out a town with one routed rig, so the sling's bead,
// target-rig and duplicate reads resolve without a live database.
func slingCallTown(t *testing.T) (townRoot, rigBeads string) {
	t.Helper()
	townRoot = t.TempDir()
	townBeads := filepath.Join(townRoot, ".beads")
	rigBeads = filepath.Join(townRoot, "gastown", "mayor", "rig", ".beads")
	for _, dir := range []string{townBeads, rigBeads} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	routes := `{"prefix": "hq-", "path": "."}
{"prefix": "gt-", "path": "gastown/mayor/rig"}
`
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte(routes), 0o644); err != nil {
		t.Fatalf("write routes: %v", err)
	}
	return townRoot, rigBeads
}

// slingCallBD is the bd seam a counted sling runs against: it answers the reads
// and writes a sling of one bead makes, and records every call the wrapper made
// for its caller (the --allow-stale capability probe is recorded separately,
// because it is not a call the sling spends).
type slingCallBD struct {
	mu       sync.Mutex
	title    string
	status   string
	assignee string
	calls    [][]string
	probes   int
}

func newSlingCallBD(title string) *slingCallBD {
	return &slingCallBD{title: title, status: "open"}
}

func (f *slingCallBD) run(_ context.Context, args []string) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(args) == 2 && args[0] == "--allow-stale" && args[1] == "version" {
		f.probes++
		// A non-empty answer without "unknown flag" reads as support for the
		// flag, so the call keeps the argv a real bd would see.
		return []byte("[]"), nil, nil
	}
	args = slices.DeleteFunc(slices.Clone(args), func(a string) bool { return a == "--allow-stale" })
	f.calls = append(f.calls, args)
	verb := ""
	if len(args) > 0 {
		verb = args[0]
	}
	switch verb {
	case "show":
		issue := beads.Issue{ID: args[1], Title: f.title, Status: f.status, Assignee: f.assignee}
		out, err := json.Marshal([]beads.Issue{issue})
		if err != nil {
			return nil, nil, err
		}
		return out, nil, nil
	case "list":
		return []byte("[]"), nil, nil
	case "update":
		for _, a := range args[2:] {
			if v, ok := strings.CutPrefix(a, "--status="); ok {
				f.status = v
			}
			if v, ok := strings.CutPrefix(a, "--assignee="); ok {
				f.assignee = v
			}
		}
		return []byte("{}"), nil, nil
	}
	return []byte("[]"), nil, nil
}

// recorded is every call the wrapper made for the sling, in order.
func (f *slingCallBD) recorded() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// slingCallDeps is the running gt's collaborators with the sling's bd stores
// bound to stores, and the mechanisms that spawn, lock and start faked so the
// test spawns nothing and touches no live town.
func slingCallDeps(t *testing.T, townRoot string, trace *session.SandboxTrace, fake *slingCallBD) *slingDeps {
	t.Helper()
	d := specSlingDeps(trace, beads.WithRunner(fake.run))
	d.out = io.Discard
	d.stdin = strings.NewReader("")
	d.findTown = func() (string, error) { return townRoot, nil }
	d.lockBead = func(string, string) (func(), error) { return func() {}, nil }
	d.lockAssignee = func(string, string) (func(), error) { return func() {}, nil }
	d.ringHold = func(string) string { return "" }
	d.estopOn = func(string, string) (bool, error) { return false, nil }
	d.rigParked = func(string, string) (bool, string) { return false, "" }
	d.agentDead = func(string) bool { return false }
	d.releaseSeat = func(*SpawnedPolecatInfo) {}
	d.spawnPolecat = func(rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
		return &SpawnedPolecatInfo{
			RigName:       rigName,
			PolecatName:   "Toast",
			ClonePath:     filepath.Join(townRoot, rigName, "polecats", "Toast"),
			SessionName:   "gt-" + rigName + "-p-Toast",
			HookBead:      opts.HookBead,
			FreshSpawn:    true,
			BranchCreated: true,
		}, nil
	}
	d.startSession = func(*SpawnedPolecatInfo) (string, error) { return "%1", nil }
	d.logFeed = func(string, string, map[string]interface{}) error { return nil }
	return d
}

// slingCallParams is one bead dispatched to the town's rig with the formula
// off, so the trace counts the dispatch's own reads and writes.
func slingCallParams(townRoot, beadID string, trace *session.SandboxTrace) SlingParams {
	return SlingParams{
		BeadID:           beadID,
		RigName:          "gastown",
		TownRoot:         townRoot,
		BeadsDir:         filepath.Join(townRoot, ".beads"),
		Agent:            "gastown/polecats/Toast",
		HookRawBead:      true,
		NoBoot:           true,
		CallerContext:    "test",
		FormulaFailFatal: true,
		Steps:            trace.Steps(),
	}
}

// slingNoteBDCounts reads the per-step bd counts out of a sling note's summary.
// The summary is comma-separated one level deep, and a count's own commas are
// inside its bracket.
func slingNoteBDCounts(t *testing.T, summary string) (perStep map[string]int, total int) {
	t.Helper()
	perStep = map[string]int{}
	count := regexp.MustCompile(`\[bd (\d+), git \d+\]`)
	for _, part := range splitTopLevelParts(summary) {
		m := count.FindStringSubmatch(part)
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("parsing %q: %v", part, err)
		}
		name := strings.TrimSpace(part[:strings.Index(part, " [bd ")])
		// The name is everything before its duration, the last token.
		if i := strings.LastIndexByte(name, ' '); i > 0 {
			name = name[:i]
		}
		perStep[name] += n
		total += n
	}
	return perStep, total
}

// splitTopLevelParts splits s on the commas outside brackets.
func splitTopLevelParts(s string) []string {
	var parts []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '[':
			depth++
		case ']':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	return append(parts, strings.TrimSpace(s[start:]))
}

// TestSlingNoteShowsBDCallsPerStep: a sling's note line carries, per step, the
// bd calls that step made. Every call the sling spent appears in the line
// exactly once, and the duplicate check's own two pool reads land on its step.
func TestSlingNoteShowsBDCallsPerStep(t *testing.T) {
	t.Parallel()
	townRoot, _ := slingCallTown(t)
	fake := newSlingCallBD("fix TestSlingNoteShowsBDCallsPerStep in internal/cmd/sling.go")
	trace := session.NewSandboxTrace(time.Now)
	d := slingCallDeps(t, townRoot, trace, fake)

	result, err := d.executeSling(slingCallParams(townRoot, "gt-call1", trace))
	if err != nil {
		t.Fatalf("executeSling: %v", err)
	}
	if result == nil || !result.Success {
		t.Fatalf("result = %+v, want success", result)
	}

	summary := trace.Summary()
	if summary == "" {
		t.Fatal("the sling recorded no step, so no note line was produced")
	}
	perStep, total := slingNoteBDCounts(t, summary)
	if want := len(fake.recorded()); total != want {
		t.Fatalf("%q counts %d bd call(s), the sling made %d\ncalls: %v", summary, total, want, fake.recorded())
	}
	if total == 0 {
		t.Fatal("the sling made no bd calls, so the test proves nothing")
	}
	// The counts are per step, not one lump: the guard, the duplicate pool read
	// and the hook each carry their own.
	if got := perStep["duplicate check"]; got < 2 {
		t.Errorf("duplicate check counted %d bd call(s), want at least its two pool reads: %q", got, summary)
	}
	if len(perStep) < 3 {
		t.Errorf("only %d step(s) carry a bd count, want the dispatch's steps charged separately: %q", len(perStep), summary)
	}
	if line := specSlingTraceLine("gt-call1", "gastown/polecats/Toast", trace, nil); !strings.Contains(line, "[bd ") {
		t.Errorf("note line %q shows no bd counts", line)
	}
}

// TestSlingCallsCountSeparatelyForConcurrentSlings: two slings running together
// count their own bd calls on their own traces. A shared observer would give
// each the sum; a per-sling one gives each its own reads.
func TestSlingCallsCountSeparatelyForConcurrentSlings(t *testing.T) {
	t.Parallel()
	townRoot, _ := slingCallTown(t)
	// The first bead names a test and a file, so its duplicate check reads the
	// pool; the second names neither, so the same guard declines to run. The
	// two slings therefore differ by exactly the pool reads the first one made.
	first := newSlingCallBD("fix TestConcurrentCount in internal/cmd/sling.go")
	second := newSlingCallBD("count the calls")

	runOne := func(fake *slingCallBD, beadID string) (*session.SandboxTrace, error) {
		trace := session.NewSandboxTrace(time.Now)
		d := slingCallDeps(t, townRoot, trace, fake)
		_, err := d.executeSling(slingCallParams(townRoot, beadID, trace))
		return trace, err
	}

	type outcome struct {
		trace *session.SandboxTrace
		fake  *slingCallBD
		err   error
	}
	start := make(chan struct{})
	done := make(chan outcome, 2)
	go func() { <-start; tr, err := runOne(first, "gt-conc1"); done <- outcome{tr, first, err} }()
	go func() { <-start; tr, err := runOne(second, "gt-conc2"); done <- outcome{tr, second, err} }()
	close(start)

	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("the test needs a deadline to bound the slings")
	}
	timeout := time.NewTimer(time.Until(deadline))
	defer timeout.Stop()

	totals := map[*slingCallBD]int{}
	for i := 0; i < 2; i++ {
		select {
		case got := <-done:
			if got.err != nil {
				t.Fatalf("executeSling: %v", got.err)
			}
			_, totals[got.fake] = slingNoteBDCounts(t, got.trace.Summary())
		case <-timeout.C:
			t.Fatal("the concurrent slings did not finish before the test deadline")
		}
	}

	for fake, total := range totals {
		if want := len(fake.recorded()); total != want {
			t.Errorf("a sling counted %d bd call(s), its own calls were %d: %v", total, want, fake.recorded())
		}
	}
	// The first sling made the pool's list reads and the second made none, so
	// the difference is exactly those reads: each trace holds its own calls and
	// nothing of the other's.
	poolReads := 0
	for _, call := range first.recorded() {
		if len(call) > 0 && call[0] == "list" {
			poolReads++
		}
	}
	if poolReads == 0 {
		t.Fatal("the first sling made no pool read, so the two runs do not differ")
	}
	if firstTotal, secondTotal := totals[first], totals[second]; firstTotal != secondTotal+poolReads {
		t.Errorf("counts %d and %d, want the first ahead by its %d pool read(s)", firstTotal, secondTotal, poolReads)
	}
}

// slingBDExcludedSites names every bd construction in the sling path files that
// is deliberately not counted on a spec sling's trace, with the reason. Every
// other construction reaches bd through a sling store (slingStores.open,
// openPinned, pinnedDB, routedFrom or a caller's storeFor), which carries the
// trace's observer.
//
// The AST test below fails when a raw construction appears that this table does
// not name, and when a named one no longer exists. A site moved or removed is
// an entry to update; a site added is a decision about whether it counts.
var slingBDExcludedSites = map[string]string{
	"sling_formula.go:closeFormulaWisp":           "standalone formula sling: it builds no trace, so there is nothing to count on",
	"sling_formula.go:findHookedFormulaSingleton": "standalone formula sling: it builds no trace, so there is nothing to count on",
	"sling_pool.go:openClient":                    "the fallback for a seat read whose caller set no opener; a sling's seat reads come through open",
	"pool_seats.go:poolSeatWorkFor":               "the operator seat listing's landing-seat read; a sling's seat decision confirms landing seats through its own storeFor",
	"hook.go:runHook":                             "the operator's gt hook command, not a sling",
	"hook.go:runHookShow":                         "the operator's gt hook show command, not a sling",
}

// slingPathFiles are the files the bd constructions of a sling (and of the
// commands that share its helpers) live in. The list is the scan's scope: a
// construction outside it is a command that is not a sling.
var slingPathFiles = []string{
	"capacity_dispatch.go",
	"formula_cook.go",
	"hook.go",
	"polecat_capacity.go",
	"polecat_spawn.go",
	"polecat_work_release.go",
	"pool_seats.go",
	"sling.go",
	"sling_backpressure.go",
	"sling_batch.go",
	"sling_duplicate.go",
	"sling_engine.go",
	"sling_formula.go",
	"sling_helpers.go",
	"sling_pool.go",
	"sling_respawn.go",
	"sling_rework.go",
	"sling_rollback.go",
	"sling_run.go",
	"sling_schedule.go",
	"sling_stores.go",
	"sling_target.go",
	"spec_claim.go",
	"spec_graph.go",
}

// slingBDConstructors are the constructors the scan treats as a bd
// construction site.
var slingBDConstructors = map[string]bool{
	"New":             true,
	"NewPinned":       true,
	"NewWithBeadsDir": true,
	"NewPlain":        true,
}

// TestSlingBDSitesAreCountedOrNamed: every bd construction in the sling path
// either opens from a sling store — built with the store's construction
// options, which carry the trace's observer — or is named in
// slingBDExcludedSites with its reason. A new raw construction fails here
// rather than quietly dropping its calls off the note line.
func TestSlingCallsEveryBDSiteIsCountedOrNamed(t *testing.T) {
	t.Parallel()
	found := map[string]int{}
	for _, name := range slingPathFiles {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "beads" || !slingBDConstructors[sel.Sel.Name] {
					return true
				}
				if callPassesStoreOptions(call) {
					return true
				}
				found[name+":"+fn.Name.Name]++
				return true
			})
		}
	}

	for site := range found {
		if _, ok := slingBDExcludedSites[site]; !ok {
			t.Errorf("%s builds a bd store without a sling store's observer, and is not named in slingBDExcludedSites: open it from the sling's stores, or name it with its reason", site)
		}
	}
	for site := range slingBDExcludedSites {
		if found[site] == 0 {
			t.Errorf("slingBDExcludedSites names %s, which no longer builds a bd store there: drop the entry", site)
		}
	}
}

// callPassesStoreOptions reports whether a construction call is handed a sling
// store's construction options, which is what carries the trace's observer into
// the store it builds.
func callPassesStoreOptions(call *ast.CallExpr) bool {
	found := false
	for _, arg := range call.Args {
		ast.Inspect(arg, func(n ast.Node) bool {
			switch e := n.(type) {
			case *ast.Ident:
				if e.Name == "opts" {
					found = true
				}
			case *ast.SelectorExpr:
				if e.Sel.Name == "opts" {
					found = true
				}
			}
			return !found
		})
	}
	return found
}
