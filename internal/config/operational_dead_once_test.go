package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The loader runs on every gt command and every daemon tick, so a dead-key
// warning that fires on every load would put the same lines on the stderr of
// every command and into the daemon log forever. These tests pin the
// once-per-(file, key) rule (gt-x2w2g).

// TestDeadOperationalWarningsOncePerProcess: loading the same settings file
// twice in one process warns for its dead key on the first load only.
func TestDeadOperationalWarningsOncePerProcess(t *testing.T) {
	t.Parallel()

	path := writeDeadKeySettings(t, `{"type":"town-settings","version":1,`+
		`"operational":{"dolt":{"max_connections":1000}}}`)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// warnDeadOperationalKeys prints exactly the lines newDeadOperationalWarnings
	// returns, so a call here is what the next load would put on stderr.
	first := newDeadOperationalWarnings(path, data)
	if len(first) != 1 || !strings.Contains(first[0], "max_connections") {
		t.Fatalf("first load warns %q, want one naming max_connections", first)
	}
	if second := newDeadOperationalWarnings(path, data); len(second) != 0 {
		t.Errorf("second load warns %q, want nothing", second)
	}
}

// TestDeadOperationalWarningsPerFileAndKey: remembering a key silences it in
// that file only — another file setting the same key, and a second dead key in
// the same file, each still warn.
func TestDeadOperationalWarningsPerFileAndKey(t *testing.T) {
	t.Parallel()

	sameKey := `{"type":"town-settings","version":1,` +
		`"operational":{"dolt":{"max_connections":1000}}}`
	one := writeDeadKeySettings(t, sameKey)
	two := writeDeadKeySettings(t, sameKey)
	if got := newDeadOperationalWarnings(one, []byte(sameKey)); len(got) != 1 {
		t.Fatalf("first file warns %q, want one", got)
	}
	if got := newDeadOperationalWarnings(two, []byte(sameKey)); len(got) != 1 {
		t.Errorf("second file setting the same key warns %q, want one", got)
	}

	both := `{"type":"town-settings","version":1,` +
		`"operational":{"dolt":{"max_connections":1000,"cmd_timeout":"5s"}}}`
	if got := newDeadOperationalWarnings(writeDeadKeySettings(t, both), []byte(both)); len(got) != 2 {
		t.Errorf("two dead keys in one file warn %q, want two", got)
	}
}

// TestLoadOrCreateTownSettingsWarnsOncePerProcess: the loader itself records
// what it printed, so the second load is quiet and the file still loads — the
// warning was never an error.
func TestLoadOrCreateTownSettingsWarnsOncePerProcess(t *testing.T) {
	t.Parallel()

	path := writeDeadKeySettings(t, `{"type":"town-settings","version":1,`+
		`"operational":{"dolt":{"max_connections":1000,"commits_per_day_warn":800}}}`)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for load := 1; load <= 2; load++ {
		loaded, err := LoadOrCreateTownSettings(path)
		if err != nil {
			t.Fatalf("load %d: %v", load, err)
		}
		if got := loaded.Operational.GetDoltConfig().CommitsPerDayWarnV(); got != 800 {
			t.Errorf("load %d: commits_per_day_warn = %d, want the live 800 beside the dead key", load, got)
		}
	}
	if got := newDeadOperationalWarnings(path, data); len(got) != 0 {
		t.Errorf("both loads recorded the key, but it still reads as unwarned: %q", got)
	}
}

// writeDeadKeySettings writes settings JSON to a fresh temp path and returns
// it, so each test's (file, key) entries cannot collide in the process-wide
// warned set.
func writeDeadKeySettings(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
