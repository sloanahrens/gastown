package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/polecat"
)

// TestChoosePoolAgent pins the whole table: the agent AND the exact one-line
// reason, because the line is the only thing a sling prints (gt-ipk7).
func TestChoosePoolAgent(t *testing.T) {
	t.Parallel()
	s := func(agent string) poolSession { return poolSession{name: "gt-x", agent: agent} }
	capped := seatPool("deepseek-flash", 2)
	// A pool whose seat map is empty names no seat at all.
	noSeats := &config.PolecatPool{}
	// A seat with a cap of 0 is closed.
	closed := seatPool("deepseek-flash", 0)
	// Two seats, each with its own cap.
	twoSeats := &config.PolecatPool{Seats: map[string]config.SeatConfig{"deepseek-flash": {Cap: 2}, "claude-sonnet": {Cap: 2}}}
	// The live town's pool still names its retired local seat; it changes nothing.
	retiredLocal := &config.PolecatPool{LocalAgent: []byte(`"local-coder-polecat"`), MaxLocal: []byte(`0`), Seats: map[string]config.SeatConfig{"deepseek-flash": {Cap: 2}}}
	oneTaken := []poolSession{s("deepseek-flash"), s("claude-sonnet")}
	full := []poolSession{s("deepseek-flash"), s("deepseek-flash")}
	bothFull := []poolSession{s("deepseek-flash"), s("deepseek-flash"), s("claude-sonnet"), s("claude-sonnet")}
	// A full seat filled by a crashed polecat whose restart is due (gt-tldj4).
	crashed := []poolSession{{name: "dead/gastown/ember", agent: "deepseek-flash", deadHooked: true}, s("deepseek-flash")}
	// A town whose seats are each inside their own cap — flash at 2 of 3,
	// sonnet at 1 of 2 — and whose total of 3 is therefore the cap in force.
	totaled := &config.PolecatPool{Seats: map[string]config.SeatConfig{"deepseek-flash": {Cap: 3}}, MaxTotal: intPtr(3)}
	underTotal := []poolSession{s("deepseek-flash"), s("deepseek-flash"), s("claude-sonnet")}
	// The town's total with one of the three seats filled by a crashed polecat
	// whose restart is due: max_total bounds the polecats, not the live sessions,
	// so this is a town with no room left (gt-bc9b6).
	totaledCrashed := []poolSession{{name: "dead/gastown/ember", agent: "deepseek-flash", deadHooked: true}, s("deepseek-flash"), s("claude-sonnet")}
	// A pinned-only flash seat with the most room: an unnamed sling must skip it
	// for sonnet, and a sling naming flash is still admitted by its cap (gt-xtuet).
	pinnedFirst := &config.PolecatPool{Seats: map[string]config.SeatConfig{"deepseek-flash": {Cap: 3, PinnedOnly: true}, "claude-sonnet": {Cap: 2}}}
	pinnedOnly := &config.PolecatPool{Seats: map[string]config.SeatConfig{"deepseek-flash": {Cap: 2, PinnedOnly: true}}}
	// A pinned-only seat filled by a dead-hooked polecat: the crashed seat still
	// holds its room, so it is full with no live session behind it (gt-3efs0).
	pinnedFlashCrashed := []poolSession{{name: "dead/gastown/ember", agent: "deepseek-flash", deadHooked: true}, s("deepseek-flash"), s("deepseek-flash")}
	pinnedOnlyCrashed := []poolSession{{name: "dead/gastown/ember", agent: "deepseek-flash", deadHooked: true}, s("deepseek-flash")}
	sonnetCrashed := []poolSession{{name: "dead/gastown/ember", agent: "claude-sonnet", deadHooked: true}}

	const flashCap = "polecat_pool.seats.deepseek-flash.cap"
	cases := []struct {
		name        string
		pool        *config.PolecatPool
		requested   string
		sessions    []poolSession
		want        string
		wantWhy     string
		wantRefused bool
		wantRaise   string
	}{
		{"no pool", nil, "", nil, "", "pool: no polecat pool configured", false, ""},
		{"a pool without seats has no opinion", noSeats, "", nil, "", "pool: polecat_pool has no seats; using the default agent", false, ""},
		{"a pool without seats leaves a named agent to its caller", noSeats, "claude-sonnet", full, "", "pool: polecat_pool has no seats; using the default agent", false, ""},
		{"empty town takes the first seat", capped, "", nil, "deepseek-flash", "pool: seat 1/2 -> deepseek-flash", false, ""},
		{"other agents' sessions do not count", capped, "", oneTaken, "deepseek-flash", "pool: seat 2/2 -> deepseek-flash", false, ""},
		{"a full pool refuses", capped, "", full, "deepseek-flash", "pool: full (2/2) -> no seat for deepseek-flash", true, flashCap},
		{"a full pool names the dead-hooked seat that filled it", capped, "", crashed, "deepseek-flash", "pool: full (2/2, 1 dead-hooked) -> no seat for deepseek-flash", true, flashCap},
		{"a request for a seat's agent is held by its cap", capped, "deepseek-flash", full, "deepseek-flash", "pool: full (2/2) -> no seat for deepseek-flash", true, flashCap},
		{"a request for an agent with no seat stands untouched", capped, "claude-sonnet", full, "", "", false, ""},
		{"a request for the second seat's agent is admitted by that seat's cap", twoSeats, "claude-sonnet", full, "claude-sonnet", "pool: seat 1/2 -> claude-sonnet", false, ""},
		{"a full seat refuses its own agent and names its own cap", twoSeats, "claude-sonnet", bothFull, "claude-sonnet", "pool: full (2/2) -> no seat for claude-sonnet", true, "polecat_pool.seats.claude-sonnet.cap"},
		{"a seat with a cap of 0 is closed", closed, "deepseek-flash", nil, "deepseek-flash", "pool: closed (cap 0) -> no seat for deepseek-flash", true, flashCap},
		{"a request that names no agent goes to the seat with the most room", twoSeats, "", full, "claude-sonnet", "pool: seat 1/2 -> claude-sonnet", false, ""},
		{"seats with equal room are broken by name", twoSeats, "", nil, "claude-sonnet", "pool: seat 1/2 -> claude-sonnet", false, ""},
		{"a request that names no agent skips a pinned-only seat for the next open seat", pinnedFirst, "", nil, "claude-sonnet", "pool: seat 1/2 -> claude-sonnet", false, ""},
		{"a request that names no agent is refused by a pool whose only seat is pinned-only", pinnedOnly, "", nil, "", "pool: every seat is full, closed or pinned-only -> no seat for a sling that named no agent", true, "polecat_pool.seats"},
		{"a request that names a pinned-only seat's agent is admitted by its cap", pinnedFirst, "deepseek-flash", nil, "deepseek-flash", "pool: seat 1/3 -> deepseek-flash", false, ""},
		{"a pinned-only seat filled by a dead-hooked polecat is skipped for the open seat", pinnedFirst, "", pinnedFlashCrashed, "claude-sonnet", "pool: seat 1/2 -> claude-sonnet", false, ""},
		{"a dead-hooked polecat on the open seat leaves the pinned-only seat skipped", pinnedFirst, "", sonnetCrashed, "claude-sonnet", "pool: seat 2/2 -> claude-sonnet", false, ""},
		{"a pinned-only pool whose seat a dead-hooked polecat fills refuses an unnamed sling", pinnedOnly, "", pinnedOnlyCrashed, "", "pool: every seat is full, closed or pinned-only -> no seat for a sling that named no agent", true, "polecat_pool.seats"},
		{"a sling naming a pinned-only seat is held by its cap, counting the dead-hooked polecat", pinnedOnly, "deepseek-flash", pinnedOnlyCrashed, "deepseek-flash", "pool: full (2/2, 1 dead-hooked) -> no seat for deepseek-flash", true, flashCap},
		{"a request that names no agent is refused when every seat is full", twoSeats, "", bothFull, "", "pool: every seat is full, closed or pinned-only -> no seat for a sling that named no agent", true, "polecat_pool.seats"},
		{"a request that names no agent on a one-seat pool is refused by that closed seat", closed, "", nil, "deepseek-flash", "pool: closed (cap 0) -> no seat for deepseek-flash", true, "polecat_pool.seats.deepseek-flash.cap"},
		{"retired local keys change nothing", retiredLocal, "", oneTaken, "deepseek-flash", "pool: seat 2/2 -> deepseek-flash", false, ""},
		{"a seat with room answers to the town's total", totaled, "", underTotal, "deepseek-flash", "pool: total full (3/3 across every seat) -> no seat for deepseek-flash", true, "polecat_pool.max_total"},
		{"the town's total counts a dead-hooked seat, so a restart cannot push past it", totaled, "", totaledCrashed, "deepseek-flash", "pool: total full (3/3 across every seat) -> no seat for deepseek-flash", true, "polecat_pool.max_total"},
		{"the town's total answers a request for another agent", totaled, "claude-sonnet", underTotal, "claude-sonnet", "pool: total full (3/3 across every seat) -> no seat for claude-sonnet", true, "polecat_pool.max_total"},
		{"under the total the per-agent cap decides", totaled, "", oneTaken, "deepseek-flash", "pool: seat 2/3 -> deepseek-flash", false, ""},
		{"the town's total holds a pool with no seats", &config.PolecatPool{MaxTotal: intPtr(1)}, "", full, "the default agent", "pool: total full (2/1 across every seat) -> no seat for the default agent", true, "polecat_pool.max_total"},
		{"max_total 0 is no total cap", &config.PolecatPool{Seats: map[string]config.SeatConfig{"deepseek-flash": {Cap: 3}}, MaxTotal: intPtr(0)}, "", oneTaken, "deepseek-flash", "pool: seat 2/3 -> deepseek-flash", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason, refused, raise := choosePoolAgent(c.pool, c.requested, c.sessions, poolTestNow)
			if got != c.want {
				t.Errorf("agent = %q, want %q (reason: %s)", got, c.want, reason)
			}
			if reason != c.wantWhy {
				t.Errorf("reason = %q, want %q", reason, c.wantWhy)
			}
			if refused != c.wantRefused {
				t.Errorf("refused = %v, want %v (reason: %s)", refused, c.wantRefused, reason)
			}
			if raise != c.wantRaise {
				t.Errorf("raise = %q, want %q", raise, c.wantRaise)
			}
		})
	}
}

type fakeLister struct {
	sessions map[string]map[string]string // name -> env
	created  map[string]time.Time
	err      error
}

func (f *fakeLister) ListSessions() ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []string
	for n := range f.sessions {
		out = append(out, n)
	}
	return out, nil
}
func (f *fakeLister) GetEnvironment(session, key string) (string, error) {
	v, ok := f.sessions[session][key]
	if !ok {
		return "", errors.New("unknown variable")
	}
	return v, nil
}
func (f *fakeLister) GetSessionCreatedTime(name string) (time.Time, error) {
	if c, ok := f.created[name]; ok {
		return c, nil
	}
	return time.Time{}, errors.New("no such session")
}

// Only polecat sessions count, identified by GT_ROLE; witnesses, refineries
// and dogs on the same server are ignored; a polecat without GT_AGENT is
// counted with an empty agent (so it never inflates the seat count).
func TestListPolecatSessions(t *testing.T) {
	t.Parallel()
	now := time.Now()
	f := &fakeLister{
		sessions: map[string]map[string]string{
			"gt-marble":   {"GT_ROLE": "gastown/polecats/marble", "GT_AGENT": "local-coder-polecat"},
			"gt-slate":    {"GT_ROLE": "gastown/polecats/slate", "GT_AGENT": "deepseek-flash"},
			"gt-opal":     {"GT_ROLE": "gastown/polecats/opal"},
			"gt-witness":  {"GT_ROLE": "gastown/witness", "GT_AGENT": "local-coder-polecat"},
			"gt-refinery": {"GT_ROLE": "gastown/refinery", "GT_AGENT": "local-coder-polecat"},
			"hq-overseer": {"GT_ROLE": "overseer"},
			"random":      {},
		},
		created: map[string]time.Time{"gt-marble": now.Add(-time.Minute), "gt-slate": now.Add(-time.Hour)},
	}
	got, err := listPolecatSessionsWith(f, poolDispositionFor(""), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d polecat sessions, want 3: %+v", len(got), got)
	}
	agents := map[string]string{}
	for _, s := range got {
		agents[s.name] = s.agent
	}
	if agents["gt-marble"] != "local-coder-polecat" || agents["gt-slate"] != "deepseek-flash" || agents["gt-opal"] != "" {
		t.Errorf("agents: %v", agents)
	}
	// A polecat whose creation time tmux cannot report counts as just spawned.
	for _, s := range got {
		if s.name == "gt-opal" && time.Since(s.created) > time.Minute {
			t.Errorf("unknown created time should read as now, got %v", s.created)
		}
	}
	if n := poolSeatCount("deepseek-flash", got); n != 1 {
		t.Errorf("only slate sits on the deepseek-flash seat: seat count = %d, want 1", n)
	}
}

// TestListPolecatSessionsExcludesDoneWithOpenMR pins gt-2nft: a polecat whose
// own agent bead reads done with an MR still in the queue does not occupy the
// seat its session name would otherwise claim. The pool refused a 4th flash
// spawn at 3/3 with only two live flash sessions because two "done, MR ready"
// polecats were counted as occupants; this is the fix.
func TestListPolecatSessionsExcludesDoneWithOpenMR(t *testing.T) {
	t.Parallel()
	disposition := func(rigName, polecatName string) (poolSeatState, error) {
		if polecatName == "diamond" {
			return poolSeatState{Disposition: polecat.WorkstateDisposition{Verdict: polecat.WorkstateVerdictPendingMR, ReuseStatus: "idle-pr-open"}}, nil
		}
		return poolSeatState{Disposition: polecat.WorkstateDisposition{Verdict: polecat.WorkstateVerdictWorking}}, nil
	}

	f := &fakeLister{
		sessions: map[string]map[string]string{
			"gt-granite": {"GT_ROLE": "gastown/polecats/granite", "GT_AGENT": "deepseek-flash"},
			"gt-diamond": {"GT_ROLE": "gastown/polecats/diamond", "GT_AGENT": "deepseek-flash"},
		},
	}
	got, err := listPolecatSessionsWith(f, disposition, poolTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].name != "gt-granite" {
		t.Fatalf("want only granite counted, got %+v", got)
	}

	if n := poolSeatCount("deepseek-flash", got); n != 1 {
		t.Errorf("seat occupancy should count only the live working session, got %d", n)
	}
}

// TestListPolecatSessionsExcludesFinishedSeatWithPreservedCommits pins gt-b9ud0:
// a finished seat whose bead is already terminal and whose only capacity-
// counting evidence is commits preserved on its branch does not occupy the seat
// its session name would otherwise claim. Nuke preserves the branch, so the
// seat's work stays recoverable without the seat filling the roster.
func TestListPolecatSessionsExcludesFinishedSeatWithPreservedCommits(t *testing.T) {
	t.Parallel()
	disposition := func(rigName, polecatName string) (poolSeatState, error) {
		if polecatName == "agate" {
			return poolSeatState{Disposition: polecat.WorkstateDisposition{
				Verdict:                  polecat.WorkstateVerdictNeedsRecovery,
				Reason:                   "git-unpushed",
				NeedsRecovery:            true,
				CommitsPreservedOnBranch: true,
				ReuseStatus:              "idle-recovery-needed",
			}}, nil
		}
		// A recovery-blocked seat that still counts keeps occupying the pool.
		return poolSeatState{Disposition: polecat.WorkstateDisposition{Verdict: polecat.WorkstateVerdictNeedsRecovery, NeedsRecovery: true, CountsTowardCapacity: true}}, nil
	}

	f := &fakeLister{
		sessions: map[string]map[string]string{
			"gt-agate":  {"GT_ROLE": "gastown/polecats/agate", "GT_AGENT": "deepseek-flash"},
			"gt-garnet": {"GT_ROLE": "gastown/polecats/garnet", "GT_AGENT": "deepseek-flash"},
		},
	}
	got, err := listPolecatSessionsWith(f, disposition, poolTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].name != "gt-garnet" {
		t.Fatalf("want only garnet counted, got %+v", got)
	}
}

// TestListPolecatSessionsFailsOpenOnDispositionError keeps a session counted
// when the polecat's own state cannot be read: an overrun the pool exists to
// prevent (gt-md4z) is worse than one extra refused sling.
func TestListPolecatSessionsFailsOpenOnDispositionError(t *testing.T) {
	t.Parallel()
	disposition := func(rigName, polecatName string) (poolSeatState, error) {
		return poolSeatState{}, errors.New("dolt unreachable")
	}

	f := &fakeLister{
		sessions: map[string]map[string]string{
			"gt-diamond": {"GT_ROLE": "gastown/polecats/diamond", "GT_AGENT": "deepseek-flash"},
		},
	}
	got, err := listPolecatSessionsWith(f, disposition, poolTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want the session still counted on lookup failure, got %+v", got)
	}
}

// ── The pool router on fakes ────────────────────────────────────────────────

// poolTestNow is the fixed clock every pool router test decides at.
var poolTestNow = time.Date(2026, 9, 18, 16, 0, 0, 0, time.UTC)

