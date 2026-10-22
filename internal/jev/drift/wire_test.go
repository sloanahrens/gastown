package drift

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/jev/battery"
	"github.com/steveyegge/gastown/internal/jev/submission"
)

// TestOpenLocksTheCloneFetch: the pass Open builds fetches the submission
// battery's clone under that clone's fetch lock. The origin is unreachable, so
// an unlocked fetch fails at once; a locked one waits out the held lock until
// the context ends.
//
//testpolicy:allow parallel — XDG_CONFIG_HOME is process-wide, and pointing it at the throwaway home here would point every parallel test's Jev client there too
func TestOpenLocksTheCloneFetch(t *testing.T) {
	cfgHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cfgHome, "gt"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgHome, "gt", "jev.env"), []byte("TYPESAFE_API_KEY=test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-env — the Jev client reads its key from XDG_CONFIG_HOME, so the throwaway config home must reach the whole process
	t.Setenv("XDG_CONFIG_HOME", cfgHome)

	town := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(constants.MayorRigsPath(town)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveRigsConfig(constants.MayorRigsPath(town), &config.RigsConfig{
		Version: 1,
		Rigs:    map[string]config.RigEntry{RigName: {GitURL: filepath.Join(town, "no-such-origin.git")}},
	}); err != nil {
		t.Fatal(err)
	}

	p, err := Open(town, Settings{USDPerBillionTokens: 1}, nil)
	if err != nil {
		t.Fatalf("Open = %v, want the pass", err)
	}

	release, err := battery.Lock(submission.FetchLockPath(town))
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := p.Git.Sync(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Sync with the fetch lock held = %v, want the wait to end with the ctx", err)
	}
}
