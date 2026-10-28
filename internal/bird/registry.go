package bird

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/procid"
)

// Registry is one state file, the pool Pick draws from and the polecat
// directories whose names count as taken.
//
// Every read and write holds an flock on LockPath, released by the kernel when
// the process exits, so a holder that dies leaves no stale lock to break.
type Registry struct {
	// Path is the state file.
	Path string
	// LockPath is the file flock guards; empty means Path + ".lock".
	LockPath string
	// Pool is the names Pick chooses from; nil means the embedded pool.
	Pool []string
	// PolecatGlob matches polecat directories whose base names are taken;
	// empty counts none.
	PolecatGlob string
	// Now is the clock the registry stamps records with; nil means time.Now.
	Now func() time.Time
	// Rand returns a number in [0, n); nil means the math/rand/v2 source.
	Rand func(n int) int
	// ProcStart reads a pid's start token; nil means procid.StartToken.
	ProcStart func(pid int) (string, bool)
	// Write replaces the state file; nil means atomicfile.EnsureDirAndWriteJSON,
	// which writes a temp file beside Path and renames it over it.
	Write func(path string, v any) error
}

// New returns the Registry of the town rooted at townRoot, with its state file
// under .runtime/agents and every rig's polecat directories counting as taken.
func New(townRoot string) *Registry {
	return &Registry{
		Path:        filepath.Join(townRoot, ".runtime", "agents", "state.json"),
		PolecatGlob: filepath.Join(townRoot, "*", "polecats", "*"),
	}
}

// Live reports whether a names a process that is still running.
func (r *Registry) Live(a Agent) bool {
	return procid.ID{PID: a.PID, Start: a.Start}.Running(r.procStart())
}

// Load returns the state under the registry lock.
func (r *Registry) Load() (*State, error) {
	var st *State
	err := r.withLock(func() error {
		var err error
		st, err = r.read()
		return err
	})
	if err != nil {
		return nil, err
	}
	return st, nil
}

// Update runs fn on the state under the registry lock and writes the result
// atomically. fn returning an error writes nothing, and fn must not call
// another Registry method: it already holds the lock, and a second acquisition
// would wait on itself.
func (r *Registry) Update(fn func(*State) error) error {
	return r.withLock(func() error {
		st, err := r.read()
		if err != nil {
			return err
		}
		if err := fn(st); err != nil {
			return err
		}
		write := r.Write
		if write == nil {
			write = atomicfile.EnsureDirAndWriteJSON
		}
		if err := write(r.Path, st); err != nil {
			return fmt.Errorf("writing the bird registry: %w", err)
		}
		return nil
	})
}

// List prunes dead agents from the state and returns the live ones by name.
func (r *Registry) List() ([]Agent, error) {
	var live []Agent
	err := r.Update(func(st *State) error {
		st.PruneDead(r.Live)
		live = slices.Clone(st.Agents)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(live, func(a, b Agent) int { return strings.Compare(a.Name, b.Name) })
	return live, nil
}

// Bind records that name ran bead, adding nothing when the pair is already
// bound, and reports whether it added the binding.
func (r *Registry) Bind(name, bead string) (bool, error) {
	var added bool
	err := r.Update(func(st *State) error {
		added = st.Bind(name, bead, r.now())
		return nil
	})
	return added, err
}

// Bound returns the newest name bound to bead.
func (r *Registry) Bound(bead string) (string, bool, error) {
	st, err := r.Load()
	if err != nil {
		return "", false, err
	}
	name, ok := st.Bound(bead)
	return name, ok, nil
}

// Owns reports whether name is one of ours: in the pool, or bound to a bead.
func (r *Registry) Owns(name string) (bool, error) {
	if slices.Contains(r.pool(), name) {
		return true, nil
	}
	st, err := r.Load()
	if err != nil {
		return false, err
	}
	for _, b := range st.Bindings {
		if b.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// Taken returns every name in use: bound names, names of live agents, and the
// polecat directory names the glob matches.
func (r *Registry) Taken() (map[string]bool, error) {
	st, err := r.Load()
	if err != nil {
		return nil, err
	}
	polecats, err := r.PolecatNames()
	if err != nil {
		return nil, err
	}
	taken := st.Taken(r.Live)
	for _, name := range polecats {
		taken[name] = true
	}
	return taken, nil
}

// PolecatNames returns the base names of the polecat directories the glob
// matches.
func (r *Registry) PolecatNames() ([]string, error) {
	if r.PolecatGlob == "" {
		return nil, nil
	}
	dirs, err := filepath.Glob(r.PolecatGlob)
	if err != nil {
		return nil, fmt.Errorf("listing polecat directories with %q: %w", r.PolecatGlob, err)
	}
	names := make([]string, len(dirs))
	for i, dir := range dirs {
		names[i] = filepath.Base(dir)
	}
	return names, nil
}

// Pick returns a random free name from the pool; ok is false when every name
// is taken.
func (r *Registry) Pick() (string, bool, error) {
	taken, err := r.Taken()
	if err != nil {
		return "", false, err
	}
	var free []string
	for _, name := range r.pool() {
		if ValidName(name) && !taken[name] {
			free = append(free, name)
		}
	}
	if len(free) == 0 {
		return "", false, nil
	}
	return free[r.index(len(free))], true, nil
}

func (r *Registry) pool() []string {
	if r.Pool != nil {
		return r.Pool
	}
	return Pool()
}

func (r *Registry) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Registry) index(n int) int {
	if r.Rand != nil {
		return r.Rand(n)
	}
	return rand.IntN(n)
}

func (r *Registry) procStart() func(int) (string, bool) {
	if r.ProcStart != nil {
		return r.ProcStart
	}
	return procid.StartToken
}

func (r *Registry) lockPath() string {
	if r.LockPath != "" {
		return r.LockPath
	}
	return r.Path + ".lock"
}

func (r *Registry) withLock(fn func() error) error {
	path := r.lockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating the bird registry directory: %w", err)
	}
	fl := flock.New(path)
	if err := fl.Lock(); err != nil {
		return fmt.Errorf("locking the bird registry: %w", err)
	}
	defer func() { _ = fl.Unlock() }()
	return fn()
}

func (r *Registry) read() (*State, error) {
	data, err := os.ReadFile(r.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return &State{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the bird registry: %w", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("the bird registry at %s is not valid JSON: %w", r.Path, err)
	}
	if st.Agents == nil {
		st.Agents = []Agent{}
	}
	if st.Bindings == nil {
		st.Bindings = []Binding{}
	}
	return &st, nil
}
