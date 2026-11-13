package session

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
)

// censusGateway is the openshell a census test drives. It answers `sandbox
// list` from listing, fails the first listErrs list calls, and counts the list
// calls it made.
type censusGateway struct {
	mu       sync.Mutex
	listing  string
	listErrs int
	lists    int
}

func (g *censusGateway) run(_ context.Context, argv []string, _ []byte, _ []string) ([]byte, []byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !strings.Contains(strings.Join(argv, " "), " list") {
		return nil, nil, fmt.Errorf("unexpected argv %v", argv)
	}
	g.lists++
	if g.lists <= g.listErrs {
		return nil, []byte("gateway unreachable"), errors.New("exit status 1")
	}
	return []byte(g.listing), nil, nil
}

func (g *censusGateway) listCalls() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lists
}

// seatNames are the sandboxed seats of a census test, and the sandbox listing
// that names them all Ready.
func seatNames(n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("gt-rig-s%02d", i)
	}
	return names
}

// listingOf is the `sandbox list` table of rows: sandbox name to phase.
func listingOf(rows map[string]string) string {
	names := make([]string, 0, len(rows))
	for name := range rows {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("NAME            CREATED              PHASE\n")
	for _, name := range names {
		fmt.Fprintf(&b, "%-15s 2026-11-13 09:00:00  %s\n", name, rows[name])
	}
	return b.String()
}

// seatProbes counts the probes a test makes per verb.
type seatProbes struct {
	mu     sync.Mutex
	counts map[string]int
	// failNext makes the next probe fail, once.
	failNext bool
}

func (p *seatProbes) probe(verb string) func() (string, error) {
	return func() (string, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.counts == nil {
			p.counts = map[string]int{}
		}
		p.counts[verb]++
		if p.failNext {
			p.failNext = false
			return "", errors.New("tmux: no server running")
		}
		return "GT_ROLE=rig/polecats/" + verb, nil
	}
}

func (p *seatProbes) of(verb string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counts[verb]
}

// censusTown is a temp town with one sandbox record per seat name, and the
// census of that town installed for the test.
func censusTown(t *testing.T, gw *censusGateway, names []string) {
	t.Helper()
	town := t.TempDir()
	ResetSandboxRoutingForTest(town)
	t.Cleanup(func() { ResetSandboxRoutingForTest("") })
	for _, name := range names {
		if err := WriteSandboxRecord(town, SandboxRecord{Session: name, Gateway: "gw"}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(BeginSandboxCensus(gw.run))
}

// readSeat is one sling step's census read of a seat's GT_ROLE and creation time.
func readSeat(name string, p *seatProbes) error {
	if _, err := SandboxCensusRead(name, "show-environment", []string{"GT_ROLE"}, p.probe("show-environment")); err != nil {
		return err
	}
	_, err := SandboxCensusRead(name, "session-created", nil, p.probe("session-created"))
	return err
}

func TestSlingCensus(t *testing.T) {
	// The routing is process-wide, so these tests do not run in parallel.
	t.Run("ten seats over three reads: one listing and one probe per seat per verb", func(t *testing.T) {
		names := seatNames(10)
		rows := map[string]string{}
		for _, n := range names {
			rows[n] = "Ready"
		}
		gw := &censusGateway{listing: listingOf(rows)}
		censusTown(t, gw, names)
		var p seatProbes
		for read := 0; read < 3; read++ {
			for _, n := range names {
				if err := readSeat(n, &p); err != nil {
					t.Fatal(err)
				}
			}
		}
		if got := gw.listCalls(); got != 1 {
			t.Errorf("list calls = %d, want 1", got)
		}
		for _, verb := range []string{"show-environment", "session-created"} {
			if got := p.of(verb); got != 10 {
				t.Errorf("%s probes = %d, want 10 (one per seat)", verb, got)
			}
		}
	})

	t.Run("without a census every read probes, as before", func(t *testing.T) {
		names := seatNames(10)
		ResetSandboxRoutingForTest(t.TempDir())
		t.Cleanup(func() { ResetSandboxRoutingForTest("") })
		var p seatProbes
		for read := 0; read < 3; read++ {
			for _, n := range names {
				if err := readSeat(n, &p); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, verb := range []string{"show-environment", "session-created"} {
			if got := p.of(verb); got != 30 {
				t.Errorf("%s probes = %d, want 30 (no census, so one per read)", verb, got)
			}
		}
	})

	t.Run("a seat absent from the listing or not Ready gets no probe", func(t *testing.T) {
		names := []string{"gt-rig-ready", "gt-rig-starting", "gt-rig-gone"}
		gw := &censusGateway{listing: listingOf(map[string]string{
			"gt-rig-ready":    "Ready",
			"gt-rig-starting": "Provisioning",
		})}
		censusTown(t, gw, names)
		var p seatProbes
		for read := 0; read < 2; read++ {
			if _, err := SandboxCensusRead("gt-rig-ready", "show-environment", nil, p.probe("show-environment")); err != nil {
				t.Fatalf("ready seat: %v", err)
			}
			for _, n := range names[1:] {
				_, err := SandboxCensusRead(n, "show-environment", nil, p.probe("show-environment"))
				if !errors.Is(err, ErrSeatNotLive) {
					t.Errorf("%s: err = %v, want ErrSeatNotLive", n, err)
				}
			}
		}
		if got := p.of("show-environment"); got != 1 {
			t.Errorf("probes = %d, want 1 (only the Ready seat is probed, once)", got)
		}
	})

	t.Run("a failed listing is not kept: the next read asks again and falls back to a probe", func(t *testing.T) {
		names := []string{"gt-rig-s00"}
		gw := &censusGateway{listing: listingOf(map[string]string{"gt-rig-s00": "Ready"}), listErrs: 1}
		censusTown(t, gw, names)
		var p seatProbes
		if _, err := SandboxCensusRead("gt-rig-s00", "show-environment", nil, p.probe("show-environment")); err != nil {
			t.Fatalf("first read (listing fails) must fall back to the probe: %v", err)
		}
		if got := p.of("show-environment"); got != 1 {
			t.Fatalf("after a failed listing, probes = %d, want 1 (the per-seat fallback)", got)
		}
		if _, err := SandboxCensusRead("gt-rig-s00", "show-environment", nil, p.probe("show-environment")); err != nil {
			t.Fatal(err)
		}
		if got := gw.listCalls(); got != 2 {
			t.Errorf("list calls = %d, want 2 (the failed one is retried)", got)
		}
		if _, err := SandboxCensusRead("gt-rig-s00", "show-environment", nil, p.probe("show-environment")); err != nil {
			t.Fatal(err)
		}
		if got := p.of("show-environment"); got != 2 {
			t.Errorf("probes after a successful listing = %d, want 2 (the third read is answered from the census)", got)
		}
	})

	t.Run("an empty listing while the registry holds the seat is unknown, not absent", func(t *testing.T) {
		gw := &censusGateway{listing: "No sandboxes found.\n"}
		censusTown(t, gw, []string{"gt-rig-s00"})
		var p seatProbes
		for read := 0; read < 2; read++ {
			if _, err := SandboxCensusRead("gt-rig-s00", "show-environment", nil, p.probe("show-environment")); err != nil {
				t.Fatalf("read %d: %v; an unknown listing must fall back to the probe", read, err)
			}
		}
		if got := p.of("show-environment"); got != 2 {
			t.Errorf("probes = %d, want 2 (not read as absent, not kept)", got)
		}
		if got := gw.listCalls(); got != 2 {
			t.Errorf("list calls = %d, want 2 (an empty listing is not kept)", got)
		}
	})

	t.Run("a probe that errors is asked again by the next read", func(t *testing.T) {
		gw := &censusGateway{listing: listingOf(map[string]string{"gt-rig-s00": "Ready"})}
		censusTown(t, gw, []string{"gt-rig-s00"})
		p := &seatProbes{failNext: true}
		if _, err := SandboxCensusRead("gt-rig-s00", "show-environment", nil, p.probe("show-environment")); err == nil {
			t.Fatal("the failing probe's error was not returned")
		}
		if _, err := SandboxCensusRead("gt-rig-s00", "show-environment", nil, p.probe("show-environment")); err != nil {
			t.Fatalf("second read: %v", err)
		}
		if _, err := SandboxCensusRead("gt-rig-s00", "show-environment", nil, p.probe("show-environment")); err != nil {
			t.Fatal(err)
		}
		if got := p.of("show-environment"); got != 2 {
			t.Errorf("probes = %d, want 2 (the error was retried, the success kept)", got)
		}
	})

	t.Run("a host session with no sandbox record is probed every read", func(t *testing.T) {
		gw := &censusGateway{listing: listingOf(map[string]string{"gt-rig-s00": "Ready"})}
		censusTown(t, gw, []string{"gt-rig-s00"})
		var p seatProbes
		for read := 0; read < 3; read++ {
			if _, err := SandboxCensusRead("gt-host-seat", "show-environment", nil, p.probe("show-environment")); err != nil {
				t.Fatal(err)
			}
		}
		if got := p.of("show-environment"); got != 3 {
			t.Errorf("probes = %d, want 3 (a host seat is not the census's)", got)
		}
		if got := gw.listCalls(); got != 0 {
			t.Errorf("list calls = %d, want 0 for a host seat", got)
		}
	})

	t.Run("forget drops the seat's answers and the listing", func(t *testing.T) {
		gw := &censusGateway{listing: listingOf(map[string]string{"gt-rig-s00": "Ready", "gt-rig-s01": "Ready"})}
		censusTown(t, gw, []string{"gt-rig-s00", "gt-rig-s01"})
		var p seatProbes
		read := func(name string) {
			t.Helper()
			if _, err := SandboxCensusRead(name, "show-environment", nil, p.probe("show-environment")); err != nil {
				t.Fatal(err)
			}
		}
		read("gt-rig-s00")
		read("gt-rig-s01")
		routing.forget("gt-rig-s00")
		read("gt-rig-s00")
		read("gt-rig-s01")
		if got := p.of("show-environment"); got != 3 {
			t.Errorf("probes = %d, want 3 (the forgotten seat asked again, the other kept)", got)
		}
		if got := gw.listCalls(); got != 2 {
			t.Errorf("list calls = %d, want 2 (the listing is read again after a forget)", got)
		}
	})
}

// TestSlingCensusSandboxCreatedAfterTheListing guards the timing the census
// cannot see on its own: a sandbox this sling creates after the listing was
// taken is not in that listing, so it must not read as absent. The create path
// forgets the seat, which reads the listing again.
func TestSlingCensusSandboxCreatedAfterTheListing(t *testing.T) {
	gw := &censusGateway{listing: listingOf(map[string]string{"gt-rig-old": "Ready"})}
	censusTown(t, gw, []string{"gt-rig-old", "gt-rig-new"})
	var p seatProbes
	if _, err := SandboxCensusRead("gt-rig-old", "show-environment", nil, p.probe("show-environment")); err != nil {
		t.Fatal(err)
	}
	if _, err := SandboxCensusRead("gt-rig-new", "show-environment", nil, p.probe("show-environment")); !errors.Is(err, ErrSeatNotLive) {
		t.Fatalf("before the create, a seat the listing does not name: err = %v, want ErrSeatNotLive", err)
	}
	gw.listing = listingOf(map[string]string{"gt-rig-old": "Ready", "gt-rig-new": "Ready"})
	routing.forget("gt-rig-new")
	if _, err := SandboxCensusRead("gt-rig-new", "show-environment", nil, p.probe("show-environment")); err != nil {
		t.Fatalf("after the create is forgotten, the new sandbox is probed: %v", err)
	}
	if got := gw.listCalls(); got != 2 {
		t.Errorf("list calls = %d, want 2 (the forget reads the listing again)", got)
	}
}
