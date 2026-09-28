//go:build integration

package real

import (
	"testing"

	"github.com/steveyegge/gastown/internal/testpolicy/testdata/contracts/goodfake"
)

func TestIntegrationThing(t *testing.T) {
	goodfake.RunThingContract(t)
}
