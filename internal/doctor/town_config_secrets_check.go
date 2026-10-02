package doctor

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/townconfig"
)

// TownConfigSecretsCheck warns about agent env values in
// settings/config.json that hold a token in plain text, by key path only
// (gt-y3pgh.5). Tokens belong in settings/daemon.env, referenced by name.
// The town refuses to load them once secrets.refuse_literals is set; then
// TownConfigParseCheck reports the refusal and this check has nothing to say.
// It has no gt doctor fix: moving tokens is gt config secrets migrate, run by the
// operator.
type TownConfigSecretsCheck struct {
	BaseCheck
}

// NewTownConfigSecretsCheck creates the literal-token check.
func NewTownConfigSecretsCheck() *TownConfigSecretsCheck {
	return &TownConfigSecretsCheck{
		BaseCheck: BaseCheck{
			CheckName:        "town-config-secrets",
			CheckDescription: "Check that agent tokens are referenced from settings/daemon.env, not written into settings/config.json",
			CheckCategory:    CategoryConfig,
		},
	}
}

// Run reports each literal token's key path.
func (c *TownConfigSecretsCheck) Run(ctx *CheckContext) *CheckResult {
	town, err := townconfig.Load(ctx.TownRoot)
	if err != nil {
		return &CheckResult{Name: c.Name(), Status: StatusOK, Message: "Town config does not load; see town-config-parse"}
	}
	found := town.LiteralSecrets()
	if len(found) == 0 {
		return &CheckResult{Name: c.Name(), Status: StatusOK, Message: "No literal tokens in settings/config.json"}
	}
	return &CheckResult{
		Name:    c.Name(),
		Status:  StatusWarning,
		Message: fmt.Sprintf("%d literal token(s) in settings/config.json; they reach sessions on the command line", len(found)),
		Details: config.LiteralSecretPaths(found),
		FixHint: "gt config secrets migrate --dry-run, then gt config secrets migrate, then gt config set secrets.refuse_literals true",
	}
}
