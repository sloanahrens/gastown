// Package gatemodel measures a selective landing gate without changing it
// (gt-gg9hv.71). For each landing it recomputes which packages a gate that ran
// the changed packages and their dependents would have tested, then compares
// the modeled unit-tier wall with the wall a saved gate job log reports.
//
// Its inputs are saved gate job logs, the landing commits' diffs, and the
// repository's `go list` import graph. It calls no network, writes nothing
// but a report, and no caller may use it to skip a test: the gate it models is
// hypothetical until an operator decides otherwise.
package gatemodel

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// AlwaysRun are the packages every landing's gate runs whatever the landing
// changed: the tree-wide policy and guard suites, and the landing gate's own
// checks. land.GuardSelects adds the guard packages a landing's paths trigger.
var AlwaysRun = []string{"internal/guardlint", "internal/makegate", "internal/testpolicy"}

// HubPackages is the selection size at which a landing is a hub: it selects
// this many packages or more, and so saves little.
const HubPackages = 60

// ShortSHA is the length of the short sha in a report.
const ShortSHA = 12

// Model names the wall formula, for the report and the command's help.
const Model = "max(longest selected package, sum of package walls / cpus)"

// Package is one package of the gated tree.
type Package struct {
	// Dir is the package directory relative to the repository root,
	// "internal/land".
	Dir string
	// ImportPath is the package's full import path.
	ImportPath string
	// Imports are the in-module package directories it imports directly,
	// through Imports, TestImports and XTestImports alike: a change to a
	// package a test imports can change what that test verifies.
	Imports []string
}

// Graph is the repository's import graph, as `go list -json ./...` reports it.
type Graph struct {
	// Module is the main module's path.
	Module string
	// Packages are the main module's packages, sorted by Dir.
	Packages []Package

	dirOf     map[string]string
	importers map[string][]string
}

// goListPackage is the part of one `go list -json` object the graph reads.
type goListPackage struct {
	ImportPath   string
	Imports      []string
	TestImports  []string
	XTestImports []string
	Module       *goListModule
}

// goListModule is the module a listed package belongs to.
type goListModule struct {
	Path string
	Main bool
}

// ParseGraph reads a `go list -json` stream, keeping the main module's
// packages. A stream it cannot decode, or one with no main-module package, is
// an error: the caller then models the full run rather than an empty one.
func ParseGraph(r io.Reader) (*Graph, error) {
	dec := json.NewDecoder(r)
	var listed []goListPackage
	for {
		var p goListPackage
		err := dec.Decode(&p)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading the go list stream: %w", err)
		}
		if p.Module == nil || !p.Module.Main {
			continue
		}
		listed = append(listed, p)
	}
	if len(listed) == 0 {
		return nil, errors.New("the go list stream names no main-module package")
	}
	module := listed[0].Module.Path
	if module == "" {
		return nil, errors.New("the go list stream names a main module with no path")
	}
	for _, p := range listed {
		if p.Module.Path != module {
			return nil, fmt.Errorf("the go list stream names more than one main module (%q and %q)", module, p.Module.Path)
		}
	}

	g := &Graph{Module: module, dirOf: map[string]string{}, importers: map[string][]string{}}
	dirs := make([]string, len(listed))
	for i, p := range listed {
		dir, ok := Dir(module, p.ImportPath)
		if !ok {
			return nil, fmt.Errorf("package %q is outside module %q", p.ImportPath, module)
		}
		dirs[i] = dir
		g.dirOf[p.ImportPath] = dir
	}
	for i, p := range listed {
		dir := dirs[i]
		seen := map[string]bool{}
		var imports []string
		for _, list := range [][]string{p.Imports, p.TestImports, p.XTestImports} {
			for _, imp := range list {
				dep, ok := g.dirOf[imp]
				if !ok || seen[dep] || dep == dir {
					continue
				}
				seen[dep] = true
				imports = append(imports, dep)
			}
		}
		sort.Strings(imports)
		g.Packages = append(g.Packages, Package{Dir: dir, ImportPath: p.ImportPath, Imports: imports})
	}
	sort.Slice(g.Packages, func(i, j int) bool { return g.Packages[i].Dir < g.Packages[j].Dir })
	for _, p := range g.Packages {
		for _, dep := range p.Imports {
			g.importers[dep] = append(g.importers[dep], p.Dir)
		}
	}
	return g, nil
}

