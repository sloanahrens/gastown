package polecat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/tmux/tmuxfake"
	"github.com/steveyegge/gastown/internal/util"
)

// polecatDB is one beads database as the Manager sees it: a beadsfake
// database plus the agent-bead and merge-request helpers polecatBeads adds,
// each reduced to the Client operations *beads.Beads performs. It records the
// sites the manager opened it for and every write.
type polecatDB struct {
	*beadsfake.Fake

	mu sync.Mutex
	// err, when set, fails every read and write: a rig with no database, or
	// no bd installed.
	err error
	// createErr, when set, fails agent-bead creation only.
	createErr error
	// hidden agent beads read as not found (a bead that cannot be read).
	hidden  map[string]bool
	sites   []beadsSite
	updates []string // "<id>" per Update, in order
	reads   int      // Show, List and assignee reads
}

func newPolecatDB() *polecatDB {
	return &polecatDB{Fake: beadsfake.New(), hidden: map[string]bool{}}
}

// noDatabaseErr is what the real bd says, with exit 1, to any call in a
// directory with no beads database.
var noDatabaseErr = errors.New("Error: no beads database found\nHint: run 'bd where' to inspect the resolved workspace, or 'bd init' to create a new database")

// newNoDatabaseDB is a rig with no beads database, what a test with no bd set
// up used to reach through the real bd on PATH.
func newNoDatabaseDB() *polecatDB {
	db := newPolecatDB()
	db.err = noDatabaseErr
	return db
}

// newMissingDB is a host with no bd installed.
func newMissingDB() *polecatDB {
	db := newPolecatDB()
	db.err = beads.ErrNotInstalled
	return db
}

// open is the manager's opener: it records the site and hands out db.
func (db *polecatDB) open(site beadsSite) polecatStore {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.sites = append(db.sites, site)
	return db
}

func (db *polecatDB) openedSites() []beadsSite {
	db.mu.Lock()
	defer db.mu.Unlock()
	return append([]beadsSite(nil), db.sites...)
}

func (db *polecatDB) updated() []string {
	db.mu.Lock()
	defer db.mu.Unlock()
	return append([]string(nil), db.updates...)
}

func (db *polecatDB) readCount() int {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.reads
}

func (db *polecatDB) read() {
	db.mu.Lock()
	db.reads++
	db.mu.Unlock()
}

func (db *polecatDB) Show(id string) (*beads.Issue, error) {
	db.read()
	if db.err != nil {
		return nil, db.err
	}
	// A hidden bead is one that could not be read at all (forgetAgent), so
	// every read of it — beads.GetAgentBead's Show included — misses it.
	if db.isHidden(id) {
		return nil, fmt.Errorf("%s: %w", id, beads.ErrNotFound)
	}
	return db.Fake.Show(id)
}

func (db *polecatDB) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	db.read()
	if db.err != nil {
		return nil, db.err
	}
	issues, err := db.Fake.List(opts)
	if err != nil {
		return nil, err
	}
	out := make([]*beads.Issue, 0, len(issues))
	for _, issue := range issues {
		if !db.isHidden(issue.ID) {
			out = append(out, issue)
		}
	}
	return out, nil
}

func (db *polecatDB) ListByAssignee(assignee string) ([]*beads.Issue, error) {
	db.read()
	if db.err != nil {
		return nil, db.err
	}
	return db.Fake.ListByAssignee(assignee)
}

func (db *polecatDB) GetAssignedIssue(assignee string) (*beads.Issue, error) {
	db.read()
	if db.err != nil {
		return nil, db.err
	}
	return db.Fake.GetAssignedIssue(assignee)
}

func (db *polecatDB) ListIssueStatuses(statuses ...beads.IssueStatus) ([]*beads.Issue, error) {
	db.read()
	if db.err != nil {
		return nil, db.err
	}
	return db.Fake.ListIssueStatuses(statuses...)
}

