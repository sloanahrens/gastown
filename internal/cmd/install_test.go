package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/deps"
	"github.com/steveyegge/gastown/internal/doltserver"
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

func TestBuildBdInitOptions_TownEndpointWithoutReinit(t *testing.T) {
	t.Parallel()
	townDir := writeInstallTown(t, `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z","dolt":{"port":4400}}`, "listener:\n  port: 5500\n")

	opts := buildBdInitOptions(townDir)

	want := beads.InitOptions{Prefix: "hq", ServerPort: 4400}
	if opts != want {
		t.Fatalf("buildBdInitOptions = %+v, want %+v (town.json port, no reinit flag)", opts, want)
	}
}

func TestBuildBdInitOptions_ConfigYAMLWithoutTownJSONEndpoint(t *testing.T) {
	t.Parallel()
	townDir := writeInstallTown(t, "", "listener:\n  host: 127.0.0.2\n  port: 5500\n")

	if opts := buildBdInitOptions(townDir); opts.ServerPort != 5500 {
		t.Fatalf("buildBdInitOptions = %+v, want port 5500 from config.yaml", opts)
	}
	if cfg := bdInitDoltConfig(townDir); cfg.Host != "127.0.0.2" {
		t.Fatalf("bdInitDoltConfig host = %q, want 127.0.0.2 from config.yaml", cfg.Host)
	}
}

