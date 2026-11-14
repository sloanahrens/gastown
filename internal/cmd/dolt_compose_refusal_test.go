package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// composeExitError is a docker call that exited with a status, as the runner's
// error reports it.
type composeExitError int

func (e composeExitError) Error() string { return "exit status" }

func (e composeExitError) ExitCode() int { return int(e) }

// composeFailingWriter is a status destination that refuses every write.
type composeFailingWriter struct{}

func (composeFailingWriter) Write([]byte) (int, error) { return 0, errors.New("pipe closed") }

// TestDoltComposeRunErrorNamesTheExitStatus: a docker call that exits non-zero is
// named by its status, and one that never started is named as not run. Neither
// echoes what the call wrote.
func TestDoltComposeRunErrorNamesTheExitStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"non-zero exit", fmt.Errorf("wrapped: %w", composeExitError(3)), "docker exited with status 3"},
		{"never started", errors.New(`exec: "docker": executable file not found`), "the docker command could not be run"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := doltComposeRunError(tt.err).Error(); got != tt.want {
				t.Errorf("doltComposeRunError = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDoltComposeRefusesUnparseableValues: a values file that does not parse
// refuses the start before docker runs.
func TestDoltComposeRefusesUnparseableValues(t *testing.T) {
	t.Parallel()
	townRoot := composeTown(t, composeRef("gastown-bd"))
	writeComposeValues(t, townRoot, "daemon: [unclosed\n")
	var calls []composeCall
	err := doltComposeStart(townRoot, fakeComposeDeps(townRoot, &calls, nil, nil))
	if err == nil || !strings.Contains(err.Error(), "reading") {
		t.Fatalf("doltComposeStart error = %v, want a refusal that names the values file read", err)
	}
	if len(calls) != 0 {
		t.Errorf("refused start still made %d docker calls, want 0", len(calls))
	}
}

// TestDoltComposeRefusesUnreadableBackupRoot: a backup root that cannot be found
// refuses the start before docker runs.
func TestDoltComposeRefusesUnreadableBackupRoot(t *testing.T) {
	t.Parallel()
	townRoot := composeTown(t, composeRef("gastown-bd"))
	var calls []composeCall
	deps := fakeComposeDeps(townRoot, &calls, nil, nil)
	deps.backupRoot = func() (string, error) { return "", errors.New("no backup root") }
	err := doltComposeStart(townRoot, deps)
	if err == nil || !strings.Contains(err.Error(), "backup directory") {
		t.Fatalf("doltComposeStart error = %v, want a refusal that names the backup directory", err)
	}
	if len(calls) != 0 {
		t.Errorf("refused start still made %d docker calls, want 0", len(calls))
	}
}

// TestDoltComposeSurfacesStopAndStatusFailures: a docker call that fails under
// stop or status is the command's error, named by the call.
func TestDoltComposeSurfacesStopAndStatusFailures(t *testing.T) {
	t.Parallel()
	townRoot := composeTown(t, composeRef("gastown-bd"))
	tests := []struct {
		name string
		op   func(deps doltComposeDeps) error
		want string
	}{
		{"stop", func(deps doltComposeDeps) error { return doltComposeStop(townRoot, deps) }, "docker compose stop"},
		{"status", func(deps doltComposeDeps) error {
			return doltComposeStatus(townRoot, deps, &bytes.Buffer{})
		}, "docker compose ps"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []composeCall
			err := tt.op(fakeComposeDeps(townRoot, &calls, nil, errors.New("docker exited with status 1")))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("%s error = %v, want one naming %q", tt.name, err, tt.want)
			}
		})
	}
}

// TestDoltComposeStatusReportsAWriteFailure: a status that cannot be written is
// an error, not a silent success.
func TestDoltComposeStatusReportsAWriteFailure(t *testing.T) {
	t.Parallel()
	townRoot := composeTown(t, composeRef("gastown-bd"))
	var calls []composeCall
	err := doltComposeStatus(townRoot, fakeComposeDeps(townRoot, &calls, []byte("dolt running\n"), nil), composeFailingWriter{})
	if err == nil || !strings.Contains(err.Error(), "writing the status") {
		t.Fatalf("doltComposeStatus error = %v, want a write failure", err)
	}
}
