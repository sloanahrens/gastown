package calls

import (
	"testing"

	"example.com/testutil"
)

func TestStore(t *testing.T) {
	t.Parallel()
	testutil.RequireDoltContainer(t)
}
