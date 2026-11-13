package bdgateway

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// writeTokenFile writes one file in a token directory and returns its path.
// WriteFile's mode is masked by the umask, so the mode is set again; the
// authenticator tests then see exactly the mode they asked for.
func writeTokenFile(t *testing.T, dir, name, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", name, err)
	}
	return path
}

// waitContext bounds a test's channel waits. A wait that times out fails the
// test naming what it waited for instead of hanging until the suite deadline.
func waitContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestAuthTokens is the acceptance criterion that a valid token resolves to the
// class and caller its file name gives, and that an unknown or empty token is
// 401. The caller comes from the token file, so a token cannot speak as a
// caller whose file it is not.
func TestAuthTokens(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTokenFile(t, dir, "read.oncall-dashboard.token", "read-token\n", 0o600)
	writeTokenFile(t, dir, "daemon.landing-worker.token", "daemon-token\n", 0o600)
	writeTokenFile(t, dir, "operator.mac-wrapper.token", "operator-token\n", 0o600)
	auth, err := NewAuthenticator(dir)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	tests := []struct {
		name    string
		header  string
		want    Identity
		wantErr error
	}{
		{"read token", "Bearer read-token", Identity{TokenRead, "oncall-dashboard"}, nil},
		{"daemon token", "Bearer daemon-token", Identity{TokenDaemon, "landing-worker"}, nil},
		{"operator token", "Bearer operator-token", Identity{TokenOperator, "mac-wrapper"}, nil},
		{"no header", "", Identity{}, ErrNoToken},
		{"bearer with no token", "Bearer ", Identity{}, ErrNoToken},
		{"another scheme", "Basic dXNlcjpwYXNz", Identity{}, ErrNoToken},
		{"unknown token", "Bearer not-a-token", Identity{}, ErrUnknownToken},
		{"prefix of a valid token", "Bearer read", Identity{}, ErrUnknownToken},
		{"a caller name is not a token", "Bearer oncall-dashboard", Identity{}, ErrUnknownToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := auth.Authenticate(tt.header)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Authenticate(%q) error = %v, want %v", tt.header, err, tt.wantErr)
				}
				if status := HTTPStatus(err); status != http.StatusUnauthorized {
					t.Errorf("HTTPStatus(%v) = %d, want 401", err, status)
				}
				return
			}
			if err != nil {
				t.Fatalf("Authenticate(%q): %v", tt.header, err)
			}
			if got != tt.want {
				t.Errorf("Authenticate(%q) = %+v, want %+v", tt.header, got, tt.want)
			}
		})
	}
}

// TestAuthModeRefused is the acceptance criterion that a token file wider than
// 0600 is refused, with a named error naming the file and mode but never the
// token.
func TestAuthModeRefused(t *testing.T) {
	t.Parallel()
	const token = "super-secret-value"
	tests := []struct {
		name    string
		mode    os.FileMode
		wantErr error
	}{
		{"0600 accepted", 0o600, nil},
		{"0644 group and other read", 0o644, ErrTokenFileMode},
		{"0660 group read and write", 0o660, ErrTokenFileMode},
		{"0604 other read", 0o604, ErrTokenFileMode},
		{"0400 owner read only", 0o400, ErrTokenFileMode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTokenFile(t, dir, "read.dashboard.token", token+"\n", tt.mode)
			auth, err := NewAuthenticator(dir)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("NewAuthenticator: %v", err)
				}
				if _, err := auth.Authenticate("Bearer " + token); err != nil {
					t.Errorf("Authenticate with a 0600 file: %v", err)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("NewAuthenticator error = %v, want %v", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), token) {
				t.Errorf("error names the token: %v", err)
			}
			if !strings.Contains(err.Error(), "read.dashboard.token") {
				t.Errorf("error does not name the file: %v", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%04o", tt.mode)) {
				t.Errorf("error does not name the mode: %v", err)
			}
		})
	}
}

