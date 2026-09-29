package doctor

import (
	"errors"
	"testing"
)

func TestErrCannotFix(t *testing.T) {
	t.Parallel()
	// Test that ErrCannotFix is defined and has expected message
	if ErrCannotFix == nil {
		t.Fatal("ErrCannotFix should not be nil")
	}

	expected := "check does not support auto-fix"
	if ErrCannotFix.Error() != expected {
		t.Errorf("ErrCannotFix.Error() = %q, want %q", ErrCannotFix.Error(), expected)
	}
}

func TestErrCannotFixIsError(t *testing.T) {
	t.Parallel()
	// Verify ErrCannotFix implements the error interface correctly
	var err error = ErrCannotFix
	if err == nil {
		t.Fatal("ErrCannotFix should implement error interface")
	}

	// Test errors.Is compatibility
	if !errors.Is(ErrCannotFix, ErrCannotFix) {
		t.Error("errors.Is should return true for ErrCannotFix")
	}
}
