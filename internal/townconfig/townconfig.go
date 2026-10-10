// Package townconfig is the town config kernel (gt-y3pgh.1, wayfinder D5).
//
// Load reads the town's config files once into an immutable, validated Town:
// mayor/town.json, mayor/rigs.json, settings/config.json, every registered
// rig's settings/config.json, mayor/daemon.json, settings/daemon.env and the
// managed Dolt server config .dolt-data/config.yaml, plus any pre-registry
// park records under .beads-wisp/config (parked.go). Every file is decoded
// strictly through internal/config's one parser, so an unknown key is an
// error.
//
// On the two-file layout (gt config migrate, internal/config/layout.go)
// mayor/rigs.json and mayor/daemon.json are sections of mayor/town.json and
// settings/config.json. The kernel reads them through the same paths: the
// config loaders follow a retired file to its section, so both layouts
// load, and gt doctor's config-layout check warns until the town moves.
//
// A file that does not parse fails the whole load, with one line per broken
// file naming the file and position. Nothing falls back to compiled
// defaults and there is no last-known-good copy: the town does not start
// until the operator fixes the file. An absent optional file reads as
// absent (Present reports it), never as defaults.
//
// Town has exactly one resolver per fact. The accessors return copies, so no
// caller can change what another caller reads. Writes go through
// config.WriteConfigJSON / config.UpdateConfigJSON, never through a Town.
package townconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/steveyegge/gastown/internal/config"
)

// The files the kernel reads, relative to the town root.
const (
	FileTown      = "mayor/town.json"
	FileRigs      = "mayor/rigs.json"
	FileSettings  = "settings/config.json"
	FileDaemon    = "mayor/daemon.json"
	FileDaemonEnv = "settings/daemon.env"
	FileDolt      = ".dolt-data/config.yaml"
)

var (
	// ErrNotATown: the root has no mayor/town.json.
	ErrNotATown = errors.New("not a Gas Town root")
	// ErrUnknownRig: the rig is not in mayor/rigs.json. It is never given
	// a fallback prefix (G3-06).
	ErrUnknownRig = errors.New("unknown rig")
	// ErrNoRigPrefix: the rig is registered without a beads prefix.
	ErrNoRigPrefix = errors.New("rig has no beads prefix")
)

// DoltEndpoint is the town's Dolt server listener (Town.DoltEndpoint) and
// the data directory gt dolt start wrote to .dolt-data/config.yaml.
type DoltEndpoint struct {
	Host    string
	Port    int
	DataDir string
}

// Town is one validated, immutable read of a town's config files.
type Town struct {
	root      string
	present   map[string]bool
	identity  config.TownConfig
	rigs      map[string]config.RigEntry
	settings  *config.TownSettings
	daemon    *config.DaemonPatrolConfig
	daemonEnv map[string]string
	dolt      DoltEndpoint
	// literalSecrets are the agent env values in settings/config.json that
	// hold a token in plain text (secrets.go).
	literalSecrets []config.LiteralSecret
	// legacyParked holds, per rig, why its pre-registry wisp park record
	// makes it read as parked (parked.go).
	legacyParked map[string]error
}

// Load reads every kernel file under root. The error joins one error per
// broken file (each a single line naming the file); on error the Town is nil.
// A root without mayor/town.json is ErrNotATown.
func Load(root string) (*Town, error) {
	t, errs := load(root)
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return t, nil
}

// Check reports every kernel file under root that exists and does not load,
// one line per file. Unlike Load it does not require mayor/town.json: it
// answers "do the files that are here load", and finding the town is the
// caller's job (a first run has no files yet). The startup gate, the daemon
// and gt doctor call it.
func Check(root string) error {
	_, errs := load(root)
	var kept []error
	for _, err := range errs {
		if !errors.Is(err, ErrNotATown) {
			kept = append(kept, err)
		}
	}
	return errors.Join(kept...)
}

func load(root string) (*Town, []error) {
	t := &Town{root: root, present: map[string]bool{}, rigs: map[string]config.RigEntry{}, daemonEnv: map[string]string{}}
	var errs []error
	for _, step := range []func() error{t.loadTown, t.loadRigs, t.loadRigSettings, t.loadLegacyParked, t.loadSettings, t.loadDaemon, t.loadDaemonEnv, t.loadDolt} {
		if err := step(); err != nil {
			errs = append(errs, err)
		}
	}
	return t, errs
}

func (t *Town) path(file string) string { return filepath.Join(t.root, file) }

// read returns the file's bytes, or nil with no error when it is absent.
func (t *Town) read(file string) ([]byte, error) {
	data, err := os.ReadFile(t.path(file)) //nolint:gosec // G304: kernel file under the town root
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", t.path(file), err)
	}
	t.present[file] = true
	return data, nil
}

