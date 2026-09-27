package allownoreason

import (
	"testing"
	"time"
)

func TestA(t *testing.T) {
	t.Parallel()
	time.Sleep(time.Millisecond) //testpolicy:allow no-sleep
}
