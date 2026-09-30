package util

import "testing"

func TestIsDoltSQLServerArgsSlice(t *testing.T) {
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
