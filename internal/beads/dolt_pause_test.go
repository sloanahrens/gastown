package beads

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/doltpause"
)

// pausedTown is a town whose Dolt pause marker holds for the next hour.
func pausedTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := doltpause.Write(town, doltpause.Marker{
		Actor: "deacon", Reason: "weekly gc", Until: time.Now().Add(time.Hour), Since: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	return town
}

// A bd call that fails while Dolt is paused reports the pause, keeping
// ErrUnavailable and bd's own failure in the chain; a not-found answer is
// still a not-found (gt-8z769.2).
func TestBdFailureWhilePaused_ReportsThePause(t *testing.T) {
	t.Parallel()
	town := pausedTown(t)
	r := newRecorder(func(args []string) reply {
		if args[0] == "show" {
			return reply{err: exitError{code: bdNotFoundExit}}
		}
		return reply{stderr: "dial tcp 127.0.0.1:3307: connection refused", err: exitError{code: 1}}
	})
	b := newBeads(beadsFields{workDir: town, isolated: true, exec: r.exec})

	_, err := b.Run("list", "--json")
	var pe *doltpause.Error
	if !errors.As(err, &pe) || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Run = %v, want *doltpause.Error wrapping ErrUnavailable", err)
	}
	if !strings.HasPrefix(err.Error(), "Dolt paused by deacon until ") || !strings.Contains(pe.Cause.Error(), "connection refused") {
		t.Errorf("err = %q, cause = %v", err, pe.Cause)
	}

	if _, err := b.Run("show", "gt-x"); !errors.Is(err, ErrNotFound) || errors.As(err, &pe) {
		t.Errorf("show = %v, want plain ErrNotFound", err)
	}

	plain := NewPlain(town, nil)
	plain.exec = r.exec
	if _, err := plain.Run("list"); !errors.As(err, &pe) {
		t.Errorf("plain Run = %v, want the pause", err)
	}
}

// Without a marker the failure reads exactly as before.
func TestBdFailureNotPaused_Unchanged(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	r := newRecorder(func([]string) reply {
		return reply{stderr: "connection refused", err: exitError{code: 1}}
	})
	b := newBeads(beadsFields{workDir: town, isolated: true, exec: r.exec})
	_, err := b.Run("list", "--json")
	var pe *doltpause.Error
	if err == nil || errors.As(err, &pe) {
		t.Errorf("Run = %v, want a plain failure", err)
	}
}
