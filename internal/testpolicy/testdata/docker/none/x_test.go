package none

import "testing"

// RequireDoltContainer in a comment or a string is not a call.
func TestNothing(t *testing.T) {
	t.Parallel()
	_ = "testutil.RequireDoltContainer(t)"
}
