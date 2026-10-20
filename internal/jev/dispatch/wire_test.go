package dispatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/jev"
)

// fakeBeadStore records the query the production lister sends.
type fakeBeadStore struct {
	opts  beads.ListOptions
	calls int
}

func (f *fakeBeadStore) List(o beads.ListOptions) ([]*beads.Issue, error) {
	f.calls++
	f.opts = o
	return nil, nil
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
	if p.CapUSD != DefaultMaxUSDPerDay {
		t.Errorf("cap = %v, want the default %v", p.CapUSD, DefaultMaxUSDPerDay)
	}

	if _, err := p.Lister.Placed(); err != nil {
		t.Fatalf("Placed: %v", err)
	}
	if store.calls != 1 || store.opts.Status != "in_progress,hooked" {
		t.Errorf("lister asked %d times with status %q, want one ask for in_progress,hooked", store.calls, store.opts.Status)
	}

	// The spend is the town's one ledger, priced at the configured rate and
	// counted whatever caller wrote it.
	ledger := jev.UsageLedgerPath(town)
	if err := os.MkdirAll(filepath.Dir(ledger), 0o700); err != nil {
		t.Fatal(err)
	}
	rec, err := json.Marshal(jev.LedgerRecord{TS: time.Now(), Caller: "jev-dedupe", InputTokens: 250_000_000})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledger, append(rec, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	spent, err := p.SpentToday()
	if err != nil {
		t.Fatalf("SpentToday: %v", err)
	}
	if spent != 1 {
		t.Errorf("SpentToday = %v, want 0.25B tokens at 4 USD per billion to cost 1", spent)
	}
}

// An explicit cap wins over the default.
func TestJevDispatchOpenKeepsTheConfiguredCap(t *testing.T) {
	t.Parallel()
	p, err := open(townWithBeads(t), Settings{MaxUSDPerDay: 2.5, USDPerBillionTokens: 3}, nil,
		keyedClient(t), func(string) beadLister { return &fakeBeadStore{} })
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if p.CapUSD != 2.5 {
		t.Errorf("cap = %v, want the configured 2.5", p.CapUSD)
	}
}
