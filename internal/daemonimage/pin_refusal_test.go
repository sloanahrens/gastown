package daemonimage

import (
	"errors"
	"fmt"
	"testing"
)

func TestPinRefusalKeepsItsTextAndIsTyped(t *testing.T) {
	t.Parallel()
	err := Refusef("bd tip %s lacks it", "abc")
	if got, want := err.Error(), "bd tip abc lacks it"; got != want {
		t.Fatalf("Refusef text = %q, want %q", got, want)
	}
	if !errors.Is(err, ErrPinRefused) {
		t.Fatalf("errors.Is(Refusef(...), ErrPinRefused) = false, want true")
	}
	wrapped := fmt.Errorf("x: %w", fmt.Errorf("x: %w", err))
	if !errors.Is(wrapped, ErrPinRefused) {
		t.Fatalf("errors.Is(twice-wrapped refusal, ErrPinRefused) = false, want true")
	}
	if errors.Is(errors.New("bd tip abc lacks it"), ErrPinRefused) {
		t.Fatalf("a plain errors.New with the same text matched ErrPinRefused, want no match")
	}
}
