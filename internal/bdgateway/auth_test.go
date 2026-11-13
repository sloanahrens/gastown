package bdgateway

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
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

// TestAuthTokens is the acceptance criterion that a valid token resolves to its
// class and caller, and that an unknown or empty token is 401.
func TestAuthTokens(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTokenFile(t, dir, "read.token", "read-token\n", 0o600)
	writeTokenFile(t, dir, "daemon.token", "daemon-token\n", 0o600)
	writeTokenFile(t, dir, "operator.token", "operator-token\n", 0o600)
	auth, err := NewAuthenticator(dir)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	tests := []struct {
		name      string
		header    string
		caller    string
		wantClass Class
		wantErr   error
	}{
		{"read token", "Bearer read-token", "dashboard", ClassRead, nil},
		{"daemon token", "Bearer daemon-token", "daemon.spec-tick", ClassDaemon, nil},
		{"operator token", "Bearer operator-token", "mac.sloan-shell", ClassOperator, nil},
		{"no header", "", "dashboard", "", ErrNoToken},
		{"bearer with no token", "Bearer ", "dashboard", "", ErrNoToken},
		{"another scheme", "Basic dXNlcjpwYXNz", "dashboard", "", ErrNoToken},
		{"unknown token", "Bearer not-a-token", "dashboard", "", ErrUnknownToken},
		{"prefix of a valid token", "Bearer read", "dashboard", "", ErrUnknownToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := auth.Authenticate(tt.header, tt.caller)
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
			if got.Class != tt.wantClass {
				t.Errorf("class = %q, want %q", got.Class, tt.wantClass)
			}
			if got.Caller != tt.caller {
				t.Errorf("caller = %q, want %q", got.Caller, tt.caller)
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
		{"0000 no access", 0o000, ErrTokenFileMode},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTokenFile(t, dir, "read.token", token+"\n", tt.mode)
			auth, err := NewAuthenticator(dir)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("NewAuthenticator: %v", err)
				}
				if _, err := auth.Authenticate("Bearer "+token, "dashboard"); err != nil {
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
			if !strings.Contains(err.Error(), "read.token") {
				t.Errorf("error does not name the file: %v", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%04o", tt.mode)) {
				t.Errorf("error does not name the mode: %v", err)
			}
		})
	}
}

// TestAuthConstantTime is the acceptance criterion that the comparison uses
// crypto/subtle.ConstantTimeCompare, asserted by a test that fails if the
// compare is replaced. Constant time is not observable from outside the
// process, so the test pins the call in TokenTable.Lookup, where the compare
// lives: replacing it with == makes this test fail.
func TestAuthConstantTime(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "auth.go", nil, 0)
	if err != nil {
		t.Fatalf("parse auth.go: %v", err)
	}
	var lookup *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Lookup" || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		recv := fn.Recv.List[0].Type
		if star, ok := recv.(*ast.StarExpr); ok {
			recv = star.X
		}
		if ident, ok := recv.(*ast.Ident); ok && ident.Name == "TokenTable" {
			lookup = fn
		}
	}
	if lookup == nil {
		t.Fatal("auth.go has no TokenTable.Lookup method to pin")
	}
	found := false
	ast.Inspect(lookup.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if ok && pkg.Name == "subtle" && sel.Sel.Name == "ConstantTimeCompare" {
			found = true
		}
		return true
	})
	if !found {
		t.Fatal("TokenTable.Lookup does not call subtle.ConstantTimeCompare; a token compare that can exit on the first differing byte leaks how much matched")
	}
}

