package bird

import (
	"context"
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

const (
	// defaultLockTimeout bounds a wait for the state lock, so a holder that
	// never returns a write the caller can see failed instead of one that
	// hangs.
	defaultLockTimeout = 5 * time.Second
	// lockRetry is how often a waiter tries the lock again while it waits.
	lockRetry = 10 * time.Millisecond
	// defaultReservationTTL is how long a name Pick handed out stays reserved
	// when nothing binds it.
	defaultReservationTTL = 5 * time.Minute
)

// ErrInvalidName is what Bind refuses a name with: a name has to reach a
// session name, a git identity and a bead label unchanged, so it may only use
// the characters ValidName accepts.
var ErrInvalidName = errors.New("a bird name must match " + nameShape.String())

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
	// ProcStart reads a pid's start token, and says whether the process is
	// still there; an error means the token could not be read, which is not
	// the same as the process being gone. nil means read it from the kernel.
	ProcStart func(pid int) (token string, live bool, err error)
	// LockTimeout bounds a wait for the state lock; zero means
	// defaultLockTimeout.
	LockTimeout time.Duration
	// ReservationTTL is how long a name Pick handed out stays reserved when
	// nothing binds it; zero means defaultReservationTTL.
	ReservationTTL time.Duration
	// Write replaces the state file; nil means atomicfile.EnsureDirAndWriteJSON,
	// which writes a temp file beside Path and renames it over it.
	Write func(path string, v any) error
}

// New returns the Registry of the town rooted at townRoot, with its state file
// under .runtime/agents and every rig's polecat directories counting as taken.
func New(townRoot string) *Registry {
	return &Registry{
		Path:        filepath.Join(townRoot, ".runtime", "agents", "state.json"),
		PolecatGlob: PolecatGlob(townRoot),
	}
}

// PolecatGlob is the glob matching every rig's polecat directories under
// townRoot. The root is escaped, so a town under a directory whose name holds
// a glob metacharacter still matches the polecats inside it rather than a
// pattern of its own.
func PolecatGlob(townRoot string) string {
	return filepath.Join(quoteGlob(townRoot), "*", "polecats", "*")
}

