package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentconfig "github.com/steveyegge/gastown/internal/config"
)

func boolPtr(b bool) *bool { return &b }

// decodePatrols decodes daemon.json text the way the daemon loads it.
func decodePatrols(t *testing.T, text string) *DaemonPatrolConfig {
	t.Helper()
	var cfg DaemonPatrolConfig
	if err := agentconfig.DecodeJSONFile("daemon.json", []byte(text), &cfg); err != nil {
		t.Fatalf("decoding %s: %v", text, err)
	}
	return &cfg
}

// A hand-written spec_dispatch block that tunes the dispatcher but omits
// "enabled" leaves it on: the field is documented as on unless disabled.
func TestIsPatrolEnabledSpecDispatchOmittedEnabled(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		`{"patrols":{"spec_dispatch":{"interval":"30s"}}}`,
		`{"patrols":{"spec_dispatch":{"max_per_tick":2}}}`,
		`{"patrols":{"spec_dispatch":{}}}`,
	} {
		if !IsPatrolEnabled(decodePatrols(t, text), "spec_dispatch") {
			t.Errorf("%s: spec_dispatch off, want on", text)
		}
	}
}

func TestIsPatrolEnabledSpecDispatchExplicitFalse(t *testing.T) {
	t.Parallel()
	cfg := decodePatrols(t, `{"patrols":{"spec_dispatch":{"enabled":false}}}`)
	if IsPatrolEnabled(cfg, "spec_dispatch") {
		t.Error(`"enabled": false: spec_dispatch on, want off`)
	}
	cfg = decodePatrols(t, `{"patrols":{"spec_dispatch":{"enabled":true,"interval":"30s"}}}`)
	if !IsPatrolEnabled(cfg, "spec_dispatch") {
		t.Error(`"enabled": true: spec_dispatch off, want on`)
	}
}

// theDaemonJSON is a mayor/daemon.json a leftover file on a migrated town
// would hold, distinguishable from a config read out of its host section.
const theDaemonJSON = `{"type":"daemon-patrol-config","version":1,"container_name":"live-daemon-9","compose_project":"live-9"}`

// TestReadPatrolConfigRefusesABrokenOperatorSettings: on the two-file layout
// the patrol config is settings/config.json's "daemon" section, and
// config.readConfigFile defers a host that does not parse to the host's own
// loader (it cannot tell whether the section is there), so a broken operator
// file used to reach every caller of ReadPatrolConfig as "no patrol config" —
// and still did when a leftover mayor/daemon.json supplied the config, which
// the daemon started from while the operator's own file was broken (gt-dcxbw).
//
// A settings file that parses is accepted whatever keys it declares. That
// leniency is the point: the strict loader (LoadTownSettings) rejects a key it
// does not declare, and running it here would turn an undeclared key into an
// error for every caller of this reader. Swap the check for the strict loader
// and the "no daemon section" rows go red.
func TestReadPatrolConfigRefusesABrokenOperatorSettings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// settings is the body of settings/config.json; emptyDir writes it as
		// a directory instead; empty leaves the file absent.
		settings    string
		emptyDir    bool
		daemonJSON  string // body of mayor/daemon.json; empty leaves it absent
		wantErr     bool
		wantParse   bool   // the error is ErrUnparseable, not a read failure
		wantFile    string // file the error must name, relative to the town
		wantProject string // compose project of the config a clean read returns
	}{
		{name: "absent settings, no daemon.json"},
		{name: "absent settings, daemon.json present", daemonJSON: theDaemonJSON, wantProject: "live-9"},
		{name: "settings with no daemon section", settings: `{"unrelated":1}`},
		{name: "settings with no daemon section, daemon.json present", settings: `{"unrelated":1}`, daemonJSON: theDaemonJSON, wantProject: "live-9"},
		{name: "settings holding the daemon section", settings: `{"daemon":{"type":"daemon-patrol-config","version":1,"container_name":"from-section","compose_project":"from-section"}}`, wantProject: "from-section"},
		{name: "settings is not valid JSON", settings: `{not json`, wantErr: true, wantParse: true, wantFile: "settings/config.json"},
		{name: "settings is not valid JSON, daemon.json present", settings: `{not json`, daemonJSON: theDaemonJSON, wantErr: true, wantParse: true, wantFile: "settings/config.json"},
		{name: "settings is not a JSON object", settings: `[]`, wantErr: true, wantParse: true, wantFile: "settings/config.json"},
		{name: "settings is not a JSON object, daemon.json present", settings: `[]`, daemonJSON: theDaemonJSON, wantErr: true, wantParse: true, wantFile: "settings/config.json"},
		{name: "settings is truncated", settings: `{"daemon":{"type":"daemon-patrol-config"`, daemonJSON: theDaemonJSON, wantErr: true, wantParse: true, wantFile: "settings/config.json"},
		{name: "settings cannot be read", emptyDir: true, wantErr: true, wantFile: "settings/config.json"},
		{name: "settings cannot be read, daemon.json present", emptyDir: true, daemonJSON: theDaemonJSON, wantErr: true, wantFile: "settings/config.json"},
		{name: "daemon.json is not valid JSON", daemonJSON: `{not json`, wantErr: true, wantParse: true, wantFile: "mayor/daemon.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			settings := filepath.Join(town, "settings", "config.json")
			switch {
			case tt.emptyDir:
				if err := os.MkdirAll(filepath.Join(town, "settings"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(settings, 0o755); err != nil {
					t.Fatal(err)
				}
			case tt.settings != "":
				writeTownFile(t, town, "settings/config.json", tt.settings)
			}
			if tt.daemonJSON != "" {
				writeTownFile(t, town, "mayor/daemon.json", tt.daemonJSON)
			}

			cfg, err := ReadPatrolConfig(town)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("ReadPatrolConfig = %v, want no error", err)
				}
				got := ""
				if cfg != nil {
					got = cfg.ComposeProject()
				}
				if got != tt.wantProject {
					t.Fatalf("ReadPatrolConfig compose project = %q, want %q", got, tt.wantProject)
				}
				return
			}
			if err == nil {
				t.Fatalf("ReadPatrolConfig = %+v, nil; want the refusal", cfg)
			}
			if cfg != nil {
				t.Errorf("ReadPatrolConfig = %+v with the error, want no config: a refusal is not a partial answer", cfg)
			}
			if got := errors.Is(err, agentconfig.ErrUnparseable); got != tt.wantParse {
				t.Errorf("errors.Is(err, ErrUnparseable) = %v, want %v (error: %v)", got, tt.wantParse, err)
			}
			want := filepath.Join(town, tt.wantFile)
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to name %s", err, want)
			}
		})
	}
}
