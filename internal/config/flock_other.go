//go:build !unix

package config

import "errors"

// lockConfigFile refuses on platforms without flock: an unlocked
// read-modify-write of a town config file is the lost update this writer
// exists to prevent.
func lockConfigFile(path string) (func(), error) {
	return nil, errors.New("writing town config files needs flock, which this platform lacks: " + path)
}
