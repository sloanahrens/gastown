package util

import "testing"

func TestIsDoltSQLServerArgsSlice(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"plain dolt", []string{"dolt", "sql-server", "--config", "/tmp/gt/.dolt-data/config.yaml"}, true},
		{"absolute dolt", []string{"/usr/bin/dolt", "sql-server"}, true},
		{"homebrew dolt", []string{"/opt/homebrew/bin/dolt", "sql-server", "--config", "/t/.dolt-data/config.yaml"}, true},
		{"global flags before the subcommand", []string{"dolt", "--data-dir", "/x", "sql-server"}, true},
		{"docker port forwarder", []string{"/Applications/Docker.app/Contents/MacOS/com.docker.backend", "services"}, false},
		{"not sql server", []string{"dolt", "status"}, false},
		{"other subcommand", []string{"dolt", "sql"}, false},
		{"grep", []string{"grep", "dolt", "sql-server"}, false},
		{"not dolt", []string{"/bin/sleep", "sql-server"}, false},
		{"bare dolt", []string{"dolt"}, false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsDoltSQLServerArgs(tt.args); got != tt.want {
				t.Fatalf("IsDoltSQLServerArgs(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

// TestClassifyStrayDoltRemedy feeds a fake process list through the remedy
// classifier (gt-gyw5w): the town's own endpoint points at kill-imposters, a
// leak on a foreign port at its own pid, and nothing at all at nothing.
func TestClassifyStrayDoltRemedy(t *testing.T) {
	t.Parallel()
	const (
		townPort    = 3307
		townDataDir = "/Users/x/gt/.dolt-data"
	)
	tests := []struct {
		name string
		// procs is the stray-process listing as a scanner would return it.
		procs []StrayDoltProcess
		want  []string // remedy per finding, in order
	}{
		{
			name:  "no zombies",
			procs: nil,
			want:  []string{},
		},
		{
			name:  "town-port zombie is an imposter",
			procs: []StrayDoltProcess{{PID: 500, Port: townPort, DataDir: "/tmp/other/data"}},
			want:  []string{"gt dolt kill-imposters"},
		},
		{
			name:  "town data-dir on a foreign port still points at kill-imposters",
			procs: []StrayDoltProcess{{PID: 501, Port: 3399, DataDir: townDataDir}},
			want:  []string{"gt dolt kill-imposters"},
		},
		{
			// The gt-gyw5w leak: dolt sql-server --port 3399 --data-dir
			// /tmp/doltprobe/data, ppid 1. kill-imposters never matches it,
			// so the remedy is the pid and only the pid.
			name: "foreign-port zombie gets kill <pid>",
			procs: []StrayDoltProcess{
				{PID: 29490, Port: 3399, DataDir: "/tmp/doltprobe/data"},
			},
			want: []string{"kill 29490"},
		},
		{
			name: "mixed list keeps each remedy with its pid",
			procs: []StrayDoltProcess{
				{PID: 1, Port: townPort, DataDir: "/tmp/other/data"},
				{PID: 2, Port: 3399, DataDir: "/tmp/doltprobe/data"},
				{PID: 3, Port: 0, DataDir: ""}, // argv carried no endpoint: fails safe to the pid
			},
			want: []string{"gt dolt kill-imposters", "kill 2", "kill 3"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			findings := ClassifyStrayDolt(tt.procs, townPort, townDataDir)
			if len(findings) != len(tt.want) {
				t.Fatalf("ClassifyStrayDolt() returned %d findings, want %d", len(findings), len(tt.want))
			}
			for i, f := range findings {
				if f.Remedy != tt.want[i] {
					t.Errorf("finding %d (pid %d) remedy = %q, want %q", i, f.Process.PID, f.Remedy, tt.want[i])
				}
				if tt.name != "no zombies" && f.Process.PID == 0 {
					t.Errorf("finding %d lost its process: %+v", i, f.Process)
				}
			}
		})
	}
}
