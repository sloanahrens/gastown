package hostenv

import (
	"os"
	"testing"
)

// homeEnvVariable names HOME, so a read through it is the same read.
const homeEnvVariable = "HOME"

func TestReads(t *testing.T) {
	t.Parallel()
	_ = os.Getenv("HOME")
	_, _ = os.LookupEnv("SHELL")
	_, _ = os.UserHomeDir()
	_, _ = os.UserConfigDir()
	_, _ = os.UserCacheDir()
	_ = os.Getenv(homeEnvVariable)
}