// testPoolTown is one town's pool with every collaborator of the decision
// faked: the settings, the tmux server and the seat claim filesystem. Each
// process() is a fresh `gt sling` against it.
type testPoolTown struct {
	pool   *config.PolecatPool
	lister sessionLister
	fs     poolSeatFS
	root   string
	// dead names the PIDs whose slings are gone; every other PID is alive.
	dead    map[int]bool
	lockErr error
	// onLock runs as the decision lock is taken, standing in for another
	// sling's session starting in the window before this sling's count. A test
	// uses it to stage exactly the state the count has to see (gt-8o35b).
	onLock func()
	// locked is true while the seat decision lock is held, so a lister can
	// report whether the count was taken under it.
	locked  bool
	nextPID int
}

func newTestPoolTown(pool *config.PolecatPool) *testPoolTown {
	return &testPoolTown{
		pool:   pool,
		lister: &fakeLister{sessions: map[string]map[string]string{}, created: map[string]time.Time{}},
		fs:     newFakeSeatFS(),
		root:   "/town",
		dead:   map[int]bool{},
	}
}

// liveSessions puts polecat sessions on the fake tmux server, one per agent
// given, each created an hour before the test clock.
func (w *testPoolTown) liveSessions(agents ...string) {
	f := &fakeLister{sessions: map[string]map[string]string{}, created: map[string]time.Time{}}
	for i, agent := range agents {
		name := fmt.Sprintf("gt-live-%d", i)
		f.sessions[name] = map[string]string{"GT_ROLE": "gastown/polecats/live" + fmt.Sprint(i), "GT_AGENT": agent}
		f.created[name] = poolTestNow.Add(-time.Hour)
	}
	w.lister = f
}

// process is a router standing in for one `gt sling` process: its own claim
// store and PID, the town's shared state.
func (w *testPoolTown) process() *poolRouter {
	w.nextPID++
	clock := func() time.Time { return poolTestNow }
	return &poolRouter{
		pool:        func() *config.PolecatPool { return w.pool },
		sessions:    func() sessionLister { return w.lister },
		disposition: nil,
		seats: &poolSeatLedger{
			townRoot: w.root,
			fs:       w.fs,
			store:    &poolSeatClaimStore{},
			lock: func() (func(), error) {
				if w.lockErr != nil {
					return nil, w.lockErr
				}
				w.locked = true
				if w.onLock != nil {
					w.onLock()
				}
				return func() { w.locked = false }, nil
			},
			alive: func(pid int) bool { return !w.dead[pid] },
			pid:   w.nextPID,
			now:   clock,
		},
		now: clock,
	}
}

// claims reads the town's claims. Every caller reads a claim set it wrote
// itself, so a read failure is the test being wrong.
func (w *testPoolTown) claims(t *testing.T) []poolSeatClaim {
	t.Helper()
	claims, err := w.process().seats.read()
	if err != nil {
		t.Fatalf("reading seat claims: %v", err)
	}
	return claims
}

// sling decides as a fresh live `gt sling` would, failing the test on an error.
func (w *testPoolTown) sling(t *testing.T) (string, string) {
	t.Helper()
	agent, reason, err := w.process().route("", true)
	if err != nil {
		t.Fatalf("slinging: %v", err)
	}
	return agent, reason
}

// lockWatchingLister is a fake tmux server that records, per listing, whether
// the pool's seat-decision lock was held. The pool's count has to come from a
// listing taken under that lock (gt-8o35b), and a listing is otherwise
// indistinguishable from one taken a moment earlier — the ordering is only
// visible from inside the read.
type lockWatchingLister struct {
	*fakeLister
	town *testPoolTown
	// under is the lock state at each ListSessions call, oldest first.
	under []bool
}

func newLockWatchingLister(w *testPoolTown) *lockWatchingLister {
	return &lockWatchingLister{
		fakeLister: &fakeLister{sessions: map[string]map[string]string{}, created: map[string]time.Time{}},
		town:       w,
	}
}

func (l *lockWatchingLister) ListSessions() ([]string, error) {
	l.under = append(l.under, l.town.locked)
	return l.fakeLister.ListSessions()
}

// add puts a live polecat session on the fake tmux server, an hour old.
func (l *lockWatchingLister) add(name, agent string) {
	l.fakeLister.sessions[name] = map[string]string{
		"GT_ROLE":  "gastown/polecats/" + name,
		"GT_AGENT": agent,
	}
	l.fakeLister.created[name] = poolTestNow.Add(-time.Hour)
}

// listedUnderLock reports the listing the pool counted from: it must exist and
// must have happened with the lock held. A fake lister that was never called
// means the count came from somewhere else, which is a failure of its own.
func (l *lockWatchingLister) listedUnderLock(t *testing.T) {
	t.Helper()
	if len(l.under) == 0 {
		t.Fatal("the pool never listed the town's sessions")
	}
	for i, under := range l.under {
		if !under {
			t.Errorf("session listing %d of %d happened outside the seat-decision lock", i+1, len(l.under))
		}
	}
}

// decisionLockReleased reports whether the pool gave the lock back. A route
// that has returned holds nothing: a lock left behind makes the next sling wait
// out the 5s timeout.
func (w *testPoolTown) decisionLockReleased(t *testing.T) {
	t.Helper()
	if w.locked {
		t.Error("the seat-decision lock is still held after the route was decided")
	}
}

// The router reads the town's pool: no pool has no opinion, a tmux failure is
// an unknown count rather than a full pool, and a capped seat at its cap
// refuses with the convoy feeder's deferral marker.
func TestResolvePolecatPoolAgent(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(nil)
	r := w.process()
	if a, rs, err := r.route("", true); a != "" || rs != "" || err != nil {
		t.Errorf("no pool: got %q %q %v", a, rs, err)
	}
	w.pool = cappedPool(1)
	w.liveSessions()
	if a, _, err := r.route("", false); a != claimAgent || err != nil {
		t.Errorf("empty town: got %q %v", a, err)
	}
	w.lister = &fakeLister{err: errors.New("no server")}
	if a, rs, err := r.route("", true); a != claimAgent || !strings.Contains(rs, "cannot list sessions") || err != nil {
		t.Errorf("a lister failure is not a full pool, it is an unknown one: got %q %q %v", a, rs, err)
	}

	w.liveSessions(claimAgent)
	agent, reason, err := w.process().route("", true)
	if !errors.Is(err, errPoolBackpressure) {
		t.Fatalf("capped pool: want a refusal, got %q %q %v", agent, reason, err)
	}
	if agent != claimAgent || !strings.Contains(reason, "pool: full (1/1)") {
		t.Errorf("the refusal names the seat it could not take: %q %q", agent, reason)
	}
	// The convoy feeder defers on this prefix instead of failing the bead,
	// and the way through it names is the pool's own config — no flag spawns
	// past the cap (gt-4lbz). internal/daemon/convoy_sling_backpressure_test.go
	// pins the parse of this exact wording.
	if !strings.HasPrefix(err.Error(), "sling refused:") || !strings.HasSuffix(err.Error(), "; raise polecat_pool.seats.deepseek-flash.cap to spawn") {
		t.Errorf("refusal message must carry the deferral marker and the way through: %q", err)
	}
	if claims := w.claims(t); len(claims) != 0 {
		t.Errorf("a refused sling must not claim a seat: %v", claims)
	}
}

// The sling path the bug was reported on: `gt sling <bead> --agent claude-sonnet`
// with every seat taken came back as a backpressure refusal, so the convoy
// feeder deferred a bead that was never the pool's to place (gt-gcrk). The pool
// must answer nothing here — not a seat, not a refusal, not a claim.
func TestResolvePoolAgentLeavesANonPoolRequestUntouched(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(cappedPool(1))
	w.liveSessions(claimAgent)

	agent, reason, err := w.process().route("claude-sonnet", true)
	if err != nil || agent != "" || reason != "" {
		t.Fatalf("a non-pool request must pass through untouched: got %q %q %v", agent, reason, err)
	}
	if claims := w.claims(t); len(claims) != 0 {
		t.Errorf("a request the pool does not answer must claim no seat: %v", claims)
	}
}

// Every seat in the map is the pool's to admit and claim: a request for the
// second seat's agent takes a claim counted on that agent, and a full first seat
// neither admits it nor lends it room. The refusal for the full seat names that
// seat's own cap, so the operator raises the number that held the sling
// (gt-mukgk).
func TestRouteAdmitsAndClaimsEachSeatByItsOwnCap(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(&config.PolecatPool{Seats: map[string]config.SeatConfig{
		claimAgent:      {Cap: 1},
		"claude-sonnet": {Cap: 2},
	}})
	w.liveSessions(claimAgent)

	agent, reason, err := w.process().route("claude-sonnet", true)
	if err != nil || agent != "claude-sonnet" || !strings.Contains(reason, "seat 1/2") {
		t.Fatalf("a request for the second seat's agent is admitted by that seat's cap: got %q (%s) %v", agent, reason, err)
	}
	if claims := w.claims(t); len(claims) != 1 || claims[0].Agent != "claude-sonnet" {
		t.Fatalf("the claim is counted on the agent the sling named: %v", claims)
	}

	_, _, err = w.process().route(claimAgent, true)
	if !errors.Is(err, errPoolBackpressure) || !strings.HasSuffix(err.Error(), "; raise polecat_pool.seats."+claimAgent+".cap to spawn") {
		t.Fatalf("a full seat refuses its own agent and names its own cap: %v", err)
	}
	if claims := w.claims(t); len(claims) != 1 {
		t.Errorf("a refused sling claims no seat: %v", claims)
	}
}

// The sling surface of the town's total (gt-bcpj5): live polecats on two
// agents, each inside its own seat's cap, and the sling that would be the
// polecat past max_total is refused. The refusal names the key that raises the
// cap, because raising a seat's cap would not move it. A request for an agent the
// pool does not own answers to the total as well: the cap is the town's, and a
// polecat on any agent is one of the polecats it bounds.
func TestRouteRefusesTheTownTotal(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(&config.PolecatPool{Seats: map[string]config.SeatConfig{claimAgent: {Cap: 3}}, MaxTotal: intPtr(2)})
	w.liveSessions(claimAgent, "claude-sonnet")

	agent, reason, err := w.process().route("", true)
	if !errors.Is(err, errPoolBackpressure) {
		t.Fatalf("a town at its total must refuse: got %q (%s) %v", agent, reason, err)
	}
	if !strings.Contains(reason, "pool: total full (2/2 across every seat)") {
		t.Errorf("the refusal names the total it counted: %q", reason)
	}
	if !strings.HasSuffix(err.Error(), "; raise polecat_pool.max_total to spawn") {
		t.Errorf("the refusal must name the key that raises it, not a seat's cap: %q", err)
	}
	if claims := w.claims(t); len(claims) != 0 {
		t.Errorf("a refused sling claims no seat: %v", claims)
	}

	if _, _, err := w.process().route("claude-sonnet", true); !errors.Is(err, errPoolBackpressure) {
		t.Errorf("a request for another agent answers to the town's total: %v", err)
	}

	// One polecat fewer: the pool's own seat has room and the sling goes.
	under := newTestPoolTown(&config.PolecatPool{Seats: map[string]config.SeatConfig{claimAgent: {Cap: 3}}, MaxTotal: intPtr(3)})
	under.liveSessions(claimAgent, "claude-sonnet")
	if agent, reason := under.sling(t); agent != claimAgent || !strings.Contains(reason, "seat 2/3") {
		t.Errorf("a town under its total spends the free seat: %q (%s)", agent, reason)
	}
}

// A tmux-listing failure must not override an explicit non-pool --agent
// (gt-67fj): the fallback that answers an unknown session count with the
// pool agent ran ahead of the "not the pool's seat" check, so a request
// for claude-sonnet came back deepseek-flash whenever tmux hiccupped. The
// request has to stand untouched here exactly as it does with a clean count
// in TestResolvePoolAgentLeavesANonPoolRequestUntouched above.
func TestResolvePoolAgentLeavesANonPoolRequestUntouchedOnListSessionsFailure(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(cappedPool(1))
	w.lister = &fakeLister{err: errors.New("no server")}

	agent, reason, err := w.process().route("claude-sonnet", true)
	if err != nil || agent != "" || reason != "" {
		t.Fatalf("a non-pool request must survive a tmux-listing failure untouched: got %q %q %v", agent, reason, err)
	}
}

// The same requirement against the seat-claim read path (gt-67fj): a claims
// directory that cannot be read must not override a non-pool --agent either.
func TestResolvePoolAgentLeavesANonPoolRequestUntouchedOnSeatClaimReadFailure(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(cappedPool(3))
	seatFS := newFakeSeatFS()
	seatFS.readDirErr = errSeatFSUnreadable
	w.fs = seatFS

	agent, reason, err := w.process().route("claude-sonnet", true)
	if err != nil || agent != "" || reason != "" {
		t.Fatalf("a non-pool request must survive a seat-claim read failure untouched: got %q %q %v", agent, reason, err)
	}
	if files := seatFS.claimFiles(); len(files) != 0 {
		t.Errorf("a request the pool does not answer must claim no seat: %v", files)
	}
}

// A decision lock that cannot be taken still routes, from the live sessions
// alone, and the reason says the seat was not reserved rather than passing a
// racy decision off as a reserved one.
func TestResolvePoolAgentUnreservedWhenTheDecisionLockFails(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(cappedPool(2))
	w.lockErr = errors.New("polecat pool lock busy for 5s")

	agent, reason := w.sling(t)
	if agent != claimAgent || !strings.Contains(reason, "[seat not reserved: polecat pool lock busy for 5s]") {
		t.Errorf("got %q (%s), want the seat with the missing reservation named", agent, reason)
	}
	if claims := w.claims(t); len(claims) != 0 {
		t.Errorf("a decision taken without the lock claims no seat: %v", claims)
	}
}

// gt-8o35b: route read the pool's occupied seats before taking the
// seat-decision lock, so a sling that started its session and dropped its claim
// in the window between the two was counted by neither — the claim was gone by
// the time the lock was held, and the session was not in the snapshot taken
// before it. The count named a seat free that the other sling had taken, and
// the pool ran one over its cap (the 4/3 on the capped pool agent). Both
// reads are inside the lock now, so a seat is the session or its claim and
// never neither.
func TestPoolCountsASessionThatAppearsBeforeTheDecisionLock(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(cappedPool(2))
	lister := newLockWatchingLister(w)
	w.lister = lister
	// The concurrent sling's session starts as this sling reaches the lock:
	// after route was entered, before the count is taken.
	w.onLock = func() { lister.add("gt-other", claimAgent) }

	agent, reason, err := w.process().route("", true)
	if err != nil {
		t.Fatalf("the pool has room for one more: %v", err)
	}
	if agent != claimAgent || !strings.Contains(reason, "seat 2/2") {
		t.Errorf("the session that appeared before the lock must hold seat 1: got %q (%s)", agent, reason)
	}
	lister.listedUnderLock(t)
	w.decisionLockReleased(t)
}

// The same window with the seat already full: cap 2 and the two sessions that
// fill it appear before the lock. The sling that reaches the lock sees a full
// pool and is refused, rather than counting an empty town and spawning a third
// polecat onto a two-seat cap.
func TestPoolIsFullWhenTheCapFillsBeforeTheDecisionLock(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(cappedPool(2))
	lister := newLockWatchingLister(w)
	w.lister = lister
	w.onLock = func() {
		lister.add("gt-other-1", claimAgent)
		lister.add("gt-other-2", claimAgent)
	}

	agent, reason, err := w.process().route("", true)
	if !errors.Is(err, errPoolBackpressure) {
		t.Fatalf("a cap filled before the lock must refuse, not spawn: got %q (%s) %v", agent, reason, err)
	}
	if !strings.Contains(reason, "pool: full (2/2)") {
		t.Errorf("the refusal must count the sessions that appeared before the lock: %q", reason)
	}
	if claims := w.claims(t); len(claims) != 0 {
		t.Errorf("a refused sling claims no seat: %v", claims)
	}
	lister.listedUnderLock(t)
	w.decisionLockReleased(t)
}

// The failure path takes its count under the lock too, and gives the lock up on
// the way out. route falls back to the pool agent when it cannot list the
// town's sessions, and that fallback is only as good as the count it is decided
// from — the same count, so the same lock (gt-8o35b).
func TestPoolListsUnderTheDecisionLockWhenTheListerFails(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(cappedPool(2))
	lister := newLockWatchingLister(w)
	lister.err = errors.New("no server")
	w.lister = lister

	agent, reason, err := w.process().route("", true)
	if err != nil {
		t.Fatalf("a lister failure routes rather than failing the sling: %v", err)
	}
	if agent != claimAgent || !strings.Contains(reason, "cannot list sessions") {
		t.Errorf("the fallback names the read it could not make: %q (%s)", agent, reason)
	}
	lister.listedUnderLock(t)
	w.decisionLockReleased(t)
}

// ── Seat claims (gt-eoi9) ───────────────────────────────────────────────────

// claimAgent is the pool's agent in the seat-claim tests.
const claimAgent = "deepseek-flash"

