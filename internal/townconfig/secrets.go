package townconfig

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
)

// holdVerb is the verb the literal-token message needs for n tokens.
func holdVerb(n int) string {
	if n == 1 {
		return "holds"
	}
	return "hold"
}

// LiteralSecrets lists the agent env values in settings/config.json that
// hold a token in plain text, by location only. It is empty when the town
// refuses literals (Load fails instead) or has none.
func (t *Town) LiteralSecrets() []config.LiteralSecret {
	return append([]config.LiteralSecret(nil), t.literalSecrets...)
}

// LiteralSecretsWarning is the one-line warning for t's literal tokens, or ""
// when it has none.
func (t *Town) LiteralSecretsWarning() string {
	found := t.literalSecrets
	if len(found) == 0 {
		return ""
	}
	return fmt.Sprintf("warning: %s holds %d literal token(s) (%s); move them to %s with 'gt config secrets migrate'",
		FileSettings, len(found), strings.Join(config.LiteralSecretPaths(found), ", "), FileDaemonEnv)
}
