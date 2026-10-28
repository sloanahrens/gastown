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
	"syscall"
	"time"

	"github.com/gofrs/flock"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/procid"
)

const (
	// defaultLockTimeout bounds a wait for the state lock, so a write the
	// caller can see fail replaces one that hangs.
	defaultLockTimeout = 5 * time.Second
	// lockRetry is how often a waiter tries the lock again while it waits.
	lockRetry = 10 * time.Millisecond
	// defaultReservationTTL is how long a name Pick handed out stays reserved
	// when nothing binds it.
	defaultReservationTTL = 5 * time.Minute
)

// ErrInvalidName is what Bind refuses a name with, since a name has to reach a
// session name, a git identity and a bead label unchanged.
var ErrInvalidName = errors.New("a bird name must match " + nameShape.String())

// Registry is one state file, the pool Pick draws from and the polecat
// directories whose names count as taken, read and written only under an flock
// on a sibling lock file.
type Registry struct {
	// Path is the state file.
	Path string
	// LockPath is the file flock guards; empty means Path + ".lock".
	LockPath string
	// Pool is the names Pick chooses from; nil means the embedded pool.
	Pool []string
	// PolecatRoot is the town root holding the rigs whose polecat directories
	// count as taken. It is read as the literal path it is, so a root whose
	// own name holds a glob metacharacter still matches the directories
	// inside it.
	PolecatRoot string
	// PolecatGlob is the pattern matching polecat directories under
	// PolecatRoot, one path component per element; empty counts none.
	PolecatGlob string
	// Now is the clock the registry stamps records with; nil means time.Now.
	Now func() time.Time
	// Rand returns a number in [0, n); nil means the math/rand/v2 source.
	Rand func(n int) int
	// ProcStart reads a pid's start token and says whether the kernel still
	// has the process. A live process with no token is one the kernel will not
	// give a token for, and an error means the process table itself could not
	// be read, which is not the same as the process being gone. nil means read
	// it from the kernel.
	ProcStart func(pid int) (token string, live bool, err error)
	// Sleep waits between attempts at the state lock; nil means time.Sleep. A
	// test drives a fake clock through it, so no test waits real time.
	Sleep func(time.Duration)
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

// New returns the Registry of the town rooted at townRoot.
func New(townRoot string) *Registry {
	return &Registry{
		Path:        filepath.Join(townRoot, ".runtime", "agents", "state.json"),
		PolecatRoot: townRoot,
		PolecatGlob: filepath.Join("*", "polecats", "*"),
	}
}

// Live reports whether the agent a still has its process running, failing
// rather than answering false when the process table cannot be read.
func (r *Registry) Live(a Agent) (bool, error) {
	if a.PID <= 0 {
		return false, nil
	}
	token, live, err := r.procStart()(a.PID)
	if err != nil {
		return false, fmt.Errorf("reading the liveness of %s: %w", a.Name, err)
	}
	if !live {
		return false, nil
	}
	if a.Start == "" || token == "" {
		// One side has no start token to compare, so the kernel's process
		// cannot be told from the agent that recorded it: it is a process
		// that is there, and pruning a bird that may still be running is
		// worse than keeping the record until it exits.
		return true, nil
	}
	return token == a.Start, nil
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
// atomically, writing nothing when fn fails; fn must not call another Registry
// method, since it already holds the lock and a second acquisition would wait
// on itself.
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

// List prunes dead agents from the state and returns the live ones by name,
// failing rather than pruning when a liveness cannot be read.
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
// bound, and reports whether it added it, refusing a name no session could use
// since a binding keeps that name taken for good.
func (r *Registry) Bind(name, bead string) (bool, error) {
	if !ValidName(name) {
		return false, fmt.Errorf("binding %q: %w", name, ErrInvalidName)
	}
	var added bool
	err := r.mutate(func(st *State) (bool, error) {
		reserved := len(st.Reservations)
		added = st.Bind(name, bead, r.now())
		// The pair may have been bound already, but Bind also drops a
		// reservation Pick made for the name, which is a change of its own.
		return added || len(st.Reservations) != reserved, nil
	})
	if err != nil {
		// A write that failed bound nothing, whatever the in-memory state said.
		return false, err
	}
	return added, nil
}

// Record writes a as the agent holding its name, replacing the record of that
// name and dropping the reservation Pick made for it, so the name reads as
// held by a process rather than as free. A zero Started is stamped from the
// registry's clock. A record whose Start is empty is kept while its pid lives:
// see Live.
func (r *Registry) Record(a Agent) error {
	if !ValidName(a.Name) {
		return fmt.Errorf("recording %q: %w", a.Name, ErrInvalidName)
	}
	if a.PID <= 0 {
		return fmt.Errorf("recording the agent %q: a bird holds a name through the pid that runs it", a.Name)
	}
	if a.Started.IsZero() {
		a.Started = r.now()
	}
	return r.Update(func(st *State) error {
		st.DropReservation(a.Name)
		st.SetAgent(a)
		return nil
	})
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

// Names returns the pool Pick chooses from, which is also the pool Plan picks a
// free name out of.
func (r *Registry) Names() []string {
	return slices.Clone(r.pool())
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
	dirs, err := expandGlob(r.PolecatRoot, r.PolecatGlob)
	if err != nil {
		return nil, fmt.Errorf("listing polecat directories with %q under %q: %w", r.PolecatGlob, r.PolecatRoot, err)
	}
	names := make([]string, len(dirs))
	for i, dir := range dirs {
		names[i] = filepath.Base(dir)
	}
	return names, nil
}

// expandGlob returns the paths under root matching pattern, one pattern
// component per element. root is the literal directory it is, and only the
// components of pattern are matched. filepath.Glob reads a whole pattern at
// once, and Windows has no way to escape a metacharacter in it, so a town root
// whose own name holds one (say "gt [x] *?") would match the wrong directories
// or none.
func expandGlob(root, pattern string) ([]string, error) {
	return expandGlobStat(root, pattern, os.Stat)
}

// expandGlobStat is expandGlob with the stat of a literal component passed in,
// so a test can answer it with an error no fixture directory produces.
func expandGlobStat(root, pattern string, stat func(string) (fs.FileInfo, error)) ([]string, error) {
	parts := strings.Split(pattern, string(filepath.Separator))
	for _, part := range parts {
		if !hasWildcard(part) {
			continue
		}
		// Report a malformed wildcard even when no directory is read, as
		// filepath.Glob does.
		if _, err := filepath.Match(part, ""); err != nil {
			return nil, err
		}
	}
	matches := []string{root}
	for _, part := range parts {
		var next []string
		for _, dir := range matches {
			if !hasWildcard(part) {
				// A literal component matches the name it is, and only while
				// that is there. Only "not there" means no match: any other
				// error (a directory above it that cannot be searched) hides
				// directories the taken set must hold, so it is the caller's
				// error, never a quiet miss.
				//
				// A regular file where a directory should be (a file in the town
				// root matches the leading "*") answers ENOTDIR: it has no
				// polecats under it, which is no match too.
				path := filepath.Join(dir, part)
				_, err := stat(path)
				switch {
				case err == nil:
					next = append(next, path)
				case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
				default:
					return nil, fmt.Errorf("checking %q: %w", path, err)
				}
				continue
			}
			entries, err := os.ReadDir(dir)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("reading %q: %w", dir, err)
			}
			for _, entry := range entries {
				// The pattern's components were matched once above, so
				// Match cannot fail on one here; only its answer is read.
				if ok, _ := filepath.Match(part, entry.Name()); ok {
					next = append(next, filepath.Join(dir, entry.Name()))
				}
			}
		}
		matches = next
	}
	return matches, nil
}

// hasWildcard reports whether part holds a character filepath.Match reads as a
// pattern. On Windows '\' separates path components, so it is never in one.
func hasWildcard(part string) bool {
	return strings.ContainsAny(part, "*?[\\")
}

// Pick returns a random free name from the pool and reserves it under the
// lock, so two launches never receive the same name before either binds it; ok
// is false when every name is taken, and a reservation nothing binds expires
// after ReservationTTL.
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
		taken := st.Held(now, ttl)
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

func (r *Registry) timeout() time.Duration {
	if r.LockTimeout > 0 {
		return r.LockTimeout
	}
	return defaultLockTimeout
}

func (r *Registry) sleep(d time.Duration) {
	if r.Sleep != nil {
		r.Sleep(d)
		return
	}
	time.Sleep(d)
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

// withLock runs fn under the state lock, trying the lock until LockTimeout has
// passed on the registry's clock and its Sleep seam has waited between tries.
func (r *Registry) withLock(fn func() error) (err error) {
	path := r.lockPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating the bird registry directory: %w", err)
	}
	timeout := r.timeout()
	deadline := r.now().Add(timeout)
	fl := flock.New(path)
	for {
		// TryLock asks the kernel once and never waits, so the only waiting
		// here is this loop's own and a test scripts all of it.
		locked, lockErr := fl.TryLock()
		if lockErr != nil {
			return fmt.Errorf("taking the bird registry lock: %w", lockErr)
		}
		if locked {
			break
		}
		if !r.now().Before(deadline) {
			return fmt.Errorf("waiting up to %s for the bird registry lock: another process still holds it", timeout)
		}
		r.sleep(lockRetry)
	}
	defer func() {
		if unlockErr := fl.Unlock(); unlockErr != nil && err == nil {
			err = fmt.Errorf("releasing the bird registry lock: %w", unlockErr)
		}
	}()
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
// not give may mean the process is gone, or that it is there but its token is
// not readable (a process under another uid); only a table that cannot be read
// at all is an error, since the registry prunes on a gone process alone.
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
		// A live process with no token it will give: live, with nothing to
		// compare against.
		return "", true, nil
	}
	return "", false, nil
}