// seatPool is a pool whose one seat, on agent, holds seatCap polecats.
func seatPool(agent string, seatCap int) *config.PolecatPool {
	return &config.PolecatPool{Seats: map[string]config.SeatConfig{agent: {Cap: seatCap}}}
}

// cappedPool is a pool whose seat holds maxSeats polecats. The fake tmux
// server has not heard of any of the slings racing each other, which is the
// moment the cap used to break.
func cappedPool(maxSeats int) *config.PolecatPool {
	return seatPool(claimAgent, maxSeats)
}

// The gt-eoi9 bug: slings started in parallel each counted the same live
// sessions, so each read a free seat and spawned, putting three polecats on a
// two-seat pool (opal, shale and agate). The seat claim the first sling leaves
// behind is what the second counts instead, and the sling past the cap is
// refused rather than spawned (gt-jzr1).
func TestPoolSeatClaimsHoldTheCapForConcurrentSlings(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(cappedPool(2))

	a1, r1 := w.sling(t)
	a2, r2 := w.sling(t)
	if a1 != claimAgent || a2 != claimAgent {
		t.Fatalf("the two seats go to the first two slings: got %q (%s) and %q (%s)", a1, r1, a2, r2)
	}
	if !strings.Contains(r1, "seat 1/2") || !strings.Contains(r2, "seat 2/2") {
		t.Errorf("each sling must see the seat it took: %q, %q", r1, r2)
	}
	if claims := w.claims(t); len(claims) != 2 {
		t.Fatalf("each admitted sling claims its seat, got %v", claims)
	}

	agent, reason, err := w.process().route("", true)
	if !errors.Is(err, errPoolBackpressure) {
		t.Fatalf("the third sling must refuse, not overfill the pool: got %q (%s) %v", agent, reason, err)
	}
	if !strings.Contains(reason, "pool: full (2/2)") {
		t.Errorf("the refusal must count the capped seat: %q", reason)
	}
	if claims := w.claims(t); len(claims) != 2 {
		t.Errorf("a refused sling claims no seat, got %v", claims)
	}
}

// The claim lives exactly as long as the seat is invisible to tmux: StartSession
// drops it, and the live session counts instead — one seat either way, never
// both and never neither.
func TestPoolSeatClaimIsHandedOverToTheSession(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(cappedPool(2))
	first := w.process()
	if a, r, err := first.route("", true); err != nil || a != claimAgent {
		t.Fatalf("empty pool: want the seat, got %q (%s) %v", a, r, err)
	}
	if claims := w.claims(t); len(claims) != 1 {
		t.Fatalf("a capped route must claim a seat, got %d claims", len(claims))
	}

	// StartSession: the tmux session is now the record of that seat.
	first.seats.store.release()
	if claims := w.claims(t); len(claims) != 0 {
		t.Fatalf("StartSession must drop the claim, %d left", len(claims))
	}

	// A second sling in a fresh process counts the live session, not a phantom
	// claim: seat 2 of 2, which is what a double-count would call "full".
	w.liveSessions(claimAgent)
	a, r := w.sling(t)
	if a != claimAgent || !strings.Contains(r, "seat 2/2") {
		t.Errorf("the live session must count as one seat, got %q (%s)", a, r)
	}
}

// A dry run reads the claims — so it prints the route a real sling would take —
// but claims nothing and drops nothing.
func TestPeekPolecatPoolAgentNeitherClaimsNorDropsSeats(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(cappedPool(2))

	if a, _, _ := w.process().route("", false); a != claimAgent {
		t.Fatalf("empty pool: the preview route should be the seat, got %q", a)
	}
	if claims := w.claims(t); len(claims) != 0 {
		t.Fatalf("a dry run must not claim a seat, got %d claims", len(claims))
	}

	if a, _ := w.sling(t); a != claimAgent {
		t.Fatalf("live sling: want the seat, got %q", a)
	}
	// Seen from another process, that claim is the second seat taken.
	if _, r, _ := w.process().route("", false); !strings.Contains(r, "seat 2/2") {
		t.Errorf("the preview must count the seat another sling claimed: %q", r)
	}
	if claims := w.claims(t); len(claims) != 1 {
		t.Errorf("a dry run must not drop another sling's claim, %d left", len(claims))
	}
}

// A claim whose sling is gone cannot become a session, so the seat is free: a
// crashed sling must not shadow a seat for the TTL, and a claim held too long
// by a process that lingers must not shadow one forever.
func TestPoolSeatClaimCleanupDropsCrashedAndStaleSlingers(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(cappedPool(2))
	const crashedPID, livePID = 9001, 9002
	w.dead[crashedPID] = true
	seats := w.process().seats
	crashed, err := seats.publish(poolSeatClaim{
		ID: "crashed-1", PID: crashedPID, Agent: claimAgent, CreatedAt: poolTestNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := seats.publish(poolSeatClaim{
		ID: "stale-1", PID: livePID, Agent: claimAgent, CreatedAt: poolTestNow.Add(-poolSeatClaimTTL - time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	held, err := seats.publish(poolSeatClaim{
		ID: "held-1", PID: livePID, Agent: claimAgent, CreatedAt: poolTestNow,
	})
	if err != nil {
		t.Fatal(err)
	}

	seats.cleanupStale()

	got := w.claims(t)
	if len(got) != 1 || got[0].ID != held.ID {
		t.Fatalf("only the live, fresh claim survives, got %v (crashed=%s stale=%s held=%s)",
			got, crashed.ID, stale.ID, held.ID)
	}
}

// A reader that does not prune (the dry run, the dispatcher's roster) must not
// count a claim whose sling is gone or past the TTL as a taken seat, and must
// leave the file where it found it (gt-zkqtn).
func TestPoolClaimSessionsSkipDeadAndExpired(t *testing.T) {
	t.Parallel()
	poolClaims := func(sessions []poolSession) []string {
		var names []string
		for _, s := range sessions {
			if strings.HasPrefix(s.name, "pool-claim/") {
				names = append(names, s.name)
			}
		}
		return names
	}
	occupied := func() ([]poolSession, error) { return nil, nil }

	t.Run("dry run and live decision through the ledger", func(t *testing.T) {
		t.Parallel()
		w := newTestPoolTown(cappedPool(3))
		const crashedPID, livePID = 9001, 9002
		w.dead[crashedPID] = true
		seats := w.process().seats
		for _, c := range []poolSeatClaim{
			{ID: "crashed-1", PID: crashedPID, Agent: claimAgent, CreatedAt: poolTestNow},
			{ID: "expired-1", PID: livePID, Agent: claimAgent, CreatedAt: poolTestNow.Add(-poolSeatClaimTTL - time.Minute)},
		} {
			if _, err := seats.publish(c); err != nil {
				t.Fatal(err)
			}
		}
		held, err := seats.publish(poolSeatClaim{ID: "held-1", PID: livePID, Agent: claimAgent, CreatedAt: poolTestNow})
		if err != nil {
			t.Fatal(err)
		}

		for _, live := range []bool{false, true} {
			sessions, d, err := w.process().seats.begin(live, occupied)
			if err != nil {
				t.Fatalf("begin(live=%v): %v", live, err)
			}
			d.done()
			if got := poolClaims(sessions); len(got) != 1 || got[0] != "pool-claim/"+held.ID {
				t.Errorf("begin(live=%v) counted %v, want only the live fresh claim", live, got)
			}
		}
	})

	t.Run("dispatcher roster on disk", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		// The claim files are real; which pid is alive and what time it is
		// are pinned, so the test names no process and reads no clock.
		const deadPID, livePID = 9001, 9002
		dir := poolSeatClaimDir(townRoot)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, c := range []poolSeatClaim{
			{ID: "crashed-1", PID: deadPID, Agent: claimAgent, CreatedAt: poolTestNow},
			{ID: "expired-1", PID: livePID, Agent: claimAgent, CreatedAt: poolTestNow.Add(-poolSeatClaimTTL - time.Minute)},
			{ID: "held-1", PID: livePID, Agent: claimAgent, CreatedAt: poolTestNow},
		} {
			data, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, c.ID+".json"), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}

		// poolSeatClaimSessions is this ledger with the live process table and
		// the wall clock; the dispatcher's roster reads through it.
		ledger := poolSeatLedgerFor(townRoot, newPoolSeatClaimStore())
		ledger.alive = func(pid int) bool { return pid != deadPID }
		ledger.now = func() time.Time { return poolTestNow }
		sessions, err := ledger.claimSessions("")
		if err != nil {
			t.Fatal(err)
		}
		if got := poolClaims(sessions); len(got) != 1 || got[0] != "pool-claim/held-1" {
			t.Errorf("roster counted %v, want only the live fresh claim", got)
		}
		for _, id := range []string{"crashed-1", "expired-1"} {
			if _, err := os.Stat(filepath.Join(dir, id+".json")); err != nil {
				t.Errorf("a reader deleted claim %s: %v", id, err)
			}
		}
	})
}

// ── A claim set that cannot be read (gt-t8q5) ───────────────────────────────

// errSeatFSUnreadable stands in for the failure a real directory read produces
// (permissions, I/O) — the one that is not "no directory yet".
var errSeatFSUnreadable = errors.New("input/output error")

// fakeSeatFS is the seat claim filesystem in memory. It records every operation,
// so a test can tell a claim that was made and dropped from one that was never
// made, and it can be told to fail its directory reads, which is how the
// fail-closed path is reached without a town on disk.
type fakeSeatFS struct {
	mu         sync.Mutex
	dirs       map[string]bool
	files      map[string][]byte
	ops        []string
	readDirErr error
}

func newFakeSeatFS() *fakeSeatFS {
	return &fakeSeatFS{dirs: map[string]bool{}, files: map[string][]byte{}}
}

func (f *fakeSeatFS) log(format string, args ...any) {
	f.ops = append(f.ops, fmt.Sprintf(format, args...))
}

// claimFiles names the claim files still present.
func (f *fakeSeatFS) claimFiles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.files))
	for path := range f.files {
		out = append(out, filepath.Base(path))
	}
	return out
}

// wroteClaim reports whether this filesystem saw a claim published: the setup
// assertion that separates "the claim was dropped on the way out" from "the
// claim was never made".
func (f *fakeSeatFS) wroteClaim() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, op := range f.ops {
		if strings.HasPrefix(op, "rename ") {
			return true
		}
	}
	return false
}

func (f *fakeSeatFS) MkdirAll(path string, _ os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs[path] = true
	f.log("mkdirall %s", path)
	return nil
}

func (f *fakeSeatFS) WriteFile(name string, data []byte, _ os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[name] = append([]byte(nil), data...)
	f.log("write %s", filepath.Base(name))
	return nil
}

func (f *fakeSeatFS) Rename(oldpath, newpath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[oldpath]
	if !ok {
		return &os.PathError{Op: "rename", Path: oldpath, Err: os.ErrNotExist}
	}
	delete(f.files, oldpath)
	f.files[newpath] = data
	f.log("rename %s", filepath.Base(newpath))
	return nil
}

func (f *fakeSeatFS) Remove(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("remove %s", filepath.Base(name))
	delete(f.files, name)
	return nil
}

func (f *fakeSeatFS) ReadFile(name string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[name]
	if !ok {
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrNotExist}
	}
	return append([]byte(nil), data...), nil
}

func (f *fakeSeatFS) ReadDir(name string) ([]os.DirEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readDirErr != nil {
		f.log("readdir %s -> %v", name, f.readDirErr)
		return nil, f.readDirErr
	}
	if !f.dirs[name] {
		// A directory nothing has written to yet, as os.ReadDir reports it.
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrNotExist}
	}
	f.log("readdir %s", name)
	var entries []os.DirEntry
	for path := range f.files {
		if filepath.Dir(path) == name {
			entries = append(entries, fakeSeatDirEntry{name: filepath.Base(path)})
		}
	}
	return entries, nil
}

type fakeSeatDirEntry struct{ name string }

func (e fakeSeatDirEntry) Name() string { return e.name }
func (e fakeSeatDirEntry) IsDir() bool  { return false }
func (e fakeSeatDirEntry) Type() os.FileMode {
	return 0
}
func (e fakeSeatDirEntry) Info() (os.FileInfo, error) {
	return nil, fmt.Errorf("fakeSeatFS entry %s carries no file info", e.name)
}

