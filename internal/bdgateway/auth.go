package bdgateway

import (
	"context"
	"crypto/sha256"
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

// Identity is what a bearer token authenticates to, both halves read from the
// token's file name (docs/design/bd-container.md, "Token classes").
type Identity struct {
	Class  TokenClass
	Caller string
}

// Require returns ErrClassDenied unless the identity's token class may run a
// verb of verbClass. The gateway answers the refusal with 403.
func (i Identity) Require(verbClass VerbClass) error {
	if !Allowed(i.Class, verbClass) {
		return fmt.Errorf("%w: %s token may not run a %s verb", ErrClassDenied, i.Class, verbClass)
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
	// ErrTokenFileName is LoadTokens' refusal of a token file name that is not
	// <class>.<caller>.token, or whose caller is not 1 to 64 lowercase letters,
	// digits, '-' or '_'.
	ErrTokenFileName = errors.New("bdgateway: token file name must be <class>.<caller>.token")
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
	// token: a token must name one identity, or which class and caller it
	// resolves to would depend on file order.
	ErrDuplicateToken = errors.New("bdgateway: two token files hold the same token")
	// ErrNoTokens is LoadTokens' refusal of a directory holding no token file.
	// An authenticator with no tokens refuses every request, so the gateway
	// refuses to start instead of running uselessly.
	ErrNoTokens = errors.New("bdgateway: token directory holds no tokens")
)

// TokenTable is one immutable snapshot of the token directory: the identities
// its `<class>.<caller>.token` files name, and the digest of each token.
type TokenTable struct {
	entries []tokenEntry
	// compare is the token comparison Lookup runs. Production uses
	// constantTimeDigestCompare; a test replaces it to observe the operands or
	// to hold one comparison open while the table is swapped.
	compare func(a, b []byte) int
}

type tokenEntry struct {
	identity Identity
	name     string
	digest   [sha256.Size]byte
}

// constantTimeDigestCompare compares two SHA-256 digests. Both operands are
// always sha256.Size bytes, the whole of each is compared, and no byte decides
// the result before the rest are read: crypto/subtle.ConstantTimeCompare's
// early return on different lengths never runs, and a comparison's duration
// does not say how much of a token matched.
func constantTimeDigestCompare(a, b []byte) int {
	return subtle.ConstantTimeCompare(a, b)
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

// LoadTokens reads dir's `<class>.<caller>.token` files into a new TokenTable.
// The caller is part of the file name, so the token itself resolves which
// caller a request speaks as. Non-token files (README, editor droppings) are
// ignored. Every refusal names the file or directory and what was wrong; none
// echoes a token.
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
	table := &TokenTable{compare: constantTimeDigestCompare}
	for _, dirent := range dirents {
		name := dirent.Name()
		if !strings.HasSuffix(name, ".token") {
			continue
		}
		identity, err := tokenFileName(name)
		if err != nil {
			return nil, err
		}
		digest, err := readTokenDigest(fs, filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		for _, prev := range table.entries {
			if table.compare(prev.digest[:], digest[:]) == 1 {
				return nil, fmt.Errorf("%w: %s and %s", ErrDuplicateToken, name, prev.name)
			}
		}
		table.entries = append(table.entries, tokenEntry{identity: identity, name: name, digest: digest})
	}
	if len(table.entries) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoTokens, dir)
	}
	return table, nil
}

// tokenFileName returns the identity a token file name names: the class it may
// run and the caller it speaks as.
func tokenFileName(name string) (Identity, error) {
	stem := strings.TrimSuffix(name, ".token")
	classPart, caller, ok := strings.Cut(stem, ".")
	if !ok || !validCaller(caller) {
		return Identity{}, fmt.Errorf("%w: file %s", ErrTokenFileName, name)
	}
	class, err := parseTokenClass(classPart)
	if err != nil {
		return Identity{}, fmt.Errorf("%w: file %s", err, name)
	}
	return Identity{Class: class, Caller: caller}, nil
}

// parseTokenClass returns the token class called name, or ErrUnknownTokenClass.
// The class part of a token file name goes through it, so a file naming a verb
// class (write, admin) or a typo is refused rather than loaded.
func parseTokenClass(name string) (TokenClass, error) {
	c := TokenClass(name)
	if _, ok := tokenClassRank(c); !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownTokenClass, name)
	}
	return c, nil
}

// maxCallerBytes bounds the caller part of a token file name, so a name stays
// a label a log line and a directory listing both show whole.
const maxCallerBytes = 64

