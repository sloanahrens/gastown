package doltserver

import (
	"strings"
	"testing"
)

// TestDefaultConfig_TimeZoneEmptySettingOptsOut verifies that an explicitly
// empty operational.dolt.time_zone disables the override (caller wants Dolt's
// host-TZ default).
func TestDefaultConfig_TimeZoneEmptySettingOptsOut(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeTownDoltSettings(t, townRoot, `{"time_zone":""}`)
	config := newFakeHost().host().DefaultConfig(townRoot)

	if config.TimeZone != "" {
		t.Errorf("TimeZone = %q with explicit empty setting, want empty (opt-out)", config.TimeZone)
	}
}

// TestDefaultConfig_TimeZoneSettingsOverride verifies that
// operational.dolt.time_zone replaces the default.
func TestDefaultConfig_TimeZoneSettingsOverride(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeTownDoltSettings(t, townRoot, `{"time_zone":"America/Los_Angeles"}`)
	config := newFakeHost().host().DefaultConfig(townRoot)

	if config.TimeZone != "America/Los_Angeles" {
		t.Errorf("TimeZone = %q, want America/Los_Angeles", config.TimeZone)
	}
}

// TestDefaultConfig_TimeZoneDefaultWithoutSettings verifies the compiled-in
// default holds when the town has no settings file.
func TestDefaultConfig_TimeZoneDefaultWithoutSettings(t *testing.T) {
	t.Parallel()
	config := newFakeHost().host().DefaultConfig(t.TempDir())

	if config.TimeZone != DefaultTimeZone {
		t.Errorf("TimeZone = %q, want the default %q", config.TimeZone, DefaultTimeZone)
	}
}

// TestBuildTimeZoneQuery verifies the exact SQL emitted by applyTimeZone.
func TestBuildTimeZoneQuery(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tz   string
		want string
	}{
		{"+00:00", "SET GLOBAL time_zone = '+00:00'"},
		{"UTC", "SET GLOBAL time_zone = 'UTC'"},
		{"America/Los_Angeles", "SET GLOBAL time_zone = 'America/Los_Angeles'"},
	}
	for _, tc := range cases {
		got := buildTimeZoneQuery(tc.tz)
		if got != tc.want {
			t.Errorf("buildTimeZoneQuery(%q) = %q, want %q", tc.tz, got, tc.want)
		}
	}
}

// TestApplyTimeZone_EmptyShortCircuits verifies that the override is opted out of: no SET
// GLOBAL reaches dolt.
func TestApplyTimeZone_EmptyShortCircuits(t *testing.T) {
	t.Parallel()
	f := newFakeHost()
	for _, v := range []string{""} {
		f.host().applyTimeZone(t.TempDir(), &Config{TimeZone: v})
	}
	if ran := f.ranMatching("SET GLOBAL"); len(ran) != 0 {
		t.Errorf("SET GLOBAL sent for an opted-out value: %q", ran)
	}
}

// TestApplyTimeZone_DispatchesQuery verifies that a set value sends the expected
// SET GLOBAL statement to the town's running server.
func TestApplyTimeZone_DispatchesQuery(t *testing.T) {
	t.Parallel()
	f := newFakeHost().townPort(4520).on("dolt *", fakeReply{})
	f.host().applyTimeZone(t.TempDir(), &Config{TimeZone: "+00:00"})

	ran := f.ranMatching("SET GLOBAL")
	if len(ran) != 1 || !strings.HasSuffix(ran[0], "sql -q SET GLOBAL time_zone = '+00:00'") || !strings.Contains(ran[0], "--port 4520") {
		t.Errorf("dolt calls = %q, want one %q against port 4520", ran, "SET GLOBAL time_zone = '+00:00'")
	}
}

// TestApplyTimeZone_ErrorIsBestEffort verifies that a SQL failure does not panic or
// propagate: the server is already up, and failing here would fail the start.
func TestApplyTimeZone_ErrorIsBestEffort(t *testing.T) {
	t.Parallel()
	f := newFakeHost().on("dolt *", fakeReply{stderr: "simulated SQL failure", code: 1})
	f.host().applyTimeZone(t.TempDir(), &Config{TimeZone: "+00:00"})
	if len(f.ranMatching("SET GLOBAL")) != 1 {
		t.Errorf("the statement was not attempted: %q", f.commands())
	}
}
