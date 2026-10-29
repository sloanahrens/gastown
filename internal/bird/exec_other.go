//go:build !unix

package bird

import "errors"

// ErrNoExec is what a start fails with where the platform cannot replace the
// process image, so gt bird start refuses rather than running claude beside gt.
var ErrNoExec = errors.New("bird: this platform cannot replace gt with claude")

// execProcess has nothing to replace the process with here.
func execProcess(string, []string, []string) error {
	return ErrNoExec
}