// Neither transient running state nor a default makes an endpoint: a town
// without one passes bd no port (gt-y3pgh.3).
func TestBuildBdInitOptions_NoEndpointPassesNoPort(t *testing.T) {
	t.Parallel()
	townDir := t.TempDir()
	daemonDir := filepath.Join(townDir, "daemon")
	if err := os.MkdirAll(daemonDir, 0755); err != nil {
		t.Fatalf("mkdir daemon: %v", err)
	}
	if err := os.WriteFile(filepath.Join(daemonDir, "dolt-state.json"), []byte(`{"running":true,"port":4417}`), 0644); err != nil {
		t.Fatalf("write state: %v", err)
	}

	if opts := buildBdInitOptions(townDir); opts.ServerPort != 0 {
		t.Fatalf("buildBdInitOptions = %+v, want no server port", opts)
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

	env := withBeadsDirEnvFrom([]string{"BEADS_DOLT_SERVER_HOST=stale-host", "BEADS_DOLT_SERVER_PORT=9999", "BEADS_DOLT_PORT=9999"}, beadsDir)
	got := installEnvMap(env)
	if got["BEADS_DOLT_SERVER_HOST"] != "127.0.0.2" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST = %q, want config host in %v", got["BEADS_DOLT_SERVER_HOST"], env)
	}
	if got["BEADS_DOLT_SERVER_PORT"] != "5507" || got["BEADS_DOLT_PORT"] != "5507" {
		t.Fatalf("ports = server:%q legacy:%q, want config port in %v", got["BEADS_DOLT_SERVER_PORT"], got["BEADS_DOLT_PORT"], env)
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

	env := withBeadsDirEnvFrom([]string{"BEADS_DOLT_SERVER_HOST=stale-host", "BEADS_DOLT_SERVER_PORT=9999"}, beadsDir)
	got := installEnvMap(env)
	if _, ok := got["BEADS_DOLT_SERVER_HOST"]; ok {
		t.Fatalf("BEADS_DOLT_SERVER_HOST leaked from config without host: %v", env)
	}
	if got["BEADS_DOLT_SERVER_PORT"] != "5507" || got["BEADS_DOLT_PORT"] != "5507" {
		t.Fatalf("ports = server:%q legacy:%q, want 5507 in %v", got["BEADS_DOLT_SERVER_PORT"], got["BEADS_DOLT_PORT"], env)
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

func TestEnsureEscalationConfig_CreatesDefaultsWhenMissing(t *testing.T) {
	t.Parallel()
	path := config.EscalationConfigPath(t.TempDir())

	created, err := ensureEscalationConfig(path)
	if err != nil {
		t.Fatalf("ensureEscalationConfig: %v", err)
	}
	if !created {
		t.Fatal("ensureEscalationConfig did not create a missing escalation config")
	}

	got, err := config.LoadEscalationConfig(path)
	if err != nil {
		t.Fatalf("LoadEscalationConfig: %v", err)
	}
	want := config.NewEscalationConfig()
	if got.StaleThreshold != want.StaleThreshold {
		t.Errorf("stale_threshold = %q, want %q", got.StaleThreshold, want.StaleThreshold)
	}
	if !reflect.DeepEqual(got.Routes, want.Routes) {
		t.Errorf("routes = %v, want %v", got.Routes, want.Routes)
	}
	if got.MaxReescalations == nil || *got.MaxReescalations != *want.MaxReescalations {
		t.Errorf("max_reescalations = %v, want %v", got.MaxReescalations, want.MaxReescalations)
	}
}

func TestEnsureEscalationConfig_KeepsCustomizedFile(t *testing.T) {
	t.Parallel()
	path := config.EscalationConfigPath(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	customized := `{"type":"escalation","version":1,"routes":{"high":["bead","sms:human"]},"stale_threshold":"9h","max_reescalations":7}` + "\n"
	if err := os.WriteFile(path, []byte(customized), 0o644); err != nil {
		t.Fatal(err)
	}

	created, err := ensureEscalationConfig(path)
	if err != nil {
		t.Fatalf("ensureEscalationConfig: %v", err)
	}
	if created {
		t.Error("ensureEscalationConfig wrote over an existing escalation config")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != customized {
		t.Errorf("escalation.json = %q, want it byte-identical to %q", after, customized)
	}
}

func TestEnsureEscalationConfig_KeepsSectionOnTwoFileTown(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	for _, dir := range []string{"mayor", "settings"} {
		if err := os.MkdirAll(filepath.Join(townRoot, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// "registry" in the machine file marks the two-file layout, so the retired
	// settings/escalation.json lives as a section of settings/config.json
	// (internal/config/layout.go).
	townPath := filepath.Join(townRoot, "mayor", "town.json")
	if err := os.WriteFile(townPath, []byte(`{"type":"town","version":1,"registry":{"version":1,"rigs":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(townRoot, "settings", "config.json")
	customized := `{"escalation":{"type":"escalation","version":1,"routes":{"high":["bead","sms:human"]},"stale_threshold":"9h"}}` + "\n"
	if err := os.WriteFile(configPath, []byte(customized), 0o600); err != nil {
		t.Fatal(err)
	}

	created, err := ensureEscalationConfig(config.EscalationConfigPath(townRoot))
	if err != nil {
		t.Fatalf("ensureEscalationConfig: %v", err)
	}
	if created {
		t.Error("ensureEscalationConfig wrote a section the operator already had")
	}

	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != customized {
		t.Errorf("settings/config.json = %q, want it byte-identical to %q", after, customized)
	}
}

// TestRunInstallEmptyPathArgErrors: cobra.MaximumNArgs(1) accepts "" as the one
// argument, and runInstall indexes the path to expand ~. An empty path must be
// refused rather than panic. The error comes before any path is written, so
// there is no need for an INSTALL_DIR.
func TestRunInstallEmptyPathArgErrors(t *testing.T) {
	t.Parallel()
	err := runInstall(nil, []string{""})
	if err == nil {
		t.Fatal(`runInstall("") returned no error`)
	}
	if !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf(`runInstall("") error = %v, want it to name the empty path`, err)
	}
}

// A fresh install writes the tracker endpoint into town.json, and a re-install
// over a town that names its own port keeps it (gt-p85pw.77).
func TestRunInstallWritesTheTrackerEndpoint(t *testing.T) {
	t.Parallel()
	hqPath := filepath.Join(t.TempDir(), "hq")

	if err := runInstallWith(installOptions{noBeads: true}, []string{hqPath}); err != nil {
		t.Fatalf("gt install --no-beads: %v", err)
	}
	townPath := filepath.Join(hqPath, "mayor", "town.json")
	town, err := config.LoadTownConfig(townPath)
	if err != nil {
		t.Fatalf("load town.json: %v", err)
	}
	if town.Tracker == nil || town.Tracker.Port != config.DefaultTrackerPort {
		t.Fatalf("fresh install town.json tracker = %+v, want port %d", town.Tracker, config.DefaultTrackerPort)
	}

	if err := config.UpdateConfigJSON(townPath, 0o600, func(town *config.TownConfig, _ bool) error {
		town.Tracker = &config.TrackerEndpoint{Host: "tracker.internal", Port: 8000}
		return nil
	}); err != nil {
		t.Fatalf("write the operator's tracker endpoint: %v", err)
	}
	if err := runInstallWith(installOptions{noBeads: true, force: true}, []string{hqPath}); err != nil {
		t.Fatalf("gt install --force: %v", err)
	}
	town, err = config.LoadTownConfig(townPath)
	if err != nil {
		t.Fatalf("load town.json after re-install: %v", err)
	}
	want := config.TrackerEndpoint{Host: "tracker.internal", Port: 8000}
	if town.Tracker == nil || *town.Tracker != want {
		t.Errorf("re-install town.json tracker = %+v, want %+v", town.Tracker, want)
	}
}

// An install with a Dolt host writes that endpoint to town.json and starts no
// local Dolt: a local server's data directory is .dolt-data, and its beads are
// .beads/ (gt-i3ywa).
func TestRunInstallDoltHostWritesTheRemoteEndpoint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		doltPort int
		wantPort int
	}{
		{"the server's default port", 0, doltserver.DefaultPort},
		{"the port the flag names", 3308, 3308},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hqPath := filepath.Join(t.TempDir(), "hq")

			opts := installOptions{doltHost: "scratch-dolt", doltHostSet: true, doltPort: tt.doltPort}
			if err := runInstallWith(opts, []string{hqPath}); err != nil {
				t.Fatalf("gt install --dolt-host scratch-dolt: %v", err)
			}

			town, err := config.LoadTownConfig(filepath.Join(hqPath, "mayor", "town.json"))
			if err != nil {
				t.Fatalf("load town.json: %v", err)
			}
			want := config.DoltEndpoint{Host: "scratch-dolt", Port: tt.wantPort}
			if town.Dolt == nil || *town.Dolt != want {
				t.Fatalf("town.json dolt = %+v, want %+v", town.Dolt, want)
			}
			for _, rel := range []string{".dolt-data", ".beads"} {
				if _, err := os.Stat(filepath.Join(hqPath, rel)); !os.IsNotExist(err) {
					t.Errorf("gt install --dolt-host left %s/ behind (err = %v)", rel, err)
				}
			}
		})
	}
}

// A host that names a local Dolt, an empty one, and --dolt-host beside
// --no-beads are each refused before the install writes anything (gt-i3ywa).
func TestRunInstallRefusesADoltHostThatNamesNoRemote(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts installOptions
		want string
	}{
		{"an empty host", installOptions{doltHostSet: true}, "--dolt-host needs a host"},
		{"localhost", installOptions{doltHost: "localhost", doltHostSet: true}, "loopback"},
		{"127.0.0.1", installOptions{doltHost: "127.0.0.1", doltHostSet: true}, "loopback"},
		{"an ipv6 loopback", installOptions{doltHost: "::1", doltHostSet: true}, "loopback"},
		{"a bracketed ipv6 loopback", installOptions{doltHost: "[::1]", doltHostSet: true}, "loopback"},
		{"beside --no-beads", installOptions{doltHost: "scratch-dolt", doltHostSet: true, noBeads: true}, "--no-beads"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			hqPath := filepath.Join(t.TempDir(), "hq")

			err := runInstallWith(tt.opts, []string{hqPath})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("runInstallWith(%+v) = %v, want an error naming %q", tt.opts, err, tt.want)
			}
			if _, statErr := os.Stat(hqPath); !os.IsNotExist(statErr) {
				t.Errorf("refused install wrote %s (err = %v)", hqPath, statErr)
			}
		})
	}
}

// The endpoint writer leaves a host the town already names alone when the
// endpoint it is given has none, which is what keeps a local --force
// re-install off the operator's endpoint. A local install cannot reach this
// in the unit tier (its beads need a running Dolt), so the writer is called
// directly; TestRunInstallDoltHostWritesTheRemoteEndpoint covers the install's
// own call to it (gt-i3ywa).
func TestStampTownDoltEndpointKeepsTheHostOfALocalInstall(t *testing.T) {
	t.Parallel()
	townPath := filepath.Join(t.TempDir(), "town.json")
	if err := config.SaveTownConfig(townPath, &config.TownConfig{
		Type: "town", Version: config.CurrentTownVersion, Name: "t",
		Dolt: &config.DoltEndpoint{Host: "scratch-dolt", Port: 3307},
	}); err != nil {
		t.Fatalf("write town.json: %v", err)
	}

	if err := stampTownDoltEndpoint(townPath, config.DoltEndpoint{Port: 4400}); err != nil {
		t.Fatalf("stampTownDoltEndpoint: %v", err)
	}

	town, err := config.LoadTownConfig(townPath)
	if err != nil {
		t.Fatalf("load town.json: %v", err)
	}
	want := config.DoltEndpoint{Host: "scratch-dolt", Port: 4400}
	if town.Dolt == nil || *town.Dolt != want {
		t.Fatalf("town.json dolt = %+v, want %+v", town.Dolt, want)
	}
}

// An install that cannot write the tracker endpoint fails rather than
// reporting an HQ whose endpoint is missing.
func TestRunInstallFailsWhenItCannotWriteTheTrackerEndpoint(t *testing.T) {
	t.Parallel()
	hqPath := filepath.Join(t.TempDir(), "hq")
	townPath := filepath.Join(hqPath, "mayor", "town.json")
	if err := os.MkdirAll(filepath.Dir(townPath), 0o755); err != nil {
		t.Fatalf("create mayor/: %v", err)
	}
	if err := os.WriteFile(townPath, []byte(`{"type":`), 0o600); err != nil {
		t.Fatalf("write town.json: %v", err)
	}

	err := runInstallWith(installOptions{noBeads: true, force: true}, []string{hqPath})
	if err == nil || !strings.Contains(err.Error(), "tracker endpoint") {
		t.Fatalf("runInstall over a torn town.json = %v, want it to name the tracker endpoint", err)
	}
}
