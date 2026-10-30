package cmd

import (
	"errors"
	"fmt"
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

// bd's own default host is 127.0.0.1, so a remote endpoint has to name its
// host or bd init would initialize a database on this machine instead
// (gt-q0ppq).
func TestBuildBdInitOptions_RemoteEndpointNamesTheServer(t *testing.T) {
	t.Parallel()
	townDir := writeInstallTown(t, `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z","dolt":{"host":"dolt.example","port":3307}}`, "")

	want := beads.InitOptions{Prefix: "hq", ServerHost: "dolt.example", ServerPort: 3307}
	if opts := buildBdInitOptions(townDir); opts != want {
		t.Fatalf("buildBdInitOptions = %+v, want %+v", opts, want)
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

// recordingSteps are install steps that record what the install asked them to
// do, so a test needs neither a Dolt server nor bd. Every Dolt step the local
// path runs is here, startDolt included, so the same recorder drives both
// branches and the remote test can tell them apart (gt-q0ppq).
type recordingSteps struct {
	beadChecks      int
	localDoltChecks int
	identityChecks  int
	portChecks      []int
	rigCalls        []string
	starts          []string
	townInits       []string
	townConfigs     []string
	initRigErrs     error
}

func (r *recordingSteps) steps() installSteps {
	return installSteps{
		checkBeads:         func() error { r.beadChecks++; return nil },
		ensureLocalDolt:    func() error { r.localDoltChecks++; return nil },
		ensureDoltIdentity: func() error { r.identityChecks++; return nil },
		checkDoltPort: func(port int) error {
			r.portChecks = append(r.portChecks, port)
			return nil
		},
		initRig: func(_, rigName string) (doltserver.InitRigResult, error) {
			r.rigCalls = append(r.rigCalls, rigName)
			return doltserver.InitRigResult{Created: true}, r.initRigErrs
		},
		startDolt:     func(townRoot string) error { r.starts = append(r.starts, townRoot); return nil },
		initTownBeads: func(townRoot string) error { r.townInits = append(r.townInits, townRoot); return nil },
		configureTownBeads: func(townRoot string) {
			r.townConfigs = append(r.townConfigs, townRoot)
		},
	}
}

// ranLocalDolt names the local-Dolt steps r recorded, for a failure message
// that says which one the install should not have run.
func (r *recordingSteps) ranLocalDolt() []string {
	var ran []string
	if r.localDoltChecks > 0 {
		ran = append(ran, "the dolt binary preflight")
	}
	if r.identityChecks > 0 {
		ran = append(ran, "the dolt identity preflight")
	}
	if len(r.portChecks) > 0 {
		ran = append(ran, "the port availability check")
	}
	if len(r.starts) > 0 {
		ran = append(ran, fmt.Sprintf("a local Dolt start at %q", r.starts))
	}
	return ran
}

// An install with a Dolt host writes that endpoint to town.json, creates the hq
// database on that server, and initializes the town's beads against it without
// starting a local Dolt (gt-q0ppq).
//
// This test judges the install's wiring: which steps it runs, and that none of
// the local-Dolt ones fire. What the real doltserver.InitRig does on that
// wiring is judged against a fake SQL server and a recording process start in
// TestInitRigRemoteCreatesTheDatabaseOnTheServer and its neighbours.
func TestRunInstallDoltHostCreatesHQOnTheServer(t *testing.T) {
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
			rec := &recordingSteps{}

			opts := installOptions{doltHost: "scratch-dolt", doltHostSet: true, doltPort: tt.doltPort, steps: rec.steps()}
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
			if !reflect.DeepEqual(rec.rigCalls, []string{"hq"}) {
				t.Errorf("databases created = %q, want the hq database alone", rec.rigCalls)
			}
			if !reflect.DeepEqual(rec.townInits, []string{hqPath}) {
				t.Errorf("town beads initialized at %q, want %q", rec.townInits, hqPath)
			}
			if !reflect.DeepEqual(rec.townConfigs, []string{hqPath}) {
				t.Errorf("town beads configured at %q, want %q", rec.townConfigs, hqPath)
			}
			if rec.beadChecks != 1 {
				t.Errorf("the install checked for bd %d time(s), want once: bd initializes the beads on the server", rec.beadChecks)
			}
			// The local-Dolt steps are what a remote install must not run:
			// startDolt here is the only way the install starts a server, so
			// a local start added back to this path is recorded and fails.
			if ran := rec.ranLocalDolt(); len(ran) != 0 {
				t.Errorf("gt install --dolt-host ran local Dolt work, want none: %v", ran)
			}
			if _, err := os.Stat(filepath.Join(hqPath, ".dolt-data")); !os.IsNotExist(err) {
				t.Errorf("gt install --dolt-host left .dolt-data/ behind (err = %v)", err)
			}
			if matches, err := filepath.Glob(filepath.Join(hqPath, ".beads", "formulas", "*.toml")); err != nil || len(matches) == 0 {
				t.Errorf("gt install --dolt-host provisioned %d formula(s) (err = %v), want the embedded set", len(matches), err)
			}
		})
	}
}

// A server that cannot create the hq database fails the install with the host
// and port named, and the town's beads are never initialized against it
// (gt-q0ppq).
func TestRunInstallDoltHostNamesAnUnreachableServer(t *testing.T) {
	t.Parallel()
	hqPath := filepath.Join(t.TempDir(), "hq")
	rec := &recordingSteps{initRigErrs: errors.New("dial tcp: connection refused")}

	opts := installOptions{doltHost: "scratch-dolt", doltHostSet: true, doltPort: 3310, steps: rec.steps()}
	err := runInstallWith(opts, []string{hqPath})
	if err == nil {
		t.Fatal("gt install --dolt-host over an unreachable server succeeded, want an error")
	}
	for _, want := range []string{"scratch-dolt:3310", "connection refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if len(rec.townInits) != 0 {
		t.Errorf("the install initialized the town's beads at %q after the server refused, want none", rec.townInits)
	}
	if ran := rec.ranLocalDolt(); len(ran) != 0 {
		t.Errorf("an unreachable remote server fell back to local Dolt work, want none: %v", ran)
	}
}

// A local install keeps the setup a remote one skips: the dolt preflights, the
// port check, the hq database and the server start (gt-q0ppq).
func TestRunInstallKeepsLocalDoltSetup(t *testing.T) {
	t.Parallel()
	hqPath := filepath.Join(t.TempDir(), "hq")
	rec := &recordingSteps{}

	if err := runInstallWith(installOptions{steps: rec.steps()}, []string{hqPath}); err != nil {
		t.Fatalf("gt install: %v", err)
	}
	if rec.localDoltChecks != 1 {
		t.Errorf("the dolt binary preflight ran %d time(s), want once", rec.localDoltChecks)
	}
	if rec.identityChecks != 1 {
		t.Errorf("the dolt identity preflight ran %d time(s), want once", rec.identityChecks)
	}
	if !reflect.DeepEqual(rec.portChecks, []int{doltserver.DefaultPort}) {
		t.Errorf("the port check saw %v, want the default port %d", rec.portChecks, doltserver.DefaultPort)
	}
	if !reflect.DeepEqual(rec.rigCalls, []string{"hq"}) {
		t.Errorf("databases created = %q, want the hq database alone", rec.rigCalls)
	}
	if !reflect.DeepEqual(rec.starts, []string{hqPath}) {
		t.Errorf("local Dolt starts = %q, want one at %q", rec.starts, hqPath)
	}
	if !reflect.DeepEqual(rec.townInits, []string{hqPath}) {
		t.Errorf("town beads initialized at %q, want %q", rec.townInits, hqPath)
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
