package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDoltExternalContainer: dolt.external_container survives strict decoding
// into the field the resolver carries, parses to the container name gt runs
// docker start/stop against, and is empty when unset. A value that is not a
// Docker container name is refused rather than handed to docker, where a
// leading '-' would read as a flag (gt-xeb1x).
func TestDoltExternalContainer(t *testing.T) {
	t.Parallel()
	body := `{"type":"town","version":2,"name":"t","dolt":{"port":3307,"external_container":"gt-dolt"}}`
	var town TownConfig
	if err := DecodeJSONFile("town.json", []byte(body), &town); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if town.Dolt == nil {
		t.Fatal("dolt decoded as absent")
	}
	got, err := town.Dolt.ParseExternalContainer()
	if err != nil {
		t.Fatalf("ParseExternalContainer: %v", err)
	}
	if want := "gt-dolt"; got != want {
		t.Errorf("external_container = %q, want %q", got, want)
	}
	ep, ok := ResolveDoltEndpointFor(townRootWithTownJSON(t, body), false)
	if !ok {
		t.Fatal("ResolveDoltEndpointFor found no endpoint")
	}
	if ep.ExternalContainer != "gt-dolt" {
		t.Errorf("resolved endpoint external_container = %q, want %q", ep.ExternalContainer, "gt-dolt")
	}

	// Unset keeps the host-process server, and a town with no dolt section
	// reads the same way.
	if got, err := (&DoltEndpoint{Port: 3307}).ParseExternalContainer(); got != "" || err != nil {
		t.Errorf("unset external_container = %q, %v; want empty, nil", got, err)
	}
	var absent *DoltEndpoint
	if got, err := absent.ParseExternalContainer(); got != "" || err != nil {
		t.Errorf("absent dolt section = %q, %v; want empty, nil", got, err)
	}

	tests := []struct {
		name string
		set  string
	}{
		{name: "leading dash reads as a docker flag", set: "-v"},
		{name: "space", set: "gt dolt"},
		{name: "leading space", set: " gt-dolt"},
		{name: "trailing space", set: "gt-dolt "},
		{name: "path separator", set: "a/b"},
		{name: "docker reference separator", set: "gt/dolt:latest"},
		{name: "shell substitution", set: "$(rm -rf /)"},
		{name: "does not start with a letter or digit", set: "_gt"},
		{name: "newline", set: "gt-dolt\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := &DoltEndpoint{Port: 3307, ExternalContainer: tt.set}
			got, err := d.ParseExternalContainer()
			if err == nil {
				t.Fatalf("ParseExternalContainer(%q) = %q, want an error", tt.set, got)
			}
			if got != "" {
				t.Errorf("ParseExternalContainer(%q) = %q on error, want empty", tt.set, got)
			}
			if !strings.Contains(err.Error(), "dolt.external_container") {
				t.Errorf("error %q does not name the field", err)
			}
		})
	}
}

// townRootWithTownJSON writes body as townRoot/mayor/town.json and returns
// townRoot.
func townRootWithTownJSON(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	mayor := filepath.Join(dir, "mayor")
	if err := os.MkdirAll(mayor, 0o755); err != nil {
		t.Fatalf("creating mayor/: %v", err)
	}
	if err := os.WriteFile(filepath.Join(mayor, "town.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("writing town.json: %v", err)
	}
	return dir
}
