package git

import (
	"os"
	"os/exec"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
)

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

// SeatLocalEmail returns the seat's own author address, <name>@gastown.local,
// with the name SeatIdentityFor derives from the compound address. It is ""
// when the address names no seat, so an empty name never becomes "@gastown.local".
func SeatLocalEmail(seat string) string {
	return config.SeatLocalEmail(SeatIdentityFor(seat, "").Name)
}

// GlobalAuthorEmail returns the host's global git user.email, "" when unset or
// unreadable. It runs with only HOME and PATH, so a repository's or an ambient
// GIT_* setting does not decide the answer.
func GlobalAuthorEmail() string {
	return globalAuthorEmailFrom(func(name string, args ...string) ([]byte, error) {
		cmd := exec.Command(name, args...) //nolint:gosec // G204: fixed command
		cmd.Env = []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH")}
		return cmd.Output()
	})
}

// globalAuthorEmailFrom asks run for `git config --global user.email` and
// returns the trimmed answer, "" when git fails. run is the seam that keeps the
// subprocess out of the unit test.
func globalAuthorEmailFrom(run func(name string, args ...string) ([]byte, error)) string {
	out, err := run("git", "config", "--global", "user.email")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
