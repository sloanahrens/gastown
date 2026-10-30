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
