package testutil

import "testing"

func TestContainer(t *testing.T) {
	t.Parallel()
	RequireDoltContainer(t)
}
