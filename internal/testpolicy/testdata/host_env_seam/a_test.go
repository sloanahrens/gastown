package hostenv

import "testing"

// fakeGetenv stands in for os.Getenv, so the test reads no developer value.
func fakeGetenv(name string) string { return "/fake/" + name }

func fromEnv(getenv func(string) string) string { return getenv("HOME") }

func TestPassesASeam(t *testing.T) {
	t.Parallel()
	if got := fromEnv(fakeGetenv); got != "/fake/HOME" {
		t.Errorf("fromEnv = %q, want the seam's answer", got)
	}
}
