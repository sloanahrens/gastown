package cmd

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"reflect"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/townstate"
)

// podEnv is the variable Kubernetes sets in every pod. The profile rewrites a
// town's settings, so it runs only inside a pod: on the Mac it would rewrite
// the live town (gt-gvr05p).
const podEnv = "KUBERNETES_SERVICE_HOST"

// rehearsalBackupSuffix is appended to the daemon patrol file's path for the
// copy a write leaves beside it: config.json.pre-rehearsal on the two-file
// layout, daemon.json.pre-rehearsal on the five-file one.
const rehearsalBackupSuffix = ".pre-rehearsal"

// rehearsalTempSuffix is appended to the settings path for the file a write
// builds before renaming it over the settings.
const rehearsalTempSuffix = ".rehearsal-tmp"

var townStateProfileCmd = &cobra.Command{
	Use:   "profile",
	Short: "Turn the daemon's patrols off in a seeded settings file (rehearsal pods only)",
	Long: `Rewrite the file the daemon reads its patrol config from, for a rehearsal pod,
so a seeded copy of the live settings starts none of the work the live daemon
starts. That file is settings/config.json's daemon section on the two-file
layout, and mayor/daemon.json on the five-file one. Every patrol object directly
under daemon.patrols is set to enabled false (a missing enabled becomes an
explicit false), and daemon.supervisor is removed. Every other key and value is
left as it was.

The command refuses to run unless KUBERNETES_SERVICE_HOST is set, which
Kubernetes sets in every pod. A write copies the old file beside itself as
<file>.pre-rehearsal first, and refuses when that copy already exists, so the
original is never overwritten. A profile that changes nothing writes nothing.

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
	read   func(string) ([]byte, error)
	open   func(name string, flag int, perm fs.FileMode) (io.WriteCloser, error)
	rename func(oldpath, newpath string) error
	remove func(string) error
}

// hostProfileFiles is the profile's filesystem on this host.
func hostProfileFiles() profileFiles {
	return profileFiles{
		stat: os.Stat,
		read: func(name string) ([]byte, error) {
			return os.ReadFile(name) //nolint:gosec // G304: the name is the town's daemon patrol file
		},
		open: func(name string, flag int, perm fs.FileMode) (io.WriteCloser, error) {
			return os.OpenFile(name, flag, perm) //nolint:gosec // G304: the name is the town's daemon patrol file or a fixed name beside it
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
	// The daemon loads its patrols from daemon.PatrolConfigFile: the settings/config.json
	// section that stands in for it when the town carries one, else the file
	// itself: mayor/daemon.json on the five-file layout, whose top level is the
	// daemon object. Profiling any other file leaves the patrols running
	// (gt-gvr05p).
	patrolFile := daemon.PatrolConfigFile(env.town)
	path, err := config.PatrolWritePath(patrolFile)
	if err != nil {
		return fmt.Errorf("reading the town settings: %w", err)
	}
	info, err := env.files.stat(path)
	if err != nil {
		return fmt.Errorf("reading the town settings: %w", err)
	}
	settings, err := env.files.read(path)
	if err != nil {
		return fmt.Errorf("reading the town settings: %w", err)
	}
	profile := townstate.RehearsalProfile
	if path == patrolFile {
		profile = townstate.RehearsalDaemonProfile
	}
	out, changes, err := profile(settings, knownPatrolKeys())
	if err != nil {
		return fmt.Errorf("profiling %s: %w", path, err)
	}
	if env.check {
		return checkRehearsal(env, path, changes)
	}
	if len(changes) == 0 {
		fmt.Fprintf(env.out, "%s is already rehearsal-profiled; nothing written\n", path)
		return nil
	}
	if err := writeRehearsalSettings(env.files, path, info.Mode().Perm(), settings, out); err != nil {
		return err
	}
	fmt.Fprintf(env.out, "rehearsal profile written to %s (%d changes); the old file is %s\n", path, len(changes), path+rehearsalBackupSuffix)
	return nil
}

// checkRehearsal is --check: it prints each setting still on in path, one per
// line, and fails with exit 1 when there is one. changes names the file's own
// patrols and supervisor that are still on. The daemon's view is read back
// through ReadPatrolConfig, the loader the daemon runs on, and each known
// patrol the daemon would still run is named as well.
func checkRehearsal(env townStateProfileEnv, path string, changes []string) error {
	cfg, err := daemon.ReadPatrolConfig(env.town)
	if err != nil {
		return fmt.Errorf("reading the daemon patrol config back: %w", err)
	}
	if cfg == nil {
		return fmt.Errorf("reading the daemon patrol config back: no patrol config at %s", daemon.PatrolConfigFile(env.town))
	}
	on := slices.Clone(changes)
	add := func(c string) {
		if !slices.Contains(on, c) {
			on = append(on, c)
		}
	}
	for _, key := range knownPatrolKeys() {
		if patrolReadsOn(cfg, key) {
			add("daemon.patrols." + key + ".enabled")
		}
	}
	if cfg.Supervisor != "" {
		add("daemon.supervisor")
	}
	if len(on) == 0 {
		fmt.Fprintln(env.out, "rehearsal profile: ok")
		return nil
	}
	for _, c := range on {
		fmt.Fprintf(env.out, "%s: %s\n", path, c)
	}
	return &ExitCodeError{Code: 1, Err: fmt.Errorf("rehearsal profile: %d settings still on in %s; run without --check to write the profile", len(on), path)}
}

// patrolReadsOn is whether the daemon that loaded cfg would run the patrol key.
// A patrol whose config carries its own enabled flag (update, dolt_server) is
// gated by that flag, which IsPatrolEnabled does not read: IsPatrolEnabled
// answers true for it by default. Any other patrol, or one the file leaves out,
// is the daemon's own IsPatrolEnabled.
func patrolReadsOn(cfg *daemon.DaemonPatrolConfig, key string) bool {
	if on, ok := patrolOwnFlag(cfg.Patrols, key); ok {
		return on
	}
	return daemon.IsPatrolEnabled(cfg, key)
}

// patrolOwnFlag reads the enabled flag of the patrol key's own config struct,
// and false when the patrol has no such struct, or no flag set in it.
func patrolOwnFlag(patrols *config.PatrolsConfig, key string) (on, ok bool) {
	if patrols == nil {
		return false, false
	}
	v := reflect.ValueOf(patrols).Elem()
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != key {
			continue
		}
		field := v.Field(i)
		if field.Kind() != reflect.Pointer || field.IsNil() || field.Elem().Kind() != reflect.Struct {
			return false, false
		}
		flag := field.Elem().FieldByName("Enabled")
		if !flag.IsValid() {
			return false, false
		}
		switch {
		case flag.Kind() == reflect.Bool:
			return flag.Bool(), true
		case flag.Kind() == reflect.Pointer && !flag.IsNil() && flag.Elem().Kind() == reflect.Bool:
			return flag.Elem().Bool(), true
		}
		return false, false
	}
	return false, false
}

// writeRehearsalSettings keeps old beside path as the rehearsal backup, then
// builds the new settings in a temporary file and renames it over path, so a
// reader sees the old file or the new one. A backup that already exists is
// refused, so an earlier original is never overwritten; a backup that fails
// part-way is left for an operator to look at rather than removed.
func writeRehearsalSettings(files profileFiles, path string, mode fs.FileMode, old, out []byte) error {
	backup := path + rehearsalBackupSuffix
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
