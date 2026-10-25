package modsync

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.HermeticMain(m)) }

// requirements returns module -> version for the require lines of a go.mod,
// in a block or on a line of their own.
func requirements(goMod string) map[string]string {
	out := map[string]string{}
	inBlock := false
	for _, line := range strings.Split(goMod, "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "//", 2)[0])
		switch {
		case line == "require (":
			inBlock = true
		case inBlock && line == ")":
			inBlock = false
		case inBlock:
			if f := strings.Fields(line); len(f) == 2 {
				out[f[0]] = f[1]
			}
		case strings.HasPrefix(line, "require "):
			if f := strings.Fields(strings.TrimPrefix(line, "require ")); len(f) == 2 {
				out[f[0]] = f[1]
			}
		}
	}
	return out
}

// hashed returns the "module version" pairs a go.sum holds a module-content
// hash for (a line without the /go.mod suffix), i.e. the versions whose source
// the root module can have in its cache.
func hashed(goSum string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(goSum, "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && !strings.HasSuffix(f[1], "/go.mod") {
			out[f[0]+" "+f[1]] = true
		}
	}
	return out
}

// unresolvable lists the tracker requirements on a module the root module also
// requires, at a version the root module has no content hash for.
func unresolvable(rootMod, rootSum, trackerMod string) []string {
	root, have, tracker := requirements(rootMod), hashed(rootSum), requirements(trackerMod)
	var bad []string
	for mod, ver := range tracker {
		if _, shared := root[mod]; !shared {
			continue // only the tracker uses it: nothing for it to drift from
		}
		if !have[mod+" "+ver] {
			bad = append(bad, mod+" "+ver+" (root requires "+root[mod]+")")
		}
	}
	sort.Strings(bad)
	return bad
}

func TestTheTrackerModuleRequiresOnlyVersionsTheRootModuleHas(t *testing.T) {
	t.Parallel()
	_, file, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(file), "..", "..")
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(repo, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if bad := unresolvable(read("go.mod"), read("go.sum"), read("tracker/go.mod")); len(bad) > 0 {
		t.Errorf("tracker/go.mod requires versions the root module does not pin:\n  %s\n"+
			"A sandboxed gate resolves modules from the root module's cache only. Pin tracker/go.mod to the root's version "+
			"(go get <module>@<root version> in tracker/, then go mod tidy), or add the module to the root first.", strings.Join(bad, "\n  "))
	}
}

func TestUnresolvableNamesADriftedSharedModuleAndIgnoresTrackerOnlyOnes(t *testing.T) {
	t.Parallel() // unit tier: pure
	root := "module r\nrequire (\n\tgithub.com/go-sql-driver/mysql v1.9.3\n\tgolang.org/x/text v0.29.0 // indirect\n)\n"
	sum := "github.com/go-sql-driver/mysql v1.9.3 h1:aaa=\ngithub.com/go-sql-driver/mysql v1.9.3/go.mod h1:bbb=\ngolang.org/x/text v0.29.0 h1:ccc=\n"
	tracker := "module t\nrequire (\n\tgithub.com/go-sql-driver/mysql v1.10.1\n\tgithub.com/jackc/pgx/v5 v5.11.0\n\tgolang.org/x/text v0.29.0 // indirect\n)\n"
	got := unresolvable(root, sum, tracker)
	if len(got) != 1 || !strings.HasPrefix(got[0], "github.com/go-sql-driver/mysql v1.10.1") || !strings.Contains(got[0], "root requires v1.9.3") {
		t.Errorf("unresolvable = %v, want only mysql v1.10.1 (pgx is the tracker's alone; x/text matches)", got)
	}
	if bad := unresolvable(root, sum, strings.Replace(tracker, "v1.10.1", "v1.9.3", 1)); len(bad) != 0 {
		t.Errorf("an aligned tracker is flagged: %v", bad)
	}
	// A version with only a /go.mod hash is not source the cache has.
	if bad := unresolvable("require x.io/m v1.0.0\n", "x.io/m v1.0.0/go.mod h1:z=\n", "require x.io/m v1.0.0\n"); len(bad) != 1 {
		t.Errorf("a go.mod-only hash counted as present: %v", bad)
	}
}

func TestRequirementsReadsBlocksSingleLinesAndIgnoresComments(t *testing.T) {
	t.Parallel()
	got := requirements("module m\n\nrequire a.io/x v1.0.0\n\nrequire (\n\tb.io/y v2.1.0 // indirect\n\t// c.io/z v9.9.9\n)\nreplace b.io/y => ./y\n")
	if len(got) != 2 || got["a.io/x"] != "v1.0.0" || got["b.io/y"] != "v2.1.0" {
		t.Errorf("requirements = %v, want a.io/x v1.0.0 and b.io/y v2.1.0 only", got)
	}
}
