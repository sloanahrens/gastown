//go:build integration

package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/supervisor"
)

// TestRunDoneLeavesSourceBeadOpenAndReadableByItsConsumers pins what gt done
// leaves behind and what the other packages make of it. Once gt done submits,
// the polecat's session is gone but its hook still holds the source bead,
// which stays open until the landing worker closes it. Every crash detector
// reads that as a dead polecat with work, so each of them has to recognise
// the bead, the intent record, or both, as submitted. Their own tests build
// those inputs by hand; this one feeds them what a real gt done wrote, so a
// change to the writer that a reader would not recognise fails here.
func TestIntegrationRunDoneLeavesSourceBeadOpenAndReadableByItsConsumers(t *testing.T) {
	gate := passingDoneGate()
	r := runDoneSubmit(t, gate, func(t *testing.T, workDir string) {
		setupRoutedSubmitGitRepo(t, workDir, false)
	})
	if r.err != nil {
		t.Fatalf("runDone: %v", r.err)
	}
	head := gitOut(t, r.workDir, "rev-parse", "HEAD")
	townRoot := routedSourceTestTownRoot(r.workDir)

	// gt done never closes the source bead. Closed means landed, and the
	// landing worker is the only thing that says so.
	for _, line := range strings.Split(r.bdLog, "\n") {
		if strings.Contains(line, "\tclose ") && strings.Contains(line, "bd-source") {
			t.Fatalf("gt done closed the source bead:\n%s", r.bdLog)
		}
	}

	// The bead as the owner database reports it after the label write. The
	// stub keeps labels but not status, and gt sling leaves the bead hooked.
	issue := showSourceBead(t, filepath.Join(townRoot, "beads", "mayor", "rig", ".beads"))
	issue.Status = string(beads.StatusHooked)

	t.Run("polecat states", func(t *testing.T) {
		if !polecat.IsSubmittedWork(issue) {
			t.Errorf("polecat.IsSubmittedWork(%+v) = false; the polecat would read as stalled", issue)
		}
		ev := assessPolecatAssignedIssueWork(issue)
		if !ev.BlocksCleanup {
			t.Errorf("a submitted bead does not hold its polecat's seat: %+v", ev)
		}
		if !ev.Submitted {
			t.Errorf("polecat inventory does not read the bead as submitted: %+v", ev)
		}
	})

	t.Run("landing request", func(t *testing.T) {
		want := land.Work{Branch: doneTestBranch, Head: head, Target: "main", Worker: "refuge"}
		got, ok := land.ParseReadyNote(appendedNotes(t, r.bdLog))
		if !ok {
			t.Fatalf("land cannot parse the READY TO LAND note gt done wrote:\n%s", r.bdLog)
		}
		if got.Branch != want.Branch || got.Head != want.Head || got.Target != want.Target || got.Worker != want.Worker {
			t.Errorf("landing request = %+v, want %+v", got, want)
		}
		if got.Submitted.IsZero() {
			t.Error("the note carries no submission time; the landing worker orders the queue by it (gt-t2jhf)")
		}
	})

	t.Run("supervisor refuses a restart", func(t *testing.T) {
		restarted := false
		s := supervisor.New(supervisor.Options{
			TownRoot: townRoot,
			Restart:  func(supervisor.Seat) error { restarted = true; return nil },
			Logf:     t.Logf,
		})
		seat := supervisor.SeatFor("gastown", "polecat", "refuge")
		err := s.Restart(seat, "dead agent", "witness")
		if !errors.Is(err, supervisor.ErrSubmitted) {
			t.Fatalf("Restart after gt done = %v, want ErrSubmitted", err)
		}
		if !strings.Contains(err.Error(), "bd-source") {
			t.Errorf("refusal %q does not name the submitted bead", err)
		}
		if restarted {
			t.Error("a session was raised on submitted work")
		}
	})
}

// showSourceBead runs the installed bd stub's show for bd-source against
// beadsDir and decodes the answer.
func showSourceBead(t *testing.T, beadsDir string) *beads.Issue {
	t.Helper()
	issue, err := beads.NewPlain("", append(os.Environ(), "BEADS_DIR="+beadsDir)).Show("bd-source")
	if err != nil {
		t.Fatalf("bd show bd-source: %v", err)
	}
	return issue
}

// bdLogCalls splits the bd recorder's log into one entry per call. The log is
// one tab-separated line per call, so a multi-line argument runs until the next
// line that has a tab in it.
func bdLogCalls(bdLog string) []string {
	var calls []string
	for _, line := range strings.Split(strings.TrimRight(bdLog, "\n"), "\n") {
		if strings.Contains(line, "\t") || len(calls) == 0 {
			calls = append(calls, line)
			continue
		}
		calls[len(calls)-1] += "\n" + line
	}
	return calls
}

// readyToLandUpdate returns the one bd update that marks bd-source ready to
// land. gt done writes the READY TO LAND note and the gt:ready-to-land label in
// that single call (gt-uhdbi), so a note without the label, or the label in its
// own call, leaves a window where the landing worker sees half a submission.
func readyToLandUpdate(t *testing.T, bdLog string) string {
	t.Helper()
	var updates []string
	for _, call := range bdLogCalls(bdLog) {
		_, args, _ := strings.Cut(call, "\t")
		if !strings.HasPrefix(args, "update bd-source ") {
			continue
		}
		if strings.Contains(args, "--append-notes=READY TO LAND") || strings.Contains(args, "--add-label=gt:ready-to-land") {
			updates = append(updates, args)
		}
	}
	if len(updates) != 1 {
		t.Fatalf("want exactly one bd update carrying the READY TO LAND note and the ready label, got %d:\n%s", len(updates), bdLog)
	}
	for _, want := range []string{"--append-notes=READY TO LAND", "--add-label=gt:ready-to-land"} {
		if !strings.Contains(updates[0], want) {
			t.Fatalf("the ready-to-land update lacks %q, so note and label are not written together:\n%s", want, updates[0])
		}
	}
	return updates[0]
}

// appendedNotes returns the text gt done appended to the source bead's notes,
// as the bd recorder logged it. The note is the --append-notes= value, which
// runs to the label flags that follow its last line.
func appendedNotes(t *testing.T, bdLog string) string {
	t.Helper()
	_, note, _ := strings.Cut(readyToLandUpdate(t, bdLog), "--append-notes=")
	note, _, _ = strings.Cut(note, " --add-label=")
	return note + "\n"
}
