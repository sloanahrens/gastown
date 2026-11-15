package session

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/sandbox"
)

// seatPeakArgv is the peak read a test's recorder drives through the exec seam:
// the shape internal/resourcesampler prints, with a peak file's name.
var seatPeakArgv = []string{"sh", "-c", "printf '@@ cpu.stat @@\\n'; cat /sys/fs/cgroup/cpu.stat"}

// seatPeakRead is a recorder that runs the seat's read through the seam the
// backend hands it, the way the linked recorder does, and returns the read's
// error: the row is only built after it.
func seatPeakRead(calls *[]string) func(ctx context.Context, townRoot string, exec SeatPeakExec, rec SandboxRecord, endedAt time.Time) error {
	return func(ctx context.Context, townRoot string, exec SeatPeakExec, rec SandboxRecord, endedAt time.Time) error {
		if calls != nil {
			*calls = append(*calls, "recorder")
		}
		_, _, err := exec.Exec(ctx, rec.Session, nil, seatPeakArgv, nil)
		return err
	}
}

// deleteCallIndex is the index of the sandbox delete in the fake's calls, or -1.
func deleteCallIndex(calls [][]string) int {
	for i, argv := range calls {
		if len(argv) > 2 && argv[0] == "openshell" && argv[1] == "sandbox" && argv[2] == "delete" {
			return i
		}
	}
	return -1
}

// containsCall reports whether calls holds want.
func containsCall(calls [][]string, want []string) bool {
	for _, argv := range calls {
		if reflect.DeepEqual(argv, want) {
			return true
		}
	}
	return false
}

// warningSays reports whether any warning carries text.
func warningSays(warnings []string, text string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, text) {
			return true
		}
	}
	return false
}

// seatPeakWarnings collects the backend's warnings and is refutable after the
// call under test.
func seatPeakWarnings(b *openshellBackend) *[]string {
	warnings := &[]string{}
	b.warnf = func(format string, args ...any) { *warnings = append(*warnings, fmt.Sprintf(format, args...)) }
	return warnings
}

// TestSeatPeakReadRunsBeforeTheSeatSandboxIsDeleted pins the read's place in
// the seat's end: the recorder is handed the record, the town root, the exec
// seam and the time the read was taken, and the sandbox is deleted straight
// after it, since the peak is gone with the sandbox (gt-wi50nw).
func TestSeatPeakReadRunsBeforeTheSeatSandboxIsDeleted(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{Reply: map[string]string{"exec": "peak text\n"}}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	writeOpsRecord(t, b)
	endedAt := time.Date(2026, 11, 15, 8, 10, 0, 0, time.UTC)
	b.now = func() time.Time { return endedAt }

	var gotRec SandboxRecord
	var gotText, gotTown string
	var gotEndedAt time.Time
	var beforeRecorder [][]string
	b.seatPeak = func(ctx context.Context, townRoot string, exec SeatPeakExec, rec SandboxRecord, when time.Time) error {
		gotRec, gotTown, gotEndedAt = rec, townRoot, when
		stdout, _, err := exec.Exec(ctx, rec.Session, nil, seatPeakArgv, nil)
		if err != nil {
			return err
		}
		gotText = string(stdout)
		beforeRecorder = fake.Calls()
		return nil
	}

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if gotRec.Session != openshellTestSession || gotRec.Rig != openshellTestRig || gotRec.Polecat != openshellTestPolecat {
		t.Errorf("the recorder read the record of %+v, want the seat's own", gotRec)
	}
	if gotTown != b.townRoot {
		t.Errorf("the recorder was handed town root %q, want %q", gotTown, b.townRoot)
	}
	if !gotEndedAt.Equal(endedAt) {
		t.Errorf("ended_at = %s, want the time the read was taken, %s", gotEndedAt, endedAt)
	}
	if gotText != "peak text\n" {
		t.Errorf("the recorder read %q, want what the seat's read printed", gotText)
	}
	// The seam is the backend's CLI, and it addresses the seat's own sandbox.
	if want := sandboxExec(seatPeakArgv...); !containsCall(beforeRecorder, want) {
		t.Errorf("the exec seam ran %v, want %v", beforeRecorder, want)
	}
	// The delete is the very next call: a read after it would address a sandbox
	// the gateway has already removed.
	if got, want := deleteCallIndex(fake.Calls()), len(beforeRecorder); got != want {
		t.Errorf("the sandbox delete is call %d, want call %d, straight after the peak read: %v", got, want, fake.Calls())
	}
}

