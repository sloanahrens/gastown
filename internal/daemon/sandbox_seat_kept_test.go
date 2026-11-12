package daemon

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/steveyegge/gastown/internal/intent"
)

// keptSeatReason is the keep reason a seat whose push failed gets: the one the
// kept record must carry exactly.
const keptSeatReason = "push_failed=true: the seat's branch never reached origin"

// keptSeatRecordFile is the record of endPolecat, spelled out here rather than
// taken from the code under test.
func keptSeatRecordFile(town string) string {
	return filepath.Join(town, ".runtime", "sandbox-seat-kept", endRig, endPolecat+".json")
}

// keptSeatScenario is a finished sandbox seat with its host directories, whose
// push failed, so the end keeps it. The clock is fixed at now and returned so a
// test can advance it between passes.
func keptSeatScenario(t *testing.T, now time.Time) (*seatEndFake, *clockwork.FakeClock) {
	t.Helper()
	f := newSeatEndFake(t)
	f.seatFields.PushFailed = true
	f.seedAgent()
	f.retire(intent.DesiredStop)
	f.seatDirs()
	clk := clockwork.NewFakeClockAt(now)
	f.host.d.clock = clk
	return f, clk
}

// keptSeatLanding is the landing the seat's end runs for. Submitted is left zero,
// so no jev_rules ask can be pending and only the seat's own facts decide.
func keptSeatLanding() sandboxSeatLanding {
	return sandboxSeatLanding{BeadID: endBead, Branch: endBranch, Landed: true}
}

// The keep for push_failed leaves a record with the seat's bead, the exact keep
// reason, and a since of the pass that kept it (gt-464nw6).
func TestKeptSeatRecordIsWrittenAtTheKeptBranch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
	f, _ := keptSeatScenario(t, now)

	if f.host.endFinishedSandboxSeat(endRig, endPolecat, keptSeatLanding()) {
		t.Fatal("a seat whose push failed was removed")
	}

	seats, bad, err := KeptSandboxSeats(f.town)
	if err != nil {
		t.Fatalf("reading the kept seats: %v", err)
	}
	if len(bad) != 0 {
		t.Fatalf("unreadable kept records = %v", bad)
	}
	if len(seats) != 1 {
		t.Fatalf("kept seats = %+v, want one", seats)
	}
	got := seats[0]
	if got.Rig != endRig || got.Name != endPolecat || got.Bead != endBead || got.Reason != keptSeatReason {
		t.Errorf("kept seat = %+v, want %s/%s bead %s reason %q", got, endRig, endPolecat, endBead, keptSeatReason)
	}
	if !got.Since.Equal(now) {
		t.Errorf("since = %v, want the keep's first pass %v", got.Since, now)
	}
}

// A keep that repeats its reason on a later pass keeps the since it began with,
// so the record ages from the start of the keep. A changed reason starts a new
// since (gt-464nw6). Writing now on every pass would turn the second check red.
func TestKeptSeatRecordKeepsItsSinceWhileTheReasonStands(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
	f, clk := keptSeatScenario(t, start)

	f.host.endFinishedSandboxSeat(endRig, endPolecat, keptSeatLanding())
	clk.Advance(30 * time.Minute)
	f.host.endFinishedSandboxSeat(endRig, endPolecat, keptSeatLanding())

	seats, _, err := KeptSandboxSeats(f.town)
	if err != nil || len(seats) != 1 {
		t.Fatalf("after a repeated keep: seats = %+v, %v; want the one record", seats, err)
	}
	if !seats[0].Since.Equal(start) {
		t.Errorf("since after a repeated keep = %v, want the first pass %v", seats[0].Since, start)
	}

	// The reason changes: the push succeeds, and a hook is left on the seat.
	f.seatFields.PushFailed = false
	f.seatFields.HookBead = "gt-other"
	f.setAgent()
	clk.Advance(15 * time.Minute)
	f.host.endFinishedSandboxSeat(endRig, endPolecat, keptSeatLanding())

	seats, _, err = KeptSandboxSeats(f.town)
	if err != nil || len(seats) != 1 {
		t.Fatalf("after a changed reason: seats = %+v, %v; want the one record", seats, err)
	}
	if seats[0].Reason != "hook_bead=gt-other is still set" {
		t.Errorf("reason = %q, want the hook keep", seats[0].Reason)
	}
	if want := start.Add(45 * time.Minute); !seats[0].Since.Equal(want) {
		t.Errorf("since after a changed reason = %v, want %v", seats[0].Since, want)
	}
}

