package convoy

import (
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// gtCall is one gt notice child: its directory, environment and arguments.
type gtCall struct {
	Dir  string
	Env  []string
	Args []string
}

// gtScript is a gtRunner that records every gt call and fails none.
type gtScript struct {
	mu    sync.Mutex
	calls []gtCall
}

func (s *gtScript) run(dir string, env []string, args ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, gtCall{Dir: dir, Env: env, Args: args})
	return nil
}

func (s *gtScript) recorded() []gtCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]gtCall(nil), s.calls...)
}

// envValue is key's value in env, the last one winning as in exec.
func envValue(env []string, key string) string {
	val := ""
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			val = v
		}
	}
	return val
}

// testTown is a Town at root whose every store, the routed issue lookup
// included, is db, and whose gt calls go to gt.
func testTown(root string, db Store, gt *gtScript) Town {
	t := Town{Root: root, Open: func(string) Store { return db }, Issues: db}
	if gt != nil {
		t.gtRun = gt.run
	}
	return t
}

// townDB is an empty fake town database (prefix hq).
func townDB() *beadsfake.Fake { return beadsfake.New(beadsfake.WithPrefix("hq")) }

// seedConvoy stores an open gt:convoy convoy and the issues it tracks. A
// tracked issue that is not already in db is seeded as given.
func seedConvoy(t *testing.T, db *beadsfake.Fake, convoy beads.Issue, tracked ...beads.Issue) {
	t.Helper()
	if convoy.Type == "" {
		convoy.Type = "convoy"
	}
	convoy.Labels = append(convoy.Labels, ConvoyLabel)
	db.Seed(convoy)
	for _, is := range tracked {
		if _, err := db.Show(is.ID); err != nil {
			db.Seed(is)
		}
		if err := db.AddTypedDependency(convoy.ID, is.ID, "tracks"); err != nil {
			t.Fatalf("tracks %s -> %s: %v", convoy.ID, is.ID, err)
		}
	}
}

// rawDepsAnswer scripts db's bd sql answer for the tracked-edge query of
// each convoy in tracks (convoy ID -> raw depends_on_id values); any other
// convoy has none.
func rawDepsAnswer(db *beadsfake.Fake, tracks map[string][]string) {
	db.OnSQL(func(query string) ([][]string, error) {
		rows := [][]string{{"depends_on_id"}}
		for convoyID, targets := range tracks {
			if strings.Contains(query, "issue_id = '"+convoyID+"'") {
				for _, id := range targets {
					rows = append(rows, []string{id})
				}
			}
		}
		return rows, nil
	})
}
