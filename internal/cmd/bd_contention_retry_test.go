package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A bond that Dolt aborts for contention is repeated, not surfaced: the abort
// rolled the transaction back, so nothing was written and the retry cannot
// duplicate the wisp (gt-4ckuf).
func TestBondFormulaDirectRetriesDoltSerializationFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub is a POSIX shell script")
	}
	townRoot, logPath := setUpContentionStub(t, `#!/bin/sh
echo "CMD:$*" >> "${BD_LOG}"
if [ "$1" = "mol" ]; then
  if [ "$(grep -c '^CMD:mol bond' "${BD_LOG}")" -eq 1 ]; then
    echo '{"error":"bonding: spawning and attaching proto: sql commit (regular): Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction","schema_version":1}'
    exit 1
  fi
  echo '{"result_id":"gt-x","id_mapping":{"mol-polecat-work":"gt-wisp-retry"}}'
  exit 0
fi
exit 0
`)

	rootID, err := bondFormulaDirect("mol-polecat-work", "mol-polecat-work", "gt-x", townRoot, townRoot, []string{"feature=t"})
	if err != nil {
		t.Fatalf("bondFormulaDirect: %v", err)
	}
	if rootID != "gt-wisp-retry" {
		t.Fatalf("rootID = %q, want gt-wisp-retry", rootID)
	}
	if got := countStubAttempts(t, logPath, "CMD:mol bond"); got != 2 {
		t.Fatalf("bond attempts = %d, want 2 (one abort, one success)", got)
	}
}

// A failure bd reports for any other reason is an answer, not contention: one
// attempt, so a permanent error cannot turn into a retry storm.
func TestBondFormulaDirectDoesNotRetryNonContentionFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub is a POSIX shell script")
	}
	townRoot, logPath := setUpContentionStub(t, `#!/bin/sh
echo "CMD:$*" >> "${BD_LOG}"
echo '{"error":"missing required vars: feature","schema_version":1}'
exit 1
`)

	_, err := bondFormulaDirect("mol-polecat-work", "mol-polecat-work", "gt-x", townRoot, townRoot, []string{})
	if err == nil {
		t.Fatal("bondFormulaDirect succeeded, want failure")
	}
	if !strings.Contains(err.Error(), "missing required vars") {
		t.Fatalf("error hides bd's cause: %v", err)
	}
	if got := countStubAttempts(t, logPath, "CMD:mol bond"); got != 1 {
		t.Fatalf("bond attempts = %d, want 1 (no retry for a non-contention cause)", got)
	}
}

// setUpContentionStub installs script as the bd on PATH, logs its argv to a
// temp file, and neutralizes the retry backoff so the loop's tests finish
// without waiting out real sleeps.
func setUpContentionStub(t *testing.T, script string) (townRoot, logPath string) {
	t.Helper()
	townRoot = t.TempDir()
	binDir := filepath.Join(townRoot, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("mkdir binDir: %v", err)
	}
	logPath = filepath.Join(townRoot, "bd.log")
	_ = writeBDStub(t, binDir, script, "")
	t.Setenv("BD_LOG", logPath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	prevSleep := bdContentionSleep
	bdContentionSleep = func(time.Duration) {}
	t.Cleanup(func() { bdContentionSleep = prevSleep })
	return townRoot, logPath
}

// countStubAttempts counts the stub invocations whose logged argv starts with
// prefix — one line per attempt, which is what pins the retry to a count.
func countStubAttempts(t *testing.T, logPath, prefix string) int {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read stub log: %v", err)
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			count++
		}
	}
	return count
}

func TestBdSerializationFailure(t *testing.T) {
	tests := []struct {
		name  string
		cause string
		want  bool
	}{
		{
			name:  "dolt 1213 as bd reports it",
			cause: "bonding: spawning and attaching proto: sql commit (regular): Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction",
			want:  true,
		},
		{name: "sqlstate 40001 text alone", cause: "sql commit: serialization failure", want: true},
		{name: "restart advice alone", cause: "commit write tx: try restarting transaction", want: true},
		{name: "empty cause", cause: "", want: false},
		{name: "connection refused is not contention", cause: "connection refused", want: false},
		{name: "circuit breaker is not contention", cause: "circuit breaker is open", want: false},
		{name: "a permanent bd answer", cause: "missing required vars: feature", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := bdSerializationFailure(tc.cause); got != tc.want {
				t.Fatalf("bdSerializationFailure(%q) = %v, want %v", tc.cause, got, tc.want)
			}
		})
	}
}

// The retry verdict adds the one exclusion the cause alone cannot express: an
// attempt killed at bd's own deadline is a wedge, not contention, so its cause
// text is never read (matchesTransientMarkers makes the same call).
func TestBdContentionRetryable(t *testing.T) {
	abort := "Error 1213 (40001): serialization failure: try restarting transaction"
	tests := []struct {
		name  string
		err   error
		cause string
		want  bool
	}{
		{name: "contention abort", err: errors.New("exit status 1"), cause: abort, want: true},
		{name: "deadline kills the retry", err: context.DeadlineExceeded, cause: abort, want: false},
		{name: "wrapped deadline kills the retry", err: fmt.Errorf("bd mol timed out after 30s: %w", context.DeadlineExceeded), cause: abort, want: false},
		{name: "the wrapper's own timeout text", err: errors.New("bd mol ... (5 args) timed out after 30s"), cause: abort, want: false},
		{name: "no cause to read", err: errors.New("exit status 1"), cause: "", want: false},
		{name: "no error at all", err: nil, cause: abort, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := bdContentionRetryable(tc.err, tc.cause); got != tc.want {
				t.Fatalf("bdContentionRetryable(%v, %q) = %v, want %v", tc.err, tc.cause, got, tc.want)
			}
		})
	}
}

// A failed bond's cause is read from the machine envelope's typed error as well
// as the legacy {"error": "..."} string.
func TestBdJSONErrorMessage(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want string
	}{
		{"machine envelope", `{"schema_version":1,"contract_version":1,"data":null,"error":{"kind":"internal","message":"Error 1213 (40001): serialization failure"}}`, "Error 1213 (40001): serialization failure"},
		{"legacy string", `{"error":"creating wisp: boom"}`, "creating wisp: boom"},
		{"other shape", "not json\n", "not json"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bdJSONErrorMessage([]byte(tt.out)); got != tt.want {
				t.Fatalf("bdJSONErrorMessage = %q, want %q", got, tt.want)
			}
		})
	}
}
