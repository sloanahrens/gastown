package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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

func TestBdReadOnlyRoutingEnvDoesNotPinDatabase(t *testing.T) {
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
	assertEnvAbsent(t, env, "BEADS_DIR")
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
		t.Fatalf("%s count/value = %d/%q, want 1/%q in %v", key, count, value, want, env)
	}
}

func assertEnvAbsent(t *testing.T, env []string, key string) {
	t.Helper()
	for _, e := range env {
		if strings.HasPrefix(e, key+"=") {
			t.Fatalf("%s should be absent, got %q in %v", key, e, env)
		}
	}
}