// A record the keep cannot read back restarts the age from now, and the log says
// why. The keep itself still stands (gt-464nw6).
func TestKeptSeatRecordRestartsTheAgeFromAnUnreadableRecord(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
	f, clk := keptSeatScenario(t, start)
	var logs strings.Builder
	f.host.d.logger = log.New(&logs, "", 0)

	f.host.endFinishedSandboxSeat(endRig, endPolecat, keptSeatLanding())
	if err := os.WriteFile(keptSeatRecordFile(f.town), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	clk.Advance(30 * time.Minute)
	if f.host.endFinishedSandboxSeat(endRig, endPolecat, keptSeatLanding()) {
		t.Fatal("a seat whose push failed was removed")
	}

	seats, bad, err := KeptSandboxSeats(f.town)
	if err != nil || len(bad) != 0 || len(seats) != 1 {
		t.Fatalf("kept seats = %+v, bad %v, %v; want the rewritten record", seats, bad, err)
	}
	if want := start.Add(30 * time.Minute); !seats[0].Since.Equal(want) {
		t.Errorf("since = %v, want the pass that found the record unreadable, %v", seats[0].Since, want)
	}
	if !strings.Contains(logs.String(), endRig+"/"+endPolecat+": its kept record is unreadable") {
		t.Errorf("log %q does not say the kept record was unreadable", logs.String())
	}
}

// Each end that decides the seat is no longer kept leaves no kept record: a
// removal, a seat that is no longer a sandbox seat, and an agent bead that is
// gone (gt-464nw6). Each row keeps the seat first, so there is a record to clear.
func TestKeptSeatRecordGoesWhenTheSeatIsRemoved(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// change is the fact that settles the seat before the second pass.
		change      func(*seatEndFake)
		wantRemoved bool
	}{
		{
			name: "the seat is removed",
			change: func(f *seatEndFake) {
				f.seatFields.PushFailed = false
				f.setAgent()
			},
			wantRemoved: true,
		},
		{
			name: "the seat is no longer a sandbox seat",
			change: func(f *seatEndFake) {
				f.seatFields.Agent = endHostAgent
				f.setAgent()
			},
		},
		{
			name: "the agent bead is gone",
			change: func(f *seatEndFake) {
				if err := f.agentDB.DeleteIssues(f.agentBead); err != nil {
					f.t.Fatal(err)
				}
			},
		},
		{
			// A removal that deleted some of the seat's directories and then failed
			// leaves nothing to wait for, so the record goes with it.
			name: "a removal that fails halfway clears the record",
			change: func(f *seatEndFake) {
				f.seatFields.PushFailed = false
				f.setAgent()
				f.removeErr = errors.New("removing the seat's clone: permission denied")
				f.removeDeleted = true
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, _ := keptSeatScenario(t, time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC))
			f.host.endFinishedSandboxSeat(endRig, endPolecat, keptSeatLanding())
			if seats, _, _ := KeptSandboxSeats(f.town); len(seats) != 1 {
				t.Fatalf("the first pass kept %+v, want the seat recorded", seats)
			}

			tt.change(f)
			if got := f.host.endFinishedSandboxSeat(endRig, endPolecat, keptSeatLanding()); got != tt.wantRemoved {
				t.Errorf("second pass removed = %v, want %v", got, tt.wantRemoved)
			}

			seats, bad, err := KeptSandboxSeats(f.town)
			if err != nil || len(bad) != 0 {
				t.Fatalf("reading the kept seats: bad %v, %v", bad, err)
			}
			if len(seats) != 0 {
				t.Errorf("kept seats after the seat was settled = %+v, want none", seats)
			}
		})
	}
}

// A seat kept for the jev_rules ask leaves no non-rules record behind: its keep
// reason is now the rules ask, which has its own record, so an older push_failed
// record would name a reason the seat no longer has.
func TestKeptSeatRecordGoesWhenTheKeepBecomesTheRulesAsk(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
	f, _ := keptSeatScenario(t, now)
	f.host.endFinishedSandboxSeat(endRig, endPolecat, keptSeatLanding())
	if seats, _, _ := KeptSandboxSeats(f.town); len(seats) != 1 {
		t.Fatalf("the first pass kept %+v, want the seat recorded", seats)
	}

	f.seatFields.PushFailed = false
	f.setAgent()
	f.host.d.patrolConfig = jevRulesOnly()
	f.cursorAt = now.Add(-11 * time.Minute)
	landing := keptSeatLanding()
	landing.Submitted = now.Add(-10 * time.Minute)
	if f.host.endFinishedSandboxSeat(endRig, endPolecat, landing) {
		t.Fatal("a seat whose rules ask is outstanding was removed")
	}

	seats, _, err := KeptSandboxSeats(f.town)
	if err != nil || len(seats) != 0 {
		t.Errorf("kept seats = %+v, %v; want none, the rules keep has its own record", seats, err)
	}
}

