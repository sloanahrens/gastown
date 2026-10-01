package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/deps"
)

func TestBuildBdInitArgs_AlwaysIncludesServerPortWithoutReinit(t *testing.T) {
	t.Parallel()
	townDir := t.TempDir()

	args := buildBdInitArgsWith(townDir, envListGetter([]string{}))

	if len(args) != 6 {
		t.Fatalf("expected 6 args, got %d: %v", len(args), args)
	}
	if args[4] != "--server-port" {
		t.Fatalf("expected args[4] = --server-port, got %q", args[4])
	}
	if args[5] != "3307" {
		t.Fatalf("expected default port 3307, got %q", args[5])
	}
	for _, arg := range args {
		if arg == "--force" || arg == "--reinit-local" {
			t.Fatalf("expected no destructive reinit flag, got %v", args)
		}
	}
}

func TestBuildBdInitArgs_RespectsGTDoltPortEnv(t *testing.T) {
	t.Parallel()
	townDir := t.TempDir()

	args := buildBdInitArgsWith(townDir, envListGetter([]string{"GT_DOLT_PORT=4400"}))

	if args[5] != "4400" {
		t.Fatalf("expected port 4400 from GT_DOLT_PORT, got %q", args[5])
	}
}

func TestBuildBdInitArgs_ConfigYAMLTakesPrecedence(t *testing.T) {
	t.Parallel()
	townDir := t.TempDir()
	doltDataDir := filepath.Join(townDir, ".dolt-data")
	if err := os.MkdirAll(doltDataDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configYAML := "listener:\n  host: 127.0.0.1\n  port: 5500\n"
	if err := os.WriteFile(filepath.Join(doltDataDir, "config.yaml"), []byte(configYAML), 0644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	args := buildBdInitArgsWith(townDir, envListGetter([]string{"GT_DOLT_PORT=4400"}))

	if args[5] != "5500" {
		t.Fatalf("expected port 5500 from config.yaml (precedence over env), got %q", args[5])
	}
}

func TestBdInitDoltConfig_ConfigYAMLHostTakesPrecedence(t *testing.T) {
	t.Parallel()
	townDir := t.TempDir()
	doltDataDir := filepath.Join(townDir, ".dolt-data")
	if err := os.MkdirAll(doltDataDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	configYAML := "listener:\n  host: 127.0.0.2\n  port: 5500\n"
	if err := os.WriteFile(filepath.Join(doltDataDir, "config.yaml"), []byte(configYAML), 0644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	cfg := bdInitDoltConfigWith(townDir, envListGetter([]string{"GT_DOLT_HOST=stale-host"}))
	if cfg.Host != "127.0.0.2" {
		t.Fatalf("expected host 127.0.0.2 from config.yaml (precedence over env), got %q", cfg.Host)
	}
}

func TestBuildBdInitArgs_IgnoresTransientRunningState(t *testing.T) {
	t.Parallel()
	townDir := t.TempDir()
	daemonDir := filepath.Join(townDir, "daemon")
	if err := os.MkdirAll(daemonDir, 0755); err != nil {
		t.Fatalf("mkdir daemon: %v", err)
	}
	if err := os.WriteFile(filepath.Join(daemonDir, "dolt-state.json"), []byte(`{"running":true,"port":4417}`), 0644); err != nil {
		t.Fatalf("write state: %v", err)
	}

	args := buildBdInitArgsWith(townDir, envListGetter([]string{}))

	if args[5] != "3307" {
		t.Fatalf("expected default configured port 3307, got %q", args[5])
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

	env := withBeadsDirEnvFrom([]string{"BEADS_DIR=/wrong", "BEADS_DB=/wrong.db", "BD_DB=/wrong.bd", "BEADS_DOLT_SERVER_DATABASE=wrongdb", "BEADS_DOLT_SERVER_HOST=stale-host", "BEADS_DOLT_SERVER_PORT=9999", "BEADS_DOLT_PORT=9999", "BEADS_DOLT_DATA_DIR=/wrong/data", "BEADS_DOLT_AUTO_START=1", "GT_DOLT_DATA=/wrong/gt-data", "GT_DOLT_HOST=127.0.0.2", "GT_DOLT_PORT=5507"}, envListGetter([]string{"BEADS_DIR=/wrong", "BEADS_DB=/wrong.db", "BD_DB=/wrong.bd", "BEADS_DOLT_SERVER_DATABASE=wrongdb", "BEADS_DOLT_SERVER_HOST=stale-host", "BEADS_DOLT_SERVER_PORT=9999", "BEADS_DOLT_PORT=9999", "BEADS_DOLT_DATA_DIR=/wrong/data", "BEADS_DOLT_AUTO_START=1", "GT_DOLT_DATA=/wrong/gt-data", "GT_DOLT_HOST=127.0.0.2", "GT_DOLT_PORT=5507"}), beadsDir)
	got := installEnvMap(env)
	if got["BEADS_DIR"] != beadsDir {
		t.Fatalf("BEADS_DIR = %q, want %q in %v", got["BEADS_DIR"], beadsDir, env)
	}
	if got["BEADS_DOLT_SERVER_DATABASE"] != "rigdb" {
		t.Fatalf("BEADS_DOLT_SERVER_DATABASE = %q, want rigdb in %v", got["BEADS_DOLT_SERVER_DATABASE"], env)
	}
	if got["BEADS_DOLT_SERVER_HOST"] != "127.0.0.2" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want 127.0.0.2 in %v", got["BEADS_DOLT_SERVER_HOST"], env)
	}
	if got["BEADS_DOLT_SERVER_PORT"] != "5507" || got["BEADS_DOLT_PORT"] != "5507" {
		t.Fatalf("ports = server:%q legacy:%q, want 5507 in %v", got["BEADS_DOLT_SERVER_PORT"], got["BEADS_DOLT_PORT"], env)
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

	env := withBeadsDirEnvFrom([]string{"GT_DOLT_HOST=stale-host", "GT_DOLT_PORT=4400", "BEADS_DOLT_SERVER_HOST=stale-host", "BEADS_DOLT_SERVER_PORT=9999", "BEADS_DOLT_PORT=9999"}, envListGetter([]string{"GT_DOLT_HOST=stale-host", "GT_DOLT_PORT=4400", "BEADS_DOLT_SERVER_HOST=stale-host", "BEADS_DOLT_SERVER_PORT=9999", "BEADS_DOLT_PORT=9999"}), beadsDir)
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

	env := withBeadsDirEnvFrom([]string{"GT_DOLT_HOST=stale-host", "BEADS_DOLT_SERVER_HOST=stale-host", "GT_DOLT_PORT=9999"}, envListGetter([]string{"GT_DOLT_HOST=stale-host", "BEADS_DOLT_SERVER_HOST=stale-host", "GT_DOLT_PORT=9999"}), beadsDir)
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

// envListGetter is a getenv over KEY=VALUE pairs.
func envListGetter(env []string) func(string) string {
	m := installEnvMap(env)
	return func(k string) string { return m[k] }
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
