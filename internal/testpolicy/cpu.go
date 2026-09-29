package testpolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// CPUDirEnv names the directory where the budget runner's exec wrapper
// records each test binary's CPU time. The runner sets it for `go test`; the
// wrapper removes it from the test binary's environment.
const CPUDirEnv = "TESTPOLICY_CPU_DIR"

// CPUTime is the CPU a package's test process tree used: the test binary and
// every descendant it waited for (git, bd, a re-exec'd helper), as wait4
// reports it when the test binary exits.
type CPUTime struct {
	User, Sys time.Duration
}

// CPUSource returns the CPU time the test binary of pkg (module-relative)
// used, and false when there is no measurement for it.
type CPUSource func(pkg string) (CPUTime, bool)

type cpuRecord struct {
	Dir     string  `json:"dir"`
	UserSec float64 `json:"user_sec"`
	SysSec  float64 `json:"sys_sec"`
}

// cpuFile is the record path for the test binary that ran in pkgDir. `go test`
// runs each test binary in its package directory, so the directory names the
// package on both sides.
func cpuFile(dir, pkgDir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(pkgDir)))
	return filepath.Join(dir, hex.EncodeToString(sum[:12])+".json")
}

// WriteCPU records the CPU time of the test binary that ran in pkgDir.
func WriteCPU(dir, pkgDir string, c CPUTime) error {
	b, err := json.Marshal(cpuRecord{filepath.Clean(pkgDir), c.User.Seconds(), c.Sys.Seconds()})
	if err != nil {
		return err
	}
	return os.WriteFile(cpuFile(dir, pkgDir), b, 0o644)
}

// ReadCPU returns the CPU time recorded for the test binary that ran in
// pkgDir, and false when none was recorded.
func ReadCPU(dir, pkgDir string) (CPUTime, bool, error) {
	b, err := os.ReadFile(cpuFile(dir, pkgDir))
	if errors.Is(err, fs.ErrNotExist) {
		return CPUTime{}, false, nil
	}
	if err != nil {
		return CPUTime{}, false, err
	}
	var r cpuRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return CPUTime{}, false, err
	}
	sec := func(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }
	return CPUTime{User: sec(r.UserSec), Sys: sec(r.SysSec)}, true, nil
}
