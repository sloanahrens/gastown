package testpolicy

import (
	"path/filepath"
	"testing"
	"time"
)

// TestCPURecordRoundTrip checks that the exec wrapper's record for one package
// directory is what the budget runner reads back for that directory, and for
// no other.
func TestCPURecordRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := filepath.Join(t.TempDir(), "repo")
	want := CPUTime{User: 2100 * time.Millisecond, Sys: 40 * time.Second}
	if err := WriteCPU(dir, filepath.Join(root, "internal", "testpolicy"), want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadCPU(dir, filepath.Join(root, "internal", "testpolicy"))
	if err != nil || !ok || got != want {
		t.Fatalf("ReadCPU = %+v, %v, %v; want %+v, true, nil", got, ok, err, want)
	}
	// The same directory spelled with a trailing separator is the same package.
	if got, ok, err := ReadCPU(dir, filepath.Join(root, "internal", "testpolicy")+string(filepath.Separator)); err != nil || !ok || got != want {
		t.Fatalf("ReadCPU(trailing separator) = %+v, %v, %v; want %+v, true, nil", got, ok, err, want)
	}
	if _, ok, err := ReadCPU(dir, filepath.Join(root, "internal", "git")); err != nil || ok {
		t.Fatalf("ReadCPU(unrecorded package) = %v, %v; want false, nil", ok, err)
	}
}
