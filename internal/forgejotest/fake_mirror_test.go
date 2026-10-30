package forgejotest

import (
	"testing"

	"github.com/steveyegge/gastown/internal/forgejoprov"
)

// TestFakeMirrorsProductionDefaults checks that the bot prefix, the bot email domain and
// the token name the fake uses are the provisioning run's defaults. The fake cannot import
// the run, so the copies are held to the run's values here.
func TestFakeMirrorsProductionDefaults(t *testing.T) {
	t.Parallel()
	if botPrefix != forgejoprov.DefaultBotPrefix {
		t.Errorf("fake bot prefix %q, production default %q", botPrefix, forgejoprov.DefaultBotPrefix)
	}
	if botEmailDomain != forgejoprov.DefaultBotEmailDomain {
		t.Errorf("fake bot email domain %q, production default %q", botEmailDomain, forgejoprov.DefaultBotEmailDomain)
	}
	if fixtureTokenName != forgejoprov.DefaultTokenName {
		t.Errorf("fake token name %q, production default %q", fixtureTokenName, forgejoprov.DefaultTokenName)
	}
}
