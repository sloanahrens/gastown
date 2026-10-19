package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// settingsWith wraps a body of top-level members in a town settings document.
func settingsWith(members string) []byte {
	return []byte(`{"type":"town-settings","version":1,` + members + `}`)
}

// Every retired seat key fails the load with a message that names what replaced
// it and says to run gt pool migrate, whichever file holds it (gt-mukgk).
func TestRetiredSeatKeysFailToLoad(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// doc is a town settings member list; key is the path the error names.
		doc, key, replacement string
	}{
		{"pool agent", `"polecat_pool":{"agent":"claude-sonnet"}`, "polecat_pool.agent", "polecat_pool.seats"},
		{"pool max_seats", `"polecat_pool":{"max_seats":3}`, "polecat_pool.max_seats", "polecat_pool.seats.<agent>.cap"},
		{"pool overflow_agent", `"polecat_pool":{"overflow_agent":"claude-sonnet"}`, "polecat_pool.overflow_agent", "polecat_pool.seats"},
		{"pool max_overflow", `"polecat_pool":{"max_overflow":3}`, "polecat_pool.max_overflow", "polecat_pool.seats.<agent>.cap"},
		{"pro agent", `"polecat_pool":{"pro_agent":"claude-opus"}`, "polecat_pool.pro_agent", "polecat_pool.seats"},
		{"pro max", `"polecat_pool":{"pro_max":1}`, "polecat_pool.pro_max", "polecat_pool.seats.<agent>.cap"},
		{"pro label", `"polecat_pool":{"pro_label":"needs-pro"}`, "polecat_pool.pro_label", "an agent:<name> label on the bead"},
		{"hooked agent", `"daemon":{"patrols":{"spec_dispatch":{"hooked_agent":"claude-sonnet"}}}`,
			"daemon.patrols.spec_dispatch.hooked_agent", "polecat_pool.seats"},
		{"max hooked", `"daemon":{"patrols":{"spec_dispatch":{"max_hooked":2}}}`,
			"daemon.patrols.spec_dispatch.max_hooked", "polecat_pool.seats.<agent>.cap"},
		{"prefer hooked", `"daemon":{"patrols":{"spec_dispatch":{"prefer_hooked":true}}}`,
			"daemon.patrols.spec_dispatch.prefer_hooked", "draws uniformly"},
		{"role agents polecat", `"role_agents":{"polecat":"claude-sonnet"}`, "role_agents.polecat", "polecat_pool.seats"},
		{"spec entry agent", `"daemon":{"patrols":{"scheduled_slings":{"enabled":true,"entries":[` +
			`{"name":"deep-review","rig":"gastown","formula":"f","interval":"24h","dispatch":"spec","agent":"claude-sonnet"}]}}}`,
			"daemon.patrols.scheduled_slings.entries[0].agent", "an agent:<name> label"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ts TownSettings
			err := DecodeJSONFile("settings/config.json", settingsWith(tc.doc), &ts)
			if err == nil {
				t.Fatalf("a file holding %s loaded; it must fail", tc.key)
			}
			if !errors.Is(err, ErrUnparseable) {
				t.Errorf("error is not ErrUnparseable: %v", err)
			}
			msg := err.Error()
			for _, want := range []string{tc.key + " was replaced by", tc.replacement, "gt pool migrate"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not contain %q", msg, want)
				}
			}
			if strings.Contains(msg, "fix the file by hand") {
				t.Errorf("error %q tells the operator to hand-edit a file gt pool migrate converts", msg)
			}
		})
	}
}