// TestSeatPeakFailedReadWritesNoRowAndTheSeatEndCompletes is the seat end's
// best effort: a read the gateway refuses writes no row, warns, and the
// sandbox is still deleted, so a lost peak never keeps a seat alive (gt-wi50nw).
func TestSeatPeakFailedReadWritesNoRowAndTheSeatEndCompletes(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{
		Err:    map[string]error{"exec": errors.New("exit status 1")},
		Stderr: map[string]string{"exec": "exec failed: gateway unreachable"},
	}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	writeOpsRecord(t, b)
	warnings := seatPeakWarnings(b)
	var recorders int
	read := seatPeakRead(nil)
	b.seatPeak = func(ctx context.Context, townRoot string, exec SeatPeakExec, rec SandboxRecord, endedAt time.Time) error {
		recorders++
		return read(ctx, townRoot, exec, rec, endedAt)
	}

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete of a seat whose peak read failed = %v, want the seat's end to complete", err)
	}
	if recorders != 1 {
		t.Fatalf("the recorder ran %d times, want the read attempted once", recorders)
	}
	if got := deleteCallIndex(fake.Calls()); got < 0 {
		t.Fatalf("the sandbox was not deleted: %v", fake.Calls())
	}
	if !warningSays(*warnings, "peak row is not written") {
		t.Errorf("warnings = %v, want one naming the peak row that is not written", *warnings)
	}
}

// TestSeatPeakHungReadWritesNoRowAndTheSeatEndCompletes pins the bound: a read
// that never answers is cut by the seat end's short timeout, and the sandbox
// is deleted anyway.
func TestSeatPeakHungReadWritesNoRowAndTheSeatEndCompletes(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	runner := fake.Runner()
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	writeOpsRecord(t, b)
	warnings := seatPeakWarnings(b)
	var readStarted, readDeadline time.Time
	b.cli = &sandbox.CLI{Gateway: b.profile.Gateway, Run: func(ctx context.Context, argv []string, stdin []byte, env []string) ([]byte, []byte, error) {
		// The peak read is the exec whose command is the recorder's argv; the
		// transcript copy's is a find.
		if len(argv) == 0 || argv[len(argv)-1] != seatPeakArgv[len(seatPeakArgv)-1] {
			return runner(ctx, argv, stdin, env)
		}
		// It answers only when the seat end's bound releases it, as a wedged
		// gateway call does; a read given no bound says so instead of holding
		// the seat's end open.
		readStarted = time.Now()
		var ok bool
		if readDeadline, ok = ctx.Deadline(); !ok {
			return nil, nil, errors.New("the peak read was run with no bound")
		}
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}}
	b.seatPeak = seatPeakRead(nil)
	b.seatPeakTimeout = time.Nanosecond

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete of a seat whose peak read hung = %v, want the seat's end to complete", err)
	}
	if readDeadline.IsZero() {
		t.Fatal("the peak read ran with no bound, want the seat end's short timeout")
	}
	if bound := readDeadline.Sub(readStarted); bound > time.Second {
		t.Errorf("the peak read was given %s, want the seat end's short bound", bound)
	}
	if got := deleteCallIndex(fake.Calls()); got < 0 {
		t.Fatalf("the sandbox was not deleted: %v", fake.Calls())
	}
	if !warningSays(*warnings, "peak row is not written") {
		t.Errorf("warnings = %v, want one naming the peak row that is not written", *warnings)
	}
	if want := "did not answer within " + time.Nanosecond.String(); !warningSays(*warnings, want) {
		t.Errorf("warnings = %v, want the cut named with the bound it was given", *warnings)
	}
}

