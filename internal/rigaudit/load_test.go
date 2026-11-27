package rigaudit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestLoad checks Load builds the input from a town - its rigs, the town config
// and each rig's own settings, field by field - and refuses a registry or town
// config it cannot use, carrying none of the file's contents.
func TestLoad(t *testing.T) {
	t.Parallel()
	town := `{"version":1,"daemon":{"patrols":{"landing_worker":{"rigs":["alpha","bravo"]}}}}`
	alpha := `{"version":1,"merge_queue":{"forgejo":"alpha","post_land_command":"a"}}`
	bravo := `{"version":1,"merge_queue":{"forgejo":"bravo","post_land_command":"b"}}`
	const marker = "untrusted-marker"
	tests := []struct {
		name  string
		setup func(*testing.T, string)
		check func(*testing.T, Input)
	}{
		{"town config and each rig's settings load", func(t *testing.T, root string) {
			writeRigs(t, root, `{"version":1,"rigs":{"alpha":{},"bravo":{}}}`)
			writeConfig(t, root, "settings/config.json", town)
			writeConfig(t, root, "alpha/settings/config.json", alpha)
			writeConfig(t, root, "bravo/settings/config.json", bravo)
		}, func(t *testing.T, in Input) {
			if !slices.Equal(in.Unparked, []string{"alpha", "bravo"}) || len(in.Parked) != 0 {
				t.Errorf("rigs = %v parked %v, want alpha and bravo unparked", in.Unparked, in.Parked)
			}
			if want := doc(t, town); !reflect.DeepEqual(in.Config, want) {
				t.Errorf("Config = %v, want %v", in.Config, want)
			}
			for rig, body := range map[string]string{"alpha": alpha, "bravo": bravo} {
				got, err := in.LoadSettings(rig)
				if err != nil {
					t.Fatalf("LoadSettings(%q): %v", rig, err)
				}
				if want := doc(t, body); !reflect.DeepEqual(got, want) {
					t.Errorf("LoadSettings(%q) = %v, want %v", rig, got, want)
				}
			}
		}},
		{"missing town config", func(t *testing.T, root string) { writeRigs(t, root, `{"version":1,"rigs":{}}`) }, nil},
		{"unparseable town config", func(t *testing.T, root string) {
			writeRigs(t, root, `{"version":1,"rigs":{}}`)
			writeConfig(t, root, "settings/config.json", `{"secret":"untrusted-marker"`)
		}, nil},
		{"null town config", func(t *testing.T, root string) {
			writeRigs(t, root, `{"version":1,"rigs":{}}`)
			writeConfig(t, root, "settings/config.json", `null`)
		}, nil},
		{"unparseable registry", func(t *testing.T, root string) {
			writeRigs(t, root, `{"rigs":{"untrusted-marker":`)
			writeConfig(t, root, "settings/config.json", town)
		}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := testutil.PrivateTempDir(t)
			tc.setup(t, root)
			in, err := Load(root, func(string) string { return "" })
			if tc.check == nil {
				if err == nil {
					t.Fatalf("Load = %+v, <nil>, want an error", in)
				}
				if strings.Contains(err.Error(), marker) {
					t.Fatalf("error carries the file's contents %q: %v", marker, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tc.check(t, in)
		})
	}
}

// doc decodes body as the JSON object Load returns.
func doc(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// writeConfig writes body at rel under root, creating the parent directories.
func writeConfig(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
