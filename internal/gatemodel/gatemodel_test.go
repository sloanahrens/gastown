package gatemodel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const fxModule = "example.com/fx"

// fxPkg is one `go list -json` object of a fixture graph.
type fxPkg struct {
	ImportPath   string      `json:"ImportPath"`
	Imports      []string    `json:"Imports,omitempty"`
	TestImports  []string    `json:"TestImports,omitempty"`
	XTestImports []string    `json:"XTestImports,omitempty"`
	Module       fxModuleRef `json:"Module"`
}

type fxModuleRef struct {
	Path string `json:"Path"`
	Main bool   `json:"Main"`
}

// pkg names a fixture package that imports others through Imports.
func pkg(importPath string, imports ...string) fxPkg {
	return fxPkg{ImportPath: fxModule + "/" + importPath, Imports: imports,
		Module: fxModuleRef{Path: fxModule, Main: true}}
}

// fixtureGraph parses the objects as a `go list -json` stream.
func fixtureGraph(t *testing.T, pkgs ...fxPkg) *Graph {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, p := range pkgs {
		if err := enc.Encode(p); err != nil {
			t.Fatalf("encoding fixture package: %v", err)
		}
	}
	g, err := ParseGraph(&buf)
	if err != nil {
		t.Fatalf("ParseGraph: %v", err)
	}
	return g
}

// hasGo reports a fixture directory as a package directory exactly when the
// graph holds it, as land.PackageDirHasGo reads the working tree.
func hasGo(g *Graph) func(string) bool {
	dirs := map[string]bool{}
	for _, d := range g.Dirs() {
		dirs[d] = true
	}
	return func(dir string) bool { return dirs[dir] }
}

// status renders paths as `git diff --name-status -z` records.
func status(paths ...string) string {
	var b strings.Builder
	for _, p := range paths {
		b.WriteString("M\x00" + p + "\x00")
	}
	return b.String()
}

func TestGateModelFindsDependentsIncludingTestImports(t *testing.T) {
	t.Parallel()
	g := fixtureGraph(t,
		pkg("internal/a"),
		pkg("internal/b", fxModule+"/internal/a"),
		fxPkg{ImportPath: fxModule + "/internal/c", TestImports: []string{fxModule + "/internal/a"},
			Module: fxModuleRef{Path: fxModule, Main: true}},
		fxPkg{ImportPath: fxModule + "/internal/d", XTestImports: []string{fxModule + "/internal/a"},
			Module: fxModuleRef{Path: fxModule, Main: true}},
		pkg("internal/e"),
		pkg("internal/f", fxModule+"/internal/b"),
	)

	sel := g.Select(status("internal/a/a.go"), hasGo(g), nil)

	wantDependents := []string{"internal/b", "internal/c", "internal/d", "internal/f"}
	if !slices.Equal(sel.Dependents, wantDependents) {
		t.Errorf("dependents = %v, want %v (build, test and external-test importers, transitively)", sel.Dependents, wantDependents)
	}
	if !slices.Equal(sel.Changed, []string{"internal/a"}) {
		t.Errorf("changed = %v, want [internal/a]", sel.Changed)
	}
	if slices.Contains(sel.Packages, "internal/e") {
		t.Errorf("packages = %v, must not hold internal/e, which neither changed nor imports the change", sel.Packages)
	}
	for _, want := range wantDependents {
		if !slices.Contains(sel.Packages, want) {
			t.Errorf("packages = %v, want %s in the modeled run", sel.Packages, want)
		}
	}
	if sel.All {
		t.Errorf("a single-package change must not fall back to the full run: %s", sel.Reason)
	}
}

