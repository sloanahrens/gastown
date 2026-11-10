package townstate

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// failingFS is a town root whose metadata read fails at named paths.
type failingFS struct {
	fs.FS
	readDirErr map[string]error
	statErr    map[string]error
	openErr    map[string]error
}

func (f failingFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if err, ok := f.readDirErr[name]; ok {
		return nil, err
	}
	return fs.ReadDir(f.FS, name)
}

func (f failingFS) Stat(name string) (fs.FileInfo, error) {
	if err, ok := f.statErr[name]; ok {
		return nil, err
	}
	return fs.Stat(f.FS, name)
}

func (f failingFS) Open(name string) (fs.File, error) {
	if err, ok := f.openErr[name]; ok {
		return nil, err
	}
	return f.FS.Open(name)
}

// registryTown is a town whose registry names hm, a rig with no config.json and
// no other marker: only the registry makes it a rig.
func registryTown() fstest.MapFS {
	return fstest.MapFS{
		"mayor/town.json":              {Data: []byte(`{"registry":{"version":1,"rigs":{"hm":{"git_url":"file:///srv/hm"}}}}`)},
		"hm/crew/eagle/notes.md":       {Data: []byte("x")},
		"hm/polecats/rust/worktree.go": {Data: []byte("x")},
	}
}

// rigTreeWithMarker is a rig the registry does not name, with no config.json.
// Its bare repository and its canonical clone mark it, and its crew and
// polecats are covered by the rig's own rows.
func rigTreeWithMarker() fstest.MapFS {
	return fstest.MapFS{
		"mayor/town.json":        {Data: []byte("{}")},
		"tango/.repo.git/HEAD":   {Data: []byte("ref: refs/heads/main\n")},
		"tango/mayor/rig/go.mod": {Data: []byte("x")},
		"tango/crew/echo/a.md":   {Data: []byte("x")},
		"tango/polecats/bo/b.go": {Data: []byte("x")},
	}
}

// unclassifiedTown holds one directory no entry names, with two hundred
// descendants.
func unclassifiedTown() fstest.MapFS {
	town := fstest.MapFS{"mayor/town.json": {Data: []byte("{}")}}
	for i := range 200 {
		town[fmt.Sprintf("stray/deep/f%03d.txt", i)] = &fstest.MapFile{Data: []byte("x")}
	}
	return town
}

// sessionTown is a small town root with a rig, a bird directory and the
// runtime subsystems a verify walk reaches.
func sessionTown() fstest.MapFS {
	return fstest.MapFS{
		"mayor/town.json":            {Data: []byte("{}")},
		"gastown/config.json":        {Data: []byte("{}")},
		"gastown/crew/eagle/a.txt":   {Data: []byte("x")},
		".runtime/jev/run.json":      {Data: []byte("{}")},
		".runtime/wren/reports.json": {Data: []byte("{}")},
	}
}

// withUsageUnderBird is sessionTown with a daemon subsystem directory under a
// bird's tree, the path verify stats when it checks recent writes.
func withUsageUnderBird() fstest.MapFS {
	town := sessionTown()
	town[".runtime/wren/usage/run.json"] = &fstest.MapFile{Data: []byte("{}")}
	return town
}

