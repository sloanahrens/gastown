package townstatus

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/util"
)

// TestStrayDoltInfos feeds a fake process list through the status model's
// stray list (gt-gyw5w): nothing in, nothing out; a town-port stray points at
// kill-imposters; a foreign-port leak at its own pid.
func TestStrayDoltInfos(t *testing.T) {
	t.Parallel()
	const (
		townPort    = 3307
		townDataDir = "/Users/x/gt/.dolt-data"
	)
	tests := []struct {
		name    string
		orphans []util.DoltOrphanServer
		want    []StrayDoltInfo
	}{
		{name: "no zombies", orphans: nil, want: nil},
		{
			name:    "foreign-port leak gets kill <pid>",
			orphans: []util.DoltOrphanServer{{PID: 29490, Port: 3399, DataDir: "/tmp/doltprobe/data"}},
			want:    []StrayDoltInfo{{PID: 29490, Port: 3399, DataDir: "/tmp/doltprobe/data", Remedy: "kill 29490"}},
		},
		{
			name:    "town-port zombie gets kill-imposters",
			orphans: []util.DoltOrphanServer{{PID: 500, Port: townPort, DataDir: "/tmp/other/data"}},
			want:    []StrayDoltInfo{{PID: 500, Port: townPort, DataDir: "/tmp/other/data", Remedy: "gt dolt kill-imposters"}},
		},
		{
			name:    "town data-dir on a foreign port still gets kill-imposters",
			orphans: []util.DoltOrphanServer{{PID: 501, Port: 3399, DataDir: townDataDir}},
			want:    []StrayDoltInfo{{PID: 501, Port: 3399, DataDir: townDataDir, Remedy: "gt dolt kill-imposters"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := strayDoltInfos(tt.orphans, townPort, townDataDir)
			if len(got) != len(tt.want) {
				t.Fatalf("strayDoltInfos() = %+v, want %+v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("strayDoltInfos()[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestStrayDoltInfoJSON pins the --json half of gt-gyw5w: the status model
// carries each stray pid with its port, data-dir and remedy.
func TestStrayDoltInfoJSON(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(DoltInfo{Stray: []StrayDoltInfo{
		{PID: 29490, Port: 3399, DataDir: "/tmp/doltprobe/data", Remedy: "kill 29490"},
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"stray"`, `"pid":29490`, `"port":3399`, `"data_dir":"/tmp/doltprobe/data"`, `"remedy":"kill 29490"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("stray JSON missing %q: %s", want, b)
		}
	}
}
