package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/util"
)

func TestNewDoltStrayServersCheck(t *testing.T) {
	t.Parallel()
	check := NewDoltStrayServersCheck()

	if check.Name() != "dolt-stray-servers" {
		t.Errorf("expected name 'dolt-stray-servers', got %q", check.Name())
	}
	if check.Category() != CategoryInfrastructure {
		t.Errorf("expected category %q, got %q", CategoryInfrastructure, check.Category())
	}
	if check.CanFix() {
		t.Error("expected CanFix to return false — this check is read-only")
	}
}

// newStrayTown builds a town root whose Dolt endpoint resolves to 3307, so the
// check has a town port to compare a stray server against.
func newStrayTown(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeStrayFile(t, filepath.Join(root, ".dolt-data", "config.yaml"), "listener:\n  port: 3307\n")
	return root
}

func writeStrayFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeWorkspaceMetadata records beadsDir as a server-mode Dolt workspace whose
// bd commands select database db.
func writeWorkspaceMetadata(t *testing.T, beadsDir, db string) {
	t.Helper()
	writeStrayFile(t, filepath.Join(beadsDir, "metadata.json"),
		`{"backend":"dolt","dolt_mode":"server","dolt_database":"`+db+`","dolt_server_port":3307}`)
}

// writePortFile records the port a workspace's bd commands will dial.
func writePortFile(t *testing.T, beadsDir string, port int) {
	t.Helper()
	writeStrayFile(t, filepath.Join(beadsDir, "dolt-server.port"), strconv.Itoa(port)+"\n")
}

// reachable and unreachable stand in for the TCP probe, so no case opens a
// socket.
func reachable(string) error   { return nil }
func unreachable(string) error { return errors.New("connection refused") }

// TestDoltStrayServersCheck_Run covers each shape the check names, driven by a
// fake process list and a temp workspace: a workspace's own server on a
// non-town port, a port file pointing at a dead port, a port file pointing at a
// server that lacks the workspace's database, and the healthy version of each.
func TestDoltStrayServersCheck_Run(t *testing.T) {
	t.Parallel()

	// townServer is the fake process-table entry for the town's own server,
	// serving the databases in .dolt-data.
	townServer := func(root string) util.DoltServerProcess {
		return util.DoltServerProcess{
			PID:        42,
			Port:       3307,
			ConfigPath: filepath.Join(root, ".dolt-data", "config.yaml"),
			DataDir:    filepath.Join(root, ".dolt-data"),
		}
	}

	tests := []struct {
		name       string
		serversErr error
		dial       func(string) error
		// setup builds the workspace files under the temp town root and
		// returns the fake process list the check will see.
		setup       func(t *testing.T, root string) []util.DoltServerProcess
		wantStatus  CheckStatus
		wantMessage string
		wantDetails []string
	}{
		{
			name:       "clean town",
			dial:       reachable,
			wantStatus: StatusOK,
		},
		{
			name: "workspace server on a random port",
			dial: reachable,
			setup: func(t *testing.T, root string) []util.DoltServerProcess {
				workspace := filepath.Join(root, ".claude")
				beadsDir := filepath.Join(workspace, ".beads")
				writeWorkspaceMetadata(t, beadsDir, "claude")
				writePortFile(t, beadsDir, 50095)
				return []util.DoltServerProcess{{
					PID: 50095, Port: 50095,
					ConfigPath: filepath.Join(beadsDir, "dolt-server-config.yaml"),
					DataDir:    beadsDir,
				}}
			},
			// The workspace is found through the server's --config, so its own
			// port file is judged too: the server behind it holds no "claude".
			wantStatus:  StatusWarning,
			wantMessage: "1 stray workspace Dolt server(s), 1 workspace port file(s) with no usable server",
			wantDetails: []string{
				"Stray Dolt server for workspace TOWN/.claude",
				"PID 50095",
				"port 50095",
				"'kill 50095'",
				`served by PID 50095`,
				`holds no "claude" database`,
			},
		},
		{
			name: "workspace port file pointing at a dead port",
			dial: unreachable,
			setup: func(t *testing.T, root string) []util.DoltServerProcess {
				writeWorkspaceMetadata(t, filepath.Join(root, ".beads"), "hq")
				writePortFile(t, filepath.Join(root, ".beads"), 51646)
				return nil
			},
			wantStatus:  StatusWarning,
			wantMessage: "0 stray workspace Dolt server(s), 1 workspace port file(s) with no usable server",
			wantDetails: []string{
				"port 51646 accepts no connections",
				filepath.Join("TOWN", ".beads", "dolt-server.port"),
			},
		},
		{
			name: "port file pointing at a server without the workspace database",
			dial: reachable,
			setup: func(t *testing.T, root string) []util.DoltServerProcess {
				writeWorkspaceMetadata(t, filepath.Join(root, ".beads"), "hq")
				writePortFile(t, filepath.Join(root, ".beads"), 3307)
				return []util.DoltServerProcess{townServer(root)}
			},
			wantStatus: StatusWarning,
			wantDetails: []string{
				"served by PID 42",
				`holds no "hq" database`,
				"database not found",
			},
		},
		{
			name: "port file pointing at a server whose database directory is a plain directory",
			dial: reachable,
			setup: func(t *testing.T, root string) []util.DoltServerProcess {
				writeWorkspaceMetadata(t, filepath.Join(root, ".beads"), "hq")
				writePortFile(t, filepath.Join(root, ".beads"), 3307)
				if err := os.MkdirAll(filepath.Join(root, ".dolt-data", "hq"), 0o755); err != nil {
					t.Fatal(err)
				}
				return []util.DoltServerProcess{townServer(root)}
			},
			wantStatus: StatusWarning,
			wantDetails: []string{
				"served by PID 42",
				`holds no "hq" database`,
			},
		},
		{
			name: "healthy: the town's server holds the workspace database",
			dial: reachable,
			setup: func(t *testing.T, root string) []util.DoltServerProcess {
				writeWorkspaceMetadata(t, filepath.Join(root, ".beads"), "hq")
				writePortFile(t, filepath.Join(root, ".beads"), 3307)
				writeStrayDatabase(t, filepath.Join(root, ".dolt-data"), "hq")
				return []util.DoltServerProcess{townServer(root)}
			},
			wantStatus: StatusOK,
		},
		{
			name: "healthy: a workspace pointed at the town's own port is not stray",
			dial: reachable,
			setup: func(t *testing.T, root string) []util.DoltServerProcess {
				beadsDir := filepath.Join(root, ".claude", ".beads")
				writeWorkspaceMetadata(t, beadsDir, "claude")
				writePortFile(t, beadsDir, 3307)
				writeStrayDatabase(t, filepath.Join(root, ".dolt-data"), "claude")
				return []util.DoltServerProcess{{
					PID: 43, Port: 3307,
					ConfigPath: filepath.Join(beadsDir, "dolt-server-config.yaml"),
					DataDir:    filepath.Join(root, ".dolt-data"),
				}}
			},
			wantStatus: StatusOK,
		},
		{
			name:       "process table unreadable",
			serversErr: errors.New("ps: exit status 1"),
			dial:       reachable,
			wantStatus: StatusSkipped,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := newStrayTown(t)

			var servers []util.DoltServerProcess
			if tt.setup != nil {
				servers = tt.setup(t, root)
			}

			check := NewDoltStrayServersCheck()
			check.findServers = func() ([]util.DoltServerProcess, error) {
				return servers, tt.serversErr
			}
			check.dial = tt.dial

			result := check.Run(&CheckContext{TownRoot: root})

			if result.Status != tt.wantStatus {
				t.Fatalf("Status = %v (%s), want %v", result.Status, result.Message, tt.wantStatus)
			}
			if tt.wantMessage != "" && result.Message != tt.wantMessage {
				t.Errorf("Message = %q, want %q", result.Message, tt.wantMessage)
			}
			// The temp root is unique per case; naming it TOWN keeps the
			// expectations readable.
			joined := strings.ReplaceAll(strings.Join(result.Details, "\n"), root, "TOWN")
			for _, want := range tt.wantDetails {
				if !strings.Contains(joined, want) {
					t.Errorf("details missing %q:\n%s", want, joined)
				}
			}
		})
	}
}

