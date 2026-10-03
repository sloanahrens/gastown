package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/polecat"
)

// TestChoosePoolAgent pins the whole table: the agent AND the exact one-line
// reason, because the line is the only thing a sling prints (gt-ipk7).
func TestChoosePoolAgent(t *testing.T) {
	t.Parallel()
	s := func(agent string) poolSession { return poolSession{name: "gt-x", agent: agent} }
	capped := &config.PolecatPool{OverflowAgent: "deepseek-flash", MaxOverflow: 2}
	uncapped := &config.PolecatPool{OverflowAgent: "deepseek-flash"}
	// The live town's pool still names its retired local seat; it changes nothing.
	retiredLocal := &config.PolecatPool{LocalAgent: []byte(`"local-coder-polecat"`), MaxLocal: []byte(`0`), OverflowAgent: "deepseek-flash", MaxOverflow: 2}
	oneTaken := []poolSession{s("deepseek-flash"), s("claude-sonnet")}
	full := []poolSession{s("deepseek-flash"), s("deepseek-flash")}

	cases := []struct {
		name        string
		pool        *config.PolecatPool
		requested   string
		sessions    []poolSession
		want        string
		wantWhy     string
		wantRefused bool
	}{
		{"no pool", nil, "", nil, "", "pool: no polecat pool configured", false},
		{"pool without an agent", &config.PolecatPool{MaxOverflow: 2}, "", nil, "", "pool: polecat_pool has no overflow_agent; using the role default", false},
		{"empty town takes the first seat", capped, "", nil, "deepseek-flash", "pool: seat 1/2 -> deepseek-flash", false},
		{"other agents' sessions do not count", capped, "", oneTaken, "deepseek-flash", "pool: seat 2/2 -> deepseek-flash", false},
		{"a full pool refuses", capped, "", full, "deepseek-flash", "pool: full (2/2) -> no seat for deepseek-flash", true},
		{"a request for the pool's agent is held by its cap", capped, "deepseek-flash", full, "deepseek-flash", "pool: full (2/2) -> no seat for deepseek-flash", true},
		{"a request the pool does not own stands untouched", capped, "claude-sonnet", full, "", "", false},
		{"max_overflow 0 leaves the seat uncapped", uncapped, "", full, "deepseek-flash", "pool: seat 3 (uncapped) -> deepseek-flash", false},
		{"retired local keys change nothing", retiredLocal, "", oneTaken, "deepseek-flash", "pool: seat 2/2 -> deepseek-flash", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason, refused := choosePoolAgent(c.pool, c.requested, c.sessions)
			if got != c.want {
				t.Errorf("agent = %q, want %q (reason: %s)", got, c.want, reason)
			}
			if reason != c.wantWhy {
				t.Errorf("reason = %q, want %q", reason, c.wantWhy)
			}
			if refused != c.wantRefused {
				t.Errorf("refused = %v, want %v (reason: %s)", refused, c.wantRefused, reason)
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
			"hq-mayor":    {"GT_ROLE": "mayor"},
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
	pool := &config.PolecatPool{OverflowAgent: "deepseek-flash", MaxOverflow: 2}
	if n := poolSeatCount(pool, got); n != 1 {
		t.Errorf("only slate runs the pool's agent: seat count = %d, want 1", n)
	}
}

// TestListPolecatSessionsExcludesDoneWithOpenMR pins gt-2nft: a polecat whose
// own agent bead reads done with an MR still in the queue does not occupy the
// seat its session name would otherwise claim. The pool refused a 4th flash
// spawn at 3/3 with only two live flash sessions because two "done, MR ready"
// polecats were counted as occupants; this is the fix.
func TestListPolecatSessionsExcludesDoneWithOpenMR(t *testing.T) {
	t.Parallel()
	disposition := func(rigName, polecatName string) (polecat.WorkstateDisposition, error) {
		if polecatName == "diamond" {
			return polecat.WorkstateDisposition{Verdict: polecat.WorkstateVerdictPendingMR, ReuseStatus: "idle-pr-open"}, nil
		}
		return polecat.WorkstateDisposition{Verdict: polecat.WorkstateVerdictWorking}, nil
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

	pool := &config.PolecatPool{OverflowAgent: "deepseek-flash", MaxOverflow: 2}
	if n := poolSeatCount(pool, got); n != 1 {
		t.Errorf("seat occupancy should count only the live working session, got %d", n)
	}
}

// TestListPolecatSessionsFailsOpenOnDispositionError keeps a session counted
// when the polecat's own state cannot be read: an overrun the pool exists to
// prevent (gt-md4z) is worse than one extra refused sling.
func TestListPolecatSessionsFailsOpenOnDispositionError(t *testing.T) {
	t.Parallel()
	disposition := func(rigName, polecatName string) (polecat.WorkstateDisposition, error) {
		return polecat.WorkstateDisposition{}, errors.New("dolt unreachable")
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
				return func() {}, nil
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
	if !strings.HasPrefix(err.Error(), "sling refused:") || !strings.HasSuffix(err.Error(), "; raise polecat_pool.max_overflow to spawn") {
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

// A tmux-listing failure must not override an explicit non-pool --agent
// (gt-67fj): the fallback that answers an unknown session count with the
// overflow agent ran ahead of the "not the pool's seat" check, so a request
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

// ── Seat claims (gt-eoi9) ───────────────────────────────────────────────────

// claimAgent is the pool's agent in the seat-claim tests.
const claimAgent = "deepseek-flash"

// cappedPool is a pool whose seat holds maxOverflow polecats. The fake tmux
// server has not heard of any of the slings racing each other, which is the
// moment the cap used to break.
func cappedPool(maxOverflow int) *config.PolecatPool {
	return &config.PolecatPool{OverflowAgent: claimAgent, MaxOverflow: maxOverflow}
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
			prepare: func(_, rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
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
		prepare: func(_, rigName string, opts SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
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
func TestPeekPolecatPoolAgentReportsTheOverflowRefusal(t *testing.T) {
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
	pool := &config.PolecatPool{OverflowAgent: claimAgent, MaxOverflow: 1}
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
			if got := poolSeatCount(pool, picture); got != c.wantOccupied {
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
	pool := &config.PolecatPool{OverflowAgent: claimAgent, MaxOverflow: 1}
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