// TestVerifyRefusesWhatItCannotRead drives every read failure verify can meet:
// an unreadable root, an unreadable .runtime, a walk that fails inside a
// directory, a stat that fails on a session path, and a manifest it cannot
// validate.
func TestVerifyRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	now := time.Now()
	boom := errors.New("boom")

	cases := []struct {
		name string
		fsys fs.FS
		m    *Manifest
		want string
	}{
		{
			name: "nil manifest",
			fsys: sessionTown(),
			m:    nil,
			want: "nil manifest",
		},
		{
			name: "invalid manifest",
			fsys: sessionTown(),
			m: &Manifest{Entries: []Entry{
				{Path: "", Owner: OwnerDaemon, Why: "no path"},
			}},
			want: "empty path",
		},
		{
			name: "town root unreadable",
			fsys: failingFS{FS: sessionTown(), readDirErr: map[string]error{".": boom}},
			m:    Default(),
			want: "reading the town root",
		},
		{
			name: "runtime unreadable",
			fsys: failingFS{FS: sessionTown(), readDirErr: map[string]error{".runtime": boom}},
			m:    Default(),
			want: "reading .runtime",
		},
		{
			name: "walk fails inside a directory",
			fsys: failingFS{FS: sessionTown(), readDirErr: map[string]error{"mayor": boom}},
			m:    Default(),
			want: "walking the town root",
		},
		{
			name: "stat fails on a daemon-named path under a session entry",
			fsys: failingFS{FS: withUsageUnderBird(), statErr: map[string]error{".runtime/wren/usage": boom}},
			m:    Default(),
			want: "stat .runtime/wren/usage",
		},
		{
			name: "registry section unreadable",
			fsys: failingFS{FS: registryTown(), openErr: map[string]error{"mayor/town.json": boom}},
			m:    Default(),
			want: "reading mayor/town.json",
		},
		{
			name: "registry file unreadable on the five-file layout",
			fsys: failingFS{FS: sessionTown(), openErr: map[string]error{"mayor/rigs.json": boom}},
			m:    Default(),
			want: "reading mayor/rigs.json",
		},
		{
			name: "machine file is not a JSON object",
			fsys: fstest.MapFS{"mayor/town.json": {Data: []byte("{")}},
			m:    Default(),
			want: "reading mayor/town.json",
		},
		{
			name: "registry section holds a key the registry does not know",
			fsys: fstest.MapFS{"mayor/town.json": {Data: []byte(`{"registry":{"version":1,"rigs":{"hm":{"git_url":"x","surprise":true}}}}`)}},
			m:    Default(),
			want: "surprise",
		},
		{
			name: "five-file registry holds a key the registry does not know",
			fsys: fstest.MapFS{"mayor/rigs.json": {Data: []byte(`{"version":1,"rigs":{"hm":{"git_url":"x","surprise":true}}}`)}},
			m:    Default(),
			want: "surprise",
		},
		{
			name: "rig marker unreadable",
			fsys: failingFS{FS: sessionTown(), statErr: map[string]error{"gastown/config.json": boom}},
			m:    Default(),
			want: "stat gastown/config.json",
		},
		{
			name: "rig marker bare repository unreadable",
			fsys: failingFS{FS: rigTreeWithMarker(), statErr: map[string]error{"tango/.repo.git": boom}},
			m:    Default(),
			want: "stat tango/.repo.git",
		},
		{
			name: "rig marker canonical clone unreadable",
			fsys: failingFS{
				FS: fstest.MapFS{
					"mayor/town.json":        {Data: []byte("{}")},
					"tango/mayor/rig/go.mod": {Data: []byte("x")},
				},
				statErr: map[string]error{"tango/mayor/rig": boom},
			},
			m:    Default(),
			want: "stat tango/mayor/rig",
		},
		{
			name: "machine file is null",
			fsys: fstest.MapFS{"mayor/town.json": {Data: []byte("null")}},
			m:    Default(),
			want: "not a JSON object",
		},
		{
			name: "registry section is null beside a five-file registry",
			fsys: fstest.MapFS{
				"mayor/town.json": {Data: []byte(`{"registry":null}`)},
				"mayor/rigs.json": {Data: []byte(`{"version":1,"rigs":{"hm":{"git_url":"x"}}}`)},
			},
			m:    Default(),
			want: `section "registry" is null`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Verify(tc.fsys, tc.m, now)
			if err == nil {
				t.Fatalf("Verify accepted an unreadable input")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Verify error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestVerifyNamesEveryRig drives a rig the town has but no config.json marks:
// one the registry names, and one a bare repository marks. Their manifest rows
// expand and their trees are classified, so verify reports nothing for them.
func TestVerifyNamesEveryRig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		fsys fs.FS
	}{
		{name: "rig named by the registry, no config.json", fsys: registryTown()},
		{name: "rig marked by a bare repository, not in the registry", fsys: rigTreeWithMarker()},
		{name: "rig named only by mayor/rigs.json on the five-file layout", fsys: fstest.MapFS{
			"mayor/town.json":              {Data: []byte("{}")},
			"mayor/rigs.json":              {Data: []byte(`{"version":1,"rigs":{"hm":{"git_url":"file:///srv/hm"}}}`)},
			"hm/crew/eagle/notes.md":       {Data: []byte("x")},
			"hm/polecats/rust/worktree.go": {Data: []byte("x")},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			problems, err := Verify(tc.fsys, Default(), time.Now())
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if len(problems) != 0 {
				t.Errorf("Verify reported a rig's own tree: %v", problems)
			}
		})
	}
}

// TestVerifyNamesARigByItsOnlyMarker drives a rig that one marker alone makes a
// rig: a bare repository, or a canonical clone. Neither has a config.json or a
// registry record, so the marker is the only thing that names the rig, and its
// crew and polecats trees fall under the rig's own rows.
func TestVerifyNamesARigByItsOnlyMarker(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		fsys fs.FS
	}{
		{name: ".repo.git only", fsys: fstest.MapFS{
			"mayor/town.json":        {Data: []byte("{}")},
			"tango/.repo.git/HEAD":   {Data: []byte("ref: refs/heads/main\n")},
			"tango/crew/echo/a.md":   {Data: []byte("x")},
			"tango/polecats/bo/b.go": {Data: []byte("x")},
		}},
		{name: "mayor/rig only", fsys: fstest.MapFS{
			"mayor/town.json":        {Data: []byte("{}")},
			"tango/mayor/rig/go.mod": {Data: []byte("x")},
			"tango/crew/echo/a.md":   {Data: []byte("x")},
			"tango/polecats/bo/b.go": {Data: []byte("x")},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			problems, err := Verify(tc.fsys, Default(), time.Now())
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if len(problems) != 0 {
				t.Errorf("Verify reported a rig named by its only marker: %v", problems)
			}
		})
	}
}

