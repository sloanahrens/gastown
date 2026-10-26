package hostenv

import (
	"os"
	"testing"
)

func TestReads(t *testing.T) {
	t.Parallel()
	_ = os.Getenv("HOME")
	_, _ = os.LookupEnv("SHELL")
	_, _ = os.UserHomeDir()
	_, _ = os.UserConfigDir()
	_, _ = os.UserCacheDir()
}
