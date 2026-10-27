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

func TestPassesHostEnvAsAValue(t *testing.T) {
	t.Parallel()
	_ = fromEnv(os.Getenv)
	_, _ = lookupEnv(os.LookupEnv)
	_, _ = homeDir(os.UserHomeDir)
	_, _ = homeDir(os.UserConfigDir)
	_, _ = homeDir(os.UserCacheDir)
}
