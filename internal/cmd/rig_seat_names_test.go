package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// seatNamesRig returns a town root and a rig name under it, with the rig's
// directory made so settings and config files can be written into it.
func seatNamesRig(t *testing.T) (townRoot, rigName string) {
	t.Helper()
	townRoot = t.TempDir()
	rigName = "alpha"
	if err := os.MkdirAll(filepath.Join(townRoot, rigName, "settings"), 0o755); err != nil {
		t.Fatalf("making rig dir: %v", err)
	}
	return townRoot, rigName
}

// writeRigSettingsNamepool saves a rig settings file whose namepool block is pool.
func writeRigSettingsNamepool(t *testing.T, townRoot, rigName string, pool *config.NamepoolConfig) {
	t.Helper()
	s := config.NewRigSettings()
	s.Namepool = pool
	if err := config.SaveRigSettings(filepath.Join(townRoot, rigName, "settings", "config.json"), s); err != nil {
		t.Fatalf("saving rig settings: %v", err)
	}
}

func TestRigSeatNames(t *testing.T) {
	t.Parallel()
	t.Run("custom names capped by max_before_numbering, seat names lower-cased once and sorted", func(t *testing.T) {
		townRoot, rigName := seatNamesRig(t)
		writeRigSettingsNamepool(t, townRoot, rigName, &config.NamepoolConfig{
			Names:              []string{"Nova", "Ember", "Quill"},
			MaxBeforeNumbering: 2,
		})

		got, err := rigSeatNames(townRoot, rigName, map[string][]string{
			"opus": {"Aegis", "NOVA"},
		})
		if err != nil {
			t.Fatalf("rigSeatNames: unexpected error: %v", err)
		}
		want := []string{"aegis", "ember", "nova"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("rigSeatNames = %v, want %v", got, want)
		}
	})

	t.Run("rig config polecat_names draw the pool when no settings file exists", func(t *testing.T) {
		townRoot, rigName := seatNamesRig(t)
		cfg := `{"type":"rig","version":1,"name":"alpha","git_url":"https://example.invalid/alpha.git","created_at":"2026-01-01T00:00:00Z","polecat_names":["Kite","Gale"]}`
		if err := os.WriteFile(filepath.Join(townRoot, rigName, "config.json"), []byte(cfg), 0o644); err != nil {
			t.Fatalf("writing rig config: %v", err)
		}

		got, err := rigSeatNames(townRoot, rigName, nil)
		if err != nil {
			t.Fatalf("rigSeatNames: unexpected error: %v", err)
		}
		want := []string{"gale", "kite"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("rigSeatNames = %v, want %v", got, want)
		}
	})

	t.Run("unloadable settings file is an error naming the rig and no names", func(t *testing.T) {
		townRoot, rigName := seatNamesRig(t)
		if err := os.WriteFile(filepath.Join(townRoot, rigName, "settings", "config.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatalf("writing settings: %v", err)
		}

		got, err := rigSeatNames(townRoot, rigName, map[string][]string{"opus": {"Aegis"}})
		if err == nil {
			t.Fatalf("rigSeatNames = %v, want an error", got)
		}
		if !strings.Contains(err.Error(), rigName) {
			t.Errorf("error %q does not name rig %q", err, rigName)
		}
		if got != nil {
			t.Errorf("rigSeatNames returned names %v with the error, want none", got)
		}
	})

	t.Run("unloadable rig config is an error naming the rig and no names", func(t *testing.T) {
		townRoot, rigName := seatNamesRig(t)
		if err := os.WriteFile(filepath.Join(townRoot, rigName, "config.json"), []byte("{not json"), 0o644); err != nil {
			t.Fatalf("writing rig config: %v", err)
		}

		got, err := rigSeatNames(townRoot, rigName, nil)
		if err == nil {
			t.Fatalf("rigSeatNames = %v, want an error", got)
		}
		if !strings.Contains(err.Error(), rigName) {
			t.Errorf("error %q does not name rig %q", err, rigName)
		}
		if got != nil {
			t.Errorf("rigSeatNames returned names %v with the error, want none", got)
		}
	})

	t.Run("a rig name that is not one directory name is refused with no names", func(t *testing.T) {
		townRoot, _ := seatNamesRig(t)
		for _, rigName := range []string{"", ".", "..", "../alpha", `..\alpha`, "a/b"} {
			got, err := rigSeatNames(townRoot, rigName, nil)
			if err == nil {
				t.Errorf("rigSeatNames(%q) = %v, want an error", rigName, got)
			}
			if got != nil {
				t.Errorf("rigSeatNames(%q) returned names %v with the error, want none", rigName, got)
			}
		}
	})

	t.Run("unresolved theme is an error naming the rig and no names", func(t *testing.T) {
		townRoot, rigName := seatNamesRig(t)
		writeRigSettingsNamepool(t, townRoot, rigName, &config.NamepoolConfig{
			Style: "no-such-theme-file",
		})

		got, err := rigSeatNames(townRoot, rigName, map[string][]string{"opus": {"Aegis"}})
		if err == nil {
			t.Fatalf("rigSeatNames = %v, want an error", got)
		}
		if !strings.Contains(err.Error(), rigName) {
			t.Errorf("error %q does not name rig %q", err, rigName)
		}
		if got != nil {
			t.Errorf("rigSeatNames returned names %v with the error, want none", got)
		}
	})
}