// Dir maps an in-module import path to its package directory relative to the
// repository root.
func Dir(module, importPath string) (string, bool) {
	if importPath == module {
		return ".", true
	}
	rest, ok := strings.CutPrefix(importPath, module+"/")
	if !ok || rest == "" {
		return "", false
	}
	return rest, true
}

// Dirs returns every package directory, sorted.
func (g *Graph) Dirs() []string {
	if g == nil {
		return nil
	}
	dirs := make([]string, 0, len(g.Packages))
	for _, p := range g.Packages {
		dirs = append(dirs, p.Dir)
	}
	return dirs
}

// Landing is one landing commit the model reads.
type Landing struct {
	// SHA is the landing commit's full sha.
	SHA string
	// Subject is the commit's one-line subject.
	Subject string
	// NameStatus is `git diff --name-status -z -M <sha>^ <sha>` output.
	NameStatus string
	// Unreadable names why the diff could not be read. The model then falls
	// back to the full run; an unreadable diff never selects nothing.
	Unreadable string
}

// Selection is the packages the modeled gate would run for one landing.
type Selection struct {
	// Changed are the packages the landing's diff names, sorted.
	Changed []string
	// Dependents are the packages that import a changed package, directly or
	// through another dependent, sorted. AlwaysRun is not in this closure.
	Dependents []string
	// AlwaysRun are the packages every gate run includes, sorted.
	AlwaysRun []string
	// Packages is the modeled run set, sorted.
	Packages []string
	// All is set when the model falls back to the full run: a tree-wide input
	// changed, a path could not be placed, or an input could not be read.
	All bool
	// Reason names what forced the fallback, and is empty otherwise.
	Reason string
}

// Select computes the modeled gate selection for one landing's name-status
// output. It never fails: a diff, guard set or graph it cannot read becomes
// the full run with the reason named, because a selection that fell open to
// nothing would report a saving the gate would not deliver.
func (g *Graph) Select(nameStatus string, hasGo func(dir string) bool, nested []string) Selection {
	if g == nil {
		return g.fullSelection("unreadable graph")
	}
	paths, err := land.ChangedPaths(nameStatus)
	if err != nil {
		return g.fullSelection("unreadable diff: " + err.Error())
	}
	for _, p := range paths {
		if dir, ok := underNestedModule(p, nested); ok {
			return g.fullSelection("nested module " + dir)
		}
		if isFallbackPath(p) {
			return g.fullSelection(p)
		}
	}
	changed, err := land.ChangedPackages(nameStatus, hasGo)
	if err != nil {
		return g.fullSelection("unreadable diff: " + err.Error())
	}
	for _, p := range paths {
		if !underGateTree(p) {
			continue
		}
		// One record per path, so a path the package rules cannot place is
		// visible instead of being absorbed by another path's package.
		dirs, err := land.ChangedPackages(singleChange(p), hasGo)
		if err != nil {
			return g.fullSelection("unreadable diff: " + err.Error())
		}
		if len(dirs) == 0 {
			return g.fullSelection("unplaceable path " + p)
		}
	}
	guards, err := land.GuardSelects(nameStatus, hasGo)
	if err != nil {
		return g.fullSelection("unreadable diff: " + err.Error())
	}
	always := alwaysRunSet(guards)

	selected := map[string]bool{}
	for _, d := range changed {
		selected[d] = true
	}
	queue := append([]string(nil), changed...)
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		for _, importer := range g.importers[dir] {
			if !selected[importer] {
				selected[importer] = true
				queue = append(queue, importer)
			}
		}
	}
	var dependents []string
	for d := range selected {
		if !contains(changed, d) {
			dependents = append(dependents, d)
		}
	}
	sort.Strings(dependents)

	set := map[string]bool{}
	for _, d := range changed {
		set[d] = true
	}
	for _, d := range dependents {
		set[d] = true
	}
	for _, d := range always {
		set[d] = true
	}
	return Selection{
		Changed:    sortedCopy(changed),
		Dependents: dependents,
		AlwaysRun:  always,
		Packages:   sortedSet(set),
	}
}

// fullSelection is the fallback: every package in the graph runs.
func (g *Graph) fullSelection(reason string) Selection {
	return Selection{AlwaysRun: sortedCopy(AlwaysRun), Packages: g.Dirs(), All: true, Reason: reason}
}

// isFallbackPath reports whether a change to p forces the full run: a module
// or build file, the CI config, the scripts, the deploy tree, or the Makefile.
func isFallbackPath(p string) bool {
	switch p {
	case "go.mod", "go.sum", "go.work", "Makefile":
		return true
	}
	return strings.HasPrefix(p, "scripts/") || strings.HasPrefix(p, ".forgejo/") ||
		strings.HasPrefix(p, "deploy/")
}