func (t *Town) loadTown() error {
	cfg, err := config.LoadTownConfig(t.path(FileTown))
	if errors.Is(err, config.ErrNotFound) {
		return fmt.Errorf("%w: %s does not exist", ErrNotATown, t.path(FileTown))
	}
	if err != nil {
		return oneLine(t.path(FileTown), err)
	}
	t.present[FileTown] = true
	t.identity = *cfg
	return nil
}

func (t *Town) loadRigs() error {
	cfg, err := config.LoadRigsConfig(t.path(FileRigs))
	if errors.Is(err, config.ErrNotFound) {
		return nil
	}
	if err != nil {
		return oneLine(t.path(FileRigs), err)
	}
	t.present[FileRigs] = true
	owner := map[string]string{}
	var dups []string
	for _, name := range sortedKeys(cfg.Rigs) {
		entry := cfg.Rigs[name]
		t.rigs[name] = entry
		p := normalizePrefix(entry)
		if p == "" {
			continue
		}
		if other, taken := owner[p]; taken {
			dups = append(dups, fmt.Sprintf("rigs %q and %q share prefix %q", other, name, p))
			continue
		}
		owner[p] = name
	}
	if len(dups) > 0 {
		return fmt.Errorf("%s: %s; bead ids of one would route into the other's database", t.path(FileRigs), strings.Join(dups, "; "))
	}
	return nil
}

// loadRigSettings validates every registered rig's settings/config.json, the
// sibling of the town file this kernel already reads. A rig file with an
// unknown key decodes to nothing at resolve time, so the rig silently drops to
// the town default agent; the load catches it instead (gt-ptysu). Absent is
// the one silent answer — that is the default.
func (t *Town) loadRigSettings() error {
	var errs []error
	for _, name := range sortedKeys(t.rigs) {
		// The rig directory under the town root, never local_repo: that is a
		// reference clone for sharing git objects and holds no settings the
		// town reads (gt-4b0i1).
		path := config.RigSettingsPath(filepath.Join(t.root, name))
		if _, err := config.LoadRigSettings(path); err != nil && !errors.Is(err, config.ErrNotFound) {
			errs = append(errs, oneLine(path, err))
		}
	}
	return errors.Join(errs...)
}

func (t *Town) loadSettings() error {
	data, err := t.read(FileSettings)
	if err != nil || data == nil {
		return err
	}
	var s config.TownSettings
	if err := config.DecodeJSONFile(t.path(FileSettings), data, &s); err != nil {
		return err
	}
	if s.Type != "town-settings" && s.Type != "" {
		return fmt.Errorf("%s: %w: expected type 'town-settings', got %q", t.path(FileSettings), config.ErrInvalidType, s.Type)
	}
	if s.Version > config.CurrentTownSettingsVersion {
		return fmt.Errorf("%s: %w: got %d, max supported %d", t.path(FileSettings), config.ErrInvalidVersion, s.Version, config.CurrentTownSettingsVersion)
	}
	if err := s.PolecatPool.Validate(); err != nil {
		return oneLine(t.path(FileSettings), err)
	}
	if err := t.checkLiteralSecrets(&s); err != nil {
		return err
	}
	t.settings = &s
	return nil
}

func (t *Town) loadDaemon() error {
	cfg, err := config.LoadDaemonPatrolConfig(t.path(FileDaemon))
	if errors.Is(err, config.ErrNotFound) {
		return nil
	}
	if err != nil {
		return oneLine(t.path(FileDaemon), err)
	}
	t.present[FileDaemon] = true
	t.daemon = cfg
	return nil
}

func (t *Town) loadDaemonEnv() error {
	if _, err := os.Stat(t.path(FileDaemonEnv)); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	env, err := config.LoadDaemonEnv(t.root)
	if err != nil {
		return err
	}
	t.present[FileDaemonEnv] = true
	t.daemonEnv = env
	return nil
}

// doltServerFile is the schema gt dolt start writes (doltserver.writeServerConfig).
type doltServerFile struct {
	LogLevel string `yaml:"log_level"`
	Listener struct {
		Host               string `yaml:"host"`
		Port               int    `yaml:"port"`
		MaxConnections     int    `yaml:"max_connections"`
		ReadTimeoutMillis  int    `yaml:"read_timeout_millis"`
		WriteTimeoutMillis int    `yaml:"write_timeout_millis"`
	} `yaml:"listener"`
	DataDir  string `yaml:"data_dir"`
	Behavior struct {
		DoltTransactionCommit bool   `yaml:"dolt_transaction_commit"`
		EventScheduler        string `yaml:"event_scheduler"`
		AutoGCBehavior        struct {
			Enable       bool `yaml:"enable"`
			ArchiveLevel int  `yaml:"archive_level"`
		} `yaml:"auto_gc_behavior"`
	} `yaml:"behavior"`
	SystemVariables map[string]any `yaml:"system_variables"`
}