// TestAuthReload is the acceptance criterion that a reload swaps the table
// atomically: a token removed by the reload is 401 on its next request, and a
// request that authenticated before the reload finishes on the identity it
// already holds.
func TestAuthReload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTokenFile(t, dir, "read.token", "old-token\n", 0o600)
	auth, err := NewAuthenticator(dir)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	inFlight, err := auth.Authenticate("Bearer old-token", "dashboard")
	if err != nil {
		t.Fatalf("Authenticate before the reload: %v", err)
	}

	writeTokenFile(t, dir, "read.token", "new-token\n", 0o600)
	if err := auth.HandleSignal(syscall.SIGHUP); err != nil {
		t.Fatalf("HandleSignal(SIGHUP): %v", err)
	}

	if err := inFlight.Require(ClassRead); err != nil {
		t.Errorf("the identity a request authenticated with before the reload no longer authorizes its call: %v", err)
	}
	if _, err := auth.Authenticate("Bearer old-token", "dashboard"); !errors.Is(err, ErrUnknownToken) {
		t.Errorf("removed token's next request error = %v, want ErrUnknownToken", err)
	}
	got, err := auth.Authenticate("Bearer new-token", "dashboard")
	if err != nil {
		t.Fatalf("new token after the reload: %v", err)
	}
	if got.Class != ClassRead {
		t.Errorf("new token class = %q, want %q", got.Class, ClassRead)
	}

	writeTokenFile(t, dir, "read.token", "third-token\n", 0o600)
	if err := auth.HandleSignal(syscall.SIGTERM); err != nil {
		t.Fatalf("HandleSignal(SIGTERM): %v", err)
	}
	if _, err := auth.Authenticate("Bearer new-token", "dashboard"); err != nil {
		t.Errorf("SIGTERM reloaded the table; only SIGHUP should: %v", err)
	}
}

// TestAuthReloadFailureKeepsTable is the refusal branch: a rotation that leaves
// the directory unreadable refuses the reload and keeps the old table in
// service, rather than locking every caller out.
func TestAuthReloadFailureKeepsTable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeTokenFile(t, dir, "read.token", "read-token\n", 0o600)
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
	if _, err := auth.Authenticate("Bearer read-token", "dashboard"); err != nil {
		t.Errorf("the old table stopped serving after a failed reload: %v", err)
	}
}

// TestAuthWatchSignals drives the SIGHUP loop with a channel: closing it after
// the signals means WatchSignals has processed them when it returns.
func TestAuthWatchSignals(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTokenFile(t, dir, "read.token", "old-token\n", 0o600)
	auth, err := NewAuthenticator(dir)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	writeTokenFile(t, dir, "read.token", "new-token\n", 0o600)

	var reports []error
	sig := make(chan os.Signal, 2)
	sig <- syscall.SIGHUP
	sig <- syscall.SIGTERM
	close(sig)
	auth.WatchSignals(context.Background(), sig, func(err error) { reports = append(reports, err) })

	if len(reports) != 0 {
		t.Fatalf("WatchSignals reported %d error(s): %v", len(reports), reports)
	}
	if _, err := auth.Authenticate("Bearer old-token", "dashboard"); !errors.Is(err, ErrUnknownToken) {
		t.Errorf("SIGHUP from WatchSignals did not reload: old token error = %v", err)
	}
	if _, err := auth.Authenticate("Bearer new-token", "dashboard"); err != nil {
		t.Errorf("new token after WatchSignals reload: %v", err)
	}
}

// TestAuthWatchSignalsReportsReloadFailure checks a failed reload is reported
// and the loop keeps running, so the old table stays in service.
func TestAuthWatchSignalsReportsReloadFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := writeTokenFile(t, dir, "read.token", "read-token\n", 0o600)
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
	auth.WatchSignals(context.Background(), sig, func(err error) { reports = append(reports, err) })

	if len(reports) != 1 || !errors.Is(reports[0], ErrTokenFileMode) {
		t.Fatalf("WatchSignals reported %v, want one ErrTokenFileMode", reports)
	}
	if _, err := auth.Authenticate("Bearer read-token", "dashboard"); err != nil {
		t.Errorf("the old table stopped serving after a reported reload failure: %v", err)
	}
}

