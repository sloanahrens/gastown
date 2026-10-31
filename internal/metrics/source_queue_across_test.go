package metrics

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// queueTrackerTown is a town whose rigs share one database, the way the
// tracker holds them: every rig's store is the same one, and a list scoped to
// several rigs answers them all in a single call.
type queueTrackerTown struct {
	rows   map[string]int    // "<rig name>/<column>" issue counts
	fail   map[string]bool   // "<rig name>/waiting_to_land" refused in the one call
	refuse bool              // the one call is refused as a whole
	prefix map[string]string // rig name to the prefix the store files it under

	mu     sync.Mutex
	across [][]string // the rig list of each cross-rig request
	lists  []string   // every per-rig list
	perRig []string   // every per-rig read of another column
}

// queueTrackerStore is one rig's store on the tracker; every rig's store is
// the same database, so each can answer for all of them.
type queueTrackerStore struct {
	owner *queueTrackerTown
	rig   string
}

func (t *queueTrackerTown) open(_ context.Context, dir string) BeadStore {
	return &queueTrackerStore{owner: t, rig: filepath.Base(dir)}
}

// counts is one rig's issue count for a column.
func (t *queueTrackerTown) counts(rig, column string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rows[rig+"/"+column]
}

func (t *queueTrackerStore) List(beads.ListOptions) ([]*beads.Issue, error) {
	t.owner.mu.Lock()
	t.owner.lists = append(t.owner.lists, t.rig)
	t.owner.mu.Unlock()
	return issues(t.owner.counts(t.rig, "waiting_to_land")), nil
}

func (t *queueTrackerStore) ListAcross(rigs []string, _ beads.ListOptions) (beads.AcrossResult, error) {
	t.owner.mu.Lock()
	t.owner.across = append(t.owner.across, append([]string(nil), rigs...))
	t.owner.mu.Unlock()
	if t.owner.refuse {
		return beads.AcrossResult{}, fmt.Errorf("tracker: the request was refused")
	}
	res := beads.AcrossResult{Rigs: map[string][]*beads.Issue{}, Errors: map[string]error{}}
	for _, prefix := range rigs {
		rig := t.owner.rigName(prefix)
		if t.owner.fail[rig+"/waiting_to_land"] {
			res.Errors[prefix] = fmt.Errorf("%s: no tracker", rig)
			continue
		}
		res.Rigs[prefix] = issues(t.owner.counts(rig, "waiting_to_land"))
	}
	return res, nil
}

func (t *queueTrackerStore) ReadyFiltered(opts beads.ReadyOptions) ([]*beads.Issue, error) {
	column := "ready"
	if opts.Unassigned {
		column = "dispatchable"
	}
	return t.own(column)
}

func (t *queueTrackerStore) Blocked() ([]*beads.Issue, error) { return t.own("blocked") }

func (t *queueTrackerStore) ShowMultiple([]string) (map[string]*beads.Issue, error) {
	return nil, fmt.Errorf("the queue group read beads by id")
}

func (t *queueTrackerStore) own(column string) ([]*beads.Issue, error) {
	t.owner.mu.Lock()
	t.owner.perRig = append(t.owner.perRig, t.rig+"/"+column)
	t.owner.mu.Unlock()
	return issues(t.owner.counts(t.rig, column)), nil
}

// rigName is the town's name for the rig filed under prefix; a prefix the
// town does not know is the prefix itself, so a test sees the mismatch as a
// wrong count rather than a panic.
func (t *queueTrackerTown) rigName(prefix string) string {
	for name, p := range t.prefix {
		if p == prefix {
			return name
		}
	}
	return prefix
}

func issues(n int) []*beads.Issue {
	out := make([]*beads.Issue, n)
	for i := range out {
		out[i] = &beads.Issue{ID: fmt.Sprintf("x-%d", i)}
	}
	return out
}

