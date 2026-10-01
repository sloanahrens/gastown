package beads

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"
)

type codedExit int

func (e codedExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e codedExit) ExitCode() int { return int(e) }

// infrastructureFailures are what bd prints when it could not answer. None
// of them says anything about whether a bead exists (G3-01, G5-02).
var infrastructureFailures = []string{
	"Error: database not found: gastown",
	"Error: table not found: wisps",
	"Error: failed to open store: column not found: owner",
	`exec: "bd": executable file not found in $PATH`,
	"Error: remote not found: origin",
	"Error: dial tcp: lookup dolt: no such host",
	"Error resolving gt-abc: table not found: issues",
	"Error: not found",
}

func TestBDSaidNotFound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		exit   int
		stdout string
		want   bool
	}{
		{"machine exit 20", 20, "", true},
		{"envelope kind", 1, `{"schema_version":1,"contract_version":1,"data":null,"pagination":null,"error":{"kind":"not_found","message":"issue gt-x not found"}}`, true},
		// The not-found sentence under another kind is not absence: bd
		// types an unknown id as not_found (be-2bc).
		{"envelope internal kind with resolver sentence", 1, `{"schema_version":1,"contract_version":1,"data":null,"pagination":null,"error":{"kind":"internal","message":"resolving issue ID gt-nosuch: no issue found matching \"gt-nosuch\""}}`, false},
		{"envelope other kind", 25, `{"schema_version":1,"contract_version":1,"data":null,"pagination":null,"error":{"kind":"store_unavailable","message":"database not found: gastown"}}`, false},
		{"legacy json error", 1, `{"error": "no issues found matching the provided IDs", "schema_version": 1}`, false},
		{"guard exit", 13, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := bdSaidNotFound(tc.exit, []byte(tc.stdout)); got != tc.want {
				t.Fatalf("bdSaidNotFound(%d, %q) = %v, want %v", tc.exit, tc.stdout, got, tc.want)
			}
		})
	}
}

// TestWrapErrorInfrastructureIsUnavailable: every failure that is not bd
// saying the bead does not exist is ErrUnavailable, keeps its message and
// keeps the process error (and its exit status) reachable.
func TestWrapErrorInfrastructureIsUnavailable(t *testing.T) {
	t.Parallel()
	b := New("/test")
	for _, stderr := range infrastructureFailures {
		err := b.wrapError(codedExit(1), nil, stderr, []string{"show", "gt-x", "--json"})
		if errors.Is(err, ErrNotFound) {
			t.Errorf("%q: wrapError = ErrNotFound", stderr)
		}
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("%q: wrapError = %v, want ErrUnavailable", stderr, err)
		}
		if want := "bd show gt-x --json: " + stderr; err.Error() != want {
			t.Errorf("message = %q, want %q", err.Error(), want)
		}
		var code interface{ ExitCode() int }
		if !errors.As(err, &code) || code.ExitCode() != 1 {
			t.Errorf("%q: exit status lost", stderr)
		}
	}
}

func TestWrapErrorTypedExits(t *testing.T) {
	t.Parallel()
	b := New("/test")
	if err := b.wrapError(codedExit(20), nil, "", []string{"show", "gt-x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("exit 20 = %v, want ErrNotFound", err)
	}
	for _, code := range []int{24, 25, 26} {
		if err := b.wrapError(codedExit(code), nil, "Issue gt-x not found", []string{"show", "gt-x"}); errors.Is(err, ErrNotFound) || !errors.Is(err, ErrUnavailable) {
			t.Errorf("exit %d = %v, want ErrUnavailable and not ErrNotFound", code, err)
		}
	}
	if err := b.wrapError(&exec.Error{Name: "bd", Err: exec.ErrNotFound}, nil, "", []string{"show"}); !errors.Is(err, ErrNotInstalled) || errors.Is(err, ErrNotFound) {
		t.Errorf("missing bd = %v, want ErrNotInstalled only", err)
	}
}

func TestCLIErrorUnwrapUsesClassifier(t *testing.T) {
	t.Parallel()
	for _, stderr := range infrastructureFailures {
		err := error(&CLIError{Args: []string{"show"}, Stderr: []byte(stderr), Err: codedExit(1)})
		if errors.Is(err, ErrNotFound) {
			t.Errorf("CLIError %q unwraps to ErrNotFound", stderr)
		}
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("CLIError %q does not unwrap to ErrUnavailable", stderr)
		}
	}
	err := error(&CLIError{Args: []string{"show"}, Stderr: []byte("Error: issue gt-9 not found\n"), Err: codedExit(bdNotFoundExit)})
	if !errors.Is(err, ErrNotFound) || errors.Is(err, ErrUnavailable) {
		t.Errorf("bd not-found CLIError: Is(NotFound)=%v Is(Unavailable)=%v", errors.Is(err, ErrNotFound), errors.Is(err, ErrUnavailable))
	}
}
