package hostenv

import (
	"os"
	"testing"
)

// fromEnv and homeDir are the seams a test hands a unit instead of letting it
// read the process environment itself.
func fromEnv(getenv func(string) string) string { return getenv("PATH") }

func lookupEnv(lookup func(string) (string, bool)) (string, bool) { return lookup("PATH") }

func homeDir(dir func() (string, error)) (string, error) { return dir() }

// seams is the struct a test fills to inject the process's own readers.
type seams struct {
	getenv func(string) string
	home   func() (string, error)
}

// hostSeams returns the readers as a composite literal's values.
func hostSeams() seams {
	return seams{getenv: os.Getenv, home: os.UserHomeDir}
}

// hostLookup returns a reader to its caller as a value.
func hostLookup() func(string) (string, bool) {
	return os.LookupEnv
}

// hostAssigned assigns a reader to a local before handing it on.
func hostAssigned() func(string) string {
	getenv := os.Getenv
	return getenv
}

// hostConfigDir holds a reader in a package variable.
var hostConfigDir = os.UserConfigDir

func TestPassesHostEnvAsAValue(t *testing.T) {
	t.Parallel()
	_ = fromEnv(os.Getenv)
	_, _ = lookupEnv(os.LookupEnv)
	_, _ = homeDir(os.UserHomeDir)
	_, _ = homeDir(os.UserConfigDir)
	_, _ = homeDir(os.UserCacheDir)
}