// queueTrackerRigs is a mayor/rigs.json whose rigs record the beads prefix the
// tracker files their issues under.
func queueTrackerRigs(prefix map[string]string) map[string]string {
	var b strings.Builder
	b.WriteString(`{"version":1,"rigs":{`)
	first := true
	for name, p := range prefix {
		if !first {
			b.WriteString(",")
		}
		first = false
		fmt.Fprintf(&b, "%q:{\"beads\":{\"repo\":\"local\",\"prefix\":%q}}", name, p+"-")
	}
	b.WriteString("}}")
	return map[string]string{filepath.Join(queueTestTown, "mayor", "rigs.json"): b.String()}
}

func queueTrackerEnv(t *testing.T, town *queueTrackerTown, rigs map[string]string) Env {
	t.Helper()
	env := queueTestEnv(t, rigs, &queueStores{}, nil)
	env.Beads = town.open
	return env
}

// The landing column asks a tracker rig's store once for the whole town: three
// rigs, one request, and the sum is the board all three answered.
func TestSourceQueueLandingColumnAsksATrackerTownOnceForEveryRig(t *testing.T) {
	t.Parallel()
	prefix := map[string]string{"gastown": "gt", "hm": "hm", "om": "om"}
	town := &queueTrackerTown{
		rows: map[string]int{
			"gastown/waiting_to_land": 3, "hm/waiting_to_land": 1, "om/waiting_to_land": 2,
			"gastown/ready": 4, "hm/ready": 1, "om/ready": 2,
		},
		prefix: prefix,
	}
	cols, err := queueSource{}.Collect(context.Background(), queueTrackerEnv(t, town, queueTrackerRigs(prefix)))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	queueWantColumns(t, cols, map[string]any{"waiting_to_land": 6, "ready": 7})
	if len(town.across) != 1 {
		t.Fatalf("cross-rig requests = %d, want one for the town's three rigs", len(town.across))
	}
	if got := town.across[0]; strings.Join(got, ",") != "gt,hm,om" {
		t.Errorf("the request named %v, want the rigs' prefixes gt, hm, om", got)
	}
	if len(town.lists) != 0 {
		t.Errorf("the landing board was listed per rig %d time(s) (%v); the one request answers them", len(town.lists), town.lists)
	}
	// The other columns still read one store per rig.
	seen := append([]string(nil), town.perRig...)
	sort.Strings(seen)
	if len(seen) != 3*3 {
		t.Errorf("per-rig column reads = %v, want one per rig per column", seen)
	}
}

// A rig the one request fails for nils the column and leaves the others: a sum
// of the rigs that answered would read as a queue that shrank.
func TestSourceQueueLandingColumnNilWhenOneTrackerRigFails(t *testing.T) {
	t.Parallel()
	prefix := map[string]string{"gastown": "gt", "hm": "hm"}
	town := &queueTrackerTown{
		rows:   map[string]int{"gastown/waiting_to_land": 3, "hm/waiting_to_land": 1, "gastown/ready": 4, "hm/ready": 2},
		fail:   map[string]bool{"hm/waiting_to_land": true},
		prefix: prefix,
	}
	cols, err := queueSource{}.Collect(context.Background(), queueTrackerEnv(t, town, queueTrackerRigs(prefix)))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	queueWantColumns(t, cols, map[string]any{"waiting_to_land": nil, "ready": 6, "blocked": 0})
}

// A tracker that refuses the whole request nils the landing column and leaves
// the per-rig columns, which the request does not touch, as they were.
func TestSourceQueueLandingColumnNilWhenTheTrackerRefusesTheRequest(t *testing.T) {
	t.Parallel()
	prefix := map[string]string{"gastown": "gt", "hm": "hm"}
	town := &queueTrackerTown{
		rows:   map[string]int{"gastown/waiting_to_land": 3, "hm/waiting_to_land": 1, "gastown/ready": 4, "hm/ready": 2},
		refuse: true,
		prefix: prefix,
	}
	cols, err := queueSource{}.Collect(context.Background(), queueTrackerEnv(t, town, queueTrackerRigs(prefix)))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	queueWantColumns(t, cols, map[string]any{"waiting_to_land": nil, "ready": 6, "blocked": 0})
}

