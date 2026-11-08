package cloudledger

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// failingLedgerFile is an opened ledger whose stat or read fails, the two
// failures a healthy filesystem will not produce.
type failingLedgerFile struct {
	fi      fs.FileInfo
	statErr error
	readErr error
}

func (f failingLedgerFile) ReadAt([]byte, int64) (int, error) {
	if f.readErr != nil {
		return 0, f.readErr
	}
	return 0, io.EOF
}

func (f failingLedgerFile) Stat() (fs.FileInfo, error) { return f.fi, f.statErr }
func (f failingLedgerFile) Close() error               { return nil }

// The tail read's stat and read failures are named ledger reads, not a line
// read from a fragment.
func TestFileSourceLastReleaseNamesAFileFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "app"+LedgerSuffix)
	if err := os.WriteFile(path, []byte(`{"time":"2026-11-08T11:00:00Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	for name, file := range map[string]failingLedgerFile{
		"stat": {fi: fi, statErr: errors.New("stat failed")},
		"read": {fi: fi, readErr: errors.New("read failed")},
	} {
		src := &FileSource{
			releasesDir: dir,
			openLedger:  func(string) (ledgerFile, error) { return file, nil },
		}
		_, err := src.LastRelease(context.Background(), "app")
		if !errors.Is(err, ErrLedgerRead) {
			t.Errorf("%s failure = %v, want ErrLedgerRead", name, err)
		}
	}
}

// The DSN read's own failure is an error naming the file, not a DSN read as
// unset.
func TestReadDSNNamesTheReadFailure(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "ledger.dsn")
	if err := os.WriteFile(path, []byte("postgres://reader@fake/ledger\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gone := errors.New("the file went away mid-read")
	_, err := readDSN(path, func(string) ([]byte, error) { return nil, gone })
	if !errors.Is(err, gone) || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want the file named and the read failure kept", err)
	}
}