// quoteGlob escapes every character filepath.Match reads as a pattern, so s
// matches itself and nothing else. Windows reads '\' as a path separator
// rather than an escape, so there a root is left as it is.
func quoteGlob(s string) string {
	if os.PathSeparator == '\\' {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, c := range s {
		if c == '*' || c == '?' || c == '[' || c == ']' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// Live reports whether a names a process that is still running. An error means
// the process table could not be read: the caller must not read it as death.
func (r *Registry) Live(a Agent) (bool, error) {
	if a.PID <= 0 || a.Start == "" {
		return false, nil
	}
	token, ok, err := r.procStart()(a.PID)
	if err != nil {
		return false, fmt.Errorf("reading the liveness of %s: %w", a.Name, err)
	}
	return ok && token == a.Start, nil
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
	return r.mutate(func(st *State) (bool, error) {
		return true, fn(st)
	})
}

// mutate is Update with the choice of writing left to fn: it writes only when
// fn reports a change, so a sweep that pruned nothing leaves the file alone.
func (r *Registry) mutate(fn func(*State) (bool, error)) error {
	return r.withLock(func() error {
		st, err := r.read()
		if err != nil {
			return err
		}
		changed, err := fn(st)
		if err != nil {
			return err
		}
		if !changed {
			return nil
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

// List prunes dead agents from the state and returns the live ones by name. A
// liveness it cannot read fails the list rather than pruning the agent, and a
// list that prunes nothing does not rewrite the file.
func (r *Registry) List() ([]Agent, error) {
	var live []Agent
	err := r.mutate(func(st *State) (bool, error) {
		dropped, err := st.PruneDead(r.Live)
		if err != nil {
			return false, err
		}
		live = slices.Clone(st.Agents)
		return dropped > 0, nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(live, func(a, b Agent) int { return strings.Compare(a.Name, b.Name) })
	return live, nil
}

// Bind records that name ran bead, adding nothing when the pair is already
// bound, and reports whether it added the binding. A name no session could use
// is refused, since a binding keeps that name taken for good.
func (r *Registry) Bind(name, bead string) (bool, error) {
	if !ValidName(name) {
		return false, fmt.Errorf("binding %q: %w", name, ErrInvalidName)
	}
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

// Taken returns every name in use: bound names, reserved names, names of live
// agents, and the polecat directory names the glob matches.
func (r *Registry) Taken() (map[string]bool, error) {
	st, err := r.Load()
	if err != nil {
		return nil, err
	}
	polecats, err := r.PolecatNames()
	if err != nil {
		return nil, err
	}
	taken, err := st.Taken(r.now(), r.ttl(), r.Live)
	if err != nil {
		return nil, err
	}
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

// Pick returns a random free name from the pool and reserves it under the
// lock, so two launches never receive the same name before either binds it.
// ok is false when every name is taken. The reservation is dropped when Bind
// records the name, or when it ages past ReservationTTL: a launcher that dies
// between Pick and Bind leaves no name held for good.
func (r *Registry) Pick() (string, bool, error) {
	var picked string
	var free bool
	err := r.mutate(func(st *State) (bool, error) {
		now := r.now()
		ttl := r.ttl()
		dropped, err := st.PruneDead(r.Live)
		if err != nil {
			return false, err
		}
		dropped += st.PruneReservations(now, ttl)
		taken, err := st.Taken(now, ttl, r.Live)
		if err != nil {
			return false, err
		}
		polecats, err := r.PolecatNames()
		if err != nil {
			return false, err
		}
		for _, name := range polecats {
			taken[name] = true
		}
		candidates := make([]string, 0, len(r.pool()))
		for _, name := range r.pool() {
			if ValidName(name) && !taken[name] {
				candidates = append(candidates, name)
			}
		}
		if len(candidates) == 0 {
			return dropped > 0, nil
		}
		picked, free = candidates[r.index(len(candidates))], true
		st.Reserve(picked, now)
		return true, nil
	})
	if err != nil {
		return "", false, err
	}
	return picked, free, nil
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

func (r *Registry) ttl() time.Duration {
	if r.ReservationTTL > 0 {
		return r.ReservationTTL
	}
	return defaultReservationTTL
}

func (r *Registry) index(n int) int {
	if r.Rand != nil {
		return r.Rand(n)
	}
	return rand.IntN(n)
}

func (r *Registry) procStart() func(int) (string, bool, error) {
	if r.ProcStart != nil {
		return r.ProcStart
	}
	return startToken
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
	timeout := r.LockTimeout
	if timeout <= 0 {
		timeout = defaultLockTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	fl := flock.New(path)
	if locked, err := fl.TryLockContext(ctx, lockRetry); !locked {
		return fmt.Errorf("waiting up to %s for the bird registry lock: %w", timeout, err)
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

// startToken reads pid's start token from the kernel. A token the kernel will
// not give may mean the process is gone or that the table cannot be read, and
// only the second is an error: the registry prunes on the first alone.
func startToken(pid int) (string, bool, error) {
	return tokenFrom(pid, procid.StartToken, probeProcess)
}

// tokenFrom is startToken with its two kernel calls handed in, so a test can
// drive every answer without a process table.
func tokenFrom(pid int, token func(int) (string, bool), probe func(int) (bool, error)) (string, bool, error) {
	if pid <= 0 {
		return "", false, nil
	}
	if tok, ok := token(pid); ok {
		return tok, true, nil
	}
	alive, err := probe(pid)
	if err != nil {
		return "", false, err
	}
	if alive {
		return "", false, fmt.Errorf("the kernel gave no start token for process %d", pid)
	}
	return "", false, nil
}
