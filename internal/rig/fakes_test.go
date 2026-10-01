package rig

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/deps"
	"github.com/steveyegge/gastown/internal/git/gitfake"
)

// testManager is a Manager for a town at root whose git is a gitfake world,
// whose bd is a fakeBD, and whose bd environment starts empty: nothing it
// does runs a process or reads the test process's environment.
func testManager(root string, cfg *config.RigsConfig) (*Manager, *gitfake.Fake, *fakeBD) {
	f := gitfake.New()
	bd := &fakeBD{}
	m := &Manager{
		townRoot:  root,
		config:    cfg,
		git:       f.Open(root),
		openRepo:  func(gitDir, workDir string) Repo { return f.OpenWithDir(gitDir, workDir) },
		openBD:    bd.open,
		openLocal: bd.openLocal,
		env:       []string{},
		bdVersion: func() (deps.BeadsStatus, string) { return deps.BeadsOK, "1.2.3" },
		dolt:      newFakeDolt(),
	}
	return m, f, bd
}

func newTestManager(root string, cfg *config.RigsConfig) *Manager {
	m, _, _ := testManager(root, cfg)
	return m
}

// remoteRepo makes a bare repository at dir with one commit on main holding
// files (a README when nil), and returns dir.
func remoteRepo(t *testing.T, f *gitfake.Fake, dir string, files map[string]string) string {
	t.Helper()
	if files == nil {
		files = map[string]string{"README.md": "# " + filepath.Base(dir) + "\n"}
	}
	f.InitBare(t, dir)
	f.Commit(t, dir, "main", "Initial commit", files)
	return dir
}

// bdCall is one bd call the manager made: the directory and environment
// it ran with, the verb ("init", "config get", "config set", "migrate"),
// the config key and value, and init's options.
type bdCall struct {
	Dir  string
	Env  []string
	Verb string
	Args []string
	Init beads.InitOptions
}

// argv is the call as bd's argv reads, init's options aside.
func (c bdCall) argv() string { return strings.Join(append([]string{c.Verb}, c.Args...), " ") }

// fakeBD is rig setup's bd in memory, recording every call. fail, when set,
// fails a call with the error it returns; config answers config get. A
// successful init marks the database's types configured, as a database gt
// set up would be, so CreateAgentBead's EnsureCustomTypes (which runs bd
// itself) has nothing to do.
type fakeBD struct {
	mu     sync.Mutex
	fail   func(c bdCall) error
	config map[string]string
	calls  []bdCall
}

func (b *fakeBD) open(dir string, env []string) rigBD { return fakeBDAt{b, dir, env} }

func (b *fakeBD) openLocal(dir string) configSetter { return fakeBDAt{b, dir, nil} }

func (b *fakeBD) call(c bdCall) (string, error) {
	b.mu.Lock()
	c.Env = slices.Clone(c.Env)
	b.calls = append(b.calls, c)
	fail := b.fail
	value := b.config[strings.Join(c.Args, " ")]
	b.mu.Unlock()
	if fail != nil {
		if err := fail(c); err != nil {
			return "", err
		}
	}
	return value, nil
}

// fakeBDAt is fakeBD at one directory and environment.
type fakeBDAt struct {
	b   *fakeBD
	dir string
	env []string
}

func (f fakeBDAt) InitDatabase(opts beads.InitOptions) error {
	if _, err := f.b.call(bdCall{Dir: f.dir, Env: f.env, Verb: "init", Init: opts}); err != nil {
		return err
	}
	if dir, ok := envValue(f.env, "BEADS_DIR"); ok {
		_ = os.WriteFile(filepath.Join(dir, ".gt-types-configured"), []byte(beads.TypeConfigSentinelValue()+"\n"), 0o644)
	}
	return nil
}

func (f fakeBDAt) ConfigGet(key string) (string, error) {
	return f.b.call(bdCall{Dir: f.dir, Env: f.env, Verb: "config get", Args: []string{key}})
}

func (f fakeBDAt) ConfigSet(key, value string) error {
	_, err := f.b.call(bdCall{Dir: f.dir, Env: f.env, Verb: "config set", Args: []string{key, value}})
	return err
}

func (f fakeBDAt) MigrateRepoID() error {
	_, err := f.b.call(bdCall{Dir: f.dir, Env: f.env, Verb: "migrate"})
	return err
}

// envValue is key's value in env, the last one winning as in a process.
func envValue(env []string, key string) (string, bool) {
	value, found := "", false
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			value, found = v, true
		}
	}
	return value, found
}

// recorded returns every call.
func (b *fakeBD) recorded() []bdCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.calls)
}

// argvs returns every recorded call's argv.
func (b *fakeBD) argvs() []string {
	var out []string
	for _, c := range b.recorded() {
		out = append(out, c.argv())
	}
	return out
}

// withVerb returns the recorded calls whose verb starts with verb.
func (b *fakeBD) withVerb(verb string) []bdCall {
	var out []bdCall
	for _, c := range b.recorded() {
		if strings.HasPrefix(c.Verb, verb) {
			out = append(out, c)
		}
	}
	return out
}

// fakeDolt is doltDatabases in memory: the databases on the server, and the
// issue prefix each rig database was seeded with.
type fakeDolt struct {
	mu       sync.Mutex
	dbs      map[string]bool
	stuck    map[string]bool // Remove reports success but the database stays
	prefixes map[string]string
}

func newFakeDolt(dbs ...string) *fakeDolt {
	d := &fakeDolt{dbs: map[string]bool{}, stuck: map[string]bool{}, prefixes: map[string]string{}}
	for _, db := range dbs {
		d.dbs[db] = true
	}
	return d
}

func (d *fakeDolt) Exists(name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dbs[name]
}

func (d *fakeDolt) Remove(name string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.dbs[name] {
		return fmt.Errorf("database %q not found", name)
	}
	if !d.stuck[name] {
		delete(d.dbs, name)
	}
	return nil
}

func (d *fakeDolt) SetIssuePrefix(_, database, prefix string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prefixes[database] = prefix
	return nil
}
