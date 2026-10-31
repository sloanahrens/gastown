package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dispatch"
)

// censusSlingJob is the job one sling reports, as the tick printed it.
const censusSlingJob = `{"bead":"gt-a","rig":"gastown","agent":"claude-sonnet"}`

// A background sling reports the bd calls it made to the per-run file the
// daemon named, so the daemon can fold them into its census line (gt-3y319).
//
// The town holds the rig, so the sling stops at the hold without spawning a
// session: the test is about the census the sling writes on its way out.
func TestSlingWritesCensusSinkAtExit(t *testing.T) {
	t.Parallel()
	town := censusSlingTown(t)
	sink := filepath.Join(town, "daemon", "sling_census.txt")
	rows := []beads.CensusRow{{Verb: "show", Caller: "cmd.specSlingDuplicateCheck", Count: 7}}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := runSpecSlingInTown(cmd, town, censusSlingJob, sink, func() []beads.CensusRow { return rows }); err != nil {
		t.Fatalf("runSpecSlingInTown: %v", err)
	}

	got, err := beads.ReadCensusSink(sink)
	if err != nil {
		t.Fatalf("the sling wrote no census to %s: %v", sink, err)
	}
	if len(got) != 1 || got[0] != rows[0] {
		t.Errorf("census = %+v, want %+v", got, rows)
	}
}

// A census the sling cannot write costs the census and nothing else: the sling
// still reports its outcome, and the failure is named once (gt-3y319).
func TestSlingCensusWriteFailureDoesNotFailSling(t *testing.T) {
	t.Parallel()
	town := censusSlingTown(t)
	blocker := filepath.Join(town, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The sink's directory is a file, so creating its parent fails.
	sink := filepath.Join(blocker, "sling_census.txt")

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	rows := func() []beads.CensusRow {
		return []beads.CensusRow{{Verb: "show", Caller: "cmd.specSlingDuplicateCheck", Count: 1}}
	}
	if err := runSpecSlingInTown(cmd, town, censusSlingJob, sink, rows); err != nil {
		t.Fatalf("a census write that fails must not fail the sling: %v", err)
	}
	if n := strings.Count(out.String(), "bd census rows not written"); n != 1 {
		t.Errorf("census error lines = %d, want the write failure named once: %s", n, out.String())
	}
}

// censusSlingTown is a town whose operator hold covers every rig, so a sling
// there stops at the hold.
func censusSlingTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "town.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dispatch.HoldFilePath(town), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return town
}
