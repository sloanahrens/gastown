// Package signing is the keychain unlock and codesign step both install paths
// run before a built binary replaces the installed one. The install paths keep
// their own material checks, password read and log lines; the bounds and the
// restore after a failed codesign live here once, so the binaries cannot drift
// apart again (gt-e2prc).
package signing

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Exec runs one command under ctx and returns what it wrote to standard output
// and standard error.
type Exec func(ctx context.Context, name string, args ...string) (stdout, stderr string, err error)

// ErrUnlockTimeout is an unlock the deadline cut off, as distinct from one that
// was refused.
var ErrUnlockTimeout = errors.New("unlock timed out")

// CodesignError is codesign failing or timing out. Notice is the first line of
// what codesign said, for the log: an error message must not carry command
// output, which can hold a secret.
type CodesignError struct {
	Notice string
}

func (CodesignError) Error() string { return "codesign failed" }

// Bound returns parent with a deadline limit after now. A nil now is the wall
// clock.
func Bound(parent context.Context, now func() time.Time, limit time.Duration) (context.Context, context.CancelFunc) {
	if now == nil {
		now = time.Now
	}
	return context.WithDeadline(parent, now().Add(limit))
}

// Unlock unlocks keychain with password, bounded by ctx. The password travels
// in argv, where ps can see it while the unlock runs; moving it off argv is a
// separate decision (gt-tpg0p.70). The unlock's output is discarded, so the
// password is never printed.
func Unlock(ctx context.Context, exec Exec, keychain, password string) error {
	_, _, err := exec(ctx, "security", "unlock-keychain", "-p", password, OptionSafe(keychain))
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		return ErrUnlockTimeout
	}
	return err
}

// Attempt runs one codesign and reports its output and error. A caller bounds it
// as it needs: Codesign uses a context deadline, the installer uses timeout(1)
// argv.
type Attempt func() (stdout, stderr string, err error)

// Codesign signs path under identifier with identity, bounded by ctx. A failed
// codesign runs restore before it returns, because a codesign that dies
// mid-write leaves half a binary at path. A failed restore is returned as it
// is, with path in neither state, and the caller must not install it.
func Codesign(ctx context.Context, exec Exec, keychain, identity, identifier, path string, restore func() error) error {
	return CodesignWith(func() (string, string, error) {
		return exec(ctx, "codesign", CodesignArgs(keychain, identity, identifier, path)...)
	}, restore)
}

// CodesignArgs is the argv of one codesign of path under identifier with
// identity, the arguments after the command name. Both codesign calls, the
// shared one and the installer's argv-bounded one, build their argv here, so the
// keychain and the path reach codesign through OptionSafe whichever bound wraps
// them.
func CodesignArgs(keychain, identity, identifier, path string) []string {
	return []string{"--force", "--keychain", OptionSafe(keychain), "-s", identity, "-i", identifier, OptionSafe(path)}
}

// CodesignWith is Codesign over a caller-bounded attempt: a failed attempt runs
// restore before it returns, and is reported as a CodesignError.
func CodesignWith(attempt Attempt, restore func() error) error {
	stdout, stderr, err := attempt()
	if err == nil {
		return nil
	}
	if restoreErr := restore(); restoreErr != nil {
		return restoreErr
	}
	out := stderr
	if out == "" {
		out = stdout
	}
	return CodesignError{Notice: FirstLine(out)}
}

// OptionSafe returns path so a command cannot read it as an option: a path that
// starts with a dash is returned from the current directory. codesign, security,
// chflags and chattr all take a path this way.
func OptionSafe(path string) string {
	if strings.HasPrefix(path, "-") {
		return "./" + path
	}
	return path
}

// FirstLine is the first line of what a command said, the script's `head -1`,
// or "no output" when it said nothing.
func FirstLine(s string) string {
	if s == "" {
		return "no output"
	}
	line, _, _ := strings.Cut(s, "\n")
	return line
}
