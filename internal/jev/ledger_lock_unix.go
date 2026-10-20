//go:build unix

package jev

import (
	"os"
	"syscall"
)

// lockLedger takes an exclusive flock on f, blocking until every other writer
// of the ledger, in this process or another, lets go.
func lockLedger(f *os.File) (func(), error) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }, nil
}
