package nodedeps

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeLock writes dir's package-lock.json with the given body.
func writeLock(t *testing.T, dir, lock string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "package-lock.json"), []byte(lock), 0o644); err != nil {
		t.Fatalf("writing package-lock.json: %v", err)
	}
}

// install writes the package.json an installed package at rel reports.
func install(t *testing.T, dir, rel, version string) {
	t.Helper()
	pkgDir := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", rel, err)
	}
	body := `{"name":"x","version":"` + version + `"}`
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s/package.json: %v", rel, err)
	}
}

// nodeModules marks dir as having an installed tree; install already does.
func nodeModules(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755); err != nil {
		t.Fatalf("creating node_modules: %v", err)
	}
}

// check runs Check and fails the test on an error.
func check(t *testing.T, dir string) Result {
	t.Helper()
	res, err := Check(dir)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return res
}

func TestCheckReportsAPackageBehindItsLockfile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nodeModules(t, dir)
	writeLock(t, dir, `{
	  "name": "app", "lockfileVersion": 3,
	  "packages": {
	    "": {"name": "app", "version": "1.0.0"},
	    "node_modules/eslint-plugin-react-hooks": {"version": "7.1.1"},
	    "node_modules/eslint": {"version": "9.0.0"}
	  }
	}`)
	install(t, dir, "node_modules/eslint-plugin-react-hooks", "7.0.1")
	install(t, dir, "node_modules/eslint", "9.0.0")

	got := check(t, dir)
	want := []Mismatch{{
		Path:      "node_modules/eslint-plugin-react-hooks",
		Installed: "7.0.1",
		Locked:    "7.1.1",
	}}
	if !reflect.DeepEqual(got.Mismatches, want) {
		t.Errorf("Mismatches = %+v, want %+v", got.Mismatches, want)
	}
}

func TestCheckReportsAnInstalledPackageAheadOfItsLockfile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nodeModules(t, dir)
	writeLock(t, dir, `{"lockfileVersion": 3, "packages": {"node_modules/left-pad": {"version": "1.0.0"}}}`)
	install(t, dir, "node_modules/left-pad", "1.3.0")

	got := check(t, dir).Mismatches
	if len(got) != 1 || got[0].Installed != "1.3.0" || got[0].Locked != "1.0.0" {
		t.Errorf("Mismatches = %+v, want left-pad installed 1.3.0 against locked 1.0.0", got)
	}
}

func TestCheckIsSortedByInstalledPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nodeModules(t, dir)
	writeLock(t, dir, `{
	  "lockfileVersion": 3,
	  "packages": {
	    "node_modules/zeta": {"version": "2.0.0"},
	    "node_modules/alpha": {"version": "2.0.0"}
	  }
	}`)
	install(t, dir, "node_modules/zeta", "1.0.0")
	install(t, dir, "node_modules/alpha", "1.0.0")

	var paths []string
	for _, m := range check(t, dir).Mismatches {
		paths = append(paths, m.Path)
	}
	if want := []string{"node_modules/alpha", "node_modules/zeta"}; !reflect.DeepEqual(paths, want) {
		t.Errorf("paths = %v, want %v", paths, want)
	}
}

func TestCheckComparesANestedDuplicateAtItsOwnPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nodeModules(t, dir)
	writeLock(t, dir, `{
	  "lockfileVersion": 3,
	  "packages": {
	    "node_modules/b": {"version": "2.0.0"},
	    "node_modules/a/node_modules/b": {"version": "1.0.0"}
	  }
	}`)
	install(t, dir, "node_modules/b", "2.0.0")
	install(t, dir, "node_modules/a/node_modules/b", "3.0.0")

	got := check(t, dir).Mismatches
	want := []Mismatch{{
		Path:      "node_modules/a/node_modules/b",
		Installed: "3.0.0",
		Locked:    "1.0.0",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Mismatches = %+v, want only the nested duplicate %+v", got, want)
	}
}