// queueHold holds the list of each rig that is not free to answer, and records
// whether each rig's store context was still live when its list began.
type queueHold struct {
	started chan string   // a held rig's list began
	release chan struct{} // closed to let the held lists answer
	mu      sync.Mutex
	live    map[string]bool
}

// queueHoldStore is one bd rig's store on a queueHold. A held rig's list waits
// for the release; the others answer at once.
type queueHoldStore struct {
	BeadStore
	hold *queueHold
	rig  string
	ctx  context.Context
	held bool
}

func (s *queueHoldStore) List(beads.ListOptions) ([]*beads.Issue, error) {
	s.hold.mu.Lock()
	s.hold.live[s.rig] = s.ctx.Err() == nil
	s.hold.mu.Unlock()
	if !s.held {
		return nil, nil
	}
	s.hold.started <- s.rig
	select {
	case <-s.hold.release:
		return nil, nil
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("the test never released %s", s.rig)
	}
}

// A bd rig queued behind the parallel bound starts its read with its full
// deadline. The batch's own clock must not run out while the rig waits for a
// slot: here the three rigs ahead of it hold past one deadline, and the fourth
// still reads on a live store. This shortens queueBdTimeout, which every read
// takes, so the test is not parallel and restores it when it ends.
func TestQueueListCountsQueuedRigReadStartsWithItsFullDeadline(t *testing.T) {
	saved := queueBdTimeout
	queueBdTimeout = 50 * time.Millisecond
	t.Cleanup(func() { queueBdTimeout = saved })

	rigs := []queueRig{{name: "alpha", prefix: "alpha"}, {name: "beta", prefix: "beta"}, {name: "gamma", prefix: "gamma"}, {name: "delta", prefix: "delta"}}
	hold := &queueHold{started: make(chan string, len(rigs)), release: make(chan struct{}), live: map[string]bool{}}
	env := Env{Town: queueTestTown, Beads: func(ctx context.Context, dir string) BeadStore {
		rig := filepath.Base(dir)
		return &queueHoldStore{hold: hold, rig: rig, ctx: ctx, held: rig != "delta"}
	}}
	type answer struct {
		counts map[string]int
		ok     bool
	}
	done := make(chan answer, 1)
	go func() {
		counts, ok := queueListCounts(context.Background(), env, rigs, beads.ListOptions{Label: "gt:ready-to-land"}, queueLandingColumn().readAcross)
		done <- answer{counts, ok}
	}()
	for range 3 {
		select {
		case <-hold.started:
		case <-time.After(5 * time.Second):
			t.Fatal("fewer than three rigs' reads started; the bound is three")
		}
	}
	// The batch opened over a deadline ago by the time the held reads release.
	<-time.After(2 * queueBdTimeout)
	close(hold.release)
	var got answer
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("queueListCounts did not answer")
	}
	if !got.ok {
		t.Fatalf("queueListCounts ok = false; every rig answered, so the column reads")
	}
	if !hold.live["delta"] {
		t.Error("delta's read started on a store whose deadline had run out while it waited for a slot")
	}
}

// A rig that a cross-rig answer neither names nor refuses was never read, so
// the column fails: a count of zero for it would read as an empty queue.
func TestQueueRigCountsOfFailsARigTheAnswerLeavesOut(t *testing.T) {
	t.Parallel()
	rigs := []queueRig{{name: "gastown", prefix: "gt"}, {name: "hm", prefix: "hm"}}
	res := beads.AcrossResult{Rigs: map[string][]*beads.Issue{"gt": issues(2)}, Errors: map[string]error{}}
	if counts, ok := queueRigCountsOf(rigs, res); ok {
		t.Errorf("counts = %v, ok; want the column failed, because hm was never read", counts)
	}
	res.Rigs["hm"] = issues(0)
	counts, ok := queueRigCountsOf(rigs, res)
	if !ok || counts["gastown"] != 2 || counts["hm"] != 0 {
		t.Errorf("counts = %v, ok = %v; want gastown 2 and hm 0 once hm is answered", counts, ok)
	}
}
