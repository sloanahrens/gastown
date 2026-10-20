package power

import (
	"context"
	"os"
	"strings"
	"testing"
)

// assertOffNotRecorded fails when Off left any power state behind.
func assertOffNotRecorded(t *testing.T, f *fake) {
	t.Helper()
	if data, err := os.ReadFile(StatePath(f.town)); !os.IsNotExist(err) {
		t.Errorf("a power state was recorded (read err %v): %q", err, data)
	}
}

func TestOffWhenComposeProbeFails(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.setPending(0)
	f.failComposePs = true
	f.jobhost = true
	o, _, _ := f.optsWithOut()
	o.Yes = true
	o.WithDolt = true
	err := Off(context.Background(), o)
	if err == nil {
		t.Fatal("Off succeeded although the compose probe failed")
	}
	if code := codeOf(t, err); code != ExitNotHealthy {
		t.Errorf("exit code = %d, want %d", code, ExitNotHealthy)
	}
	if !f.has("docker compose stop") {
		t.Error("off skipped the compose stop after a failed probe")
	}
	if f.composeN != 0 {
		t.Errorf("compose stop did not take effect: compose=%d", f.composeN)
	}
	if !f.has("gt dolt stop") || f.dolt {
		t.Errorf("off returned before stopping Dolt after the blind stop: dolt=%v", f.dolt)
	}
	if !f.jobhostRan {
		t.Error("off returned before the jobhost hook after the blind stop")
	}
	assertOffNotRecorded(t, f)
}

func TestOffWhenComposeProbeAndStopFail(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.setHealthy()
	f.setPending(0)
	f.failComposePs = true
	f.failComposeSt = true
	o, _, _ := f.optsWithOut()
	o.Yes = true
	err := Off(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "docker compose stop") {
		t.Fatalf("error = %v, want the docker compose stop failure", err)
	}
	assertOffNotRecorded(t, f)
}
