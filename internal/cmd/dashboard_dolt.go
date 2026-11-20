package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/dashboard"
)

// The Dolt panel is built here from the sections the town's own files register.
// Each section registers its reader from its file's init through
// registerDoltSection, so one bead adds one section without editing a list
// another bead holds; dashboardDolt runs them and assembles the panel the hub
// carries (gt-055sfu).

// doltSectionRead fills one section of the Dolt panel. Its context carries the
// town root (doltTownRoot) and ends at doltSectionTimeout: a read that outlives
// the timeout is abandoned and its result dropped.
type doltSectionRead func(ctx context.Context, now time.Time) (dashboard.DoltSection, error)

// doltSectionReader is one registered section: the key it is drawn under and
// the read that fills it.
type doltSectionReader struct {
	key  string
	read doltSectionRead
}

// doltSectionRegistry holds the registered sections in registration order.
type doltSectionRegistry struct {
	mu      sync.Mutex
	readers []doltSectionReader
}

func newDoltSectionRegistry() *doltSectionRegistry {
	return &doltSectionRegistry{}
}

// register adds one section. A registration that cannot name and read a
// section is a programming error at init, so it panics rather than leaving the
// panel a section it cannot draw or two rows under one key.
func (r *doltSectionRegistry) register(key string, read doltSectionRead) {
	if key == "" {
		panic("dolt panel: a section was registered with no key")
	}
	if read == nil {
		panic("dolt panel: section " + strconv.Quote(key) + " has no read")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.readers {
		if existing.key == key {
			panic("dolt panel: section " + strconv.Quote(key) + " is registered twice")
		}
	}
	r.readers = append(r.readers, doltSectionReader{key: key, read: read})
}

// snapshot returns the registered sections, so a panel built now runs the set
// registered when it was built.
func (r *doltSectionRegistry) snapshot() []doltSectionReader {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.readers)
}

// doltSections is the town's registry. Sections register from their own init; a
// test swaps it for its own and restores it.
var doltSections = newDoltSectionRegistry()

// registerDoltSection adds one section of the Dolt panel. A section's own file
// calls it from init.
func registerDoltSection(key string, read doltSectionRead) {
	doltSections.register(key, read)
}

// doltSectionTimeout bounds one section's read in the town's panel. The
// section beads' tests drive a shorter bound by building the panel directly, so
// this figure stays the production one rather than a test's.
var doltSectionTimeout = 20 * time.Second

// doltTownRootKey carries the town root into a section's read.
type doltTownRootKey struct{}

// withDoltTownRoot hands a section's read the town root it draws from.
func withDoltTownRoot(ctx context.Context, townRoot string) context.Context {
	return context.WithValue(ctx, doltTownRootKey{}, townRoot)
}

// doltTownRoot is the town root dashboardDolt hands a section's read, and
// whether it carried one. A section registers from init, before it can know a
// root, so the root travels in the read's context rather than its registration;
// a read that cannot find it fails its own section rather than reading the
// empty root.
func doltTownRoot(ctx context.Context) (string, bool) {
	root, ok := ctx.Value(doltTownRootKey{}).(string)
	return root, ok
}

// dashboardDolt returns the hub's Dolt panel reader over the town's registered
// sections: every read in parallel, each isolated, and the sections returned in
// key order. A read's error, timeout or panic becomes that section's Err and
// never the panel's, so one broken reading costs its own row while the others
// draw.
func dashboardDolt(townRoot string) func() *dashboard.DoltPanel {
	return doltPanel(doltSections.snapshot(), townRoot, doltSectionTimeout)
}

// doltPanel builds the panel reader the town's dashboard uses, over the given
// sections, town root and read bound. dashboardDolt supplies the registered
// sections, the town root and doltSectionTimeout; a test supplies fakes and a
// shorter bound.
func doltPanel(readers []doltSectionReader, townRoot string, timeout time.Duration) func() *dashboard.DoltPanel {
	return func() *dashboard.DoltPanel {
		now := time.Now()
		panel := &dashboard.DoltPanel{At: now, Sections: make([]dashboard.DoltSection, len(readers))}
		var wg sync.WaitGroup
		wg.Add(len(readers))
		for i, r := range readers {
			go func(i int, r doltSectionReader) {
				defer wg.Done()
				panel.Sections[i] = readDoltSection(r, townRoot, timeout, now)
			}(i, r)
		}
		wg.Wait()
		slices.SortFunc(panel.Sections, func(a, b dashboard.DoltSection) int {
			return strings.Compare(a.Key, b.Key)
		})
		return panel
	}
}

// doltReadResult is what one section's read produced: its section, or the error
// that kept it from producing one.
type doltReadResult struct {
	section dashboard.DoltSection
	err     error
}

// readDoltSection runs one section's read under the section timeout. The read
// runs in its own goroutine so its panic stays with the section: the goroutine
// recovers it and reports it as the section's Err. The read's context ends at
// the timeout; a read still running then is abandoned, and the buffered result
// channel keeps its late send from blocking.
func readDoltSection(r doltSectionReader, townRoot string, timeout time.Duration, now time.Time) dashboard.DoltSection {
	section := dashboard.DoltSection{Key: r.key}
	ctx, cancel := context.WithTimeout(withDoltTownRoot(context.Background(), townRoot), timeout)
	defer cancel()

	done := make(chan doltReadResult, 1)
	go func() {
		var got doltReadResult
		defer func() {
			if p := recover(); p != nil {
				got = doltReadResult{err: fmt.Errorf("the %s section panicked: %v", r.key, p)}
			}
			done <- got
		}()
		got.section, got.err = r.read(ctx, now)
	}()

	var err error
	select {
	case got := <-done:
		if got.err == nil {
			// The registration's key is the section's identity: a read cannot
			// move its own row.
			got.section.Key = r.key
			return got.section
		}
		err = got.err
	case <-ctx.Done():
		err = context.DeadlineExceeded
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// A read that returns as its own deadline expires is a timeout too.
		section.Err = "timed out after " + timeout.String()
		return section
	}
	// The Err is drawn on the page, and a failed read's text can carry the
	// address and DSN its dialect printed, so it is scrubbed like the hub's
	// other failed reads (dashWhy).
	section.Err = dashWhy(err)
	return section
}
