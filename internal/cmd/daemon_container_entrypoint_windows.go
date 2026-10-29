//go:build windows

package cmd

import "errors"

// execEntrypointArgv has no process-replacing exec on Windows. The container
// entrypoint only runs in a Linux container, so this refuses rather than start
// the daemon some other way.
func execEntrypointArgv(argv []string) error {
	return errors.New("gt daemon container-entrypoint execs the daemon, which needs a unix host")
}
