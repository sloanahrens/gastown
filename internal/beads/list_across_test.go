package beads

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// acrossRig is one rig's own store: it answers the issues it holds, or the
// error it holds.
type acrossRig struct {
	issues []*Issue
	err    error
}

func (s *acrossRig) List(ListOptions) ([]*Issue, error) { return s.issues, s.err }

// acrossOpen opens the stores a test built, keyed by rig, and records the
// order the rigs were opened in.
type acrossOpen struct {
	stores map[string]Lister
	errs   map[string]error
	mu     sync.Mutex
	opened []string
}

func (o *acrossOpen) open(rig string) (Lister, error) {
	o.mu.Lock()
	o.opened = append(o.opened, rig)
	o.mu.Unlock()
	if err := o.errs[rig]; err != nil {
		return nil, err
	}
	return o.stores[rig], nil
}

func issuesNamed(ids ...string) []*Issue {
	out := make([]*Issue, len(ids))
	for i, id := range ids {
		out[i] = &Issue{ID: id}
	}
	return out
}

// acrossGate holds every read until the batch's bound is reached, so a test
// can see how many run at once without sleeping.
type acrossGate struct {
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	live    int
	max     int
}

func newAcrossGate() *acrossGate {
	return &acrossGate{started: make(chan struct{}, 64), release: make(chan struct{})}
}

func (g *acrossGate) read(l Lister) ([]*Issue, error) {
	g.mu.Lock()
	g.live++
	if g.live > g.max {
		g.max = g.live
	}
	g.mu.Unlock()
	g.started <- struct{}{}
	<-g.release
	g.mu.Lock()
	g.live--
	g.mu.Unlock()
	return l.List(ListOptions{})
}

func (g *acrossGate) highWater() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.max
}

// A town whose rigs are separate stores reads them side by side, never more
// than listAcrossParallel at once, and every rig comes back.
func TestListAcrossReadsOwnRigsInParallelWithinTheBound(t *testing.T) {
	t.Parallel()
	names := []string{"al", "be", "bv", "dv", "fr", "gt", "hm", "om"}
	open := &acrossOpen{stores: map[string]Lister{}}
	for _, name := range names {
		open.stores[name] = &acrossRig{issues: issuesNamed(name + "-a")}
	}
	gate := newAcrossGate()
	done := make(chan AcrossResult, 1)
	go func() {
		res, _ := ListAcross(names, open.open, ListOptions{}, gate.read)
		done <- res
	}()
	for i := 0; i < listAcrossParallel; i++ {
		select {
		case <-gate.started:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d read(s) started; want %d at once", i, listAcrossParallel)
		}
	}
	if got := gate.highWater(); got != listAcrossParallel {
		t.Errorf("%d reads ran at once, want the bound of %d", got, listAcrossParallel)
	}
	close(gate.release)
	res := <-done
	if len(res.Rigs) != len(names) || len(res.Errors) != 0 {
		t.Fatalf("answer = %d rigs and %d errors, want %d and none:\n%v", len(res.Rigs), len(res.Errors), len(names), res.Errors)
	}
}

// A rig whose store fails is named with its error and the rigs that answered
// keep their issues: one dead store does not hide the others.
func TestListAcrossKeepsTheRigsThatAnswerWhenOneFails(t *testing.T) {
	t.Parallel()
	dead := errors.New("bd: no Dolt server")
	open := &acrossOpen{
		stores: map[string]Lister{
			"gt": &acrossRig{issues: issuesNamed("gt-a")},
			"hm": &acrossRig{issues: issuesNamed("hm-a")},
		},
		errs: map[string]error{"dv": nil, "fr": errors.New("unreadable route table")},
	}
	open.stores["dv"] = &acrossRig{err: dead}
	res, err := ListAcross([]string{"gt", "dv", "hm", "fr"}, open.open, ListOptions{}, nil)
	if err != nil {
		t.Fatalf("ListAcross: %v", err)
	}
	if len(res.Rigs["gt"]) != 1 || len(res.Rigs["hm"]) != 1 {
		t.Errorf("rigs that answered = %v, want gt and hm", res.Rigs)
	}
	if !errors.Is(res.Errors["dv"], dead) || !strings.Contains(res.Errors["fr"].Error(), "route table") {
		t.Errorf("errors = %v, want dv's read failure and fr's open failure", res.Errors)
	}
	if _, ok := res.Rigs["dv"]; ok {
		t.Error("dv is in the answered rigs; a failed rig must not read as an empty one")
	}
}

// acrossStore is one store that holds several rigs: it answers them in one
// call and records how many calls it took.
type acrossStore struct {
	mu     sync.Mutex
	calls  [][]string
	answer map[string][]*Issue
	leave  string
	err    error
}

func (s *acrossStore) List(ListOptions) ([]*Issue, error) {
	return nil, errors.New("a store holding several rigs is read with ListAcross")
}

