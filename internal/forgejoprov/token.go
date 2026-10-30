package forgejoprov

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// tokenFileMode is the mode of a role's token file: the owner alone reads it.
const tokenFileMode fs.FileMode = 0o600

// tokenValue is what a minted token may hold. A value outside it is not a token
// the run can write into an env file and read back.
var tokenValue = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Files is the filesystem the token files go through, so a test can record the
// modes it was asked for.
type Files interface {
	// WriteFileAtomic writes data to path with mode: a temp file in the same
	// directory, then a rename over path, so a reader sees the old file or the
	// complete new one and a failure leaves no partial file. It creates the
	// directory when it is missing.
	WriteFileAtomic(path string, data []byte, mode fs.FileMode) error
	// Chmod sets the mode of path.
	Chmod(path string, mode fs.FileMode) error
	// Stat describes path; an absent path is an error that satisfies
	// errors.Is(err, fs.ErrNotExist).
	Stat(path string) (fs.FileInfo, error)
}

// tempFile is the file WriteFileAtomic writes through: *os.File in production, a
// fake whose steps fail in a test.
type tempFile interface {
	Name() string
	Chmod(mode fs.FileMode) error
	Write(p []byte) (int, error)
	Close() error
}

// osFiles is the real filesystem. Its zero value runs on the real disk; a test
// sets the function fields to make one step fail. The hooks are the Files seam
// pattern this package and internal/docslint (OSFiles.walk and .stat) already
// use for steps a unit test cannot make fail on a real disk.
type osFiles struct {
	mkdirAll   func(path string, perm fs.FileMode) error
	createTemp func(dir, pattern string) (tempFile, error)
	rename     func(oldpath, newpath string) error
	remove     func(path string) error
}

func (o osFiles) doMkdirAll(path string) error {
	if o.mkdirAll != nil {
		return o.mkdirAll(path, 0o700)
	}
	return os.MkdirAll(path, 0o700)
}

func (o osFiles) doCreateTemp(dir, pattern string) (tempFile, error) {
	if o.createTemp != nil {
		return o.createTemp(dir, pattern)
	}
	return os.CreateTemp(dir, pattern)
}

func (o osFiles) doRename(oldpath, newpath string) error {
	if o.rename != nil {
		return o.rename(oldpath, newpath)
	}
	return os.Rename(oldpath, newpath)
}

func (o osFiles) doRemove(path string) error {
	if o.remove != nil {
		return o.remove(path)
	}
	return os.Remove(path)
}

// WriteFileAtomic writes data through a temp file beside path and renames it
// over path. The directory is created 0700 when it is missing, since it holds
// credentials. A step that fails removes the temp file, so no partial file is
// left.
func (o osFiles) WriteFileAtomic(path string, data []byte, mode fs.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := o.doMkdirAll(dir); err != nil {
		return err
	}
	tmp, err := o.doCreateTemp(dir, ".forgejo-token.*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = o.doRemove(name)
		}
	}()
	// The mode is set before the value is written, so the temp file is never
	// readable by anyone else.
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return o.doRename(name, path)
}

// Chmod sets the mode of path.
func (osFiles) Chmod(path string, mode fs.FileMode) error { return os.Chmod(path, mode) }

// Stat describes path.
func (osFiles) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }

func (p *Provisioner) files() Files {
	if p.Files != nil {
		return p.Files
	}
	return osFiles{}
}

// tokenFilePath is where the role's token file lives.
func (p *Provisioner) tokenFilePath(role string) string {
	return filepath.Join(p.ConfigDir, "forgejo-"+role+".env")
}

