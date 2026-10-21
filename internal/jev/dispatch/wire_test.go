package dispatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev"
)

// fakeBeadStore records every query the production lister sends and answers
// from results, which nil answers with nothing.
type fakeBeadStore struct {
	opts    []beads.ListOptions
	results func(beads.ListOptions) []*beads.Issue
}

func (f *fakeBeadStore) List(o beads.ListOptions) ([]*beads.Issue, error) {
	f.opts = append(f.opts, o)
	if f.results == nil {
		return nil, nil
	}
	return f.results(o), nil
}

// keyedClient builds a Jev client that reads the key from a test file, so the
// test proves Open's own options are enough and needs no key in the
// environment.
func keyedClient(t *testing.T) func(...jev.Option) (*jev.Client, error) {
	t.Helper()
	keyFile := filepath.Join(t.TempDir(), "jev.env")
	if err := os.WriteFile(keyFile, []byte(jev.EnvKey+"=test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return func(opts ...jev.Option) (*jev.Client, error) {
		return jev.NewClient(append(opts, jev.WithKeyPaths(keyFile))...)
	}
}

// townWithBeads makes a town root whose gastown rig has a beads directory.
func townWithBeads(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, RigName, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	return town
}

// Open hands the pass the rig's beads, the town's paths and the town-wide cap,
// and the production lister asks the bead store for the two held statuses.
func TestJevDispatchOpenWiresTheRigListerAndTheCap(t *testing.T) {
	t.Parallel()
	town := townWithBeads(t)
	beadsDir := filepath.Join(town, RigName, ".beads")

	store := &fakeBeadStore{}
	var gotDir string
	p, err := open(town, Settings{USDPerBillionTokens: 4}, nil, keyedClient(t), func(dir string) beadLister {
		gotDir = dir
		return store
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	if gotDir != beadsDir {
		t.Errorf("lister dir = %q, want the rig's beads dir %q", gotDir, beadsDir)
	}
	if p.LogPath != LogPath(town) || p.CursorPath != CursorPath(town) || p.RetryPath != RetryPath(town) {
		t.Errorf("paths = %q, %q, %q, want the town's log, cursor and retry queue", p.LogPath, p.CursorPath, p.RetryPath)
	}
	if ok, err := p.Allows(); err != nil || !ok {
		t.Errorf("Allows on an empty ledger = %v, %v, want true", ok, err)
	}

	since := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	if _, err := p.Lister.Placed(since); err != nil {
		t.Fatalf("Placed: %v", err)
	}
	if len(store.opts) != 2 {
		t.Fatalf("lister asked %d times, want one ask for held beads and one for closed ones", len(store.opts))
	}
	if store.opts[0].Status != "in_progress,hooked" {
		t.Errorf("first ask has status %q, want in_progress,hooked", store.opts[0].Status)
	}
	if store.opts[1].Status != "closed" || !store.opts[1].ClosedAfter.Equal(since) {
		t.Errorf("second ask = status %q, closed after %v; want closed since the cursor %v",
			store.opts[1].Status, store.opts[1].ClosedAfter, since)
	}

	// The spend is the town's one ledger, priced at the configured rate and
	// counted whatever caller wrote it: a dollar spent by another caller
	// reaches the default one-dollar cap and stops the pass.
	writeSpend(t, town, 250_000_000, "jev-dedupe")
	ok, err := p.Allows()
	if err != nil {
		t.Fatalf("Allows: %v", err)
	}
	if ok {
		t.Error("Allows = true with 0.25B tokens at 4 USD per billion spent, want false at the default cap")
	}
}

// writeSpend puts one record in the town's usage ledger.
func writeSpend(t *testing.T, town string, inputTokens int, caller string) {
	t.Helper()
	ledger := jev.UsageLedgerPath(town)
	if err := os.MkdirAll(filepath.Dir(ledger), 0o700); err != nil {
		t.Fatal(err)
	}
	rec, err := json.Marshal(jev.LedgerRecord{TS: time.Now(), Caller: caller, InputTokens: inputTokens})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledger, append(rec, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// An explicit cap wins over the default.
func TestJevDispatchOpenKeepsTheConfiguredCap(t *testing.T) {
	t.Parallel()
	town := townWithBeads(t)
	p, err := open(town, Settings{MaxUSDPerDay: 2.5, USDPerBillionTokens: 4}, nil,
		keyedClient(t), func(string) beadLister { return &fakeBeadStore{} })
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// A dollar fifty spent today is over the default one-dollar cap, so a pass
	// reading the default would stop here, but it is under the 2.5 the settings
	// name.
	writeSpend(t, town, 375_000_000, "jev-dedupe")
	ok, err := p.Allows()
	if err != nil {
		t.Fatalf("Allows: %v", err)
	}
	if !ok {
		t.Error("Allows = false with a dollar spent under the configured 2.5 cap, want true")
	}
}

// A placement slung and closed between two passes is returned by Placed. Only
// the closed read can see it: a bead that finishes inside one quarter-hour
// interval holds no seat when the pass runs, so without this read the log
// samples only the placements that happened to be slow.
func TestPlacedIncludesBeadsClosedSinceLastPass(t *testing.T) {
	t.Parallel()
	cursor := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	held := &beads.Issue{
		ID: "gt-held", Title: "Still working", Status: "in_progress",
		Description: "attached_at: 2026-10-20T10:05:00Z\n\n## Goal\n\nTake a while.\n",
	}
	fast := &beads.Issue{
		ID: "gt-fast", Title: "Done already", Status: "closed",
		Description: "attached_at: 2026-10-20T10:01:00Z\n\n## Goal\n\nFinish fast.\n",
	}
	store := &fakeBeadStore{results: func(o beads.ListOptions) []*beads.Issue {
		if o.Status == "closed" {
			return []*beads.Issue{fast}
		}
		return []*beads.Issue{held}
	}}

	got, err := rigBeads{b: store}.Placed(cursor)
	if err != nil {
		t.Fatalf("Placed: %v", err)
	}
	var ids []string
	for _, b := range got {
		ids = append(ids, b.ID)
	}
	if want := []string{"gt-held", "gt-fast"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("Placed = %v, want %v: the held bead and the one closed since the cursor", ids, want)
	}
}

// With no cursor yet the pass starts from its own time, so Placed skips the
// closed read rather than pulling every bead ever closed.
func TestPlacedSkipsClosedBeadsWithoutACursor(t *testing.T) {
	t.Parallel()
	store := &fakeBeadStore{}

	if _, err := (rigBeads{b: store}).Placed(time.Time{}); err != nil {
		t.Fatalf("Placed: %v", err)
	}
	if len(store.opts) != 1 || store.opts[0].Status != "in_progress,hooked" {
		t.Errorf("asks = %+v, want only the held-bead read before a cursor exists", store.opts)
	}
}
