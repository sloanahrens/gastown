package shebang

import "testing"

func TestA(t *testing.T) {
	t.Parallel()
	script := "#!/bin/sh\necho hi\n"
	_ = script
}
