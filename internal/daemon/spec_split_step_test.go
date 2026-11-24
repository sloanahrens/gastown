package daemon

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/beads"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// specSplitTestDaemon builds a daemon whose spec_split step is on or off,
// with a fake clock so the tick's timing is the zero a test can read and a
// locked log, which the tick's goroutine writes while the test reads.
func specSplitTestDaemon(t *testing.T, enabled bool) (*Daemon, *lockedBuffer) {
	t.Helper()
	logged := &lockedBuffer{}
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(logged, "", 0),
		clock:  clockwork.NewFakeClockAt(time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)),
	}
	if enabled {
		d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			SpecSplit: &PatrolConfig{Enabled: true},
		}}
	}
	return d, logged
}

// specSplitRunTick drives the heartbeat entry, the production call site, and
// waits for the goroutine it starts.
func specSplitRunTick(t *testing.T, d *Daemon) {
	t.Helper()
	d.specSplitStep()
	d.specSplitCycles.Wait()
}

// specSplitTestBead is one candidate the fake reader answers.
func specSplitTestBead(id, assignee string, labels ...string) *beads.Issue {
	return &beads.Issue{ID: id, Assignee: assignee, Labels: labels}
}

// specSplitReadStore answers each List with the next page and records the
// options it read with, so a test drives the production reader of a town.
type specSplitReadStore struct {
	mu    sync.Mutex
	pages [][]*beads.Issue
	errs  map[int]error
	opts  []beads.ListOptions
}

func (s *specSplitReadStore) Show(string) (*beads.Issue, error) {
	return nil, errors.New("the split step reads no bead by id")
}

func (s *specSplitReadStore) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opts = append(s.opts, opts)
	n := len(s.opts)
	if err := s.errs[n]; err != nil {
		return nil, err
	}
	if n > len(s.pages) {
		return nil, nil
	}
	return s.pages[n-1], nil
}

