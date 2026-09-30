package wrapper

import (
	"testing"

	"example.com/testutil"
)

// StartServer is an exported wrapper around an entry point: a package that
// calls it starts a container without calling an entry point by name.
func StartServer(t *testing.T) { testutil.RequireDoltContainer(t) }

// unexported wrappers are only reachable from this package's own tests.
func startLocal(t *testing.T) { testutil.RequireDoltContainer(t) }
