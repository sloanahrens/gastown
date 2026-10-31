package ring

import (
	"errors"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// ErrKeyNotConfigured: settings/config.json names no ring.key. The message is
// the one RingKeyFrom gives for the same case, so the Ring verbs report it the
// same way.
var ErrKeyNotConfigured = errors.New(`ring.key is not set in settings/config.json: name the signing key as "${GT_RING_KEY}"`)

// KeyFunc resolves the town's signing key. A caller supplies it, so no reader
// of the Ring reads the environment itself.
type KeyFunc func(townRoot string) ([]byte, error)

// TownKey resolves the signing key the way the Ring verbs do: the town's
// settings through config.LoadTownSettings, then RingKeyFrom through getenv,
// the daemon environment in production.
func TownKey(getenv func(string) string) KeyFunc {
	return func(townRoot string) ([]byte, error) {
		settings, err := config.LoadTownSettings(config.TownSettingsPath(townRoot))
		if err != nil {
			return nil, fmt.Errorf("reading settings/config.json: %w", err)
		}
		if settings == nil || settings.Ring == nil || settings.Ring.Key == "" {
			return nil, ErrKeyNotConfigured
		}
		key, err := settings.RingKeyFrom(getenv)
		if err != nil {
			return nil, err
		}
		return []byte(key), nil
	}
}

// Hold answers whether the Ring holds the town's automatic work, and why: the
// reason, or "" when it does not. Every reader that refuses work under the
// Ring calls it, so the rule is stated once here.
//
// A Waned Ring holds, naming its wane time. A ring section that exists but
// cannot be verified holds with the reason: its config cannot be read, its key
// cannot be resolved, or its signature or fields do not verify (gt-ifijm). A
// town with no ring section (or no mayor/town.json at all) holds nothing, and
// neither does a town whose settings name no ring.key. An Absent Ring does not
// hold in this slice: Absent = off lands in slice 9, after the proving step
// (slice 8), so a town that was never forged keeps running until then.
func Hold(townRoot string, key KeyFunc, now time.Time) string {
	return hold(townRoot, config.SectionHost, key, now)
}

// hold is Hold over a section lookup, so a test can point it at a town file
// it wrote itself.
func hold(townRoot string, hostOf func(key string) (host string, ok bool), key KeyFunc, now time.Time) string {
	if townRoot == "" {
		return ""
	}
	raw, err := section(townRoot, hostOf)
	if errors.Is(err, config.ErrNotFound) {
		// A town with no machine config has no ring section, so it holds
		// nothing; a file that exists but cannot be read still holds.
		return ""
	}
	if err != nil {
		return fmt.Sprintf("the Ring cannot be read (%v); treating as held", err)
	}
	if len(raw) == 0 {
		return ""
	}
	k, err := key(townRoot)
	if errors.Is(err, ErrKeyNotConfigured) {
		return ""
	}
	if err != nil {
		return fmt.Sprintf("the Ring cannot be verified (%v); treating as held", err)
	}
	st := classify(raw, k, now)
	switch st.Kind {
	case Valid:
		return ""
	case Waned:
		return fmt.Sprintf("the Ring waned at %s; the town stands by", st.Record.ExpiresAt.UTC().Format(time.RFC3339))
	default:
		return fmt.Sprintf("the Ring cannot be verified (%s); treating as held", st.Reason)
	}
}