func TestSpecSplitStepTick(t *testing.T) {
	t.Parallel()
	t.Run("the patrols.spec_split key decodes and ships off", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "daemon.json")
		if err := os.WriteFile(path, []byte(`{"patrols":{"spec_split":{"enabled":true}}}`), 0o600); err != nil {
			t.Fatalf("writing the block: %v", err)
		}
		cfg, err := agentconfig.LoadDaemonPatrolConfig(path)
		if err != nil {
			t.Fatalf("loading the block: %v", err)
		}
		if !IsPatrolEnabled(cfg, specSplitPatrol) {
			t.Error("spec_split enabled:true did not enable the step")
		}
		if IsPatrolEnabled(&DaemonPatrolConfig{}, specSplitPatrol) {
			t.Error("spec_split is on with no key, want off")
		}
	})

	t.Run("disabled does nothing", func(t *testing.T) {
		t.Parallel()
		d, logged := specSplitTestDaemon(t, false)
		reads := 0
		var applied []string
		d.specSplitDeps = SpecSplitDeps{
			Candidates: func() ([]*beads.Issue, error) {
				reads++
				return []*beads.Issue{specSplitTestBead("gt-a", "", "split")}, nil
			},
			Apply: func(_, id string) (string, error) {
				applied = append(applied, id)
				return "", nil
			},
		}
		d.specSplitStep()
		if reads != 0 {
			t.Errorf("candidates read %d times with the step off, want 0", reads)
		}
		if len(applied) != 0 {
			t.Errorf("applied %v with the step off, want nothing", applied)
		}
		if logged.String() != "" {
			t.Errorf("logged %q with the step off, want nothing", logged.String())
		}
	})

	t.Run("enabled splits at most two eligible beads", func(t *testing.T) {
		t.Parallel()
		d, logged := specSplitTestDaemon(t, true)
		candidates := []*beads.Issue{
			nil,
			specSplitTestBead("gt-a", "", "split"),
			specSplitTestBead("gt-assigned", "gastown/polecats/x", "split"),
			specSplitTestBead("gt-rework", "", "split", "rework"),
			specSplitTestBead("gt-hold", "", "split", "hold:decision"),
			specSplitTestBead("gt-not-split", ""),
			specSplitTestBead("gt-b", "", "split"),
			specSplitTestBead("gt-c", "", "split"),
		}
		var applied []string
		d.specSplitDeps = SpecSplitDeps{
			Candidates: func() ([]*beads.Issue, error) { return candidates, nil },
			Apply: func(_, id string) (string, error) {
				applied = append(applied, id)
				return "split " + id + " into two pieces\n", nil
			},
		}
		specSplitRunTick(t, d)
		want := []string{"gt-a", "gt-b"}
		if !slices.Equal(applied, want) {
			t.Errorf("applied %v, want %v: two at most, skipping an assigned, reworked, held or unlabeled bead", applied, want)
		}
		if got := logged.String(); !strings.Contains(got, "split 2 bead(s) in 0s") {
			t.Errorf("log = %q, want the fake clock's zero timing for two beads", got)
		}
	})

	t.Run("a refused apply is logged and the tick goes on", func(t *testing.T) {
		t.Parallel()
		d, logged := specSplitTestDaemon(t, true)
		var applied []string
		d.specSplitDeps = SpecSplitDeps{
			Candidates: func() ([]*beads.Issue, error) {
				return []*beads.Issue{
					specSplitTestBead("gt-a", "", "split"),
					specSplitTestBead("gt-b", "", "split"),
				}, nil
			},
			Apply: func(_, id string) (string, error) {
				applied = append(applied, id)
				if id == "gt-a" {
					return "verdict: fails: piece A: lint refused: too big\n", errors.New("exit status 1")
				}
				return "split gt-b into two pieces\n", nil
			},
		}
		specSplitRunTick(t, d)
		if want := []string{"gt-a", "gt-b"}; !slices.Equal(applied, want) {
			t.Errorf("applied %v, want %v", applied, want)
		}
		got := logged.String()
		for _, line := range []string{"gt-a was not split", "lint refused: too big"} {
			if !strings.Contains(got, line) {
				t.Errorf("log = %q, want it to name %q", got, line)
			}
		}
	})

	t.Run("a refusal does not use a split slot", func(t *testing.T) {
		t.Parallel()
		d, logged := specSplitTestDaemon(t, true)
		var applied []string
		d.specSplitDeps = SpecSplitDeps{
			Candidates: func() ([]*beads.Issue, error) {
				return []*beads.Issue{
					specSplitTestBead("gt-r1", "", "split"),
					specSplitTestBead("gt-r2", "", "split"),
					specSplitTestBead("gt-a", "", "split"),
					specSplitTestBead("gt-b", "", "split"),
					specSplitTestBead("gt-c", "", "split"),
				}, nil
			},
			Apply: func(_, id string) (string, error) {
				applied = append(applied, id)
				if strings.HasPrefix(id, "gt-r") {
					return "verdict: fails: piece A: lint refused\n", errors.New("exit status 1")
				}
				return "split " + id + " into two pieces\n", nil
			},
		}
		specSplitRunTick(t, d)
		// gt-c is left for the next tick: the two refusals used two of the
		// four attempts and no split slot, so the two beads behind them split.
		if want := []string{"gt-r1", "gt-r2", "gt-a", "gt-b"}; !slices.Equal(applied, want) {
			t.Errorf("applied %v, want %v", applied, want)
		}
		if got := logged.String(); !strings.Contains(got, "split 2 bead(s)") {
			t.Errorf("log = %q, want two splits reported", got)
		}
	})

	t.Run("one tick starts at most four applies", func(t *testing.T) {
		t.Parallel()
		d, logged := specSplitTestDaemon(t, true)
		var refused []string
		var candidates []*beads.Issue
		for _, id := range []string{"gt-1", "gt-2", "gt-3", "gt-4", "gt-5", "gt-6"} {
			candidates = append(candidates, specSplitTestBead(id, "", "split"))
		}
		d.specSplitDeps = SpecSplitDeps{
			Candidates: func() ([]*beads.Issue, error) { return candidates, nil },
			Apply: func(_, id string) (string, error) {
				refused = append(refused, id)
				return "verdict: fails: piece B: lint refused\n", errors.New("exit status 1")
			},
		}
		specSplitRunTick(t, d)
		if len(refused) != 4 {
			t.Errorf("applied %d beads in one tick, want the cap of 4: %v", len(refused), refused)
		}
		if got := logged.String(); strings.Contains(got, "bead(s)") {
			t.Errorf("log = %q, want no split count when every apply refused", got)
		}
	})

	t.Run("a candidate read error splits nothing", func(t *testing.T) {
		t.Parallel()
		d, logged := specSplitTestDaemon(t, true)
		applied := 0
		d.specSplitDeps = SpecSplitDeps{
			Candidates: func() ([]*beads.Issue, error) { return nil, errors.New("bd is down") },
			Apply: func(_, _ string) (string, error) {
				applied++
				return "", nil
			},
		}
		specSplitRunTick(t, d)
		if applied != 0 {
			t.Errorf("applied %d beads after a failed read, want 0", applied)
		}
		if got := logged.String(); !strings.Contains(got, "bd is down") {
			t.Errorf("log = %q, want it to name the read error", got)
		}
	})

	t.Run("no apply installed splits nothing", func(t *testing.T) {
		t.Parallel()
		d, logged := specSplitTestDaemon(t, true)
		d.specSplitDeps = SpecSplitDeps{
			Candidates: func() ([]*beads.Issue, error) {
				return []*beads.Issue{specSplitTestBead("gt-a", "", "split")}, nil
			},
		}
		specSplitRunTick(t, d)
		if got := logged.String(); !strings.Contains(got, "no apply is installed") {
			t.Errorf("log = %q, want it to name the missing apply", got)
		}
	})

	t.Run("no town root splits nothing", func(t *testing.T) {
		t.Parallel()
		d, logged := specSplitTestDaemon(t, true)
		d.config = nil
		specSplitRunTick(t, d)
		if got := logged.String(); !strings.Contains(got, "no town root") {
			t.Errorf("log = %q, want it to name the missing town root", got)
		}
	})

	t.Run("the production reader lists each rig's open unassigned split beads", func(t *testing.T) {
		t.Parallel()
		d, _ := specSplitTestDaemon(t, true)
		d.knownRigsCache, d.knownRigsCacheValid = []string{"rig-a", "rig-b"}, true
		store := &specSplitReadStore{pages: [][]*beads.Issue{
			{specSplitTestBead("gt-a", "", "split")},
			{specSplitTestBead("gt-b", "", "split")},
		}}
		d.openWorkBeads = func([]string, time.Duration) workBeadReader { return store }
		var applied []string
		d.specSplitDeps = SpecSplitDeps{
			Apply: func(_, id string) (string, error) {
				applied = append(applied, id)
				return "", nil
			},
		}
		specSplitRunTick(t, d)
		if want := []string{"gt-a", "gt-b"}; !slices.Equal(applied, want) {
			t.Errorf("applied %v, want %v from the two rigs' stores", applied, want)
		}
		if len(store.opts) != 2 {
			t.Fatalf("the reader listed %d stores, want one per rig (2)", len(store.opts))
		}
		for i, got := range store.opts {
			if got.Status != "open" || got.Label != specdispatch.SplitLabel || !got.NoAssignee || got.Priority != -1 || got.Limit != 0 {
				t.Errorf("store %d was listed with %+v, want open beads labeled %q with no assignee", i, got, specdispatch.SplitLabel)
			}
		}
	})

	t.Run("a rig store that cannot be read splits nothing", func(t *testing.T) {
		t.Parallel()
		d, logged := specSplitTestDaemon(t, true)
		d.knownRigsCache, d.knownRigsCacheValid = []string{"rig-a", "rig-b"}, true
		store := &specSplitReadStore{
			pages: [][]*beads.Issue{{specSplitTestBead("gt-a", "", "split")}},
			errs:  map[int]error{2: errors.New("bd is down")},
		}
		d.openWorkBeads = func([]string, time.Duration) workBeadReader { return store }
		applied := 0
		d.specSplitDeps = SpecSplitDeps{
			Apply: func(_, _ string) (string, error) {
				applied++
				return "", nil
			},
		}
		specSplitRunTick(t, d)
		if applied != 0 {
			t.Errorf("applied %d beads from a partial town, want 0", applied)
		}
		got := logged.String()
		for _, line := range []string{"cannot read the split-labeled beads", "rig-b", "bd is down"} {
			if !strings.Contains(got, line) {
				t.Errorf("log = %q, want it to name %q", got, line)
			}
		}
	})

	t.Run("the tick runs off the heartbeat and one at a time", func(t *testing.T) {
		t.Parallel()
		d, logged := specSplitTestDaemon(t, true)
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		var applies atomic.Int32
		d.specSplitDeps = SpecSplitDeps{
			Candidates: func() ([]*beads.Issue, error) {
				return []*beads.Issue{specSplitTestBead("gt-a", "", "split")}, nil
			},
			Apply: func(_, id string) (string, error) {
				applies.Add(1)
				entered <- struct{}{}
				<-release
				return "split gt-a into two pieces\n", nil
			},
		}
		d.specSplitStep()
		within(t, 5*time.Second, "the first apply", entered)
		// A heartbeat while the apply runs starts no second tick.
		d.specSplitStep()
		close(release)
		d.specSplitCycles.Wait()
		if got := applies.Load(); got != 1 {
			t.Errorf("the step applied %d beads, want 1: a second tick started while the first was in its apply", got)
		}
		if !strings.Contains(logged.String(), "previous tick is still running") {
			t.Errorf("log = %q, want the line that says the running tick skipped this one", logged.String())
		}
	})
}
