package metrics

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// queueTrackerTown is a town whose rigs share one database, the way the
// tracker holds them: every rig's store is the same one, and a list scoped to
// several rigs answers them all in a single call.
type queueTrackerTown struct {
	rows   map[string]int    // "<rig name>/<column>" issue counts
	fail   map[string]bool   // "<rig name>/waiting_to_land" refused in the one call
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