// The same requirement against a claim path that really cannot be read — a
// regular file where the directory belongs, the shape an interrupted write or a
// bad copy leaves behind — so the fail-closed behavior does not rest on the
// in-memory filesystem alone: the real one has to report ENOTDIR as a failure,
// not as "no claims yet".
func TestPoolSeatClaimReadFailureIsNamedWhenThePathIsNotADirectory(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(cappedPool(3))
	w.root = t.TempDir()
	w.fs = osPoolSeatFS{}
	if err := os.MkdirAll(filepath.Join(w.root, ".runtime"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(poolSeatClaimDir(w.root), []byte("not a directory"), 0644); err != nil {
		t.Fatal(err)
	}

	agent, reason := w.sling(t)
	if agent != claimAgent || !strings.Contains(reason, "cannot read seat claims") {
		t.Errorf("the reason names the failure rather than a clean count: %q (%s)", agent, reason)
	}
}

// A claim set that could not be read is not a claim set that is empty: the
// seats it holds are unknown, so the decision does not pass an unknown count
// off as a clean one. It routes without a reservation and says so (gt-t8q5).
func TestPoolSeatClaimReadFailureRoutesUnreserved(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(cappedPool(3))
	seatFS := newFakeSeatFS()
	seatFS.readDirErr = errSeatFSUnreadable
	w.fs = seatFS

	agent, reason := w.sling(t)
	if agent != claimAgent {
		t.Errorf("an unreadable claim set falls back to the pool's agent: got %q (%s)", agent, reason)
	}
	if !strings.Contains(reason, "cannot read seat claims") || !strings.Contains(reason, errSeatFSUnreadable.Error()) {
		t.Errorf("the reason names the failure rather than a clean count: %q", reason)
	}
	if files := seatFS.claimFiles(); len(files) != 0 {
		t.Errorf("a route taken without a reservation claims no seat: %v", files)
	}

	// A preview has to print the route a live sling would take, and that sling
	// would take the fallback.
	peekAgent, peekReason, err := w.process().route("", false)
	if err != nil || peekAgent != agent || peekReason != reason {
		t.Errorf("peek = %q %q %v, want the live sling's route %q %q", peekAgent, peekReason, err, agent, reason)
	}
}

// A claim is released on the spawn paths that fail too. The seat a failed spawn
// reserved stands for a polecat that never existed, and cleanup leaves it alone
// for the whole TTL because the process holding it is alive, so every sling in
// the meantime routes away from a seat nobody is using. A spawn that succeeds
// hands the claim to its session instead, and keeps it.
func TestSlingSeatSpawnReleasesTheClaimOfASpawnThatFailed(t *testing.T) {
	t.Parallel()
	spawnWith := func(prepareErr error) (*fakeSeatFS, *poolRouter, error) {
		w := newTestPoolTown(cappedPool(3))
		seatFS := newFakeSeatFS()
		w.fs = seatFS
		router := w.process()
		s := slingSeatSpawn{
			backpressure: func(string, string, SlingSpawnOptions) error { return nil },
			resolvePool: func(_, requested string) (string, string, error) {
				return router.route(requested, true)
			},
			releaseSeat: router.seats.store.release,
			prepare: func(_, rigName string, opts SlingSpawnOptions, _ slingStores) (*SpawnedPolecatInfo, error) {
				if prepareErr != nil {
					return nil, prepareErr
				}
				return &SpawnedPolecatInfo{RigName: rigName, agent: opts.Agent}, nil
			},
		}
		_, err := s.spawn("/town", "gastown", SlingSpawnOptions{HookBead: "gt-a"})
		return seatFS, router, err
	}

	// A rig that does not exist is one of the failures that come after the pool
	// has already claimed the seat for the spawn.
	seatFS, router, err := spawnWith(errors.New("rig 'no-such-rig' not found"))
	if err == nil || !strings.Contains(err.Error(), "no-such-rig") {
		t.Fatalf("the failure names the rig: %v", err)
	}
	if !seatFS.wroteClaim() {
		t.Fatal("setup: the failed spawn must have reached the pool decision and claimed a seat")
	}
	if files := seatFS.claimFiles(); len(files) != 0 {
		t.Errorf("a spawn that failed must leave no seat claimed behind it: %v", files)
	}
	if own := router.seats.store.ownID(); own != "" {
		t.Errorf("this process must not still think it holds a claim: %q", own)
	}

	seatFS, router, err = spawnWith(nil)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if files := seatFS.claimFiles(); len(files) != 1 || router.seats.store.ownID() == "" {
		t.Errorf("a spawn that succeeded hands its claim to the session start, got files %v own %q", files, router.seats.store.ownID())
	}
}

// The daemon dispatches concurrently inside one process: the convoy feeder and
// the convoy continuation feed both run in it, and each feeds one bead at a
// time. A claim that lived in one process-wide store would not survive that —
// the second spawn's claim would overwrite the first's, and whichever dispatch
// returned first would remove the survivor's file, leaving a seat reserved for
// a polecat that is still spawning unreserved (gt-t8q5).
func TestSeatClaimsOfConcurrentSpawnsDoNotClobberEachOther(t *testing.T) {
	t.Parallel()
	fsys := newFakeSeatFS()
	dir := poolSeatClaimDir("/town")

	// Two dispatches in flight in one process, each with the store its own
	// spawn created.
	first, second := newPoolSeatClaimStore(), newPoolSeatClaimStore()
	if first == second {
		t.Fatal("two spawns must not share a claim store")
	}
	claimIn := func(store *poolSeatClaimStore, pid int, beadID string) string {
		ledger := &poolSeatLedger{
			townRoot: "/town",
			fs:       fsys,
			store:    store,
			alive:    func(int) bool { return true },
			pid:      pid,
			now:      func() time.Time { return poolTestNow },
		}
		claim, err := ledger.write(claimAgent, beadID)
		if err != nil {
			t.Fatalf("writing the claim for %s: %v", beadID, err)
		}
		store.hold(fsys, dir, claim.ID)
		return claim.ID
	}
	firstID := claimIn(first, 4001, "gt-a")
	secondID := claimIn(second, 4002, "gt-b")
	if files := fsys.claimFiles(); len(files) != 2 {
		t.Fatalf("setup: both spawns must have a claim in hand, got %v", files)
	}

	// The first dispatch reaches its session start first and drops its claim.
	first.release()
	if files := fsys.claimFiles(); len(files) != 1 || !strings.Contains(files[0], secondID) {
		t.Fatalf("releasing one spawn's claim left %v, want the other spawn's seat (%s) still reserved", files, secondID)
	}
	if firstID == secondID {
		t.Fatalf("setup: the two claims must be distinct, both are %s", firstID)
	}

	// It is idempotent, and the second dispatch's own release is what frees the
	// second seat.
	first.release()
	if files := fsys.claimFiles(); len(files) != 1 {
		t.Fatalf("a second release of the same claim changed the claim set: %v", files)
	}
	second.release()
	if files := fsys.claimFiles(); len(files) != 0 {
		t.Fatalf("both dispatches finished with a seat still claimed: %v", files)
	}
}

// TestEachSpawnGetsItsOwnSeatClaimStore is the wiring half of the same
// invariant: realSlingSeatSpawn is called once per spawn, and the store it
// builds is what that spawn's pool decision claims in and its release drops
// from.
func TestEachSpawnGetsItsOwnSeatClaimStore(t *testing.T) {
	t.Parallel()
	a, b := realSlingSeatSpawn(), realSlingSeatSpawn()
	if a.seatClaims == nil || b.seatClaims == nil {
		t.Fatal("a spawn must carry the store its seat claim lives in")
	}
	if a.seatClaims == b.seatClaims {
		t.Fatal("two spawns share one seat claim store: one dispatch's release would drop the other's seat")
	}
	if a.seatClaims.ownID() != "" || b.seatClaims.ownID() != "" {
		t.Errorf("a spawn starts holding no claim: %q, %q", a.seatClaims.ownID(), b.seatClaims.ownID())
	}
}

// The seat-decision lock covers the count-and-claim and nothing else: the
// worktree clone and the sandbox build, which take minutes under load, run in
// prepare, after the route has returned and the lock is back. The scope is
// what keeps the lock's 5s wait from growing with another sling's clone; it is
// not what bounds a sling, whose 5m kill at its own bound is answered by the
// dispatcher's retry (gt-vaivp).
func TestTheSeatDecisionLockIsReleasedBeforeTheSpawnsSlowWork(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(cappedPool(2))
	router := w.process()
	lockedDuringPrepare := false
	s := slingSeatSpawn{
		backpressure: func(string, string, SlingSpawnOptions) error { return nil },
		resolvePool:  func(_, requested string) (string, string, error) { return router.route(requested, true) },
		releaseSeat:  router.seats.store.release,
		prepare: func(_, rigName string, opts SlingSpawnOptions, _ slingStores) (*SpawnedPolecatInfo, error) {
			lockedDuringPrepare = w.locked
			return &SpawnedPolecatInfo{RigName: rigName, agent: opts.Agent}, nil
		},
	}

	if _, err := s.spawn("/town", "gastown", SlingSpawnOptions{HookBead: "gt-a"}); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if lockedDuringPrepare {
		t.Error("the seat-decision lock was held across the spawn's slow work; every other sling waits on it")
	}
	w.decisionLockReleased(t)
}

// The claim a spawn makes travels on the spawn record, because that is what
// both ends of the claim's life hold: StartSession, once the tmux session
// exists and is the record of the seat, and the rollback, when no session ever
// will. A spawn that reached the pool decision and came back without its store
// would hold a seat nothing could release.
func TestSpawnHandsItsSeatClaimToTheSpawnRecord(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(cappedPool(3))
	seatFS := newFakeSeatFS()
	w.fs = seatFS
	router := w.process()
	s := slingSeatSpawn{
		backpressure: func(string, string, SlingSpawnOptions) error { return nil },
		resolvePool:  func(_, requested string) (string, string, error) { return router.route(requested, true) },
		releaseSeat:  router.seats.store.release,
		prepare: func(_, rigName string, opts SlingSpawnOptions, _ slingStores) (*SpawnedPolecatInfo, error) {
			return &SpawnedPolecatInfo{RigName: rigName, agent: opts.Agent}, nil
		},
		seatClaims: router.seats.store,
	}

	info, err := s.spawn("/town", "gastown", SlingSpawnOptions{HookBead: "gt-a"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if info.seatClaim != router.seats.store {
		t.Fatalf("the spawn record carries %p, want the store its claim was made in (%p)", info.seatClaim, router.seats.store)
	}
	if files := seatFS.claimFiles(); len(files) != 1 {
		t.Fatalf("setup: the successful spawn must hold one claim, got %v", files)
	}
	info.releaseSeatClaim()
	if files := seatFS.claimFiles(); len(files) != 0 {
		t.Errorf("releasing through the spawn record left %v, want the seat free", files)
	}
	// Idempotent: StartSession and the rollback can both run, and run's own
	// boundary after them.
	info.releaseSeatClaim()
	if own := router.seats.store.ownID(); own != "" {
		t.Errorf("the store still thinks it holds a claim: %q", own)
	}
}

// A dry run prints the refusal a live sling would raise — that is the route it
// would take — and still claims nothing.
func TestPeekPolecatPoolAgentReportsTheSeatRefusal(t *testing.T) {
	t.Parallel()
	w := newTestPoolTown(cappedPool(1))
	if a, _ := w.sling(t); a != claimAgent {
		t.Fatalf("setup: the seat should go to the first sling, got %q", a)
	}
	_, reason, err := w.process().route("", false)
	if !errors.Is(err, errPoolBackpressure) {
		t.Fatalf("a preview must report the refusal it would hit: %v", err)
	}
	if !strings.Contains(err.Error(), "sling refused: "+reason) {
		t.Errorf("the refusal carries the pool's own reason line: %q vs %q", err, reason)
	}
	if claims := w.claims(t); len(claims) != 1 {
		t.Errorf("a dry run must not claim or drop a seat, got %v", claims)
	}
}

// ── Admission counts the seat picture (gt-3o7zk) ────────────────────────────

// TestPoolAdmissionAndSeatPictureCountTheSameSeats pins the acceptance
// criterion that a live sling and the dispatch seat picture are one count of
// one pool: on the same fixture they name the same occupied seats, so a sling
// refuses exactly when the picture has no free seat. The mid-landing case is
// the one admission missed — gt-thy6r taught the picture that a seat whose
// polecat submitted still holds it, but poolRouter.route counted live sessions
// alone and admitted past the cap while one was landing (gt-3o7zk).
func TestPoolAdmissionAndSeatPictureCountTheSameSeats(t *testing.T) {
	t.Parallel()
	pool := seatPool(claimAgent, 1)
	oneLive := func() *fakeLister {
		return &fakeLister{sessions: map[string]map[string]string{
			"gt-jade": {"GT_ROLE": "gastown/polecats/jade", "GT_AGENT": claimAgent},
		}}
	}

	cases := []struct {
		name          string
		lister        *fakeLister
		claim         bool
		landing       bool
		submittedBead string // the bead the landing seat still waits to land
		wantOccupied  int
	}{
		{"no session, no claim, no submission: free", &fakeLister{}, false, false, "", 0},
		{"a live session holds the seat", oneLive(), false, false, "", 1},
		{"an in-flight claim holds the seat", &fakeLister{}, true, false, "", 1},
		{"a seat mid-landing holds the seat", &fakeLister{}, false, true, "gt-ruby", 1},
		{"a landing record whose bead is not submitted: free", &fakeLister{}, false, true, "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if c.claim {
				writeSeatClaim(t, town, "claim-1", claimAgent)
			}
			if c.landing {
				writeLandingSeat(t, town, "gastown", "ruby", "gt-ruby")
			}
			work := submittedWork(c.submittedBead)

			// The picture `gt daemon dispatch-check` nudges from and the spec
			// dispatcher's roster reads.
			picture, err := poolSeatSessionsWith(c.lister, town, nil, work, pool)
			if err != nil {
				t.Fatalf("poolSeatSessionsWith: %v", err)
			}
			if got := poolSeatCount(claimAgent, picture); got != c.wantOccupied {
				t.Fatalf("picture counts %d occupied seats, want %d (%+v)", got, c.wantOccupied, picture)
			}

			// A live sling against the same fixture must take the same view of
			// the same seats.
			r := &poolRouter{
				townRoot:    town,
				pool:        func() *config.PolecatPool { return pool },
				sessions:    func() sessionLister { return c.lister },
				disposition: nil,
				work:        work,
				seats:       poolSeatLedgerFor(town, newPoolSeatClaimStore()),
				now:         func() time.Time { return poolTestNow },
			}
			agent, reason, err := r.route("", true)
			if c.wantOccupied == 0 {
				if err != nil || agent != claimAgent || !strings.Contains(reason, "seat 1/1") {
					t.Fatalf("a truly free seat must be admitted: got %q (%s) %v", agent, reason, err)
				}
				return
			}
			if !errors.Is(err, errPoolBackpressure) || !strings.Contains(reason, "pool: full (1/1)") {
				t.Fatalf("admission must refuse the %d seat(s) the picture shows taken: got %q (%s) %v",
					c.wantOccupied, agent, reason, err)
			}
		})
	}
}

// TestRouteRefusesASeatMidLanding is the reported surface on its own: a live
// `gt sling` reaching executeSling with the pool's only seat held by a polecat
// that submitted and whose session is gone is refused with the typed
// backpressure, not admitted (gt-3o7zk). The refusal must not claim a seat.
func TestRouteRefusesASeatMidLanding(t *testing.T) {
	t.Parallel()
	pool := seatPool(claimAgent, 1)
	town := t.TempDir()
	writeLandingSeat(t, town, "gastown", "ruby", "gt-ruby")

	r := &poolRouter{
		townRoot:    town,
		pool:        func() *config.PolecatPool { return pool },
		sessions:    func() sessionLister { return &fakeLister{} },
		disposition: nil,
		work:        submittedWork("gt-ruby"),
		seats:       poolSeatLedgerFor(town, newPoolSeatClaimStore()),
		now:         func() time.Time { return poolTestNow },
	}
	agent, reason, err := r.route("", true)
	if !errors.Is(err, errPoolBackpressure) {
		t.Fatalf("a seat mid-landing must refuse the next sling: got %q (%s) %v", agent, reason, err)
	}
	if agent != claimAgent || !strings.Contains(reason, "pool: full (1/1)") {
		t.Errorf("the refusal names the occupied seat: %q (%s)", agent, reason)
	}
	claims, err := poolSeatLedgerFor(town, newPoolSeatClaimStore()).read()
	if err != nil {
		t.Fatalf("reading seat claims: %v", err)
	}
	if len(claims) != 0 {
		t.Errorf("a refused sling must not claim a seat: %v", claims)
	}
}

// fakeAssignedIssues answers the direct-tracking read (assignedIssueLookup)
// without a database, and records the assignee it was asked for.
type fakeAssignedIssues struct {
	issue *beads.Issue
	err   error
	seen  string
}

func (f *fakeAssignedIssues) GetAssignedIssue(assignee string) (*beads.Issue, error) {
	f.seen = assignee
	return f.issue, f.err
}

// TestPoolAssignedWorkEvidence pins the pool's read of the seat's own work: the
// bead assigned to the polecat, assessed exactly as `gt polecat list` assesses
// it (gt-tldj4's no-second-definition rule), and fail-closed when the read
// cannot be made. The read is the one that survives a hand-off: gt done clears
// the agent bead's hook_bead slot while the work bead stays assigned, so a seat
// whose turn ended DEFERRED is visible here and nowhere in the slot (gt-bc9b6).
func TestPoolAssignedWorkEvidence(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		issue      *beads.Issue
		err        error
		wantBlocks bool
		wantCounts bool
		wantSub    bool
		wantIssue  string
	}{
		{
			name:       "an assigned in_progress bead holds the seat and counts",
			issue:      &beads.Issue{ID: "gt-held", Status: string(beads.StatusInProgress), Assignee: "gastown/polecats/ember"},
			wantBlocks: true, wantCounts: true, wantIssue: "gt-held",
		},
		{
			name:       "an assigned hooked bead counts the same way",
			issue:      &beads.Issue{ID: "gt-held", Status: string(beads.IssueStatusHooked), Assignee: "gastown/polecats/ember"},
			wantBlocks: true, wantCounts: true, wantIssue: "gt-held",
		},
		{
			name:       "an unassigned seat has no evidence",
			issue:      nil,
			wantBlocks: false, wantCounts: false,
		},
		{
			name:       "a terminal assignment is no evidence",
			issue:      &beads.Issue{ID: "gt-done", Status: string(beads.StatusClosed), Assignee: "gastown/polecats/ember"},
			wantBlocks: false, wantCounts: false,
		},
		{
			name:       "a read that fails is the list path's fail-closed evidence",
			err:        errors.New("bd list: boom"),
			wantBlocks: true, wantCounts: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			bd := &fakeAssignedIssues{issue: c.issue, err: c.err}
			got := poolAssignedWorkEvidence(bd, "gastown", "ember")
			if bd.seen != "gastown/polecats/ember" {
				t.Errorf("assignee = %q, want gastown/polecats/ember", bd.seen)
			}
			if got.BlocksCleanup != c.wantBlocks {
				t.Errorf("BlocksCleanup = %v, want %v (%+v)", got.BlocksCleanup, c.wantBlocks, got)
			}
			if got.CountsTowardCapacity != c.wantCounts {
				t.Errorf("CountsTowardCapacity = %v, want %v (%+v)", got.CountsTowardCapacity, c.wantCounts, got)
			}
			if got.Submitted != c.wantSub {
				t.Errorf("Submitted = %v, want %v (%+v)", got.Submitted, c.wantSub, got)
			}
			if got.AssignedIssue != c.wantIssue {
				t.Errorf("AssignedIssue = %q, want %q (%+v)", got.AssignedIssue, c.wantIssue, got)
			}
			if got.BlocksCleanup && got.Blocker == "" {
				t.Error("evidence that blocks cleanup must name its blocker")
			}
		})
	}
}

// TestPoolAssignedWorkEvidenceMarksSubmittedWork is the half that keeps one
// seat one seat: a bead carrying gt:ready-to-land is the landing worker's, and
// poolLandingSessions counts the seat from its own record. Evidence that did not
// say so here would let the dead-hooked read count the same seat twice
// (gt-xmcyk, the gt-2nft shape).
func TestPoolAssignedWorkEvidenceMarksSubmittedWork(t *testing.T) {
	t.Parallel()
	bd := &fakeAssignedIssues{issue: &beads.Issue{
		ID:       "gt-held",
		Status:   string(beads.IssueStatusHooked),
		Assignee: "gastown/polecats/ember",
		Labels:   []string{land.LabelReadyToLand},
	}}
	got := poolAssignedWorkEvidence(bd, "gastown", "ember")
	if !got.Submitted {
		t.Fatalf("a ready-to-land assignment must read as submitted: %+v", got)
	}
	if !got.BlocksCleanup || !got.CountsTowardCapacity {
		t.Fatalf("a submitted assignment still blocks cleanup and counts: %+v", got)
	}
}

