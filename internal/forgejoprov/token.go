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

// osFiles is the real filesystem.
type osFiles struct{}

// WriteFileAtomic writes data through a temp file beside path and renames it
// over path. The directory is created 0700 when it is missing, since it holds
// credentials.
func (osFiles) WriteFileAtomic(path string, data []byte, mode fs.FileMode) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".forgejo-token.*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(name)
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
	return os.Rename(name, path)
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
	if p.Rotate {
		if err := p.revokeOldToken(user); err != nil {
			return err
		}
	}

	token, err := p.mintToken(user, scopes)
	if err != nil {
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
		return fmt.Errorf("could not set mode 600 on %s: %w", path, err)
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
func (p *Provisioner) mintToken(user string, scopes []string) (string, error) {
	path := "/admin/users/" + url.PathEscape(user) + "/tokens"
	body := map[string]any{"name": p.TokenName, "scopes": scopes}
	status, resp, err := p.Client.Do(http.MethodPost, path, body)
	if err != nil {
		return "", err
	}
	switch status {
	case http.StatusCreated:
	case http.StatusForbidden:
		return "", fmt.Errorf("the admin token cannot mint a token for %s; it needs site-administrator rights, and a token limit of 'all'.%s", user, p.Client.BodyTail(resp))
	default:
		return "", p.Client.Unexpected(http.MethodPost, path, status, resp)
	}
	var reply map[string]any
	_ = json.Unmarshal(resp, &reply)
	token, _ := reply["token"].(string)
	if token == "" {
		token, _ = reply["sha1"].(string)
	}
	// The reply is not quoted: it holds the value that did not pass.
	if !tokenValue.MatchString(token) {
		return "", fmt.Errorf("POST %s returned no usable token", path)
	}
	return token, nil
}

// revokeOldToken removes the token of this run's name so a rotation does not
// leave the previous credential live. A miss is a warning: the file may hold a
// token the operator made by hand, and replacing the file is what matters.
func (p *Provisioner) revokeOldToken(user string) error {
	base := "/admin/users/" + url.PathEscape(user) + "/tokens"
	status, resp, err := p.Client.Do(http.MethodGet, base, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		p.logf("warning: cannot list %s's tokens (HTTP %d)%s; revoke the old '%s' token by hand", user, status, p.Client.BodyTail(resp), p.TokenName)
		return nil
	}
	if _, found := FindObject(resp, "name", p.TokenName); !found {
		p.logf("no token named '%s' to revoke; if the file held a token minted under another name, revoke it in the web UI", p.TokenName)
		return nil
	}
	status, resp, err = p.Client.Do(http.MethodDelete, base+"/"+url.PathEscape(p.TokenName), nil)
	if err != nil {
		return err
	}
	if status == http.StatusNoContent {
		p.logf("revoked the previous '%s' token", p.TokenName)
		return nil
	}
	p.logf("warning: could not revoke the previous '%s' token (HTTP %d)%s; revoke it in the web UI", p.TokenName, status, p.Client.BodyTail(resp))
	return nil
}
