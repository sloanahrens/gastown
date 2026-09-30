package daemon

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/session"
)

// ghostTestEnv holds the test environment for killDefaultPrefixGhosts tests:
// a Daemon over a fake tmux, and the sessions the test put there.
type ghostTestEnv struct {
	daemon *Daemon
	logBuf *strings.Builder
	tm     *fakeTmux
	added  []string
}

// setupGhostTest returns a Daemon over an empty fake tmux in a fresh town,
// reading the prefix registry reg.
func setupGhostTest(t *testing.T, reg *session.PrefixRegistry) *ghostTestEnv {
	t.Helper()
	tm := newFakeTmux(newFixedClock())
	var logBuf strings.Builder
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(&logBuf, "", 0),
		tmux:   tm,
	}
	d.prefixRegistryFn = func() *session.PrefixRegistry { return reg }

	return &ghostTestEnv{daemon: d, logBuf: &logBuf, tm: tm}
}

// addSessions makes the named sessions exist on the fake tmux.
func (e *ghostTestEnv) addSessions(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		e.tm.addSession(name, "claude", time.Time{})
		e.added = append(e.added, name)
	}
}

// writeRigsJSON writes a rigs.json for getKnownRigs().
func writeRigsJSON(t *testing.T, townRoot string, rigs []string) {
	t.Helper()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	entries := make([]string, len(rigs))
	for i, r := range rigs {
		entries[i] = `"` + r + `":{}`
	}
	content := `{"rigs":{` + strings.Join(entries, ",") + `}}`
	if err := os.WriteFile(filepath.Join(mayorDir, "rigs.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// kills returns the sessions the test added that are gone: the daemon killed
// them.
func (e *ghostTestEnv) kills() []string {
	var gone []string
	for _, name := range e.added {
		if has, _ := e.tm.HasSession(name); !has {
			gone = append(gone, name)
		}
	}
	return gone
}

func TestKillDefaultPrefixGhosts_EmptyRegistry(t *testing.T) {
	t.Parallel()

	// Empty registry → allRigs is empty → bail immediately.
	env := setupGhostTest(t, session.NewPrefixRegistry())
	// A ghost-shaped session exists, so an early return is what spares it.
	env.addSessions(t, "gt-witness")

	env.daemon.killDefaultPrefixGhosts()

	kills := env.kills()
	if len(kills) > 0 {
		t.Errorf("expected no kills with empty registry, got: %v", kills)
	}
	if strings.Contains(env.logBuf.String(), "Killing") {
		t.Error("should not log any kill messages with empty registry")
	}
}

func TestKillDefaultPrefixGhosts_GTIsLegitimate(t *testing.T) {
	t.Parallel()

	// Register gastown with "gt" prefix — makes gt-* sessions legitimate.
	reg := session.NewPrefixRegistry()
	reg.Register("gt", "gastown")
	env := setupGhostTest(t, reg)

	// Even if gt-witness exists, it should NOT be killed.
	env.addSessions(t, "gt-witness")

	env.daemon.killDefaultPrefixGhosts()

	kills := env.kills()
	if len(kills) > 0 {
		t.Errorf("expected no kills when gt is legitimate, got: %v", kills)
	}
	if strings.Contains(env.logBuf.String(), "Killing") {
		t.Error("should not kill anything when a rig owns the gt prefix")
	}
}

func TestKillDefaultPrefixGhosts_KillsGhostPatrolSessions(t *testing.T) {
	t.Parallel()

	// Register a rig with non-gt prefix. No rig owns "gt".
	reg := session.NewPrefixRegistry()
	reg.Register("ti", "titanium")
	env := setupGhostTest(t, reg)

	// Ghost sessions exist with default "gt" prefix.
	env.addSessions(t, "gt-witness")

	env.daemon.killDefaultPrefixGhosts()

	kills := env.kills()
	if len(kills) != 1 {
		t.Fatalf("expected 1 kill, got %d: %v", len(kills), kills)
	}
	killSet := map[string]bool{}
	for _, k := range kills {
		killSet[k] = true
	}
	if !killSet["gt-witness"] {
		t.Error("expected gt-witness to be killed")
	}
}

func TestKillDefaultPrefixGhosts_NoKillWhenGhostsAbsent(t *testing.T) {
	t.Parallel()

	// Non-gt registry but no ghost sessions exist.
	reg := session.NewPrefixRegistry()
	reg.Register("ti", "titanium")
	env := setupGhostTest(t, reg)

	// No sessions file entries — nothing exists.

	env.daemon.killDefaultPrefixGhosts()

	kills := env.kills()
	if len(kills) > 0 {
		t.Errorf("expected no kills when ghost sessions don't exist, got: %v", kills)
	}
}

func TestKillDefaultPrefixGhosts_PolecatDuplicate_Killed(t *testing.T) {
	t.Parallel()

	// Register rig with non-gt prefix.
	reg := session.NewPrefixRegistry()
	reg.Register("ti", "titanium")
	env := setupGhostTest(t, reg)

	// Set up rigs.json and polecat directory.
	writeRigsJSON(t, env.daemon.config.TownRoot, []string{"titanium"})
	if err := os.MkdirAll(filepath.Join(env.daemon.config.TownRoot, "titanium", "polecats", "furiosa"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Both ghost and correct sessions exist → ghost is a confirmed duplicate.
	env.addSessions(t, "gt-furiosa", "ti-furiosa")

	env.daemon.killDefaultPrefixGhosts()

	kills := env.kills()
	found := false
	for _, k := range kills {
		if k == "gt-furiosa" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected gt-furiosa to be killed (duplicate), kills: %v", kills)
	}
	if !strings.Contains(env.logBuf.String(), "Killing duplicate ghost polecat session gt-furiosa") {
		t.Errorf("expected duplicate kill log message, got: %s", env.logBuf.String())
	}
}

func TestKillDefaultPrefixGhosts_PolecatSolo_NotKilled(t *testing.T) {
	t.Parallel()

	// Register rig with non-gt prefix.
	reg := session.NewPrefixRegistry()
	reg.Register("ti", "titanium")
	env := setupGhostTest(t, reg)

	// Set up rigs.json and polecat directory.
	writeRigsJSON(t, env.daemon.config.TownRoot, []string{"titanium"})
	if err := os.MkdirAll(filepath.Join(env.daemon.config.TownRoot, "titanium", "polecats", "furiosa"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Only ghost session exists — correct one is absent.
	// Should log warning but NOT kill (may have active work).
	env.addSessions(t, "gt-furiosa")

	env.daemon.killDefaultPrefixGhosts()

	kills := env.kills()
	for _, k := range kills {
		if k == "gt-furiosa" {
			t.Error("should NOT kill solo ghost polecat gt-furiosa (may have active work)")
		}
	}
	if !strings.Contains(env.logBuf.String(), "not killing") {
		t.Errorf("expected 'not killing' log message for solo ghost, got: %s", env.logBuf.String())
	}
}

func TestKillDefaultPrefixGhosts_PolecatSkippedWhenRigUsesDefaultPrefix(t *testing.T) {
	t.Parallel()

	// If any rig uses "gt", gtIsLegitimate is true and the whole function bails.
	reg := session.NewPrefixRegistry()
	reg.Register("gt", "gastown")
	reg.Register("ti", "titanium")
	env := setupGhostTest(t, reg)

	writeRigsJSON(t, env.daemon.config.TownRoot, []string{"gastown", "titanium"})
	if err := os.MkdirAll(filepath.Join(env.daemon.config.TownRoot, "gastown", "polecats", "alice"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(env.daemon.config.TownRoot, "titanium", "polecats", "bob"), 0o755); err != nil {
		t.Fatal(err)
	}

	env.addSessions(t, "gt-alice", "gt-bob")

	env.daemon.killDefaultPrefixGhosts()

	// gtIsLegitimate should cause early return — nothing killed.
	kills := env.kills()
	if len(kills) > 0 {
		t.Errorf("expected no kills when a rig owns gt prefix, got: %v", kills)
	}
}