// EnsureToken writes the role bot's token file, minting a token only when the
// file is absent or Rotate is set. A token's value comes back once, at creation,
// so a lost file means a rotation, never a recovery. An existing token keeps the
// scopes it was minted with until Rotate replaces it.
func (p *Provisioner) EnsureToken(role string) error {
	scopes, ok := RoleScopes(role)
	if !ok {
		return fmt.Errorf("unknown role %q (known: %s)", role, KnownRoles())
	}
	user := p.BotPrefix + role
	path := p.tokenFilePath(role)

	exists, err := p.tokenFileExists(path)
	if err != nil {
		return err
	}
	if exists {
		// A dry run writes nothing, a mode change included.
		if !p.Client.DryRun {
			if err := p.files().Chmod(path, tokenFileMode); err != nil {
				return fmt.Errorf("could not set mode 600 on %s: %w", path, err)
			}
		}
		if !p.Rotate {
			p.logf("token %s exists; leaving it (--rotate mints a new one)", path)
			return nil
		}
		p.logf("rotating the token in %s", path)
	}
	if p.Client.DryRun {
		p.logf("would mint a '%s' token for %s and write %s", p.TokenName, user, path)
		return nil
	}
	// A rotation revokes the old token BEFORE it mints: Forgejo refuses a second
	// token of the same name, so the order cannot be reversed. A mint that then
	// fails leaves the file holding a token that no longer works, and the error
	// says so.
	outcome := revokeNone
	if p.Rotate {
		outcome = p.revokeOldToken(user)
	}

	token, duplicate, err := p.mintToken(user, scopes)
	if err != nil {
		switch {
		case outcome == revokeDone:
			return fmt.Errorf("%w; the previous '%s' token for %s was already revoked, so the token in %s no longer works: run again with --rotate to mint a new one", err, p.TokenName, user, path)
		case outcome == revokeFailed:
			// The revoke warned and did not take (or could not be tried), so the
			// old token may still be live whatever the mint says.
			return fmt.Errorf("%w; the previous '%s' token for %s could not be revoked and may still be live (%s still holds it): revoke it by hand in the web UI, then run again with --rotate", err, p.TokenName, user, path)
		case duplicate:
			// A token with this name exists but there is no file: the value came
			// back once, at creation, and is lost.
			return fmt.Errorf("%w; a '%s' token for %s already exists on the server but %s is missing, so its value is lost: run again with --rotate to revoke it and mint a new one", err, p.TokenName, user, path)
		}
		return err
	}
	data := []byte("FORGEJO_URL=" + p.WebURL + "\nFORGEJO_USER=" + user + "\nFORGEJO_TOKEN=" + token + "\n")
	if err := p.files().WriteFileAtomic(path, data, tokenFileMode); err != nil {
		// The error names the path and never the value. The token now exists on
		// the server and was stored nowhere, so the operator is told how to
		// replace it.
		return fmt.Errorf("minted a '%s' token for %s but could not write %s (%w); run again with --rotate to replace it", p.TokenName, user, path, err)
	}
	if err := p.files().Chmod(path, tokenFileMode); err != nil {
		// The file was written, with mode 600 from its temp file, so the token is
		// stored: say so, and what is left to do.
		return fmt.Errorf("the '%s' token for %s IS stored in %s (it is not lost), but setting its mode to 600 afterwards failed (%w); check the file's mode by hand", p.TokenName, user, path, err)
	}
	p.logf("minted a '%s' token for %s as %s (mode 600)", p.TokenName, user, path)
	return nil
}

// tokenFileExists reports whether path is an existing file. A directory there
// is an error, caught before a token is minted that could not be stored; so is
// any stat failure but "not there", since a file the run cannot see might hold a
// token it would then replace.
func (p *Provisioner) tokenFileExists(path string) (bool, error) {
	info, err := p.files().Stat(path)
	switch {
	case err == nil && info.IsDir():
		return false, fmt.Errorf("%s is a directory, not a token file", path)
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("cannot stat %s: %w", path, err)
	}
}