// TestAuthLookupUsesSeam checks the production path compares every stored
// digest through the table's comparison seam, once per entry for a known token
// and for an unknown one, over two 32-byte digests. A Lookup that compared
// digests itself would leave the counting fake uncalled, so the test goes red.
func TestAuthLookupUsesSeam(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTokenFile(t, dir, "read.dashboard.token", "stored-token\n", 0o600)
	writeTokenFile(t, dir, "daemon.worker.token", "daemon-token\n", 0o600)
	table, err := LoadTokens(dir)
	if err != nil {
		t.Fatalf("LoadTokens: %v", err)
	}
	tests := []struct {
		name      string
		token     string
		want      Identity
		wantFound bool
	}{
		{"known token", "stored-token", Identity{TokenRead, "dashboard"}, true},
		{"unknown token", "not-a-token", Identity{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var operands [][2]int
			table.compare = func(a, b []byte) int {
				operands = append(operands, [2]int{len(a), len(b)})
				return subtle.ConstantTimeCompare(a, b)
			}
			got, found := table.Lookup(tt.token)
			if found != tt.wantFound {
				t.Fatalf("Lookup(%q) found = %t, want %t", tt.token, found, tt.wantFound)
			}
			if got != tt.want {
				t.Errorf("Lookup(%q) identity = %+v, want %+v", tt.token, got, tt.want)
			}
			if len(operands) != len(table.entries) {
				t.Fatalf("Lookup ran %d comparisons over %d entries: the production path does not use the compare seam", len(operands), len(table.entries))
			}
			for i, op := range operands {
				if op[0] != sha256.Size || op[1] != sha256.Size {
					t.Errorf("comparison %d got operands of %d and %d bytes, want %d and %d: the token's length is leaking into the compare",
						i, op[0], op[1], sha256.Size, sha256.Size)
				}
			}
		})
	}
}

// TestAuthDefaultCompare calls the comparison the production path uses, the
// table's own default seam, on equal and unequal digests at the fixed 32-byte
// size: 1 for equal and 0 for unequal.
func TestAuthDefaultCompare(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTokenFile(t, dir, "read.dashboard.token", "stored-token\n", 0o600)
	table, err := LoadTokens(dir)
	if err != nil {
		t.Fatalf("LoadTokens: %v", err)
	}
	first := sha256.Sum256([]byte("first"))
	second := sha256.Sum256([]byte("second"))
	if got := table.compare(first[:], first[:]); got != 1 {
		t.Errorf("default compare on equal digests = %d, want 1", got)
	}
	if got := table.compare(first[:], second[:]); got != 0 {
		t.Errorf("default compare on unequal digests = %d, want 0", got)
	}
}

// TestAuthTokenMismatchRefused checks a token is accepted only when every byte
// matches: one changed byte at any position, an extra byte, a missing byte and
// a changed case are all unknown tokens.
func TestAuthTokenMismatchRefused(t *testing.T) {
	t.Parallel()
	const token = "0123456789abcdef0123456789abcdef"
	dir := t.TempDir()
	writeTokenFile(t, dir, "read.dashboard.token", token+"\n", 0o600)
	auth, err := NewAuthenticator(dir)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	if _, err := auth.Authenticate("Bearer " + token); err != nil {
		t.Fatalf("the exact token is refused: %v", err)
	}
	flip := func(at int, b byte) string {
		mutated := []byte(token)
		mutated[at] = b
		return string(mutated)
	}
	mutations := map[string]string{
		"prefix":            token[:len(token)-1],
		"suffix":            token[1:],
		"one byte appended": token + "0",
		"case changed":      strings.ToUpper(token),
	}
	for at := 0; at < len(token); at++ {
		mutations[fmt.Sprintf("byte %d flipped", at)] = flip(at, token[at]^0xff)
	}
	for name, mutated := range mutations {
		t.Run(name, func(t *testing.T) {
			if _, err := auth.Authenticate("Bearer " + mutated); !errors.Is(err, ErrUnknownToken) {
				t.Errorf("Authenticate(%q) error = %v, want ErrUnknownToken", mutated, err)
			}
		})
	}
}

