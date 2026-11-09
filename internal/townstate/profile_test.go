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

// rehearsalKnown are the patrol keys the daemon knows. events_prune and
// landing_infra are on by default and the fixtures leave them out, so the
// profile must add them disabled; landing_worker is in the fixture already.
var rehearsalKnown = []string{"events_prune", "landing_infra", "landing_worker"}

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
	out, changes, err := RehearsalProfile([]byte(rehearsalFixture), rehearsalKnown)
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
		"daemon.patrols.events_prune.enabled",
		"daemon.patrols.handler.enabled",
		"daemon.patrols.jev_bench.enabled",
		"daemon.patrols.landing_infra.enabled",
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
	wantPatrols := len(inPatrols)
	for _, name := range rehearsalKnown {
		if _, has := inPatrols[name]; !has {
			wantPatrols++
		}
	}
	if len(gotPatrols) != wantPatrols {
		t.Fatalf("output has %d patrols, want %d (the input's and each known patrol it left out)", len(gotPatrols), wantPatrols)
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

// rehearsalDaemonDoc is the five-file layout's mayor/daemon.json: the daemon
// object itself, with no "daemon" key around it.
const rehearsalDaemonDoc = `{
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
  "supervisor": "container",
  "type": "daemon-patrol-config",
  "version": 1
}`

func TestRehearsalProfileTurnsTheDaemonDocumentOff(t *testing.T) {
	t.Parallel()
	out, changes, err := RehearsalDaemonProfile([]byte(rehearsalDaemonDoc), rehearsalKnown)
	if err != nil {
		t.Fatalf("RehearsalDaemonProfile: %v", err)
	}

	// The same patrols change as in the settings shape, named the same way.
	wantChanges := []string{
		"daemon.patrols.disk_maintenance.enabled",
		"daemon.patrols.events_prune.enabled",
		"daemon.patrols.handler.enabled",
		"daemon.patrols.jev_bench.enabled",
		"daemon.patrols.landing_infra.enabled",
		"daemon.patrols.landing_worker.enabled",
		"daemon.patrols.spec_dispatch.enabled",
		"daemon.patrols.tier_sweep.enabled",
		"daemon.patrols.wisp_reaper.enabled",
		"daemon.supervisor",
	}
	if !slices.Equal(changes, wantChanges) {
		t.Fatalf("changes = %v, want %v", changes, wantChanges)
	}

	// The document is the daemon object: the settings shape, wrapped under
	// "daemon", profiles to the same daemon object.
	wrapped := []byte(`{"daemon": ` + rehearsalDaemonDoc + `}`)
	wrappedOut, wrappedChanges, err := RehearsalProfile(wrapped, rehearsalKnown)
	if err != nil {
		t.Fatalf("RehearsalProfile of the wrapped document: %v", err)
	}
	if !slices.Equal(changes, wrappedChanges) {
		t.Errorf("document changes = %v, wrapped settings changes = %v", changes, wrappedChanges)
	}
	got := decodeRehearsalSettings(t, out)
	if want := object(t, decodeRehearsalSettings(t, wrappedOut)["daemon"], "wrapped daemon"); !reflect.DeepEqual(got, want) {
		t.Errorf("document profile = %v, wrapped settings profile = %v", got, want)
	}

	if _, ok := got["supervisor"]; ok {
		t.Errorf("supervisor survived: %v", got["supervisor"])
	}
	if got["container_name"] != "gastown-daemon" || got["type"] != "daemon-patrol-config" || got["version"] != json.Number("1") {
		t.Errorf("a non-patrol key changed: container_name=%v type=%v version=%v", got["container_name"], got["type"], got["version"])
	}
	for name, p := range object(t, got["patrols"], "output patrols") {
		if object(t, p, "patrol "+name)["enabled"] != false {
			t.Errorf("patrol %s is still on: %v", name, p)
		}
	}
}

func TestRehearsalProfileRefusesAMalformedDaemonDocument(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{name: "invalid JSON", doc: `{"patrols":`, want: "not valid JSON"},
		{name: "bytes after the object", doc: `{"patrols":{}} {}`, want: "trailing data"},
		{name: "no patrols object", doc: `{"type":"daemon"}`, want: "no daemon.patrols object"},
		{name: "a patrol not an object", doc: `{"patrols":{"landing_worker":true}}`, want: "daemon.patrols.landing_worker is not an object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, changes, err := RehearsalDaemonProfile([]byte(tc.doc), rehearsalKnown)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("RehearsalDaemonProfile = %v, want a refusal naming %q", err, tc.want)
			}
			if out != nil || changes != nil {
				t.Errorf("a refused document returned output %q and changes %v", out, changes)
			}
		})
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
			out, changes, err := RehearsalProfile([]byte(tc.settings), rehearsalKnown)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("RehearsalProfile = %v, want a refusal naming %q", err, tc.want)
			}
			if out != nil || changes != nil {
				t.Errorf("a refused file returned output %q and changes %v", out, changes)
			}
		})
	}
}

// TestRemoveSupervisorKeepsEveryOtherKey: the cut-over removes daemon.supervisor and
// nothing else. A patrol the rehearsal would turn off stays on, its values keep
// their text, and a file with no supervisor is reported with no change.
func TestRemoveSupervisorKeepsEveryOtherKey(t *testing.T) {
	t.Parallel()
	out, changes, err := RemoveSupervisor([]byte(rehearsalFixture))
	if err != nil {
		t.Fatalf("RemoveSupervisor: %v", err)
	}
	if !slices.Equal(changes, []string{"daemon.supervisor"}) {
		t.Errorf("changes = %q, want only daemon.supervisor", changes)
	}
	got := decodeRehearsalSettings(t, out)
	daemon := object(t, got["daemon"], "daemon")
	if _, has := daemon["supervisor"]; has {
		t.Errorf("daemon.supervisor survives the removal")
	}
	want := decodeRehearsalSettings(t, []byte(rehearsalFixture))
	if !reflect.DeepEqual(got["agents"], want["agents"]) {
		t.Errorf("agents changed: %v, want %v", got["agents"], want["agents"])
	}
	if !reflect.DeepEqual(daemon["patrols"], object(t, want["daemon"], "daemon")["patrols"]) {
		t.Errorf("patrols changed by the supervisor removal")
	}

	again, changes, err := RemoveSupervisor(out)
	if err != nil {
		t.Fatalf("RemoveSupervisor on a removed file: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("a file with no supervisor reports changes %q", changes)
	}
	if !bytes.Equal(again, out) {
		t.Errorf("a second removal rewrote the file")
	}
}

// TestRemoveSupervisorRefusesAMalformedFile: a file that is not one JSON object, or
// whose daemon is not an object, is refused.
func TestRemoveSupervisorRefusesAMalformedFile(t *testing.T) {
	t.Parallel()
	for _, doc := range []string{"{", `{"daemon": {}} trailing`, `{"daemon": "container"}`} {
		if _, _, err := RemoveSupervisor([]byte(doc)); err == nil {
			t.Errorf("RemoveSupervisor(%q) = nil error, want a refusal", doc)
		}
	}
}
