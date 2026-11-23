package dispatch

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

var (
	liftAt         = time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	liftFixture    = LiftRequest{Reason: "main fixed", By: "overseer", At: liftAt}
	refusalFixture = LiftRefusal{At: liftAt, Still: []string{RedHoldMainRed, RedHoldSweepRed}}
)

// liftFile is one lift record file and the calls a test drives it through.
type liftFile struct {
	name  string
	path  func(string) string
	base  string
	want  any
	write func(root string) error
	read  func(root string) (any, bool, error)
	clear func(root string) error
}

func liftFiles() []liftFile {
	return []liftFile{
		{"lift", RedHoldLiftPath, "red-hold.lift", liftFixture,
			func(root string) error { return WriteLift(root, liftFixture) },
			func(root string) (any, bool, error) { return ReadLift(root) },
			ClearLift},
		{"refusal", RedHoldRefusalPath, "red-hold.lift-refused", refusalFixture,
			func(root string) error { return WriteRefusal(root, refusalFixture) },
			func(root string) (any, bool, error) { return ReadRefusal(root) },
			ClearRefusal},
	}
}

// TestRedHoldLiftRoundTrip: each record reads back field for field from its
// daemon path.
func TestRedHoldLiftRoundTrip(t *testing.T) {
	t.Parallel()
	for _, f := range liftFiles() {
		t.Run(f.name, func(t *testing.T) {
			root := t.TempDir()
			if err := f.write(root); err != nil {
				t.Fatalf("write: %v", err)
			}
			if got, want := f.path(root), filepath.Join(root, "daemon", f.base); got != want {
				t.Fatalf("path = %q, want %q", got, want)
			}
			got, found, err := f.read(root)
			if err != nil || !found {
				t.Fatalf("read after write = %+v, found %v, err %v; want found and nil", got, found, err)
			}
			if !reflect.DeepEqual(got, f.want) {
				t.Errorf("read = %+v, want %+v", got, f.want)
			}
		})
	}
}

// TestRedHoldLiftReadRefusals: a missing file is false and nil, and a file that
// cannot be parsed or read is an error.
func TestRedHoldLiftReadRefusals(t *testing.T) {
	t.Parallel()
	for _, f := range liftFiles() {
		t.Run(f.name, func(t *testing.T) {
			if _, found, err := f.read(t.TempDir()); found || err != nil {
				t.Fatalf("a missing file: found %v, err %v; want false and nil", found, err)
			}
			bad := t.TempDir()
			if err := writeRawFile(f.path(bad), "{not json"); err != nil {
				t.Fatal(err)
			}
			if _, _, err := f.read(bad); err == nil {
				t.Fatal("invalid JSON: nil, want an error")
			}
			dir := t.TempDir()
			if err := os.MkdirAll(f.path(dir), 0o755); err != nil {
				t.Fatal(err)
			}
			if _, _, err := f.read(dir); err == nil {
				t.Fatal("a directory at the path: nil, want an error")
			}
		})
	}
}

// TestRedHoldLiftClear: each clear removes its file, tolerates a missing one,
// and reports a removal that fails.
func TestRedHoldLiftClear(t *testing.T) {
	t.Parallel()
	for _, f := range liftFiles() {
		t.Run(f.name, func(t *testing.T) {
			root := t.TempDir()
			if err := f.write(root); err != nil {
				t.Fatalf("write: %v", err)
			}
			if err := f.clear(root); err != nil {
				t.Fatalf("clear: %v", err)
			}
			if _, err := os.Stat(f.path(root)); !os.IsNotExist(err) {
				t.Fatalf("after clear: err %v, want not exist", err)
			}
			if err := f.clear(root); err != nil {
				t.Fatalf("clear a missing file: %v, want nil", err)
			}
			blocked := t.TempDir()
			if err := os.MkdirAll(filepath.Join(f.path(blocked), "keep"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := f.clear(blocked); err == nil {
				t.Fatal("a non-empty directory at the path: nil, want an error")
			}
		})
	}
}

func writeRawFile(path, data string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(data), 0o644)
}
