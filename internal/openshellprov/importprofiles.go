package openshellprov

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// forgejoPolecat is the profile that binds the Forgejo endpoint of the gateway it
// is imported to, so it goes in as gt renders it for that gateway, not as its
// file names it.
const forgejoPolecat = "forgejo-polecat"

// Files is the filesystem a run reads and writes. Production uses OSFiles; a
// test passes an in-memory fake, so no file is touched for real.
type Files interface {
	// ReadDir lists the entries of dir in name order.
	ReadDir(dir string) ([]fs.DirEntry, error)
	// ReadFile returns the contents of name.
	ReadFile(name string) ([]byte, error)
	// Create opens name for writing, truncating it if it exists.
	Create(name string) (io.WriteCloser, error)
	// MkdirTemp makes an empty directory for one run and returns its path.
	MkdirTemp() (string, error)
	// RemoveAll removes path and everything under it.
	RemoveAll(path string) error
}

// OSFiles is the production Files: each method is the os call of the same name.
type OSFiles struct{}

// ReadDir lists the entries of dir, as os.ReadDir does.
func (OSFiles) ReadDir(dir string) ([]fs.DirEntry, error) { return os.ReadDir(dir) }

// ReadFile returns the contents of name.
func (OSFiles) ReadFile(name string) ([]byte, error) { return os.ReadFile(name) }

// Create opens name for writing, truncating it. On error the file is nil and the
// caller must not close it.
func (OSFiles) Create(name string) (io.WriteCloser, error) { return os.Create(name) }

// MkdirTemp makes an empty directory under the system temp dir.
func (OSFiles) MkdirTemp() (string, error) { return os.MkdirTemp("", Name+"-") }

// RemoveAll removes path and everything under it.
func (OSFiles) RemoveAll(path string) error { return os.RemoveAll(path) }

// Run is one run of the profile import. Every openshell call goes through
// Client and every gt call through Gt; every file goes through Files.
type Run struct {
	// Client makes the openshell calls.
	Client Client
	// Gt makes the gt sandbox endpoint calls that render and check forgejo-polecat.
	Gt Gt
	// Paths says where the profiles are.
	Paths Paths
	// Files is the filesystem the run reads and writes.
	Files Files
	// Env is the environment a provider create runs with; the credential is added
	// to a copy of it, so the secret reaches only that child.
	Env []string
	// Out receives the run's progress lines and what openshell prints to stdout.
	Out io.Writer
	// Err receives what openshell prints to stderr.
	Err io.Writer
}

// ImportProfiles imports each profile under Paths.Profiles, in file-name order. A
// profile the gateway already lists is left alone, except forgejo-polecat, which
// is updated when it does not bind the gateway's endpoint. The rendered and
// exported profiles live in a temp dir that is removed on every exit path.
func (r *Run) ImportProfiles() (err error) {
	files, err := r.profiles(r.Paths.Profiles)
	if err != nil {
		return fmt.Errorf("%s: listing profiles in %s: %w", Name, r.rel(r.Paths.Profiles), err)
	}
	if len(files) == 0 {
		fmt.Fprintf(r.Out, "%s: no profiles under %s, nothing to import\n", Name, r.rel(r.Paths.Profiles))
		return nil
	}
	work, err := r.Files.MkdirTemp()
	if err != nil {
		return fmt.Errorf("%s: making the run's temp dir: %w", Name, err)
	}
	defer func() {
		if rmErr := r.Files.RemoveAll(work); rmErr != nil && err == nil {
			err = fmt.Errorf("%s: removing the run's temp dir: %w", Name, rmErr)
		}
	}()
	for _, file := range files {
		if err := r.importProfile(file, work); err != nil {
			return err
		}
	}
	return nil
}

// profiles lists the profile yaml files under dir, in name order. A dir that does
// not exist holds none, as the script's glob found none.
func (r *Run) profiles(dir string) ([]string, error) {
	entries, err := r.Files.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".yaml") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	return files, nil
}

