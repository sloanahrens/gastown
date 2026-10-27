package hostenv

import (
	"os"
	"testing"
)

func TestDeclared(t *testing.T) {
	t.Parallel()
	//testpolicy:allow no-host-env — the test asserts the value the process was started with
	_ = os.Getenv("HOME")
	//testpolicy:allow no-host-env — the expansion is the input under test
	_, _ = os.UserHomeDir()
}
