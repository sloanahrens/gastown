//go:build integration

package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// seedFailureReport returns the shell that reports a failed seed step, taken
// from the Dockerfile's seed RUN: from the line count of the log to the close
// of the tail branch. The log path is moved into dir, so the report runs
// against the fixture and not /tmp.
func seedFailureReport(t *testing.T, dir string) string {
	t.Helper()
	const start = "n=$(wc -l < /tmp/seed.log)"
	const end = "tail -n 10 /tmp/seed.log; fi"
	seed := seedRun(t)
	i := strings.Index(seed, start)
	j := strings.Index(seed, end)
	if i < 0 || j < i {
		t.Fatalf("the seed RUN has no failure report from %q to %q:\n%s", start, end, seed)
	}
	report := seed[i : j+len(end)]
	return strings.ReplaceAll(report, "/tmp/seed.log", filepath.Join(dir, "seed.log"))
}

// runSeedFailureReport writes a log of n numbered lines, runs the Dockerfile's
// report over it as a failed `go build`, and returns the report's output.
func runSeedFailureReport(t *testing.T, n int) string {
	t.Helper()
	dir := t.TempDir()
	var log strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&log, "line %03d\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "seed.log"), []byte(log.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "step='go build ./...'; dir=/seed/rig; " + seedFailureReport(t, dir)
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the failure report exited with %v over a %d-line log; output:\n%s", err, n, out)
	}
	return string(out)
}

// The seed RUN shows a failed step's log: the first 40 lines, and once the log
// runs past 40 lines the last 10 under a heading that says so. Lines 41 up to
// the last 10 are dropped only when the log has more than 50 lines; a log of 41
// to 50 lines repeats its end. The report runs the Dockerfile's own text against
// logs of known length, so a tail-for-head swap or a moved threshold turns a row
// red (gt-vvu6d).
func TestIntegrationPolecatImageSeedReportsBothEndsOfAFailureLog(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		lines   int
		dropped [2]int // the first and last line numbers the report must not show; zero means none
		tail    bool   // whether the report names a tail
	}{
		{"a short log is shown whole", 10, [2]int{}, false},
		{"a 40-line log is shown whole", 40, [2]int{}, false},
		{"a 50-line log drops nothing", 50, [2]int{}, true},
		{"a 51-line log drops from line 41", 51, [2]int{41, 41}, true},
		{"a 120-line log keeps the first 40 and the last 10", 120, [2]int{41, 110}, true},
		{"an empty log says so and has no tail", 0, [2]int{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := runSeedFailureReport(t, tc.lines)
			if !strings.Contains(out, "failed in /seed/rig") {
				t.Fatalf("the report does not name the failed step and directory:\n%s", out)
			}
			if got := strings.Contains(out, "seed: last 10 of"); got != tc.tail {
				t.Fatalf("the report names a tail = %v, want %v:\n%s", got, tc.tail, out)
			}
			shown := map[string]bool{}
			for _, line := range strings.Split(out, "\n") {
				shown[line] = true
			}
			for i := 1; i <= tc.lines; i++ {
				text := fmt.Sprintf("line %03d", i)
				dropped := tc.dropped[0] != 0 && i >= tc.dropped[0] && i <= tc.dropped[1]
				if dropped && shown[text] {
					t.Errorf("the report shows %q, which lies in the dropped range %d-%d:\n%s", text, tc.dropped[0], tc.dropped[1], out)
				}
				if !dropped && !shown[text] {
					t.Errorf("the report drops %q, which must show:\n%s", text, out)
				}
			}
		})
	}
}
