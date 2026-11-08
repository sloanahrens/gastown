package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// assertFailedStatus reads the status file and requires a failed run that names
// the apps file, so a previous healthy status cannot survive a broken run.
func assertFailedStatus(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the failed run's status was not written: %v", err)
	}
	var got struct {
		OK     int `json:"ok"`
		Failed int `json:"failed"`
		Items  []struct {
			Level string `json:"level"`
			Text  string `json:"text"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("the status does not parse: %v", err)
	}
	if got.OK != 0 || got.Failed != 1 {
		t.Errorf("status = %d ok, %d failed; want 0 ok, 1 failed", got.OK, got.Failed)
	}
	if len(got.Items) != 1 || got.Items[0].Level != "fail" || !strings.Contains(got.Items[0].Text, "apps file") {
		t.Errorf("items = %+v, want one fail item naming the apps file", got.Items)
	}
}

func TestCloudCheckExitCodes(t *testing.T) {
	t.Parallel()
	//testpolicy:allow no-network — the bead requires httptest servers: this one answers in-process on loopback with the passing and failing paths
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	writeFile := func(t *testing.T, name, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	run := func(args ...string) error {
		c := newCloudCheckCmd()
		c.SetArgs(args)
		c.SetOut(io.Discard)
		c.SetErr(io.Discard)
		return c.Execute()
	}
	apps := func(t *testing.T, url string) string {
		t.Helper()
		return writeFile(t, "apps", "web="+url+"\n")
	}

	t.Run("a passing check exits 0 and writes the status", func(t *testing.T) {
		t.Parallel()
		path := apps(t, srv.URL+"/ok")
		jsonPath := filepath.Join(t.TempDir(), "status.json")
		if err := run("--apps", path, "--json", jsonPath); err != nil {
			t.Fatalf("exit %d, want 0: %v", exitCodeForError(err), err)
		}
		b, err := os.ReadFile(jsonPath)
		if err != nil {
			t.Fatalf("the status file was not written: %v", err)
		}
		if len(b) == 0 {
			t.Error("the status file is empty")
		}
	})

	t.Run("a failing check exits 1", func(t *testing.T) {
		t.Parallel()
		err := run("--apps", apps(t, srv.URL+"/bad"))
		if err == nil || exitCodeForError(err) != 1 {
			t.Fatalf("exit = %v (code %d), want 1", err, exitCodeForError(err))
		}
	})

	t.Run("an off app exits 0", func(t *testing.T) {
		t.Parallel()
		offDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(offDir, "gastown.off"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		path := writeFile(t, "apps", "app=gastown\nweb="+srv.URL+"/bad\n")
		if err := run("--apps", path, "--off-dir", offDir); err != nil {
			t.Fatalf("exit %d, want 0: %v", exitCodeForError(err), err)
		}
	})

	t.Run("an extra argument exits 2", func(t *testing.T) {
		t.Parallel()
		err := run("--apps", apps(t, srv.URL+"/ok"), "extra")
		if err == nil || exitCodeForError(err) != 2 {
			t.Fatalf("exit = %v (code %d), want 2", err, exitCodeForError(err))
		}
	})

	t.Run("a missing --apps exits 2", func(t *testing.T) {
		t.Parallel()
		err := run()
		if err == nil || exitCodeForError(err) != 2 {
			t.Fatalf("exit = %v (code %d), want 2", err, exitCodeForError(err))
		}
	})

	t.Run("an unknown flag exits 2", func(t *testing.T) {
		t.Parallel()
		err := run("--no-such-flag")
		if err == nil || exitCodeForError(err) != 2 {
			t.Fatalf("exit = %v (code %d), want 2", err, exitCodeForError(err))
		}
	})

	t.Run("an unreadable apps file exits 1 and replaces an ok status", func(t *testing.T) {
		t.Parallel()
		jsonPath := filepath.Join(t.TempDir(), "status.json")
		if err := run("--apps", apps(t, srv.URL+"/ok"), "--json", jsonPath); err != nil {
			t.Fatalf("the good run exits %d, want 0: %v", exitCodeForError(err), err)
		}
		err := run("--apps", filepath.Join(t.TempDir(), "missing"), "--json", jsonPath)
		if err == nil || exitCodeForError(err) != 1 {
			t.Fatalf("exit = %v (code %d), want 1", err, exitCodeForError(err))
		}
		assertFailedStatus(t, jsonPath)
	})

	t.Run("an apps file that cannot be read after opening exits 1 and writes a failed status", func(t *testing.T) {
		t.Parallel()
		jsonPath := filepath.Join(t.TempDir(), "status.json")
		// A directory opens but cannot be read as a line list.
		err := run("--apps", t.TempDir(), "--json", jsonPath)
		if err == nil || exitCodeForError(err) != 1 {
			t.Fatalf("exit = %v (code %d), want 1", err, exitCodeForError(err))
		}
		assertFailedStatus(t, jsonPath)
	})

	t.Run("an apps file line over the cap exits 1", func(t *testing.T) {
		t.Parallel()
		path := writeFile(t, "apps", "gt="+strings.Repeat("a", 64<<10)+"\n")
		err := run("--apps", path)
		if err == nil || exitCodeForError(err) != 1 {
			t.Fatalf("exit = %v (code %d), want 1", err, exitCodeForError(err))
		}
	})

	t.Run("a status file that cannot be written exits 1", func(t *testing.T) {
		t.Parallel()
		missingDir := filepath.Join(t.TempDir(), "missing", "status.json")
		err := run("--apps", apps(t, srv.URL+"/ok"), "--json", missingDir)
		if err == nil || exitCodeForError(err) != 1 {
			t.Fatalf("exit = %v (code %d), want 1", err, exitCodeForError(err))
		}
	})
}