// TestPoolSeatWorkFacts pins the fold that decides whether a seat with no live
// session still holds one (gt-bc9b6, gt-tldj4). The case that mattered on
// 2026-10-12 18:31: dispatcher read 2/3 at 18:32:11 with furiosa dead and
// gt-92qx3 still assigned, slung fr-r1h, and the restart at 18:33:43 read 4/3.
// gt done had cleared the agent bead's hook_bead on the DEFERRED exit, so the
// slot named nothing while the work bead was still assigned and in_progress.
func TestPoolSeatWorkFacts(t *testing.T) {
	t.Parallel()
	anAssignment := polecatActiveWorkEvidence{
		BlocksCleanup:        true,
		RequiresRestart:      true,
		CountsTowardCapacity: true,
		Blocker:              "assigned_work=gt-held status=in_progress",
		AssignedIssue:        "gt-held",
	}
	submitted := polecatActiveWorkEvidence{
		BlocksCleanup:        true,
		RequiresRestart:      true,
		CountsTowardCapacity: true,
		Blocker:              "assigned_work=gt-held status=hooked",
		AssignedIssue:        "gt-held",
		Submitted:            true,
	}
	unreadable := polecatActiveWorkEvidence{
		BlocksCleanup: true,
		Blocker:       "assigned_work status=lookup_error: bd list: boom",
	}
	aHook := polecat.HookBeadDisposition{Blocker: "hook_bead=gt-held status=in_progress"}

	cases := []struct {
		name       string
		hd         polecat.HookBeadDisposition
		assigned   polecatActiveWorkEvidence
		wantHeld   bool
		wantBlk    string
		wantCounts bool
		wantSubm   bool
	}{
		{
			name:       "a cleared hook reference with the bead still assigned holds the seat",
			hd:         polecat.HookBeadDisposition{Safe: true},
			assigned:   anAssignment,
			wantHeld:   true,
			wantBlk:    anAssignment.Blocker,
			wantCounts: true,
		},
		{
			name:       "a hook reference holds the seat when no assignment is read",
			hd:         aHook,
			wantHeld:   true,
			wantBlk:    aHook.Blocker,
			wantCounts: true,
		},
		{
			name:       "a submitted assignment is the landing worker's, not a held seat",
			hd:         polecat.HookBeadDisposition{Safe: true},
			assigned:   submitted,
			wantHeld:   false,
			wantBlk:    submitted.Blocker,
			wantCounts: true,
		},
		{
			name:       "an unreadable assignment keeps the seat without counting it",
			hd:         polecat.HookBeadDisposition{Safe: true},
			assigned:   unreadable,
			wantHeld:   true,
			wantBlk:    unreadable.Blocker,
			wantCounts: false,
		},
		{
			name:     "a submitted hook reference is not a held seat",
			hd:       polecat.HookBeadDisposition{Safe: true, Submitted: true},
			wantHeld: false,
			wantSubm: true,
		},
		{
			name:     "neither source names work: a finished seat is free",
			hd:       polecat.HookBeadDisposition{Safe: true},
			wantHeld: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			facts := polecat.WorkstateFacts{}
			held := poolSeatWorkFacts(&facts, c.hd, c.assigned)
			if held != c.wantHeld {
				t.Errorf("held = %v, want %v", held, c.wantHeld)
			}
			if facts.ActiveWorkBlocker != c.wantBlk {
				t.Errorf("ActiveWorkBlocker = %q, want %q", facts.ActiveWorkBlocker, c.wantBlk)
			}
			if facts.ActiveWorkCountsTowardCapacity != c.wantCounts {
				t.Errorf("ActiveWorkCountsTowardCapacity = %v, want %v", facts.ActiveWorkCountsTowardCapacity, c.wantCounts)
			}
			if facts.HookBeadSubmitted != c.wantSubm {
				t.Errorf("HookBeadSubmitted = %v, want %v", facts.HookBeadSubmitted, c.wantSubm)
			}
		})
	}
}

// poolSeatReadsAnswering is the seat read's collaborators with the polecat's
// agent bead and its assigned work answered from canned values. The by-id lookup
// fails the test if it is reached: the agent bead these reads answer with
// carries neither a hook reference nor a source-issue hint, so a seat read that
// asks for one is not reading the case under test.
func poolSeatReadsAnswering(t *testing.T, fields *beads.AgentFields, assigned polecatActiveWorkEvidence) poolSeatReads {
	t.Helper()
	return poolSeatReads{
		agentBead: func(beads.Client, string) (*beads.Issue, *beads.AgentFields, error) {
			return &beads.Issue{ID: "gt-gastown-polecat-ember"}, fields, nil
		},
		assigned: func(assignedIssueLookup, string, string) polecatActiveWorkEvidence {
			return assigned
		},
		show: func(_ beads.Client, id string) (*beads.Issue, error) {
			t.Errorf("unexpected beads read of %s: the canned agent bead names no hook and no source issue", id)
			return nil, beads.ErrNotFound
		},
	}
}

// poolDispositionAnswering is the seat read poolSeatSessions takes, over the
// canned reads: the whole read — the disposition the roster counts and the work
// that holds a crashed seat — runs for real, with no database.
func poolDispositionAnswering(town string, reads poolSeatReads) polecatDispositionFunc {
	return func(rigName, polecatName string) (poolSeatState, error) {
		return poolPolecatSeatStateWith(town, rigName, polecatName, reads)
	}
}

// deferredExitFields is the agent bead a polecat leaves behind when its turn
// ends with `gt done --status DEFERRED` (or the hand-off that redirects to it):
// agent_state=stuck, the recorded cleanup status has_unpushed, and — the fact
// this bead is about — hook_bead cleared by gt done on the way out.
func deferredExitFields() *beads.AgentFields {
	return &beads.AgentFields{AgentState: "stuck", HookBead: "", CleanupStatus: string(polecat.CleanupUnpushed)}
}

// assignedActiveWork is the evidence a seat's assigned, in-progress bead
// produces (assessPolecatAssignedIssueWork): it blocks cleanup, counts toward
// capacity, and holds the seat while the work is in flight.
func assignedActiveWork(bead string) polecatActiveWorkEvidence {
	return polecatActiveWorkEvidence{
		BlocksCleanup:        true,
		RequiresRestart:      true,
		CountsTowardCapacity: true,
		Blocker:              "assigned_work=" + bead + " status=in_progress",
		AssignedIssue:        bead,
	}
}

// TestPoolRosterCountsADeferredExitSeatsWork is gt-bc9b6 end to end through the
// roster the spec dispatcher ticks on: a real town directory, the real rig and
// seat enumeration, the real seat read, with only the two beads reads stubbed.
//
// On 2026-10-12 furiosa's turn ended DEFERRED with gt-92qx3 still assigned. The
// roster read 2/3 at 18:32:11 and slung fr-r1h; patrol_scan restarted the seat
// at 18:33:43 and the town read 4/3 at 18:34:38. A seat read that trusts the
// agent bead's hook_bead slot alone finds nothing there, because gt done clears
// it on every exit — the work bead, assigned and active, is what still holds
// the seat, and it is the read the other three consumers of this question
// (daemon crash detection, patrol_scan's AssignedWork, `gt polecat list`)
// already make.
func TestPoolRosterCountsADeferredExitSeatsWork(t *testing.T) {
	t.Parallel()
	// A single-seat pool, so the held seat is the whole cap: what the roster
	// counts and what the next sling is refused on are the same seat.
	pool := seatPool("deepseek-flash", 1)
	ts := config.NewTownSettings()
	ts.PolecatPool = pool

	stillAssigned := assignedActiveWork("gt-92qx3")
	submitted := stillAssigned
	submitted.Submitted = true

	cases := []struct {
		name     string
		assigned polecatActiveWorkEvidence
		// wantSeats is the roster's Live count and the pool's own seat count;
		// wantDead is the dead-hooked breakdown the tick line names.
		wantSeats int
		wantDead  int
	}{
		{"a bead still assigned holds the crashed seat", stillAssigned, 1, 1},
		{"a seat with no work at all is free", polecatActiveWorkEvidence{}, 0, 0},
		{"a submitted bead is the landing worker's seat, counted from its record", submitted, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeTestRigsConfig(t, town, "gastown")
			writeTownPolecatDir(t, town, "gastown", "ember")
			reads := poolSeatReadsAnswering(t, deferredExitFields(), c.assigned)

			sessions, err := poolSeatSessionsWith(&fakeLister{}, town, poolDispositionAnswering(town, reads), nil, pool)
			if err != nil {
				t.Fatalf("poolSeatSessionsWith: %v", err)
			}
			r := specRosterFrom(sessions, ts)
			if r.Live["deepseek-flash"] != c.wantSeats {
				t.Fatalf("roster = %d/%d, want %d/%d (%+v)", r.Live["deepseek-flash"], 1, c.wantSeats, 1, sessions)
			}
			if r.DeadHooked["deepseek-flash"] != c.wantDead {
				t.Fatalf("dead-hooked = %d, want %d (%+v)", r.DeadHooked["deepseek-flash"], c.wantDead, sessions)
			}
			if n := poolSeatCount("deepseek-flash", sessions); n != c.wantSeats {
				t.Fatalf("pool seat count = %d, want %d (%+v)", n, c.wantSeats, sessions)
			}
			// A sling next to this seat must be refused while the crashed
			// polecat's restart is coming, and its line must name the seat that
			// filled the pool (gt-bc9b6, acceptance 2 and 3).
			_, reason, refused, raise := choosePoolAgent(pool, "", sessions, poolTestNow)
			if refused != (c.wantSeats > 0) {
				t.Fatalf("refused = %v, want %v (%s)", refused, c.wantSeats > 0, reason)
			}
			if c.wantSeats > 0 && (!strings.Contains(reason, "pool: full (1/1, 1 dead-hooked)") || raise != "polecat_pool.seats.deepseek-flash.cap") {
				t.Fatalf("the refusal names the dead-hooked seat: %q raise=%q", reason, raise)
			}
		})
	}
}

// TestPoolRosterHoldsADeferredExitSeatUnderTheTownTotal is gt-bc9b6's second
// acceptance: max_total bounds the polecats, not the live sessions, so a town
// whose seats each have room but whose total is filled by a crashed polecat
// refuses the next dispatch too.
func TestPoolRosterHoldsADeferredExitSeatUnderTheTownTotal(t *testing.T) {
	t.Parallel()
	pool := &config.PolecatPool{Seats: map[string]config.SeatConfig{"deepseek-flash": {Cap: 3}}, MaxTotal: intPtr(1)}
	town := t.TempDir()
	writeTestRigsConfig(t, town, "gastown")
	writeTownPolecatDir(t, town, "gastown", "ember")
	reads := poolSeatReadsAnswering(t, deferredExitFields(), assignedActiveWork("gt-92qx3"))

	sessions, err := poolSeatSessionsWith(&fakeLister{}, town, poolDispositionAnswering(town, reads), nil, pool)
	if err != nil {
		t.Fatalf("poolSeatSessionsWith: %v", err)
	}
	_, reason, refused, raise := choosePoolAgent(pool, "", sessions, poolTestNow)
	if !refused || raise != "polecat_pool.max_total" {
		t.Fatalf("the town's total must refuse with the dead seat counted: refused=%v raise=%q (%s)", refused, raise, reason)
	}
}

// ── Seat windows on a sling (gt-y2tm3) ──────────────────────────────────────

// windowedPool is flash shut 20:00-08:00 Chicago time beside an always-open
// sonnet seat. Flash has the more room, so a pick that ignores the window lands
// on it.
func windowedPool() *config.PolecatPool {
	return &config.PolecatPool{
		Timezone: "America/Chicago",
		Seats: map[string]config.SeatConfig{
			"deepseek-flash": {Cap: 3, Closed: "20:00-08:00"},
			"claude-sonnet":  {Cap: 2},
		},
	}
}

// chicagoAt is the wall clock in the pool's zone; September is CDT.
func chicagoAt(t *testing.T, hour, min int) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("loading America/Chicago: %v", err)
	}
	return time.Date(2026, 9, 18, hour, min, 0, 0, loc)
}

// TestChoosePoolAgentSeatWindows pins that a sling naming no agent is placed
// only on a seat open at the clock it is given, and that a sling naming a closed
// seat's agent is still admitted by the cap alone.
func TestChoosePoolAgentSeatWindows(t *testing.T) {
	t.Parallel()
	s := func(agent string) poolSession { return poolSession{name: "gt-x", agent: agent} }
	pool := windowedPool()
	flashOnly := &config.PolecatPool{Timezone: "America/Chicago", Seats: map[string]config.SeatConfig{"deepseek-flash": {Cap: 3, Closed: "20:00-08:00"}}}
	noZone := &config.PolecatPool{Seats: map[string]config.SeatConfig{"deepseek-flash": {Cap: 3, Closed: "20:00-08:00"}, "claude-sonnet": {Cap: 2}}}
	badRange := &config.PolecatPool{Timezone: "America/Chicago", Seats: map[string]config.SeatConfig{"deepseek-flash": {Cap: 3, Closed: "bogus"}, "claude-sonnet": {Cap: 2}}}
	sonnetFull := []poolSession{s("claude-sonnet"), s("claude-sonnet")}
	flashFull := []poolSession{s("deepseek-flash"), s("deepseek-flash"), s("deepseek-flash")}
	// A pinned-only seat that also keeps the window's hours: both rules take it
	// out of an unnamed choice, and a dead-hooked polecat in it holds its room
	// (gt-3efs0).
	pinnedWindow := &config.PolecatPool{Timezone: "America/Chicago", Seats: map[string]config.SeatConfig{"deepseek-flash": {Cap: 3, PinnedOnly: true, Closed: "20:00-08:00"}, "claude-sonnet": {Cap: 2}}}
	pinnedWindowOnly := &config.PolecatPool{Timezone: "America/Chicago", Seats: map[string]config.SeatConfig{"deepseek-flash": {Cap: 3, PinnedOnly: true, Closed: "20:00-08:00"}}}
	flashCrashedFull := []poolSession{{name: "dead/gastown/ember", agent: "deepseek-flash", deadHooked: true}, s("deepseek-flash"), s("deepseek-flash")}

	const (
		everySeat = "pool: every seat is full, closed or pinned-only -> no seat for a sling that named no agent"
		flashCap  = "polecat_pool.seats.deepseek-flash.cap"
	)
	cases := []struct {
		name        string
		pool        *config.PolecatPool
		requested   string
		sessions    []poolSession
		now         time.Time
		want        string
		wantWhy     string
		wantRefused bool
		wantRaise   string
	}{
		{"by day the seat with the most room wins", pool, "", nil, chicagoAt(t, 12, 0), "deepseek-flash", "pool: seat 1/3 -> deepseek-flash", false, ""},
		{"at 21:00 an unnamed sling skips the closed seat with the most room", pool, "", nil, chicagoAt(t, 21, 0), "claude-sonnet", "pool: seat 1/2 -> claude-sonnet", false, ""},
		{"at 03:00 the window still holds past midnight", pool, "", nil, chicagoAt(t, 3, 0), "claude-sonnet", "pool: seat 1/2 -> claude-sonnet", false, ""},
		{"the window opens at its end", pool, "", nil, chicagoAt(t, 8, 0), "deepseek-flash", "pool: seat 1/3 -> deepseek-flash", false, ""},
		{"the window closes at its start", pool, "", nil, chicagoAt(t, 20, 0), "claude-sonnet", "pool: seat 1/2 -> claude-sonnet", false, ""},
		{"at 21:00 with every open seat full an unnamed sling is refused", pool, "", sonnetFull, chicagoAt(t, 21, 0), "", everySeat, true, "polecat_pool.seats"},
		{"a one-seat pool whose seat is shut refuses an unnamed sling", flashOnly, "", nil, chicagoAt(t, 21, 0), "", everySeat, true, "polecat_pool.seats"},
		{"a one-seat pool whose seat is open still shows the seat's own count", flashOnly, "", flashFull, chicagoAt(t, 12, 0), "deepseek-flash", "pool: full (3/3) -> no seat for deepseek-flash", true, flashCap},
		{"a sling naming the closed seat's agent is admitted by the cap alone", pool, "deepseek-flash", nil, chicagoAt(t, 21, 0), "deepseek-flash", "pool: seat 1/3 -> deepseek-flash", false, ""},
		{"a sling naming the closed seat's agent is still held by its cap", pool, "deepseek-flash", flashFull, chicagoAt(t, 21, 0), "deepseek-flash", "pool: full (3/3) -> no seat for deepseek-flash", true, flashCap},
		{"a window with no timezone reads as shut", noZone, "", nil, chicagoAt(t, 12, 0), "claude-sonnet", "pool: seat 1/2 -> claude-sonnet", false, ""},
		{"by day an unnamed sling skips a pinned-only seat even while its window is open", pinnedWindow, "", nil, chicagoAt(t, 12, 0), "claude-sonnet", "pool: seat 1/2 -> claude-sonnet", false, ""},
		{"at 21:00 an unnamed sling skips a seat that is both pinned-only and shut", pinnedWindow, "", nil, chicagoAt(t, 21, 0), "claude-sonnet", "pool: seat 1/2 -> claude-sonnet", false, ""},
		{"at 21:00 with the open seat full a pinned-only shut seat refuses an unnamed sling", pinnedWindow, "", sonnetFull, chicagoAt(t, 21, 0), "", everySeat, true, "polecat_pool.seats"},
		{"a one-seat pool whose seat is pinned-only refuses an unnamed sling even by day", pinnedWindowOnly, "", nil, chicagoAt(t, 12, 0), "", everySeat, true, "polecat_pool.seats"},
		{"a one-seat pool pinned-only and full from a dead-hooked polecat names no dead-hooked count for an unnamed sling", pinnedWindowOnly, "", flashCrashedFull, chicagoAt(t, 12, 0), "", everySeat, true, "polecat_pool.seats"},
		{"a sling naming a pinned-only shut seat is admitted by the cap alone", pinnedWindow, "deepseek-flash", nil, chicagoAt(t, 21, 0), "deepseek-flash", "pool: seat 1/3 -> deepseek-flash", false, ""},
		{"a sling naming a pinned-only shut seat is held by its cap, naming the dead-hooked polecat", pinnedWindow, "deepseek-flash", flashCrashedFull, chicagoAt(t, 21, 0), "deepseek-flash", "pool: full (3/3, 1 dead-hooked) -> no seat for deepseek-flash", true, flashCap},
		{"a window that does not parse reads as shut", badRange, "", nil, chicagoAt(t, 12, 0), "claude-sonnet", "pool: seat 1/2 -> claude-sonnet", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason, refused, raise := choosePoolAgent(c.pool, c.requested, c.sessions, c.now)
			if got != c.want || reason != c.wantWhy || refused != c.wantRefused || raise != c.wantRaise {
				t.Errorf("got (%q, %q, refused=%v, raise=%q), want (%q, %q, refused=%v, raise=%q)",
					got, reason, refused, raise, c.want, c.wantWhy, c.wantRefused, c.wantRaise)
			}
		})
	}
}

