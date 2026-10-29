package seattoken

import (
	"context"
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
	cfg, err := hostClientConfig(townRoot)
	if err != nil {
		return nil, err
	}
	tc, err := client.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("seattoken: tracker client: %w", err)
	}
	return NewTrackerIssuer(tc), nil
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
	// The loader answers a valid credential or an error, never a zero one, and
	// client.New refuses an empty token rather than issuing without one.
	secret, _ := token.Reveal()
	return client.Config{BaseURL: endpoint.URL(), Token: secret}, nil
}
