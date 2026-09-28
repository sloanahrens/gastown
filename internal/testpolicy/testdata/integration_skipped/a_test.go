//go:build integration

package integrationskipped

import (
	"testing"
	"time"
)

func TestA(t *testing.T) {
	t.Parallel()
	time.Sleep(time.Millisecond)
}