func (s *acrossStore) ListAcross(rigs []string, _ ListOptions) (AcrossResult, error) {
	s.mu.Lock()
	s.calls = append(s.calls, append([]string(nil), rigs...))
	s.mu.Unlock()
	if s.err != nil {
		return AcrossResult{}, s.err
	}
	res := AcrossResult{Rigs: map[string][]*Issue{}, Errors: map[string]error{}}
	for _, rig := range rigs {
		if rig == s.leave {
			continue
		}
		res.Rigs[rig] = s.answer[rig]
	}
	return res, nil
}

func (s *acrossStore) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// The rigs a store holding several of them answers cost one request between
// them, while the rigs on their own stores are still read one at a time.
func TestListAcrossAsksACrossRigStoreOnceForEveryRigItHolds(t *testing.T) {
	t.Parallel()
	cross := &acrossStore{answer: map[string][]*Issue{
		"gt": issuesNamed("gt-a", "gt-b"),
		"hq": issuesNamed("hq-a"),
	}}
	open := &acrossOpen{stores: map[string]Lister{
		"gt": cross,
		"hq": cross,
		"hm": &acrossRig{issues: issuesNamed("hm-a")},
	}}
	res, err := ListAcross([]string{"gt", "hq", "hm"}, open.open, ListOptions{}, nil)
	if err != nil {
		t.Fatalf("ListAcross: %v", err)
	}
	if calls := cross.callCount(); calls != 1 {
		t.Fatalf("the cross-rig store was asked %d times, want once for gt and hq", calls)
	}
	sort.Strings(cross.calls[0])
	if got := strings.Join(cross.calls[0], ","); got != "gt,hq" {
		t.Errorf("the one request named %q, want gt and hq", got)
	}
	if len(res.Rigs["gt"]) != 2 || len(res.Rigs["hq"]) != 1 || len(res.Rigs["hm"]) != 1 {
		t.Errorf("answer = %v, want gt 2, hq 1 and hm 1", res.Rigs)
	}
	if len(res.Errors) != 0 {
		t.Errorf("errors = %v, want none", res.Errors)
	}
}

// A cross-rig store that fails, or that leaves a rig out of its answer, names
// those rigs in Errors: an unread rig is not a rig with no issues.
func TestListAcrossNamesTheRigsACrossRigStoreDidNotAnswer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		store *acrossStore
		want  string
	}{
		{"a refused request", &acrossStore{err: errors.New("unknown rig nope")}, "unknown rig"},
		{"a rig left out", &acrossStore{answer: map[string][]*Issue{"gt": issuesNamed("gt-a")}, leave: "hq"}, "did not answer"},
	}
	for _, tc := range tests {
		open := &acrossOpen{stores: map[string]Lister{"gt": tc.store, "hq": tc.store}}
		res, err := ListAcross([]string{"gt", "hq"}, open.open, ListOptions{}, nil)
		if err != nil {
			t.Fatalf("%s: ListAcross: %v", tc.name, err)
		}
		var joined []string
		for rig, e := range res.Errors {
			joined = append(joined, rig+": "+e.Error())
		}
		sort.Strings(joined)
		if !strings.Contains(strings.Join(joined, "\n"), tc.want) {
			t.Errorf("%s: errors = %v, want one naming %q", tc.name, res.Errors, tc.want)
		}
	}
}

// A test's own read is the one every rig on its own store is read with: the
// caller that needs its reads attributed to it keeps the frame.
func TestListAcrossReadsOwnRigsWithTheCallersRead(t *testing.T) {
	t.Parallel()
	open := &acrossOpen{stores: map[string]Lister{"gt": &acrossRig{}}}
	var read []string
	_, err := ListAcross([]string{"gt"}, open.open, ListOptions{}, func(l Lister) ([]*Issue, error) {
		read = append(read, fmt.Sprintf("%T", l))
		return l.List(ListOptions{})
	})
	if err != nil {
		t.Fatalf("ListAcross: %v", err)
	}
	if len(read) != 1 {
		t.Errorf("the caller's read ran %d times, want once", len(read))
	}
}

// A rig asked for twice is read once, and a call the seam cannot honour — no
// opener, an empty rig name, a merge-request rig filter — is refused rather
// than answered wider.
func TestListAcrossRefusesACallItCannotHonour(t *testing.T) {
	t.Parallel()
	open := &acrossOpen{stores: map[string]Lister{"gt": &acrossRig{}}}
	res, err := ListAcross([]string{"gt", "gt"}, open.open, ListOptions{}, nil)
	if err != nil {
		t.Fatalf("ListAcross: %v", err)
	}
	if len(open.opened) != 1 || len(res.Rigs) != 1 {
		t.Errorf("a rig asked for twice was opened %d times and answered %d, want once each", len(open.opened), len(res.Rigs))
	}
	for name, call := range map[string]func() error{
		"no opener":    func() error { _, err := ListAcross([]string{"gt"}, nil, ListOptions{}, nil); return err },
		"an empty rig": func() error { _, err := ListAcross([]string{""}, open.open, ListOptions{}, nil); return err },
		"a Rig filter": func() error { _, err := ListAcross([]string{"gt"}, open.open, ListOptions{Rig: "gt"}, nil); return err },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: ListAcross = nil, want a refusal", name)
		}
	}
}
