package daemon

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestBdReadOnlyEnv verifies that the read-only env holds exactly one
// BD_DOLT_AUTO_COMMIT=off entry, whatever BD_DOLT_AUTO_COMMIT the daemon's
// environment carried.
func TestBdReadOnlyEnv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		base []string
	}{
		{name: "unset parent"},
		{name: "parent has empty", base: []string{"BD_DOLT_AUTO_COMMIT="}},
		{name: "parent has off", base: []string{"BD_DOLT_AUTO_COMMIT=off"}},
		{name: "parent has on", base: []string{"BD_DOLT_AUTO_COMMIT=on"}},
		{name: "parent has stale value", base: []string{"BD_DOLT_AUTO_COMMIT=batched"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := bdReadOnlyRoutingEnvFrom(tc.base, "")

			assertSingleEnvValue(t, env, "BD_DOLT_AUTO_COMMIT", "off")
			assertSingleEnvValue(t, env, "BD_READONLY", "true")
		})
	}
}

func TestBdReadOnlyPinnedEnvUsesSelectedBeadsDir(t *testing.T) {
	t.Parallel()
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	metadata := []byte(`{"dolt_database":"rigdb","dolt_server_host":"127.0.0.1","dolt_server_port":4407}`)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), metadata, 0644); err != nil {
		t.Fatal(err)
	}
	base := []string{
		"BEADS_DIR=/wrong",
		"BEADS_DOLT_SERVER_DATABASE=hq",
		"GT_DOLT_HOST=",
		"GT_DOLT_PORT=",
		"GT_DOLT_DATA=" + filepath.Join(t.TempDir(), "wrong-data"),
		"BD_DOLT_AUTO_COMMIT=on",
	}

	env := bdReadOnlyPinnedEnvFrom(base, beadsDir)
	assertSingleEnvValue(t, env, "BEADS_DIR", beadsDir)
	assertSingleEnvValue(t, env, "BEADS_DOLT_SERVER_DATABASE", "rigdb")
	assertSingleEnvValue(t, env, "BEADS_DOLT_SERVER_PORT", "4407")
	assertSingleEnvValue(t, env, "BEADS_DOLT_PORT", "4407")
	assertSingleEnvValue(t, env, "BD_DOLT_AUTO_COMMIT", "off")
	assertSingleEnvValue(t, env, "BD_READONLY", "true")
	assertSingleEnvValue(t, env, "BD_EXPORT_AUTO", "false")
	assertEnvAbsent(t, env, "BEADS_DOLT_DATA_DIR")
	assertEnvAbsent(t, env, "GT_DOLT_DATA")
}

// TestBdReadOnlyRoutingEnvCarriesTownBeadsDirButNotDatabase pins gt-dl9ug0:
// the daemon's routed read names the town's .beads, the workspace the bd
// gateway forwarder places the call in, and still pins no database, so prefix
// routing chooses it.
func TestBdReadOnlyRoutingEnvCarriesTownBeadsDirButNotDatabase(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	metadata := []byte(`{"dolt_database":"hq","dolt_server_host":"127.0.0.1","dolt_server_port":4407}`)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), metadata, 0644); err != nil {
		t.Fatal(err)
	}
	base := []string{
		"BEADS_DIR=/wrong",
		"BEADS_DOLT_SERVER_DATABASE=wrong",
		"GT_DOLT_HOST=",
		"GT_DOLT_PORT=",
		"GT_DOLT_DATA=" + filepath.Join(t.TempDir(), "wrong-data"),
	}

	env := bdReadOnlyRoutingEnvFrom(base, townRoot)
	assertSingleEnvValue(t, env, "BEADS_DIR", beads.ResolveBeadsDir(beadsDir))
	assertEnvAbsent(t, env, "BEADS_DOLT_SERVER_DATABASE")
	assertSingleEnvValue(t, env, "BEADS_DOLT_SERVER_PORT", "4407")
	assertSingleEnvValue(t, env, "BD_DOLT_AUTO_COMMIT", "off")
	assertSingleEnvValue(t, env, "BD_READONLY", "true")
	assertEnvAbsent(t, env, "BEADS_DOLT_DATA_DIR")
	assertEnvAbsent(t, env, "GT_DOLT_DATA")
}

// TestBdEnvWrappersStartFromTheProcessEnvironment is the wiring guard for the
// ...From twins above: each wrapper must build on the daemon's own
// environment, so a variable every process has (PATH) comes through.
func TestBdEnvWrappersStartFromTheProcessEnvironment(t *testing.T) {
	t.Parallel()
	path, ok := os.LookupEnv("PATH")
	if !ok {
		t.Fatal("PATH is unset in the test process; the guard has nothing to look for")
	}
	for name, env := range map[string][]string{
		"bdReadOnlyEnv":        bdReadOnlyEnv(),
		"bdReadOnlyRoutingEnv": bdReadOnlyRoutingEnv(t.TempDir()),
		"bdMutationRoutingEnv": bdMutationRoutingEnv(t.TempDir()),
		"bdReadOnlyPinnedEnv":  bdReadOnlyPinnedEnv(filepath.Join(t.TempDir(), ".beads")),
	} {
		if !slices.Contains(env, "PATH="+path) {
			t.Errorf("%s does not carry the process PATH: it must start from os.Environ()", name)
		}
	}
}

// TestWorkBeadStateRoutesWithTheTownsBeadsDir pins the call site gt-dl9ug0 was
// filed for: the overrun guard's bead read is a routed call, so the environment
// it hands bd must name the town's .beads and stay read-only. Without a
// BEADS_DIR the bd gateway forwarder refuses the read and the guard cannot
// judge the bead.
func TestWorkBeadStateRoutesWithTheTownsBeadsDir(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	d := &Daemon{config: &Config{TownRoot: townRoot}, logger: log.New(io.Discard, "", 0)}
	var got []string
	d.openWorkBeads = func(env []string, _ time.Duration) workBeadReader {
		got = env
		return workBeadStore{"gt-abc": {ID: "gt-abc", Status: "open"}}
	}

	if _, err := d.workBeadState("gt-abc"); err != nil {
		t.Fatalf("workBeadState: %v", err)
	}
	assertSingleEnvValue(t, got, "BEADS_DIR", beads.ResolveBeadsDir(townRoot))
	assertSingleEnvValue(t, got, "BD_READONLY", "true")
	assertSingleEnvValue(t, got, "BD_DOLT_AUTO_COMMIT", "off")
	assertEnvAbsent(t, got, "BEADS_DOLT_SERVER_DATABASE")
}

func assertSingleEnvValue(t *testing.T, env []string, key, want string) {
	t.Helper()
	var count int
	var value string
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			count++
			value = strings.TrimPrefix(e, key+"=")
		}
	}
	if count != 1 || value != want {
		t.Fatalf("%s count/value = %d/%q, want 1/%q", key, count, value, want)
	}
}

func assertEnvAbsent(t *testing.T, env []string, key string) {
	t.Helper()
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			t.Fatalf("%s should be absent, got %q", key, e)
		}
	}
}
