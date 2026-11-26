package cmd

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/update"
)

// TestPodSweepSmokeRecord pins the daemon pod's launch-smoke read: a record is
// decoded, a missing one is not an error, and an unreadable one is, carrying no output.
func TestPodSweepSmokeRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const secret = "SECRET-MARKER"
	const record = `{"time":"2026-11-26T08:00:00Z","daemon_start":"2026-11-26T07:59:00Z","outcome":"failed","detail":"killed"}`
	fake := func(out string, exit int, err error, argv *[]string) update.RunFunc {
		return func(_ context.Context, _ string, _ []string, name string, args ...string) (string, int, error) {
			*argv = append([]string{name}, args...)
			return out, exit, err
		}
	}

	t.Run("a record is read and decoded from the daemon pod", func(t *testing.T) {
		t.Parallel()
		var argv []string
		rec, exists, err := podSweepSmokeRecord(podPairTarget(), fake(record, 0, nil, &argv))(ctx)
		if err != nil || !exists {
			t.Fatalf("read: exists %v, err %v", exists, err)
		}
		if want := time.Date(2026, 11, 26, 8, 0, 0, 0, time.UTC); rec.Outcome != "failed" || rec.Detail != "killed" || !rec.Time.Equal(want) {
			t.Errorf("record = %+v, want the failed record stamped %s", rec, want)
		}
		onRecord := func(a string) bool { return strings.HasSuffix(a, "/.runtime/tier-sweep/launch-smoke.json") }
		if !slices.Contains(argv, "deployment/"+k8sdaemon.Name) || !slices.ContainsFunc(argv, onRecord) {
			t.Errorf("ran %v, want a read of the launch-smoke record in the daemon's deployment", argv)
		}
	})

	t.Run("a missing record reads as not existing, with no error", func(t *testing.T) {
		t.Parallel()
		rec, exists, err := podSweepSmokeRecord(podPairTarget(), fake("", podSweepSmokeAbsent, nil, &[]string{}))(ctx)
		if err != nil || exists || rec != (daemon.SweepSmokeRecord{}) {
			t.Errorf("missing record: %+v exists %v err %v, want the zero record, not existing, no error", rec, exists, err)
		}
	})

	t.Run("a record that cannot be decoded is an error carrying no output", func(t *testing.T) {
		t.Parallel()
		for _, out := range []string{"", "{not json", "{" + secret} {
			_, exists, err := podSweepSmokeRecord(podPairTarget(), fake(out, 0, nil, &[]string{}))(ctx)
			if err == nil || exists || strings.Contains(err.Error(), secret) {
				t.Errorf("output %q: exists %v, err %v, want an error carrying no output", out, exists, err)
			}
		}
	})

	t.Run("a failed read is an error carrying no output", func(t *testing.T) {
		t.Parallel()
		boom := errors.New("kubectl refused")
		if _, _, err := podSweepSmokeRecord(podPairTarget(), fake("", 0, boom, &[]string{}))(ctx); !errors.Is(err, boom) {
			t.Errorf("error = %v, want the run error", err)
		}
		_, exists, err := podSweepSmokeRecord(podPairTarget(), fake(record, 1, nil, &[]string{}))(ctx)
		if err == nil || exists {
			t.Errorf("exit 1: exists %v, err %v, want an error and no record", exists, err)
		}
	})
}