func (t *Town) loadDolt() error {
	data, err := t.read(FileDolt)
	if err != nil || data == nil {
		return err
	}
	var f doltServerFile
	if err := config.DecodeYAMLFile(t.path(FileDolt), data, &f); err != nil {
		return err
	}
	if f.Listener.Port < 1 || f.Listener.Port > 65535 {
		return fmt.Errorf("%s: listener.port %d is not a TCP port", t.path(FileDolt), f.Listener.Port)
	}
	t.dolt = DoltEndpoint{Host: f.Listener.Host, Port: f.Listener.Port, DataDir: f.DataDir}
	return nil
}

// oneLine makes sure a loader's error names the file. The loaders' errors
// are single lines already: a *config.ParseError names the file itself.
func oneLine(path string, err error) error {
	if strings.Contains(err.Error(), path) {
		return err
	}
	return fmt.Errorf("%s: %w", path, err)
}

func normalizePrefix(entry config.RigEntry) string {
	if entry.BeadsConfig == nil {
		return ""
	}
	return strings.TrimSuffix(entry.BeadsConfig.Prefix, "-")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// clone deep-copies a config value through its JSON form, the same form it
// was decoded from, so the copy carries every field the file can set.
func clone[T any](v *T) *T {
	if v == nil {
		return nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("townconfig: copying %T: %v", v, err))
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		panic(fmt.Sprintf("townconfig: copying %T: %v", v, err))
	}
	return &out
}

// Root is the town root the kernel was loaded from.
func (t *Town) Root() string { return t.root }

// Present reports whether a kernel file (one of the File* constants) exists.
func (t *Town) Present(file string) bool { return t.present[file] }

// Identity is mayor/town.json.
func (t *Town) Identity() config.TownConfig { return t.identity }

// RigNames lists the registered rigs, sorted.
func (t *Town) RigNames() []string { return sortedKeys(t.rigs) }

// Rig returns a registered rig's registry entry, or ErrUnknownRig.
func (t *Town) Rig(name string) (config.RigEntry, error) {
	entry, ok := t.rigs[name]
	if !ok {
		return config.RigEntry{}, fmt.Errorf("%w %q: not in %s", ErrUnknownRig, name, t.path(FileRigs))
	}
	return *clone(&entry), nil
}

// RigPrefix is the rig's beads prefix without a trailing hyphen. An
// unregistered rig is ErrUnknownRig and a rig without a prefix is
// ErrNoRigPrefix; neither is ever answered with "gt" (G3-06).
func (t *Town) RigPrefix(name string) (string, error) {
	entry, ok := t.rigs[name]
	if !ok {
		return "", fmt.Errorf("%w %q: not in %s", ErrUnknownRig, name, t.path(FileRigs))
	}
	p := normalizePrefix(entry)
	if p == "" {
		return "", fmt.Errorf("%w: rig %q in %s", ErrNoRigPrefix, name, t.path(FileRigs))
	}
	return p, nil
}

// RigPrefixes lists every registered rig's prefix, sorted.
func (t *Town) RigPrefixes() []string {
	var out []string
	for _, entry := range t.rigs {
		if p := normalizePrefix(entry); p != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// Settings is settings/config.json, or nil when the file is absent.
func (t *Town) Settings() *config.TownSettings { return clone(t.settings) }

// Operational is the operational block of settings/config.json. It is never
// nil; its accessors supply the documented default for a knob the file
// leaves unset.
func (t *Town) Operational() *config.OperationalConfig {
	if t.settings == nil || t.settings.Operational == nil {
		return &config.OperationalConfig{}
	}
	return clone(t.settings.Operational)
}

// Daemon is mayor/daemon.json, or nil when the file is absent.
func (t *Town) Daemon() *config.DaemonPatrolConfig { return clone(t.daemon) }

// DaemonEnv is settings/daemon.env, empty when the file is absent.
func (t *Town) DaemonEnv() map[string]string {
	out := make(map[string]string, len(t.daemonEnv))
	for k, v := range t.daemonEnv {
		out[k] = v
	}
	return out
}

// DoltEndpoint is the town's Dolt server endpoint, with the precedence of
// config.ResolveDoltEndpoint: mayor/town.json "dolt", else the managed
// .dolt-data/config.yaml listener. ok is false when neither is present.
func (t *Town) DoltEndpoint() (DoltEndpoint, bool) {
	if d := t.identity.Dolt; d != nil {
		ep := t.dolt
		ep.Host, ep.Port = strings.TrimSpace(d.Host), d.Port
		return ep, true
	}
	return t.dolt, t.present[FileDolt]
}
