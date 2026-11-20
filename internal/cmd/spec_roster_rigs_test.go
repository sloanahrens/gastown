package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestTickEffRigsWithPolecatDirs: polecats in two of five rigs, an empty polecat
// directory in one and none in two, answer those two in the list's order (the
// list is out of name order); a directory that cannot be listed keeps its rig
// and returns its error.
func TestTickEffRigsWithPolecatDirs(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	for _, dir := range []string{
		"gastown/polecats/nux", "gastown/polecats/furiosa",
		"beaver/polecats/chip", "empty/polecats",
		"broken",
	} {
		if err := os.MkdirAll(filepath.Join(town, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A file where a polecat directory would be cannot be listed.
	if err := os.WriteFile(filepath.Join(town, "broken", "polecats"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// "quiet" and "gone" hold no polecat directory.
	got, err := rigsWithPolecatDirs(town, []string{"gone", "beaver", "quiet", "gastown", "empty", "broken"})
	if err == nil {
		t.Fatalf("rigsWithPolecatDirs error = nil, want the broken rig's listing error")
	}
	if want := []string{"beaver", "gastown", "broken"}; !reflect.DeepEqual(got, want) {
		t.Errorf("rigsWithPolecatDirs = %v, want %v", got, want)
	}
}
