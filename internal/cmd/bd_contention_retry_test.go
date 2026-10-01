package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A bond that Dolt aborts for contention is repeated, not surfaced: the abort
// rolled the transaction back, so nothing was written and the retry cannot
// duplicate the wisp (gt-4ckuf).
func TestBondFormulaDirectRetriesDoltSerializationFailure(t *testing.T) {
	t.Parallel()
	fake := &fakeCook{bond: func(n int) ([]byte, error) {
		if n == 1 {
			return nil, errors.New("bonding: spawning and attaching proto: sql commit (regular): Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction")
		}
		return []byte(`{"result_id":"gt-x","id_mapping":{"mol-polecat-work":"gt-wisp-retry"}}`), nil
	}}

	townRoot := t.TempDir()
	rootID, err := formulaBDVia(fake.open).bond("mol-polecat-work", "mol-polecat-work", "gt-x", townRoot, townRoot, []string{"feature=t"})
	if err != nil {
		t.Fatalf("bondFormulaDirect: %v", err)
	}
	if rootID != "gt-wisp-retry" {
		t.Fatalf("rootID = %q, want gt-wisp-retry", rootID)
	}
	if n := len(fake.called("mol bond ")); n != 2 {
		t.Fatalf("bond attempts = %d, want 2 (one abort, one success)", n)
	}
}

// A failure bd reports for any other reason is an answer, not contention: one
// attempt, so a permanent error cannot turn into a retry storm.
func TestBondFormulaDirectDoesNotRetryNonContentionFailure(t *testing.T) {
	t.Parallel()
	fake := &fakeCook{bond: func(int) ([]byte, error) { return nil, errors.New("missing required vars: feature") }}

	townRoot := t.TempDir()
	_, err := formulaBDVia(fake.open).bond("mol-polecat-work", "mol-polecat-work", "gt-x", townRoot, townRoot, []string{})
	if err == nil {
		t.Fatal("bondFormulaDirect succeeded, want failure")
	}
	if !strings.Contains(err.Error(), "missing required vars") {
		t.Fatalf("error hides bd's cause: %v", err)
	}
	if n := len(fake.called("mol bond ")); n != 1 {
		t.Fatalf("bond attempts = %d, want 1 (no retry for a non-contention cause)", n)
	}
}

func TestBdSerializationFailure(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