// underNestedModule reports whether p lies in one of the repository's nested
// modules, and names it. `go test ./...` from the root does not build them, so
// the model cannot place such a change.
func underNestedModule(p string, nested []string) (string, bool) {
	for _, dir := range nested {
		if dir == "" || dir == "." {
			continue
		}
		if p == dir || strings.HasPrefix(p, dir+"/") {
			return dir, true
		}
	}
	return "", false
}

// underGateTree reports whether p is a path the selection rules claim to
// place: a file under internal/ or cmd/. A path there that names no package
// is unplaceable, and the model falls back rather than dropping it.
func underGateTree(p string) bool {
	return strings.HasPrefix(p, "internal/") || strings.HasPrefix(p, "cmd/")
}

// singleChange renders p as one name-status record.
func singleChange(p string) string {
	return "M\x00" + p + "\x00"
}

// alwaysRunSet is the fixed always-run list plus the guard packages the
// landing's changed paths trigger.
func alwaysRunSet(guards []land.GuardSelect) []string {
	set := map[string]bool{}
	for _, p := range AlwaysRun {
		set[p] = true
	}
	for _, g := range guards {
		set[g.Package] = true
	}
	return sortedSet(set)
}

// Log is one saved gate job log.
type Log struct {
	// Name is the log's base name.
	Name string
	// Costs maps each package directory the log's tierwall lines report to
	// its wall in seconds. A package whose import path is not in the graph is
	// left out.
	Costs map[string]float64
	// Tier is the wall on the log's `tierwall: tier` line in seconds.
	Tier float64
	// HasTier says the log carries a tier line.
	HasTier bool
	// Total is the sum of the package walls the log lists, in seconds: the
	// tier's wall when the log carries no tier line.
	Total float64
	// Packages is how many of the log's package lines name a package in the
	// graph.
	Packages int
}

// Matches reports whether the log belongs to the landing sha: its base name
// without extension is the full sha or the short one.
func (l Log) Matches(sha string) bool {
	base := strings.TrimSuffix(filepath.Base(l.Name), filepath.Ext(l.Name))
	if base == sha {
		return true
	}
	short := sha
	if len(short) > ShortSHA {
		short = short[:ShortSHA]
	}
	return short != "" && base == short
}

// ParseLog reads one gate job log: `tierwall: <importpath> <seconds>` lines
// give per-package walls and `tierwall: tier <seconds> bar ...` gives the
// tier's own wall. A line it does not understand is skipped; a read error is
// returned, because a log half-read would understate costs.
func ParseLog(name string, r io.Reader, g *Graph) (Log, error) {
	l := Log{Name: filepath.Base(name), Costs: map[string]float64{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "tierwall: ")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 2 {
			continue
		}
		if fields[0] == "tier" {
			if secs, ok := parseSeconds(fields[1]); ok {
				l.Tier, l.HasTier = secs, true
			}
			continue
		}
		secs, ok := parseSeconds(fields[1])
		if !ok {
			continue
		}
		dir, ok := g.dirOfImportPath(fields[0])
		if !ok {
			continue
		}
		if secs > l.Costs[dir] {
			l.Costs[dir] = secs
		}
		l.Packages++
	}
	if err := sc.Err(); err != nil {
		return Log{}, fmt.Errorf("reading gate log %s: %w", l.Name, err)
	}
	for _, secs := range l.Costs {
		l.Total += secs
	}
	return l, nil
}

// ParseLogs reads every regular file in dir, sorted by name. A directory it
// cannot read, one that holds no log file, and one that holds a subdirectory
// are errors: each would leave packages uncosted while the report still
// looked complete.
func ParseLogs(dir string, g *Graph) ([]Log, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading the log directory: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	logs := make([]Log, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			return nil, fmt.Errorf("gate log directory %s holds the subdirectory %s; pass a directory of log files", dir, e.Name())
		}
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("opening gate log %s: %w", e.Name(), err)
		}
		l, err := ParseLog(e.Name(), f, g)
		f.Close()
		if err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	if len(logs) == 0 {
		return nil, fmt.Errorf("gate log directory %s holds no log file", dir)
	}
	return logs, nil
}

// MaxCosts is the largest wall any log reports for each package. A package no
// log lists is absent, and the model counts it as 0 and notes it.
func MaxCosts(logs []Log) map[string]float64 {
	max := map[string]float64{}
	for _, l := range logs {
		for dir, secs := range l.Costs {
			if secs > max[dir] {
				max[dir] = secs
			}
		}
	}
	return max
}

