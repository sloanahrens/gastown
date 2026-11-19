package daemon

import (
	"bytes"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/tiersweep"
)

// sweepRunsNow is the patrol's clock in these tests.
var sweepRunsNow = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// sweepRunsName is a run id the sweep writes for rig, with the given hash.
func sweepRunsName(rig, hash string) string {
	return rig + "-" + hash + "-" + tiersweep.TierShell
}

// fakeSweepInfo is the fs.FileInfo and fs.DirEntry of one node in the fake tree.
type fakeSweepInfo struct {
	name    string
	mode    fs.FileMode
	size    int64
	modTime time.Time
}

func (i fakeSweepInfo) Name() string       { return i.name }
func (i fakeSweepInfo) Size() int64        { return i.size }
func (i fakeSweepInfo) Mode() fs.FileMode  { return i.mode }
func (i fakeSweepInfo) ModTime() time.Time { return i.modTime }
func (i fakeSweepInfo) IsDir() bool        { return i.mode.IsDir() }
func (i fakeSweepInfo) Sys() any           { return nil }

func (i fakeSweepInfo) Type() fs.FileMode { return i.mode.Type() }

// fakeSweepEntry is a directory entry whose Info may be made to fail.
type fakeSweepEntry struct {
	fakeSweepInfo
	infoErr error
}

func (e fakeSweepEntry) Info() (fs.FileInfo, error) {
	if e.infoErr != nil {
		return nil, e.infoErr
	}
	return e.fakeSweepInfo, nil
}

// fakeSweepRunsFS is an in-memory tree the prune lists, sizes, checks and
// removes through. Every failure a test needs is set on it by path.
type fakeSweepRunsFS struct {
	nodes     map[string]fakeSweepInfo
	readErr   map[string]error
	infoErr   map[string]error
	lstatMode map[string]fs.FileMode
	vanish    map[string]bool
	removeErr map[string]error
	removed   []string
}

func newFakeSweepRunsFS() *fakeSweepRunsFS {
	return &fakeSweepRunsFS{
		nodes:     map[string]fakeSweepInfo{},
		readErr:   map[string]error{},
		infoErr:   map[string]error{},
		lstatMode: map[string]fs.FileMode{},
		vanish:    map[string]bool{},
		removeErr: map[string]error{},
	}
}

// dir adds a directory at path, last modified at modTime.
func (f *fakeSweepRunsFS) dir(path string, modTime time.Time) {
	f.nodes[path] = fakeSweepInfo{name: filepath.Base(path), mode: fs.ModeDir | 0o755, modTime: modTime}
}

// file adds a regular file of size bytes at path.
func (f *fakeSweepRunsFS) file(path string, size int64, modTime time.Time) {
	f.nodes[path] = fakeSweepInfo{name: filepath.Base(path), mode: 0o644, size: size, modTime: modTime}
}

// link adds a symlink at path.
func (f *fakeSweepRunsFS) link(path string, modTime time.Time) {
	f.nodes[path] = fakeSweepInfo{name: filepath.Base(path), mode: fs.ModeSymlink | 0o777, modTime: modTime}
}

func (f *fakeSweepRunsFS) ReadDir(dir string) ([]fs.DirEntry, error) {
	if err := f.readErr[dir]; err != nil {
		return nil, err
	}
	if _, ok := f.nodes[dir]; !ok {
		return nil, &fs.PathError{Op: "readdir", Path: dir, Err: fs.ErrNotExist}
	}
	var children []string
	for p := range f.nodes {
		if filepath.Dir(p) == dir {
			children = append(children, p)
		}
	}
	slices.Sort(children)
	out := make([]fs.DirEntry, 0, len(children))
	for _, p := range children {
		out = append(out, fakeSweepEntry{fakeSweepInfo: f.nodes[p], infoErr: f.infoErr[p]})
	}
	return out, nil
}

func (f *fakeSweepRunsFS) Lstat(path string) (fs.FileInfo, error) {
	if f.vanish[path] {
		// A directory that goes away between the plan and its check.
		delete(f.nodes, path)
	}
	n, ok := f.nodes[path]
	if !ok {
		return nil, &fs.PathError{Op: "lstat", Path: path, Err: fs.ErrNotExist}
	}
	if mode, swapped := f.lstatMode[path]; swapped {
		n.mode = mode
	}
	return n, nil
}

