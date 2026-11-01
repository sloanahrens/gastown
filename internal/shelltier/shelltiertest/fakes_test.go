package shelltiertest

import (
	"errors"
	"io/fs"
	"testing"
)

func TestMemFilesReadsBackWhatWasWritten(t *testing.T) {
	t.Parallel()
	files := MemFiles{}
	if err := files.WriteFile("/town/state.json", []byte("{}")); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := files.ReadFile("/town/state.json")
	if err != nil || string(got) != "{}" {
		t.Fatalf("read = %q, %v; want the written bytes", got, err)
	}
}

func TestMemFilesReportsAMissingFileAsNotExist(t *testing.T) {
	t.Parallel()
	_, err := MemFiles{}.ReadFile("/town/absent.json")
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error = %v, want fs.ErrNotExist", err)
	}
}

func TestTimingAppendRecordsTheLineAndAnswersWithItsError(t *testing.T) {
	t.Parallel()
	denied := errors.New("permission denied")
	files := &Timing{Err: denied}
	if err := files.Append("/town/shell.jsonl", []byte("{}\n")); !errors.Is(err, denied) {
		t.Fatalf("Append = %v, want the configured error", err)
	}
	if len(files.Lines) != 1 || files.Lines[0] != "{}\n" || files.Paths[0] != "/town/shell.jsonl" {
		t.Fatalf("recorded paths=%v lines=%v, want the line and its path", files.Paths, files.Lines)
	}
}
