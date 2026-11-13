package bdgateway

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
)

// Class is a gateway token class: what a caller holding the token may run.
// The classes and what each may run are described in docs/design/bd-container.md,
// "Token classes".
type Class string

const (
	// ClassRead is the dashboard and monitor class: the read verbs only.
	ClassRead Class = "read"
	// ClassDaemon is the daemon, landing worker and plugin class: the read
	// verbs and the write verbs gt makes.
	ClassDaemon Class = "daemon"
	// ClassOperator is the Mac wrapper class: everything daemon may, plus
	// `config set` for issue_prefix.
	ClassOperator Class = "operator"
)

// rank orders the classes by what they may run, and is 0 for a class this
// package does not know, so an unknown class allows nothing.
func (c Class) rank() int {
	switch c {
	case ClassRead:
		return 1
	case ClassDaemon:
		return 2
	case ClassOperator:
		return 3
	default:
		return 0
	}
}

// Allows reports whether a caller of class c may run what required requires.
// An unknown class, held or required, allows nothing.
func (c Class) Allows(required Class) bool {
	r := required.rank()
	return r > 0 && c.rank() >= r
}

// ParseClass returns the class called name, or ErrUnknownClass.
func ParseClass(name string) (Class, error) {
	c := Class(name)
	if c.rank() == 0 {
		return "", fmt.Errorf("%w: %q", ErrUnknownClass, name)
	}
	return c, nil
}

// Identity is what a request's bearer token authenticates to: the token's
// class, and the caller's own name from the X-Gt-Caller header. Caller is free
// text (docs/design/bd-container.md, "The call log"), so only Class decides
// what the request may run.
type Identity struct {
	Class  Class
	Caller string
}

// Require returns ErrForbidden unless the identity's class may run what
// required names. The gateway answers the refusal with 403.
func (i Identity) Require(required Class) error {
	if !i.Class.Allows(required) {
		return fmt.Errorf("%w: class %q may not run a %q call", ErrForbidden, i.Class, required)
	}
	return nil
}

// The authenticator's named refusals. Callers match on the error rather than on
// message text; none carries a token or part of one.
var (
	// ErrNoToken is ParseBearer's and Authenticate's refusal of a request with
	// no bearer token: no Authorization header, another scheme, or an empty
	// token.
	ErrNoToken = errors.New("bdgateway: no bearer token")
	// ErrUnknownToken is Authenticate's refusal of a well-formed token no file
	// holds.
	ErrUnknownToken = errors.New("bdgateway: unknown bearer token")
	// ErrForbidden is Require's refusal of a class that may not run the call.
	ErrForbidden = errors.New("bdgateway: class may not run this call")
	// ErrUnknownClass is LoadTokens' refusal of a token file whose name is not
	// one of the classes.
	ErrUnknownClass = errors.New("bdgateway: unknown token class")
	// ErrTokenDir is LoadTokens' refusal of a directory it cannot list: missing,
	// not a directory, or a symlink.
	ErrTokenDir = errors.New("bdgateway: token directory is not usable")
	// ErrTokenFile is LoadTokens' refusal of a token file that is not a
	// readable regular file.
	ErrTokenFile = errors.New("bdgateway: token file is not usable")
	// ErrTokenFileMode is LoadTokens' refusal of a token file whose mode is not
	// exactly 0600.
	ErrTokenFileMode = errors.New("bdgateway: token file mode is not 0600")
	// ErrEmptyToken is LoadTokens' refusal of a file that holds no token.
	ErrEmptyToken = errors.New("bdgateway: token file holds no token")
	// ErrDuplicateToken is LoadTokens' refusal of two files holding the same
	// token: a token must name one class, or the class it resolves to would
	// depend on file order.
	ErrDuplicateToken = errors.New("bdgateway: two token files hold the same token")
	// ErrNoTokens is LoadTokens' refusal of a directory holding no token file.
	// An authenticator with no tokens refuses every request, so the gateway
	// refuses to start instead of running uselessly.
	ErrNoTokens = errors.New("bdgateway: token directory holds no tokens")
)