// TestPoolUncountedFallback pins the agent a decision falls back to when it
// cannot count the pool: the requested agent stands, else the first seat by
// name that has a cap and is open at the clock, else no agent at all.
func TestPoolUncountedFallback(t *testing.T) {
	t.Parallel()
	night := chicagoAt(t, 21, 0)
	shutFirst := &config.PolecatPool{
		Timezone: "America/Chicago",
		Seats: map[string]config.SeatConfig{
			"a-shut": {Cap: 1, Closed: "20:00-08:00"},
			"b-open": {Cap: 1},
		},
	}

	cases := []struct {
		name      string
		pool      *config.PolecatPool
		requested string
		now       time.Time
		agent     string
		named     string
	}{
		{"a named agent stands even when a seat is open", windowedPool(), "deepseek-flash", night, "deepseek-flash", "deepseek-flash"},
		{"an unnamed sling takes the first open seat by name", windowedPool(), "", night, "claude-sonnet", "claude-sonnet"},
		{"a shut window is skipped for the next open seat by name", shutFirst, "", night, "b-open", "b-open"},
		{"a seat at cap 0 is never the fallback", &config.PolecatPool{Seats: map[string]config.SeatConfig{"a-closed": {Cap: 0}, "b-open": {Cap: 1}}}, "", night, "b-open", "b-open"},
		{"a pinned-only seat is never the fallback", &config.PolecatPool{Seats: map[string]config.SeatConfig{"a-pinned": {Cap: 1, PinnedOnly: true}, "b-open": {Cap: 1}}}, "", night, "b-open", "b-open"},
		{"a pinned-only seat inside its closed window is never the fallback", &config.PolecatPool{Timezone: "America/Chicago", Seats: map[string]config.SeatConfig{"a-pinned-shut": {Cap: 1, PinnedOnly: true, Closed: "20:00-08:00"}, "b-open": {Cap: 1}}}, "", night, "b-open", "b-open"},
		{"no open seat leaves the default agent to run", &config.PolecatPool{Timezone: "America/Chicago", Seats: map[string]config.SeatConfig{"claude-sonnet": {Cap: 0}, "deepseek-flash": {Cap: 3, Closed: "20:00-08:00"}}}, "", night, "", "the default agent"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			agent, named := poolUncountedFallback(c.pool, c.requested, c.now)
			if agent != c.agent || named != c.named {
				t.Errorf("poolUncountedFallback = %q, %q; want %q, %q", agent, named, c.agent, c.named)
			}
		})
	}
}

// TestRouteUnnamedSlingReadsTheRouterClock is the sling end of gt-y2tm3: the
// router hands its own clock to the choice, so a live `gt sling` with no agent
// at 21:00 claims the open seat, and one with the open seat full is refused
// naming polecat_pool.seats, while a sling naming the shut seat's agent is
// admitted.
func TestRouteUnnamedSlingReadsTheRouterClock(t *testing.T) {
	t.Parallel()
	night := chicagoAt(t, 21, 0)
	atNight := func(w *testPoolTown) *poolRouter {
		r := w.process()
		r.now = func() time.Time { return night }
		return r
	}

	t.Run("an unnamed sling claims the open seat", func(t *testing.T) {
		t.Parallel()
		w := newTestPoolTown(windowedPool())
		agent, reason, err := atNight(w).route("", true)
		if err != nil || agent != "claude-sonnet" {
			t.Fatalf("route = %q (%s) %v, want claude-sonnet", agent, reason, err)
		}
		if claims := w.claims(t); len(claims) != 1 || claims[0].Agent != "claude-sonnet" {
			t.Errorf("claims = %+v, want one on claude-sonnet", claims)
		}
	})

	t.Run("an unnamed sling with every open seat full is refused", func(t *testing.T) {
		t.Parallel()
		w := newTestPoolTown(windowedPool())
		w.liveSessions("claude-sonnet", "claude-sonnet")
		agent, reason, err := atNight(w).route("", true)
		if !errors.Is(err, errPoolBackpressure) || agent != "" {
			t.Fatalf("route = %q (%s) %v, want a backpressure refusal", agent, reason, err)
		}
		if !strings.Contains(err.Error(), "raise polecat_pool.seats to spawn") {
			t.Errorf("the refusal must name polecat_pool.seats: %v", err)
		}
		if claims := w.claims(t); len(claims) != 0 {
			t.Errorf("a refused sling must not claim a seat: %+v", claims)
		}
	})

	t.Run("a sling naming the shut seat's agent is admitted", func(t *testing.T) {
		t.Parallel()
		w := newTestPoolTown(windowedPool())
		agent, reason, err := atNight(w).route("deepseek-flash", true)
		if err != nil || agent != "deepseek-flash" {
			t.Fatalf("route = %q (%s) %v, want deepseek-flash", agent, reason, err)
		}
	})

	t.Run("an unreadable count falls back to an open seat", func(t *testing.T) {
		t.Parallel()
		w := newTestPoolTown(windowedPool())
		w.lister = &fakeLister{err: errors.New("tmux: no server")}
		agent, reason, err := atNight(w).route("", true)
		if err != nil || agent != "claude-sonnet" {
			t.Fatalf("route = %q (%s) %v, want the open seat", agent, reason, err)
		}
	})
}

// A seat whose leftover commits the landing already took is reusable in `gt
// polecat list` (reason landed), so the pool must read it the same way and not
// count it against capacity as git-unpushed (gt-wfxa2). Without the landed
// head it stays an unpushed seat in both.
func TestPoolPolecatSeatStateLandedSeatIsReusable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		landedRef  bool
		wantSafe   bool
		wantReason string
		wantCounts bool
	}{
		{name: "landed head holds the seat's commit", landedRef: true, wantSafe: true, wantReason: "landed"},
		{name: "no landed head", wantReason: "git-unpushed", wantCounts: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			seat := newLandedSeat(t, tt.landedRef)
			town := t.TempDir()
			writeTestRigsConfig(t, town, "gastown")
			// The pool resolves the worktree by directory; the probe and the
			// landed repository are the modeled ones.
			if err := os.MkdirAll(filepath.Join(town, "gastown", "polecats", "nitro", "gastown"), 0o755); err != nil {
				t.Fatalf("mkdir worktree: %v", err)
			}
			live := polecat.LiveGitState{Branch: landedSeatBranch, Source: polecat.GitStateSourceLive, UnpushedCommits: 1}
			fields := &beads.AgentFields{AgentState: string(beads.AgentStateIdle), CleanupStatus: string(polecat.CleanupClean), Branch: landedSeatBranch}
			reads := poolSeatReadsAnswering(t, fields, polecatActiveWorkEvidence{})
			reads.show = func(_ beads.Client, id string) (*beads.Issue, error) { return seatBeads(t).Show(id) }
			reads.probe = func(string) polecat.LiveGitState { return live }
			reads.openWorkLanded = seat.open()

			state, err := poolPolecatSeatStateWith(town, "gastown", "nitro", reads)
			if err != nil {
				t.Fatalf("poolPolecatSeatStateWith: %v", err)
			}
			d := state.Disposition
			if d.SafeToNuke != tt.wantSafe || d.Reason != tt.wantReason {
				t.Fatalf("pool disposition = %+v; want safe_to_nuke=%v reason=%q", d, tt.wantSafe, tt.wantReason)
			}

			// The list path's verdict for the same seat.
			item := buildPolecatInventoryItem("gastown", "nitro", fields, nil, polecatSessionSet{}, polecatInventoryEnv{
				WorktreePath:   seat.worktree,
				IssueSource:    seatBeads(t),
				openWorkLanded: seat.open(),
				probe:          func(string, bool) polecat.LiveGitState { return live },
			})
			if got := item.Disposition; got.SafeToNuke != d.SafeToNuke || got.Reason != d.Reason || got.ReuseStatus != d.ReuseStatus {
				t.Fatalf("pool disposition %+v differs from the list's %+v", d, got)
			}

			if d.CountsTowardCapacity != tt.wantCounts {
				t.Fatalf("seat counts toward capacity = %v, want %v (%+v)", d.CountsTowardCapacity, tt.wantCounts, d)
			}
		})
	}
}

// A landed lookup that fails is not evidence of a landing: the seat stays
// git-unpushed in the pool and the list, and the blocker names the failed read
// so the reader can tell it from a bead that was read and is still open
// (gt-0laxm).
func TestPoolPolecatSeatStateLandedLookupFailureStaysUnpushed(t *testing.T) {
	t.Parallel()
	seat := newLandedSeat(t, false)
	town := t.TempDir()
	writeTestRigsConfig(t, town, "gastown")
	if err := os.MkdirAll(filepath.Join(town, "gastown", "polecats", "nitro", "gastown"), 0o755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}
	lookupErr := errors.New("bd show: connection refused")
	live := polecat.LiveGitState{Branch: landedSeatBranch, Source: polecat.GitStateSourceLive, UnpushedCommits: 1}
	fields := &beads.AgentFields{AgentState: string(beads.AgentStateIdle), CleanupStatus: string(polecat.CleanupClean), Branch: landedSeatBranch}
	reads := poolSeatReadsAnswering(t, fields, polecatActiveWorkEvidence{})
	reads.show = func(beads.Client, string) (*beads.Issue, error) { return nil, lookupErr }
	reads.probe = func(string) polecat.LiveGitState { return live }
	reads.openWorkLanded = seat.open()

	state, err := poolPolecatSeatStateWith(town, "gastown", "nitro", reads)
	if err != nil {
		t.Fatalf("poolPolecatSeatStateWith: %v", err)
	}
	d := state.Disposition
	if d.SafeToNuke || d.Reason != "git-unpushed" || !d.CountsTowardCapacity {
		t.Fatalf("pool disposition = %+v; want unsafe git-unpushed counting toward capacity", d)
	}
	if !slices.ContainsFunc(d.Blockers, func(b string) bool { return strings.Contains(b, "read bead gt-seat") }) {
		t.Fatalf("pool blockers = %q; want the failed landed read named", d.Blockers)
	}

	item := buildPolecatInventoryItem("gastown", "nitro", fields, nil, polecatSessionSet{}, polecatInventoryEnv{
		WorktreePath:   seat.worktree,
		IssueSource:    issueReadFunc(func(string) (*beads.Issue, error) { return nil, lookupErr }),
		openWorkLanded: seat.open(),
		probe:          func(string, bool) polecat.LiveGitState { return live },
	})
	if got := item.Disposition; got.SafeToNuke != d.SafeToNuke || got.Reason != d.Reason || !slices.Equal(got.Blockers, d.Blockers) {
		t.Fatalf("pool disposition %+v differs from the list's %+v", d, got)
	}
}

// TestPoolRouterReadsSeatsThroughTheBatch is the sling's seat picture over two
// rigs of three idle polecat directories each. The router reads them through
// the rig batch: one bd call per rig, the assigned-work list that also answers
// the agent beads it carries, where the per-seat read spent two on each of the
// six. The idle polecats hold no seat, so the count is empty (gt-4efmk).
func TestPoolRouterReadsSeatsThroughTheBatch(t *testing.T) {
	t.Parallel()
	town, db := seatBatchTown(t)
	reads := newSpecTickReads()
	r := seatBatchRouter(town, db, reads, &fakeLister{sessions: map[string]map[string]string{}})

	got, err := r.occupied()()
	if err != nil {
		t.Fatalf("occupied: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("idle polecats with no session hold no seat, got %+v", got)
	}
	if calls := reads.Calls(); calls != 2 {
		t.Fatalf("bd calls = %d, want 2 (one assigned-work list per rig)", calls)
	}
}

// snapshotOccupied is the occupancy read of a dispatched sling: the live
// sessions of the moment plus the dead-hooked and mid-landing seats of the
// tick's roster snapshot, and nothing read per polecat directory.
func TestSnapshotOccupied(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	lister := &fakeLister{sessions: map[string]map[string]string{
		"gt-capable": {"GT_ROLE": "gastown/polecats/capable", "GT_AGENT": "claude-haiku"},
		"gt-dag":     {"GT_ROLE": "gastown/polecats/dag", "GT_AGENT": "deepseek-flash"},
		"gt-crew":    {"GT_ROLE": "gastown/crew/sloan"},
	}}
	snapshot := []poolSession{
		{name: "gt-capable", agent: "claude-haiku", rig: "gastown", polecat: "capable"}, // live then and now: the listing wins
		{name: "gt-old", agent: "claude-sonnet", rig: "gastown", polecat: "old"},        // live then, gone now: dropped
		{name: "dead/devops/mariner", agent: "deepseek-flash", rig: "devops", polecat: "mariner", deadHooked: true},
		{name: "dead/gastown/dag", agent: "deepseek-flash", rig: "gastown", polecat: "dag", deadHooked: true}, // its seat is live now: dropped
		{name: "landing/gastown/nux", agent: "claude-sonnet", rig: "gastown", polecat: "nux", landing: true},
		{name: "pool-claim/abc", agent: "claude-haiku"}, // the ledger appends the claims of the moment itself
	}
	got, err := snapshotOccupied(poolSeatSnapshot{sessions: snapshot, at: now}, func() sessionLister { return lister }, nil, func() time.Time { return now })()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range got {
		names = append(names, s.name)
	}
	sort.Strings(names)
	want := []string{"dead/devops/mariner", "gt-capable", "gt-dag", "landing/gastown/nux"}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("occupied = %v, want %v", names, want)
	}
	lister.err = errors.New("tmux down")
	if _, err := snapshotOccupied(poolSeatSnapshot{sessions: snapshot, at: now}, func() sessionLister { return lister }, nil, func() time.Time { return now })(); err == nil {
		t.Fatal("a failed session listing must fail the read, not count the snapshot alone")
	}

	// A seat the roster read live can die before this listing: the listing drops
	// it as dead, and the roster's live entry must still count as the
	// dead-hooked seat its own dead read would list (gt-0axfg). A seat the
	// roster released stays free: the roster read its bead terminal.
	rosterAt := now
	ember := poolSession{name: "gt-ember", agent: "deepseek-flash", rig: "gastown", polecat: "ember", created: rosterAt.Add(-time.Hour)}
	for _, tc := range []struct {
		name           string
		listed         bool
		dead           bool
		released       bool
		want           int
		wantDeadHooked bool
	}{
		{name: "live in the listing", listed: true, want: 1},
		{name: "died mid-tick", listed: true, dead: true, want: 1, wantDeadHooked: true},
		{name: "gone from the listing", want: 0},
		{name: "released by the roster", listed: true, dead: true, released: true, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deadLister := &fakeLister{sessions: map[string]map[string]string{}, created: map[string]time.Time{}}
			if tc.listed {
				deadLister.sessions["gt-ember"] = map[string]string{"GT_ROLE": "gastown/polecats/ember", "GT_AGENT": "deepseek-flash"}
				deadLister.created["gt-ember"] = ember.created
			}
			snap := poolSeatSnapshot{sessions: []poolSession{ember}, at: rosterAt}
			if tc.released {
				snap.released = map[string]bool{"gastown/ember": true}
			}
			dead := func(sessionName, rigName, polecatName string) bool { return tc.dead }
			got, err := snapshotOccupied(snap, func() sessionLister { return deadLister }, dead, func() time.Time { return rosterAt.Add(time.Minute) })()
			if err != nil {
				t.Fatal(err)
			}
			if n := poolSeatCount("deepseek-flash", got); n != tc.want {
				t.Fatalf("poolSeatCount = %d, want %d: %+v", n, tc.want, got)
			}
			for _, s := range got {
				if s.deadHooked != tc.wantDeadHooked {
					t.Fatalf("deadHooked = %v, want %v: %+v", s.deadHooked, tc.wantDeadHooked, got)
				}
			}
		})
	}

	// A snapshot entry that names no seat (a claim the dispatcher appends) is
	// never read as a dead seat: the dead answer names seats by "rig/polecat"
	// and the entry names none. The roster read stage drops such an entry, and
	// an entry the roster already counted dead-hooked is carried as it stands.
	claim := poolSession{name: "pool-claim/abc", agent: "deepseek-flash", created: rosterAt.Add(-time.Hour)}
	deadLister := &fakeLister{sessions: map[string]map[string]string{
		"gt-ember": {"GT_ROLE": "gastown/polecats/ember", "GT_AGENT": "deepseek-flash"},
	}, created: map[string]time.Time{"gt-ember": ember.created}}
	dead := func(string, string, string) bool { return true }
	read := func() []poolSession {
		got, err := snapshotOccupied(poolSeatSnapshot{sessions: []poolSession{claim}, at: rosterAt}, func() sessionLister { return deadLister }, dead, func() time.Time { return rosterAt.Add(time.Minute) })()
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := read(); poolSeatCount("deepseek-flash", got) != 0 {
		t.Fatalf("a seatless live entry must not count as a dead seat: %+v", got)
	}
	claim.deadHooked = true
	if got := read(); poolSeatCount("deepseek-flash", got) != 1 {
		t.Fatalf("a seatless dead-hooked entry must be carried: %+v", got)
	}
}

