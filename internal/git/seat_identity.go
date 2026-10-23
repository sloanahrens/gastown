package git

import "strings"

// SeatIdentity is the git author identity a Gas Town seat's commits carry.
type SeatIdentity struct {
	// Name is the seat's bare agent name: the last segment of its compound
	// address, and the value config.AgentEnv exports as GIT_AUTHOR_NAME.
	Name string
	// Email is the user.email the seat's worktree resolves.
	Email string
}

// SeatIdentityFor returns the author identity a seat's commits carry, from the
// seat's compound address ("gastown/polecats/coma") and the user.email its
// worktree resolves; gt commit authors with it and gt done's rework lease
// compares commits against it, so the wrapper cannot write an identity the
// lease reads as another seat's (gt-tip6u).
func SeatIdentityFor(seat, worktreeEmail string) SeatIdentity {
	seat = strings.TrimSuffix(strings.TrimSpace(seat), "/")
	return SeatIdentity{
		Name:  seat[strings.LastIndex(seat, "/")+1:],
		Email: strings.TrimSpace(worktreeEmail),
	}
}
