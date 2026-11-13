package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/session"
)

// TestSlingCensusIsSharedByReuse runs one sling through its seat decision, its
// admission and the reuse step's has-session reads over ten sandboxed seats.
// The sling makes one gateway listing, and each seat's has-session is probed
// once however many readers ask: the seat listing and the reuse of the seat it
// picks share the answer. A probe that failed is not kept, so the reader after
// it asks again. A seat killed in the run is probed afresh afterwards.
//
//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestSlingCensusIsSharedByReuse(t *testing.T) {
	town := t.TempDir()
	session.ResetSandboxRoutingForTest(town)
	t.Cleanup(func() { session.ResetSandboxRoutingForTest("") })
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(town, "mayor", "rigs.json"), `{"version":1,"rigs":{}}`)

	const seats = 10
	const failing = "gt-gastown-p3"
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

	lister := &countingSeatLister{
		fakeLister: fakeLister{sessions: env, created: created},
		failHas:    map[string]bool{failing: true},
	}
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
	if _, err := r.seatSnapshot(); err != nil {
		t.Fatalf("seat decision read: %v", err)
	}
	if _, _, err := r.route("", true); err != nil {
		t.Fatalf("admission: %v", err)
	}

	// The reuse step: the seat listing asks every seat's has-session, and a
	// failed read is asked again by the next reader; then the reuse asks the
	// seat it picks, which the census already holds.
	reuse := censusHasSession{tmux: lister}
	for i := 0; i < seats; i++ {
		name := fmt.Sprintf("gt-gastown-p%d", i)
		if _, err := reuse.HasSession(name); err != nil {
			if _, err := reuse.HasSession(name); err != nil {
				t.Fatalf("has-session %s asked again: %v", name, err)
			}
		}
	}
	picked := "gt-gastown-p0"
	if _, err := reuse.HasSession(picked); err != nil {
		t.Fatalf("reuse has-session %s: %v", picked, err)
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
		if reads := lister.readsOf(verb); len(reads) != seats {
			t.Errorf("%s: seats probed = %d, want %d", verb, len(reads), seats)
		}
	}
	hasReads := lister.readsOf("has-session")
	if len(hasReads) != seats {
		t.Errorf("has-session: seats probed = %d, want %d", len(hasReads), seats)
	}
	for name, n := range hasReads {
		want := 1
		if name == failing {
			want = 2 // the failed probe, then the next reader's
		}
		if n != want {
			t.Errorf("has-session %s: probes = %d, want %d", name, n, want)
		}
	}

	// A seat killed in the run has no record left, so its next read is probed.
	if err := session.CleanupSandboxWith(nil, town, picked); err != nil {
		t.Fatalf("killing %s: %v", picked, err)
	}
	if _, err := reuse.HasSession(picked); err != nil {
		t.Fatalf("has-session after kill: %v", err)
	}
	if got := lister.readsOf("has-session")[picked]; got != 2 {
		t.Errorf("has-session %s after its kill: probes = %d, want 2 (probed afresh)", picked, got)
	}
}
