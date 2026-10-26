package installer

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func putFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// KeepPrevious copies the installed binary aside, mode included: a rollback
// puts back something the town can still execute.
func TestKeepPrevious(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	putFile(t, tr.Paths.GT(), "installed", 0o755)

	if err := tr.KeepPrevious(); err != nil {
		t.Fatalf("KeepPrevious: %v", err)
	}

	if got, want := read(t, tr.Paths.Prev()), "installed"; got != want {
		t.Errorf("gt.prev = %q, want %q", got, want)
	}
	info, err := os.Stat(tr.Paths.Prev())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o755); got != want {
		t.Errorf("gt.prev mode = %v, want %v", got, want)
	}
}

// A first install has no binary to keep, and that is not a failure.
func TestKeepPreviousWithoutAnInstalledBinary(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)

	if err := tr.KeepPrevious(); err != nil {
		t.Fatalf("KeepPrevious: %v", err)
	}
	if _, err := os.Stat(tr.Paths.Prev()); !os.IsNotExist(err) {
		t.Errorf("gt.prev exists, want none (%v)", err)
	}
}

func TestKeepPreviousCopyFails(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	putFile(t, tr.Paths.GT(), "installed", 0o755)
	tr.Files.Copy = func(string, string) error { return errors.New("disk full") }

	if err := tr.KeepPrevious(); err == nil {
		t.Fatal("KeepPrevious succeeded, want an error")
	}
}

// Rollback restores the previous binary over the installed one, mode included,
// and puts the OS protection bracket back around it.
func TestRollback(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	putFile(t, tr.Paths.Prev(), "previous", 0o700)
	putFile(t, tr.Paths.GT(), "broken", 0o755)
	var cleared, restored []string
	tr.Files.ClearImmutable = func(path string) { cleared = append(cleared, path) }
	tr.Files.SetImmutable = func(path string) { restored = append(restored, path) }

	if err := tr.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if got, want := read(t, tr.Paths.GT()), "previous"; got != want {
		t.Errorf("gt = %q, want %q", got, want)
	}
	info, err := os.Stat(tr.Paths.GT())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o700); got != want {
		t.Errorf("gt mode = %v, want %v", got, want)
	}
	if want := []string{tr.Paths.GT()}; !reflect.DeepEqual(cleared, want) {
		t.Errorf("cleared the flag on %q, want %q", cleared, want)
	}
	if want := []string{tr.Paths.GT()}; !reflect.DeepEqual(restored, want) {
		t.Errorf("restored the flag on %q, want %q", restored, want)
	}
}

func TestRollbackWithoutAPrevious(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)

	if err := tr.Rollback(); !errors.Is(err, errNoPrevious) {
		t.Errorf("Rollback() = %v, want %v", err, errNoPrevious)
	}
}

// A rollback whose rename fails leaves the installed binary as it was, takes
// the temporary copy away, and still restores the flag so the broken binary is
// protected while a human answers the CRITICAL escalation (gt-vya0s).
func TestRollbackRenameFails(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	putFile(t, tr.Paths.Prev(), "previous", 0o755)
	putFile(t, tr.Paths.GT(), "broken", 0o755)
	var cleared, restored []string
	tr.Files.ClearImmutable = func(path string) { cleared = append(cleared, path) }
	tr.Files.SetImmutable = func(path string) { restored = append(restored, path) }
	tr.Files.Rename = func(string, string) error { return errors.New("read-only") }

	if err := tr.Rollback(); err == nil {
		t.Fatal("Rollback succeeded, want an error")
	}

	if got, want := read(t, tr.Paths.GT()), "broken"; got != want {
		t.Errorf("gt = %q, want %q", got, want)
	}
	if want := []string{tr.Paths.GT()}; !reflect.DeepEqual(cleared, want) {
		t.Errorf("cleared the flag on %q, want %q", cleared, want)
	}
	if want := []string{tr.Paths.GT()}; !reflect.DeepEqual(restored, want) {
		t.Errorf("restored the flag on %q, want %q", restored, want)
	}
	entries, err := os.ReadDir(tr.Paths.BinDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".gt.rollback.") {
			t.Errorf("left %q behind", entry.Name())
		}
	}
}

