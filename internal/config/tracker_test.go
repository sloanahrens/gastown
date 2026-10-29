package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveTrackerEndpoint(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		townJSON string
		want     TrackerEndpoint
		wantErr  string
	}{
		{"configured host and port", testTownJSON + `,"tracker":{"host":"tracker.internal","port":8000}}`, TrackerEndpoint{Host: "tracker.internal", Port: 8000}, ""},
		{"configured host is trimmed", testTownJSON + `,"tracker":{"host":" tracker.internal ","port":8000}}`, TrackerEndpoint{Host: "tracker.internal", Port: 8000}, ""},
		{"port only leaves the host local", testTownJSON + `,"tracker":{"port":8000}}`, TrackerEndpoint{Port: 8000}, ""},
		{"no tracker entry is the default", testTownJSON + `}`, TrackerEndpoint{Port: DefaultTrackerPort}, ""},
		{"a Dolt endpoint is not a tracker endpoint", testTownJSON + `,"dolt":{"host":"127.0.0.2","port":3307}}`, TrackerEndpoint{Port: DefaultTrackerPort}, ""},
		{"a port that is not a number is an error", testTownJSON + `,"tracker":{"port":"seven"}}`, TrackerEndpoint{}, "town.json"},
		{"a port above the range is an error", testTownJSON + `,"tracker":{"port":70000}}`, TrackerEndpoint{}, "tracker.port 70000 is not a TCP port"},
		{"a missing town.json is the default", "", TrackerEndpoint{Port: DefaultTrackerPort}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveTrackerEndpoint(writeDoltTown(t, tt.townJSON, ""))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ResolveTrackerEndpoint error = %v, want it to name %q", err, tt.wantErr)
				}
				if got != (TrackerEndpoint{}) {
					t.Errorf("ResolveTrackerEndpoint = %+v with an error; it must not fall back to an endpoint", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveTrackerEndpoint: %v", err)
			}
			if got != tt.want {
				t.Errorf("ResolveTrackerEndpoint = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// A town with no town.json at all still resolves: trackerd's default listen
// is the endpoint, so the caller needs no separate absent case.
func TestResolveTrackerEndpoint_NoTownRootDefaults(t *testing.T) {
	t.Parallel()
	got, err := ResolveTrackerEndpoint(t.TempDir())
	if err != nil {
		t.Fatalf("ResolveTrackerEndpoint: %v", err)
	}
	if want := (TrackerEndpoint{Port: DefaultTrackerPort}); got != want {
		t.Errorf("ResolveTrackerEndpoint = %+v, want %+v", got, want)
	}
}

func TestTrackerEndpointURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		ep   TrackerEndpoint
		want string
	}{
		{"an empty host is the local machine", TrackerEndpoint{Port: DefaultTrackerPort}, "http://127.0.0.1:7433"},
		{"a loopback host", TrackerEndpoint{Host: "127.0.0.1", Port: 8000}, "http://127.0.0.1:8000"},
		{"an IPv6 host is bracketed", TrackerEndpoint{Host: "::1", Port: 8000}, "http://[::1]:8000"},
		{"a named host passes through as written", TrackerEndpoint{Host: "tracker.internal", Port: 8000}, "http://tracker.internal:8000"},
		{"a non-loopback IP passes through as written", TrackerEndpoint{Host: "10.0.0.5", Port: 8000}, "http://10.0.0.5:8000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.ep.URL(); got != tt.want {
				t.Errorf("URL() = %q, want %q", got, tt.want)
			}
		})
	}
}

// A town.json the operator wrote with a bad tracker port is refused on load,
// so no path reads it as the default endpoint.
func TestSaveTownConfigRefusesAnOutOfRangeTrackerPort(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "town.json")
	err := SaveTownConfig(path, &TownConfig{
		Type: "town", Version: CurrentTownVersion, Name: "t",
		Tracker: &TrackerEndpoint{Port: 70000},
	})
	if err == nil || !strings.Contains(err.Error(), "tracker.port") {
		t.Fatalf("SaveTownConfig = %v, want it to name tracker.port", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("SaveTownConfig wrote %s over a refused port", path)
	}
}
