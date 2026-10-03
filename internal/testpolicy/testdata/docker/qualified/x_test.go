package qualified

import (
	"testing"

	"example.com/testutil"
)

func TestStore(t *testing.T) {
	t.Parallel()
	_ = testutil.TakePooledSQLDatabase(t)
}