// A cap-1 pool whose only live session is an idle-pr-open seat: the roster
// reads the seat free, and the snapshot a spec sling decides from must too. It
// counted every live session, so the budget freed the seat and the sling
// refused it with "pool: full", every tick until the session ended.
func TestSnapshotOccupiedReleasesTheSeatsTheRosterReleased(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "gastown")
	rosterAt := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return rosterAt.Add(time.Minute) }
	lister := &fakeLister{
		sessions: map[string]map[string]string{
			"gt-diamond": {"GT_ROLE": "gastown/polecats/diamond", "GT_AGENT": "claude-haiku"},
		},
		created: map[string]time.Time{"gt-diamond": rosterAt.Add(-time.Hour)},
	}
	disposition := func(rigName, polecatName string) (poolSeatState, error) {
		return poolSeatState{Disposition: polecat.WorkstateDisposition{Verdict: polecat.WorkstateVerdictPendingMR, ReuseStatus: "idle-pr-open"}}, nil
	}
	pool := seatPool("claude-haiku", 1)
	snap, err := poolSeatSnapshotWith(lister, town, disposition, nil, pool, func() time.Time { return rosterAt })
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	if len(snap.sessions) != 0 {
		t.Fatalf("the roster counts an idle-pr-open seat free, got %+v", snap.sessions)
	}

	got, err := snapshotOccupied(snap, func() sessionLister { return lister }, nil, now)()
	if err != nil {
		t.Fatal(err)
	}
	if n := poolSeatCount("claude-haiku", got); n != 0 {
		t.Fatalf("the spec snapshot counts the idle-pr-open seat occupied (%d), %+v", n, got)
	}

	r := poolRouterForWith(town, newPoolSeatClaimStore(), func(string) beads.Client { return beadsfake.New() })
	r.pool = func() *config.PolecatPool { return pool }
	r.sessions = func() sessionLister { return lister }
	r.now = now
	agent, reason, err := r.routeWith("claude-haiku", true, snapshotOccupied(snap, func() sessionLister { return lister }, nil, now))
	if err != nil || agent != "claude-haiku" {
		t.Fatalf("a spec sling is refused the seat the roster freed: agent %q, reason %q, err %v", agent, reason, err)
	}

	// A session that is new since the roster is not the one it released, even
	// on the same seat: it counts.
	lister.created["gt-diamond"] = rosterAt.Add(time.Second)
	got, err = snapshotOccupied(snap, func() sessionLister { return lister }, nil, now)()
	if err != nil {
		t.Fatal(err)
	}
	if n := poolSeatCount("claude-haiku", got); n != 1 {
		t.Fatalf("a session created after the roster counts, got %d: %+v", n, got)
	}

	// A zero creation time is unknown, not old: the session counts.
	lister.created["gt-diamond"] = time.Time{}
	got, err = snapshotOccupied(snap, func() sessionLister { return lister }, nil, now)()
	if err != nil {
		t.Fatal(err)
	}
	if n := poolSeatCount("claude-haiku", got); n != 1 {
		t.Fatalf("a session with a zero creation time counts, got %d: %+v", n, got)
	}
}

// The recorder notes the seats the one seat-free predicate releases, and no
// seat whose state could not be read.
func TestReleasedSeatRecorder(t *testing.T) {
	t.Parallel()
	states := map[string]func() (poolSeatState, error){
		"idle": func() (poolSeatState, error) {
			return poolSeatState{Disposition: polecat.WorkstateDisposition{ReuseStatus: "idle-pr-open"}}, nil
		},
		"preserved": func() (poolSeatState, error) {
			return poolSeatState{Disposition: polecat.WorkstateDisposition{CommitsPreservedOnBranch: true}}, nil
		},
		"working": func() (poolSeatState, error) { return poolSeatState{}, nil },
		"unread":  func() (poolSeatState, error) { return poolSeatState{}, errors.New("bd down") },
	}
	recorder, released := releasedSeatRecorder(func(rigName, polecatName string) (poolSeatState, error) {
		return states[polecatName]()
	})
	for name := range states {
		if _, err := recorder("gastown", name); (err != nil) != (name == "unread") {
			t.Fatalf("%s: the recorder must pass the read through, err %v", name, err)
		}
	}
	got := released()
	want := map[string]bool{"gastown/idle": true, "gastown/preserved": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("released = %v, want %v", got, want)
	}
	if rec, rel := releasedSeatRecorder(nil); rec != nil || len(rel()) != 0 {
		t.Fatal("no disposition records nothing and stays nil, a caller that fails open")
	}
}

// Two slings of one tick decide their seats concurrently over the tick's
// snapshot: the in-flight claim each writes under the decision lock is what
// keeps them off one seat, so a single-seat pool admits exactly one.
func TestRouteWithSnapshotAdmitsOneOfTwoConcurrentSlings(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "gastown")
	db := beadsfake.New()
	lister := &fakeLister{sessions: map[string]map[string]string{}}
	now := func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) }
	occupied := snapshotOccupied(poolSeatSnapshot{}, func() sessionLister { return lister }, nil, now)
	type outcome struct {
		agent string
		err   error
	}
	results := make([]outcome, 2)
	var start, done sync.WaitGroup
	start.Add(1)
	done.Add(2)
	for i := range results {
		go func(i int) {
			defer done.Done()
			r := poolRouterForWith(town, newPoolSeatClaimStore(), func(string) beads.Client { return db })
			r.pool = func() *config.PolecatPool { return seatPool("claude-haiku", 1) }
			r.sessions = func() sessionLister { return lister }
			r.now = now
			start.Wait()
			agent, _, err := r.routeWith("claude-haiku", true, occupied)
			results[i] = outcome{agent: agent, err: err}
		}(i)
	}
	start.Done()
	done.Wait()
	admitted, refused := 0, 0
	for _, o := range results {
		var bp *poolBackpressureError
		switch {
		case o.err == nil && o.agent == "claude-haiku":
			admitted++
		case errors.As(o.err, &bp):
			refused++
		default:
			t.Fatalf("unexpected outcome %+v", o)
		}
	}
	if admitted != 1 || refused != 1 {
		t.Fatalf("admitted %d refused %d over a one-seat pool, want 1 and 1: %+v", admitted, refused, results)
	}
}

// seatBatchTown is two rigs of three idle polecat directories each, every one
// with its agent bead and no hook.
func seatBatchTown(t *testing.T) (string, *beadsfake.Fake) {
	t.Helper()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "gastown", "longeye")
	db := beadsfake.New()
	for _, rig := range []string{"gastown", "longeye"} {
		for _, name := range []string{"p01", "p02", "p03"} {
			writeTownPolecatDir(t, town, rig, name)
			seedPolecatAgentBead(db, town, rig, name, "")
		}
	}
	return town, db
}

// seatBatchRouter is the router over db, with its bd reads counted by reads and
// its sessions listed by lister.
func seatBatchRouter(town string, db *beadsfake.Fake, reads *specTickReads, lister sessionLister) *poolRouter {
	r := poolRouterForWith(town, newPoolSeatClaimStore(), func(string) beads.Client { return reads.client(db) })
	r.pool = func() *config.PolecatPool { return seatPool(claimAgent, 2) }
	r.sessions = func() sessionLister { return lister }
	return r
}

// TestPoolRouterCountsSeatHoldersThroughTheBatch is the seat picture with one
// polecat holding its seat on a live session. The router counts it from the
// same one assignment list per rig the idle polecats cost (gt-rcktb, gt-4efmk).
func TestPoolRouterCountsSeatHoldersThroughTheBatch(t *testing.T) {
	t.Parallel()
	town, db := seatBatchTown(t)
	reads := newSpecTickReads()
	lister := &fakeLister{sessions: map[string]map[string]string{
		"gt-p01": {"GT_ROLE": "gastown/polecats/p01", "GT_AGENT": claimAgent},
	}}
	r := seatBatchRouter(town, db, reads, lister)

	got, err := r.occupied()()
	if err != nil {
		t.Fatalf("occupied: %v", err)
	}
	if len(got) != 1 || got[0].rig != "gastown" || got[0].polecat != "p01" {
		t.Fatalf("seat holders = %+v, want gastown/p01 alone", got)
	}
	if calls := reads.Calls(); calls != 2 {
		t.Fatalf("bd calls = %d, want 2 (one assigned-work list per rig)", calls)
	}
}

// TestPoolRouterSeatBatchLivesOneDecision pins how long the batch's reads hold.
// A router answers from the reads it made, so a second occupancy read on the
// same router does not see a hook set since. The next decision's router reads
// the rig again and counts the polecat whose hook changed (gt-rcktb).
func TestPoolRouterSeatBatchLivesOneDecision(t *testing.T) {
	t.Parallel()
	town, db := seatBatchTown(t)
	reads := newSpecTickReads()
	lister := &fakeLister{sessions: map[string]map[string]string{}}
	first := seatBatchRouter(town, db, reads, lister)

	if got, err := first.occupied()(); err != nil || len(got) != 0 {
		t.Fatalf("first decision = %+v, %v; want no seats", got, err)
	}
	firstCalls := reads.Calls()

	// p02 takes a hook on work in progress after the first decision read the rig.
	db.Seed(beads.Issue{ID: "gt-p02-work", Status: string(beads.StatusInProgress), Assignee: "gastown/polecats/p02"})
	seedPolecatAgentBead(db, town, "gastown", "p02", "gt-p02-work")

	if got, err := first.occupied()(); err != nil || len(got) != 0 {
		t.Fatalf("same router = %+v, %v; want no seats from the batch it read", got, err)
	}
	if calls := reads.Calls(); calls != firstCalls {
		t.Fatalf("same router re-read the rig: bd calls = %d, want %d", calls, firstCalls)
	}

	next := seatBatchRouter(town, db, reads, lister)
	got, err := next.occupied()()
	if err != nil {
		t.Fatalf("next decision: %v", err)
	}
	if len(got) != 1 || got[0].rig != "gastown" || got[0].polecat != "p02" || !got[0].deadHooked {
		t.Fatalf("next decision = %+v, want gastown/p02 as a dead-hooked seat", got)
	}
}

// A bead labeled agent:<name> asks for that seat when the pool has it; every
// other shape leaves the sling to the pool's own choice (gt-5h1r7).
func TestPoolPinnedAgent(t *testing.T) {
	t.Parallel()
	pool := &config.PolecatPool{Seats: map[string]config.SeatConfig{
		"deepseek-flash": {Cap: 2, PinnedOnly: true},
		"claude-sonnet":  {Cap: 2},
	}}
	cases := []struct {
		name   string
		pool   *config.PolecatPool
		labels []string
		want   string
	}{
		{"a label naming a pinned-only seat", pool, []string{"gt:task", "agent:deepseek-flash"}, "deepseek-flash"},
		{"a label naming an open seat", pool, []string{"agent:claude-sonnet"}, "claude-sonnet"},
		{"the prefix is read without case or padding, as the dispatcher reads it", pool, []string{" Agent: claude-sonnet "}, "claude-sonnet"},
		{"the same label twice is one name", pool, []string{"agent:claude-sonnet", "agent:claude-sonnet"}, "claude-sonnet"},
		{"no agent label", pool, []string{"gt:task"}, ""},
		{"no labels", pool, nil, ""},
		{"conflicting labels pin nothing", pool, []string{"agent:claude-sonnet", "agent:deepseek-flash"}, ""},
		{"a label naming an agent with no seat", pool, []string{"agent:claude-haiku"}, ""},
		{"no pool", nil, []string{"agent:claude-sonnet"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := poolPinnedAgent(tc.pool, tc.labels); got != tc.want {
				t.Errorf("poolPinnedAgent(%v) = %q, want %q", tc.labels, got, tc.want)
			}
		})
	}
}

// A sling that names no agent lands a labeled bead on its pinned-only seat,
// where the same sling for an unlabeled bead skips that seat; --agent wins
// over the label (gt-5h1r7).
func TestSlingSeatSpawnHonorsTheBeadAgentLabel(t *testing.T) {
	t.Parallel()
	pool := &config.PolecatPool{Seats: map[string]config.SeatConfig{
		"deepseek-flash": {Cap: 3, PinnedOnly: true},
		"claude-sonnet":  {Cap: 2},
	}}
	labels := map[string][]string{
		"gt-labeled": {"agent:deepseek-flash"},
		"gt-plain":   {"gt:task"},
	}
	spawnAgent := func(t *testing.T, bead, agent string) string {
		t.Helper()
		w := newTestPoolTown(pool)
		w.fs = newFakeSeatFS()
		router := w.process()
		s := slingSeatSpawn{
			backpressure: func(string, string, SlingSpawnOptions) error { return nil },
			pinnedAgent: func(_, beadID string) (string, error) {
				return poolPinnedAgent(pool, labels[beadID]), nil
			},
			resolvePool: func(_, requested string) (string, string, error) { return router.route(requested, true) },
			releaseSeat: router.seats.store.release,
			prepare: func(_, rigName string, opts SlingSpawnOptions, _ slingStores) (*SpawnedPolecatInfo, error) {
				return &SpawnedPolecatInfo{RigName: rigName, agent: opts.Agent}, nil
			},
			seatClaims: router.seats.store,
		}
		info, err := s.spawn("/town", "gastown", SlingSpawnOptions{HookBead: bead, Agent: agent})
		if err != nil {
			t.Fatalf("spawn: %v", err)
		}
		return info.agent
	}

	if got := spawnAgent(t, "gt-labeled", ""); got != "deepseek-flash" {
		t.Errorf("a labeled bead with no --agent ran on %q, want its pinned-only seat deepseek-flash", got)
	}
	if got := spawnAgent(t, "gt-plain", ""); got != "claude-sonnet" {
		t.Errorf("an unlabeled bead ran on %q, want claude-sonnet (the pinned-only seat is skipped)", got)
	}
	if got := spawnAgent(t, "gt-labeled", "claude-sonnet"); got != "claude-sonnet" {
		t.Errorf("--agent claude-sonnet on a labeled bead ran on %q, want the flag to win", got)
	}
}

// poolRequestedAgent keeps --agent over the bead's label, and reads the label
// only when a bead is slung and a reader is wired (gt-5h1r7). A label whose seat
// is closed refuses the request; --agent naming that same seat does not.
func TestPoolRequestedAgent(t *testing.T) {
	t.Parallel()
	refusal := &poolBackpressureError{}
	label := func(townRoot, beadID string) (string, error) {
		if townRoot != "/town" || beadID != "gt-1" {
			t.Errorf("label read for %q in %q, want gt-1 in /town", beadID, townRoot)
		}
		return "deepseek-flash", nil
	}
	closedLabel := func(townRoot, beadID string) (string, error) {
		return "", refusal
	}
	cases := []struct {
		name    string
		agent   string
		beadID  string
		pinned  func(townRoot, beadID string) (string, error)
		want    string
		wantErr error
	}{
		{"no agent takes the label", "", "gt-1", label, "deepseek-flash", nil},
		{"--agent wins over the label", "claude-sonnet", "gt-1", label, "claude-sonnet", nil},
		{"no bead reads no label", "", "", label, "", nil},
		{"no reader reads no label", "", "gt-1", nil, "", nil},
		{"a closed label refuses the request", "", "gt-1", closedLabel, "", refusal},
		{"--agent over a closed label is not refused", "claude-sonnet", "gt-1", closedLabel, "claude-sonnet", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := poolRequestedAgent("/town", tc.agent, tc.beadID, tc.pinned)
			if got != tc.want {
				t.Errorf("poolRequestedAgent(%q, %q) = %q, want %q", tc.agent, tc.beadID, got, tc.want)
			}
			if (err != nil) != (tc.wantErr != nil) {
				t.Errorf("poolRequestedAgent(%q, %q) error = %v, want %v", tc.agent, tc.beadID, err, tc.wantErr)
			}
		})
	}
}

