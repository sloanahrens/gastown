package seattoken

import (
	"context"
	"errors"
	"fmt"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/client"
)

// TrackerIssuer issues seat tokens over the tracker's admin route, so a host
// process needs no database password to hand a seat its own token.
type TrackerIssuer struct{ tc *client.Client }

// NewTrackerIssuer returns an issuer whose client carries the admin credential.
func NewTrackerIssuer(tc *client.Client) *TrackerIssuer { return &TrackerIssuer{tc: tc} }

// Issue returns a fresh seat-scoped token for actor; the tracker keeps only its
// hash, so this answer is the one copy in the clear.
func (i *TrackerIssuer) Issue(ctx context.Context, actor string) (string, error) {
	issued, err := i.tc.IssueToken(ctx, actor, api.ScopeSeat)
	if err != nil {
		return "", fmt.Errorf("the tracker issued no seat token: %w", err)
	}
	return issued.Token, nil
}

// NewHostIssuer returns the issuer a host process uses: the admin credential
// from settings/daemon.env, over the endpoint mayor/town.json names. Neither
// reaches a seat (gt-p85pw.79).
func NewHostIssuer(townRoot string) (*TrackerIssuer, error) {
	tc, err := newHostClient(townRoot)
	if err != nil {
		return nil, err
	}
	return NewTrackerIssuer(tc), nil
}

// TrackerRevoker ends a seat's tracker tokens over the admin route, so a seat
// that is gone can no longer authenticate with a token it was given
// (gt-p85pw.85).
type TrackerRevoker struct{ tc *client.Client }

// NewTrackerRevoker returns a revoker whose client carries the admin credential.
func NewTrackerRevoker(tc *client.Client) *TrackerRevoker { return &TrackerRevoker{tc: tc} }

// Revoke ends every live token of actor; an actor holding none is a success.
func (r *TrackerRevoker) Revoke(ctx context.Context, actor string) error {
	if _, err := r.tc.RevokeActorTokens(ctx, actor); err != nil {
		// The failure may be a refusal or a call that never reached the tracker,
		// so the error names the ask and not an outcome neither side can vouch
		// for.
		return fmt.Errorf("revoking the tracker tokens of %s: %w", actor, err)
	}
	return nil
}

// NewHostRevoker returns the revoker a host process uses, on the same admin
// credential and endpoint as NewHostIssuer (gt-p85pw.79).
func NewHostRevoker(townRoot string) (*TrackerRevoker, error) {
	tc, err := newHostClient(townRoot)
	if err != nil {
		return nil, err
	}
	return NewTrackerRevoker(tc), nil
}

// newHostClient is the admin client the host issuer and the host revoker share.
func newHostClient(townRoot string) (*client.Client, error) {
	cfg, err := hostClientConfig(townRoot)
	if err != nil {
		return nil, err
	}
	tc, err := client.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("seattoken: tracker client: %w", err)
	}
	return tc, nil
}

// hostClientConfig assembles the admin client's config: the endpoint from
// durable town config, the credential from the town's daemon.env.
func hostClientConfig(townRoot string) (client.Config, error) {
	endpoint, err := config.ResolveTrackerEndpoint(townRoot)
	if err != nil {
		return client.Config{}, err
	}
	token, err := config.LoadTrackerAdminToken(townRoot)
	if err != nil {
		return client.Config{}, err
	}
	return adminClientConfig(endpoint.URL(), token)
}

// adminClientConfig pairs the endpoint with the admin credential. A token the
// loader did not produce is refused here, so no client is built around an empty
// secret; the error names the credential and never its value.
func adminClientConfig(baseURL string, token config.TrackerAdminToken) (client.Config, error) {
	secret, ok := token.Reveal()
	if !ok {
		return client.Config{}, errors.New("seattoken: the tracker admin credential is not valid")
	}
	return client.Config{BaseURL: baseURL, Token: secret}, nil
}