// TestAuthReload is the acceptance criterion that a reload swaps the table: a
// token the reload removed is 401 on its next request, the new token works, and
// an identity handed out before the reload still authorizes its call.
func TestAuthReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTokenFile(t, dir, "read.dashboard.token", "old-token\n", 0o600)
	auth, err := NewAuthenticator(dir)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	verified, err := auth.Authenticate("Bearer old-token")
	if err != nil {
		t.Fatalf("Authenticate before the reload: %v", err)
	}

	writeTokenFile(t, dir, "read.dashboard.token", "new-token\n", 0o600)
	if err := auth.HandleSignal(syscall.SIGHUP); err != nil {
		t.Fatalf("HandleSignal(SIGHUP): %v", err)
	}

	if err := verified.Require(ClassRead); err != nil {
		t.Errorf("the identity a request verified before the reload no longer authorizes its call: %v", err)
	}
	if _, err := auth.Authenticate("Bearer old-token"); !errors.Is(err, ErrUnknownToken) {
		t.Errorf("removed token's next request error = %v, want ErrUnknownToken", err)
	}
	got, err := auth.Authenticate("Bearer new-token")
	if err != nil {
		t.Fatalf("new token after the reload: %v", err)
	}
	if got != (Identity{TokenRead, "dashboard"}) {
		t.Errorf("new token identity = %+v, want the read/dashboard file's identity", got)
	}

	writeTokenFile(t, dir, "read.dashboard.token", "third-token\n", 0o600)
	if err := auth.HandleSignal(syscall.SIGTERM); err != nil {
		t.Fatalf("HandleSignal(SIGTERM): %v", err)
	}
	if _, err := auth.Authenticate("Bearer new-token"); err != nil {
		t.Errorf("SIGTERM reloaded the table; only SIGHUP should: %v", err)
	}
}

