package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
)

// censusGateway is the openshell a census test drives. It answers `sandbox
// list` from listing, fails the first listErrs list calls, and counts the list
// calls it made. When gate is set, the first list call announces itself on
// entered and then blocks until gate is closed.
type censusGateway struct {
	mu       sync.Mutex
	listing  string
	listErrs int
	lists    int
	gate     chan struct{}
	entered  chan struct{}
}

func (g *censusGateway) run(_ context.Context, argv []string, _ []byte, _ []string) ([]byte, []byte, error) {
	g.mu.Lock()
	if !strings.Contains(strings.Join(argv, " "), " list") {
		g.mu.Unlock()
		return nil, nil, fmt.Errorf("unexpected argv %v", argv)
	}
	g.lists++
	first := g.lists == 1
	fail := g.lists <= g.listErrs
	g.mu.Unlock()
	if first && g.gate != nil {
		g.entered <- struct{}{}
		<-g.gate
	}
	if fail {
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

// listingOf is the `sandbox list -o json` reply of rows: sandbox name to phase.
func listingOf(rows map[string]string) string {
	names := make([]string, 0, len(rows))
	for name := range rows {
		names = append(names, name)
	}
	sort.Strings(names)
	sandboxes := make([]map[string]string, 0, len(names))
	for _, name := range names {
		sandboxes = append(sandboxes, map[string]string{"name": name, "phase": rows[name]})
	}
	b, err := json.Marshal(map[string]any{"sandboxes": sandboxes})
	if err != nil {
		panic(err)
	}
	return string(b)
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

//testpolicy:allow parallel — the census is one process-wide answer that BeginSandboxCensus installs, so these tests run one at a time
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
			"gt-rig-starting": "Stopped",
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

	t.Run("a failed listing is remembered for the sling: one list call, every read falls back to a probe", func(t *testing.T) {
		names := seatNames(10)
		rows := map[string]string{}
		for _, n := range names {
			rows[n] = "Ready"
		}
		gw := &censusGateway{listing: listingOf(rows), listErrs: 1}
		censusTown(t, gw, names)
		var p seatProbes
		for read := 0; read < 3; read++ {
			for _, n := range names {
				if err := readSeat(n, &p); err != nil {
					t.Fatalf("read %d of %s: %v; a failed listing must fall back to the probe", read, n, err)
				}
			}
		}
		if got := gw.listCalls(); got != 1 {
			t.Errorf("list calls = %d, want 1 (a failed listing is not asked again within the sling)", got)
		}
		for _, verb := range []string{"show-environment", "session-created"} {
			if got := p.of(verb); got != 30 {
				t.Errorf("%s probes = %d, want 30 (the per-seat fallback on every read)", verb, got)
			}
		}
	})

	t.Run("the next sling reads the listing again after a failed one", func(t *testing.T) {
		names := []string{"gt-rig-s00"}
		gw := &censusGateway{listing: listingOf(map[string]string{"gt-rig-s00": "Ready"}), listErrs: 1}
		censusTown(t, gw, names)
		var p seatProbes
		if _, err := SandboxCensusRead("gt-rig-s00", "show-environment", nil, p.probe("show-environment")); err != nil {
			t.Fatalf("first sling, failed listing: %v", err)
		}
		// A new sling's census starts clean.
		t.Cleanup(BeginSandboxCensus(gw.run))
		if _, err := SandboxCensusRead("gt-rig-s00", "show-environment", nil, p.probe("show-environment")); err != nil {
			t.Fatal(err)
		}
		if _, err := SandboxCensusRead("gt-rig-s00", "show-environment", nil, p.probe("show-environment")); err != nil {
			t.Fatal(err)
		}
		if got := gw.listCalls(); got != 2 {
			t.Errorf("list calls = %d, want 2 (one failed in the first sling, one that answers in the second)", got)
		}
		if got := p.of("show-environment"); got != 2 {
			t.Errorf("probes = %d, want 2 (the first read's fallback, then the second sling's census answers from cache)", got)
		}
	})

	t.Run("seats asking at once share one listing in flight, and the census lock is free during the call", func(t *testing.T) {
		names := seatNames(10)
		rows := map[string]string{}
		for _, n := range names {
			rows[n] = "Ready"
		}
		gw := &censusGateway{listing: listingOf(rows), gate: make(chan struct{}), entered: make(chan struct{}, 1)}
		censusTown(t, gw, names)
		// Release the blocked list on every path, so a failure above does not
		// leave the seats' goroutines waiting on it.
		var release sync.Once
		openGate := func() { release.Do(func() { close(gw.gate) }) }
		t.Cleanup(openGate)
		// A seat whose answer is already kept, read while the list is blocked.
		known := seatKey("gt-rig-known", "show-environment", []string{"GT_ROLE"})
		installedCensus().keep(known, "GT_ROLE=rig/polecats/known")
		var p seatProbes
		var wg sync.WaitGroup
		errs := make(chan error, len(names))
		for _, n := range names {
			wg.Add(1)
			go func(name string) {
				defer wg.Done()
				errs <- readSeat(name, &p)
			}(n)
		}
		// allRead closes once every read has returned, so a regression that
		// makes no list call fails here instead of waiting for one.
		allRead := make(chan struct{})
		go func() {
			wg.Wait()
			close(allRead)
		}()
		select {
		case <-gw.entered:
		case <-allRead:
			t.Fatal("every read returned without a list call reaching the gateway")
		}
		// The first read is inside the list call, which holds no census lock.
		// A census lock held across that call makes TryLock fail, at once and
		// without a timer.
		c := installedCensus()
		if !c.mu.TryLock() {
			t.Fatal("the census lock is held while a list call is in flight: a second reader would wait on the gateway")
		}
		c.mu.Unlock()
		if _, ok := c.cached(known); !ok {
			t.Error("cache lookup during the in-flight list found no kept answer for the known seat")
		}
		openGate()
		<-allRead
		close(errs)
		for err := range errs {
			if err != nil {
				t.Errorf("concurrent read: %v", err)
			}
		}
		if got := gw.listCalls(); got != 1 {
			t.Errorf("list calls = %d, want 1 (ten seats in flight share one listing)", got)
		}
		for _, verb := range []string{"show-environment", "session-created"} {
			if got := p.of(verb); got != 10 {
				t.Errorf("%s probes = %d, want 10 (one per seat)", verb, got)
			}
		}
	})

	t.Run("an empty listing while the registry holds the seat is unknown, not absent", func(t *testing.T) {
		gw := &censusGateway{listing: listingOf(map[string]string{})}
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
		if got := gw.listCalls(); got != 1 {
			t.Errorf("list calls = %d, want 1 (an empty listing is remembered as unavailable for the sling)", got)
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
//
//testpolicy:allow parallel — the census is one process-wide answer that BeginSandboxCensus installs, so these tests run one at a time
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

// TestSlingCensusPhases pins how each listed phase is read: Ready is live and
// probed once; Stopped, Error and Failed are not live and get no probe; any
// other phase is neither, so the seat is probed on every read and nothing is
// kept.
//
//testpolicy:allow parallel — the census is one process-wide answer that BeginSandboxCensus installs, so these tests run one at a time
func TestSlingCensusPhases(t *testing.T) {
	tests := []struct {
		phase     string
		notLive   bool
		wantProbe int
	}{
		{phase: "Ready", wantProbe: 1},
		{phase: "READY", wantProbe: 1},
		{phase: " Ready ", wantProbe: 1},
		{phase: "Stopped", notLive: true},
		{phase: "Error", notLive: true},
		{phase: "failed", notLive: true},
		{phase: "Provisioning", wantProbe: 2},
		{phase: "Pending", wantProbe: 2},
		{phase: "Starting", wantProbe: 2},
		{phase: "Hibernating", wantProbe: 2},
	}
	for _, tt := range tests {
		t.Run(tt.phase, func(t *testing.T) {
			gw := &censusGateway{listing: listingOf(map[string]string{"gt-rig-s00": tt.phase})}
			censusTown(t, gw, []string{"gt-rig-s00"})
			var p seatProbes
			for read := 0; read < 2; read++ {
				_, err := SandboxCensusRead("gt-rig-s00", "show-environment", nil, p.probe("show-environment"))
				if tt.notLive && !errors.Is(err, ErrSeatNotLive) {
					t.Fatalf("read %d: err = %v, want ErrSeatNotLive", read, err)
				}
				if !tt.notLive && err != nil {
					t.Fatalf("read %d: err = %v, want nil", read, err)
				}
			}
			if got := p.of("show-environment"); got != tt.wantProbe {
				t.Errorf("probes = %d, want %d for phase %q", got, tt.wantProbe, tt.phase)
			}
		})
	}
}
