package dashboard

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestReadSeatGate reads the gate time the overrun guard charged a seat from
// its state file, and shows every refusal answering (0, false).
func TestReadSeatGate(t *testing.T) {
	t.Parallel()
	// gate_spent is a duration's integer nanoseconds, and omitempty leaves the
	// key out when it is zero (internal/daemon/overrun_guard_decide.go).
	const valid = `{"bead":"gt-abc","gate_spent":936000000000}`
	cases := []struct {
		name   string
		bead   string
		path   string // the file's path under the runtime dir; "" writes none
		file   string
		want   time.Duration
		wantOK bool
	}{
		{name: "valid", bead: "gt-abc", path: "overrun-guard/gt-abc.json", file: valid, want: 936 * time.Second, wantOK: true},
		{name: "zero omitted", bead: "gt-abc", path: "overrun-guard/gt-abc.json", file: `{"bead":"gt-abc"}`, want: 0, wantOK: true},
		{name: "missing file", bead: "gt-abc"},
		{name: "malformed", bead: "gt-abc", path: "overrun-guard/gt-abc.json", file: `{"gate_spent":`},
		{name: "negative", bead: "gt-abc", path: "overrun-guard/gt-abc.json", file: `{"gate_spent":-1}`},
		// The file is written where a join that skipped the id check lands.
		{name: "bead id with a separator", bead: "../gt-abc", path: "gt-abc.json", file: valid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			//testpolicy:allow no-exec-files — the TempDir root, kept private under the gate's umask 000
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if c.path != "" {
				path := filepath.Join(dir, c.path)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(c.file), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, ok := readSeatGate(dir, c.bead)
			if got != c.want || ok != c.wantOK {
				t.Errorf("readSeatGate(%q, %q) = %v, %v; want %v, %v", dir, c.bead, got, ok, c.want, c.wantOK)
			}
		})
	}
}
