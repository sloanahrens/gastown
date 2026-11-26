package forgejoprov

import (
	"fmt"
	"slices"
)

// revokeNamedToken deletes the token named name that user holds, refusing one that
// is the bot's only token. A token's value is never logged.
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
	if err := p.Client.RevokeToken(user, name); err != nil {
		return err
	}
	p.logf("revoked the '%s' token of %s", name, user)
	return nil
}
