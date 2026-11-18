package update

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// appendRawHistory adds text to the end of history.jsonl exactly as given, so a
// test can write a corrupt or partial line that AppendHistory would never write.
func appendRawHistory(t *testing.T, dir, text string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(dir, historyFileName), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open history.jsonl: %v", err)
	}
	if _, err := f.WriteString(text); err != nil {
		t.Fatalf("write history.jsonl: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close history.jsonl: %v", err)
	}
}

// appendRun records one finished run with AppendHistory and returns it.
func appendRun(t *testing.T, store *Store, runID string) State {
	t.Helper()
	st := fullState()
	st.RunID = runID
	st.Outcome = OutcomeOK
	if err := store.AppendHistory(st); err != nil {
		t.Fatalf("AppendHistory(%s): %v", runID, err)
	}
	return st
}

func TestUpdateHistoryReadNewestFirstAndLimit(t *testing.T) {
	t.Parallel()
	store := &Store{Dir: filepath.Join(t.TempDir(), "update")}
	first := appendRun(t, store, "20261103T100000Z-furiosa")
	second := appendRun(t, store, "20261103T110000Z-furiosa")
	third := appendRun(t, store, "20261103T120000Z-furiosa")

	tests := []struct {
		name string
		n    int
		want []State
	}{
		{name: "the newest two of three, newest first", n: 2, want: []State{third, second}},
		{name: "every run when n exceeds the count", n: 10, want: []State{third, second, first}},
		{name: "exactly the count", n: 3, want: []State{third, second, first}},
		{name: "zero asks for no runs", n: 0, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ReadHistory(store.Dir, tt.n)
			if err != nil {
				t.Fatalf("ReadHistory(n=%d): %v", tt.n, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("ReadHistory(n=%d) returned %d runs, want %d", tt.n, len(got), len(tt.want))
			}
			for i := range tt.want {
				if got[i].RunID != tt.want[i].RunID {
					t.Errorf("run %d = %s, want %s", i, got[i].RunID, tt.want[i].RunID)
				}
				assertSameState(t, got[i].State, tt.want[i])
			}
		})
	}
}

func TestUpdateHistoryReadMissingFileIsEmpty(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		dir  string
	}{
		{name: "the state directory exists without a history file", dir: t.TempDir()},
		{name: "the state directory does not exist", dir: filepath.Join(t.TempDir(), "update")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ReadHistory(tt.dir, 5)
			if err != nil {
				t.Fatalf("ReadHistory on a missing file = %v, want nil", err)
			}
			if len(got) != 0 {
				t.Errorf("ReadHistory on a missing file = %d runs, want none", len(got))
			}
		})
	}
}

func TestUpdateHistoryReadSkipsCorruptAndPartialLines(t *testing.T) {
	t.Parallel()
	store := &Store{Dir: filepath.Join(t.TempDir(), "update")}
	older := appendRun(t, store, "20261103T100000Z-furiosa")
	appendRawHistory(t, store.Dir, "not json at all\n")
	// A line at another schema version is skipped too: this build has no decoder
	// for it, so it is not read as a run.
	appendRawHistory(t, store.Dir, `{"v":2,"run_id":"20261103T105000Z-future"}`+"\n")
	newer := appendRun(t, store, "20261103T110000Z-furiosa")
	// A truncated line that does not parse.
	appendRawHistory(t, store.Dir, `{"v":1,"run_id":"20261103T115000Z-cut`+"\n")
	// The last line is a whole run whose newline has not been written yet: the
	// append is still in progress, so it is not a finished run.
	inFlight := fullState()
	inFlight.RunID = "20261103T120000Z-inflight"
	inFlight.Outcome = OutcomeOK
	data, err := json.Marshal(HistoryLine{State: inFlight})
	if err != nil {
		t.Fatalf("encode in-flight run: %v", err)
	}
	appendRawHistory(t, store.Dir, string(data))

	got, err := ReadHistory(store.Dir, 10)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ReadHistory returned %d runs, want 2 (the corrupt, other-version and partial lines skipped)", len(got))
	}
	assertSameState(t, got[0].State, newer)
	assertSameState(t, got[1].State, older)
}

func TestUpdateHistoryReadRefusesNegativeCount(t *testing.T) {
	t.Parallel()
	store := &Store{Dir: filepath.Join(t.TempDir(), "update")}
	appendRun(t, store, "20261103T100000Z-furiosa")

	got, err := ReadHistory(store.Dir, -1)
	if err == nil {
		t.Fatalf("ReadHistory(n=-1) = %d runs, want an error", len(got))
	}
	if !strings.Contains(err.Error(), "negative") {
		t.Errorf("error %q does not name the negative count", err)
	}
}

func TestUpdateHistoryReadReportsAnUnreadableFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		dir  func(t *testing.T) string
	}{
		{
			// The file opens but the read fails.
			name: "history.jsonl is a directory",
			dir: func(t *testing.T) string {
				dir := filepath.Join(t.TempDir(), "update")
				if err := os.MkdirAll(filepath.Join(dir, historyFileName), 0o700); err != nil {
					t.Fatalf("make history.jsonl a directory: %v", err)
				}
				return dir
			},
		},
		{
			// The open fails with a path error that is not a missing file.
			name: "the state directory is a regular file",
			dir: func(t *testing.T) string {
				dir := filepath.Join(t.TempDir(), "update")
				if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
					t.Fatalf("make the state directory a file: %v", err)
				}
				return dir
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ReadHistory(tt.dir(t), 5)
			if err == nil {
				t.Fatalf("ReadHistory = %d runs, want an error", len(got))
			}
			if errors.Is(err, os.ErrNotExist) {
				t.Errorf("error %v reads as a missing file; a missing file must be an empty list", err)
			}
		})
	}
}
