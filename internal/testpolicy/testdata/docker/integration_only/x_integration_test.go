//go:build integration

package integrationonly

import (
	"testing"

	"example.com/testutil"
)

func TestIntegrationContainer(t *testing.T) {
	testutil.RequireDoltContainer(t)
}