// writeStrayDatabase lays out a loadable Dolt database (.dolt/noms/manifest)
// under dataDir.
func writeStrayDatabase(t *testing.T, dataDir, name string) {
	t.Helper()
	writeStrayFile(t, filepath.Join(dataDir, name, ".dolt", "noms", "manifest"), "manifest")
}

// TestDoltStrayServersCheck_PortFileServerWithPhantomDatabaseDir: a directory
// named after the workspace database, with .dolt/ but no noms/manifest, is not a
// database the server can load, so bd fails with "database not found" there.
func TestDoltStrayServersCheck_PortFileServerWithPhantomDatabaseDir(t *testing.T) {
	t.Parallel()
	root := newStrayTown(t)
	dataDir := filepath.Join(root, ".dolt-data")
	if err := os.MkdirAll(filepath.Join(dataDir, "foo", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeWorkspaceMetadata(t, filepath.Join(root, ".beads"), "foo")
	writePortFile(t, filepath.Join(root, ".beads"), 3307)

	check := NewDoltStrayServersCheck()
	check.findServers = func() ([]util.DoltServerProcess, error) {
		return []util.DoltServerProcess{{
			PID: 42, Port: 3307,
			ConfigPath: filepath.Join(dataDir, "config.yaml"),
			DataDir:    dataDir,
		}}, nil
	}
	check.dial = reachable

	result := check.Run(&CheckContext{TownRoot: root})

	if result.Status != StatusWarning {
		t.Fatalf("Status = %v (%s), want %v", result.Status, result.Message, StatusWarning)
	}
	if joined := strings.Join(result.Details, "\n"); !strings.Contains(joined, `holds no "foo" database`) {
		t.Errorf("details missing the phantom-database report:\n%s", joined)
	}
}
