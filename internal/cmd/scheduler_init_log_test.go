package cmd

// The log decision for the scheduler integration helper's bd init. Untagged,
// so the unit tier compiles it beside the integration helper that calls it.

import (
	"fmt"
	"strings"
	"testing"
)

// schedulerInitLine returns the line to log for the bd init in
// initBeadsDBForServer, and whether there is one. A passing init returns no
// line, so the tier sweep's first line under a failing test is the real
// assertion; a failing init returns the command, its directory, its error and
// its output.
func schedulerInitLine(prefix, dir string, err error, out []byte) (string, bool) {
	if err == nil {
		return "", false
	}
	return fmt.Sprintf("bd init --prefix %s in %s: exit=%v\n%s", prefix, dir, err, out), true
}

func TestSchedulerInitLine(t *testing.T) {
	t.Parallel()

	t.Run("nil error yields no line", func(t *testing.T) {
		t.Parallel()
		line, ok := schedulerInitLine("gtsched", "/scheduler/town", nil, []byte("initialized\n"))
		if ok || line != "" {
			t.Fatalf("schedulerInitLine with a nil error = (%q, %v), want (\"\", false)", line, ok)
		}
	})

	t.Run("failed init yields a line naming prefix dir error and output", func(t *testing.T) {
		t.Parallel()
		err := fmt.Errorf("exit status 1")
		out := []byte("bd: dolt server unreachable\n")
		line, ok := schedulerInitLine("gtsched", "/scheduler/town", err, out)
		if !ok {
			t.Fatalf("schedulerInitLine with an error = (%q, false), want ok true", line)
		}
		for _, want := range []string{"gtsched", "/scheduler/town", err.Error(), string(out)} {
			if !strings.Contains(line, want) {
				t.Errorf("line %q does not hold %q", line, want)
			}
		}
	})
}
