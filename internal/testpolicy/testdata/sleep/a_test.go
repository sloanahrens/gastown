package sleep

import (
	"testing"
	"time"
)

func TestA(t *testing.T) {
	t.Parallel()
	time.Sleep(time.Millisecond)
}
