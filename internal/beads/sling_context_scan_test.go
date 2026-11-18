package beads

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// writeTownRegistry writes the town config townconfig.Load reads: mayor/town.json
// and mayor/rigs.json, registering exactly rigs.
func writeTownRegistry(t *testing.T, town string, rigs ...string) {
	t.Helper()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(town, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("mayor/town.json", `{"type":"town","version":1,"name":"testtown"}`)
	entries := make([]string, 0, len(rigs))
	for _, rig := range rigs {
		entries = append(entries, fmt.Sprintf("%q:{}", rig))
	}
	write("mayor/rigs.json", fmt.Sprintf(`{"version":1,"rigs":{%s}}`, strings.Join(entries, ",")))
}

func TestSlingContextSearchDirs_TownRigsAndMayorRigsOnly(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	for _, dir := range []string{
		"gastown/.beads",         // a rig with its own store
		"beads/mayor/rig/.beads", // a rig whose store is under mayor/rig
		"norig",                  // registered but no store: skipped
		".hidden/.beads",         // dot dirs are skipped
		"mayor/.beads",           // the mayor's dir is skipped
		"settings/.beads",        // so is settings
	} {
		if err := os.MkdirAll(filepath.Join(town, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTownRegistry(t, town, "beads", "gastown", "norig")

	got, err := SlingContextSearchDirs(town)
	if err != nil {
		t.Fatal(err)
	}
	// RigNames sorts by name, so the order is stable.
	want := []string{
		town,
		filepath.Join(town, "beads", "mayor", "rig"),
		filepath.Join(town, "gastown"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SlingContextSearchDirs = %v, want %v", got, want)
	}
}

// The scan searches the registered rigs, not every folder with a .beads: a
// stale leftover and a route target both hold a store, and the bd gateway's
// workspace holds neither, so reading one fails the dispatch (gt-qczeve).
func TestSlingContextSearchDirsReadsRegisteredRigsOnly(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	for _, dir := range []string{
		"gastown/.beads",
		"beads/.beads",
		"claude/.beads",       // a stale non-rig leftover
		"claude-beads/.beads", // a route target, not a rig
	} {
		if err := os.MkdirAll(filepath.Join(town, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTownRegistry(t, town, "gastown", "beads")

	got, err := SlingContextSearchDirs(town)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		town,
		filepath.Join(town, "beads"),
		filepath.Join(town, "gastown"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SlingContextSearchDirs = %v, want the registered rigs only %v", got, want)
	}
}

func TestSlingContextSearchDirsSkipsMayorRigWithoutBeads(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	for _, dir := range []string{
		"hasstore/mayor/rig/.beads", // a registered rig whose store is under mayor/rig
		"nostore/mayor/rig",         // the same shape, no store
	} {
		if err := os.MkdirAll(filepath.Join(town, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTownRegistry(t, town, "hasstore", "nostore")

	got, err := SlingContextSearchDirs(town)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		town,
		filepath.Join(town, "hasstore", "mayor", "rig"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SlingContextSearchDirs = %v, want %v", got, want)
	}
}

func TestSlingContextSearchDirsFailsOnUnreadableRegistry(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "gastown", ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTownRegistry(t, town, "gastown")
	// A corrupt registry must fail the scan rather than fall back to the
	// folders on disk.
	if err := os.WriteFile(filepath.Join(town, "mayor", "rigs.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := SlingContextSearchDirs(town)
	if err == nil {
		t.Fatalf("SlingContextSearchDirs with a corrupt registry = %v, want an error", got)
	}
	if got != nil {
		t.Errorf("SlingContextSearchDirs returned %v beside its error; a failed read must return no directories", got)
	}
}

// The registry's name reaches the filesystem, so only a name that is one path
// element is accepted: a name that escapes the town, and a name that is a
// path of more than one element, are both refused before any stat.
func TestSlingContextSearchDirsRefusesARigNameThatIsNotOnePathElement(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		rig  string
	}{
		{name: "a name that escapes the town", rig: "../elsewhere"},
		{name: "a name with a separator", rig: "alpha/beta"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeTownRegistry(t, town, tc.rig)

			got, err := SlingContextSearchDirs(town)
			if err == nil {
				t.Fatalf("SlingContextSearchDirs with a rig name %q = %v, want an error", tc.rig, got)
			}
			if got != nil {
				t.Errorf("SlingContextSearchDirs returned %v beside its error; a refused name must return no directories", got)
			}
		})
	}
}

// townconfig.Load requires mayor/town.json and decodes every kernel file, so
// the scan refuses a root that is not a town, and a town whose kernel does not
// load, exactly as the bd gateway does with the same load: it never falls back
// to searching the folders on disk. Both roots hold a store in a folder, which
// the old glob searched.
func TestSlingContextSearchDirsRefusesATownItCannotLoad(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		build func(t *testing.T, town string)
	}{
		{
			name: "a root that is not a town",
			build: func(t *testing.T, town string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(town, "gastown", ".beads"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a town whose kernel file does not load",
			build: func(t *testing.T, town string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(town, "gastown", ".beads"), 0o755); err != nil {
					t.Fatal(err)
				}
				writeTownRegistry(t, town, "gastown")
				// An unrelated kernel file, not the registry: the load fails as
				// a whole, so the scan must not answer from the folders.
				if err := os.MkdirAll(filepath.Join(town, "settings"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(town, "settings", "config.json"), []byte("{not json"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			tc.build(t, town)

			got, err := SlingContextSearchDirs(town)
			if err == nil {
				t.Fatalf("SlingContextSearchDirs(%s) = %v, want an error", tc.name, got)
			}
			if got != nil {
				t.Errorf("SlingContextSearchDirs returned %v beside its error; a failed load must not fall back to the folders on disk", got)
			}
		})
	}
}

func TestAreScheduled_UnlistableTownFailsClosed(t *testing.T) {
	t.Parallel()
	ids := []string{"gt-a", "gt-b"}
	got := AreScheduled(filepath.Join(t.TempDir(), "missing"), ids)
	for _, id := range ids {
		if !got[id] {
			t.Errorf("AreScheduled reported %s unscheduled when contexts could not be listed; a caller would dispatch on a guess", id)
		}
	}
	if len(AreScheduled("unused", nil)) != 0 {
		t.Error("AreScheduled with no IDs must report nothing")
	}
}

// slingScanStore is one database's store as the injected open builds it: it
// answers the open sling contexts it holds, or the error it holds.
type slingScanStore struct {
	Client
	issues []*Issue
	err    error
}

func (s *slingScanStore) List(ListOptions) ([]*Issue, error) { return s.issues, s.err }

// newSlingScanFixture builds a town whose 23 search dirs resolve to 14 beads
// databases. Nine rigs own one database reachable through two dirs, and both
// redirect directions appear: rig00-rig04 route .beads/redirect to
// mayor/rig/.beads, rig05-rig08 have mayor/rig/.beads/redirect pointing back
// at the rig's own .beads. rig09 and rig10 own two distinct stores each, as
// does the town.
//
// It returns the town root and the contexts each resolved beads directory
// holds, keyed by the path ResolveBeadsDir returns.
func newSlingScanFixture(t *testing.T) (string, map[string][]*Issue) {
	t.Helper()
	town := t.TempDir()
	contexts := make(map[string][]*Issue)

	mkdir := func(rel string) string {
		path := filepath.Join(town, rel)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	writeRedirect := func(beadsDir, target string) {
		if err := os.WriteFile(filepath.Join(beadsDir, "redirect"), []byte(target), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hold := func(label, beadsDir string, n int) {
		for i := 0; i < n; i++ {
			contexts[beadsDir] = append(contexts[beadsDir], &Issue{
				ID:          fmt.Sprintf("%s-ctx-%d", label, i),
				Description: fmt.Sprintf("sling context for %s", label),
			})
		}
	}

	hold("town", mkdir(".beads"), 1)
	for i := 0; i < 5; i++ {
		rig := fmt.Sprintf("rig%02d", i)
		writeRedirect(mkdir(filepath.Join(rig, ".beads")), "mayor/rig/.beads")
		hold(rig, mkdir(filepath.Join(rig, "mayor", "rig", ".beads")), 1)
	}
	for i := 5; i < 9; i++ {
		rig := fmt.Sprintf("rig%02d", i)
		hold(rig, mkdir(filepath.Join(rig, ".beads")), 1)
		writeRedirect(mkdir(filepath.Join(rig, "mayor", "rig", ".beads")), "../../.beads")
	}
	for i := 9; i < 11; i++ {
		rig := fmt.Sprintf("rig%02d", i)
		hold(rig+"-rig", mkdir(filepath.Join(rig, ".beads")), 1)
		hold(rig+"-mayor", mkdir(filepath.Join(rig, "mayor", "rig", ".beads")), 1)
	}
	// Every rig the fixture builds is registered: the scan reads the registry,
	// not the folders.
	rigs := make([]string, 0, 11)
	for i := 0; i < 11; i++ {
		rigs = append(rigs, fmt.Sprintf("rig%02d", i))
	}
	writeTownRegistry(t, town, rigs...)
	return town, contexts
}

// scanEverySearchDir is the scan the skip replaces: it queries every search
// dir and drops duplicate records afterwards, so a paired dir costs a second
// query of the same database.
func scanEverySearchDir(dirs []string, contexts map[string][]*Issue) []SlingContextRecord {
	var records []SlingContextRecord
	seen := make(map[string]bool)
	for _, dir := range dirs {
		beadsDir := ResolveBeadsDir(dir)
		for _, ctx := range contexts[beadsDir] {
			key := beadsDir + "\x00" + ctx.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			records = append(records, SlingContextRecord{Issue: ctx, WorkDir: dir, BeadsDir: beadsDir})
		}
	}
	return records
}

func TestListOpenSlingContextRecordsQueriesEachDatabaseOnce(t *testing.T) {
	t.Parallel()
	town, contexts := newSlingScanFixture(t)
	dirs, err := SlingContextSearchDirs(town)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 23 {
		t.Fatalf("fixture has %d search dirs, want 23", len(dirs))
	}

	var opened []string
	open := func(dir string) Client {
		opened = append(opened, dir)
		return &slingScanStore{issues: contexts[ResolveBeadsDir(dir)]}
	}

	got, err := listOpenSlingContextRecords(town, open)
	if err != nil {
		t.Fatal(err)
	}
	if len(opened) != 14 {
		t.Errorf("scan opened %d stores for 23 search dirs, want 14 (one per database): %v", len(opened), opened)
	}

	want := scanEverySearchDir(dirs, contexts)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("scan with the skip = %v, want the records of the 23-call scan %v", got, want)
	}
}

func TestListOpenSlingContextRecordsKeepsTheFirstDirAsWorkDir(t *testing.T) {
	t.Parallel()
	town, contexts := newSlingScanFixture(t)
	open := func(dir string) Client {
		return &slingScanStore{issues: contexts[ResolveBeadsDir(dir)]}
	}

	got, err := listOpenSlingContextRecords(town, open)
	if err != nil {
		t.Fatal(err)
	}

	// rig00 routes .beads/redirect forward to mayor/rig/.beads and rig05's
	// mayor/rig/.beads redirects back. In both, SlingContextSearchDirs lists
	// the rig dir before its mayor/rig, so the rig dir is the one queried.
	for _, rig := range []string{"rig00", "rig05"} {
		first := filepath.Join(town, rig)
		beadsDir := ResolveBeadsDir(first)
		var fromFirst int
		for _, rec := range got {
			if rec.BeadsDir != beadsDir {
				continue
			}
			fromFirst++
			if rec.WorkDir != first {
				t.Errorf("record %s carries WorkDir %s, want the first dir %s", rec.Issue.ID, rec.WorkDir, first)
			}
		}
		if fromFirst != len(contexts[beadsDir]) {
			t.Errorf("%s returned %d records, want %d", beadsDir, fromFirst, len(contexts[beadsDir]))
		}
	}

	expanded := 0
	for _, ctxs := range contexts {
		expanded += len(ctxs)
	}
	if len(got) != expanded {
		t.Errorf("scan returned %d records, want %d: a context must be returned once", len(got), expanded)
	}
}

func TestListOpenSlingContextRecordsFailsTheScanOnAStoreError(t *testing.T) {
	t.Parallel()
	town, contexts := newSlingScanFixture(t)
	broken := errors.New("bd list failed")
	brokenDir := filepath.Join(town, "rig00")
	open := func(dir string) Client {
		if dir == brokenDir {
			return &slingScanStore{err: broken}
		}
		return &slingScanStore{issues: contexts[ResolveBeadsDir(dir)]}
	}

	got, err := listOpenSlingContextRecords(town, open)
	if !errors.Is(err, broken) {
		t.Fatalf("scan error = %v, want the failing store's error", err)
	}
	if got != nil {
		t.Errorf("scan returned %d records beside its error; a partial scan must return none", len(got))
	}
}
