package config

import (
	"errors"
	"fmt"
)

// trackerAdminTokenKey is the settings/daemon.env name of the tracker admin
// credential. Only host processes that issue or revoke seat tokens read it;
// no seat environment builder does (gt-p85pw.79).
const trackerAdminTokenKey = "TRACKER_ADMIN_TOKEN"

// trackerAdminRedacted is what every formatted view of the credential prints.
const trackerAdminRedacted = "[redacted]"

// TrackerAdminToken is the tracker admin credential. It can issue and revoke
// seat tokens, so no formatting verb, log line or error may show it: String,
// Format and the log package all print a redaction marker. Reveal is the one
// way to read the value.
type TrackerAdminToken struct {
	value string
}

// LoadTrackerAdminToken reads the tracker admin credential from the town's
// settings/daemon.env. It reads that file only: the process environment is
// not a fallback, so a token exported into a shell never becomes the admin
// credential by accident. A missing or empty key is an error that names the
// key and never prints a value.
func LoadTrackerAdminToken(townRoot string) (TrackerAdminToken, error) {
	env, err := LoadDaemonEnv(townRoot)
	if err != nil {
		return TrackerAdminToken{}, daemonEnvLoadError(err)
	}
	value := env[trackerAdminTokenKey]
	if value == "" {
		return TrackerAdminToken{}, fmt.Errorf("%s is not set in %s", trackerAdminTokenKey, DaemonEnvPath(townRoot))
	}
	return TrackerAdminToken{value: value}, nil
}

// daemonEnvLoadError describes a daemon.env that does not parse by file and
// line only. The parse error's message quotes the offending line, and that
// line may be the token itself.
func daemonEnvLoadError(err error) error {
	var pe *ParseError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s does not parse at line %d", pe.Path, pe.Line)
	}
	return err
}

// Reveal returns the credential. Only a host process that issues or revokes a
// seat token calls it, and the value must not reach a seat's environment or argv.
func (t TrackerAdminToken) Reveal() string {
	return t.value
}

// String redacts the credential.
func (t TrackerAdminToken) String() string {
	return trackerAdminRedacted
}

// Format redacts the credential for every verb. Without it, %d and %x would
// print the unexported field.
func (t TrackerAdminToken) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprint(f, trackerAdminRedacted)
}