// TestVerifyDoesNotNameAManifestDirectoryAsARig: settings/ is a top-level row
// the manifest names itself, and it holds a config.json. A directory the
// manifest names is never a rig, so no rig row expands under it. Were it a rig,
// the rig rows would put daemon-owned paths under the shared settings row, and
// verify would report those overlaps.
func TestVerifyDoesNotNameAManifestDirectoryAsARig(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"mayor/town.json":                 {Data: []byte("{}")},
		"settings/config.json":            {Data: []byte("{}")},
		"settings/plugins/tool/plugin.md": {Data: []byte("x")},
	}
	problems, err := Verify(fsys, Default(), time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(problems) != 0 {
		t.Errorf("Verify reported settings/, a directory the manifest names itself: %v", problems)
	}
}

// TestVerifyKeepsConfigJSONAsARigMarker holds verify's classification apart
// from the copy's: a directory that holds only a config.json is still a rig to
// verify, so its session trees are classified, while the rig list export and
// import take from the settings (settingsRigs) names no rig here.
func TestVerifyKeepsConfigJSONAsARigMarker(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"mayor/town.json":          {Data: []byte("{}")},
		"gastown/config.json":      {Data: []byte("{}")},
		"gastown/crew/eagle/a.txt": {Data: []byte("x")},
	}
	problems, err := Verify(fsys, Default(), time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(problems) != 0 {
		t.Errorf("Verify reported a rig marked only by its config.json: %v", problems)
	}
	rigs, err := settingsRigs(fsys)
	if err != nil {
		t.Fatalf("settingsRigs: %v", err)
	}
	if len(rigs) != 0 {
		t.Errorf("settingsRigs named %v, want no rig: the registry is the copy's only source", rigs)
	}
}