func (db *polecatDB) Update(id string, opts beads.UpdateOptions) error {
	if db.err != nil {
		return db.err
	}
	db.mu.Lock()
	db.updates = append(db.updates, id)
	db.mu.Unlock()
	return db.Fake.Update(id, opts)
}

func (db *polecatDB) ReleaseIfAssignee(id, expected string) (bool, error) {
	if db.err != nil {
		return false, db.err
	}
	return db.Fake.ReleaseIfAssignee(id, expected)
}

func (db *polecatDB) RecordReassignment(id, from, to, requester string, branches []string) error {
	if db.err != nil {
		return db.err
	}
	return beads.RecordReassignmentIn(db.Fake, id, from, to, requester, branches)
}

func (db *polecatDB) FindMRForBranchAny(branch string) (*beads.Issue, error) {
	return db.findMR(branch, false)
}

func (db *polecatDB) findMR(branch string, skipClosed bool) (*beads.Issue, error) {
	mrs, err := db.List(beads.ListOptions{Label: "gt:merge-request", Status: "all", Priority: -1})
	if err != nil {
		return nil, err
	}
	for _, mr := range mrs {
		if skipClosed && mr.Status == "closed" {
			continue
		}
		if strings.HasPrefix(mr.Description, "branch: "+branch+"\n") {
			return mr, nil
		}
	}
	return nil, nil
}

func (db *polecatDB) isHidden(id string) bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.hidden[id]
}

func (db *polecatDB) CreateOrReopenAgentBead(id, title string, fields *beads.AgentFields) (*beads.Issue, error) {
	if db.err != nil {
		return nil, db.err
	}
	if db.createErr != nil {
		return nil, db.createErr
	}
	description := beads.FormatAgentDescription(title, fields)
	if _, err := db.Fake.Show(id); err != nil {
		return db.Fake.Create(beads.CreateOptions{ID: id, Title: title, Description: description, Labels: []string{"gt:agent"}, Priority: -1})
	}
	open := string(beads.StatusOpen)
	if err := db.Fake.Update(id, beads.UpdateOptions{Title: &title, Description: &description, Status: &open, SetLabels: []string{"gt:agent"}}); err != nil {
		return nil, err
	}
	db.mu.Lock()
	delete(db.hidden, id)
	db.mu.Unlock()
	return db.Fake.Show(id)
}

func (db *polecatDB) ResetAgentBeadForReuse(id, reason string) error {
	issue, err := db.Show(id)
	if err != nil {
		return err
	}
	fields := beads.ParseAgentFields(issue.Description)
	*fields = beads.AgentFields{RoleType: fields.RoleType, Rig: fields.Rig, AgentState: string(beads.AgentStateNuked)}
	description := beads.FormatAgentDescription(issue.Title, fields)
	return db.Update(id, beads.UpdateOptions{Description: &description})
}

// setAgent writes agent bead id, creating it when missing: edit changes the
// fields of what is there (a polecat in its idle resting state, clean, when
// the bead is new).
func (db *polecatDB) setAgent(t testing.TB, id string, edit func(*beads.AgentFields)) {
	t.Helper()
	fields := &beads.AgentFields{RoleType: "polecat", AgentState: "idle", CleanupStatus: "clean"}
	title := "agent"
	if existing, err := db.Fake.Show(id); err == nil {
		fields = beads.ParseAgentFields(existing.Description)
		title = existing.Title
	}
	if edit != nil {
		edit(fields)
	}
	description := beads.FormatAgentDescription(title, fields)
	if _, err := db.Fake.Show(id); err != nil {
		if _, err := db.Fake.Create(beads.CreateOptions{ID: id, Title: title, Description: description, Labels: []string{"gt:agent"}, Priority: -1}); err != nil {
			t.Fatalf("create agent bead %s: %v", id, err)
		}
		return
	}
	if err := db.Fake.Update(id, beads.UpdateOptions{Description: &description}); err != nil {
		t.Fatalf("update agent bead %s: %v", id, err)
	}
	db.mu.Lock()
	delete(db.hidden, id)
	db.mu.Unlock()
}

