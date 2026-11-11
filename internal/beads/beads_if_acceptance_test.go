package beads

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// exitStatusErr is an error that carries a bd exit status, as *exec.ExitError
// does, so the runner can answer with any exit code.
type exitStatusErr int

func (e exitStatusErr) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e exitStatusErr) ExitCode() int { return int(e) }

// TestUpdateIfAcceptanceSendsTheGuardWithTheBlock: the guard reaches bd as
// --if-acceptance next to the criteria it guards, and bd's exit 13 is read as
// ErrGuardNotHeld, the write refused with nothing written. Any other failure
// stays a failure.
func TestUpdateIfAcceptanceSendsTheGuardWithTheBlock(t *testing.T) {
	t.Parallel()
	block := "- [x] first\n- [ ] second"
	const guard = "5f1c0d2e"
	for _, tc := range []struct {
		name    string
		exit    error
		wantErr bool
		held    bool
	}{
		{name: "guard held", exit: nil},
		{name: "guard no longer held", exit: exitStatusErr(13), wantErr: true, held: true},
		{name: "other failure", exit: exitStatusErr(1), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var argv []string
			b := New(t.TempDir(), WithRunner(func(_ context.Context, args []string) ([]byte, []byte, error) {
				argv = args
				if tc.exit == nil {
					return nil, nil, nil
				}
				return nil, []byte("precondition failed"), tc.exit
			}))
			err := b.Update("gt-guard", UpdateOptions{Acceptance: &block, IfAcceptance: guard})
			if (err != nil) != tc.wantErr {
				t.Fatalf("Update error = %v, want error %v", err, tc.wantErr)
			}
			if errors.Is(err, ErrGuardNotHeld) != tc.held {
				t.Errorf("Update error = %v, want ErrGuardNotHeld %v", err, tc.held)
			}
			joined := strings.Join(argv, " ")
			for _, want := range []string{"--if-acceptance=" + guard, "--acceptance=" + block} {
				if !strings.Contains(joined, want) {
					t.Errorf("bd argv %q lacks %q", joined, want)
				}
			}
		})
	}
}

// TestUpdateIfAcceptanceNeedsAcceptance: a guard with no criteria write to
// guard is refused before bd runs, so no write goes out unguarded.
func TestUpdateIfAcceptanceNeedsAcceptance(t *testing.T) {
	t.Parallel()
	ran := false
	b := New(t.TempDir(), WithRunner(func(context.Context, []string) ([]byte, []byte, error) {
		ran = true
		return nil, nil, nil
	}))
	err := b.Update("gt-guard", UpdateOptions{IfAcceptance: "5f1c0d2e"})
	if err == nil || !strings.Contains(err.Error(), "IfAcceptance needs Acceptance") {
		t.Fatalf("Update error = %v, want the refusal naming the missing Acceptance", err)
	}
	if ran {
		t.Error("bd ran for a guard with no criteria write")
	}
}
