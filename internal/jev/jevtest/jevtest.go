// Package jevtest holds test doubles for the Jev client, shared by the tests
// that build a battery's client and check the body it posts.
package jevtest

import (
	"io"
	"net/http"
	"strings"
)

// PostedBody stands in for the network. It keeps the body of every request the
// client posts, counts the requests, and answers 502 Bad Gateway, so a call
// fails with a *jev.StatusError carrying that status. A test checks that
// error to know the request reached the transport.
type PostedBody struct {
	// Calls is how many requests the transport received.
	Calls int
	// Body is the body of the last request received.
	Body string
}

// RoundTrip records the request's body and answers 502 Bad Gateway.
func (p *PostedBody) RoundTrip(r *http.Request) (*http.Response, error) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	p.Calls++
	p.Body = string(b)
	return &http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    r,
	}, nil
}
