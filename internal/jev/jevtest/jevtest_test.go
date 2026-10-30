package jevtest

import (
	"errors"
	"io"
	"net/http"
	"testing"
)

// unreadable is a request body that fails on every read.
type unreadable struct{}

func (unreadable) Read([]byte) (int, error) { return 0, errors.New("body unreadable") }

// TestPostedBodyRefusesAnUnreadableBody: a request whose body cannot be read
// returns that error and counts as no request, so a test cannot take a failed
// read for a request the transport received.
func TestPostedBodyRefusesAnUnreadableBody(t *testing.T) {
	t.Parallel()
	p := &PostedBody{}
	r, err := http.NewRequest(http.MethodPost, "http://jev.test/v1/systemone", io.NopCloser(unreadable{}))
	if err != nil {
		t.Fatal(err)
	}

	resp, err := p.RoundTrip(r)

	if err == nil {
		t.Fatal("RoundTrip returned no error for an unreadable body")
	}
	if resp != nil {
		t.Errorf("RoundTrip returned a response %v with the error", resp.StatusCode)
	}
	if p.Calls != 0 {
		t.Errorf("Calls = %d, want 0 for a request that was not read", p.Calls)
	}
}