// validCaller reports whether caller is a token file name's caller part:
// 1 to 64 lowercase letters, digits, '-' or '_'.
func validCaller(caller string) bool {
	if caller == "" || len(caller) > maxCallerBytes {
		return false
	}
	for i := 0; i < len(caller); i++ {
		c := caller[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

// readTokenDigest returns the SHA-256 digest of the token path holds. It checks
// the file with Lstat, so a symlink is refused rather than followed, and
// requires mode 0600 exactly: a token readable by anyone else is refused with
// the mode named and the token left unread. Surrounding whitespace is trimmed,
// so a file written with a trailing newline works.
func readTokenDigest(fs tokenFS, path string) ([sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	info, err := fs.lstat(path)
	switch {
	case err != nil:
		return digest, fmt.Errorf("%w: %s: %v", ErrTokenFile, path, err)
	case !info.Mode().IsRegular():
		return digest, fmt.Errorf("%w: %s is not a regular file (a symlink or a directory)", ErrTokenFile, path)
	case info.Mode().Perm() != 0o600:
		return digest, fmt.Errorf("%w: %s has mode %04o", ErrTokenFileMode, path, info.Mode().Perm())
	}
	b, err := fs.readFile(path)
	if err != nil {
		return digest, fmt.Errorf("%w: read %s: %v", ErrTokenFile, path, err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return digest, fmt.Errorf("%w: %s", ErrEmptyToken, path)
	}
	return sha256.Sum256([]byte(token)), nil
}

// Lookup returns the identity token authenticates to, and false for a token no
// entry holds. Every entry is compared, and each comparison runs
// constantTimeDigestCompare over two SHA-256 digests, so a request's duration
// says neither how much of a token matched nor which entry did.
func (t *TokenTable) Lookup(token string) (Identity, bool) {
	digest := sha256.Sum256([]byte(token))
	var match Identity
	found := 0
	for _, entry := range t.entries {
		if t.compare(entry.digest[:], digest[:]) == 1 {
			match = entry.identity
			found = 1
		}
	}
	return match, found == 1
}

// Authenticator holds the current token table behind an atomic pointer, so a
// reload swaps it and a request keeps the table it loaded until it returns.
type Authenticator struct {
	dir   string
	fs    tokenFS
	table atomic.Pointer[TokenTable]
}

// NewAuthenticator loads dir's tokens and returns an Authenticator. A
// directory it cannot load is an error: the gateway refuses to start with no
// usable token table rather than come up refusing every request.
func NewAuthenticator(dir string) (*Authenticator, error) {
	a := &Authenticator{dir: dir, fs: osTokenFS}
	if err := a.Reload(); err != nil {
		return nil, err
	}
	return a, nil
}

// Reload builds a table from the token directory and swaps it in. A failed
// load leaves the current table in place, so a bad rotation refuses the reload
// instead of locking every caller out. The directory is read off the request
// path, so requests keep being served by the current table throughout.
func (a *Authenticator) Reload() error {
	table, err := loadTokens(a.fs, a.dir)
	if err != nil {
		return err
	}
	a.table.Store(table)
	return nil
}

// Authenticate returns the class and caller the bearer token authenticates to;
// the request names neither. A missing, malformed or empty bearer token is
// ErrNoToken; a well-formed token no file holds is ErrUnknownToken. Both are
// 401 (see HTTPStatus).
func (a *Authenticator) Authenticate(authorization string) (Identity, error) {
	token, err := ParseBearer(authorization)
	if err != nil {
		return Identity{}, err
	}
	table := a.table.Load()
	if table == nil {
		return Identity{}, fmt.Errorf("%w: no token table loaded", ErrUnknownToken)
	}
	identity, ok := table.Lookup(token)
	if !ok {
		return Identity{}, ErrUnknownToken
	}
	return identity, nil
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

// HTTPStatus maps an authentication, class or verb-policy refusal to the status
// the gateway answers before any frame: 200 for no error, 401 for no or unknown
// token, 403 for a verb, flag or class the policy refuses, and 500 for a failure
// this package does not know. An unrecognized failure must not read as success.
func HTTPStatus(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, ErrNoToken), errors.Is(err, ErrUnknownToken):
		return http.StatusUnauthorized
	case errors.Is(err, ErrClassDenied), errors.Is(err, ErrVerbNotAllowed), errors.Is(err, ErrFlagRefused):
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