// dirOfImportPath maps an import path to a package directory, or reports that
// the graph does not hold it.
func (g *Graph) dirOfImportPath(importPath string) (string, bool) {
	if g == nil {
		return "", false
	}
	dir, ok := g.dirOf[importPath]
	return dir, ok
}

// parseSeconds reads a tierwall duration ("203.4s").
func parseSeconds(s string) (float64, bool) {
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, false
	}
	return d.Seconds(), true
}

// LandingReport is the model's answer for one landing.
type LandingReport struct {
	SHA        string
	Short      string
	Subject    string
	Changed    []string
	Dependents []string
	AlwaysRun  []string
	Packages   []string
	// Fallback names what forced the full run, and is empty otherwise.
	Fallback string
	// ModeledWall is the modeled unit-tier wall; ModeledKnown is false
	// when no readable graph made it computable.
	ModeledWall  time.Duration
	ModeledKnown bool
	// LoggedWall is the wall the landing's own log reports; HasLog says one
	// was found.
	LoggedWall time.Duration
	HasLog     bool
	// Saving is 1 - modeled/full run; HasSaving is false when either wall is
	// unknown or the full run is zero.
	Saving    float64
	HasSaving bool
	// Hub is set when the selection reaches HubPackages.
	Hub bool
	// MissingCosts are selected packages no log listed, counted as 0.
	MissingCosts []string
}

// Summary aggregates the landings.
type Summary struct {
	Landings     int
	Fallbacks    int
	MedianSaving float64
	P90Saving    float64
	HasSavings   bool
	Hubs         int
	// MissingCosts are packages no log listed, unioned over the landings.
	MissingCosts []string
}

// Report is the model's answer.
type Report struct {
	CPUS     int
	Landings []LandingReport
	Summary  Summary
}

// Build computes the report. It returns an error for a cpu count below 1,
// which would divide by zero; every other unreadable input becomes the full
// run per landing.
func Build(landings []Landing, g *Graph, hasGo func(dir string) bool, nested []string, logs []Log, cpus int) (Report, error) {
	if cpus < 1 {
		return Report{}, fmt.Errorf("cpus must be at least 1, got %d", cpus)
	}
	costs := MaxCosts(logs)
	fullWall, fullKnown := fullRunWall(g, costs, cpus)

	rep := Report{CPUS: cpus}
	missing := map[string]bool{}
	var savings []float64
	for _, l := range landings {
		sel := selectLanding(g, l, hasGo, nested)
		log := matchLog(logs, l.SHA)
		r := LandingReport{
			SHA:        l.SHA,
			Short:      shortSHA(l.SHA),
			Subject:    l.Subject,
			Changed:    sel.Changed,
			Dependents: sel.Dependents,
			AlwaysRun:  sel.AlwaysRun,
			Packages:   sel.Packages,
			Fallback:   sel.Reason,
			Hub:        len(sel.Packages) >= HubPackages,
		}
		if log != nil && (log.HasTier || log.Packages > 0) {
			r.HasLog = true
			if log.HasTier {
				r.LoggedWall = secsDuration(log.Tier)
			} else {
				r.LoggedWall = secsDuration(log.Total)
			}
		}
		if g != nil {
			est := estimate(sel.Packages, log, costs, cpus)
			r.ModeledWall, r.ModeledKnown = est.Wall, true
			r.MissingCosts = est.Missing
			for _, m := range est.Missing {
				missing[m] = true
			}
			if fullKnown && fullWall > 0 {
				r.Saving, r.HasSaving = 1-est.Wall.Seconds()/fullWall.Seconds(), true
				savings = append(savings, r.Saving)
			}
		}
		rep.Landings = append(rep.Landings, r)
		if sel.All {
			rep.Summary.Fallbacks++
		}
		if r.Hub {
			rep.Summary.Hubs++
		}
	}
	rep.Summary.Landings = len(rep.Landings)
	rep.Summary.MissingCosts = sortedSet(missing)
	sort.Float64s(savings)
	if len(savings) > 0 {
		rep.Summary.HasSavings = true
		rep.Summary.MedianSaving = median(savings)
		rep.Summary.P90Saving = percentile(savings, 0.9)
	}
	return rep, nil
}

