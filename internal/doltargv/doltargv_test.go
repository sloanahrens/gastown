package doltargv

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

func TestIsSQLServer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"plain", []string{"dolt", "sql-server", "--config", "c.yaml"}, true},
		{"absolute", []string{"/opt/homebrew/bin/dolt", "sql-server", "--port", "3307"}, true},
		{"global flag before subcommand", []string{"dolt", "--config", "x", "sql-server"}, true},
		{"tail of the log", []string{"tail", "-f", "dolt-sql-server.log"}, false},
		{"editor on dolt-server.go", []string{"vim", "dolt-server.go"}, false},
		{"dolt client", []string{"dolt", "sql", "-q", "select 1"}, false},
		{"bare dolt", []string{"dolt"}, false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSQLServer(tt.args); got != tt.want {
				t.Fatalf("IsSQLServer(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}
