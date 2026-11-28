package daemon

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/steveyegge/gastown/internal/townflight"
)

// drainTown is a town with a daemon directory and a daemon.lock, so
// townflight.Register lists a command in flight and reads the drain marker.
func drainTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "daemon", "daemon.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return town
}

// blockedMarker puts a non-empty directory where the drain marker is written,
// so removing the marker fails and the failure is not a missing file.
func blockedMarker(t *testing.T, town string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(town, "daemon", "draining", "stuck"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// A marker a previous daemon left is removed, and a town with no marker is not
// an error; a marker that cannot be removed is returned, not passed over
// (gt-niaopb).
func TestShutdownDrainClearsTheMarker(t *testing.T) {
	t.Parallel()
	t.Run("removes a marker a previous daemon left", func(t *testing.T) {
		t.Parallel()
		town := drainTown(t)
		if err := townflight.StartDrain(town); err != nil {
			t.Fatalf("StartDrain: %v", err)
		}
		d := &Daemon{config: &Config{TownRoot: town}}
		if err := d.clearDrainMarker(); err != nil {
			t.Fatalf("clearDrainMarker = %v, want nil", err)
		}
		release, err := townflight.Register(town, "gt status")
		if err != nil {
			t.Fatalf("Register after the clear = %v, want the command listed", err)
		}
		if err := release(); err != nil {
			t.Errorf("release = %v", err)
		}
	})
	t.Run("a town with no marker is not an error", func(t *testing.T) {
		t.Parallel()
		d := &Daemon{config: &Config{TownRoot: drainTown(t)}}
		if err := d.clearDrainMarker(); err != nil {
			t.Fatalf("clearDrainMarker with no marker = %v, want nil", err)
		}
	})
	t.Run("returns the error when the marker cannot be removed", func(t *testing.T) {
		t.Parallel()
		town := drainTown(t)
		blockedMarker(t, town)
		d := &Daemon{config: &Config{TownRoot: town}}
		if err := d.clearDrainMarker(); !errors.Is(err, syscall.ENOTEMPTY) {
			t.Fatalf("clearDrainMarker over a blocked marker = %v, want ENOTEMPTY", err)
		}
	})
}

// The start clears a marker left by its predecessor before it dispatches
// anything, so a listed command is accepted once the daemon is up (gt-niaopb).
func TestShutdownDrainRunClearsALeftMarker(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := townflight.StartDrain(town); err != nil {
		t.Fatalf("StartDrain: %v", err)
	}
	_, finish := startParkedRunIn(t, town, nil, unreachableProbe(t))
	defer func() { _ = finish() }()

	release, err := townflight.Register(town, "gt status")
	if err != nil {
		t.Fatalf("Register while the daemon runs = %v, want the command listed", err)
	}
	if err := release(); err != nil {
		t.Errorf("release = %v", err)
	}
}

// A start whose predecessor marker cannot be removed refuses at the lock step:
// the error wraps the removal failure, and the start writes no pid file, so
// nothing is dispatched into a town that refuses its commands (gt-niaopb).
func TestShutdownDrainRunRefusesWhenTheMarkerCannotBeCleared(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	blockedMarker(t, town)
	cfg := DefaultConfig(town)
	d := &Daemon{
		config:     cfg,
		logger:     log.New(io.Discard, "", 0),
		getenv:     ownerEnv(nil),
		hostname:   fixtureHostname("fixture-host"),
		ownerProbe: unreachableProbe(t),
	}
	// A start that got past the marker would run the whole loop; a canceled
	// context ends it at the first select instead of parking the test.
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.cancel()

	err := d.Run()
	if !errors.Is(err, syscall.ENOTEMPTY) {
		t.Fatalf("Run = %v, want the marker removal failure", err)
	}
	if _, err := os.Stat(cfg.PidFile); !os.IsNotExist(err) {
		t.Errorf("Run wrote %s before refusing", cfg.PidFile)
	}
}
