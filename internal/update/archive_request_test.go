package update

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

var (
	archiveRequestFile = filepath.Join(maintTestDir, ArchiveRequestFileName)
	archiveResultFile  = filepath.Join(maintTestDir, ArchiveResultFileName)
)

func newTestArchiveRequest(files MaintFiles, clock Clock) *ArchiveRequest {
	return &ArchiveRequest{Dir: maintTestDir, Files: files, Clock: clock}
}

// The archive request uses its own file pair: a maintenance request and result
// already in the directory are neither read nor cleared.
func TestArchiveRequestWritesOnlyItsOwnPair(t *testing.T) {
	t.Parallel()
	files := newFakeMaintFiles()
	clock := newFakeMaintClock(maintTestStart())
	putMaintJSON(t, files, maintRequestFile, RequestFile{RunID: "run-maintenance", At: maintTestStart()})
	putMaintJSON(t, files, maintResultFile, MaintenanceResult{RunID: "run-maintenance", Outcome: "completed"})

	if err := newTestArchiveRequest(files, clock).Request("run-archive"); err != nil {
		t.Fatalf("Request: %v", err)
	}

	entry, ok := files.entry(archiveRequestFile)
	if !ok {
		t.Fatal("the archive request file was not created")
	}
	if !strings.Contains(string(entry.data), "run-archive") {
		t.Errorf("archive request = %s, want run-archive", entry.data)
	}
	if _, ok := files.entry(maintRequestFile); !ok {
		t.Errorf("the maintenance request was removed by an archive request")
	}
	if _, ok := files.entry(maintResultFile); !ok {
		t.Errorf("the maintenance result was cleared by an archive request")
	}
	if _, ok := files.entry(archiveResultFile); ok {
		t.Errorf("a result file was written by the request")
	}
}

// Two archive requests do not overwrite each other: the exclusive create keeps
// the first, and the second is refused by run id.
func TestArchiveRequestIsExclusive(t *testing.T) {
	t.Parallel()
	files := newFakeMaintFiles()
	clock := newFakeMaintClock(maintTestStart())
	req := newTestArchiveRequest(files, clock)
	if err := req.Request("run-first"); err != nil {
		t.Fatalf("first Request: %v", err)
	}

	if err := req.Request("run-second"); err == nil {
		t.Fatal("a second archive request for another run id was accepted")
	} else if !strings.Contains(err.Error(), "run-first") {
		t.Errorf("error = %v, want it to name the waiting run", err)
	}
	if entry, ok := files.entry(archiveRequestFile); !ok || !strings.Contains(string(entry.data), "run-first") {
		t.Errorf("the first request was overwritten: %q", entry.data)
	}

	if err := req.Request("run-first"); err != nil {
		t.Errorf("a repeated request for the same run id: %v", err)
	}
}

// Wait returns the daemon's archive results, and reports failed for anything
// that does not answer this run.
func TestArchiveRequestWait(t *testing.T) {
	t.Parallel()

	t.Run("the daemon's result", func(t *testing.T) {
		t.Parallel()
		files := newFakeMaintFiles()
		clock := newFakeMaintClock(maintTestStart())
		files.put(archiveResultFile, []byte(`{"run_id":"run-archive","outcome":"completed","detail":"hq: archived"}`), 0o600)

		got, err := newTestArchiveRequest(files, clock).Wait(context.Background(), clock.sleep, "run-archive", maintTestTimeout, maintTestPoll)
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if got.Outcome != "completed" || got.Detail != "hq: archived" {
			t.Errorf("result = %+v, want the archive result", got)
		}
	})

	t.Run("no result", func(t *testing.T) {
		t.Parallel()
		files := newFakeMaintFiles()
		clock := newFakeMaintClock(maintTestStart())

		got, err := newTestArchiveRequest(files, clock).Wait(context.Background(), clock.sleep, "run-archive", maintTestTimeout, maintTestPoll)
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if got.Outcome != "failed" || !strings.Contains(got.Detail, ArchiveResultFileName) {
			t.Errorf("result = %+v, want failed naming %s", got, ArchiveResultFileName)
		}
	})

	t.Run("another run's result", func(t *testing.T) {
		t.Parallel()
		files := newFakeMaintFiles()
		clock := newFakeMaintClock(maintTestStart())
		putMaintJSON(t, files, archiveResultFile, MaintenanceResult{RunID: "run-other", Outcome: "completed"})

		got, err := newTestArchiveRequest(files, clock).Wait(context.Background(), clock.sleep, "run-archive", maintTestTimeout, maintTestPoll)
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if got.Outcome != "failed" {
			t.Errorf("result = %+v, want failed for another run's result", got)
		}
	})
}

// The empty run id is refused before any file is touched, as the maintenance
// protocol refuses it.
func TestArchiveRequestEmptyRunID(t *testing.T) {
	t.Parallel()
	files := newFakeMaintFiles()
	clock := newFakeMaintClock(maintTestStart())
	req := newTestArchiveRequest(files, clock)

	if err := req.Request(""); err == nil {
		t.Error("Request(\"\") returned nil")
	}
	if _, err := req.Wait(context.Background(), clock.sleep, "", maintTestTimeout, maintTestPoll); err == nil {
		t.Error("Wait(\"\") returned nil")
	}
	if entry, ok := files.entry(archiveRequestFile); ok {
		t.Errorf("the refused request wrote %q", entry.data)
	}
}

// A request file that exists but cannot be read is an error rather than a new
// request, so a run never writes over the file it could not see.
func TestArchiveRequestUnreadableExisting(t *testing.T) {
	t.Parallel()
	files := newFakeMaintFiles()
	clock := newFakeMaintClock(maintTestStart())
	files.put(archiveRequestFile, []byte(`{"run_id":"run-other"}`), 0o600)
	boom := errors.New("input/output error")
	files.readErr[archiveRequestFile] = boom

	err := newTestArchiveRequest(files, clock).Request("run-archive")
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the read error", err)
	}
}