func (f *fakeSweepRunsFS) RemoveAll(path string) error {
	if err := f.removeErr[path]; err != nil {
		return err
	}
	f.removed = append(f.removed, path)
	for p := range f.nodes {
		if p == path || strings.HasPrefix(p, path+string(filepath.Separator)) {
			delete(f.nodes, p)
		}
	}
	return nil
}

// sweepRunsFixture is a daemon whose patrol is enabled in mode, reading the fake
// tree and logging to logs. The dispatch runs its pass inline.
type sweepRunsFixture struct {
	d    *Daemon
	logs *bytes.Buffer
	root string
	fsys *fakeSweepRunsFS
}

func newSweepRunsFixture(t *testing.T, mode string, keep int) *sweepRunsFixture {
	t.Helper()
	town := t.TempDir()
	var logs bytes.Buffer
	patrol := diskMaintEnabled("", mode)
	cfg := patrol.Patrols.DiskMaintenance
	cfg.SweepRunsKeep = &keep
	fsys := newFakeSweepRunsFS()
	d := &Daemon{
		config:       &Config{TownRoot: town},
		logger:       log.New(&logs, "", 0),
		patrolConfig: patrol,
	}
	d.diskMaint.now = func() time.Time { return sweepRunsNow }
	d.diskMaint.sweepRunsFS = fsys
	d.diskMaint.dispatch = func(fn func()) { fn() }
	return &sweepRunsFixture{
		d:    d,
		logs: &logs,
		root: filepath.Join(town, filepath.FromSlash(sweepRunsRootRel)),
		fsys: fsys,
	}
}

// rigTree lays out the rig gastown with five run directories, aged 200h, 150h,
// 100h, 10h and 5h, each 1 GB. With keep 2 the three oldest are planned.
func (f *sweepRunsFixture) rigTree() (planned []string) {
	rig := filepath.Join(f.root, "gastown")
	f.fsys.dir(f.root, sweepRunsNow)
	f.fsys.dir(rig, sweepRunsNow)
	ages := []time.Duration{200 * time.Hour, 150 * time.Hour, 100 * time.Hour, 10 * time.Hour, 5 * time.Hour}
	for i, age := range ages {
		name := sweepRunsName("gastown", []string{"0a1b2c3d", "1a1b2c3d", "2a1b2c3d", "3a1b2c3d", "4a1b2c3d"}[i])
		p := filepath.Join(rig, name)
		f.fsys.dir(p, sweepRunsNow.Add(-age))
		f.fsys.file(filepath.Join(p, "out.log"), 1e9, sweepRunsNow.Add(-age))
		// A nested directory with an empty file: sizing descends into it and the
		// run's total is still the 1 GB its out.log holds.
		f.fsys.dir(filepath.Join(p, "logs"), sweepRunsNow.Add(-age))
		f.fsys.file(filepath.Join(p, "logs", "empty.log"), 0, sweepRunsNow.Add(-age))
		if i < 3 {
			planned = append(planned, p)
		}
	}
	return planned
}

