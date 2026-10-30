package tiersweep

import (
	_ "embed"
	"sort"
	"strings"
	"sync"
)

// serialText is the shrink-only list of packages whose tests share fixed ports,
// temporary paths or process state, so they run alone.
//
//go:embed serial.txt
var serialText string

// SerialPackages parses serial.txt: one package per line, a # starts a comment,
// blank lines and a leading ./ are dropped. A name is the one the runner prints
// for the package, module-relative in a nested module (tracker/server).
func SerialPackages() []string {
	var pkgs []string
	for _, line := range strings.Split(serialText, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		if fields := strings.Fields(line); len(fields) > 0 {
			pkgs = append(pkgs, strings.TrimPrefix(fields[0], "./"))
		}
	}
	return pkgs
}

// serialSet is SerialPackages as a lookup: a package runs alone when its name,
// less its leading ./, is here.
var serialSet = func() map[string]bool {
	set := make(map[string]bool)
	for _, pkg := range SerialPackages() {
		set[pkg] = true
	}
	return set
}()

// serialPackage reports whether name must run alone.
func serialPackage(name string) bool { return serialSet[strings.TrimPrefix(name, "./")] }

// startOrder returns tasks ordered as a run starts them: longest first by the
// wall time the previous run took for this tier, and a package with no wall
// after the ones with one, in list order. The sort is stable, so a run with no
// walls starts in list order (gt-0rtgc).
func startOrder(tasks []*task, walls map[string]int64, tier string) []*task {
	ordered := append([]*task(nil), tasks...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return walls[TimingsKey(tier, ordered[i].name)] > walls[TimingsKey(tier, ordered[j].name)]
	})
	return ordered
}

// splitSerial divides a run into the packages that may share the host and the
// ones that must run alone, each keeping the order it was given.
func splitSerial(tasks []*task) (parallel, serial []*task) {
	for _, t := range tasks {
		if serialPackage(t.name) {
			serial = append(serial, t)
		} else {
			parallel = append(parallel, t)
		}
	}
	return parallel, serial
}

// pool runs tasks on at most Jobs workers, starting them in the order given:
// the dispatcher hands out one package per worker and waits for that worker to
// take it up before handing out the next.
func (r *Runner) pool(tier, dir string, prefix, args []string, tasks []*task) {
	if len(tasks) == 0 {
		return
	}
	tokens := make(chan struct{}, r.jobs())
	entered := make(chan struct{})
	done := make(chan *task, len(tasks))
	var wg sync.WaitGroup
	for _, t := range tasks {
		tokens <- struct{}{}
		r.start(tier, t)
		wg.Add(1)
		go func(t *task) {
			defer wg.Done()
			entered <- struct{}{}
			t.result = r.runPackage(tier, dir, prefix, args, t)
			<-tokens
			done <- t
		}(t)
		<-entered
	}
	for range tasks {
		r.report(tier, <-done)
	}
	wg.Wait()
}