func TestGateModelFallsBackToTheFullRun(t *testing.T) {
	t.Parallel()
	g := fixtureGraph(t,
		pkg("internal/a"),
		pkg("internal/c"),
		pkg("internal/land"),
	)
	everyPackage := g.Dirs()

	tests := []struct {
		name    string
		path    string
		nested  []string
		wantWhy string
	}{
		{name: "go.mod", path: "go.mod", wantWhy: "go.mod"},
		{name: "go.sum", path: "go.sum", wantWhy: "go.sum"},
		{name: "go.work", path: "go.work", wantWhy: "go.work"},
		{name: "Makefile", path: "Makefile", wantWhy: "Makefile"},
		{name: "scripts", path: "scripts/gate.sh", wantWhy: "scripts/gate.sh"},
		{name: "ci config", path: ".forgejo/workflows/gate.yml", wantWhy: ".forgejo/workflows/gate.yml"},
		{name: "deploy", path: "deploy/polecat/policy.yaml", wantWhy: "deploy/polecat/policy.yaml"},
		{
			name: "nested module", path: "gt-model-eval/main.go", nested: []string{"gt-model-eval"},
			wantWhy: "nested module gt-model-eval",
		},
		{
			name: "unplaceable path", path: "internal/empty/notes.txt",
			wantWhy: "unplaceable path internal/empty/notes.txt",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sel := g.Select(status(tt.path), hasGo(g), tt.nested)
			if !sel.All {
				t.Fatalf("%s must force the full run, got packages %v", tt.path, sel.Packages)
			}
			if sel.Reason != tt.wantWhy {
				t.Errorf("reason = %q, want %q", sel.Reason, tt.wantWhy)
			}
			if !slices.Equal(sel.Packages, everyPackage) {
				t.Errorf("packages = %v, want every package %v", sel.Packages, everyPackage)
			}
		})
	}

	t.Run("unreadable diff", func(t *testing.T) {
		t.Parallel()
		// Tab-separated name-status: what `git diff` prints without -z.
		sel := g.Select("M\tinternal/a/a.go\n", hasGo(g), nil)
		if !sel.All || !strings.HasPrefix(sel.Reason, "unreadable diff: ") {
			t.Fatalf("unreadable diff = %+v, want the full run with the reason", sel)
		}
		if !slices.Equal(sel.Packages, everyPackage) {
			t.Errorf("packages = %v, want every package %v", sel.Packages, everyPackage)
		}
	})

	t.Run("unreadable graph", func(t *testing.T) {
		t.Parallel()
		var nilGraph *Graph
		sel := nilGraph.Select(status("internal/a/a.go"), hasGo(g), nil)
		if !sel.All {
			t.Fatalf("a nil graph must select the full run, never nothing: %+v", sel)
		}
		if sel.Reason != "unreadable graph" {
			t.Errorf("reason = %q, want %q", sel.Reason, "unreadable graph")
		}
	})

	t.Run("unreadable landing diff", func(t *testing.T) {
		t.Parallel()
		landings := []Landing{{SHA: strings.Repeat("a", 40), Subject: "land: x", Unreadable: "git diff failed"}}
		rep, err := Build(landings, g, hasGo(g), nil, nil, 1)
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		got := rep.Landings[0]
		if got.Fallback == "" || !slices.Equal(got.Packages, everyPackage) {
			t.Errorf("landing = %+v, want the full run with the reason", got)
		}
	})
}

func TestGateModelAlwaysRunSet(t *testing.T) {
	t.Parallel()
	g := fixtureGraph(t,
		pkg("internal/a"),
		pkg("internal/land"),
	)
	wantAlways := []string{"internal/guardlint", "internal/makegate", "internal/testpolicy"}

	t.Run("docs only", func(t *testing.T) {
		t.Parallel()
		sel := g.Select(status("docs/reference.md", "README.md"), hasGo(g), nil)
		if !slices.Equal(sel.Packages, wantAlways) {
			t.Errorf("packages = %v, want exactly the always-run set %v", sel.Packages, wantAlways)
		}
		if len(sel.Changed) != 0 || len(sel.Dependents) != 0 {
			t.Errorf("changed = %v, dependents = %v, want none", sel.Changed, sel.Dependents)
		}
	})

	t.Run("changed package keeps always-run", func(t *testing.T) {
		t.Parallel()
		sel := g.Select(status("internal/a/a.go"), hasGo(g), nil)
		for _, want := range wantAlways {
			if !slices.Contains(sel.Packages, want) {
				t.Errorf("packages = %v, want always-run %s", sel.Packages, want)
			}
		}
	})

	t.Run("guard input adds its package", func(t *testing.T) {
		t.Parallel()
		sel := g.Select(status(".om.json"), hasGo(g), nil)
		want := []string{"internal/guardlint", "internal/land", "internal/makegate", "internal/testpolicy"}
		if !slices.Equal(sel.Packages, want) {
			t.Errorf("packages = %v, want %v: .om.json is an input of internal/land's rubric guards", sel.Packages, want)
		}
		if sel.All {
			t.Errorf("a guard input must not fall back to the full run: %s", sel.Reason)
		}
	})
}