// FailInstall is the script's fail_install: the same message on the same
// escalation, the rollback only when the installed file changed, and the run
// ends failed.
func TestFailInstall(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		gt             string // "" leaves no installed binary
		previous       string // "" leaves no gt.prev
		renameFail     bool
		lines          []string
		reason         string // the receipt's reason and the result's
		severity       string
		fingerprint    string
		wantRolledBack bool
	}{
		{
			name:        "the installed binary is unchanged, so nothing is put back",
			gt:          "same",
			previous:    "same",
			lines:       []string{"[install-gt] FAILED: the build failed"},
			reason:      "build-failed",
			severity:    "medium",
			fingerprint: "install-gt:build-failed",
		},
		{
			name:           "a changed binary is rolled back first",
			gt:             "new",
			previous:       "old",
			lines:          []string{"[install-gt] FAILED: the build failed", "[install-gt] Rolled back to c1111111."},
			reason:         "build-failed",
			severity:       "medium",
			fingerprint:    "install-gt:build-failed",
			wantRolledBack: true,
		},
		{
			name:        "a rollback that fails escalates CRITICAL with its own reason",
			gt:          "new",
			previous:    "old",
			renameFail:  true,
			lines:       []string{"[install-gt] FAILED: the build failed"},
			reason:      "rollback-failed",
			severity:    "critical",
			fingerprint: "install-gt:rollback-failed",
		},
		{
			name:        "a first install has nothing to roll back to",
			gt:          "new",
			lines:       []string{"[install-gt] FAILED: the build failed"},
			reason:      "smoke-failed",
			severity:    "high",
			fingerprint: "install-gt:smoke-failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			tr.C.Expected = "c2222222"
			tr.C.Prev = "c1111111"
			tr.C.Receipts = filepath.Join(t.TempDir(), "install-receipts.jsonl")
			if tc.gt != "" {
				putFile(t, tr.Paths.GT(), tc.gt, 0o755)
			}
			if tc.previous != "" {
				putFile(t, tr.Paths.Prev(), tc.previous, 0o755)
			}
			tr.Files.ClearImmutable = func(string) {}
			tr.Files.SetImmutable = func(string) {}
			if tc.renameFail {
				tr.Files.Rename = func(string, string) error { return errors.New("read-only") }
			}

			exit := tr.FailInstall(tc.reason, "the build failed", tc.severity, tc.fingerprint, "failed")

			if exit != ExitFailed {
				t.Errorf("FailInstall() = %d, want %d", exit, ExitFailed)
			}
			wantLines := append(append([]string{}, tc.lines...),
				"install-gt: RESULT failed c2222222 c1111111 "+tc.reason)
			if got := tr.lines(); !reflect.DeepEqual(got, wantLines) {
				t.Errorf("stdout = %q, want %q", got, wantLines)
			}
			message := "install-gt: the build failed"
			if tc.wantRolledBack {
				if got, want := read(t, tr.Paths.GT()), "old"; got != want {
					t.Errorf("gt = %q, want %q", got, want)
				}
			}
			if tc.reason == "rollback-failed" {
				message = "install-gt: the build failed; and restoring " + tr.Paths.Prev() +
					" FAILED — the town may be running a broken gt"
			}
			wantEscalation := []string{tr.C.GT, "escalate", message, "-s", tc.severity,
				"--source", "script:install-gt", "--fingerprint", tc.fingerprint}
			if got := tr.escalation(); !reflect.DeepEqual(got, wantEscalation) {
				t.Errorf("escalation = %q, want %q", got, wantEscalation)
			}
			receipts := read(t, tr.C.Receipts)
			if !strings.Contains(receipts, `"reason": "`+tc.reason+`"`) {
				t.Errorf("receipts = %q, want reason %q", receipts, tc.reason)
			}
		})
	}
}
