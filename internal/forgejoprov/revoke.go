package forgejoprov

import (
	"fmt"
	"slices"
)

// revokeNamedToken deletes the token named name that user holds, refusing one that
// is the bot's only token. A dry run reports the revoke it would send and sends none.
// A token's value is never logged.
func (p *Provisioner) revokeNamedToken(user, name string) error {
	names, err := p.Client.ListTokenNames(user)
	if err != nil {
		return err
	}
	if !slices.Contains(names, name) {
		return fmt.Errorf("%s holds no token named '%s'; nothing was revoked: %w", user, name, ErrNoToken)
	}
	// A bot with no token cannot act, so the last one is refused rather than removed.
	if len(names) == 1 {
		return fmt.Errorf("%s holds only the '%s' token; revoking it would leave the bot with no token, so nothing was revoked", user, name)
	}
	// The client answers a dry run's non-GET without sending it, and Client.RevokeToken
	// reads that answer as a refusal, so the dry run stops here and logs the revoke it
	// would send. The refusals above still apply: they are what a live run would answer.
	if p.Client.DryRun {
		p.logf("would revoke the '%s' token of %s", name, user)
		return nil
	}
	if err := p.Client.RevokeToken(user, name); err != nil {
		return err
	}
	p.logf("revoked the '%s' token of %s", name, user)
	return nil
}