// logLines returns the non-empty lines the daemon logged.
func (f *sweepRunsFixture) logLines() []string {
	var out []string
	for line := range strings.SplitSeq(f.logs.String(), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// TestSweepRunsPruneDryRunLogsOneLineAndRemovesNothing: a dry run names the
// planned directories, their count and size, and the kept count in one line, and
// removes nothing.
func TestSweepRunsPruneDryRunLogsOneLineAndRemovesNothing(t *testing.T) {
	t.Parallel()
	f := newSweepRunsFixture(t, agentconfig.DiskMaintenanceModeDryRun, 2)
	f.rigTree()
	f.d.runSweepRunsPruneTick()

	if len(f.fsys.removed) != 0 {
		t.Fatalf("a dry run removed %v", f.fsys.removed)
	}
	lines := f.logLines()
	if len(lines) != 1 {
		t.Fatalf("dry run logged %d lines, want one: %q", len(lines), lines)
	}
	want := "disk_maintenance: sweep runs prune dry_run: would remove 3 directories, 3.0 GB, keeping 2: " +
		"gastown/" + sweepRunsName("gastown", "0a1b2c3d") + ", " +
		"gastown/" + sweepRunsName("gastown", "1a1b2c3d") + ", " +
		"gastown/" + sweepRunsName("gastown", "2a1b2c3d")
	if lines[0] != want {
		t.Errorf("dry run line:\n got %q\nwant %q", lines[0], want)
	}
}

// TestSweepRunsPruneApplyRemovesOnlyPlannedDirectories: an apply removes exactly
// the planned directories. A file, a symlink, and a directory the sweep did not
// write are never removed, even under a run-shaped name.
func TestSweepRunsPruneApplyRemovesOnlyPlannedDirectories(t *testing.T) {
	t.Parallel()
	f := newSweepRunsFixture(t, agentconfig.DiskMaintenanceModeApply, 2)
	planned := f.rigTree()
	rig := filepath.Join(f.root, "gastown")
	stray := filepath.Join(f.root, "stray.txt")
	f.fsys.file(stray, 1, sweepRunsNow.Add(-300*time.Hour))
	runShapedFile := filepath.Join(rig, sweepRunsName("gastown", "5a1b2c3d"))
	f.fsys.file(runShapedFile, 1, sweepRunsNow.Add(-300*time.Hour))
	runShapedLink := filepath.Join(rig, sweepRunsName("gastown", "6a1b2c3d"))
	f.fsys.link(runShapedLink, sweepRunsNow.Add(-300*time.Hour))
	notSweep := filepath.Join(rig, "scratch")
	f.fsys.dir(notSweep, sweepRunsNow.Add(-300*time.Hour))

	f.d.runSweepRunsPruneTick()

	if !slices.Equal(f.fsys.removed, planned) {
		t.Fatalf("apply removed %v, want exactly the planned %v", f.fsys.removed, planned)
	}
	for _, p := range []string{stray, runShapedFile, runShapedLink, notSweep} {
		if _, ok := f.fsys.nodes[p]; !ok {
			t.Errorf("%s was removed; the prune names only planned directories", p)
		}
	}
	lines := f.logLines()
	want := "disk_maintenance: sweep runs prune apply: removed 3 directories, 3.0 GB, keeping 2"
	if len(lines) != 1 || lines[0] != want {
		t.Errorf("apply logged %q, want one line %q", lines, want)
	}
}

// TestSweepRunsPruneRefusesWhatItCannotList: a listing, size or stat failure at
// any level removes nothing, even when another rig has planned directories, and
// logs one line naming the path.
func TestSweepRunsPruneRefusesWhatItCannotList(t *testing.T) {
	t.Parallel()
	cases := map[string]func(f *sweepRunsFixture, rig string){
		"the root cannot be listed": func(f *sweepRunsFixture, _ string) {
			f.fsys.readErr[f.root] = fs.ErrPermission
		},
		"a rig cannot be listed": func(f *sweepRunsFixture, _ string) {
			f.fsys.dir(filepath.Join(f.root, "zeta"), sweepRunsNow)
			f.fsys.readErr[filepath.Join(f.root, "zeta")] = fs.ErrPermission
		},
		"a run cannot be read for its size": func(f *sweepRunsFixture, rig string) {
			f.fsys.readErr[filepath.Join(rig, sweepRunsName("gastown", "0a1b2c3d"))] = fs.ErrPermission
		},
		"a directory inside a run cannot be read": func(f *sweepRunsFixture, rig string) {
			logs := filepath.Join(rig, sweepRunsName("gastown", "0a1b2c3d"), "logs")
			f.fsys.readErr[logs] = fs.ErrPermission
		},
		"a run cannot be stat'd": func(f *sweepRunsFixture, rig string) {
			f.fsys.infoErr[filepath.Join(rig, sweepRunsName("gastown", "0a1b2c3d"))] = fs.ErrPermission
		},
		"a file inside a run cannot be stat'd": func(f *sweepRunsFixture, rig string) {
			f.fsys.infoErr[filepath.Join(rig, sweepRunsName("gastown", "0a1b2c3d"), "out.log")] = fs.ErrPermission
		},
	}
	for name, breakTree := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newSweepRunsFixture(t, agentconfig.DiskMaintenanceModeApply, 2)
			f.rigTree()
			breakTree(f, filepath.Join(f.root, "gastown"))
			f.d.runSweepRunsPruneTick()

			if len(f.fsys.removed) != 0 {
				t.Fatalf("a refused run removed %v", f.fsys.removed)
			}
			lines := f.logLines()
			if len(lines) != 1 || !strings.Contains(lines[0], "nothing removed") {
				t.Errorf("refusal logged %q, want one line saying nothing removed", lines)
			}
		})
	}
}

