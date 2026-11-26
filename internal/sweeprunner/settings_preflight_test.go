package sweeprunner

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

const settingsRunner = `{"backend": "kubernetes", "cpus": 8, "memory": 16, "jobs": 4, "output_root": "/out", "context": "k3s-sweep"}`

// TestSettingsPreflight holds the builder to the settings it reads.
func TestSettingsPreflight(t *testing.T) {
	t.Parallel()
	t.Run("the read is deferred and the block sizes the check", func(t *testing.T) {
		t.Parallel()
		var out bytes.Buffer
		reads := 0
		build := SettingsPreflight("/town", func() ([]byte, error) {
			reads++
			return []byte(`{"daemon": {"patrols": {"tier_sweep": {"runner": ` + settingsRunner + `}}}}`), nil
		}, &out)
		if reads != 0 {
			t.Fatalf("read ran %d times before the returned func", reads)
		}
		p := build()
		if p == nil || reads != 1 {
			t.Fatalf("preflight = %v after %d reads, want one preflight and one read; out: %s", p, reads, out.String())
		}
		_, err := p.Read([]byte(shortNode))
		var noRoom *NoRoomError
		if !errors.As(err, &noRoom) || !strings.Contains(err.Error(), "job needs 8200m/16640Mi") {
			t.Fatalf("Read = %v, want a decline naming the block's 8200m/16640Mi", err)
		}
	})
	t.Run("each failure writes one line naming the cause and returns nil", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name string
			read func() ([]byte, error)
			want string
		}{
			{"failed read", func() ([]byte, error) { return nil, errors.New("permission denied") }, "cannot read the town settings: permission denied"},
			{"undecodable document", func() ([]byte, error) { return []byte("{"), nil }, "cannot decode the town settings"},
			{"no runner block", func() ([]byte, error) { return []byte(`{"daemon": {"patrols": {"tier_sweep": {}}}}`), nil }, "name no daemon.patrols.tier_sweep.runner"},
			{"a block CapacityPreflight refuses", func() ([]byte, error) {
				return []byte(`{"daemon": {"patrols": {"tier_sweep": {"runner": {"backend": "kubernetes", "cpus": 8, "memory": 16, "jobs": 4, "output_root": "/out", "context": "k3s sweep"}}}}}`), nil
			}, `spec.context: "k3s sweep"`},
		} {
			var out bytes.Buffer
			if p := SettingsPreflight("/town", tc.read, &out)(); p != nil {
				t.Errorf("%s: SettingsPreflight returned a preflight, want nil", tc.name)
			}
			if lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n"); len(lines) != 1 || !strings.Contains(lines[0], tc.want) {
				t.Errorf("%s: out = %q, want one line carrying %q", tc.name, out.String(), tc.want)
			}
		}
	})
}
