//go:build !unix

package main

import "errors"

// lockBackupDir refuses to run off unix: the single-run guard is a flock, and a
// run without it could delete the partial of a run it overlaps. Failing here is
// the loud answer; a silent unguarded run is not.
func lockBackupDir(string) (func(), error) {
	return nil, errors.New("trackerbackup needs a unix host: its single-run lock is a flock")
}