// TestVerifyClassifiesTheArchiveAndTimingPaths: the history archive's files and
// the shell timing log have their rows, so verify reports neither; a directory
// that only shares the archive's name prefix has no row and is still reported.
func TestVerifyClassifiesTheArchiveAndTimingPaths(t *testing.T) {
	t.Parallel()
	town := fstest.MapFS{
		"mayor/town.json": {Data: []byte("{}")},
		".runtime/dolt-archive/dolt-log-x-20261108.csv.gz": {Data: []byte("x")},
		".runtime/tiertiming/shell.jsonl":                  {Data: []byte("{}\n")},
		".runtime/dolt-archive-stray/a":                    {Data: []byte("x")},
	}
	problems, err := Verify(town, Default(), time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	want := []Problem{{
		Path:   ".runtime/dolt-archive-stray",
		Reason: "no manifest entry; classify it daemon, session or shared",
	}}
	if !slices.Equal(problems, want) {
		t.Errorf("Verify reported %v, want exactly %v", problems, want)
	}
}

// TestVerifyReportsAnUnclassifiedDirectoryOnce: a directory no entry names is
// one problem at its own path, not one per descendant, so a wrong rule shows as
// one line.
func TestVerifyReportsAnUnclassifiedDirectoryOnce(t *testing.T) {
	t.Parallel()
	problems, err := Verify(unclassifiedTown(), Default(), time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	var paths []string
	for _, p := range problems {
		paths = append(paths, p.Path)
	}
	if want := []string{"stray"}; !slices.Equal(paths, want) {
		t.Errorf("Verify reported %v, want exactly %v", paths, want)
	}
}

// TestVerifyRefusesAHoldOrEstopNoRowCovers holds the shared rows to their names:
// the hold and the ESTOP files the rows cover pass, and a file that only shares
// part of one of those names is reported with the existing no-entry refusal.
func TestVerifyRefusesAHoldOrEstopNoRowCovers(t *testing.T) {
	t.Parallel()
	town := registryTown()
	for _, p := range []string{"seat-refill.hold", "ESTOP", "ESTOP.hm"} {
		town[p] = &fstest.MapFile{Data: []byte("x")}
	}
	for _, p := range []string{"seat-refill.hold.bak", "ESTOP.hm.bak", "ESTOP-old"} {
		town[p] = &fstest.MapFile{Data: []byte("x")}
	}
	problems, err := Verify(town, Default(), time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	var paths []string
	for _, p := range problems {
		paths = append(paths, p.Path)
		if !strings.Contains(p.Reason, "no manifest entry") {
			t.Errorf("Verify reported %s as %q, want the no-manifest-entry refusal", p.Path, p.Reason)
		}
	}
	if want := []string{"ESTOP-old", "ESTOP.hm.bak", "seat-refill.hold.bak"}; !slices.Equal(paths, want) {
		t.Errorf("Verify reported %v, want exactly %v", paths, want)
	}
}

// TestVerifyReportsEverySiblingFile: gastown/.runtime is a container, since rig
// rows lie below it, and it holds three files no row names. Each is its own
// problem. An unclassified directory is reported once and never entered, so
// these files are reached only through the container; a SkipDir returned for
// the first file would skip the other two.
func TestVerifyReportsEverySiblingFile(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"mayor/town.json":          {Data: []byte(`{"registry":{"version":1,"rigs":{"gastown":{"git_url":"file:///srv/gastown"}}}}`)},
		"gastown/.runtime/stray-a": {Data: []byte("x")},
		"gastown/.runtime/stray-b": {Data: []byte("x")},
		"gastown/.runtime/stray-c": {Data: []byte("x")},
	}
	problems, err := Verify(fsys, Default(), time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	var paths []string
	for _, p := range problems {
		paths = append(paths, p.Path)
	}
	want := []string{"gastown/.runtime/stray-a", "gastown/.runtime/stray-b", "gastown/.runtime/stray-c"}
	if !slices.Equal(paths, want) {
		t.Errorf("Verify reported %v, want exactly %v", paths, want)
	}
}

// rigSubsystemRigs are the two rigs the rig-row test expands.
var rigSubsystemRigs = []string{"gastown", "beads"}

// rigSubsystemTown is a town whose registry names two rigs, each holding the
// rig-level paths the manifest gives an owner: the beads workspace, plugins,
// settings, directives, harness scripts and manifest, and a name-pool state
// file under .runtime. One rig also holds a file under .runtime that no row
// names, so the walk still reports a new writer there once.
func rigSubsystemTown() fstest.MapFS {
	town := fstest.MapFS{
		"mayor/town.json": {Data: []byte(`{"registry":{"version":1,"rigs":{"gastown":{"git_url":"file:///srv/gastown"},"beads":{"git_url":"file:///srv/beads"}}}}`)},
	}
	for _, rig := range rigSubsystemRigs {
		for _, p := range []string{
			".beads/redirect",
			"plugins/tool/plugin.md",
			"settings/config.json",
			"directives/rig.md",
			"scripts/om-gate.sh",
			".gastown-harness-manifest.json",
			".runtime/namepool-state.json",
		} {
			town[rig+"/"+p] = &fstest.MapFile{Data: []byte("x")}
		}
	}
	town["gastown/.runtime/some-unlisted-file"] = &fstest.MapFile{Data: []byte("x")}
	return town
}

// TestVerifyClassifiesRigSubsystemPaths expands the rig rows against a town of
// two rigs: each rig-level path the manifest classes has its owner, and a file
// under a rig's .runtime that no row names is still reported, once, rather than
// hidden by a parent row for <rig>/.runtime.
func TestVerifyClassifiesRigSubsystemPaths(t *testing.T) {
	t.Parallel()
	m := Default()
	entries := m.Expand(rigSubsystemRigs, nil)
	owners := []struct {
		path  string
		owner Owner
	}{
		{path: ".beads", owner: OwnerDaemon},
		{path: "plugins", owner: OwnerDaemon},
		{path: "settings", owner: OwnerShared},
		{path: "directives", owner: OwnerShared},
		{path: "scripts", owner: OwnerShared},
		{path: ".gastown-harness-manifest.json", owner: OwnerShared},
		{path: ".runtime/namepool-state.json", owner: OwnerDaemon},
		{path: ".runtime/session_id", owner: OwnerSession},
		{path: ".runtime/agent.lock", owner: OwnerSession},
	}
	for _, rig := range rigSubsystemRigs {
		for _, o := range owners {
			p := rig + "/" + o.path
			e, ok := Lookup(entries, p)
			if !ok {
				t.Errorf("no entry covers %s", p)
				continue
			}
			if e.Owner != o.owner {
				t.Errorf("%s is classed %q, want %q (row %q)", p, e.Owner, o.owner, e.Path)
			}
		}
	}

	problems, err := Verify(rigSubsystemTown(), m, time.Now())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	want := []Problem{{
		Path:   "gastown/.runtime/some-unlisted-file",
		Reason: "no manifest entry; classify it daemon, session or shared",
	}}
	if !slices.Equal(problems, want) {
		t.Errorf("Verify reported %v, want exactly %v", problems, want)
	}
}
