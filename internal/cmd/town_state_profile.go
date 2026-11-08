package cmd

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/townstate"
)

// podEnv is the variable Kubernetes sets in every pod. The profile rewrites a
// town's settings, so it runs only inside a pod: on the Mac it would rewrite
// the live town (gt-gvr05p).
const podEnv = "KUBERNETES_SERVICE_HOST"

// rehearsalBackupName is the copy of the settings a write leaves beside them.
const rehearsalBackupName = "config.json.pre-rehearsal"

// rehearsalTempSuffix is appended to the settings path for the file a write
// builds before renaming it over the settings.
const rehearsalTempSuffix = ".rehearsal-tmp"

var townStateProfileCmd = &cobra.Command{
	Use:   "profile",
	Short: "Turn the daemon's patrols off in a seeded settings file (rehearsal pods only)",
	Long: `Rewrite the town's settings/config.json for a rehearsal pod, so a seeded copy
of the live settings starts none of the work the live daemon starts. Every
patrol object directly under daemon.patrols is set to enabled false (a missing
enabled becomes an explicit false), and daemon.supervisor is removed. Every
other key and value is left as it was.

The command refuses to run unless KUBERNETES_SERVICE_HOST is set, which
Kubernetes sets in every pod. A write copies the old file to
config.json.pre-rehearsal first, and refuses when that copy already exists, so
the original is never overwritten. A profile that changes nothing writes nothing.

With --check the command writes nothing. It prints each patrol still enabled and
the supervisor if set, and exits 1 when there is one.

Examples:
  gt town state profile --town /volume/town --rehearsal --check
  gt town state profile --town /volume/town --rehearsal`,
	Args: cobra.NoArgs,
	RunE: runTownStateProfile,
}

func init() {
	townStateCmd.AddCommand(townStateProfileCmd)
	townStateProfileCmd.Flags().String("town", "", "the town root whose settings to profile (required)")
	townStateProfileCmd.Flags().Bool("rehearsal", false, "profile for a rehearsal: every daemon patrol off, the container supervisor removed")
	townStateProfileCmd.Flags().Bool("check", false, "print what is still on and exit 1 when anything is; write nothing")
	_ = townStateProfileCmd.MarkFlagRequired("town")
}

// profileFiles is the filesystem the profile reads and writes through. The
// host value is what the command runs with; a test replaces one operation to
// drive the failure it names, as townstate's townOps does.
type profileFiles struct {
	stat   func(string) (fs.FileInfo, error)
	open   func(name string, flag int, perm fs.FileMode) (io.WriteCloser, error)
	rename func(oldpath, newpath string) error
	remove func(string) error
}

// hostProfileFiles is the profile's filesystem on this host.
func hostProfileFiles() profileFiles {
	return profileFiles{
		stat: os.Stat,
		open: func(name string, flag int, perm fs.FileMode) (io.WriteCloser, error) {
			return os.OpenFile(name, flag, perm) //nolint:gosec // G304: the name is the town's settings file or a fixed name beside it
		},
		rename: os.Rename,
		remove: os.Remove,
	}
}

// townStateProfileEnv is what the profile command reads from its session.
type townStateProfileEnv struct {
	town      string
	rehearsal bool
	check     bool
	getenv    func(string) string
	files     profileFiles
	out       io.Writer
}

func runTownStateProfile(cmd *cobra.Command, _ []string) error {
	town, err := cmd.Flags().GetString("town")
	if err != nil {
		return fmt.Errorf("reading --town: %w", err)
	}
	rehearsal, err := cmd.Flags().GetBool("rehearsal")
	if err != nil {
		return fmt.Errorf("reading --rehearsal: %w", err)
	}
	check, err := cmd.Flags().GetBool("check")
	if err != nil {
		return fmt.Errorf("reading --check: %w", err)
	}
	return runTownStateProfileIn(townStateProfileEnv{
		town:      town,
		rehearsal: rehearsal,
		check:     check,
		getenv:    os.Getenv,
		files:     hostProfileFiles(),
		out:       cmd.OutOrStdout(),
	})
}

// runTownStateProfileIn is what `gt town state profile` runs.
func runTownStateProfileIn(env townStateProfileEnv) error {
	if env.getenv(podEnv) == "" {
		return fmt.Errorf("gt town state profile runs only inside a pod: %s is not set", podEnv)
	}
	if !env.rehearsal {
		return errors.New("gt town state profile needs --rehearsal: it is the only profile")
	}
	path := config.TownSettingsPath(env.town)
	info, err := env.files.stat(path)
	if err != nil {
		return fmt.Errorf("reading the town settings: %w", err)
	}
	settings, err := os.ReadFile(path) //nolint:gosec // G304: the path is the town's own settings file
	if err != nil {
		return fmt.Errorf("reading the town settings: %w", err)
	}
	out, changes, err := townstate.RehearsalProfile(settings)
	if err != nil {
		return fmt.Errorf("profiling %s: %w", path, err)
	}
	if env.check {
		return reportRehearsalChanges(env.out, changes)
	}
	if len(changes) == 0 {
		fmt.Fprintf(env.out, "%s is already rehearsal-profiled; nothing written\n", path)
		return nil
	}
	if err := writeRehearsalSettings(env.files, path, info.Mode().Perm(), settings, out); err != nil {
		return err
	}
	fmt.Fprintf(env.out, "rehearsal profile written to %s (%d changes); the old file is %s\n", path, len(changes), rehearsalBackupName)
	return nil
}

// reportRehearsalChanges prints each key the profile would change, one per
// line, and fails with exit 1 when there is one: each key is a patrol still on
// or a supervisor still set.
func reportRehearsalChanges(w io.Writer, changes []string) error {
	if len(changes) == 0 {
		fmt.Fprintln(w, "rehearsal profile: ok")
		return nil
	}
	for _, c := range changes {
		fmt.Fprintln(w, c)
	}
	return &ExitCodeError{Code: 1, Err: fmt.Errorf("rehearsal profile: %d settings still on; run without --check to write the profile", len(changes))}
}

// writeRehearsalSettings keeps old beside path as the rehearsal backup, then
// builds the new settings in a temporary file and renames it over path, so a
// reader sees the old file or the new one. A backup that already exists is
// refused, so an earlier original is never overwritten; a backup that fails
// part-way is left for an operator to look at rather than removed.
func writeRehearsalSettings(files profileFiles, path string, mode fs.FileMode, old, out []byte) error {
	backup := filepath.Join(filepath.Dir(path), rehearsalBackupName)
	if err := writeRehearsalFile(files, backup, os.O_EXCL, mode, old); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists: an earlier rehearsal profile kept the original there; move it aside to write a new profile", backup)
		}
		return fmt.Errorf("writing the rehearsal backup: %w", err)
	}
	tmp := path + rehearsalTempSuffix
	if err := writeRehearsalFile(files, tmp, os.O_TRUNC, mode, out); err != nil {
		_ = files.remove(tmp)
		return fmt.Errorf("writing the rehearsal settings: %w", err)
	}
	if err := files.rename(tmp, path); err != nil {
		_ = files.remove(tmp)
		return fmt.Errorf("replacing the town settings: %w", err)
	}
	return nil
}

// writeRehearsalFile creates name with the flag and perm, writes data to it and
// closes it. The flag adds to create-and-write-only: os.O_EXCL for a file that
// must be new, os.O_TRUNC for one that may already exist.
func writeRehearsalFile(files profileFiles, name string, flag int, perm fs.FileMode, data []byte) error {
	f, err := files.open(name, flag|os.O_WRONLY|os.O_CREATE, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return nil
}