// selectLanding is Select for one landing, honoring a diff the command could
// not read.
func selectLanding(g *Graph, l Landing, hasGo func(dir string) bool, nested []string) Selection {
	if l.Unreadable != "" {
		return g.fullSelection("unreadable diff: " + l.Unreadable)
	}
	return g.Select(l.NameStatus, hasGo, nested)
}

// fullRunWall is the wall the full gate would take: every package costs its
// largest logged wall.
func fullRunWall(g *Graph, costs map[string]float64, cpus int) (time.Duration, bool) {
	if g == nil {
		return 0, false
	}
	return estimate(g.Dirs(), nil, costs, cpus).Wall, true
}

// estimate is the model for one selection: max(longest package, sum of walls
// / cpus). A package in the landing's own log takes that log's wall;
// otherwise the largest any log reported; otherwise 0, and it is named in
// Missing.
func estimate(pkgs []string, log *Log, costs map[string]float64, cpus int) wallEstimate {
	var sum, longest float64
	var missing []string
	for _, pkg := range pkgs {
		secs, ok := 0.0, false
		if log != nil {
			secs, ok = log.Costs[pkg]
		}
		if !ok {
			secs, ok = costs[pkg]
		}
		if !ok {
			missing = append(missing, pkg)
		}
		sum += secs
		if secs > longest {
			longest = secs
		}
	}
	sort.Strings(missing)
	return wallEstimate{Wall: secsDuration(math.Max(longest, sum/float64(cpus))), Missing: missing}
}

// wallEstimate is a selection's modeled wall and the packages it had to count
// as 0.
type wallEstimate struct {
	Wall    time.Duration
	Missing []string
}

// matchLog returns the landing's own log, or nil. Every log still contributes
// to the cost curve, including one named for a commit the model did not read.
func matchLog(logs []Log, sha string) *Log {
	for i := range logs {
		if logs[i].Matches(sha) {
			return &logs[i]
		}
	}
	return nil
}

// median is the middle saving, or the mean of the two middle ones.
func median(sorted []float64) float64 {
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// percentile is the nearest-rank percentile of a sorted slice.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	rank := int(math.Ceil(p*float64(n))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= n {
		rank = n - 1
	}
	return sorted[rank]
}

// RenderText writes the report: a block per landing and the summary.
func RenderText(w io.Writer, rep Report) error {
	for _, l := range rep.Landings {
		if _, err := fmt.Fprintf(w, "%s %s\n", l.Short, l.Subject); err != nil {
			return err
		}
		lines := []struct{ label, value string }{
			{"changed", list(l.Changed)},
			{"dependents", list(l.Dependents)},
			{"always-run", list(l.AlwaysRun)},
		}
		for _, ln := range lines {
			if _, err := fmt.Fprintf(w, "  %-11s %s\n", ln.label+":", ln.value); err != nil {
				return err
			}
		}
		if l.Fallback != "" {
			if _, err := fmt.Fprintf(w, "  %-11s %s\n", "fallback:", l.Fallback); err != nil {
				return err
			}
		}
		modeled := "n/a"
		if l.ModeledKnown {
			modeled = secsText(l.ModeledWall)
			if l.Fallback != "" {
				modeled += " (full run)"
			}
		}
		logged := "-"
		if l.HasLog {
			logged = secsText(l.LoggedWall)
		}
		saving := "n/a"
		if l.HasSaving {
			saving = fmt.Sprintf("%.1f%%", l.Saving*100)
		}
		if _, err := fmt.Fprintf(w, "  %-11s %s at %d cpus; logged: %s; saving %s\n", "modeled:", modeled, rep.CPUS, logged, saving); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	hub := "hubs"
	if rep.Summary.Hubs == 1 {
		hub = "hub"
	}
	medianText, p90Text := "n/a", "n/a"
	if rep.Summary.HasSavings {
		medianText = fmt.Sprintf("%.1f%%", rep.Summary.MedianSaving*100)
		p90Text = fmt.Sprintf("%.1f%%", rep.Summary.P90Saving*100)
	}
	if _, err := fmt.Fprintf(w, "summary: %d landings, %d fallbacks, median saving %s, p90 saving %s, %d %s (>=%d packages)\n",
		rep.Summary.Landings, rep.Summary.Fallbacks, medianText,
		p90Text, rep.Summary.Hubs, hub, HubPackages); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "model: %s; cpus %d\n", Model, rep.CPUS); err != nil {
		return err
	}
	if len(rep.Summary.MissingCosts) > 0 {
		if _, err := fmt.Fprintf(w, "costs: %s in no log, counted as 0s\n", capped(rep.Summary.MissingCosts, 12)); err != nil {
			return err
		}
	}
	return nil
}

