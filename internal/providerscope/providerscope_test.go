package providerscope

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// A seat reaches only the providers bound to it, and a seat that was never
// bound reaches nothing (hosted-platform plan row S7, threat model gap 3).
func TestOtherSeatsProviderRefused(t *testing.T) {
	t.Parallel()
	var reg Registry
	if err := reg.Bind("seat-a", []string{"p1"}); err != nil {
		t.Fatalf("Bind(seat-a): %v", err)
	}
	for _, tc := range []struct{ name, seat, provider string }{
		{"a provider no seat holds, asked of a bound seat", "seat-a", "p2"},
		{"a provider another seat holds, asked of an unbound seat", "seat-b", "p1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := reg.Authorize(tc.seat, tc.provider)
			var notBound *ErrProviderNotBound
			if !errors.As(err, &notBound) {
				t.Fatalf("Authorize(%q, %q) = %v, want *ErrProviderNotBound", tc.seat, tc.provider, err)
			}
			if notBound.Seat != tc.seat || notBound.Provider != tc.provider {
				t.Errorf("Authorize(%q, %q) refused naming seat %q and provider %q, want the request's own %q and %q",
					tc.seat, tc.provider, notBound.Seat, notBound.Provider, tc.seat, tc.provider)
			}
		})
	}
	if err := reg.Authorize("seat-a", "p1"); err != nil {
		t.Errorf("Authorize(seat-a, p1) = %v, want nil: the seat's own provider is allowed", err)
	}
}

// A second Bind cannot widen the first one's scope: the attempt is an error
// and the seat keeps only the providers its start gave it.
func TestBindTwiceIsError(t *testing.T) {
	t.Parallel()
	var reg Registry
	if err := reg.Bind("seat-a", []string{"p1"}); err != nil {
		t.Fatalf("first Bind(seat-a): %v", err)
	}

	err := reg.Bind("seat-a", []string{"p1", "p2"})
	var already *ErrSeatAlreadyBound
	if !errors.As(err, &already) {
		t.Fatalf("second Bind(seat-a) = %v, want *ErrSeatAlreadyBound", err)
	}
	if already.Seat != "seat-a" {
		t.Errorf("refusal names seat %q, want %q", already.Seat, "seat-a")
	}

	if err := reg.Authorize("seat-a", "p1"); err != nil {
		t.Errorf("Authorize(seat-a, p1) = %v, want nil: the first scope stands", err)
	}
	if err := reg.Authorize("seat-a", "p2"); err == nil {
		t.Error("Authorize(seat-a, p2) = nil: the refused Bind widened the scope")
	}
}

// Provider names compare exactly after one lower-casing, on both sides.
func TestAuthorizeIsCaseInsensitiveOnName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		providers []string
		request   string
		want      bool
	}{
		{"a lower-case binding takes an upper-case request", []string{"p1"}, "P1", true},
		{"an upper-case binding takes a lower-case request", []string{"P1"}, "p1", true},
		{"a mixed-case binding takes an upper-case request", []string{"OpenAI"}, "OPENAI", true},
		{"a name that differs by more than case is refused", []string{"p1"}, "p10", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var reg Registry
			if err := reg.Bind("seat-a", tc.providers); err != nil {
				t.Fatalf("Bind(seat-a, %q): %v", tc.providers, err)
			}
			err := reg.Authorize("seat-a", tc.request)
			if got := err == nil; got != tc.want {
				t.Errorf("Authorize(seat-a, %q) after Bind(%q) = %v, want allowed %v", tc.request, tc.providers, err, tc.want)
			}
			var notBound *ErrProviderNotBound
			if !tc.want && (!errors.As(err, &notBound) || notBound.Provider != tc.request) {
				t.Errorf("Authorize(seat-a, %q) refused with %v, want *ErrProviderNotBound naming %q", tc.request, err, tc.request)
			}
		})
	}
}

// Bind copies its input, so a caller cannot widen a bound scope by mutating
// the slice it passed.
func TestBindCopiesProviders(t *testing.T) {
	t.Parallel()
	var reg Registry
	providers := []string{"p1"}
	if err := reg.Bind("seat-a", providers); err != nil {
		t.Fatalf("Bind(seat-a): %v", err)
	}

	providers[0] = "p2"

	if err := reg.Authorize("seat-a", "p2"); err == nil {
		t.Error("Authorize(seat-a, p2) = nil: the caller's slice widened the scope after Bind")
	}
	if err := reg.Authorize("seat-a", "p1"); err != nil {
		t.Errorf("Authorize(seat-a, p1) = %v, want nil: the bound name was overwritten", err)
	}
}

// An empty seat or provider name is refused, and a refused Bind leaves the
// seat free for the seat-start path to bind again.
func TestBindRefusesEmptyNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		seat      string
		providers []string
	}{
		{"an empty seat", "", []string{"p1"}},
		{"an empty provider name", "seat-a", []string{"p1", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var reg Registry
			if err := reg.Bind(tc.seat, tc.providers); err == nil {
				t.Fatalf("Bind(%q, %q) = nil, want an error", tc.seat, tc.providers)
			}
			if tc.seat == "" {
				return
			}
			if err := reg.Bind("seat-a", []string{"p1"}); err != nil {
				t.Fatalf("Bind after a refused Bind = %v, want nil: the refused call must leave the seat unbound", err)
			}
		})
	}
}

// Concurrent Bind and Authorize calls keep the registry consistent: distinct
// seats bind and authorize in parallel, and one seat bound by many goroutines
// at once is bound by exactly one.
func TestRegistryIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	var reg Registry
	const seats = 8
	var wg sync.WaitGroup
	for i := 0; i < seats; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			seat := fmt.Sprintf("seat-%d", i)
			if err := reg.Bind(seat, []string{"p1"}); err != nil {
				t.Errorf("Bind(%q): %v", seat, err)
				return
			}
			if err := reg.Authorize(seat, "P1"); err != nil {
				t.Errorf("Authorize(%q, P1) = %v, want nil", seat, err)
			}
			if err := reg.Authorize(seat, "p2"); err == nil {
				t.Errorf("Authorize(%q, p2) = nil, want *ErrProviderNotBound", seat)
			}
		}()
	}
	wg.Wait()

	var winners atomic.Int32
	for i := 0; i < seats; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := reg.Bind("seat-race", []string{"p1"}); err == nil {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Errorf("concurrent Bind of one seat succeeded %d times, want 1", got)
	}
}