func TestCheckComparesAWorkspacesOwnInstall(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nodeModules(t, dir)
	writeLock(t, dir, `{
	  "lockfileVersion": 3,
	  "packages": {
	    "": {"name": "app", "version": "1.0.0"},
	    "packages/web": {"name": "web", "version": "0.1.0"},
	    "node_modules/web": {"resolved": "packages/web", "link": true},
	    "packages/web/node_modules/bar": {"version": "2.0.0"}
	  }
	}`)
	install(t, dir, "packages/web/node_modules/bar", "1.0.0")

	got := check(t, dir).Mismatches
	want := []Mismatch{{
		Path:      "packages/web/node_modules/bar",
		Installed: "1.0.0",
		Locked:    "2.0.0",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Mismatches = %+v, want the workspace's own install %+v", got, want)
	}
}

func TestCheckReportsARequiredPinTheTreeNeverInstalled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nodeModules(t, dir)
	writeLock(t, dir, `{
	  "lockfileVersion": 3,
	  "packages": {
	    "node_modules/left-pad": {"version": "1.3.0", "resolved": "https://registry.example/left-pad"},
	    "node_modules/installed": {"version": "1.0.0"}
	  }
	}`)
	install(t, dir, "node_modules/installed", "1.0.0")

	got := check(t, dir).Mismatches
	want := []Mismatch{{Path: "node_modules/left-pad", Locked: "1.3.0", Missing: true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Mismatches = %+v, want the required pin reported missing %+v", got, want)
	}
}

func TestCheckLeavesOutPinsNpmMayOmit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nodeModules(t, dir)
	// Each of these can be absent from a correct install: an optional or
	// platform-specific package, a dev package under --omit=dev, a peer, and
	// a package bundled inside its parent.
	writeLock(t, dir, `{
	  "lockfileVersion": 3,
	  "packages": {
	    "node_modules/fsevents": {"version": "2.3.3", "optional": true},
	    "node_modules/eslint": {"version": "9.0.0", "dev": true},
	    "node_modules/react": {"version": "19.0.0", "peer": true},
	    "node_modules/bundled": {"version": "1.0.0", "inBundle": true}
	  }
	}`)

	got := check(t, dir)
	if len(got.Mismatches) != 0 {
		t.Errorf("Mismatches = %+v, want none: npm may omit each of these", got.Mismatches)
	}
}

func TestCheckLeavesOutAMissingNestedDuplicate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nodeModules(t, dir)
	// Deduping is npm's business: only the tree's own node_modules is a
	// place a fresh install always writes.
	writeLock(t, dir, `{
	  "lockfileVersion": 3,
	  "packages": {"node_modules/a/node_modules/b": {"version": "1.0.0"}}
	}`)

	if got := check(t, dir).Mismatches; len(got) != 0 {
		t.Errorf("Mismatches = %+v, want none for a nested duplicate that is not installed", got)
	}
}

func TestCheckLeavesOutAWorkspaceLinkAndItsTarget(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nodeModules(t, dir)
	writeLock(t, dir, `{
	  "lockfileVersion": 3,
	  "packages": {
	    "": {"name": "app", "version": "1.0.0"},
	    "packages/web": {"name": "web", "version": "0.1.0"},
	    "node_modules/web": {"resolved": "packages/web", "link": true}
	  }
	}`)
	// The symlinked workspace reports its own version, which no lockfile
	// entry pins.
	install(t, dir, "packages/web", "0.1.0")
	install(t, dir, "node_modules/web", "0.1.0")

	got := check(t, dir)
	if len(got.Mismatches) != 0 {
		t.Errorf("Mismatches = %+v, want none for workspace links", got.Mismatches)
	}
}

func TestCheckLeavesOutAPinThatIsNotAVersion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nodeModules(t, dir)
	writeLock(t, dir, `{
	  "lockfileVersion": 3,
	  "packages": {
	    "node_modules/local": {"version": "file:../local"},
	    "node_modules/fromgit": {"version": "git+https://example/repo.git#abc"}
	  }
	}`)
	install(t, dir, "node_modules/local", "9.9.9")
	install(t, dir, "node_modules/fromgit", "9.9.9")

	got := check(t, dir)
	if len(got.Mismatches) != 0 {
		t.Errorf("Mismatches = %+v, want none: neither pin names a version to compare", got.Mismatches)
	}
}

