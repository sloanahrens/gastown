package hostenv

import (
	"os"
	"testing"
)

func TestOther(t *testing.T) {
	t.Parallel()
	_ = os.Getenv("PATH")
	_ = os.Getenv(name())
}

func name() string { return "PATH" }
