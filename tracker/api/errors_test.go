package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestErrorKindsMatchWithErrorsIs(t *testing.T) {
	t.Parallel() // unit tier: pure
	err := fmt.Errorf("show gt-1: %w", NotFound("gt-1"))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("errors.Is(%v, ErrNotFound) = false", err)
	}
	if errors.Is(err, ErrInvalid) {
		t.Errorf("a not_found error matched ErrInvalid")
	}
	var e *Error
	if !errors.As(err, &e) || e.Kind != KindNotFound || e.Message == "" {
		t.Errorf("errors.As gave %+v, want a not_found Error with a message", e)
	}
}

// The five kinds are the wire contract: a client maps them to behavior, so a
// rename here is a breaking change that must show in review.
func TestKindsAreTheFiveOfTheDesign(t *testing.T) {
	t.Parallel()
	got := []string{string(KindNotFound), string(KindGuardFailed), string(KindConflict), string(KindInvalid), string(KindUnavailable)}
	want := []string{"not_found", "guard_failed", "conflict", "invalid", "unavailable"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("kind %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestErrorJSONRoundTrip(t *testing.T) {
	t.Parallel()
	in := GuardFailed("gt-7 is held by someone else")
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Error
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Kind != KindGuardFailed || out.Message != in.Message {
		t.Errorf("round trip = %+v, want %+v (wire: %s)", out, *in, b)
	}
	if want := `{"kind":"guard_failed","message":"gt-7 is held by someone else"}`; string(b) != want {
		t.Errorf("wire form = %s, want %s", b, want)
	}
}

func TestHTTPStatusPerKind(t *testing.T) {
	t.Parallel()
	for kind, want := range map[Kind]int{
		KindNotFound: 404, KindGuardFailed: 409, KindConflict: 409, KindInvalid: 400, KindUnavailable: 503,
	} {
		if got := (&Error{Kind: kind}).HTTPStatus(); got != want {
			t.Errorf("HTTPStatus(%s) = %d, want %d", kind, got, want)
		}
	}
	if got := (&Error{Kind: "bogus"}).HTTPStatus(); got != 500 {
		t.Errorf("HTTPStatus(unknown kind) = %d, want 500", got)
	}
}
