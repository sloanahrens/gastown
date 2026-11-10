package doltserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// countingHost is a host whose dolt call is recorded and answered with a
// one-row connection count, so GetActiveConnectionCount runs with no process.
func countingHost(t *testing.T) (*host, *[]hostCall) {
	t.Helper()
	h := newFakeHost().host()
	var calls []hostCall
	h.run = func(c hostCall) ([]byte, []byte, error) {
		calls = append(calls, c)
		return []byte("cnt\n5\n"), nil, nil
	}
	return h, &calls
}

func isDir(t *testing.T, p string) bool {
	t.Helper()
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// TestConnectionCountClientDir: the connection-count call runs dolt in the data
// directory when the town has one, and in a directory that exists under the town
// root when it does not (a pod: the Dolt server is remote). A missing data
// directory never reaches exec as the Dir (gt-1saf37).
func TestConnectionCountClientDir(t *testing.T) {
	t.Parallel()
	t.Run("a missing data directory runs in a private directory that exists", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		h, calls := countingHost(t)
		n, err := h.GetActiveConnectionCount(town)
		if err != nil || n != 5 {
			t.Fatalf("GetActiveConnectionCount = %d, %v; want 5, nil", n, err)
		}
		if len(*calls) != 1 {
			t.Fatalf("dolt ran %d times, want 1", len(*calls))
		}
		dir, data := (*calls)[0].Dir, filepath.Join(town, ".dolt-data")
		if dir == data || !isDir(t, dir) {
			t.Errorf("dolt ran in %q; want a directory that exists and is not the missing %q", dir, data)
		}
		if want := filepath.Join(town, ".runtime", "dolt-client"); dir != want {
			t.Errorf("dolt ran in %q, want %q", dir, want)
		}
		if info, err := os.Stat(dir); err == nil && info.Mode().Perm() != 0o700 {
			t.Errorf("fallback directory mode = %v, want 0700", info.Mode().Perm())
		}
	})
	t.Run("an existing data directory is the Dir, unchanged", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		data := filepath.Join(town, ".dolt-data")
		if err := os.Mkdir(data, 0o755); err != nil {
			t.Fatal(err)
		}
		h, calls := countingHost(t)
		if _, err := h.GetActiveConnectionCount(town); err != nil {
			t.Fatal(err)
		}
		if got := (*calls)[0].Dir; got != data {
			t.Errorf("dolt ran in %q, want the data directory %q", got, data)
		}
		if isDir(t, filepath.Join(town, ".runtime", "dolt-client")) {
			t.Error("the fallback directory was made although the data directory exists")
		}
	})
	t.Run("a fallback that cannot be made is the error, naming the path", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		// .runtime as a regular file makes the fallback impossible.
		if err := os.WriteFile(filepath.Join(town, ".runtime"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		h, calls := countingHost(t)
		_, err := h.GetActiveConnectionCount(town)
		want := filepath.Join(town, ".runtime", "dolt-client")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want one naming %s", err, want)
		}
		if strings.Contains(err.Error(), "fork/exec") {
			t.Errorf("error is the fork/exec text: %v", err)
		}
		if len(*calls) != 0 {
			t.Errorf("dolt ran %d times after the directory failed", len(*calls))
		}
	})
}

// TestServerSQLCmdClientDir: the server SQL command (SHOW DATABASES and the other
// callers) takes the same working directory, and carries the directory error in
// cmd.Err so Start refuses to run it.
func TestServerSQLCmdClientDir(t *testing.T) {
	t.Parallel()
	t.Run("missing data directory", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		config := &Config{TownRoot: town, Host: "127.0.0.1", Port: 3307, User: "root", DataDir: filepath.Join(town, ".dolt-data")}
		cmd := buildServerSQLCmd(context.Background(), config, "-q", "SHOW DATABASES")
		if cmd.Err != nil || cmd.Dir == config.DataDir || !isDir(t, cmd.Dir) {
			t.Errorf("Dir = %q, Err = %v; want an existing directory that is not the missing data directory", cmd.Dir, cmd.Err)
		}
	})
	t.Run("existing data directory", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		data := filepath.Join(town, ".dolt-data")
		if err := os.Mkdir(data, 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := buildServerSQLCmd(context.Background(), &Config{TownRoot: town, Host: "127.0.0.1", Port: 3307, User: "root", DataDir: data}, "-q", "SELECT 1")
		if cmd.Dir != data || cmd.Err != nil {
			t.Errorf("Dir = %q, Err = %v; want %q", cmd.Dir, cmd.Err, data)
		}
	})
	t.Run("fallback cannot be made", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		if err := os.WriteFile(filepath.Join(town, ".runtime"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := buildServerSQLCmd(context.Background(), &Config{TownRoot: town, Host: "127.0.0.1", Port: 3307, User: "root", DataDir: filepath.Join(town, ".dolt-data")}, "-q", "SELECT 1")
		if cmd.Err == nil || !strings.Contains(cmd.Err.Error(), filepath.Join(town, ".runtime", "dolt-client")) {
			t.Errorf("Err = %v, want the directory error naming the path", cmd.Err)
		}
		if cmd.Dir != "" {
			t.Errorf("Dir = %q, want it unset when the fallback failed", cmd.Dir)
		}
	})
}

// TestClientWorkDirUnsetTownRoot: with no data directory and no town root there
// is no private directory to make, so the client runs in the system temporary
// directory rather than failing the exec.
func TestClientWorkDirUnsetTownRoot(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing")
	dir, err := ClientWorkDir(&Config{DataDir: missing})
	if err != nil {
		t.Fatalf("ClientWorkDir = %v, want the temporary directory", err)
	}
	if dir != os.TempDir() {
		t.Errorf("ClientWorkDir = %q, want %q", dir, os.TempDir())
	}
}