// TestSweepRunsPruneRemoveErrorStopsTheRun: a remove that fails stops the run at
// that directory, names it in the log, and leaves the directories after it alone.
func TestSweepRunsPruneRemoveErrorStopsTheRun(t *testing.T) {
	t.Parallel()
	f := newSweepRunsFixture(t, agentconfig.DiskMaintenanceModeApply, 2)
	planned := f.rigTree()
	f.fsys.removeErr[planned[1]] = errors.New("device busy")
	f.d.runSweepRunsPruneTick()

	if !slices.Equal(f.fsys.removed, planned[:1]) {
		t.Fatalf("removed %v, want only the first planned directory before the failure", f.fsys.removed)
	}
	lines := f.logLines()
	if len(lines) != 1 || !strings.Contains(lines[0], "stopped") ||
		!strings.Contains(lines[0], "gastown/"+sweepRunsName("gastown", "1a1b2c3d")) ||
		!strings.Contains(lines[0], "device busy") {
		t.Errorf("remove error logged %q, want one line naming the directory and the error", lines)
	}
}

// TestSweepRunsPruneRecheckRefusesAReplacedDirectory: a planned name that is a
// symlink when it is about to be removed is not removed, and the run stops.
func TestSweepRunsPruneRecheckRefusesAReplacedDirectory(t *testing.T) {
	t.Parallel()
	f := newSweepRunsFixture(t, agentconfig.DiskMaintenanceModeApply, 2)
	planned := f.rigTree()
	f.fsys.lstatMode[planned[0]] = fs.ModeSymlink | 0o777
	f.d.runSweepRunsPruneTick()

	if len(f.fsys.removed) != 0 {
		t.Fatalf("a directory replaced by a symlink was removed: %v", f.fsys.removed)
	}
	lines := f.logLines()
	if len(lines) != 1 || !strings.Contains(lines[0], "gastown/"+sweepRunsName("gastown", "0a1b2c3d")) {
		t.Errorf("re-check logged %q, want one line naming the directory", lines)
	}
}

// TestSweepRunsPruneVanishedDirectoryStopsTheRun: a planned directory that is gone
// when its check runs is not removed, the run stops, and the name is logged.
func TestSweepRunsPruneVanishedDirectoryStopsTheRun(t *testing.T) {
	t.Parallel()
	f := newSweepRunsFixture(t, agentconfig.DiskMaintenanceModeApply, 2)
	planned := f.rigTree()
	f.fsys.vanish[planned[0]] = true
	f.d.runSweepRunsPruneTick()

	if len(f.fsys.removed) != 0 {
		t.Fatalf("a vanished directory was removed: %v", f.fsys.removed)
	}
	lines := f.logLines()
	if len(lines) != 1 || !strings.Contains(lines[0], "stopped") || !strings.Contains(lines[0], "gastown/"+sweepRunsName("gastown", "0a1b2c3d")) {
		t.Errorf("vanished directory logged %q, want one stopped line naming it", lines)
	}
}

