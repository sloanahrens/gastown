package testpolicy

import (
	"strconv"
	"strings"
)

// CPUQuotaEnv names the environment variable the runner sets to the job
// container's CPU limit. Inside a CI job container cpu.max reads "max": the
// limit sits on the docker-in-docker container that runs it, while NumCPU is
// the host's count.
const CPUQuotaEnv = "CI_CPU_QUOTA"

// The cgroup files CPUQuota reads: the v2 quota and period on one line, then
// the v1 pair.
const (
	cpuMaxPath    = "/sys/fs/cgroup/cpu.max"
	cpuQuotaPath  = "/sys/fs/cgroup/cpu/cpu.cfs_quota_us"
	cpuPeriodPath = "/sys/fs/cgroup/cpu/cpu.cfs_period_us"
)

// CPUQuota returns the CPUs the gate may use: the cgroup's quota when it has
// one, else the positive CI_CPU_QUOTA value, else numCPU. A source that is
// unreadable or holds no positive quota falls to the next one, never to zero.
func CPUQuota(read func(path string) ([]byte, error), env func(string) string, numCPU int) int {
	if n, ok := cgroupQuotaV2(read); ok {
		return n
	}
	if n, ok := cgroupQuotaV1(read); ok {
		return n
	}
	if n, err := strconv.Atoi(strings.TrimSpace(env(CPUQuotaEnv))); err == nil && n > 0 {
		return n
	}
	return numCPU
}

// cgroupQuotaV2 reads cpu.max, whose fields are the quota and the period. A
// first field of "max" does not parse, so it falls through.
func cgroupQuotaV2(read func(string) ([]byte, error)) (int, bool) {
	b, err := read(cpuMaxPath)
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) != 2 {
		return 0, false
	}
	return quotaCPUs(fields[0], fields[1])
}

// cgroupQuotaV1 reads the v1 quota and period files, which a v2 mount lacks.
func cgroupQuotaV1(read func(string) ([]byte, error)) (int, bool) {
	qb, err := read(cpuQuotaPath)
	if err != nil {
		return 0, false
	}
	pb, err := read(cpuPeriodPath)
	if err != nil {
		return 0, false
	}
	return quotaCPUs(string(qb), string(pb))
}

// quotaCPUs is the whole CPUs quota over period allows, rounded up, or false
// when either is not positive: cgroup v1's unlimited -1 does not fall to zero.
func quotaCPUs(quota, period string) (int, bool) {
	q, err := strconv.ParseInt(strings.TrimSpace(quota), 10, 64)
	if err != nil || q <= 0 {
		return 0, false
	}
	p, err := strconv.ParseInt(strings.TrimSpace(period), 10, 64)
	if err != nil || p <= 0 {
		return 0, false
	}
	return int((q + p - 1) / p), true
}