// TestAuthWatchSignalsContext checks the loop ends on its context.
func TestAuthWatchSignalsContext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTokenFile(t, dir, "read.token", "read-token\n", 0o600)
	auth, err := NewAuthenticator(dir)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		auth.WatchSignals(ctx, make(chan os.Signal), nil)
	}()
	<-done
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
				writeTokenFile(t, dir, "admin.token", "admin-token\n", 0o600)
				return dir
			},
			wantErr: ErrUnknownClass,
		},
		{
			name: "token file is a symlink",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				target := writeTokenFile(t, t.TempDir(), "real-token", "read-token\n", 0o600)
				if err := os.Symlink(target, filepath.Join(dir, "read.token")); err != nil {
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
				if err := os.Mkdir(filepath.Join(dir, "read.token"), 0o700); err != nil {
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
				writeTokenFile(t, dir, "read.token", "", 0o600)
				return dir
			},
			wantErr: ErrEmptyToken,
		},
		{
			name: "whitespace-only token file",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeTokenFile(t, dir, "read.token", " \n\t\n", 0o600)
				return dir
			},
			wantErr: ErrEmptyToken,
		},
		{
			name: "two classes holding the same token",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeTokenFile(t, dir, "daemon.token", "shared-token\n", 0o600)
				writeTokenFile(t, dir, "operator.token", "shared-token\n", 0o600)
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

// TestAuthLoaderIOFailures drives the loader's I/O refusal branches through an
// injected filesystem: a temp directory cannot be made to fail a stat, a
// listing or a read on demand, and each refusal must still be tested.
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
		return []os.DirEntry{fakeDirEntry{name: "read.token"}}, nil
	}

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
				lstat:   func(string) (os.FileInfo, error) { return dirInfo, nil },
				readDir: func(string) ([]os.DirEntry, error) { return nil, errors.New("readdir failed") },
			},
			wantErr: ErrTokenDir,
		},
		{
			name: "statting a token file fails",
			fs: tokenFS{
				lstat: func(name string) (os.FileInfo, error) {
					if name == dir {
						return dirInfo, nil
					}
					return nil, errors.New("lstat failed")
				},
				readDir: oneEntry,
			},
			wantErr: ErrTokenFile,
		},
		{
			name: "reading a token file fails",
			fs: tokenFS{
				lstat: func(name string) (os.FileInfo, error) {
					if name == dir {
						return dirInfo, nil
					}
					return tokenInfo, nil
				},
				readDir:  oneEntry,
				readFile: func(string) ([]byte, error) { return nil, errors.New("read failed") },
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
	if _, err := auth.Authenticate("Bearer anything", "caller"); !errors.Is(err, ErrUnknownToken) {
		t.Errorf("zero Authenticator error = %v, want ErrUnknownToken", err)
	}
}

// TestAuthClassAllows pins the class hierarchy: daemon may do what read may,
// operator what daemon may, and an unknown class may do nothing.
func TestAuthClassAllows(t *testing.T) {
	t.Parallel()
	tests := []struct {
		held     Class
		required Class
		want     bool
	}{
		{ClassRead, ClassRead, true},
		{ClassRead, ClassDaemon, false},
		{ClassRead, ClassOperator, false},
		{ClassDaemon, ClassRead, true},
		{ClassDaemon, ClassDaemon, true},
		{ClassDaemon, ClassOperator, false},
		{ClassOperator, ClassRead, true},
		{ClassOperator, ClassDaemon, true},
		{ClassOperator, ClassOperator, true},
		{Class("admin"), ClassRead, false},
		{ClassRead, Class("admin"), false},
		{ClassRead, Class(""), false},
	}
	for _, tt := range tests {
		t.Run(string(tt.held)+"->"+string(tt.required), func(t *testing.T) {
			if got := tt.held.Allows(tt.required); got != tt.want {
				t.Errorf("%q.Allows(%q) = %v, want %v", tt.held, tt.required, got, tt.want)
			}
		})
	}
}

// TestAuthRequire is the 403 path: a class that may not run the verb is
// forbidden, and that refusal maps to 403.
func TestAuthRequire(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		identity Identity
		required Class
		wantErr  bool
	}{
		{"read may read", Identity{Class: ClassRead}, ClassRead, false},
		{"read may not write", Identity{Class: ClassRead}, ClassDaemon, true},
		{"daemon may read", Identity{Class: ClassDaemon}, ClassRead, false},
		{"daemon may not configure", Identity{Class: ClassDaemon}, ClassOperator, true},
		{"operator may configure", Identity{Class: ClassOperator}, ClassOperator, false},
		{"zero identity may do nothing", Identity{}, ClassRead, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.identity.Require(tt.required)
			if tt.wantErr {
				if !errors.Is(err, ErrForbidden) {
					t.Fatalf("Require(%q) error = %v, want ErrForbidden", tt.required, err)
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
		{"forbidden", fmt.Errorf("wrapped: %w", ErrForbidden), http.StatusForbidden},
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
