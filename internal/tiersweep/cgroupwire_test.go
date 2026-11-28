package tiersweep

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestSweepCgroupWired: the sweep reads the container's counters through the
// host's CgroupDir, so an integration line and a race line record them; a host
// that names no directory records none.
func TestSweepCgroupWired(t *testing.T) {
	t.Parallel()
	logDir := t.TempDir()
	// sweep runs the integration and race tiers over one tagged package.
	sweep := func(cgroupDir string) []string {
		repo := baseRepo()
		oneTagged(repo)
		var stdout, stderr bytes.Buffer
		code := Run([]string{TierIntegration, TierRace}, &stdout, &stderr, Deps{
			CgoEnv: testCgoEnv,
			Src:    repo,
			Exec:   (&fakeTier{head: "abc123"}).exec,
			Now:    func() time.Time { return time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC) },
			Host: Host{
				Env:       func(name string) string { return map[string]string{EnvLogDir: logDir}[name] },
				MkdirAll:  os.MkdirAll,
				WriteFile: os.WriteFile, AppendFile: appendToFile,
				ReadFile: cgroupFiles("", ""), CgroupDir: cgroupDir,
			},
		})
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr %q)", code, stderr.String())
		}
		data, err := os.ReadFile(filepath.Join(logDir, TimingsName))
		if err != nil {
			t.Fatalf("read the timings: %v", err)
		}
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
	wired := sweep("/cgroup")
	for _, tier := range []string{TierIntegration, TierRace} {
		if !slices.ContainsFunc(wired, func(l string) bool {
			return strings.Contains(l, `"tier":"`+tier+`"`) && strings.Contains(l, `"cgroup":{`)
		}) {
			t.Errorf("no %s line recorded the counters: %q", tier, wired)
		}
	}
	for _, line := range sweep("") {
		if strings.Contains(line, `"cgroup"`) {
			t.Errorf("timings line %q holds a cgroup object, want none", line)
		}
	}
}
