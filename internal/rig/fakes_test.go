package rig

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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
		bd:        bd.run,
		env:       []string{},
		bdVersion: func() (deps.BeadsStatus, string) { return deps.BeadsOK, "1.2.3" },
		dolt:      newFakeDolt(),
		// beads.SetupRedirect runs git in a worktree that tracks .beads;
		// what it writes is beads' to test.
		redirect: func(string, string) error { return nil },
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

// bdReply is one answer of the fake bd: what it printed and how it exited.
type bdReply struct {
	stdout, stderr string
	code           int  // exit status; 0 is success
	missing        bool // no bd on PATH at all
}

// fakeBD answers a Manager's bd calls in process through the BDRunner seam,
// in place of a bd stub on PATH (which no parallel test may put there), and
// records every call. answer, when set, answers every call but the
// --allow-stale probe; otherwise show finds nothing, create returns the
// created bead, and everything else succeeds silently. A successful init
// marks the database's types configured, as a database gt set up would be,
// so CreateAgentBead's EnsureCustomTypes (which runs bd itself, outside the
// seam) has nothing to do.
type fakeBD struct {
	mu     sync.Mutex
	answer func(c beads.BDCall) bdReply
	calls  []beads.BDCall
}

// bdExit is a bd exit status, matched through ExitCode() like
// *exec.ExitError.
type bdExit int

func (e bdExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e bdExit) ExitCode() int { return int(e) }

// bdVerb is the subcommand of a bd argv, past its global flags.
func bdVerb(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// flagValue is the value of --name=value in args.
func flagValue(args []string, name string) string {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--"+name+"="); ok {
			return v
		}
	}
	return ""
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

func (b *fakeBD) run(_ context.Context, c beads.BDCall) ([]byte, []byte, error) {
	verb := bdVerb(c.Args)
	if verb == "version" {
		return nil, nil, nil // the --allow-stale probe: answering nothing keeps argv unprefixed
	}
	b.mu.Lock()
	b.calls = append(b.calls, beads.BDCall{Dir: c.Dir, Env: slices.Clone(c.Env), Args: slices.Clone(c.Args)})
	answer := b.answer
	b.mu.Unlock()
	var r bdReply
	switch {
	case answer != nil:
		r = answer(c)
	case verb == "show":
		r.stdout = "[]"
	case verb == "list" && slices.Contains(c.Args, "--json"):
		r.stdout = "[]"
	case verb == "create":
		r.stdout = fmt.Sprintf(`{"id":%q,"title":%q,"description":"","issue_type":"agent"}`, flagValue(c.Args, "id"), flagValue(c.Args, "title"))
	}
	if r.missing {
		return nil, nil, &exec.Error{Name: "bd", Err: exec.ErrNotFound}
	}
	if r.code != 0 {
		return []byte(r.stdout), []byte(r.stderr), bdExit(r.code)
	}
	if dir, ok := envValue(c.Env, "BEADS_DIR"); ok && verb == "init" {
		_ = os.WriteFile(filepath.Join(dir, ".gt-types-configured"), []byte(beads.TypeConfigSentinelValue()+"\n"), 0o644)
	}
	return []byte(r.stdout), []byte(r.stderr), nil
}

// recorded returns every call but the --allow-stale probe.
func (b *fakeBD) recorded() []beads.BDCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.calls)
}

// argvs returns every recorded call's argv joined with spaces.
func (b *fakeBD) argvs() []string {
	var out []string
	for _, c := range b.recorded() {
		out = append(out, strings.Join(c.Args, " "))
	}
	return out
}

// withVerb returns the recorded calls of one subcommand.
func (b *fakeBD) withVerb(verb string) []beads.BDCall {
	var out []beads.BDCall
	for _, c := range b.recorded() {
		if bdVerb(c.Args) == verb {
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
