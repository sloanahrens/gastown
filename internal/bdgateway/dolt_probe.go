package bdgateway

import (
	"context"
	"errors"
	"net"
)

// NewDoltProbe builds the readiness check that reports whether Dolt answers at
// host:port. One TCP dial and a close is the whole answer: the calls themselves
// run bd against that endpoint, and readiness must not start one. dial is a
// seam, so a test drives each answer without a server listening.
//
// A nil dial, and a dial that answers with no connection and no error, are
// refused rather than dereferenced: the probe returns an error, so /readyz
// refuses instead of the request panicking or passing.
func NewDoltProbe(host, port string, dial func(ctx context.Context, network, addr string) (net.Conn, error)) func(context.Context) error {
	if dial == nil {
		return func(context.Context) error {
			return errors.New("bdgateway: no dialer for the Dolt probe")
		}
	}
	addr := net.JoinHostPort(host, port)
	return func(ctx context.Context) error {
		conn, err := dial(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		if conn == nil {
			return errors.New("bdgateway: the Dolt probe's dialer answered with no connection")
		}
		// The dial answered, which is the probe's whole question. The connection
		// carries nothing further, and a close that fails says nothing about
		// Dolt, so it is not read as the probe's result.
		_ = conn.Close()
		return nil
	}
}
