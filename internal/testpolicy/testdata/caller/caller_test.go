package caller

import (
	"runtime"
	"testing"
)

func TestA(t *testing.T) {
	t.Parallel()
	_, file, _, _ := runtime.Caller(0)
	_ = file
}
