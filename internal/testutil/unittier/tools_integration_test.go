//go:build integration

package unittier

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// TestIntegrationRefusingToolRecordsAndFails runs a refusing stand-in for
// real: it exits 1 with the reason on stderr, and the start it recorded fails
// the run.
func TestIntegrationRefusingToolRecordsAndFails(t *testing.T) {
	dir := t.TempDir()
	if err := writeRefusingTools(dir, nil, func(string) (string, error) { return "", nil }); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(dir+"/tmux", "-L", "x", "kill-server")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "tmux: the unit tier starts no tmux") {
		t.Fatalf("refusing tmux: err %v, output %q; want exit 1 and the reason", err, out)
	}
	var w bytes.Buffer
	if code := reportRefused(0, dir, &w); code != 1 || !strings.Contains(w.String(), "tmux -L x kill-server") {
		t.Errorf("report: code %d, output %q; want 1 naming the start", code, w.String())
	}
}
