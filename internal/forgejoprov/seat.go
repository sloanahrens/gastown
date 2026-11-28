package forgejoprov

import (
	"fmt"
	"regexp"
	"strings"
)

// seatRolePrefix is what a seat's role starts with: seat NAME runs as role
// seat-NAME.
const seatRolePrefix = "seat-"

// maxSeatNameLength is the longest seat name, in characters.
const maxSeatNameLength = 31

// seatName is the form a seat name must have: lower-case letters and digits in
// groups joined by single hyphens.
var seatName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// seatNameValid reports whether name is a seat name openshellprov also accepts,
// so every seat bot this package creates can have its provider.
func seatNameValid(name string) bool {
	return len(name) <= maxSeatNameLength && seatName.MatchString(name)
}

// isSeatRole reports whether role is exactly the role ProvisionSeat builds: the
// prefix followed by a seat name. The role table names no seat, so EnsureToken
// asks this before it gives a seat the polecat scopes; a string that merely
// starts with the prefix is still an unknown role.
func isSeatRole(role string) bool {
	name, ok := strings.CutPrefix(role, seatRolePrefix)
	return ok && seatNameValid(name)
}

// ProvisionSeat ensures the seat bot named name's account, token file
// forgejo-seat-name.env and write access on each repository, and nothing else.
func (p *Provisioner) ProvisionSeat(name string, repos []string) error {
	if !seatNameValid(name) {
		return fmt.Errorf("seat name %q must be at most %d characters of lower-case letters and digits in groups joined by single hyphens: nothing was sent", name, maxSeatNameLength)
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