// TestAuthReloadInFlightRequest is the acceptance criterion that an in-flight
// request finishes: a request paused inside the comparison of the table it
// loaded returns that table's identity after a reload has swapped in a table
// that no longer holds its token.
func TestAuthReloadInFlightRequest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTokenFile(t, dir, "read.dashboard.token", "old-token\n", 0o600)
	auth, err := NewAuthenticator(dir)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	ctx := waitContext(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	loaded := auth.table.Load()
	loaded.compare = func(a, b []byte) int {
		close(entered)
		<-release
		return subtle.ConstantTimeCompare(a, b)
	}

	type result struct {
		identity Identity
		err      error
	}
	done := make(chan result, 1)
	go func() {
		identity, err := auth.Authenticate("Bearer old-token")
		done <- result{identity: identity, err: err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("the in-flight request never reached the token comparison")
	}

	writeTokenFile(t, dir, "read.dashboard.token", "new-token\n", 0o600)
	if err := auth.Reload(); err != nil {
		t.Fatalf("Reload while a request is in flight: %v", err)
	}
	unblock()

	var got result
	select {
	case got = <-done:
	case <-ctx.Done():
		t.Fatal("the in-flight request did not finish after the reload")
	}
	if got.err != nil {
		t.Fatalf("the in-flight request failed after the reload: %v", got.err)
	}
	if want := (Identity{TokenRead, "dashboard"}); got.identity != want {
		t.Errorf("the in-flight request returned %+v, want %+v", got.identity, want)
	}
	if _, err := auth.Authenticate("Bearer old-token"); !errors.Is(err, ErrUnknownToken) {
		t.Errorf("removed token's next request error = %v, want ErrUnknownToken", err)
	}
}

// TestAuthRequestDuringReload is the other half of the atomic swap: a request
// that arrives while a reload is still reading the directory is served by the
// table the reload has not yet replaced, and the swap lands whole afterwards.
func TestAuthRequestDuringReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTokenFile(t, dir, "read.dashboard.token", "old-token\n", 0o600)
	auth, err := NewAuthenticator(dir)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	writeTokenFile(t, dir, "read.dashboard.token", "new-token\n", 0o600)
	ctx := waitContext(t)

	reading := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	auth.fs.open = func(path string) (tokenFile, error) {
		close(reading)
		<-release
		return openTokenFile(path)
	}
	reloaded := make(chan error, 1)
	go func() { reloaded <- auth.Reload() }()
	select {
	case <-reading:
	case <-ctx.Done():
		t.Fatal("the reload never reached the token file")
	}

	got, err := auth.Authenticate("Bearer old-token")
	if err != nil {
		t.Fatalf("a request during the reload: %v", err)
	}
	if want := (Identity{TokenRead, "dashboard"}); got != want {
		t.Errorf("a request during the reload returned %+v, want %+v", got, want)
	}

	unblock()
	select {
	case err := <-reloaded:
		if err != nil {
			t.Fatalf("Reload: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("the reload did not finish")
	}
	if _, err := auth.Authenticate("Bearer old-token"); !errors.Is(err, ErrUnknownToken) {
		t.Errorf("removed token's next request error = %v, want ErrUnknownToken", err)
	}
	got, err = auth.Authenticate("Bearer new-token")
	if err != nil {
		t.Fatalf("new token after the reload: %v", err)
	}
	if want := (Identity{TokenRead, "dashboard"}); got != want {
		t.Errorf("new token identity = %+v, want %+v", got, want)
	}
}

// TestAuthReloadFailureKeepsTable is the refusal branch: a rotation that leaves
// the directory unreadable refuses the reload and keeps the old table in
// service, rather than locking every caller out.
func TestAuthReloadFailureKeepsTable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeTokenFile(t, dir, "read.dashboard.token", "read-token\n", 0o600)
	auth, err := NewAuthenticator(dir)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := auth.HandleSignal(syscall.SIGHUP); !errors.Is(err, ErrTokenFileMode) {
		t.Fatalf("HandleSignal(SIGHUP) error = %v, want ErrTokenFileMode", err)
	}
	if _, err := auth.Authenticate("Bearer read-token"); err != nil {
		t.Errorf("the old table stopped serving after a failed reload: %v", err)
	}
}

// TestAuthWatchSignals drives the SIGHUP loop with a channel: closing it after
// the signals means WatchSignals has processed them when it returns.
func TestAuthWatchSignals(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTokenFile(t, dir, "read.dashboard.token", "old-token\n", 0o600)
	auth, err := NewAuthenticator(dir)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	writeTokenFile(t, dir, "read.dashboard.token", "new-token\n", 0o600)

	var reports []error
	sig := make(chan os.Signal, 2)
	sig <- syscall.SIGHUP
	sig <- syscall.SIGTERM
	close(sig)
	if err := auth.WatchSignals(context.Background(), sig, func(err error) { reports = append(reports, err) }); err != nil {
		t.Fatalf("WatchSignals with a report returned %v, want nil", err)
	}

	if len(reports) != 0 {
		t.Fatalf("WatchSignals reported %d error(s): %v", len(reports), reports)
	}
	if _, err := auth.Authenticate("Bearer old-token"); !errors.Is(err, ErrUnknownToken) {
		t.Errorf("SIGHUP from WatchSignals did not reload: old token error = %v", err)
	}
	if _, err := auth.Authenticate("Bearer new-token"); err != nil {
		t.Errorf("new token after WatchSignals reload: %v", err)
	}
}

// TestAuthWatchSignalsReportsReloadFailure checks a failed reload is reported
// and the loop keeps running, so the old table stays in service.
func TestAuthWatchSignalsReportsReloadFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeTokenFile(t, dir, "read.dashboard.token", "read-token\n", 0o600)
	auth, err := NewAuthenticator(dir)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	var reports []error
	sig := make(chan os.Signal, 1)
	sig <- syscall.SIGHUP
	close(sig)
	if err := auth.WatchSignals(context.Background(), sig, func(err error) { reports = append(reports, err) }); err != nil {
		t.Fatalf("WatchSignals with a report returned %v, want nil", err)
	}

	if len(reports) != 1 || !errors.Is(reports[0], ErrTokenFileMode) {
		t.Fatalf("WatchSignals reported %v, want one ErrTokenFileMode", reports)
	}
	if _, err := auth.Authenticate("Bearer read-token"); err != nil {
		t.Errorf("the old table stopped serving after a reported reload failure: %v", err)
	}
}

// TestAuthWatchSignalsNilReport checks a nil report does not swallow a failed
// reload: the loop ends and returns the error to the caller.
func TestAuthWatchSignalsNilReport(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeTokenFile(t, dir, "read.dashboard.token", "read-token\n", 0o600)
	auth, err := NewAuthenticator(dir)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	sig := make(chan os.Signal, 1)
	sig <- syscall.SIGHUP
	close(sig)
	if err := auth.WatchSignals(context.Background(), sig, nil); !errors.Is(err, ErrTokenFileMode) {
		t.Fatalf("WatchSignals with no report returned %v, want ErrTokenFileMode", err)
	}
}

