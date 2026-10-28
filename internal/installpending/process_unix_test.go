//go:build !windows

package installpending

import "testing"

// pidAboveAnyProcess is larger than any kernel's pid_max, so the probe cannot
// find a process behind it and the case stays a dead writer on every host.
const pidAboveAnyProcess = 1 << 30

// TestPending_DeadWriterIsNotPending pins the pid rule through Pending and the
// real probe: a daemon that died mid-cycle left its marker behind, and the
// town must not stay out of slings for it.
func TestPending_DeadWriterIsNotPending(t *testing.T) {
	t.Parallel()
	now := testNow()
	town := t.TempDir()
	if err := Write(town, now, pidAboveAnyProcess); err != nil {
		t.Fatalf("writing the marker: %v", err)
	}

	m, err := Pending(town, now)
	if err != nil {
		t.Fatalf("reading the marker: %v", err)
	}
	if m != nil {
		t.Fatalf("a marker whose writer is gone reads as pending: %+v", m)
	}
}
