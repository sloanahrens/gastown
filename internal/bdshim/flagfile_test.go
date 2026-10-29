package bdshim

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestRecordedFlagFileIsTheCommittedFile: the doctor check compares the
// installed bd with the copy compiled into the binary, so that copy has to be
// the recording under testdata: a check reading a stale copy would warn about
// drift that was already refreshed, or pass a table the tests reject.
func TestRecordedFlagFileIsTheCommittedFile(t *testing.T) {
	t.Parallel()
	onDisk, err := os.ReadFile(filepath.Join("testdata", "bd-flags.txt"))
	if err != nil {
		t.Fatalf("reading the recorded flag file: %v", err)
	}
	if !bytes.Equal(RecordedFlagFile, onDisk) {
		t.Error("the embedded flag table differs from testdata/bd-flags.txt, want the committed recording")
	}
}
