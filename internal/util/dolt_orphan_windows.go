//go:build windows

package util

// DoltOrphanServer is a `dolt sql-server` process that looks orphaned.
// On Windows, orphan detection is not supported, so this is a stub definition.
type DoltOrphanServer struct {
	PID        int
	PPID       int
	ConfigPath string
	Age        int
	Reason     string
}

// DoltOrphanReapResult describes what happened when a DoltOrphanServer was signaled.
// On Windows, reaping is a no-op.
type DoltOrphanReapResult struct {
	Process DoltOrphanServer
	Signal  string
	Error   error
}

// FindOrphanDoltServers is a Windows stub.
func FindOrphanDoltServers(townRoot string) ([]DoltOrphanServer, error) {
	return nil, nil
}

// ReapOrphanDoltServers is a Windows stub.
func ReapOrphanDoltServers(orphans []DoltOrphanServer) []DoltOrphanReapResult {
	return nil
}

// FindStaleBeadsTestTempDirs is a Windows stub.
func FindStaleBeadsTestTempDirs() ([]string, error) {
	return nil, nil
}

// RemoveStaleBeadsTestTempDirs is a Windows stub.
func RemoveStaleBeadsTestTempDirs(dirs []string) ([]string, error) {
	return nil, nil
}