// A bead labeled agent:<seat> routes to that seat only while the seat is open:
// inside its closed window the label refuses the sling instead of falling back
// to another seat, as the dispatcher waits for it (gt-yc9rf). The window is
// read at the given clock, so the table names no wall time.
func TestPoolLabelRoute(t *testing.T) {
	t.Parallel()
	pool := &config.PolecatPool{Timezone: "America/Chicago", Seats: map[string]config.SeatConfig{
		"deepseek-flash": {Cap: 3, PinnedOnly: true, Closed: "20:00-08:00"},
		"claude-sonnet":  {Cap: 2},
	}}
	cases := []struct {
		name    string
		pool    *config.PolecatPool
		agent   string
		now     time.Time
		want    string
		refused bool
	}{
		{"an open window routes the labeled bead to its seat", pool, "deepseek-flash", chicagoAt(t, 12, 0), "deepseek-flash", false},
		{"inside the closed window the labeled bead is refused", pool, "deepseek-flash", chicagoAt(t, 21, 0), "", true},
		{"the window's edge at 08:00 reopens the seat", pool, "deepseek-flash", chicagoAt(t, 8, 0), "deepseek-flash", false},
		{"no label passes through at any hour", pool, "", chicagoAt(t, 21, 0), "", false},
		{"a seat with no window is never refused", pool, "claude-sonnet", chicagoAt(t, 21, 0), "claude-sonnet", false},
		{"no pool passes the empty label through", nil, "", chicagoAt(t, 21, 0), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := poolLabelRoute(tc.pool, tc.agent, tc.now)
			if got != tc.want {
				t.Errorf("poolLabelRoute(%q) = %q, want %q", tc.agent, got, tc.want)
			}
			var refusal *poolBackpressureError
			if refused := errors.As(err, &refusal); refused != tc.refused {
				t.Fatalf("poolLabelRoute(%q) refused = %v (err %v), want %v", tc.agent, refused, err, tc.refused)
			}
			if tc.refused && !errors.Is(err, errPoolBackpressure) {
				t.Errorf("a closed label's refusal is not a pool backpressure: %v", err)
			}
		})
	}
}

// poolUnseatableLabel refuses a bead whose agent: labels the pool cannot seat,
// as the dispatcher skips it: two that disagree, or one naming no seat. It never
// lets the bead fall back to another seat (gt-dth4g).
func TestPoolUnseatableLabel(t *testing.T) {
	t.Parallel()
	pool := &config.PolecatPool{Seats: map[string]config.SeatConfig{
		"deepseek-flash": {Cap: 3, PinnedOnly: true},
		"claude-sonnet":  {Cap: 2},
	}}
	cases := []struct {
		name    string
		pool    *config.PolecatPool
		labels  []string
		refused bool
		reason  string
		raise   string
	}{
		{"a label naming an open seat is seatable", pool, []string{"agent:claude-sonnet"}, false, "", ""},
		{"a label naming a pinned-only seat is seatable", pool, []string{"agent:deepseek-flash"}, false, "", ""},
		{"no agent label has nothing to refuse", pool, []string{"gt:task"}, false, "", ""},
		{"the same label twice is one agent, not a conflict", pool, []string{"agent:claude-sonnet", "agent:claude-sonnet"}, false, "", ""},
		{"two labels that disagree are refused", pool, []string{"agent:claude-sonnet", "agent:deepseek-flash"}, true, "conflicting agent: labels (claude-sonnet, deepseek-flash)", "the bead's agent: labels"},
		{"a label naming an agent with no seat is refused", pool, []string{"agent:claude-haiku"}, true, "pinned to claude-haiku, which has no seat in the pool", "polecat_pool.seats.claude-haiku"},
		{"a pool with no seats has no opinion on labels", &config.PolecatPool{}, []string{"agent:claude-haiku"}, false, "", ""},
		{"no pool has no opinion on labels", nil, []string{"agent:claude-sonnet", "agent:deepseek-flash"}, false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := poolUnseatableLabel(tc.pool, tc.labels)
			var refusal *poolBackpressureError
			if refused := errors.As(err, &refusal); refused != tc.refused {
				t.Fatalf("poolUnseatableLabel(%v) refused = %v (err %v), want %v", tc.labels, refused, err, tc.refused)
			}
			if !tc.refused {
				return
			}
			if !errors.Is(err, errPoolBackpressure) {
				t.Errorf("an unseatable label's refusal is not a pool backpressure: %v", err)
			}
			if !strings.Contains(refusal.Reason, tc.reason) {
				t.Errorf("refusal reason = %q, want it to name %q", refusal.Reason, tc.reason)
			}
			if refusal.Raise != tc.raise {
				t.Errorf("refusal raises %q, want %q", refusal.Raise, tc.raise)
			}
			if want := "raise " + tc.raise + " to spawn"; !strings.Contains(err.Error(), want) {
				t.Errorf("refusal %q does not say %q", err.Error(), want)
			}
		})
	}
}

// A dry-run sling that names no agent previews the route its bead's agent:
// label takes, as the live sling runs it; --agent still wins (gt-5h1r7).
func TestResolveTarget_DryRunPeeksThePoolWithTheBeadAgentLabel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		agent string
		want  string
	}{
		{"no --agent peeks the labeled seat", "", "deepseek-flash"},
		{"--agent wins over the label", "claude-sonnet", "claude-sonnet"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.run.pinnedAgent = func(townRoot, beadID string) (string, error) {
				if townRoot != slingTestTown || beadID != "gt-new" {
					t.Errorf("label read for %q in %q, want gt-new in %s", beadID, townRoot, slingTestTown)
				}
				return "deepseek-flash", nil
			}
			var peeked []string
			h.run.peekPool = func(_, requested string) (string, string, error) {
				peeked = append(peeked, requested)
				return requested, "pool routes to " + requested, nil
			}

			if _, err := h.run.resolveSlingTarget("gastown", ResolveTargetOptions{
				DryRun: true, NoBoot: true, TownRoot: slingTestTown, HookBead: "gt-new", Agent: tc.agent,
			}); err != nil {
				t.Fatalf("resolveTarget: %v", err)
			}
			if !slices.Equal(peeked, []string{tc.want}) {
				t.Errorf("pool peeked for %q, want one peek for %q", peeked, tc.want)
			}
			if out := h.out.String(); !strings.Contains(out, "pool routes to "+tc.want) {
				t.Errorf("dry-run output lacks the pool's route to %s:\n%s", tc.want, out)
			}
		})
	}
}

// poolBeadAgentWith composes the label rules over the readers it is handed: the
// bead's label picks its seat, an unseatable or closed label refuses, and a
// bead or settings read that fails refuses rather than routing the bead
// unlabeled (gt-chnlz).
func TestPoolBeadAgentWith(t *testing.T) {
	t.Parallel()
	pool := &config.PolecatPool{Timezone: "America/Chicago", Seats: map[string]config.SeatConfig{
		"deepseek-flash": {Cap: 3, PinnedOnly: true, Closed: "20:00-08:00"},
		"claude-sonnet":  {Cap: 2},
	}}
	readErr := errors.New("bd unreachable")
	cases := []struct {
		name         string
		beadID       string
		pool         *config.PolecatPool
		settingsErr  error
		labels       []string
		showErr      error
		nilIssue     bool
		now          time.Time
		want         string
		wantRefusal  string // substring of a pool refusal's reason
		wantRaise    string // the key the refusal tells the operator to raise
		wantReadFail bool   // a plain read failure, not a pool refusal
		wantShown    bool   // the bead is read
	}{
		{name: "a label naming an open seat routes to it", beadID: "gt-1", pool: pool, labels: []string{"agent:deepseek-flash"}, now: chicagoAt(t, 12, 0), want: "deepseek-flash", wantShown: true},
		{name: "an unlabeled bead routes to no seat", beadID: "gt-1", pool: pool, labels: []string{"gt:task"}, now: chicagoAt(t, 12, 0), wantShown: true},
		{name: "a label whose seat is closed refuses", beadID: "gt-1", pool: pool, labels: []string{"agent:deepseek-flash"}, now: chicagoAt(t, 21, 0), wantRefusal: "seat is closed", wantRaise: "polecat_pool.seats.deepseek-flash.closed", wantShown: true},
		{name: "a label naming no seat refuses", beadID: "gt-1", pool: pool, labels: []string{"agent:claude-haiku"}, now: chicagoAt(t, 12, 0), wantRefusal: "has no seat in the pool", wantRaise: "polecat_pool.seats.claude-haiku", wantShown: true},
		{name: "labels that disagree refuse", beadID: "gt-1", pool: pool, labels: []string{"agent:claude-sonnet", "agent:deepseek-flash"}, now: chicagoAt(t, 12, 0), wantRefusal: "conflicting", wantRaise: "the bead's agent: labels", wantShown: true},
		{name: "a bead that cannot be read refuses", beadID: "gt-1", pool: pool, showErr: readErr, now: chicagoAt(t, 12, 0), wantReadFail: true, wantShown: true},
		{name: "settings that cannot be read refuse a labeled bead", beadID: "gt-1", pool: pool, settingsErr: readErr, labels: []string{"agent:deepseek-flash"}, now: chicagoAt(t, 12, 0), wantReadFail: true, wantShown: true},
		{name: "settings that cannot be read leave an unlabeled bead alone", beadID: "gt-1", pool: pool, settingsErr: readErr, labels: []string{"gt:task"}, now: chicagoAt(t, 12, 0), wantShown: true},
		{name: "settings and bead that cannot be read refuse", beadID: "gt-1", pool: pool, settingsErr: readErr, showErr: readErr, now: chicagoAt(t, 12, 0), wantReadFail: true, wantShown: true},
		{name: "a missing bead refuses", beadID: "gt-1", pool: pool, nilIssue: true, now: chicagoAt(t, 12, 0), wantReadFail: true, wantShown: true},
		{name: "no bead reads nothing", beadID: "", pool: pool, now: chicagoAt(t, 12, 0)},
		{name: "a town with no pool never reads the bead", beadID: "gt-1", pool: nil, showErr: readErr, now: chicagoAt(t, 12, 0)},
		{name: "a pool with no seats never reads the bead", beadID: "gt-1", pool: &config.PolecatPool{}, showErr: readErr, now: chicagoAt(t, 12, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			shown := false
			show := func(townRoot, beadID string) (*beads.Issue, error) {
				shown = true
				if townRoot != "/town" || beadID != tc.beadID {
					t.Errorf("bead read for %q in %q, want %q in /town", beadID, townRoot, tc.beadID)
				}
				if tc.showErr != nil {
					return nil, tc.showErr
				}
				if tc.nilIssue {
					return nil, nil
				}
				return &beads.Issue{ID: beadID, Labels: tc.labels}, nil
			}
			settings := func(townRoot string) (*config.TownSettings, error) {
				if tc.settingsErr != nil {
					return nil, tc.settingsErr
				}
				return &config.TownSettings{PolecatPool: tc.pool}, nil
			}

			got, err := poolBeadAgentWith("/town", tc.beadID, tc.now, show, settings)
			if got != tc.want {
				t.Errorf("agent = %q, want %q", got, tc.want)
			}
			if shown != tc.wantShown {
				t.Errorf("bead read = %v, want %v", shown, tc.wantShown)
			}
			switch {
			case tc.wantReadFail:
				if err == nil || (!tc.nilIssue && !errors.Is(err, readErr)) {
					t.Fatalf("error = %v, want it to wrap the read failure", err)
				}
				if errors.Is(err, errPoolBackpressure) {
					t.Errorf("a read failure is not a pool refusal: %v", err)
				}
			case tc.wantRefusal != "":
				var refusal *poolBackpressureError
				if !errors.As(err, &refusal) {
					t.Fatalf("error = %v, want a pool refusal", err)
				}
				if !strings.Contains(refusal.Reason, tc.wantRefusal) {
					t.Errorf("refusal reason = %q, want it to name %q", refusal.Reason, tc.wantRefusal)
				}
				if refusal.Raise != tc.wantRaise {
					t.Errorf("refusal raises %q, want %q", refusal.Raise, tc.wantRaise)
				}
			case err != nil:
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// The production wrapper reads the pool from the town's settings file: a town
// with no settings file, or none with seats, never reads the bead, and settings
// that cannot be parsed are an error for poolBeadAgentWith to weigh (gt-chnlz).
func TestPoolBeadAgentReadsTheTownSettingsFile(t *testing.T) {
	t.Parallel()
	write := func(t *testing.T, contents string) string {
		t.Helper()
		town := t.TempDir()
		path := config.TownSettingsPath(town)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
		return town
	}

	// A bead id with no database behind it: were the bead read, showBead
	// would fail and the call would error, so "", nil proves it was not.
	for name, town := range map[string]string{
		"no settings file":      t.TempDir(),
		"settings with no pool": write(t, `{"type":"town-settings","version":1}`),
	} {
		t.Run(name+" never reads the bead", func(t *testing.T) {
			got, err := poolBeadAgent(town, "gt-unreadable")
			if got != "" || err != nil {
				t.Errorf("poolBeadAgent = %q, %v; want no agent and no error", got, err)
			}
		})
	}

	t.Run("a settings file with seats is read from the town", func(t *testing.T) {
		town := write(t, `{"type":"town-settings","version":1,"polecat_pool":{"seats":{"claude-sonnet":{"cap":2}}}}`)
		ts, err := poolTownSettings(town)
		if err != nil {
			t.Fatalf("poolTownSettings: %v", err)
		}
		if ts.PolecatPool == nil || ts.PolecatPool.Seats["claude-sonnet"].Cap != 2 {
			t.Errorf("pool = %+v, want the seat the file declares", ts.PolecatPool)
		}
	})

	t.Run("a settings file that cannot be parsed is an error", func(t *testing.T) {
		if _, err := poolTownSettings(write(t, `{not json`)); err == nil {
			t.Error("poolTownSettings read a corrupt settings file without error")
		}
	})
}

// A refusal stops the live spawn before the pool is asked for a seat or a
// polecat is prepared, so nothing is claimed for a bead the sling refuses: a
// label the pool cannot honor, and a label that cannot be read at all
// (gt-chnlz).
func TestSlingSeatSpawnRefusesABeadWhoseLabelCannotBeHonored(t *testing.T) {
	t.Parallel()
	readErr := errors.New("bd unreachable")
	for _, tc := range []struct {
		name    string
		refusal error
		pool    bool // a pool backpressure, not a plain read failure
	}{
		{"a closed or unseatable label", &poolBackpressureError{Reason: "pool: deepseek-flash's seat is closed until its window opens, and the bead is labeled for it -> no seat for it"}, true},
		{"a label that cannot be read", fmt.Errorf("could not read gt-labeled for its agent: label: %w", readErr), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var resolved, prepared bool
			s := slingSeatSpawn{
				backpressure: func(string, string, SlingSpawnOptions) error { return nil },
				pinnedAgent:  func(string, string) (string, error) { return "", tc.refusal },
				resolvePool: func(string, string) (string, string, error) {
					resolved = true
					return "claude-sonnet", "", nil
				},
				releaseSeat: func() {},
				prepare: func(string, string, SlingSpawnOptions, slingStores) (*SpawnedPolecatInfo, error) {
					prepared = true
					return &SpawnedPolecatInfo{}, nil
				},
			}

			info, err := s.spawn("/town", "gastown", SlingSpawnOptions{HookBead: "gt-labeled"})
			if !errors.Is(err, tc.refusal) {
				t.Fatalf("spawn error = %v, want the label's refusal %v", err, tc.refusal)
			}
			if got := errors.Is(err, errPoolBackpressure); got != tc.pool {
				t.Errorf("spawn error is a pool backpressure = %v, want %v: %v", got, tc.pool, err)
			}
			if info != nil {
				t.Errorf("spawn returned %+v for a refused sling, want none", info)
			}
			if resolved || prepared {
				t.Errorf("a refused sling asked the pool for a seat = %v, prepared a polecat = %v; want neither", resolved, prepared)
			}
		})
	}
}

// A dry-run sling prints a label's refusal as the live sling would raise it,
// and does not peek the pool for a route the sling will not take: a label the
// pool cannot honor, and a label that cannot be read at all (gt-chnlz).
func TestResolveTarget_DryRunPrintsTheBeadLabelRefusal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		refusal error
	}{
		{"an unseatable label", &poolBackpressureError{Reason: "pool: pinned to claude-haiku, which has no seat in the pool -> no seat for the bead labeled for it"}},
		{"a label that cannot be read", fmt.Errorf("could not read gt-new for its agent: label: %w", errors.New("bd unreachable"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.run.pinnedAgent = func(string, string) (string, error) { return "", tc.refusal }
			peeked := false
			h.run.peekPool = func(string, string) (string, string, error) {
				peeked = true
				return "", "pool routes elsewhere", nil
			}

			if _, err := h.run.resolveSlingTarget("gastown", ResolveTargetOptions{
				DryRun: true, NoBoot: true, TownRoot: slingTestTown, HookBead: "gt-new",
			}); err != nil {
				t.Fatalf("resolveTarget: %v", err)
			}
			if peeked {
				t.Error("the pool was peeked for a bead whose label the sling refuses")
			}
			out := h.out.String()
			if !strings.Contains(out, tc.refusal.Error()) {
				t.Errorf("dry-run output lacks the label's refusal %q:\n%s", tc.refusal.Error(), out)
			}
			if strings.Contains(out, "pool routes elsewhere") {
				t.Errorf("dry-run output shows a route for a refused bead:\n%s", out)
			}
		})
	}
}
