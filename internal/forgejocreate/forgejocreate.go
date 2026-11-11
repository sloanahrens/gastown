// Package forgejocreate creates a private, empty Forgejo repository and seeds it with
// one commit through the contents API. It is the library behind
// cmd/forgejo-create-repo: the seed-directory rules and the request shapes live here,
// the flags and exit codes live in the command. Every request goes through
// forgejoprov.Client, so the token is sent in the Authorization header only.
package forgejocreate

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/steveyegge/gastown/internal/forgejoprov"
)

// MaxFileBytes is the largest seed file the command sends. A larger file is refused by name.
const MaxFileBytes = 1 << 20

// Branch is the default branch the repository is created with and the seed commit lands on.
const Branch = "main"

// plainName matches an owner or repository name the API takes as one path part.
var plainName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// File is one seed file: its slash-separated path inside the repository and its bytes.
type File struct {
	Path string
	Data []byte
}

// ValidateRepo splits OWNER/NAME. It refuses any other shape, and any part that is not a
// plain name, so the name can be placed in a request path as it is.
func ValidateRepo(repo string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !plainName.MatchString(owner) || !plainName.MatchString(name) {
		return "", "", fmt.Errorf("--repo must be OWNER/NAME with plain names; got %q", repo)
	}
	return owner, name, nil
}

// LoadSeed reads every file under dir, in lexical order, for the seed commit. It refuses a
// relative dir, a dir that cannot be opened as a directory (a file, or a missing path), a dir
// that holds no files, an entry that is not a regular file (a symlink inside dir is refused,
// not followed), and a file over MaxFileBytes. A symlink that is dir itself is followed by
// the open. Every read goes through an os.Root, so no read leaves dir. Each refusal names the
// path it refused, and nothing is written before LoadSeed returns.
func LoadSeed(dir string) ([]File, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("seed directory %q is relative; give an absolute path", dir)
	}
	seedRoot, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("seed directory %s: %w", dir, err)
	}
	defer func() { _ = seedRoot.Close() }()
	return collectSeed(dir, seedRoot.FS())
}

// collectSeed walks fsys and returns its files in lexical order, applying LoadSeed's refusals.
// dir names the directory in the messages. It takes an fs.FS, not a path, so a test can hand it
// a filesystem that fails an open or a read.
func collectSeed(dir string, fsys fs.FS) ([]File, error) {
	var files []File
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("seed path %s: %w", name, err)
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("seed file %s is not a regular file; symlinks and other entries are refused", name)
		}
		data, err := readSeedFile(fsys, name)
		if err != nil {
			return fmt.Errorf("seed file %s: %w", name, err)
		}
		files = append(files, File{Path: name, Data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("seed directory %s holds no files", dir)
	}
	return files, nil
}

// readSeedFile reads the seed file name from fsys and refuses one over MaxFileBytes. The read
// stops one byte past the limit, so a file that grows after it was listed is refused too.
func readSeedFile(fsys fs.FS, name string) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("is over the limit of %d bytes (1 MiB)", MaxFileBytes)
	}
	return data, nil
}

// CommitMessage is the message of the seed commit for owner/name.
func CommitMessage(owner, name string) string {
	return fmt.Sprintf("Seed %s/%s", owner, name)
}

// repoPath is the API path of the repository owner/name.
func repoPath(owner, name string) string {
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}

// CreateRepo creates the private, empty repository owner/name with default branch Branch and
// no initialisation, as the administrator the client is signed in as. It reads the repository
// first and refuses one that already exists, so a refused run sends no create.
func CreateRepo(c *forgejoprov.Client, owner, name string) error {
	path := repoPath(owner, name)
	status, body, err := c.Do(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK:
		return fmt.Errorf("repository %s/%s already exists; nothing is written", owner, name)
	case http.StatusNotFound:
	default:
		return c.Unexpected(http.MethodGet, path, status, body)
	}
	createPath := "/admin/users/" + url.PathEscape(owner) + "/repos"
	status, body, err = c.Do(http.MethodPost, createPath, map[string]any{
		"name":           name,
		"private":        true,
		"auto_init":      false,
		"default_branch": Branch,
	})
	if err != nil {
		return err
	}
	if status != http.StatusCreated {
		return c.Unexpected(http.MethodPost, createPath, status, body)
	}
	return nil
}

// commitReply is the part of the contents reply the command reports.
type commitReply struct {
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

// Seed commits every file to Branch of owner/name as one commit through the contents API. The
// author and the committer are both named author. It returns the SHA of the commit. File bytes
// travel base64-encoded, so text and binary files are sent the same way.
func Seed(c *forgejoprov.Client, owner, name, author string, files []File) (string, error) {
	ops := make([]map[string]any, 0, len(files))
	for _, f := range files {
		ops = append(ops, map[string]any{
			"operation": "create",
			"path":      f.Path,
			"content":   base64.StdEncoding.EncodeToString(f.Data),
		})
	}
	who := map[string]string{"name": author}
	path := repoPath(owner, name) + "/contents"
	status, body, err := c.Do(http.MethodPost, path, map[string]any{
		"branch":    Branch,
		"message":   CommitMessage(owner, name),
		"author":    who,
		"committer": who,
		"files":     ops,
	})
	if err != nil {
		return "", err
	}
	if status != http.StatusCreated {
		return "", c.Unexpected(http.MethodPost, path, status, body)
	}
	var reply commitReply
	if err := json.Unmarshal(body, &reply); err != nil || reply.Commit.SHA == "" {
		return "", fmt.Errorf("POST %s: HTTP %d but the reply names no commit; the seed commit may have landed on %s/%s", path, status, owner, name)
	}
	return reply.Commit.SHA, nil
}
