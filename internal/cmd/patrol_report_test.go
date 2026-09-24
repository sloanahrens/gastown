package cmd

import (
	"io"
	"testing"
)

func TestRunPatrolReportForRejectsNonPatrolRole(t *testing.T) {
	err := runPatrolReportFor(io.Discard, RoleInfo{Role: RoleCrew, Rig: "gastown", TownRoot: t.TempDir()}, "x", "", true)
	if err == nil {
		t.Fatal("runPatrolReportFor accepted a crew role; only deacon, witness and refinery patrol")
	}
}