// TestSeatPeakProductionBoundIsTheDefault pins what a binary with no test value
// does: the recorder is handed a context bounded by seatPeakTimeout, so the
// production default is exercised and not only a value a test sets (gt-b3yb24).
func TestSeatPeakProductionBoundIsTheDefault(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	writeOpsRecord(t, b)
	// The baseline is read before the call under test, so the bound measured
	// from it holds the default plus whatever the end did before the read; the
	// band below still tells the production default from a test's own value.
	start := time.Now()
	var bound time.Duration
	var bounded bool
	b.seatPeak = func(ctx context.Context, _ string, _ SeatPeakExec, _ SandboxRecord, _ time.Time) error {
		deadline, ok := ctx.Deadline()
		bounded = ok
		if ok {
			bound = deadline.Sub(start)
		}
		return nil
	}

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !bounded {
		t.Fatal("the read ran with no bound, want the production default")
	}
	if bound <= seatPeakTimeout/2 || bound >= 2*seatPeakTimeout {
		t.Errorf("the read was bounded at %s, want the production default %s", bound, seatPeakTimeout)
	}
}

// TestSeatPeakReadLostToAFailedDeleteIsNotWarned pins where a lost row is
// reported: a delete that fails keeps the record and the sandbox, so the next
// attempt reads the peak again, and the seat's end must not also say the row is
// lost. Warning there logged a peak warning on every seat delete whose gateway
// was not live (gt-b3yb24).
func TestSeatPeakReadLostToAFailedDeleteIsNotWarned(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{
		Err:    map[string]error{"delete": errors.New("exit status 1"), "exec": errors.New("exit status 1")},
		Stderr: map[string]string{"delete": "exec failed: gateway unreachable"},
	}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	writeOpsRecord(t, b)
	warnings := seatPeakWarnings(b)
	var recorders int
	read := seatPeakRead(nil)
	b.seatPeak = func(ctx context.Context, townRoot string, exec SeatPeakExec, rec SandboxRecord, endedAt time.Time) error {
		recorders++
		return read(ctx, townRoot, exec, rec, endedAt)
	}

	if err := b.Delete(openshellTestSession); err == nil {
		t.Fatal("Delete = nil, want the gateway's delete failure")
	}
	if recorders != 1 {
		t.Fatalf("the recorder ran %d times, want the read attempted once", recorders)
	}
	if warningSays(*warnings, "peak row is not written") {
		t.Errorf("warnings = %v, want no peak warning for a delete that did not complete", *warnings)
	}
}

// TestSeatPeakRefusedRowKeepsTheSeatEnd pins the recorder's own failure: a row
// the builder refuses, or one that cannot be appended, is a warning and never
// the seat's end failing.
func TestSeatPeakRefusedRowKeepsTheSeatEnd(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	writeOpsRecord(t, b)
	warnings := seatPeakWarnings(b)
	b.seatPeak = func(context.Context, string, SeatPeakExec, SandboxRecord, time.Time) error {
		return errors.New("sandbox peak: seat gt-gastown-sandbox-ace: prints no memory.peak")
	}

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete of a seat whose peak row was refused = %v, want the seat's end to complete", err)
	}
	if got := deleteCallIndex(fake.Calls()); got < 0 {
		t.Fatalf("the sandbox was not deleted: %v", fake.Calls())
	}
	if !warningSays(*warnings, "peak row is not written") {
		t.Errorf("warnings = %v, want one naming the peak row that is not written", *warnings)
	}
}

// TestSeatPeakNoRecorderKeepsTheSeatEnd pins the unlinked recorder: with no
// recorder in the binary the read has nowhere to put its row, so it is not run
// and the seat's end warns rather than inventing a row.
func TestSeatPeakNoRecorderKeepsTheSeatEnd(t *testing.T) {
	t.Parallel()
	fake := &sandbox.Fake{}
	b, _ := opsBackend(t, fake, newSandboxProbe(t, true))
	writeOpsRecord(t, b)
	warnings := seatPeakWarnings(b)
	b.seatPeak = nil
	if SeatPeakRecorder != nil {
		t.Fatal("SeatPeakRecorder is linked in this test binary, so the unlinked branch cannot be tested here")
	}

	if err := b.Delete(openshellTestSession); err != nil {
		t.Fatalf("Delete with no peak recorder = %v, want the seat's end to complete", err)
	}
	if got := deleteCallIndex(fake.Calls()); got < 0 {
		t.Fatalf("the sandbox was not deleted: %v", fake.Calls())
	}
	if !warningSays(*warnings, "no seat peak recorder is linked") {
		t.Errorf("warnings = %v, want one naming the missing recorder", *warnings)
	}
}