// TestSweepRunsPruneRunsNothingWhenInFlightNotDueOrRefused: a tick that finds a
// pass in flight, a run not yet due, or a mode the patrol cannot act on removes
// nothing, and a refused mode says so by name.
func TestSweepRunsPruneRunsNothingWhenInFlightNotDueOrRefused(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		setup func(f *sweepRunsFixture)
		logs  string
	}{
		"a pass is in flight": {
			setup: func(f *sweepRunsFixture) { f.d.sweepRunsPruneRunning.Store(true) },
		},
		"the run is not due": {
			setup: func(f *sweepRunsFixture) { f.d.diskMaintSchedule.record(sweepRunsPruneName, sweepRunsNow) },
		},
		"the mode is unknown": {
			setup: func(f *sweepRunsFixture) { f.d.patrolConfig.Patrols.DiskMaintenance.Mode = "bogus" },
			logs:  "patrols.disk_maintenance.mode",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newSweepRunsFixture(t, agentconfig.DiskMaintenanceModeApply, 2)
			planned := f.rigTree()
			tc.setup(f)
			f.d.runSweepRunsPruneTick()

			if len(f.fsys.removed) != 0 {
				t.Fatalf("removed %v; a tick that must not act removed something (planned %v)", f.fsys.removed, planned)
			}
			if tc.logs == "" {
				if lines := f.logLines(); len(lines) != 0 {
					t.Errorf("logged %q, want nothing", lines)
				}
				return
			}
			if !strings.Contains(f.logs.String(), tc.logs) {
				t.Errorf("refusal does not name %s:\n%s", tc.logs, f.logs.String())
			}
		})
	}
}

// TestSweepRunsPrunePassRefusesASettingItCannotAct: the pass re-reads its settings
// when it starts, and an invalid one refuses the run there too.
func TestSweepRunsPrunePassRefusesASettingItCannotAct(t *testing.T) {
	t.Parallel()
	zero := 0
	f := newSweepRunsFixture(t, agentconfig.DiskMaintenanceModeApply, 2)
	f.rigTree()
	f.d.patrolConfig.Patrols.DiskMaintenance.SweepRunsKeep = &zero
	f.d.runSweepRunsPrunePass(agentconfig.DiskMaintenanceModeApply)

	if len(f.fsys.removed) != 0 {
		t.Fatalf("an invalid setting removed %v", f.fsys.removed)
	}
	if !strings.Contains(f.logs.String(), "patrols.disk_maintenance.sweep_runs_keep") {
		t.Errorf("pass refusal does not name the setting:\n%s", f.logs.String())
	}
}

// TestSweepRunsPrunePassReadsDueAgainWhenItStarts: a pass dispatched by a tick
// that found the run due does not run it again when a run concluded after the
// tick's decision, so the pass removes nothing and says nothing.
func TestSweepRunsPrunePassReadsDueAgainWhenItStarts(t *testing.T) {
	t.Parallel()
	f := newSweepRunsFixture(t, agentconfig.DiskMaintenanceModeApply, 2)
	f.rigTree()
	f.d.diskMaintSchedule.record(sweepRunsPruneName, sweepRunsNow)
	f.d.runSweepRunsPrunePass(agentconfig.DiskMaintenanceModeApply)

	if len(f.fsys.removed) != 0 || len(f.logLines()) != 0 {
		t.Errorf("a pass that is not due removed %v and logged %q", f.fsys.removed, f.logLines())
	}
}

// TestSweepRunsPruneDryRunWithNothingOldEnoughNamesNothing: a dry run whose plan
// is empty still logs its line, with zero directories and no list after it.
func TestSweepRunsPruneDryRunWithNothingOldEnoughNamesNothing(t *testing.T) {
	t.Parallel()
	f := newSweepRunsFixture(t, agentconfig.DiskMaintenanceModeDryRun, 2)
	rig := filepath.Join(f.root, "gastown")
	f.fsys.dir(f.root, sweepRunsNow)
	f.fsys.dir(rig, sweepRunsNow)
	for i, age := range []time.Duration{10 * time.Hour, 5 * time.Hour} {
		p := filepath.Join(rig, sweepRunsName("gastown", []string{"0a1b2c3d", "1a1b2c3d"}[i]))
		f.fsys.dir(p, sweepRunsNow.Add(-age))
	}
	f.d.runSweepRunsPruneTick()

	want := "disk_maintenance: sweep runs prune dry_run: would remove 0 directories, 0.0 GB, keeping 2"
	if lines := f.logLines(); len(lines) != 1 || lines[0] != want {
		t.Errorf("empty plan logged %q, want %q", lines, want)
	}
}

