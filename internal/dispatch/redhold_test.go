package dispatch

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestRedHoldFileRoundTrip: what WriteRedHold publishes is what ReadRedHold reads back, field for field.
func TestRedHoldFileRoundTrip(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	since := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)
	want := RedHoldRecord{Reasons: []RedHoldReason{
		{Kind: RedHoldMainRed, Since: since, Detail: "main is red", CureBeads: []string{"gt-a", "gt-b"}},
		{Kind: RedHoldSweepNoResult, Since: since.Add(time.Hour)},
	}}
	if err := WriteRedHold(townRoot, want); err != nil {
		t.Fatalf("WriteRedHold: %v", err)
	}
	if got := RedHoldPath(townRoot); got != filepath.Join(townRoot, "daemon", "red-hold") {
		t.Fatalf("RedHoldPath = %q, want the daemon directory", got)
	}
	got, err := ReadRedHold(townRoot)
	if err != nil {
		t.Fatalf("ReadRedHold after WriteRedHold: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadRedHold = %+v, want %+v", got, want)
	}
}

// TestRedHoldFileReadRefusals: a missing file is an empty record and nil; a path that cannot be read or parsed is an error.
func TestRedHoldFileReadRefusals(t *testing.T) {
	t.Parallel()
	if rec, err := ReadRedHold(t.TempDir()); err != nil || len(rec.Reasons) != 0 {
		t.Fatalf("ReadRedHold on a missing file = %+v, %v; want empty and nil", rec, err)
	}
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(RedHoldPath(townRoot), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRedHold(townRoot); err == nil {
		t.Fatal("ReadRedHold on invalid JSON = nil, want an error")
	}
	unreadable := t.TempDir()
	if err := os.MkdirAll(RedHoldPath(unreadable), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRedHold(unreadable); err == nil {
		t.Fatal("ReadRedHold on a path that is a directory = nil, want an error")
	}
}

// TestRedHoldFileEmptyRemoves: an empty record removes the file, a missing file
// is not an error, and a removal that fails is an error.
func TestRedHoldFileEmptyRemoves(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := WriteRedHold(townRoot, RedHoldRecord{Reasons: []RedHoldReason{{Kind: RedHoldMainRed}}}); err != nil {
		t.Fatalf("WriteRedHold: %v", err)
	}
	if _, err := os.Stat(RedHoldPath(townRoot)); err != nil {
		t.Fatalf("the written file: %v", err)
	}
	if err := WriteRedHold(townRoot, RedHoldRecord{}); err != nil {
		t.Fatalf("WriteRedHold of an empty record: %v", err)
	}
	if _, err := os.Stat(RedHoldPath(townRoot)); !os.IsNotExist(err) {
		t.Fatalf("the file after an empty record: err = %v, want not exist", err)
	}
	if err := WriteRedHold(townRoot, RedHoldRecord{}); err != nil {
		t.Fatalf("WriteRedHold of an empty record with no file: %v", err)
	}
	blocked := t.TempDir()
	if err := os.MkdirAll(filepath.Join(RedHoldPath(blocked), "keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteRedHold(blocked, RedHoldRecord{}); err == nil {
		t.Fatal("WriteRedHold at a path that cannot be removed = nil, want an error")
	}
}
