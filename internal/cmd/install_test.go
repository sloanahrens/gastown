package cmd

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/deps"
)

// writeInstallTown writes a town whose endpoint is in town.json and/or the
// managed config.yaml; an empty body leaves that file out.
func writeInstallTown(t *testing.T, townJSON, configYAML string) string {
	t.Helper()
	townDir := t.TempDir()
	for rel, body := range map[string]string{"mayor/town.json": townJSON, ".dolt-data/config.yaml": configYAML} {
		if body == "" {
			continue
		}
		path := filepath.Join(townDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return townDir
}

func TestBuildBdInitArgs_TownEndpointWithoutReinit(t *testing.T) {
	t.Parallel()
	townDir := writeInstallTown(t, `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z","dolt":{"port":4400}}`, "listener:\n  port: 5500\n")

	args := buildBdInitArgs(townDir)

	want := []string{"init", "--prefix", "hq", "--server", "--server-port", "4400"}
	if !slices.Equal(args, want) {
		t.Fatalf("buildBdInitArgs = %v, want %v (town.json port, no reinit flag)", args, want)
	}
}

func TestBuildBdInitArgs_ConfigYAMLWithoutTownJSONEndpoint(t *testing.T) {
	t.Parallel()
	townDir := writeInstallTown(t, "", "listener:\n  host: 127.0.0.2\n  port: 5500\n")

	if args := buildBdInitArgs(townDir); args[len(args)-1] != "5500" {
		t.Fatalf("buildBdInitArgs = %v, want port 5500 from config.yaml", args)
	}
	if cfg := bdInitDoltConfig(townDir); cfg.Host != "127.0.0.2" {
		t.Fatalf("bdInitDoltConfig host = %q, want 127.0.0.2 from config.yaml", cfg.Host)
	}
}

// Neither transient running state nor a default makes an endpoint: a town
// without one passes bd no port (gt-y3pgh.3).
func TestBuildBdInitArgs_NoEndpointPassesNoPort(t *testing.T) {
	t.Parallel()
	townDir := t.TempDir()
	daemonDir := filepath.Join(townDir, "daemon")
	if err := os.MkdirAll(daemonDir, 0755); err != nil {
		t.Fatalf("mkdir daemon: %v", err)
	}
	if err := os.WriteFile(filepath.Join(daemonDir, "dolt-state.json"), []byte(`{"running":true,"port":4417}`), 0644); err != nil {
		t.Fatalf("write state: %v", err)
	}

	if args := buildBdInitArgs(townDir); slices.Contains(args, "--server-port") {
		t.Fatalf("buildBdInitArgs = %v, want no --server-port", args)
	}
}

func TestWithBeadsDirEnvUsesHardenedBDEnv(t *testing.T) {
	t.Parallel()
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"dolt_database":"rigdb"}`), 0644); err != nil {
		t.Fatal(err)
	}

	env := withBeadsDirEnvFrom([]string{"BEADS_DIR=/wrong", "BEADS_DB=/wrong.db", "BD_DB=/wrong.bd", "BEADS_DOLT_SERVER_DATABASE=wrongdb", "BEADS_DOLT_SERVER_HOST=inherited-host", "BEADS_DOLT_SERVER_PORT=4401", "BEADS_DOLT_PORT=4401", "BEADS_DOLT_DATA_DIR=/wrong/data", "BEADS_DOLT_AUTO_START=1", "GT_DOLT_DATA=/wrong/gt-data", "GT_DOLT_HOST=127.0.0.2", "GT_DOLT_PORT=5507"}, beadsDir)
	got := installEnvMap(env)
	if got["BEADS_DIR"] != beadsDir {
		t.Fatalf("BEADS_DIR = %q, want %q in %v", got["BEADS_DIR"], beadsDir, env)
	}
	if got["BEADS_DOLT_SERVER_DATABASE"] != "rigdb" {
		t.Fatalf("BEADS_DOLT_SERVER_DATABASE = %q, want rigdb in %v", got["BEADS_DOLT_SERVER_DATABASE"], env)
	}
	// Outside a town there is no endpoint: bd gets the endpoint it
	// inherited, and GT_DOLT_* is never translated (gt-y3pgh.3).
	if got["BEADS_DOLT_SERVER_HOST"] != "inherited-host" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want inherited-host in %v", got["BEADS_DOLT_SERVER_HOST"], env)
	}
	if got["BEADS_DOLT_SERVER_PORT"] != "4401" || got["BEADS_DOLT_PORT"] != "4401" {
		t.Fatalf("ports = server:%q legacy:%q, want 4401 in %v", got["BEADS_DOLT_SERVER_PORT"], got["BEADS_DOLT_PORT"], env)
	}
	if got["BEADS_DOLT_AUTO_START"] != "0" || got["BD_DOLT_AUTO_COMMIT"] != "on" {
		t.Fatalf("bd mutation guardrails missing in %v", env)
	}
	for _, key := range []string{"BEADS_DB", "BD_DB", "BEADS_DOLT_DATA_DIR", "GT_DOLT_DATA"} {
		if value, ok := got[key]; ok {
			t.Fatalf("%s leaked as %q in %v", key, value, env)
		}
	}
}

func TestWithBeadsDirEnvUsesTownConfigBeforeMetadataExists(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mayorDir, "town.json"), []byte(`{"name":"test-town"}`), 0644); err != nil {
		t.Fatal(err)
	}
	doltDataDir := filepath.Join(townRoot, ".dolt-data")
	if err := os.MkdirAll(doltDataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(doltDataDir, "config.yaml"), []byte("listener:\n  host: 127.0.0.2\n  port: 5507\n"), 0644); err != nil {
		t.Fatal(err)
	}

	env := withBeadsDirEnvFrom([]string{"GT_DOLT_HOST=stale-host", "GT_DOLT_PORT=4400", "BEADS_DOLT_SERVER_HOST=stale-host", "BEADS_DOLT_SERVER_PORT=9999", "BEADS_DOLT_PORT=9999"}, beadsDir)
	got := installEnvMap(env)
	if got["BEADS_DOLT_SERVER_HOST"] != "127.0.0.2" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want config host in %v", got["BEADS_DOLT_SERVER_HOST"], env)
	}
	if got["BEADS_DOLT_SERVER_PORT"] != "5507" || got["BEADS_DOLT_PORT"] != "5507" {
		t.Fatalf("ports = server:%q legacy:%q, want config port in %v", got["BEADS_DOLT_SERVER_PORT"], got["BEADS_DOLT_PORT"], env)
	}
	if got["GT_DOLT_HOST"] != "127.0.0.2" || got["GT_DOLT_PORT"] != "5507" {
		t.Fatalf("GT endpoint = %q:%q, want config endpoint in %v", got["GT_DOLT_HOST"], got["GT_DOLT_PORT"], env)
	}
}

func TestWithBeadsDirEnvClearsStaleHostWhenConfigHasNoHost(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mayorDir, "town.json"), []byte(`{"name":"test-town"}`), 0644); err != nil {
		t.Fatal(err)
	}
	doltDataDir := filepath.Join(townRoot, ".dolt-data")
	if err := os.MkdirAll(doltDataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(doltDataDir, "config.yaml"), []byte("listener:\n  port: 5507\n"), 0644); err != nil {
		t.Fatal(err)
	}

	env := withBeadsDirEnvFrom([]string{"GT_DOLT_HOST=stale-host", "BEADS_DOLT_SERVER_HOST=stale-host", "GT_DOLT_PORT=9999"}, beadsDir)
	got := installEnvMap(env)
	if _, ok := got["GT_DOLT_HOST"]; ok {
		t.Fatalf("GT_DOLT_HOST leaked from config without host: %v", env)
	}
	if _, ok := got["BEADS_DOLT_SERVER_HOST"]; ok {
		t.Fatalf("BEADS_DOLT_SERVER_HOST leaked from config without host: %v", env)
	}
	if got["GT_DOLT_PORT"] != "5507" || got["BEADS_DOLT_SERVER_PORT"] != "5507" {
		t.Fatalf("ports = GT:%q server:%q, want 5507 in %v", got["GT_DOLT_PORT"], got["BEADS_DOLT_SERVER_PORT"], env)
	}
}

func installEnvMap(env []string) map[string]string {
	out := make(map[string]string)
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			out[key] = value
		}
	}
	return out
}

func TestEnsureBeadsConfigYAML_CreatesWhenMissing(t *testing.T) {
	t.Parallel()
	beadsDir := t.TempDir()

	if err := beads.EnsureConfigYAML(beadsDir, "hq"); err != nil {
		t.Fatalf("EnsureConfigYAML: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(beadsDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}

	got := string(data)
	want := "prefix: hq\nissue-prefix: hq\ndolt.idle-timeout: \"0\"\nexport.auto: \"false\"\n"
	if got != want {
		t.Fatalf("config.yaml = %q, want %q", got, want)
	}
}

func TestEnsureBeadsConfigYAML_RepairsPrefixKeysAndPreservesOtherLines(t *testing.T) {
	t.Parallel()
	beadsDir := t.TempDir()
	path := filepath.Join(beadsDir, "config.yaml")
	original := strings.Join([]string{
		"# existing settings",
		"prefix: wrong",
		"sync-branch: main",
		"issue-prefix: wrong",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	if err := beads.EnsureConfigYAML(beadsDir, "hq"); err != nil {
		t.Fatalf("EnsureConfigYAML: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "prefix: hq\n") {
		t.Fatalf("config.yaml missing repaired prefix: %q", text)
	}
	if !strings.Contains(text, "issue-prefix: hq\n") {
		t.Fatalf("config.yaml missing repaired issue-prefix: %q", text)
	}
	if !strings.Contains(text, "sync-branch: main\n") {
		t.Fatalf("config.yaml should preserve unrelated settings: %q", text)
	}
}

func TestEnsureBeadsConfigYAML_AddsMissingIssuePrefixKey(t *testing.T) {
	t.Parallel()
	beadsDir := t.TempDir()
	path := filepath.Join(beadsDir, "config.yaml")
	if err := os.WriteFile(path, []byte("prefix: hq\n"), 0644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	if err := beads.EnsureConfigYAML(beadsDir, "hq"); err != nil {
		t.Fatalf("EnsureConfigYAML: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config.yaml: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "prefix: hq\n") {
		t.Fatalf("config.yaml missing prefix: %q", text)
	}
	if !strings.Contains(text, "issue-prefix: hq\n") {
		t.Fatalf("config.yaml missing issue-prefix: %q", text)
	}
}

func TestFormatInstallDoltError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		status    deps.DoltStatus
		version   string
		detail    string
		goos      string
		want      []string
		wantNoErr bool
	}{
		{
			name:      "ok",
			status:    deps.DoltOK,
			wantNoErr: true,
		},
		{
			name:   "missing darwin suggests homebrew",
			status: deps.DoltNotFound,
			goos:   "darwin",
			want:   []string{"dolt is required", "brew install dolt", "--no-beads"},
		},
		{
			name:    "too old includes minimum",
			status:  deps.DoltTooOld,
			version: "1.0.0",
			goos:    "linux",
			want:    []string{"dolt 1.0.0 is too old", deps.MinDoltVersion, "Upgrade Dolt"},
		},
		{
			name:   "exec failed includes detail",
			status: deps.DoltExecFailed,
			detail: "permission denied",
			goos:   "linux",
			want:   []string{"'dolt version' failed", "permission denied", "Reinstall Dolt"},
		},
		{
			name:   "unknown fails closed",
			status: deps.DoltUnknown,
			detail: "unexpected output",
			goos:   "linux",
			want:   []string{"version could not be parsed", "unexpected output", "Reinstall Dolt"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := formatInstallDoltError(tt.status, tt.version, tt.detail, tt.goos)
			if tt.wantNoErr {
				if err != nil {
					t.Fatalf("formatInstallDoltError returned error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("formatInstallDoltError returned nil, want error")
			}
			msg := err.Error()
			for _, want := range tt.want {
				if !strings.Contains(msg, want) {
					t.Fatalf("error missing %q:\n%s", want, msg)
				}
			}
		})
	}
}
