package config

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
)

// runner_idle_command and runner_idle_wait left the schema with the tier
// sweep's idle wait (gt-qpeei.8). The live settings file still carries them, so
// a load accepts both, reads neither, and names each once per process.

const toleratedTierSweepDoc = `{"type":"daemon-patrol-config","version":1,"patrols":{"tier_sweep":{` +
	`"enabled":true,"interval":"30m","runner_idle_command":"gt ci runner-idle","runner_idle_wait":"10m"}}}`

func TestToleratedTierSweepKeysDecode(t *testing.T) {
	t.Parallel()
	t.Run("patrol document", func(t *testing.T) {
		t.Parallel()
		var cfg DaemonPatrolConfig
		if err := DecodeJSONFile("daemon.json", []byte(toleratedTierSweepDoc), &cfg); err != nil {
			t.Fatalf("DecodeJSONFile: %v", err)
		}
		ts := cfg.Patrols.TierSweep
		if ts == nil || !ts.Enabled || ts.IntervalStr != "30m" {
			t.Fatalf("tier_sweep = %+v, want the keys around the tolerated ones read", ts)
		}
	})
	t.Run("town settings section", func(t *testing.T) {
		t.Parallel()
		settings := `{"type":"town-settings","version":1,"daemon":` + toleratedTierSweepDoc + `}`
		var s TownSettings
		if err := DecodeJSONFile("settings/config.json", []byte(settings), &s); err != nil {
			t.Fatalf("DecodeJSONFile: %v", err)
		}
		if s.Daemon == nil || s.Daemon.Patrols == nil || s.Daemon.Patrols.TierSweep == nil ||
			!s.Daemon.Patrols.TierSweep.Enabled {
			t.Fatalf("the daemon section did not load: %+v", s.Daemon)
		}
	})
	t.Run("an unknown key beside a tolerated one is still refused", func(t *testing.T) {
		t.Parallel()
		body := strings.Replace(toleratedTierSweepDoc,
			`"runner_idle_wait":"10m"`, `"runner_idle_wait":"10m","runner_idle_waitt":"10m"`, 1)
		err := DecodeJSONFile("daemon.json", []byte(body), &DaemonPatrolConfig{})
		var pe *ParseError
		if !errors.As(err, &pe) {
			t.Fatalf("DecodeJSONFile = %v, want a ParseError for the unknown key", err)
		}
		if want := []string{"patrols.tier_sweep.runner_idle_waitt"}; !slices.Equal(pe.Keys, want) {
			t.Errorf("Keys = %v, want only the misspelling %v", pe.Keys, want)
		}
	})
}

// The warning is one line per key per process, whichever file names the key
// and wherever in the file it sits.
func TestToleratedTierSweepKeyWarnsOncePerProcess(t *testing.T) {
	// Not parallel: it reads and resets the process-wide record of named keys.
	forgetToleratedKeyWarnings()
	t.Cleanup(forgetToleratedKeyWarnings)

	var first bytes.Buffer
	if err := decodeJSONFile("daemon.json", []byte(toleratedTierSweepDoc), &DaemonPatrolConfig{}, &first); err != nil {
		t.Fatalf("decodeJSONFile: %v", err)
	}
	want := []string{
		"warning: daemon.patrols.tier_sweep.runner_idle_command is no longer read and can be removed",
		"warning: daemon.patrols.tier_sweep.runner_idle_wait is no longer read and can be removed",
	}
	if got := strings.Split(strings.TrimSpace(first.String()), "\n"); !slices.Equal(got, want) {
		t.Fatalf("warnings = %q, want one line per key:\n%s", got, strings.Join(want, "\n"))
	}

	settings := `{"type":"town-settings","version":1,"daemon":` + toleratedTierSweepDoc + `}`
	var second bytes.Buffer
	if err := decodeJSONFile("settings/config.json", []byte(settings), &TownSettings{}, &second); err != nil {
		t.Fatalf("decodeJSONFile for the same keys in another file: %v", err)
	}
	if second.Len() != 0 {
		t.Errorf("a second load named the keys again:\n%s", second.String())
	}
}