// mintToken asks for a new token for user and returns its value. Forgejo
// serializes the value as "sha1" (the struct's long-standing field name);
// "token" is read first, for a version that renames it.
func (p *Provisioner) mintToken(user string, scopes []string) (token string, duplicate bool, err error) {
	path := "/admin/users/" + url.PathEscape(user) + "/tokens"
	body := map[string]any{"name": p.TokenName, "scopes": scopes}
	status, resp, err := p.Client.Do(http.MethodPost, path, body)
	if err != nil {
		return "", false, err
	}
	switch status {
	case http.StatusCreated:
	case http.StatusForbidden:
		return "", false, fmt.Errorf("the admin token cannot mint a token for %s; it needs site-administrator rights, and a token limit of 'all'.%s", user, p.Client.BodyTail(resp))
	default:
		// Forgejo refuses a second token of the same name with a 422 whose
		// message says the name "has been used already".
		dup := status == http.StatusUnprocessableEntity && strings.Contains(strings.ToLower(string(resp)), "used already")
		return "", dup, p.Client.Unexpected(http.MethodPost, path, status, resp)
	}
	var reply map[string]any
	_ = json.Unmarshal(resp, &reply)
	token, _ = reply["token"].(string)
	if token == "" {
		token, _ = reply["sha1"].(string)
	}
	// The reply is not quoted: it holds the value that did not pass.
	if !tokenValue.MatchString(token) {
		return "", false, fmt.Errorf("POST %s returned no usable token", path)
	}
	return token, false, nil
}

// tokenPageSize is how many tokens one page of the list asks for, and
// maxTokenPages the bound on how many pages the search for the old token reads.
// A user with more tokens than their product is not a user a rotation can read
// to the end, so it stops with a warning naming the bound.
const (
	tokenPageSize = 50
	maxTokenPages = 10
)

// revokeOutcome says what a rotation's revoke achieved, so a mint that fails
// afterwards can say what state the old token is in.
type revokeOutcome int

const (
	// revokeNone means no token of that name was found, so there was nothing to
	// revoke and the error claims nothing about one.
	revokeNone revokeOutcome = iota
	// revokeDone means the old token was deleted.
	revokeDone
	// revokeFailed means a warning was logged because the old token could not be
	// found or deleted, so it may still be live.
	revokeFailed
)

// revokeOldToken removes the token of this run's name so a rotation does not
// leave the previous credential live. Every failure to list or delete is a
// warning, not an abort: the file may hold a token the operator made by hand,
// replacing the file is what matters, and the mint is still attempted.
func (p *Provisioner) revokeOldToken(user string) revokeOutcome {
	base := "/admin/users/" + url.PathEscape(user) + "/tokens"
	for page := 1; page <= maxTokenPages; page++ {
		status, resp, err := p.Client.Do(http.MethodGet, fmt.Sprintf("%s?page=%d&limit=%d", base, page, tokenPageSize), nil)
		if err != nil {
			p.logf("warning: cannot list %s's tokens (%v); revoke the old '%s' token by hand", user, err, p.TokenName)
			return revokeFailed
		}
		if status != http.StatusOK {
			p.logf("warning: cannot list %s's tokens (HTTP %d)%s; revoke the old '%s' token by hand", user, status, p.Client.BodyTail(resp), p.TokenName)
			return revokeFailed
		}
		var items []json.RawMessage
		if err := json.Unmarshal(resp, &items); err != nil {
			p.logf("warning: cannot read %s's token list (not a JSON array); revoke the old '%s' token by hand", user, p.TokenName)
			return revokeFailed
		}
		if _, found := FindObject(resp, "name", p.TokenName); found {
			return p.deleteToken(base)
		}
		if len(items) < tokenPageSize {
			p.logf("no token named '%s' to revoke; if the file held a token minted under another name, revoke it in the web UI", p.TokenName)
			return revokeNone
		}
	}
	p.logf("warning: stopped looking for the '%s' token of %s after %d pages of %d (the bound); revoke it by hand in the web UI", p.TokenName, user, maxTokenPages, tokenPageSize)
	return revokeFailed
}

// deleteToken deletes the token of this run's name from the user's list at base.
func (p *Provisioner) deleteToken(base string) revokeOutcome {
	status, resp, err := p.Client.Do(http.MethodDelete, base+"/"+url.PathEscape(p.TokenName), nil)
	if err != nil {
		p.logf("warning: could not revoke the previous '%s' token (%v); revoke it in the web UI", p.TokenName, err)
		return revokeFailed
	}
	if status == http.StatusNoContent {
		p.logf("revoked the previous '%s' token", p.TokenName)
		return revokeDone
	}
	p.logf("warning: could not revoke the previous '%s' token (HTTP %d)%s; revoke it in the web UI", p.TokenName, status, p.Client.BodyTail(resp))
	return revokeFailed
}
