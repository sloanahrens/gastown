package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/session"
)

// countingSeatLister is the tmux double a sling reads its seats through. It
// counts the per-seat reads it actually answers, so a test can see how many
// reached tmux.
type countingSeatLister struct {
	fakeLister
	mu    sync.Mutex
	reads map[string]int // "verb session" -> probes answered
	// failHas names the seats whose next has-session read fails, as a busy
	// tmux server does; the entry is cleared by that failing read.
	failHas map[string]bool
}

func (c *countingSeatLister) count(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reads == nil {
		c.reads = map[string]int{}
	}
	c.reads[key]++
}

func (c *countingSeatLister) GetEnvironment(name, key string) (string, error) {
	c.count("show-environment " + name)
	return c.fakeLister.GetEnvironment(name, key)
}

func (c *countingSeatLister) GetSessionCreatedTime(name string) (time.Time, error) {
	c.count("session-created " + name)
	return c.fakeLister.GetSessionCreatedTime(name)
}

func (c *countingSeatLister) HasSession(name string) (bool, error) {
	c.count("has-session " + name)
	c.mu.Lock()
	fail := c.failHas[name]
	delete(c.failHas, name)
	c.mu.Unlock()
	if fail {
		return false, errors.New("tmux has-session: server busy")
	}
	return true, nil
}

func (c *countingSeatLister) readsOf(verb string) map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]int{}
	for k, n := range c.reads {
		if strings.HasPrefix(k, verb+" ") {
			out[strings.TrimPrefix(k, verb+" ")] = n
		}
	}
	return out
}

// TestSlingCensusIsSharedBySeatDecisionAndAdmission runs one sling's two reads
// of the sandboxed seats (the seat decision's picture and the admission's count
// under the lock) through the census it begins: one gateway listing, and one
// probe per seat per verb, however many reads the sling makes. The reuse step's
// has-session probes are not on this path (see the bead note on gt-l1odqt).
//
//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestSlingCensusIsSharedBySeatDecisionAndAdmission(t *testing.T) {
	town := t.TempDir()
	session.ResetSandboxRoutingForTest(town)
	t.Cleanup(func() { session.ResetSandboxRoutingForTest("") })
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(town, "mayor", "rigs.json"), `{"version":1,"rigs":{}}`)

	const seats = 4
	env := map[string]map[string]string{}
	created := map[string]time.Time{}
	sandboxes := []map[string]string{}
	for i := 0; i < seats; i++ {
		name := fmt.Sprintf("gt-gastown-p%d", i)
		if err := session.WriteSandboxRecord(town, session.SandboxRecord{Session: name, Rig: "gastown", Polecat: fmt.Sprintf("p%d", i), Gateway: "gw"}); err != nil {
			t.Fatal(err)
		}
		env[name] = map[string]string{"GT_ROLE": fmt.Sprintf("gastown/polecats/p%d", i)}
		created[name] = poolTestNow.Add(-time.Hour)
		sandboxes = append(sandboxes, map[string]string{"name": name, "phase": "Ready"})
	}
	listing, err := json.Marshal(map[string]any{"sandboxes": sandboxes})
	if err != nil {
		t.Fatal(err)
	}
	gateway := &sandbox.Fake{Reply: map[string]string{"list": string(listing)}}
	t.Cleanup(session.BeginSandboxCensus(gateway.Runner()))

	lister := &countingSeatLister{fakeLister: fakeLister{sessions: env, created: created}}
	pool := seatPool(claimAgent, seats)
	r := &poolRouter{
		townRoot:    town,
		pool:        func() *config.PolecatPool { return pool },
		sessions:    func() sessionLister { return lister },
		disposition: nil,
		hookHeld:    noHeldHooks,
		work:        submittedWork(),
		seats:       poolSeatLedgerFor(town, newPoolSeatClaimStore()),
		now:         func() time.Time { return poolTestNow },
	}

	// The seat decision's picture, then the admission's count and claim.
	if _, err := r.seatSnapshot(); err != nil {
		t.Fatalf("seat decision read: %v", err)
	}
	if _, _, err := r.route("", true); err != nil {
		t.Fatalf("admission: %v", err)
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
	for _, verb := range []string{"show-environment", "session-created"} {
		reads := lister.readsOf(verb)
		if len(reads) != seats {
			t.Errorf("%s: seats probed = %d, want %d", verb, len(reads), seats)
		}
		for name, n := range reads {
			if n != 1 {
				t.Errorf("%s %s: probes = %d, want 1 across both reads", verb, name, n)
			}
		}
	}
}