// importProfile does one file's step of the loop. A profile the gateway lists is
// logged and skipped; forgejo-polecat is checked and updated instead; anything
// else is linted and imported.
func (r *Run) importProfile(file, work string) error {
	data, err := r.Files.ReadFile(file)
	if err != nil {
		return fmt.Errorf("%s: reading %s: %w", Name, r.rel(file), err)
	}
	// A profile with no id cannot be one the gateway lists, so it goes to lint and
	// import as it always did, and lint names what is wrong with it.
	id := ProfileID(data)
	exists := false
	if id != "" {
		if exists, err = r.Client.ProfileExists(id); err != nil {
			return err
		}
	}

	if id == forgejoPolecat {
		if exists {
			updated, err := r.updateIfStale(id, file, work)
			if err != nil || updated {
				return err
			}
		} else {
			rendered := filepath.Join(work, id+".yaml")
			if err := r.render(file, rendered, ""); err != nil {
				return err
			}
			file = rendered
		}
	}

	if exists {
		fmt.Fprintf(r.Out, "%s: profile %s exists\n", Name, id)
		return nil
	}
	if err := r.openshell("profile", "lint", "-f", file); err != nil {
		return err
	}
	return r.openshell("profile", "import", "-f", file, "--global")
}

// updateIfStale checks the gateway's copy of forgejo-polecat against the endpoint
// gt renders for it. When the copy does not bind the endpoint it is updated from
// the rendered file and updateIfStale reports true; a copy that binds it is kept.
func (r *Run) updateIfStale(id, file, work string) (bool, error) {
	exported := filepath.Join(work, id+".export.yaml")
	if err := r.export(id, exported); err != nil {
		return false, err
	}
	reason, err := r.Gt.StaleReason(id, exported)
	if err != nil {
		return false, err
	}
	if reason == "" {
		return false, nil
	}
	fmt.Fprintf(r.Out, "%s: profile %s is stale (%s), updating it\n", Name, id, reason)
	rendered := filepath.Join(work, id+".yaml")
	if err := r.render(file, rendered, exported); err != nil {
		return false, err
	}
	if err := r.openshell("profile", "lint", "-f", rendered); err != nil {
		return false, err
	}
	if err := r.openshell("profile", "update", "--global", "-f", rendered, id); err != nil {
		return false, err
	}
	return true, nil
}

// export writes the gateway's profile id to name.
func (r *Run) export(id, name string) error {
	return r.writeTo(name, func(w io.Writer) error { return r.Client.ExportProfile(id, w) })
}

// render writes the forgejo-polecat profile file, bound to the gateway's endpoint,
// to name. A non-empty exported names the export whose resource_version it takes.
func (r *Run) render(file, name, exported string) error {
	return r.writeTo(name, func(w io.Writer) error { return r.Gt.Render(file, w, exported) })
}

// writeTo creates name, has write fill it and closes it. A failure of either
// names the file it was writing.
func (r *Run) writeTo(name string, write func(io.Writer) error) error {
	w, err := r.Files.Create(name)
	if err != nil {
		return fmt.Errorf("%s: creating %s: %w", Name, filepath.Base(name), err)
	}
	writeErr := write(w)
	closeErr := w.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return fmt.Errorf("%s: closing %s: %w", Name, filepath.Base(name), closeErr)
	}
	return nil
}

// openshell runs one openshell call with args and passes its output on. A failure
// names the call and not its output, which can hold anything the call printed.
func (r *Run) openshell(args ...string) error {
	stdout, stderr, err := r.Client.Run.Run(openshellBinary, r.Client.args(args...), nil, nil)
	if _, werr := r.Out.Write(stdout); werr != nil {
		return fmt.Errorf("%s: writing openshell's output: %w", Name, werr)
	}
	if _, werr := r.Err.Write(stderr); werr != nil {
		return fmt.Errorf("%s: writing openshell's errors: %w", Name, werr)
	}
	if err != nil {
		return fmt.Errorf("%s: openshell %s failed: %w", Name, strings.Join(args, " "), err)
	}
	return nil
}

// rel names path relative to the repository root, as the script's messages do.
func (r *Run) rel(path string) string {
	return strings.TrimPrefix(path, r.Paths.Root+"/")
}
