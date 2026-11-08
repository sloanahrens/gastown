//go:build !unix

package config

import "errors"

// noFlock is what a lock on this platform refuses with: an unlocked
// read-modify-write of a town config file is the lost update this writer
// exists to prevent.
func noFlock(path string) error {
	return errors.New("writing town config files needs flock, which this platform lacks: " + path)
}

// lockConfigFile refuses on platforms without flock.
func lockConfigFile(path string) (func(), error) {
	return nil, noFlock(path)
}

// lockConfigFileNoWait refuses like lockConfigFile: there is no lock to
// refuse a caller's wait-free write with (gt-yjzepk.4).
func lockConfigFileNoWait(path string) (func(), error) {
	return nil, noFlock(path)
}