// TestSweepRunsPruneMissingRootIsAnEmptyPlan: before the sweep has written
// anything there is nothing to plan, so the run concludes and says nothing.
func TestSweepRunsPruneMissingRootIsAnEmptyPlan(t *testing.T) {
	t.Parallel()
	f := newSweepRunsFixture(t, agentconfig.DiskMaintenanceModeApply, 2)
	f.d.runSweepRunsPruneTick()

	if lines := f.logLines(); len(lines) != 0 {
		t.Errorf("a missing root logged %q, want nothing", lines)
	}
	if last := f.d.diskMaintSchedule.lastRunAt(sweepRunsPruneName); last.IsZero() {
		t.Errorf("a missing root left the prune unscheduled; it concluded")
	}
}

// TestSweepRunsPruneRefusesAnInvalidSettingByName: each setting the patrol cannot
// act on runs nothing, and the refusal names the setting by its key.
func TestSweepRunsPruneRefusesAnInvalidSettingByName(t *testing.T) {
	t.Parallel()
	zero := 0
	cases := map[string]struct {
		key   string
		apply func(*agentconfig.DiskMaintenanceConfig)
	}{
		"interval": {"sweep_runs_prune_interval", func(c *agentconfig.DiskMaintenanceConfig) { c.SweepRunsPruneInterval = "hourly" }},
		"keep":     {"sweep_runs_keep", func(c *agentconfig.DiskMaintenanceConfig) { c.SweepRunsKeep = &zero }},
		"min age":  {"sweep_runs_min_age", func(c *agentconfig.DiskMaintenanceConfig) { c.SweepRunsMinAge = "0h" }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newSweepRunsFixture(t, agentconfig.DiskMaintenanceModeApply, 2)
			f.rigTree()
			tc.apply(f.d.patrolConfig.Patrols.DiskMaintenance)
			f.d.runSweepRunsPruneTick()

			if len(f.fsys.removed) != 0 {
				t.Fatalf("an invalid setting removed %v", f.fsys.removed)
			}
			if !strings.Contains(f.logs.String(), "patrols.disk_maintenance."+tc.key) {
				t.Errorf("refusal does not name %s:\n%s", tc.key, f.logs.String())
			}
		})
	}
}

// TestSweepRunsPruneRealTreeSparesSymlinksAndFiles: on the real filesystem, an
// apply removes the planned run directories. A symlink inside one points at a
// directory outside the root, and that target survives, as do a stray file and a
// file under the rig.
func TestSweepRunsPruneRealTreeSparesSymlinksAndFiles(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	root := filepath.Join(town, filepath.FromSlash(sweepRunsRootRel))
	rig := filepath.Join(root, "gastown")
	outside := filepath.Join(town, "outside")
	for _, dir := range []string{rig, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	var planned []string
	for i, age := range []time.Duration{100 * time.Hour, 5 * time.Hour} {
		p := filepath.Join(rig, sweepRunsName("gastown", []string{"0a1b2c3d", "1a1b2c3d"}[i]))
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "out.log"), []byte("out"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(p, "link")); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, sweepRunsNow.Add(-age), sweepRunsNow.Add(-age)); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			planned = append(planned, p)
		}
	}
	stray := filepath.Join(root, "stray.txt")
	if err := os.WriteFile(stray, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	patrol := diskMaintEnabled("", agentconfig.DiskMaintenanceModeApply)
	keep := 1
	patrol.Patrols.DiskMaintenance.SweepRunsKeep = &keep
	d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(&logs, "", 0), patrolConfig: patrol}
	d.diskMaint.now = func() time.Time { return sweepRunsNow }
	d.diskMaint.dispatch = func(fn func()) { fn() }
	d.runSweepRunsPruneTick()

	if _, err := os.Lstat(planned[0]); !os.IsNotExist(err) {
		t.Errorf("the planned directory survived the apply: %v", err)
	}
	for _, p := range []string{filepath.Join(rig, sweepRunsName("gastown", "1a1b2c3d")), stray} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Errorf("the symlink target outside the root was removed: %v", err)
	}
	if !strings.Contains(logs.String(), "apply: removed 1 directories, 0.0 GB, keeping 1") {
		t.Errorf("apply line missing:\n%s", logs.String())
	}
}
