//go:build darwin || linux

package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeAgedReservation(t *testing.T, townRoot string, reservation polecatAdmissionReservation) string {
	t.Helper()
	dir := polecatAdmissionDir(townRoot)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir reservations: %v", err)
	}
	data, err := json.Marshal(reservation)
	if err != nil {
		t.Fatalf("marshal reservation: %v", err)
	}
	path := filepath.Join(dir, reservation.ID+".json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write reservation: %v", err)
	}
	return path
}

func TestCleanupStaleReservationsKeepsLongLivedHolderWithMatchingStart(t *testing.T) {
	t.Parallel()
	townRoot := setupPolecatCapacityTestTown(t, 1)
	now := time.Now()
	path := writeAgedReservation(t, townRoot, polecatAdmissionReservation{
		ID:        "long",
		PID:       os.Getpid(),
		PIDStart:  processStart(os.Getpid()),
		Rig:       "gastown",
		Bead:      "gt-long",
		Operation: "test",
		CreatedAt: now.Add(-5 * polecatAdmissionReservationTTL),
	})

	if err := cleanupStalePolecatAdmissionReservations(townRoot, now); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("reservation held by the same live process was dropped: %v", err)
	}
}

func TestCleanupStaleReservationsRemovesReusedPIDWithDifferentStart(t *testing.T) {
	t.Parallel()
	townRoot := setupPolecatCapacityTestTown(t, 1)
	now := time.Now()
	path := writeAgedReservation(t, townRoot, polecatAdmissionReservation{
		ID:        "recycled",
		PID:       os.Getpid(),
		PIDStart:  "not-this-process",
		Rig:       "gastown",
		Bead:      "gt-recycled",
		Operation: "test",
		CreatedAt: now.Add(-polecatAdmissionReservationTTL - time.Minute),
	})

	if err := cleanupStalePolecatAdmissionReservations(townRoot, now); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("reservation whose PID was reused still exists: %v", err)
	}
}

func TestWritePolecatAdmissionReservationRecordsHolderStart(t *testing.T) {
	t.Parallel()
	townRoot := setupPolecatCapacityTestTown(t, 1)
	reservation, path, err := writePolecatAdmissionReservation(townRoot, "gastown", "gt-x", "test")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	defer func() { _ = os.Remove(path) }()
	if want := processStart(os.Getpid()); reservation.PIDStart == "" || reservation.PIDStart != want {
		t.Fatalf("PIDStart = %q, want %q", reservation.PIDStart, want)
	}
}

func TestProcessStartIsStableAndPerProcess(t *testing.T) {
	t.Parallel()
	first := processStart(os.Getpid())
	if first == "" {
		t.Fatal("processStart(self) is empty")
	}
	if again := processStart(os.Getpid()); again != first {
		t.Fatalf("processStart(self) changed between reads: %q then %q", first, again)
	}
	if got := processStart(99999999); got != "" {
		t.Fatalf("processStart(no such pid) = %q, want empty", got)
	}
	if got := processStart(0); got != "" {
		t.Fatalf("processStart(0) = %q, want empty", got)
	}
}
