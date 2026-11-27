package forgejoprov

import (
	"fmt"
	"regexp"
	"strings"
)

// seatRolePrefix is what a seat's role starts with: seat NAME runs as role
// seat-NAME, so it gets a role bot's account and token file.
const seatRolePrefix = "seat-"

// seatName is what a seat may be called: lower-case letters, digits and hyphens.
var seatName = regexp.MustCompile(`^[a-z0-9-]+$`)

// isSeatRole reports whether role is exactly the role ProvisionSeat builds: the
// prefix followed by a seat name. The role table names no seat, so EnsureToken
// asks this before it gives a seat the polecat scopes; a string that merely
// starts with the prefix is still an unknown role.
func isSeatRole(role string) bool {
	name, ok := strings.CutPrefix(role, seatRolePrefix)
	return ok && seatName.MatchString(name)
}

// ProvisionSeat ensures the seat bot named name's account, token file
// forgejo-seat-name.env and write access on each repository, and nothing else.
func (p *Provisioner) ProvisionSeat(name string, repos []string) error {
	if !seatName.MatchString(name) {
		return fmt.Errorf("seat name %q must match [a-z0-9-]+: nothing was sent", name)
	}
	role, user := seatRolePrefix+name, p.BotPrefix+seatRolePrefix+name
	// The account and its token are one step: SkipTokens skips the mint, as provision
	// does, and a failure of either names the seat bot.
	var err error
	if err = p.EnsureUser(role); err == nil && !p.SkipTokens {
		err = p.EnsureToken(role)
	}
	if err != nil {
		return fmt.Errorf("seat bot %s: %w", user, err)
	}
	for _, repo := range repos {
		if err := p.EnsureCollaborator(role, repo, permissionWrite); err != nil {
			return fmt.Errorf("seat bot %s: write access on %s: %w", user, repo, err)
		}
	}
	return nil
}
