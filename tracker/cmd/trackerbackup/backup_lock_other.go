//go:build !unix

package main

// lockBackupDir has no lock off unix: the single-run guard relies on flock, so
// a run there proceeds without one and says nothing about it.
func lockBackupDir(string) (func(), error) {
	return func() {}, nil
}
