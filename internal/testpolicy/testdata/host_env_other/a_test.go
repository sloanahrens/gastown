package hostenv

import (
	"os"
	"testing"
)

// pathEnvVariable names PATH, which is not the developer's own environment.
const pathEnvVariable = "PATH"

func TestOther(t *testing.T) {
	t.Parallel()
	_ = os.Getenv("PATH")
	_ = os.Getenv(pathEnvVariable)
}
