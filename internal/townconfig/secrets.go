package townconfig

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
)

// checkLiteralSecrets records the literal tokens in s's agent env blocks
// (gt-y3pgh.5). With "secrets": {"refuse_literals": true} they are a load
// error, so the daemon, gt up and every session start refuse until they are
// moved to settings/daemon.env; otherwise LiteralSecrets reports them and
// the callers warn. Neither names a value, only its key path.
func (t *Town) checkLiteralSecrets(s *config.TownSettings) error {
	found := config.FindLiteralSecrets(s)
	if len(found) > 0 && s.RefusesLiteralSecrets() {
		return fmt.Errorf("%s: secrets.refuse_literals is set and %s %s a literal token; move it to %s with 'gt config secrets migrate'",
			t.path(FileSettings), strings.Join(config.LiteralSecretPaths(found), ", "), holdVerb(len(found)), FileDaemonEnv)
	}
	t.literalSecrets = found
	return nil
}

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
