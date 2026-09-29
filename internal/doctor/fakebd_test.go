package doctor

import (
	"path/filepath"
	"sync"

	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

var _ bdCLI = (*beadsfake.Fake)(nil)

// bdOpen is one bd client a check opened: where and with what environment.
type bdOpen struct {
	dir string
	env []string
}

// fakeBD gives checks an in-memory bd database per directory
// (CheckContext.openBD) and records every open.
type fakeBD struct {
	mu    sync.Mutex
	dbs   map[string]*beadsfake.Fake
	opens []bdOpen
}

func newFakeBD() *fakeBD { return &fakeBD{dbs: map[string]*beadsfake.Fake{}} }

// db returns the database for dir, creating it on first use.
func (f *fakeBD) db(dir string) *beadsfake.Fake {
	f.mu.Lock()
	defer f.mu.Unlock()
	dir = filepath.Clean(dir)
	d, ok := f.dbs[dir]
	if !ok {
		d = beadsfake.New()
		f.dbs[dir] = d
	}
	return d
}

// put makes d the database for dir.
func (f *fakeBD) put(dir string, d *beadsfake.Fake) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dbs[filepath.Clean(dir)] = d
}

func (f *fakeBD) open(dir string, env []string) bdCLI {
	d := f.db(dir)
	f.mu.Lock()
	f.opens = append(f.opens, bdOpen{dir: filepath.Clean(dir), env: append([]string(nil), env...)})
	f.mu.Unlock()
	return d
}

// opened returns the opens so far.
func (f *fakeBD) opened() []bdOpen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bdOpen(nil), f.opens...)
}

// ctx returns a CheckContext for townRoot whose bd is f.
func (f *fakeBD) ctx(townRoot string) *CheckContext {
	return &CheckContext{TownRoot: townRoot, openBD: f.open}
}

// csvAnswer scripts a fake's SQL to answer every query with records.
func csvAnswer(records ...[]string) func(string) ([][]string, error) {
	return func(string) ([][]string, error) { return records, nil }
}