// settleAgent puts agent bead id in the resting state of a polecat whose
// spawn finished: idle, clean, nothing hooked, working on branch.
func (db *polecatDB) settleAgent(t testing.TB, id, branch string) {
	t.Helper()
	db.setAgent(t, id, func(f *beads.AgentFields) {
		f.AgentState, f.CleanupStatus, f.HookBead, f.ActiveMR, f.Branch = "idle", "clean", "", "", branch
	})
}

// forgetAgent makes agent bead id unreadable, as a bead that could not be
// read at all.
func (db *polecatDB) forgetAgent(id string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.hidden[id] = true
}

// newTestManager is NewManager with db as the bead store (nil for a real bd),
// a fake tmux when tm is non-nil, and git answered by w (a fresh, empty world
// when nil). Nothing it builds reads PATH, reaches a tmux server or runs git.
//
// With a fake store, the rig's own config reads (the rig identity bead) go to
// it too, and when the rig's beads directory exists the types sentinel is
// written there: beads.EnsureCustomTypes runs bd itself unless the sentinel
// says the custom types are already configured.
func newTestManager(r *rig.Rig, w *world, tm sessionProbe, db *polecatDB) *Manager {
	var open func(beadsSite) polecatStore
	if db != nil {
		open = db.open
		if r.IdentityBeads == nil {
			r.IdentityBeads = db
		}
		markTypesConfigured(beads.ResolveBeadsDir(r.Path))
	}
	if w == nil {
		w = newWorld()
	}
	m := newManager(r, w.repo(r.Path), tm, open)
	m.gits = w.opener()
	// util.CheckDiskSpace runs diskutil on macOS; the disk is never full.
	m.diskSpace = func(string) (util.DiskSpaceLevel, string, error) { return util.DiskSpaceOK, "", nil }
	return m
}

// markTypesConfigured writes the custom-types sentinel into an existing
// beads directory.
func markTypesConfigured(beadsDir string) {
	if info, err := os.Stat(beadsDir); err != nil || !info.IsDir() {
		return
	}
	_ = os.WriteFile(filepath.Join(beadsDir, ".gt-types-configured"), []byte(beads.TypeConfigSentinelValue()+"\n"), 0o644)
}

// fakeProbe is a tmux for the Manager: tmuxfake's sessions, plus the agent
// liveness and pane pid answers isSessionProcessDead asks for.
type fakeProbe struct {
	*tmuxfake.Server

	mu       sync.Mutex
	alive    map[string]bool
	aliveErr error
	probed   []string
	pids     map[string]string
}

// testEpoch is the fixed start of every fake clock in this package.
var testEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func newFakeProbe() *fakeProbe {
	return &fakeProbe{
		Server: tmuxfake.New(clockwork.NewFakeClockAt(testEpoch)),
		alive:  map[string]bool{},
		pids:   map[string]string{},
	}
}

func (f *fakeProbe) IsAgentAliveChecked(session string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probed = append(f.probed, session)
	if f.aliveErr != nil {
		return false, f.aliveErr
	}
	return f.alive[session], nil
}

func (f *fakeProbe) GetPanePID(target string) (string, error) {
	if ok, _ := f.HasSession(target); !ok {
		return "", fmt.Errorf("can't find pane: %s", target)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pids[target], nil
}

func (f *fakeProbe) setAlive(session string, alive bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alive[session] = alive
}

func (f *fakeProbe) probedSessions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.probed...)
}

// driveClock advances clk by step every time a goroutine blocks on it, until
// done delivers a value. It fails the test if nothing blocks within 10 s.
func driveClock[T any](t *testing.T, clk *clockwork.FakeClock, step time.Duration, done <-chan T) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		blocked := make(chan error, 1)
		go func() { blocked <- clk.BlockUntilContext(ctx, 1) }()
		select {
		case v := <-done:
			return v
		case err := <-blocked:
			if err != nil {
				t.Fatalf("nothing blocked on the fake clock: %v", err)
			}
			clk.Advance(step)
		}
	}
}
