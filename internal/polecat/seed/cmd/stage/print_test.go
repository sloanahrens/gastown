package main

import (
	"bytes"
	"strings"
	"testing"
)

// A pass prints each note once, with the stage's prefix, and nothing else.
func TestPrintNotesPrintsEachNoteOnceWithThePrefix(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	printNotes(&b, []string{"pin-drift guard: rig devops pin 5ba8 is 3 commits behind its main, main e82f: no module coverage gap"})
	want := "polecat-seed: pin-drift guard: rig devops pin 5ba8 is 3 commits behind its main, main e82f: no module coverage gap\n"
	if b.String() != want {
		t.Errorf("printNotes = %q, want %q", b.String(), want)
	}
}

func TestPrintNotesPrintsNothingWithoutNotes(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	printNotes(&b, nil)
	if strings.TrimSpace(b.String()) != "" {
		t.Errorf("printNotes(nil) wrote %q, want nothing", b.String())
	}
}
