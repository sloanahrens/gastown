package cmd

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDoltDumpContainerSectionNamesTheLogCommand: the section gt dolt dump
// prints for a container town names the container and the host command that
// reads its log, and prints no host read the container does not make
// (gt-h0v2nw).
func TestDoltDumpContainerSectionNamesTheLogCommand(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer

	writeDoltDumpContainerSection(&out, "gt-dolt")

	got := out.String()
	for _, want := range []string{"Container mode (gt-dolt)", "docker logs gt-dolt"} {
		if !strings.Contains(got, want) {
			t.Errorf("section %q does not name %q", got, want)
		}
	}
	for _, refuse := range []string{"Live PID", "SQL metadata", "Recent Dolt log lines", filepath.Join("daemon", "dolt.log")} {
		if strings.Contains(got, refuse) {
			t.Errorf("section %q prints %q", got, refuse)
		}
	}
}

// TestDoltDumpContainerSectionOnDumpCommand: gt dolt dump in a container town
// prints the container section in place of the host reads — no live PID, no
// host log path, no SQL metadata, no host log tail — and keeps the closing
// advisory (gt-h0v2nw).
func TestDoltDumpContainerSectionOnDumpCommand(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	townRoot := externalContainerTownRoot(t, "gt-dolt")
	running := func(string) (bool, int, error) { return true, 4242, nil }

	var out, errw bytes.Buffer
	if err := dumpDoltDiagnostics(townRoot, &out, &errw, running, fixedDoltLogReads(now)); err != nil {
		t.Fatalf("dumpDoltDiagnostics: %v", err)
	}

	got := out.String()
	for _, want := range []string{"Container mode (gt-dolt)", "docker logs gt-dolt", "No signal was sent."} {
		if !strings.Contains(got, want) {
			t.Errorf("dump %q does not name %q", got, want)
		}
	}
	for _, refuse := range []string{"Live PID", "SQL metadata", "Recent Dolt log lines", filepath.Join(townRoot, "daemon", "dolt.log")} {
		if strings.Contains(got, refuse) {
			t.Errorf("dump %q prints %q", got, refuse)
		}
	}
}

// TestDoltDumpContainerSectionRefusesUnusableContainer: a town whose
// dolt.external_container is set but does not parse is the dump's error, and the
// dump prints nothing — it never reads the town as a host town and dumps host
// files (gt-h0v2nw).
func TestDoltDumpContainerSectionRefusesUnusableContainer(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	townRoot := externalContainerTownRoot(t, "-v")
	running := func(string) (bool, int, error) { return true, 4242, nil }

	var out, errw bytes.Buffer
	err := dumpDoltDiagnostics(townRoot, &out, &errw, running, fixedDoltLogReads(now))

	if err == nil {
		t.Fatal("dumpDoltDiagnostics accepted dolt.external_container = \"-v\", want an error")
	}
	if !strings.Contains(err.Error(), "dolt.external_container") {
		t.Errorf("error %q does not name dolt.external_container", err)
	}
	if out.Len() != 0 || errw.Len() != 0 {
		t.Errorf("dump printed out=%q err=%q, want nothing", out.String(), errw.String())
	}
}