// RenderJSON writes the machine-readable report, one JSON document and a
// newline.
func RenderJSON(w io.Writer, rep Report) error {
	data, err := JSONReport(rep)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}

// JSONReport is the machine-readable report, for -json.
func JSONReport(rep Report) ([]byte, error) {
	out := jsonReport{CPUS: rep.CPUS, Model: Model, Summary: jsonSummary{
		Landings:     rep.Summary.Landings,
		Fallbacks:    rep.Summary.Fallbacks,
		MedianSaving: rep.Summary.MedianSaving,
		P90Saving:    rep.Summary.P90Saving,
		HasSavings:   rep.Summary.HasSavings,
		Hubs:         rep.Summary.Hubs,
		MissingCosts: nonNil(rep.Summary.MissingCosts),
	}}
	for _, l := range rep.Landings {
		j := jsonLanding{
			SHA:          l.SHA,
			Short:        l.Short,
			Subject:      l.Subject,
			Changed:      nonNil(l.Changed),
			Dependents:   nonNil(l.Dependents),
			AlwaysRun:    nonNil(l.AlwaysRun),
			Packages:     nonNil(l.Packages),
			Fallback:     l.Fallback,
			ModeledKnown: l.ModeledKnown,
			LoggedWallS:  l.LoggedWall.Seconds(),
			HasLog:       l.HasLog,
			Saving:       l.Saving,
			HasSaving:    l.HasSaving,
			Hub:          l.Hub,
			MissingCosts: nonNil(l.MissingCosts),
		}
		if l.ModeledKnown {
			j.ModeledWallS = l.ModeledWall.Seconds()
		}
		out.Landings = append(out.Landings, j)
	}
	return json.MarshalIndent(out, "", "  ")
}

type jsonReport struct {
	CPUS     int           `json:"cpus"`
	Model    string        `json:"model"`
	Landings []jsonLanding `json:"landings"`
	Summary  jsonSummary   `json:"summary"`
}

type jsonLanding struct {
	SHA          string   `json:"sha"`
	Short        string   `json:"short"`
	Subject      string   `json:"subject"`
	Changed      []string `json:"changed"`
	Dependents   []string `json:"dependents"`
	AlwaysRun    []string `json:"always_run"`
	Packages     []string `json:"packages"`
	Fallback     string   `json:"fallback"`
	ModeledWallS float64  `json:"modeled_wall_s"`
	ModeledKnown bool     `json:"modeled_known"`
	LoggedWallS  float64  `json:"logged_wall_s"`
	HasLog       bool     `json:"has_log"`
	Saving       float64  `json:"saving"`
	HasSaving    bool     `json:"has_saving"`
	Hub          bool     `json:"hub"`
	MissingCosts []string `json:"missing_costs"`
}

type jsonSummary struct {
	Landings     int      `json:"landings"`
	Fallbacks    int      `json:"fallbacks"`
	MedianSaving float64  `json:"median_saving"`
	P90Saving    float64  `json:"p90_saving"`
	HasSavings   bool     `json:"has_savings"`
	Hubs         int      `json:"hubs"`
	MissingCosts []string `json:"missing_costs"`
}

// list renders a package list, or "-" when empty.
func list(items []string) string {
	if len(items) == 0 {
		return "-"
	}
	return strings.Join(items, ", ")
}

// capped renders up to n items and counts the rest, so the note stays one
// readable line; --json keeps every name.
func capped(items []string, n int) string {
	if len(items) <= n {
		return list(items)
	}
	return fmt.Sprintf("%s and %d more", list(items[:n]), len(items)-n)
}

// nonNil makes an empty slice marshal as [] rather than null.
func nonNil(items []string) []string {
	if items == nil {
		return []string{}
	}
	return items
}

// shortSHA is the first ShortSHA bytes of sha.
func shortSHA(sha string) string {
	if len(sha) <= ShortSHA {
		return sha
	}
	return sha[:ShortSHA]
}

// sortedCopy returns a sorted copy of items.
func sortedCopy(items []string) []string {
	out := append([]string(nil), items...)
	sort.Strings(out)
	return out
}

// sortedSet returns the set's members, sorted.
func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for item := range set {
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}

// contains reports whether items holds want.
func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// secsDuration converts seconds to a duration.
func secsDuration(secs float64) time.Duration {
	return time.Duration(secs * float64(time.Second))
}

// secsText renders a duration as seconds with one decimal.
func secsText(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s"
}