// TestAuthWatchSignalsContext checks the loop ends on its context, and that
// the wait for it is bounded.
func TestAuthWatchSignalsContext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTokenFile(t, dir, "read.dashboard.token", "read-token\n", 0o600)
	auth, err := NewAuthenticator(dir)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	ctx := waitContext(t)
	watchCtx, stop := context.WithCancel(context.Background())
	stop()
	done := make(chan error, 1)
	go func() {
		done <- auth.WatchSignals(watchCtx, make(chan os.Signal), nil)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WatchSignals after its context was cancelled: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("WatchSignals did not return after its context was cancelled")
	}
}

// TestAuthConcurrentReload runs two reloads at once: Reload serializes them, so
// both succeed and the table afterwards serves the directory.
func TestAuthConcurrentReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTokenFile(t, dir, "read.dashboard.token", "read-token\n", 0o600)
	auth, err := NewAuthenticator(dir)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	ctx := waitContext(t)
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- auth.Reload() }()
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("concurrent Reload: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("a concurrent Reload did not finish")
		}
	}
	got, err := auth.Authenticate("Bearer read-token")
	if err != nil {
		t.Fatalf("Authenticate after concurrent reloads: %v", err)
	}
	if want := (Identity{TokenRead, "dashboard"}); got != want {
		t.Errorf("identity = %+v, want %+v", got, want)
	}
}

// TestAuthCallerLengthBound is the boundary of the caller part of a token file
// name: 64 bytes is a caller, 65 is refused as a file name.
func TestAuthCallerLengthBound(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		caller  string
		wantErr error
	}{
		{"64 byte caller", strings.Repeat("a", maxCallerBytes), nil},
		{"65 byte caller", strings.Repeat("a", maxCallerBytes+1), ErrTokenFileName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTokenFile(t, dir, "read."+tt.caller+".token", "read-token\n", 0o600)
			auth, err := NewAuthenticator(dir)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("NewAuthenticator error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewAuthenticator: %v", err)
			}
			got, err := auth.Authenticate("Bearer read-token")
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if got.Caller != tt.caller {
				t.Errorf("caller = %q, want the file name's %q", got.Caller, tt.caller)
			}
		})
	}
}

