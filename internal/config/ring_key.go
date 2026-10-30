package config

import (
	"errors"
	"fmt"
)

// RingConfig is the "ring" block of settings/config.json: where the key that
// signs the One comes from (gt-6mlik.2, claude-aq9.5). The key is a secret, so
// the file names it by reference, as the agent env and the Dolt password do
// (secrets.go), and the key itself is never written to the file.
type RingConfig struct {
	// Key is a reference to the signing key, "${VAR}" for a variable in the
	// daemon environment (for example "${GT_RING_KEY}"). A literal value is
	// refused when settings are loaded.
	Key string `json:"key,omitempty"`
}

// ValidateRingKey reports whether key is exactly one ${VAR} reference. The
// error never echoes key: a literal in that field is the secret itself.
func ValidateRingKey(key string) error {
	if _, end, ok := envRefAt(key, 0); !ok || end != len(key) {
		return errors.New(`ring.key must be a single ${VAR} reference such as "${GT_RING_KEY}", not a literal value`)
	}
	return nil
}

// RingKeyFrom resolves the signing key through getenv, the daemon environment.
// A key that is unset or not set at all is an error that names the variable
// and never its value, so a caller cannot sign under an empty key.
func (s *TownSettings) RingKeyFrom(getenv func(string) string) (string, error) {
	if s == nil || s.Ring == nil || s.Ring.Key == "" {
		return "", errors.New(`ring.key is not set in settings/config.json: name the signing key as "${GT_RING_KEY}"`)
	}
	if err := ValidateRingKey(s.Ring.Key); err != nil {
		return "", err
	}
	name, _, _ := envRefAt(s.Ring.Key, 0)
	key := expandEnvRefsIn(map[string]string{"ring.key": s.Ring.Key}, getenv)["ring.key"]
	if key == "" {
		return "", fmt.Errorf("ring.key names %s, which is unset or empty in the daemon environment", name)
	}
	return key, nil
}

// checkRingKey refuses a settings file whose ring.key is a literal. An unset
// ring.key is not an error here: the Ring verbs refuse it when they need it.
func (s *TownSettings) checkRingKey() error {
	if s.Ring == nil || s.Ring.Key == "" {
		return nil
	}
	return ValidateRingKey(s.Ring.Key)
}