func TestGateModelReadsTheLogsAndPrintsTheSummary(t *testing.T) {
	t.Parallel()

	var pkgs []fxPkg
	pkgs = append(pkgs,
		pkg("internal/a"),
		pkg("internal/m", fxModule+"/internal/a"),
		pkg("internal/nocost", fxModule+"/internal/a"),
		pkg("internal/guardlint"),
		pkg("internal/makegate"),
		pkg("internal/testpolicy"),
	)
	for i := 0; i < 10; i++ {
		pkgs = append(pkgs, pkg(fmt.Sprintf("internal/light%02d", i)))
	}
	for i := 0; i < 60; i++ {
		pkgs = append(pkgs, pkg(fmt.Sprintf("internal/hub%02d", i)))
	}
	g := fixtureGraph(t, pkgs...)

	baseline := []string{
		"tierwall: example.com/fx/internal/a 10.0s",
		"tierwall: example.com/fx/internal/m 20.0s",
		"tierwall: example.com/fx/internal/guardlint 1.0s",
		"tierwall: example.com/fx/internal/makegate 1.0s",
		"tierwall: example.com/fx/internal/testpolicy 1.0s",
	}
	for i := 0; i < 10; i++ {
		baseline = append(baseline, fmt.Sprintf("tierwall: example.com/fx/internal/light%02d 4.0s", i))
	}
	for i := 0; i < 60; i++ {
		baseline = append(baseline, fmt.Sprintf("tierwall: example.com/fx/internal/hub%02d 0.5s", i))
	}

	shaA := strings.Repeat("a", 40)
	shaB := strings.Repeat("b", 40)
	shaH := strings.Repeat("c", 40)

	var light, hub []string
	for i := 0; i < 10; i++ {
		light = append(light, fmt.Sprintf("internal/light%02d/main.go", i))
	}
	for i := 0; i < 60; i++ {
		hub = append(hub, fmt.Sprintf("internal/hub%02d/main.go", i))
	}
	landings := []Landing{
		{SHA: shaA, Subject: "fix: a", NameStatus: status("internal/a/a.go")},
		{SHA: shaB, Subject: "fix: light", NameStatus: status(light...)},
		{SHA: shaH, Subject: "fix: hub", NameStatus: status(hub...)},
	}

	logs := []Log{
		mustParseLog(t, shortSHA(shaA)+".log", g,
			"tierwall: example.com/fx/internal/a 8.0s",
			"tierwall: tier 56.0s bar 90s OK"),
		mustParseLog(t, "baseline.log", g, baseline...),
	}

	rep, err := Build(landings, g, hasGo(g), nil, logs, 1)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if rep.Summary.Landings != 3 || rep.Summary.Fallbacks != 0 {
		t.Errorf("summary = %+v, want 3 landings and no fallback", rep.Summary)
	}

	// package walls: a=8 in its own log (10 is the max elsewhere), m=20 from
	// the log that lists it while A's omits it, nocost=0 and noted.
	a := landingReport(t, rep, shaA)
	if !a.HasLog || a.LoggedWall != 56*time.Second {
		t.Errorf("logged wall = %v (has log %v), want the tier line's 56s", a.LoggedWall, a.HasLog)
	}
	if !a.ModeledKnown || a.ModeledWall != 31*time.Second {
		t.Errorf("modeled wall = %v, want 31s: max(20, (8+20+0+1+1+1)/1)", a.ModeledWall)
	}
	if !slices.Equal(a.MissingCosts, []string{"internal/nocost"}) {
		t.Errorf("missing costs = %v, want [internal/nocost]", a.MissingCosts)
	}

	b := landingReport(t, rep, shaB)
	if b.HasLog {
		t.Error("landing B has a log")
	}
	if b.ModeledWall != 43*time.Second {
		t.Errorf("modeled wall = %v, want 43s: the sum branch, (40+1+1+1)/1", b.ModeledWall)
	}

	h := landingReport(t, rep, shaH)
	if !h.Hub {
		t.Errorf("landing H selects %d packages, want a hub (>=%d)", len(h.Packages), HubPackages)
	}
	if h.ModeledWall != 33*time.Second {
		t.Errorf("modeled wall = %v, want 33s: (30+1+1+1)/1", h.ModeledWall)
	}

	// Full run: (10+20+40+30+3)/1 = 103s; its longest package is 20s.
	if want, got := 1-31.0/103, a.Saving; !closeEnough(got, want) {
		t.Errorf("landing A saving = %v, want %v", got, want)
	}
	if want, got := 70.0/103, rep.Summary.MedianSaving; !closeEnough(got, want) {
		t.Errorf("median saving = %v, want %v", got, want)
	}
	if want, got := 72.0/103, rep.Summary.P90Saving; !closeEnough(got, want) {
		t.Errorf("p90 saving = %v, want %v", got, want)
	}
	if rep.Summary.Hubs != 1 {
		t.Errorf("hubs = %d, want 1", rep.Summary.Hubs)
	}
	if !slices.Equal(rep.Summary.MissingCosts, []string{"internal/nocost"}) {
		t.Errorf("summary missing costs = %v, want [internal/nocost]", rep.Summary.MissingCosts)
	}

	var text bytes.Buffer
	if err := RenderText(&text, rep); err != nil {
		t.Fatalf("RenderText: %v", err)
	}
	for _, want := range []string{Model, "median saving 68.0%", "1 hub", "internal/nocost in no log"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("report text does not hold %q:\n%s", want, text.String())
		}
	}

	data, err := JSONReport(rep)
	if err != nil {
		t.Fatalf("JSONReport: %v", err)
	}
	var decoded struct {
		Model    string `json:"model"`
		Landings []struct {
			SHA          string   `json:"sha"`
			ModeledWallS float64  `json:"modeled_wall_s"`
			LoggedWallS  float64  `json:"logged_wall_s"`
			MissingCosts []string `json:"missing_costs"`
		} `json:"landings"`
		Summary struct {
			MedianSaving float64 `json:"median_saving"`
			Hubs         int     `json:"hubs"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decoding the JSON report: %v", err)
	}
	if decoded.Model != Model || len(decoded.Landings) != 3 || decoded.Summary.Hubs != 1 {
		t.Errorf("JSON report = %+v, want the model, 3 landings and 1 hub", decoded)
	}
	if decoded.Landings[0].SHA != shaA || decoded.Landings[0].ModeledWallS != 31 ||
		decoded.Landings[0].LoggedWallS != 56 || !slices.Equal(decoded.Landings[0].MissingCosts, []string{"internal/nocost"}) {
		t.Errorf("JSON landing = %+v, want 31s modeled, 56s logged, nocost missing", decoded.Landings[0])
	}
	if !closeEnough(decoded.Summary.MedianSaving, 70.0/103) {
		t.Errorf("JSON median saving = %v, want %v", decoded.Summary.MedianSaving, 70.0/103)
	}
}

// TestGateModelWallIsTheLongerBranch pins the formula's other arm: with two
// cpus, a selection whose sum halves falls back to its longest package.
func TestGateModelWallIsTheLongerBranch(t *testing.T) {
	t.Parallel()
	g := fixtureGraph(t,
		pkg("internal/a"),
		pkg("internal/m", fxModule+"/internal/a"),
	)
	logs := []Log{mustParseLog(t, "base.log", g,
		"tierwall: example.com/fx/internal/a 8.0s",
		"tierwall: example.com/fx/internal/m 20.0s")}
	landings := []Landing{{SHA: strings.Repeat("a", 40), Subject: "fix: a", NameStatus: status("internal/a/a.go")}}

	rep, err := Build(landings, g, hasGo(g), nil, logs, 2)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Selection: a, m and the three always-run packages at 0s.
	// max(20, 28/2) = 20.
	if got := rep.Landings[0].ModeledWall; got != 20*time.Second {
		t.Errorf("modeled wall = %v, want 20s: max(20, 28/2)", got)
	}
}

// mustParseLog parses lines as a saved gate log.
func mustParseLog(t *testing.T, name string, g *Graph, lines ...string) Log {
	t.Helper()
	l, err := ParseLog(name, strings.NewReader(strings.Join(lines, "\n")+"\n"), g)
	if err != nil {
		t.Fatalf("ParseLog(%s): %v", name, err)
	}
	return l
}

// landingReport returns the report for sha.
func landingReport(t *testing.T, rep Report, sha string) LandingReport {
	t.Helper()
	for _, l := range rep.Landings {
		if l.SHA == sha {
			return l
		}
	}
	t.Fatalf("no landing %s in the report", sha)
	return LandingReport{}
}

// closeEnough compares two saving fractions within a rounding error.
func closeEnough(got, want float64) bool {
	return math.Abs(got-want) < 1e-9
}

// TestGateModelRefusesUnusableInputs drives every refusal branch: a log
// directory, a log and a cpu count the model cannot work from fail, rather
// than producing a report of zeros.
func TestGateModelRefusesUnusableInputs(t *testing.T) {
	t.Parallel()
	g := fixtureGraph(t, pkg("internal/a"))

	t.Run("missing log directory", func(t *testing.T) {
		t.Parallel()
		if _, err := ParseLogs(filepath.Join(t.TempDir(), "absent"), g); err == nil {
			t.Fatal("ParseLogs of a missing directory = nil error, want a refusal")
		}
	})

	t.Run("empty log directory", func(t *testing.T) {
		t.Parallel()
		if _, err := ParseLogs(t.TempDir(), g); err == nil {
			t.Fatal("ParseLogs of a directory with no log = nil error, want a refusal")
		}
	})

	t.Run("log directory holds a subdirectory", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "nested"), 0o755); err != nil {
			t.Fatalf("creating the subdirectory: %v", err)
		}
		if _, err := ParseLogs(dir, g); err == nil {
			t.Fatal("ParseLogs of a directory holding a subdirectory = nil error, want a refusal")
		}
	})

	t.Run("unreadable log", func(t *testing.T) {
		t.Parallel()
		if _, err := ParseLog("broken.log", failingReader{}, g); err == nil {
			t.Fatal("ParseLog of an unreadable log = nil error, want a refusal")
		}
	})

	t.Run("cpus below one", func(t *testing.T) {
		t.Parallel()
		if _, err := Build(nil, g, hasGo(g), nil, nil, 0); err == nil {
			t.Fatal("Build with 0 cpus = nil error, want a refusal")
		}
	})
}

// failingReader is a log that cannot be read.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("log read failed") }

// TestGateModelRefusesAnUnusableGraph drives ParseGraph's refusals: a stream it
// cannot decode, or one that does not name exactly one main module with a path,
// is an error, so the caller models the full run instead of an empty graph.
func TestGateModelRefusesAnUnusableGraph(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		stream string
	}{
		{name: "undecodable stream", stream: `{"ImportPath": "example.com/fx/internal/a"`},
		{name: "no main-module package", stream: `{"ImportPath":"fmt","Module":{"Path":"go","Main":false}}`},
		{name: "main module with no path", stream: `{"ImportPath":"example.com/fx/internal/a","Module":{"Path":"","Main":true}}`},
		{name: "two main modules", stream: `{"ImportPath":"example.com/fx/internal/a","Module":{"Path":"example.com/fx","Main":true}}
{"ImportPath":"other.test/b","Module":{"Path":"other.test","Main":true}}`},
		{name: "package outside its module", stream: `{"ImportPath":"example.com/fxother/internal/a","Module":{"Path":"example.com/fx","Main":true}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseGraph(strings.NewReader(tt.stream)); err == nil {
				t.Fatal("ParseGraph = nil error, want a refusal")
			}
		})
	}
}