// A keep whose record cannot be written is logged with the seat's name, and the
// seat is still kept (gt-464nw6). A regular file where the store's directory
// should be makes the write fail for a reason the test can see.
func TestKeptSeatRecordLogsAFailedWrite(t *testing.T) {
	t.Parallel()
	f, _ := keptSeatScenario(t, time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC))
	var logs strings.Builder
	f.host.d.logger = log.New(&logs, "", 0)
	runtimeDir := filepath.Join(f.town, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "sandbox-seat-kept"), []byte("in the way"), 0o644); err != nil {
		t.Fatal(err)
	}

	if f.host.endFinishedSandboxSeat(endRig, endPolecat, keptSeatLanding()) {
		t.Fatal("a seat whose push failed was removed when its record could not be written")
	}
	if !dirExists(t, filepath.Join(f.town, endRig, "polecats", endPolecat)) {
		t.Error("a kept seat lost its host directory")
	}
	if !strings.Contains(logs.String(), "sandbox seat end "+endRig+"/"+endPolecat+": cannot record the keep:") {
		t.Errorf("log %q does not name the seat and the failed write", logs.String())
	}
	if !strings.Contains(logs.String(), "kept: "+keptSeatReason) {
		t.Errorf("log %q does not still say the seat was kept", logs.String())
	}
}

// A record that cannot be removed is logged with the seat's name, and the seat
// is still removed: the removal decision does not wait on the record. The record
// is replaced by a non-empty directory, which os.Remove refuses.
func TestKeptSeatRecordLogsAFailedRemoval(t *testing.T) {
	t.Parallel()
	f, _ := keptSeatScenario(t, time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC))
	var logs strings.Builder
	f.host.d.logger = log.New(&logs, "", 0)
	f.host.endFinishedSandboxSeat(endRig, endPolecat, keptSeatLanding())

	record := keptSeatRecordFile(f.town)
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(record, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(record, "held"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.seatFields.PushFailed = false
	f.setAgent()

	if !f.host.endFinishedSandboxSeat(endRig, endPolecat, keptSeatLanding()) {
		t.Fatal("a finished seat was kept because its kept record could not be removed")
	}
	if dirExists(t, filepath.Join(f.town, endRig, "polecats", endPolecat)) {
		t.Error("a finished seat kept its host directory")
	}
	if !strings.Contains(logs.String(), "sandbox seat end "+endRig+"/"+endPolecat+": cannot remove its kept record:") {
		t.Errorf("log %q does not name the seat and the failed removal", logs.String())
	}
}

// The reader returns every readable kept seat and reports a record it cannot
// read by path. A temp file of an atomic write is not a record and is not
// reported (gt-464nw6).
func TestKeptSeatRecordReaderReportsAnUnreadableFile(t *testing.T) {
	t.Parallel()
	f, _ := keptSeatScenario(t, time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC))
	f.host.endFinishedSandboxSeat(endRig, endPolecat, keptSeatLanding())

	dir := filepath.Join(f.town, ".runtime", "sandbox-seat-kept", endRig)
	corrupt := filepath.Join(dir, "agile.json")
	if err := os.WriteFile(corrupt, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(dir, endPolecat+".json.tmp.123")
	if err := os.WriteFile(temp, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory named like a record is not a record, and is reported.
	heldDir := filepath.Join(dir, "held.json")
	if err := os.MkdirAll(heldDir, 0o755); err != nil {
		t.Fatal(err)
	}

	seats, bad, err := KeptSandboxSeats(f.town)
	if err != nil {
		t.Fatalf("reading the kept seats: %v", err)
	}
	if len(seats) != 1 || seats[0].Name != endPolecat {
		t.Errorf("kept seats = %+v, want the readable %s", seats, endPolecat)
	}
	if want := []string{corrupt, heldDir}; !slices.Equal(bad, want) {
		t.Errorf("unreadable records = %v, want %v", bad, want)
	}
}

// A store that cannot be listed is an error, so a caller decides nothing on it.
func TestKeptSeatRecordReaderFailsOnAStoreThatCannotBeListed(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, ".runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A file where the store's directory should be: it exists, and a listing of it fails.
	if err := os.WriteFile(filepath.Join(town, ".runtime", "sandbox-seat-kept"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := KeptSandboxSeats(town); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("KeptSandboxSeats error = %v, want the listing error", err)
	}
}

// A seat name a path cannot hold is refused before anything is written or
// removed: both helpers return the name error, and the store is never created
// (gt-464nw6).
func TestKeptSeatRecordRefusesAnUnsafeSeatName(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	rec := keptSandboxSeatRecord{Bead: endBead, Reason: keptSeatReason, Since: time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)}
	if err := writeKeptSandboxSeat(town, endRig, "../escape", rec); err == nil {
		t.Error("a record was written under an unsafe seat name")
	}
	if err := removeKeptSandboxSeat(town, endRig, "../escape"); err == nil {
		t.Error("a record was removed under an unsafe seat name")
	}
	if dirExists(t, filepath.Join(town, ".runtime", "sandbox-seat-kept")) {
		t.Error("the kept store was created for an unsafe seat name")
	}
}