// The patrol document read on its own (mayor/daemon.json on a five-file town)
// names the same keys without the leading "daemon.".
func TestRetiredSeatKeysFailToLoadInTheStandalonePatrolDocument(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, doc, key string }{
		{"hooked agent", `{"type":"daemon-patrol-config","version":1,"patrols":{"spec_dispatch":{"hooked_agent":"x"}}}`, "patrols.spec_dispatch.hooked_agent"},
		{"max hooked", `{"type":"daemon-patrol-config","version":1,"patrols":{"spec_dispatch":{"max_hooked":1}}}`, "patrols.spec_dispatch.max_hooked"},
		{"spec entry agent", `{"type":"daemon-patrol-config","version":1,"patrols":{"scheduled_slings":{"enabled":true,"entries":[` +
			`{"name":"n","rig":"r","formula":"f","interval":"1h","dispatch":"spec","agent":"claude-haiku"}]}}}`, "patrols.scheduled_slings.entries[0].agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var pc DaemonPatrolConfig
			err := DecodeJSONFile("mayor/daemon.json", []byte(tc.doc), &pc)
			if err == nil {
				t.Fatalf("a patrol document holding %s loaded", tc.key)
			}
			for _, want := range []string{tc.key + " was replaced by", "gt pool migrate"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

// An agent on an entry the scheduler slings itself is still its pin: only the
// entries the spec dispatcher runs lose the key. Other roles keep their
// role_agents entries.
func TestRetiredSeatKeysLeaveTheirNeighboursAlone(t *testing.T) {
	t.Parallel()
	doc := settingsWith(`"role_agents":{"crew":"claude-opus","witness":"claude-haiku"},` +
		`"daemon":{"patrols":{"scheduled_slings":{"enabled":true,"entries":[` +
		`{"name":"direct","rig":"gastown","formula":"f","interval":"1h","agent":"claude-haiku"},` +
		`{"name":"queued","rig":"gastown","formula":"f","interval":"1h","dispatch":"spec"}]}}}`)
	var ts TownSettings
	if err := DecodeJSONFile("settings/config.json", doc, &ts); err != nil {
		t.Fatalf("a file with no retired key failed to load: %v", err)
	}
	if ts.RoleAgents["crew"] != "claude-opus" {
		t.Errorf("role_agents.crew = %q", ts.RoleAgents["crew"])
	}
}

// A key that is unknown but not retired keeps the unknown-key report and its
// advice, and a retired key beside it is reported in the same pass.
func TestRetiredSeatKeysAreReportedBesideUnknownKeys(t *testing.T) {
	t.Parallel()
	var ts TownSettings
	err := DecodeJSONFile("settings/config.json", settingsWith(`"polecat_pool":{"agent":"x","nonsense":1}`), &ts)
	if err == nil {
		t.Fatal("the file loaded")
	}
	for _, want := range []string{"polecat_pool.agent was replaced by", `unknown key "polecat_pool.nonsense"`, "gt pool migrate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}

	err = DecodeJSONFile("settings/config.json", settingsWith(`"polecat_pool":{"nonsense":1}`), &ts)
	if err == nil || !strings.Contains(err.Error(), "fix the file by hand") || strings.Contains(err.Error(), "gt pool migrate") {
		t.Errorf("an unknown key that is not retired must keep the hand-edit advice, got %v", err)
	}
}

// The loaders refuse a file holding a retired key, so a daemon or a gt verb
// that reads it stops with the pointer to the migration rather than running on
// config that does nothing.
func TestLoadOrCreateTownSettingsRefusesARetiredKey(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, settingsWith(`"polecat_pool":{"agent":"deepseek-flash","max_seats":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadOrCreateTownSettings(path)
	if err == nil {
		t.Fatal("a file holding polecat_pool.agent loaded")
	}
	for _, want := range []string{"polecat_pool.agent was replaced by", "polecat_pool.max_seats was replaced by", "gt pool migrate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// A seat cap below zero is refused at load by Validate, so a hand-edited file
// cannot read as a closed seat by accident (gt-71dgi).
func TestPolecatPoolValidateRejectsNegativeSeatCap(t *testing.T) {
	t.Parallel()
	err := (&PolecatPool{Seats: map[string]SeatConfig{"deepseek-flash": {Cap: -1}}}).Validate()
	if err == nil || !strings.Contains(err.Error(), "polecat_pool.seats.deepseek-flash.cap") {
		t.Fatalf("Validate(cap=-1) = %v, want an error naming polecat_pool.seats.deepseek-flash.cap", err)
	}
	if err := (&PolecatPool{Seats: map[string]SeatConfig{"deepseek-flash": {Cap: 0}}}).Validate(); err != nil {
		t.Errorf("Validate(cap=0) = %v, want nil: a cap of 0 closes the seat", err)
	}
}

// The local seat's retired keys (local_agent, max_local, idle_fill) are kept
// verbatim beside the seat map: they decode under strict decoding, and a rewrite
// does not drop operator data.
func TestTownSettings_RetiredLocalPoolKeysStillLoad(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, settingsWith(`"default_agent":"claude","polecat_pool":{`+
		`"local_agent":"local-coder-polecat","max_local":0,"idle_fill":true,"min_spawn_gap":"4m",`+
		`"seats":{"deepseek-flash":{"cap":3}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, err := LoadOrCreateTownSettings(path)
	if err != nil {
		t.Fatalf("LoadOrCreateTownSettings: %v", err)
	}
	seats, err := Seats(ts, nil)
	if err != nil || len(seats) != 1 || seats[0].Agent != "deepseek-flash" || seats[0].Cap != 3 {
		t.Fatalf("seats = %+v, %v", seats, err)
	}
	ts.PolecatPool.ShapeGate = "refuse"
	if err := SaveTownSettings(path, ts); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"local_agent": "local-coder-polecat"`, `"max_local": 0`, `"idle_fill": true`, `"shape_gate": "refuse"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("saved settings lost %s:\n%s", want, raw)
		}
	}
}
