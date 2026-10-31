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

// A background sling reports the bd calls it made to the per-run file the
// daemon named, so the daemon can fold them into its census line (gt-3y319).
//
// The town holds the rig, so the sling stops at the hold without spawning a
// session: these tests are about the census the sling writes on its way out.
//
//testpolicy:allow parallel — the test changes the process working directory, which runSpecSling reads
func TestSlingWritesCensusSinkAtExit(t *testing.T) {
	town := censusSlingTown(t)
	sink := filepath.Join(town, "daemon", "sling_census.txt")
	censusSlingFlags(t, `{"bead":"gt-a","rig":"gastown","agent":"claude-sonnet"}`, sink)
	rows := []beads.CensusRow{{Verb: "show", Caller: "cmd.specSlingDuplicateCheck", Count: 7}}
	censusSlingSnapshot(t, rows)

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := runSpecSling(cmd, nil); err != nil {
		t.Fatalf("runSpecSling: %v", err)
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
//
//testpolicy:allow parallel — the test changes the process working directory, which runSpecSling reads
func TestSlingCensusWriteFailureDoesNotFailSling(t *testing.T) {
	town := censusSlingTown(t)
	blocker := filepath.Join(town, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The sink's directory is a file, so creating its parent fails.
	sink := filepath.Join(blocker, "sling_census.txt")
	censusSlingFlags(t, `{"bead":"gt-a","rig":"gastown","agent":"claude-sonnet"}`, sink)
	censusSlingSnapshot(t, []beads.CensusRow{{Verb: "show", Caller: "cmd.specSlingDuplicateCheck", Count: 1}})

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := runSpecSling(cmd, nil); err != nil {
		t.Fatalf("a census write that fails must not fail the sling: %v", err)
	}
	if n := strings.Count(out.String(), "bd census rows not written"); n != 1 {
		t.Errorf("census error lines = %d, want the write failure named once: %s", n, out.String())
	}
}

// censusSlingTown is a town whose operator hold covers every rig, so a sling
// there stops at the hold. The working directory is the town, as it is for the
// child the daemon runs there.
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
	//testpolicy:allow no-chdir — runSpecSling resolves the town root from the working directory, as the daemon-run child does
	t.Chdir(town)
	return town
}

// censusSlingFlags points the sling command at job and sink for one test and
// clears both after it.
func censusSlingFlags(t *testing.T, job, sink string) {
	t.Helper()
	if err := specSlingCmd.Flags().Set("job", job); err != nil {
		t.Fatal(err)
	}
	if err := specSlingCmd.Flags().Set("census-path", sink); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = specSlingCmd.Flags().Set("job", "")
		_ = specSlingCmd.Flags().Set("census-path", "")
	})
}

// censusSlingSnapshot hands the sling rows without a bd subprocess, so the
// sink's contents are known.
func censusSlingSnapshot(t *testing.T, rows []beads.CensusRow) {
	t.Helper()
	saved := specSlingCensus
	//testpolicy:allow no-global-swap — the census a sling writes is a seam a test replaces
	specSlingCensus = func() []beads.CensusRow { return rows }
	t.Cleanup(func() {
		//testpolicy:allow no-global-swap — restores the seam the test replaced
		specSlingCensus = saved
	})
}