// TestAuthLoaderRefusals drives every refusal branch of the token directory
// loader.
func TestAuthLoaderRefusals(t *testing.T) {
	t.Parallel()
	emptyDir := func(t *testing.T) string { return t.TempDir() }
	tests := []struct {
		name    string
		setup   func(t *testing.T) string
		wantErr error
	}{
		{
			name: "missing directory",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "absent")
			},
			wantErr: ErrTokenDir,
		},
		{
			name: "directory is a file",
			setup: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "tokens")
				if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
				return path
			},
			wantErr: ErrTokenDir,
		},
		{
			name:    "empty directory",
			setup:   emptyDir,
			wantErr: ErrNoTokens,
		},
		{
			name: "only files that are not token files",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeTokenFile(t, dir, "README", "tokens live here\n", 0o600)
				writeTokenFile(t, dir, "notes.txt", "not a token\n", 0o600)
				return dir
			},
			wantErr: ErrNoTokens,
		},
		{
			name: "unknown class in the file name",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeTokenFile(t, dir, "admin.dashboard.token", "admin-token\n", 0o600)
				return dir
			},
			wantErr: ErrUnknownTokenClass,
		},
		{
			name: "file name with no caller",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeTokenFile(t, dir, "read.token", "read-token\n", 0o600)
				return dir
			},
			wantErr: ErrTokenFileName,
		},
		{
			name: "file name with an empty caller",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeTokenFile(t, dir, "read..token", "read-token\n", 0o600)
				return dir
			},
			wantErr: ErrTokenFileName,
		},
		{
			name: "caller with a space",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeTokenFile(t, dir, "read.dash board.token", "read-token\n", 0o600)
				return dir
			},
			wantErr: ErrTokenFileName,
		},
		{
			name: "caller with a dot",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeTokenFile(t, dir, "read.dash.board.token", "read-token\n", 0o600)
				return dir
			},
			wantErr: ErrTokenFileName,
		},
		{
			name: "caller with an uppercase letter",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeTokenFile(t, dir, "read.Dashboard.token", "read-token\n", 0o600)
				return dir
			},
			wantErr: ErrTokenFileName,
		},
		{
			name: "token file is a symlink",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				target := writeTokenFile(t, t.TempDir(), "real-token", "read-token\n", 0o600)
				if err := os.Symlink(target, filepath.Join(dir, "read.dashboard.token")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return dir
			},
			wantErr: ErrTokenFile,
		},
		{
			name: "token file is a directory",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.Mkdir(filepath.Join(dir, "read.dashboard.token"), 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				return dir
			},
			wantErr: ErrTokenFile,
		},
		{
			name: "empty token file",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeTokenFile(t, dir, "read.dashboard.token", "", 0o600)
				return dir
			},
			wantErr: ErrEmptyToken,
		},
		{
			name: "whitespace-only token file",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeTokenFile(t, dir, "read.dashboard.token", " \n\t\n", 0o600)
				return dir
			},
			wantErr: ErrEmptyToken,
		},
		{
			name: "two files holding the same token",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeTokenFile(t, dir, "daemon.alpha.token", "shared-token\n", 0o600)
				writeTokenFile(t, dir, "operator.beta.token", "shared-token\n", 0o600)
				return dir
			},
			wantErr: ErrDuplicateToken,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewAuthenticator(tt.setup(t))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("NewAuthenticator error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// fakeDirEntry is the one directory entry the injected-failure tests list.
type fakeDirEntry struct{ name string }

func (e fakeDirEntry) Name() string               { return e.name }
func (e fakeDirEntry) IsDir() bool                { return false }
func (e fakeDirEntry) Type() fs.FileMode          { return 0 }
func (e fakeDirEntry) Info() (fs.FileInfo, error) { return nil, errors.New("unused") }

// fakeTokenFile is one open token file for the injected-failure tests: Stat and
// Read report the failures a real file cannot be made to produce on demand.
type fakeTokenFile struct {
	info     os.FileInfo
	statErr  error
	data     []byte
	readErr  error
	closeErr error
	off      int
}

func (f *fakeTokenFile) Stat() (os.FileInfo, error) {
	if f.statErr != nil {
		return nil, f.statErr
	}
	return f.info, nil
}

func (f *fakeTokenFile) Read(p []byte) (int, error) {
	if f.readErr != nil {
		return 0, f.readErr
	}
	if f.off >= len(f.data) {
		return 0, io.EOF
	}
	n := copy(p, f.data[f.off:])
	f.off += n
	return n, nil
}

func (f *fakeTokenFile) Close() error { return f.closeErr }

// TestAuthLoaderIOFailures drives the loader's I/O refusal branches through an
// injected filesystem: a temp directory cannot be made to fail a stat, a
// listing, an open or a read on demand, and each refusal must still be tested.
func TestAuthLoaderIOFailures(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat the token directory: %v", err)
	}
	tokenPath := writeTokenFile(t, dir, "unused", "unused\n", 0o600)
	tokenInfo, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("stat a 0600 file: %v", err)
	}
	oneEntry := func(string) ([]os.DirEntry, error) {
		return []os.DirEntry{fakeDirEntry{name: "read.dashboard.token"}}, nil
	}
	dirOnly := func(string) (os.FileInfo, error) { return dirInfo, nil }

	tests := []struct {
		name    string
		fs      tokenFS
		wantErr error
	}{
		{
			name: "statting the directory fails",
			fs: tokenFS{
				lstat: func(string) (os.FileInfo, error) { return nil, errors.New("lstat failed") },
			},
			wantErr: ErrTokenDir,
		},
		{
			name: "listing the directory fails",
			fs: tokenFS{
				lstat:   dirOnly,
				readDir: func(string) ([]os.DirEntry, error) { return nil, errors.New("readdir failed") },
			},
			wantErr: ErrTokenDir,
		},
		{
			name: "opening a token file fails",
			fs: tokenFS{
				lstat:   dirOnly,
				readDir: oneEntry,
				open:    func(string) (tokenFile, error) { return nil, errors.New("open failed") },
			},
			wantErr: ErrTokenFile,
		},
		{
			name: "statting an opened token file fails",
			fs: tokenFS{
				lstat:   dirOnly,
				readDir: oneEntry,
				open: func(string) (tokenFile, error) {
					return &fakeTokenFile{statErr: errors.New("fstat failed")}, nil
				},
			},
			wantErr: ErrTokenFile,
		},
		{
			name: "reading a token file fails",
			fs: tokenFS{
				lstat:   dirOnly,
				readDir: oneEntry,
				open: func(string) (tokenFile, error) {
					return &fakeTokenFile{info: tokenInfo, readErr: errors.New("read failed")}, nil
				},
			},
			wantErr: ErrTokenFile,
		},
		{
			name: "closing a token file fails",
			fs: tokenFS{
				lstat:   dirOnly,
				readDir: oneEntry,
				open: func(string) (tokenFile, error) {
					return &fakeTokenFile{info: tokenInfo, data: []byte("a-token\n"), closeErr: errors.New("close failed")}, nil
				},
			},
			wantErr: ErrTokenFile,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadTokens(tt.fs, dir)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("loadTokens error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestAuthZeroValueRefuses is the fail-closed branch for an Authenticator no
// constructor built: it has no table, so it must refuse rather than allow.
func TestAuthZeroValueRefuses(t *testing.T) {
	t.Parallel()
	var auth Authenticator
	if _, err := auth.Authenticate("Bearer anything"); !errors.Is(err, ErrUnknownToken) {
		t.Errorf("zero Authenticator error = %v, want ErrUnknownToken", err)
	}
}

// TestAuthRequire is the 403 path: a token class that may not run the verb's
// class is denied, and that refusal maps to 403.
func TestAuthRequire(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		identity Identity
		required VerbClass
		wantErr  bool
	}{
		{"read may read", Identity{Class: TokenRead}, ClassRead, false},
		{"read may not write", Identity{Class: TokenRead}, ClassWrite, true},
		{"read may not administer", Identity{Class: TokenRead}, ClassAdmin, true},
		{"daemon may read", Identity{Class: TokenDaemon}, ClassRead, false},
		{"daemon may write", Identity{Class: TokenDaemon}, ClassWrite, false},
		{"daemon may not administer", Identity{Class: TokenDaemon}, ClassAdmin, true},
		{"operator may read", Identity{Class: TokenOperator}, ClassRead, false},
		{"operator may write", Identity{Class: TokenOperator}, ClassWrite, false},
		{"operator may administer", Identity{Class: TokenOperator}, ClassAdmin, false},
		{"zero identity may do nothing", Identity{}, ClassRead, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.identity.Require(tt.required)
			if tt.wantErr {
				if !errors.Is(err, ErrClassDenied) {
					t.Fatalf("Require(%q) error = %v, want ErrClassDenied", tt.required, err)
				}
				if status := HTTPStatus(err); status != http.StatusForbidden {
					t.Errorf("HTTPStatus(%v) = %d, want 403", err, status)
				}
				return
			}
			if err != nil {
				t.Fatalf("Require(%q): %v", tt.required, err)
			}
		})
	}
}

// TestParseBearer drives the header shapes the gateway must refuse.
func TestParseBearer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		header  string
		want    string
		wantErr error
	}{
		{"lowercase scheme", "bearer abc", "abc", nil},
		{"uppercase scheme", "BEARER abc", "abc", nil},
		{"surrounding spaces trimmed", "Bearer  abc  ", "abc", nil},
		{"empty header", "", "", ErrNoToken},
		{"scheme only", "Bearer", "", ErrNoToken},
		{"scheme and space only", "Bearer ", "", ErrNoToken},
		{"another scheme", "Basic abc", "", ErrNoToken},
		{"tab after scheme", "Bearer\tabc", "", ErrNoToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseBearer(tt.header)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ParseBearer(%q) error = %v, want %v", tt.header, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseBearer(%q): %v", tt.header, err)
			}
			if got != tt.want {
				t.Errorf("ParseBearer(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}

// TestHTTPStatus pins the status each refusal maps to, including the unknown
// error that must never read as success.
func TestHTTPStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"no error", nil, http.StatusOK},
		{"no token", fmt.Errorf("wrapped: %w", ErrNoToken), http.StatusUnauthorized},
		{"unknown token", ErrUnknownToken, http.StatusUnauthorized},
		{"class denied", fmt.Errorf("wrapped: %w", ErrClassDenied), http.StatusForbidden},
		{"verb not allowed", ErrVerbNotAllowed, http.StatusForbidden},
		{"flag refused", ErrFlagRefused, http.StatusForbidden},
		{"unknown failure", errors.New("disk on fire"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HTTPStatus(tt.err); got != tt.want {
				t.Errorf("HTTPStatus(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}
