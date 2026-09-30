package gitfake

import "testing"

func TestFakeWorkTreeContract(t *testing.T) {
	t.Parallel()
	RunWorkTreeContract(t, func(t *testing.T) Env { return New() })
}
