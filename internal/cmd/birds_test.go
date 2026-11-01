package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/birdreports"
)

// birdsNow is the clock the birds tests inject.
var birdsNow = time.Date(2026, 11, 1, 14, 0, 0, 0, time.UTC)

// writeBirdReport writes daily.json for reporter under the runtime dir, as the
// bird would, written_at aged before birdsNow.
func writeBirdReport(t *testing.T, runtimeDir, reporter string, age time.Duration) {
	t.Helper()
	reports := filepath.Join(runtimeDir, reporter, birdreports.ReportsSubdir)
	if err := os.MkdirAll(reports, 0o755); err != nil {
		t.Fatalf("making %s: %v", reports, err)
	}
	body := fmt.Sprintf(`{"bird":%q,"written_at":%q,"title":"t","summary":"s"}`,
		reporter, birdsNow.Add(-age).Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(reports, birdreports.SidecarName), []byte(body), 0o644); err != nil {
		t.Fatalf("writing the sidecar: %v", err)
	}
}

func TestBirdsDuePrintsTheLateReportItsAgeAndTheRequest(t *testing.T) {
	t.Parallel()
	runtimeDir := t.TempDir()
	writeBirdReport(t, runtimeDir, "dunlin", 2*time.Hour)
	writeBirdReport(t, runtimeDir, "eagle", 31*time.Hour+5*time.Minute)

	var out bytes.Buffer
	err := runBirdsDue(&out, runtimeDir, birdsNow, []string{"dunlin", "eagle", "owl"})
	if code, ok := IsSilentExit(err); !ok || code != 1 {
		t.Fatalf("runBirdsDue = %v (exit %d, coded %v), want a silent exit 1", err, exitCodeForError(err), ok)
	}

	got := out.String()
	for _, want := range []string{
		"eagle: stale, 31h05m old",
		"owl: missing (no daily.json)",
		birdreports.Request("eagle"),
		birdreports.Request("owl"),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not hold %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "dunlin") {
		t.Errorf("a fresh report is in the output:\n%s", got)
	}
	if strings.Contains(got, "<NAME>") {
		t.Errorf("a request went out with <NAME> unfilled:\n%s", got)
	}
	if n := strings.Count(got, "it is time for your daily report"); n != 2 {
		t.Errorf("output holds %d requests, want 2 for the two due reporters", n)
	}
}

func TestBirdsDueExitsZeroWhenEveryReportIsFresh(t *testing.T) {
	t.Parallel()
	runtimeDir := t.TempDir()
	writeBirdReport(t, runtimeDir, "dunlin", time.Hour)
	writeBirdReport(t, runtimeDir, "owl", 25*time.Hour)

	var out bytes.Buffer
	if err := runBirdsDue(&out, runtimeDir, birdsNow, []string{"dunlin", "owl"}); err != nil {
		t.Fatalf("runBirdsDue with fresh reports = %v, want nil", err)
	}
	if got, want := out.String(), "all 2 daily reports are fresh\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestBirdsDueCommandRefusesArguments(t *testing.T) {
	t.Parallel()
	looked := false
	err := runBirdsDueCommand(&cobra.Command{}, []string{"extra"},
		func() (string, error) { looked = true; return "", nil },
		func() time.Time { return birdsNow })
	if code := exitCodeForError(err); code != 2 {
		t.Fatalf("gt birds due with an argument exits %d (%v), want 2", code, err)
	}
	if looked {
		t.Error("a refused invocation looked up the town")
	}
}

func TestBirdsDueCommandReturnsAFailedTownLookup(t *testing.T) {
	t.Parallel()
	want := errors.New("not in a town")
	err := runBirdsDueCommand(&cobra.Command{}, nil,
		func() (string, error) { return "", want },
		func() time.Time { return birdsNow })
	if !errors.Is(err, want) {
		t.Fatalf("runBirdsDueCommand = %v, want the lookup's error", err)
	}
}

func TestBirdsDueCommandPassesAWriteFailureThrough(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	cmd.SetOut(failingWriter{})

	// A town with no reports: every reporter is due, so the first request is
	// written, and that write fails.
	err := runBirdsDueCommand(cmd, nil,
		func() (string, error) { return t.TempDir(), nil },
		func() time.Time { return birdsNow })
	if err == nil {
		t.Fatal("a failed write did not fail the command")
	}
	if _, silent := IsSilentExit(err); silent {
		t.Errorf("a failed write exited silently as %v", err)
	}
	if cmd.SilenceErrors {
		t.Error("a failed write was silenced as if the exit status were the answer")
	}
}

func TestBirdsDueCommandSilencesTheExitStatus(t *testing.T) {
	t.Parallel()
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})

	// A town with no reports is due, and the status is the answer: cobra must
	// not print "Error: exit 1" over it.
	err := runBirdsDueCommand(cmd, nil,
		func() (string, error) { return t.TempDir(), nil },
		func() time.Time { return birdsNow })
	if code, silent := IsSilentExit(err); !silent || code != 1 {
		t.Fatalf("runBirdsDueCommand = %v, want a silent exit 1", err)
	}
	if !cmd.SilenceErrors || !cmd.SilenceUsage {
		t.Error("the silent exit would be printed as a cobra error")
	}
}

func TestBirdsDueNamesAReportItCannotRead(t *testing.T) {
	t.Parallel()
	runtimeDir := t.TempDir()
	reports := filepath.Join(runtimeDir, "eagle", birdreports.ReportsSubdir)
	if err := os.MkdirAll(reports, 0o755); err != nil {
		t.Fatalf("making %s: %v", reports, err)
	}
	if err := os.WriteFile(filepath.Join(reports, birdreports.SidecarName), []byte("{"), 0o644); err != nil {
		t.Fatalf("writing the sidecar: %v", err)
	}

	var out bytes.Buffer
	err := runBirdsDue(&out, runtimeDir, birdsNow, []string{"eagle"})
	if code, ok := IsSilentExit(err); !ok || code != 1 {
		t.Fatalf("runBirdsDue = %v, want a silent exit 1 for an unreadable report", err)
	}
	if want := "eagle: unreadable (daily.json is not valid JSON)"; !strings.Contains(out.String(), want) {
		t.Errorf("output does not hold %q:\n%s", want, out.String())
	}
	if !strings.Contains(out.String(), birdreports.Request("eagle")) {
		t.Error("a reporter with an unreadable report got no request")
	}
}