// TokenTable is one immutable snapshot of the token directory: one token per
// class, each read from a `<class>.token` file.
type TokenTable struct {
	entries []tokenEntry
}

type tokenEntry struct {
	class Class
	name  string
	token []byte
}

// tokenFS is the file operations the token loader performs. Production uses
// osTokenFS; a test injects one whose calls report the I/O failures a real
// directory cannot be made to produce on demand, so every refusal branch has a
// test.
type tokenFS struct {
	lstat    func(string) (os.FileInfo, error)
	readDir  func(string) ([]os.DirEntry, error)
	readFile func(string) ([]byte, error)
}

// osTokenFS is tokenFS over the real filesystem.
var osTokenFS = tokenFS{lstat: os.Lstat, readDir: os.ReadDir, readFile: os.ReadFile}

// LoadTokens reads dir's `<class>.token` files into a new TokenTable. The
// caller name is not part of the file name: it is the request's X-Gt-Caller
// header and reaches the Authenticator beside the token. Non-token files
// (README, editor droppings) are ignored. Every refusal names the file or
// directory and what was wrong; none echoes a token.
func LoadTokens(dir string) (*TokenTable, error) {
	return loadTokens(osTokenFS, dir)
}

// loadTokens is LoadTokens over fs, so tests can drive the failures the
// filesystem will not produce on demand.
func loadTokens(fs tokenFS, dir string) (*TokenTable, error) {
	info, err := fs.lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("%w: %s does not exist", ErrTokenDir, dir)
	case err != nil:
		return nil, fmt.Errorf("%w: %s: %v", ErrTokenDir, dir, err)
	case !info.IsDir():
		return nil, fmt.Errorf("%w: %s is not a real directory (a symlink or a file)", ErrTokenDir, dir)
	}
	dirents, err := fs.readDir(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %v", ErrTokenDir, dir, err)
	}
	table := &TokenTable{}
	for _, dirent := range dirents {
		name := dirent.Name()
		if !strings.HasSuffix(name, ".token") {
			continue
		}
		class, err := ParseClass(strings.TrimSuffix(name, ".token"))
		if err != nil {
			return nil, fmt.Errorf("%w: file %s", err, name)
		}
		token, err := readTokenFile(fs, filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		for _, prev := range table.entries {
			if subtle.ConstantTimeCompare(prev.token, token) == 1 {
				return nil, fmt.Errorf("%w: %s and %s", ErrDuplicateToken, name, prev.name)
			}
		}
		table.entries = append(table.entries, tokenEntry{class: class, name: name, token: token})
	}
	if len(table.entries) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoTokens, dir)
	}
	return table, nil
}

// readTokenFile returns the token path holds. It checks the file with Lstat,
// so a symlink is refused rather than followed, and requires mode 0600 exactly:
// a token readable by anyone else is refused with the mode named and the token
// left unread. Surrounding whitespace is trimmed, so a file written with a
// trailing newline works.
func readTokenFile(fs tokenFS, path string) ([]byte, error) {
	info, err := fs.lstat(path)
	switch {
	case err != nil:
		return nil, fmt.Errorf("%w: %s: %v", ErrTokenFile, path, err)
	case !info.Mode().IsRegular():
		return nil, fmt.Errorf("%w: %s is not a regular file (a symlink or a directory)", ErrTokenFile, path)
	case info.Mode().Perm() != 0o600:
		return nil, fmt.Errorf("%w: %s has mode %04o", ErrTokenFileMode, path, info.Mode().Perm())
	}
	b, err := fs.readFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %v", ErrTokenFile, path, err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return nil, fmt.Errorf("%w: %s", ErrEmptyToken, path)
	}
	return []byte(token), nil
}

// Lookup returns the class token authenticates to, and false for a token no
// entry holds. Every entry is compared with crypto/subtle.ConstantTimeCompare
// and the loop does not stop at a match, so a request's duration does not say
// which class, if any, matched. Duplicate tokens are refused at load, so at
// most one entry can match.
func (t *TokenTable) Lookup(token string) (Class, bool) {
	var match Class
	found := 0
	for _, entry := range t.entries {
		if subtle.ConstantTimeCompare(entry.token, []byte(token)) == 1 {
			match = entry.class
			found = 1
		}
	}
	return match, found == 1
}

