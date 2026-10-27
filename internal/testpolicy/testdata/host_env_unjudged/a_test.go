package hostenv

import (
	"os"
	"testing"
)

func name() string { return "PATH" }

func TestUnjudged(t *testing.T) {
	t.Parallel()
	// Neither name is a string constant, so the scan cannot tell whether the
	// read is of the developer's environment.
	variable := "PATH"
	_ = os.Getenv(name())
	_ = os.Getenv(variable)
}
