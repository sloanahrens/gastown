package installer

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The argv is the script's escalate(): gt escalate "install-gt: <message>" -s
// <severity> --source script:install-gt --fingerprint <fingerprint>, with the
// output thrown away.
func TestEscalate(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	var got []string
	c := &Contract{
		Stdout: &out,
		GT:     "/town/bin/gt",
		Exec: func(name string, args ...string) ([]byte, error) {
			got = append([]string{name}, args...)
			return nil, nil
		},
	}

	c.Escalate("HIGH", "install-gt:build-failed", "the build did not finish")

	want := []string{"/town/bin/gt", "escalate", "install-gt: the build did not finish",
		"-s", "HIGH", "--source", "script:install-gt", "--fingerprint", "install-gt:build-failed"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("escalated with %q, want %q", got, want)
	}
	if out.Len() != 0 {
		t.Errorf("a delivered escalation logged %q, want nothing", out.String())
	}
}

// A town that does not hear the escalation is one WARNING line: the install's
// outcome is decided by the install.
func TestEscalateThatCannotBeDeliveredWarns(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	c := &Contract{
		Stdout: &out,
		Exec: func(string, ...string) ([]byte, error) {
			return nil, errors.New("no town")
		},
	}

	c.Escalate("MEDIUM", "install-gt:build-failed", "the build did not finish")

	want := "[install-gt] WARNING: escalation install-gt:build-failed did not reach the town\n"
	if got := out.String(); got != want {
		t.Errorf("wrote %q, want %q", got, want)
	}
}

// A run that named its result keeps the status it chose.
func TestOnExitPassesAStatedResultThrough(t *testing.T) {
	t.Parallel()
	var out strings.Builder
	c := &Contract{Stdout: &out, Expected: "abc123", Prev: "def456"}
	c.Report(Result{Event: "installed", Commit: "abc123", Prev: "def456"})

	if got := c.OnExit(ExitOK); got != ExitOK {
		t.Errorf("OnExit returned %d, want %d", got, ExitOK)
	}
	if want := "install-gt: RESULT installed abc123 def456 -\n"; out.String() != want {
		t.Errorf("wrote %q, want %q", out.String(), want)
	}
}

// Anything else is failed/unexpected and status 1, whatever status was headed
// for the caller: the script's trap rewrites a status that stated no result.
func TestOnExitWithoutAResultIsFailedUnexpected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		expected string
		prev     string
		want     string
	}{
		{"target known", "abc123", "def456", "install-gt: RESULT failed abc123 def456 unexpected\n"},
		{"nothing known", "", "", "install-gt: RESULT failed - - unexpected\n"},
		{"the SHA argument, before it is resolved", "origin/main", "", "install-gt: RESULT failed origin/main - unexpected\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out strings.Builder
			c := &Contract{Stdout: &out, Expected: tc.expected, Prev: tc.prev}

			if got := c.OnExit(ExitBusy); got != ExitFailed {
				t.Errorf("OnExit returned %d, want %d", got, ExitFailed)
			}
			if got := out.String(); got != tc.want {
				t.Errorf("wrote %q, want %q", got, tc.want)
			}
		})
	}
}

// The trap leaves a receipt before it reports, when the run knows where the
// daemon directory is.
func TestOnExitLeavesAFailedReceipt(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "daemon", "install-receipts.jsonl")
	now := time.Date(2026, 10, 26, 19, 20, 36, 0, time.UTC)
	var out, errOut strings.Builder
	c := &Contract{
		Stdout:   &out,
		Stderr:   &errOut,
		Receipts: path,
		Source:   "manual",
		Expected: "abc123",
		Prev:     "def456",
		Start:    now.Add(-12 * time.Second),
		Now:      func() time.Time { return now },
	}

	c.OnExit(ExitFailed)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the receipt: %v", err)
	}
	want := `{"commit": "abc123", "duration_s": 12, "event": "failed", "merged_at": null, ` +
		`"prev_commit": "def456", "reason": "unexpected", "source": "manual", "ts": "2026-10-26T19:20:36Z"}` + "\n"
	if string(got) != want {
		t.Errorf("receipt = %q, want %q", got, want)
	}
	if errOut.Len() != 0 {
		t.Errorf("a written receipt warned %q", errOut.String())
	}
}

// A receipt that cannot be written changes nothing: the result line still ends
// the run and the status is still the failed one.
func TestReceiptThatCannotBeWrittenWarnsOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	notADir := filepath.Join(dir, "daemon")
	if err := os.WriteFile(notADir, []byte("in the way"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(notADir, "install-receipts.jsonl")
	var out, errOut strings.Builder
	c := &Contract{Stdout: &out, Stderr: &errOut, Receipts: path}

	if got := c.OnExit(ExitFailed); got != ExitFailed {
		t.Errorf("OnExit returned %d, want %d", got, ExitFailed)
	}
	if want := "[install-gt] WARNING: could not append a receipt to " + path + "\n"; errOut.String() != want {
		t.Errorf("stderr = %q, want %q", errOut.String(), want)
	}
	if want := "install-gt: RESULT failed - - unexpected\n"; out.String() != want {
		t.Errorf("stdout = %q, want %q", out.String(), want)
	}
}

// Before the daemon directory resolves, the run writes no receipt and says
// nothing about it: the script has the same early paths.
func TestReceiptWithoutAPathWritesNothing(t *testing.T) {
	t.Parallel()
	var out, errOut strings.Builder
	c := &Contract{Stdout: &out, Stderr: &errOut}

	c.Receipt("installed", "abc123", "def456", "")

	if out.Len() != 0 || errOut.Len() != 0 {
		t.Errorf("wrote %q / %q, want nothing", out.String(), errOut.String())
	}
}