func TestCheckReadsALockfileVersion1Tree(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nodeModules(t, dir)
	writeLock(t, dir, `{
	  "lockfileVersion": 1,
	  "dependencies": {
	    "a": {"version": "1.0.0", "dependencies": {"b": {"version": "2.0.0"}}},
	    "c": {"version": "3.0.0"}
	  }
	}`)
	install(t, dir, "node_modules/a", "1.0.0")
	install(t, dir, "node_modules/a/node_modules/b", "1.0.0")
	install(t, dir, "node_modules/c", "3.0.0")

	got := check(t, dir).Mismatches
	want := []Mismatch{{
		Path:      "node_modules/a/node_modules/b",
		Installed: "1.0.0",
		Locked:    "2.0.0",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Mismatches = %+v, want the nested version 1 entry %+v", got, want)
	}
}

func TestCheckReportsAPackageItCannotCompare(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nodeModules(t, dir)
	writeLock(t, dir, `{
	  "lockfileVersion": 3,
	  "packages": {
	    "node_modules/broken": {"version": "1.0.0"},
	    "node_modules/versionless": {"version": "1.0.0"},
	    "node_modules/stale": {"version": "2.0.0"}
	  }
	}`)
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_modules", "broken", "package.json"), []byte(`{"version": `), 0o644); err != nil {
		t.Fatal(err)
	}
	install(t, dir, "node_modules/versionless", "x")
	install(t, dir, "node_modules/stale", "1.0.0")

	got := check(t, dir)
	// The mismatches it reached are still named: one unreadable entry does
	// not hide the rest of the tree.
	want := []Mismatch{{Path: "node_modules/stale", Installed: "1.0.0", Locked: "2.0.0"}}
	if !reflect.DeepEqual(got.Mismatches, want) {
		t.Errorf("Mismatches = %+v, want %+v", got.Mismatches, want)
	}
	wantUnreadable := []string{"node_modules/broken", "node_modules/versionless"}
	if !reflect.DeepEqual(got.NotCompared, wantUnreadable) {
		t.Errorf("NotCompared = %v, want %v", got.NotCompared, wantUnreadable)
	}
}

func TestCheckWithoutALockfileOrNodeModules(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{"no lockfile", func(t *testing.T, dir string) { nodeModules(t, dir) }},
		{"no node_modules", func(t *testing.T, dir string) {
			writeLock(t, dir, `{"lockfileVersion": 3, "packages": {"node_modules/left-pad": {"version": "1.0.0"}}}`)
		}},
		{"an empty tree", func(t *testing.T, dir string) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			tc.setup(t, dir)
			got := check(t, dir)
			if len(got.Mismatches) != 0 || len(got.NotCompared) != 0 {
				t.Errorf("Check = %+v, want nothing: there is no install to disagree", got)
			}
		})
	}
}

func TestCheckReadsNothingOutsideTheTree(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	dir := filepath.Join(parent, "tree")
	nodeModules(t, dir)
	// A package.json outside the tree, which a lockfile path escaping it
	// would reach.
	if err := os.MkdirAll(filepath.Join(parent, "secret"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "secret", "package.json"), []byte(`{"version":"0.0.1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeLock(t, dir, `{
	  "lockfileVersion": 3,
	  "packages": {
	    "node_modules/../../../secret": {"version": "1.0.0"}
	  }
	}`)

	got := check(t, dir)
	if len(got.Mismatches) != 0 || len(got.NotCompared) != 0 {
		t.Errorf("Check = %+v, want nothing: a path leaving the tree is not an installed package", got)
	}
}

func TestCheckErrorsOnALockfileItCannotParse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	nodeModules(t, dir)
	writeLock(t, dir, `{"packages": `)

	got, err := Check(dir)
	if err == nil {
		t.Fatalf("Check = %+v, nil; a lockfile that cannot be parsed is a comparison that did not happen", got)
	}
}
