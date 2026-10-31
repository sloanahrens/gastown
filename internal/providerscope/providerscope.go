// Package providerscope decides which providers one seat may use, fixed when
// the seat starts, so a seat cannot reach a provider another seat in the same
// workspace holds (hosted-platform plan row S7, threat model gap 3).
//
// The seat-start path binds each seat's providers and the gateway request
// handler authorizes a request's seat against them; both arrive in later
// wiring beads, so nothing calls this package yet.
package providerscope

import (
	"fmt"
	"strings"
	"sync"
)

// Scope is the set of providers one seat may use, fixed at seat start.
type Scope struct {
	Seat      string
	Providers []string
}

// ErrProviderNotBound is Authorize's refusal: seat may not use provider.
type ErrProviderNotBound struct {
	Seat     string
	Provider string
}

func (e *ErrProviderNotBound) Error() string {
	return fmt.Sprintf("providerscope: provider %q is not bound to seat %q", e.Provider, e.Seat)
}

// ErrSeatAlreadyBound is Bind's refusal: a seat's scope is fixed once bound
// and never widens.
type ErrSeatAlreadyBound struct {
	Seat string
}

func (e *ErrSeatAlreadyBound) Error() string {
	return fmt.Sprintf("providerscope: seat %q is already bound", e.Seat)
}

// Registry holds the scope each seat was bound to.
//
// The zero value is ready to use. A Registry is safe for concurrent use and
// must not be copied after first use.
type Registry struct {
	mu     sync.Mutex
	scopes map[string]Scope
}

// Bind fixes seat's providers for the life of the seat.
//
// Provider names are lower-cased once, when stored. Bind copies providers, so
// the caller cannot widen the scope through the slice it passed. Binding a
// seat that is already bound returns ErrSeatAlreadyBound; an empty seat or an
// empty provider name is an error, and leaves the registry unchanged.
func (r *Registry) Bind(seat string, providers []string) error {
	if seat == "" {
		return fmt.Errorf("providerscope: bind: empty seat")
	}
	scope := Scope{Seat: seat, Providers: make([]string, 0, len(providers))}
	for _, provider := range providers {
		provider = strings.ToLower(provider)
		if provider == "" {
			return fmt.Errorf("providerscope: bind seat %q: empty provider name", seat)
		}
		scope.Providers = append(scope.Providers, provider)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, bound := r.scopes[seat]; bound {
		return &ErrSeatAlreadyBound{Seat: seat}
	}
	if r.scopes == nil {
		r.scopes = make(map[string]Scope)
	}
	r.scopes[seat] = scope
	return nil
}

// Authorize returns nil when seat may use provider, and a *ErrProviderNotBound
// naming both when it may not.
//
// The comparison is exact after one lower-casing of provider and of the names
// Bind stored, and the refusal names the lower-cased provider. A seat that was
// never bound has no providers, so every provider is refused for it.
func (r *Registry) Authorize(seat, provider string) error {
	provider = strings.ToLower(provider)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, bound := range r.scopes[seat].Providers {
		if bound == provider {
			return nil
		}
	}
	return &ErrProviderNotBound{Seat: seat, Provider: provider}
}
