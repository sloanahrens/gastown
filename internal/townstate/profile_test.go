package townstate

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// rehearsalFixture carries the patrol names of the live settings. landing_worker
// keeps 1.50 and the HTML characters in agents.env keep their text, so the
// rewrite is shown to leave values alone, not only keys.
const rehearsalFixture = `{
  "agents": {"polecat": {"env": {"TOKEN": "<&>"}}},
  "daemon": {
    "container_name": "gastown-daemon",
    "patrols": {
      "disk_maintenance": {"enabled": true, "interval": "1h"},
      "handler": {"enabled": true, "plugins": ["app-promote", "reporters"]},
      "jev_bench": {"interval": 30},
      "jev_patrol": {"enabled": false},
      "landing_worker": {"enabled": true, "max_parallel": 1.50},
      "spec_dispatch": {"enabled": true},
      "tier_sweep": {"enabled": true, "runner_idle_wait": "5m"},
      "wisp_reaper": {}
    },
    "supervisor": "container"
  },
  "operational": {"threshold": 1e3}
}`

// decodeRehearsalSettings reads b the way the profile does: numbers stay text.
func decodeRehearsalSettings(t *testing.T, b []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decoding %s: %v", b, err)
	}
	return m
}

// object asserts v is a JSON object and returns it.
func object(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s = %v, want an object", what, v)
	}
	return m
}

// without returns a copy of m with the named keys left out.
func without(m map[string]any, keys ...string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if !slices.Contains(keys, k) {
			out[k] = v
		}
	}
	return out
}

func TestRehearsalProfileTurnsEveryPatrolOff(t *testing.T) {
	t.Parallel()
	out, changes, err := RehearsalProfile([]byte(rehearsalFixture))
	if err != nil {
		t.Fatalf("RehearsalProfile: %v", err)
	}

	want, err := os.ReadFile("testdata/profile.golden.json")
	if err != nil {
		t.Fatalf("reading the golden: %v", err)
	}
	if string(out) != string(want) {
		t.Fatalf("output differs from the golden:\n%s", out)
	}

	wantChanges := []string{
		"daemon.patrols.disk_maintenance.enabled",
		"daemon.patrols.handler.enabled",
		"daemon.patrols.jev_bench.enabled",
		"daemon.patrols.landing_worker.enabled",
		"daemon.patrols.spec_dispatch.enabled",
		"daemon.patrols.tier_sweep.enabled",
		"daemon.patrols.wisp_reaper.enabled",
		"daemon.supervisor",
	}
	if !slices.Equal(changes, wantChanges) {
		t.Fatalf("changes = %v, want %v", changes, wantChanges)
	}

	in := decodeRehearsalSettings(t, []byte(rehearsalFixture))
	got := decodeRehearsalSettings(t, out)
	inDaemon := object(t, in["daemon"], "input daemon")
	gotDaemon := object(t, got["daemon"], "output daemon")
	if _, ok := gotDaemon["supervisor"]; ok {
		t.Errorf("daemon.supervisor survived: %v", gotDaemon["supervisor"])
	}
	for k := range in {
		if k == "daemon" {
			continue
		}
		if !reflect.DeepEqual(in[k], got[k]) {
			t.Errorf("%s = %v, want the input's %v", k, got[k], in[k])
		}
	}
	for k := range inDaemon {
		if k == "patrols" || k == "supervisor" {
			continue
		}
		if !reflect.DeepEqual(inDaemon[k], gotDaemon[k]) {
			t.Errorf("daemon.%s = %v, want the input's %v", k, gotDaemon[k], inDaemon[k])
		}
	}
	inPatrols := object(t, inDaemon["patrols"], "input patrols")
	gotPatrols := object(t, gotDaemon["patrols"], "output patrols")
	if len(gotPatrols) != len(inPatrols) {
		t.Fatalf("output has %d patrols, input has %d", len(gotPatrols), len(inPatrols))
	}
	for name, inPatrol := range inPatrols {
		gotPatrol := object(t, gotPatrols[name], "patrol "+name)
		if gotPatrol["enabled"] != false {
			t.Errorf("patrol %s enabled = %v, want false", name, gotPatrol["enabled"])
		}
		if !reflect.DeepEqual(without(object(t, inPatrol, "input "+name), "enabled"), without(gotPatrol, "enabled")) {
			t.Errorf("patrol %s changed beyond enabled: %v, want %v", name, gotPatrol, inPatrol)
		}
	}
}

func TestRehearsalProfileRefusesAMalformedFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		settings string
		want     string
	}{
		{name: "invalid JSON", settings: `{"daemon":`, want: "not valid JSON"},
		{name: "bytes after the object", settings: `{"daemon":{"patrols":{}}} {}`, want: "trailing data"},
		{name: "no daemon object", settings: `{"agents":{}}`, want: "no daemon.patrols object"},
		{name: "daemon without patrols", settings: `{"daemon":{}}`, want: "no daemon.patrols object"},
		{name: "patrols not an object", settings: `{"daemon":{"patrols":[]}}`, want: "no daemon.patrols object"},
		{name: "a patrol not an object", settings: `{"daemon":{"patrols":{"landing_worker":true}}}`, want: "daemon.patrols.landing_worker is not an object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, changes, err := RehearsalProfile([]byte(tc.settings))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("RehearsalProfile = %v, want a refusal naming %q", err, tc.want)
			}
			if out != nil || changes != nil {
				t.Errorf("a refused file returned output %q and changes %v", out, changes)
			}
		})
	}
}
