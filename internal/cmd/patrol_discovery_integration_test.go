//go:build integration

package cmd

import (
	"os/exec"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestIntegrationFindActivePatrolReadsBeadsDir is the wiring guard for the
// patrol helpers' default client. The unit tests inject beadsfake; this one
// leaves PatrolConfig.Beads nil, so discovery and burn go through bd in
// BeadsDir against a real Dolt database.
func TestIntegrationFindActivePatrolReadsBeadsDir(t *testing.T) {
	if _, err := exec.LookPath("bd"); err != nil {
		t.Fatalf("bd CLI not on PATH: %v", err)
	}
	dir, b := setupPatrolTestDB(t)

	cfg := PatrolConfig{PatrolMolName: testPatrolMol, BeadsDir: dir, Assignee: testPatrolAssignee}

	// A stale patrol alone: discovery finds nothing and closes it.
	stale := createStalePatrol(t, b)
	if id, _, found, err := findActivePatrol(cfg); err != nil || found {
		t.Fatalf("findActivePatrol with only a stale patrol = %q, found %v, %v; want nothing", id, found, err)
	}
	wantStatus(t, b, stale, "closed")

	// An active patrol is found and left hooked.
	active := createHookedPatrol(t, b, testPatrolMol, testPatrolAssignee, true)
	id, _, found, err := findActivePatrol(cfg)
	if err != nil {
		t.Fatalf("findActivePatrol: %v", err)
	}
	if !found || id != active {
		t.Fatalf("findActivePatrol = %q, found %v; want %q", id, found, active)
	}
	wantStatus(t, b, active, beads.StatusHooked)

	burnPreviousPatrolWisps(cfg)
	wantStatus(t, b, active, "closed")
	kids, err := b.Children(active)
	if err != nil {
		t.Fatalf("children of %s: %v", active, err)
	}
	if len(kids) != 1 || kids[0].Status != "closed" {
		t.Errorf("burned patrol's steps = %+v, want one closed step", kids)
	}
}