// Authenticator holds the current token table behind an atomic pointer, so a
// reload swaps it without a request ever seeing no table or a half-built one.
type Authenticator struct {
	dir   string
	table atomic.Pointer[TokenTable]
}

// NewAuthenticator loads dir's tokens and returns an Authenticator. A
// directory it cannot load is an error: the gateway refuses to start with no
// usable token table rather than come up refusing every request.
func NewAuthenticator(dir string) (*Authenticator, error) {
	a := &Authenticator{dir: dir}
	if err := a.Reload(); err != nil {
		return nil, err
	}
	return a, nil
}

// Reload builds a table from the token directory and swaps it in. A failed
// load leaves the current table in place, so a bad rotation refuses the reload
// instead of locking every caller out.
func (a *Authenticator) Reload() error {
	table, err := LoadTokens(a.dir)
	if err != nil {
		return err
	}
	a.table.Store(table)
	return nil
}

// Authenticate returns the identity authorization authenticates to. The caller
// is the request's X-Gt-Caller value and is carried without being trusted. A
// missing, malformed or empty bearer token is ErrNoToken; a well-formed token
// no file holds is ErrUnknownToken. Both are 401 (see HTTPStatus).
func (a *Authenticator) Authenticate(authorization, caller string) (Identity, error) {
	token, err := ParseBearer(authorization)
	if err != nil {
		return Identity{}, err
	}
	table := a.table.Load()
	if table == nil {
		return Identity{}, fmt.Errorf("%w: no token table loaded", ErrUnknownToken)
	}
	class, ok := table.Lookup(token)
	if !ok {
		return Identity{}, ErrUnknownToken
	}
	return Identity{Class: class, Caller: caller}, nil
}

// ParseBearer returns the token an Authorization header carries. The scheme is
// matched without case, as RFC 6750 says, and the token must be non-empty;
// anything else is ErrNoToken, so a malformed header never becomes a
// credential.
func ParseBearer(header string) (string, error) {
	const scheme = "bearer "
	if len(header) < len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", fmt.Errorf("%w: Authorization is not a bearer token", ErrNoToken)
	}
	token := strings.TrimSpace(header[len(scheme):])
	if token == "" {
		return "", fmt.Errorf("%w: empty bearer token", ErrNoToken)
	}
	return token, nil
}

// HTTPStatus maps an authentication or class refusal to the status the gateway
// answers before any frame: 200 for no error, 401 for no or unknown token, 403
// for a class that may not run the verb, and 500 for a failure this package
// does not know. An unrecognized failure must not read as a success.
func HTTPStatus(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, ErrNoToken), errors.Is(err, ErrUnknownToken):
		return http.StatusUnauthorized
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden
	default:
		return http.StatusInternalServerError
	}
}

// HandleSignal reloads the token table when sig is SIGHUP and ignores every
// other signal. Rotation is therefore a file replace followed by SIGHUP, with
// no restart (docs/design/bd-container.md, "Token classes").
func (a *Authenticator) HandleSignal(sig os.Signal) error {
	if sig != syscall.SIGHUP {
		return nil
	}
	return a.Reload()
}

// WatchSignals calls HandleSignal for each signal read from sig until ctx is
// done or sig is closed. A failed reload is reported and does not stop the
// loop: the old table stays in service until a later reload succeeds. The
// process's own signal channel and os/signal.Notify are the gateway's to wire
// (cmd/bdgw, task G6).
func (a *Authenticator) WatchSignals(ctx context.Context, sig <-chan os.Signal, report func(error)) {
	for {
		select {
		case <-ctx.Done():
			return
		case s, ok := <-sig:
			if !ok {
				return
			}
			if err := a.HandleSignal(s); err != nil && report != nil {
				report(err)
			}
		}
	}
}
