package polecat

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/session"
)

// censusProbeTmux is the tmux client behind the census in this test. It answers
// no session unless the test says it does, fails the reads the test names, and
// counts the reads that reach it, so a probe served from the census is visible
// as one the client never saw.
type censusProbeTmux struct {
	mu      sync.Mutex
	running map[string]bool
	fail    map[string]error
	reads   map[string]int
}

func newCensusProbeTmux() *censusProbeTmux {
	return &censusProbeTmux{
		running: map[string]bool{},
		fail:    map[string]error{},
		reads:   map[string]int{},
	}
}

func (p *censusProbeTmux) HasSession(name string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads[name]++
	if err := p.fail[name]; err != nil {
		return false, err
	}
	return p.running[name], nil
}

func (p *censusProbeTmux) KillSessionWithProcesses(string) error { return nil }
func (p *censusProbeTmux) IsAgentAliveChecked(string) (bool, error) {
	return true, nil
}
func (p *censusProbeTmux) GetPanePID(string) (string, error) {
	return "", errors.New("pane pid read is not part of this test")
}

func (p *censusProbeTmux) probeCount(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reads[name]
}

// countedSessionReader counts the has-session reads the Manager sends through
// the installed seam, so the test can see the Manager asked the seam at all.
type countedSessionReader struct {
	inner SessionReader

	mu    sync.Mutex
	asked int
}

func (r *countedSessionReader) HasSession(name string) (bool, error) {
	r.mu.Lock()
	r.asked++
	r.mu.Unlock()
	return r.inner.HasSession(name)
}

func (r *countedSessionReader) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.asked
}

// TestSlingCensusIsSharedByManagerReuse drives the Manager's own seat-listing
// and reuse reads with the sling's census reader installed (SetSessionProbes):
// the listing and the reuse of the seat it picks ask each sandboxed seat once,
// a probe that failed is asked again by the next reader, and a seat killed in
// the run is probed afresh.
//
//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestSlingCensusIsSharedByManagerReuse(t *testing.T) {
	const seats = 10
	const failing = "p3"
	names := make([]string, seats)
	for i := range names {
		names[i] = fmt.Sprintf("p%d", i)
	}
	mgr, _, _, _, _ := canonicalWithPolecats(t, true, names...)
	town := mgr.townRoot
	if town == "" {
		t.Fatal("the manager has no town root")
	}
	session.ResetSandboxRoutingForTest(town)
	t.Cleanup(func() { session.ResetSandboxRoutingForTest("") })

	sandboxes := make([]map[string]string, 0, seats)
	for _, name := range names {
		if err := session.WriteSandboxRecord(town, session.SandboxRecord{
			Session: mgr.sessionName(name),
			Rig:     mgr.rig.Name,
			Polecat: name,
			Gateway: "gw",
		}); err != nil {
			t.Fatal(err)
		}
		sandboxes = append(sandboxes, map[string]string{"name": mgr.sessionName(name), "phase": "Ready"})
	}
	listing, err := json.Marshal(map[string]any{"sandboxes": sandboxes})
	if err != nil {
		t.Fatal(err)
	}
	gateway := &sandbox.Fake{Reply: map[string]string{"list": string(listing)}}
	t.Cleanup(session.BeginSandboxCensus(gateway.Runner()))

	// probes is the tmux client behind the census reader; own is the Manager's
	// own client. They are separate objects so the test can tell a read the
	// census answered from one the Manager took itself.
	probes := newCensusProbeTmux()
	// Every seat has no live session, so the listing reads them idle and the
	// reuse never has a session to kill. One seat's probe fails, as a seat whose
	// tmux cannot answer does.
	probes.fail[mgr.sessionName(failing)] = errors.New("tmux: no server")
	own := newCensusProbeTmux()
	mgr.tmux = own
	reader := &countedSessionReader{inner: session.CensusSessionReader{Tmux: probes}}
	mgr.SetSessionProbes(reader)

	// The seat listing the reuse step reads.
	listing1 := idleSeatNames(t, mgr)
	if len(listing1) != seats {
		t.Fatalf("the listing yielded %v, want all %d seats", listing1, seats)
	}

	// The reuse step's own reuse of the seat it picks: its idle-session check
	// and its post-reuse read of the seat both go through the seam.
	picked := listing1[0]
	if _, err := mgr.ReuseIdlePolecat(picked, AddOptions{HookBead: "gt-next"}); err != nil {
		t.Fatalf("reusing the picked seat: %v", err)
	}
	if _, err := mgr.Get(picked); err != nil {
		t.Fatalf("reading the picked seat: %v", err)
	}

	// The failed seat's next reader asks again, and the census does not keep it.
	if _, err := mgr.Get(failing); err != nil {
		t.Fatalf("reading the failed seat: %v", err)
	}

	listCalls := 0
	for _, argv := range gateway.Calls() {
		if strings.Contains(strings.Join(argv, " "), " list") {
			listCalls++
		}
	}
	if listCalls != 1 {
		t.Errorf("gateway list calls = %d, want 1 for the whole sling", listCalls)
	}
	if got := reader.count(); got != seats+4 {
		t.Errorf("the Manager asked the seam %d times, want %d (the listing, the reuse, the picked seat and the failed seat)", got, seats+4)
	}
	for _, name := range names {
		want := 1
		if name == failing {
			want = 2 // the failed probe, then the next reader's
		}
		if got := probes.probeCount(mgr.sessionName(name)); got != want {
			t.Errorf("has-session %s: census probes = %d, want %d", name, got, want)
		}
		// Only a reader that failed falls through to the Manager's own client;
		// a seat the census answered never reaches it.
		wantOwn := 0
		if name == failing {
			wantOwn = 2 // one fallback per reader error
		}
		if got := own.probeCount(mgr.sessionName(name)); got != wantOwn {
			t.Errorf("has-session %s: the Manager's own probes = %d, want %d", name, got, wantOwn)
		}
	}

	// A seat killed in the run has no record left, so its next read is probed.
	if err := session.CleanupSandboxWith(nil, town, mgr.sessionName(picked)); err != nil {
		t.Fatalf("killing %s: %v", picked, err)
	}
	if _, err := mgr.Get(picked); err != nil {
		t.Fatalf("reading the killed seat: %v", err)
	}
	if got := probes.probeCount(mgr.sessionName(picked)); got != 2 {
		t.Errorf("has-session %s after its kill: probes = %d, want 2 (probed afresh)", picked, got)
	}
}
