//go:build integration

package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
	"github.com/steveyegge/gastown/tracker/store/pgtest"
)

const waitBound = 20 * time.Second

func dsnEnv(t *testing.T) func(string) string {
	t.Helper()
	dsn := pgtest.New(t, "empty").Config().ConnString()
	return env(map[string]string{"TRACKER_DSN": dsn})
}

func do(t *testing.T, url string, hdr map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

// Integration tier: real postgres, a real loopback socket, ~1 s.
func TestServesWithRealPostgresTokensAndSchemaGate(t *testing.T) {
	getenv := dsnEnv(t)
	var out, errb bytes.Buffer
	ctx := context.Background()

	if err := run(ctx, []string{"-migrate"}, getenv, &out, &errb, nil); err != nil {
		t.Fatalf("-migrate: %v (%s)", err, errb.String())
	}
	tokFile := filepath.Join(t.TempDir(), "tok")
	out.Reset()
	if err := run(ctx, []string{"-issue-token", "town/crew/dipper", "-token-file", tokFile}, getenv, &out, &errb, nil); err != nil {
		t.Fatalf("-issue-token: %v", err)
	}
	raw, err := os.ReadFile(tokFile)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(token, "trk_") || strings.Contains(out.String()+errb.String(), token) {
		t.Fatalf("token file %q / output %q: want a trk_ token that is never printed", token, out.String())
	}
	if st, _ := os.Stat(tokFile); st.Mode().Perm() != 0o600 {
		t.Errorf("token file mode = %v, want 0600", st.Mode().Perm())
	}
	if err := run(ctx, []string{"-issue-token", "x", "-token-file", tokFile}, getenv, &out, &errb, nil); err == nil {
		t.Error("issuing into an existing token file succeeded; it must never overwrite")
	}

	runCtx, cancel := context.WithCancel(ctx)
	addrCh := make(chan net.Addr, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- run(runCtx, []string{"-listen", "127.0.0.1:0"}, getenv, io.Discard, io.Discard, func(a net.Addr) { addrCh <- a })
	}()
	var addr net.Addr
	select {
	case addr = <-addrCh:
	case err := <-errCh:
		t.Fatalf("server exited before it was ready: %v", err)
	case <-time.After(waitBound):
		t.Fatal("server not ready in time")
	}
	if !addr.(*net.TCPAddr).IP.IsLoopback() {
		t.Errorf("server listens on %v, want a loopback address", addr)
	}
	base := "http://" + addr.String()
	level := strconv.Itoa(store.SchemaLevel())

	if code, body := do(t, base+"/readyz", nil); code != 200 {
		t.Errorf("/readyz = %d %s, want 200 against a real database", code, body)
	}
	good := map[string]string{"Authorization": "Bearer " + token, api.SchemaHeader: level}
	if code, body := do(t, base+"/v1/whoami", good); code != 200 || !strings.Contains(body, `"actor":"town/crew/dipper"`) {
		t.Errorf("whoami with the issued token = %d %s", code, body)
	}
	if code, _ := do(t, base+"/v1/whoami", map[string]string{"Authorization": "Bearer trk_wrong", api.SchemaHeader: level}); code != 401 {
		t.Errorf("whoami with a wrong token = %d, want 401", code)
	}
	if code, _ := do(t, base+"/v1/whoami", map[string]string{"Authorization": "Bearer " + token, api.SchemaHeader: "999"}); code != 409 {
		t.Errorf("whoami with a skewed schema level = %d, want 409", code)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("shutdown returned %v, want nil", err)
		}
	case <-time.After(waitBound):
		t.Fatal("server did not shut down in time")
	}
}

func TestRefusesToServeAnUnmigratedDatabase(t *testing.T) {
	getenv := dsnEnv(t)
	err := run(context.Background(), []string{"-listen", "127.0.0.1:0"}, getenv, io.Discard, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "-migrate") {
		t.Errorf("serving an unmigrated database = %v, want an error that says to run -migrate", err)
	}
}
