package config

import (
	"errors"
	"fmt"
	"strings"
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
// way to read the value. The zero value is not a credential and its Reveal
// reports ok = false, so a caller that dropped the loader's error cannot go on
// as if it held an empty token.
type TrackerAdminToken struct {
	value string
	valid bool
}

// LoadTrackerAdminToken reads the tracker admin credential from the town's
// settings/daemon.env (mode 0600). It reads that file only: the process
// environment is not a fallback for the key itself, so a token exported into a
// shell never becomes the admin credential by accident.
//
// The value is a ${VAR} reference in settings/daemon.env, as item 2 of
// docs/research/tracker/R7-seat-lifecycle.md specifies, resolved the way the
// secret mechanism resolves one (resolveSpawnEnv): from daemon.env first, and
// from this process's environment for a name daemon.env does not define. A
// value that names only itself also takes the process value, never its own
// text.
//
// Every failure names the key, the referenced name or the file and line, and
// never a value: the entry, the line that does not parse and the name it
// resolves to may each be the token.
func LoadTrackerAdminToken(townRoot string) (TrackerAdminToken, error) {
	return loadTrackerAdminToken(townRoot, processHost.getenv)
}

// loadTrackerAdminToken is LoadTrackerAdminToken reading an undefined
// reference through getenv.
func loadTrackerAdminToken(townRoot string, getenv func(string) string) (TrackerAdminToken, error) {
	path := DaemonEnvPath(townRoot)
	env, err := LoadDaemonEnv(townRoot)
	if err != nil {
		return TrackerAdminToken{}, daemonEnvLoadError(path, err)
	}
	raw, ok := env[trackerAdminTokenKey]
	if !ok || strings.TrimSpace(raw) == "" {
		return TrackerAdminToken{}, fmt.Errorf("%s is not set in %s", trackerAdminTokenKey, path)
	}

	value := strings.TrimSpace(resolveTrackerAdminRef(raw, env, getenv))
	if value == "" {
		if refs := envRefNames(raw); len(refs) > 0 {
			return TrackerAdminToken{}, fmt.Errorf("%s in %s refers to %s, which is not set",
				trackerAdminTokenKey, path, strings.Join(refs, ", "))
		}
		return TrackerAdminToken{}, fmt.Errorf("%s in %s is empty", trackerAdminTokenKey, path)
	}
	// A name still braced after one expansion is a reference cycle or a
	// reference chain this loader does not follow. Handing it on would give a
	// caller text where it expects a credential.
	if refs := envRefNames(value); len(refs) > 0 {
		return TrackerAdminToken{}, fmt.Errorf("%s in %s leaves a reference to %s unresolved",
			trackerAdminTokenKey, path, strings.Join(refs, ", "))
	}
	return TrackerAdminToken{value: value, valid: true}, nil
}

// resolveTrackerAdminRef expands raw's ${VAR} references with the secret
// mechanism's lookup: a name daemon.env defines is read from it, any other
// from getenv. The key itself is read from getenv only, so a value that names
// itself takes the injected process value instead of resolving to its own
// text.
func resolveTrackerAdminRef(raw string, fileEnv map[string]string, getenv func(string) string) string {
	return expandEnvRefs(raw, func(name string) string {
		if name != trackerAdminTokenKey {
			if v, ok := fileEnv[name]; ok {
				return v
			}
		}
		return getenv(name)
	})
}

// daemonEnvLoadError describes why settings/daemon.env could not be read,
// naming the key that needed it. A ParseError's message quotes the offending
// line, and that line may be the token itself, so only its file and line
// survive; every other read failure is wrapped as it comes.
func daemonEnvLoadError(path string, err error) error {
	var pe *ParseError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s does not parse at line %d, so %s cannot be read", pe.Path, pe.Line, trackerAdminTokenKey)
	}
	return fmt.Errorf("reading %s for %s: %w", path, trackerAdminTokenKey, err)
}

// Reveal returns the credential and whether the loader produced it. Only a
// host process that issues or revokes a seat token calls it, and the value must
// not reach a seat's environment or argv.
func (t TrackerAdminToken) Reveal() (string, bool) {
	return t.value, t.valid
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
