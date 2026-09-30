package gitfake

import "testing"

func TestFakeCrewContract(t *testing.T) {
	t.Parallel()
	RunCrewContract(t, func(t *testing.T) Env { return New() })
}
