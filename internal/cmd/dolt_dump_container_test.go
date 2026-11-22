package cmd

import (
	"bytes"
	"errors"
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

	writeDoltDumpContainerSection(&out, "gt-dolt", func() ([]string, error) { return nil, nil })

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
	for _, want := range []string{"Container mode (gt-dolt)", "docker logs gt-dolt", "no session reader configured", "No signal was sent."} {
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

// TestDoltDumpContainerSectionListsSessions: the section prints each session
// under a Sessions: heading, one per line and carrying no query text
// (gt-f4uyom).
func TestDoltDumpContainerSectionListsSessions(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	sessions := []string{"7 root 127.0.0.1:51000 beads Sleep 0s", "8 root 127.0.0.1:51001 beads Query 1s waiting"}

	writeDoltDumpContainerSection(&out, "gt-dolt", func() ([]string, error) { return sessions, nil })

	got := out.String()
	if !strings.Contains(got, "\n  Sessions:\n") {
		t.Fatalf("section %q has no Sessions: heading", got)
	}
	for _, line := range sessions {
		if n := strings.Count(got, "\n    "+line+"\n"); n != 1 {
			t.Errorf("section %q prints %q %d times, want once on its own line", got, line, n)
		}
	}
}

// TestDoltDumpContainerSectionSessionErrorIsShown: a failed session read prints
// its error in place of the list, and the dump still ends with its no-signal
// line (gt-f4uyom).
func TestDoltDumpContainerSectionSessionErrorIsShown(t *testing.T) {
	t.Parallel()
	townRoot := externalContainerTownRoot(t, "gt-dolt")
	reads := fixedDoltLogReads(time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC))
	reads.sessions = func() ([]string, error) { return nil, errors.New("connection refused") }
	running := func(string) (bool, int, error) { return true, 4242, nil }

	var out, errw bytes.Buffer
	if err := dumpDoltDiagnostics(townRoot, &out, &errw, running, reads); err != nil {
		t.Fatalf("dumpDoltDiagnostics: %v", err)
	}

	got := out.String()
	for _, want := range []string{"session list unavailable", "connection refused"} {
		if !strings.Contains(got, want) {
			t.Errorf("dump %q does not name %q", got, want)
		}
	}
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if last := lines[len(lines)-1]; !strings.Contains(last, "No signal was sent.") {
		t.Errorf("dump does not end with its no-signal line: %q", last)
	}
}
