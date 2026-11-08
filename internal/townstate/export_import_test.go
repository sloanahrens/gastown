package townstate

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// writeStateFile makes dirs and a file under root and returns its path.
func writeStateFile(t *testing.T, root, rel, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// archiveEntry is one entry read back from a tar stream.
type archiveEntry struct {
	hdr  *tar.Header
	data []byte
}

// readArchive reads every entry of a tar stream a test produced.
func readArchive(t *testing.T, data []byte) []archiveEntry {
	t.Helper()
	var out []archiveEntry
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("reading the archive: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading the entry %q: %v", hdr.Name, err)
		}
		out = append(out, archiveEntry{hdr: hdr, data: body})
	}
}

// archiveNames returns the entry names of a tar stream.
func archiveNames(t *testing.T, data []byte) []string {
	t.Helper()
	var names []string
	for _, e := range readArchive(t, data) {
		names = append(names, e.hdr.Name)
	}
	return names
}

// writeArchive writes entries to a tar stream in memory.
func writeArchive(t *testing.T, entries ...archiveEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		if err := tw.WriteHeader(e.hdr); err != nil {
			t.Fatalf("writing the header for %q: %v", e.hdr.Name, err)
		}
		if len(e.data) > 0 {
			if _, err := tw.Write(e.data); err != nil {
				t.Fatalf("writing the entry %q: %v", e.hdr.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// regularEntry is a regular file entry for a hand-built archive.
func regularEntry(name, content string) archiveEntry {
	return archiveEntry{
		hdr:  &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content)), ModTime: time.Unix(0, 0)},
		data: []byte(content),
	}
}

// TestExportImportRoundTrip drives Export and Import over one town: the
// daemon-owned set comes back byte for byte, session-owned paths and a bird's
// directory never enter the archive, and the skipped classes are named in the
// summary.
func TestExportImportRoundTrip(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	old := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

	// The daemon-owned set. Written through the same manifest rows the export
	// walks, so a change to a row shows up as a missing or extra file.
	daemonFiles := map[string]string{
		"mayor/town.json":                    `{"registry":{"version":1,"rigs":{"gastown":{}}}}`,
		"daemon/maintenance_state.json":      `{"db":"main"}`,
		"logs/daemon.log":                    "daemon log\n",
		"events/2026-11-06.jsonl":            "{\"type\":\"event\"}\n",
		".events.jsonl":                      "{\"type\":\"town\"}\n",
		".feed.jsonl":                        "{\"type\":\"feed\"}\n",
		".beads/metadata.json":               `{"database":"gastown"}`,
		"plugins/p/plugin.toml":              "name = \"p\"\n",
		".claude-town/session1.json":         `{"session":"1"}`,
		"gastown/config.json":                `{"name":"gastown"}`,
		"gastown/.repo.git/HEAD":             "ref: refs/heads/main\n",
		"gastown/mayor/rig/README.md":        "the canonical clone\n",
		".runtime/jev/run.json":              `{"run":1}`,
		".runtime/landings/landing1.json":    `{"landing":1}`,
		".runtime/container/kubeconfig.yaml": "apiVersion: v1\n",
	}
	for rel, content := range daemonFiles {
		mode := os.FileMode(0o644)
		if strings.HasSuffix(rel, ".log") {
			mode = 0o600 // a mode the copy masks to 0644
		}
		if strings.HasSuffix(rel, "kubeconfig.yaml") {
			mode = 0o600 // private: the mode is kept
		}
		writeStateFile(t, town, rel, content, mode)
	}
	if err := os.Chtimes(filepath.Join(town, "logs", "daemon.log"), old, old); err != nil {
		t.Fatal(err)
	}

	// The skipped classes, each inside a daemon-owned row.
	writeStateFile(t, town, "gastown/mayor/rig/.git/HEAD", "ref: refs/heads/main\n", 0o644)
	writeStateFile(t, town, ".beads/.dolt-data/config.yaml", "listener: 127.0.0.1\n", 0o644)
	writeStateFile(t, town, "daemon/state.lock", "", 0o644)
	if err := syscall.Mkfifo(filepath.Join(town, "logs", "pipe"), 0o644); err != nil {
		t.Fatalf("making the fifo: %v", err)
	}
	//testpolicy:allow no-network — a unix socket at rest is the entry the export must skip, and nothing dials or accepts it
	ln, err := net.Listen("unix", filepath.Join(town, "logs", "daemon.sock"))
	if err != nil {
		t.Fatalf("making the socket: %v", err)
	}
	defer ln.Close()

	// The session-owned and shared set: never in the archive.
	untouched := map[string]string{
		"settings/config.json":             `{"edited":"by the overseer"}`,
		"gastown/crew/eagle/notes.md":      "crew work\n",
		"gastown/polecats/toast/work.md":   "polecat work\n",
		".runtime/wren/notes.txt":          "a bird's own notes\n",
		".runtime/wren/reports/index.html": "<html></html>\n",
		"gastown/witness/state.json":       `{"w":"1"}`,
		"gastown/refinery/state.json":      `{"r":"1"}`,
	}
	for rel, content := range untouched {
		writeStateFile(t, town, rel, content, 0o644)
	}
	if err := os.Chtimes(filepath.Join(town, "logs"), old, old); err != nil {
		t.Fatal(err)
	}

	var stream bytes.Buffer
	sum, err := Export(&stream, town, Default(), ExportOptions{})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	for _, class := range []SkipClass{SkipDoltData, SkipGitTree, SkipSocket, SkipLockFile, SkipNotRegular} {
		if sum.Skipped[class] != 1 {
			t.Errorf("summary skipped %d %s, want 1 (all skipped: %v)", sum.Skipped[class], class, sum.Skipped)
		}
	}
	printed := sum.String()
	for _, want := range []string{"Dolt data", "git trees", "sockets", "lock files", "the rig clones are re-fetched from Forgejo"} {
		if !strings.Contains(printed, want) {
			t.Errorf("summary does not name %q:\n%s", want, printed)
		}
	}
	for _, name := range []string{"settings/config.json", "gastown/crew/eagle/notes.md", ".runtime/wren/notes.txt", "gastown/.git/HEAD", ".beads/.dolt-data/config.yaml", "logs/pipe", "logs/daemon.sock", "daemon/state.lock"} {
		for _, got := range archiveNames(t, stream.Bytes()) {
			if got == name || strings.HasPrefix(got, name+"/") {
				t.Errorf("the archive carries %q, which the export must leave out", got)
			}
		}
	}
	for _, name := range archiveNames(t, stream.Bytes()) {
		switch {
		case strings.Contains(name, "/.git/"):
			t.Errorf("the archive carries the git tree %q", name)
		case strings.Contains(name, "/.dolt-data/"):
			t.Errorf("the archive carries the Dolt data %q", name)
		}
	}

	dest := t.TempDir()
	isum, err := Import(bytes.NewReader(stream.Bytes()), dest, Default(), ImportOptions{})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if isum.Files != len(daemonFiles) {
		t.Errorf("import wrote %d files, want %d (%s)", isum.Files, len(daemonFiles), isum)
	}
	for rel, want := range daemonFiles {
		p := filepath.Join(dest, filepath.FromSlash(rel))
		got, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("the round trip did not restore %q: %v", rel, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%q = %q after the round trip, want %q", rel, got, want)
		}
	}
	for rel := range untouched {
		if _, err := os.Lstat(filepath.Join(dest, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("the import wrote the session-owned %q (err %v)", rel, err)
		}
	}

	// Modes: the ordinary file is masked to 0644, the private row keeps 0600.
	if info, err := os.Stat(filepath.Join(dest, "logs", "daemon.log")); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("logs/daemon.log mode = %04o after the round trip, want 0644", got)
	}
	if info, err := os.Stat(filepath.Join(dest, ".runtime", "container", "kubeconfig.yaml")); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("the private kubeconfig mode = %04o after the round trip, want 0600", got)
	}

	// The mtime comes from the archive, for a file and for the directory that
	// holds it: writing the file into the directory would move the directory's
	// mtime if the import set it before writing.
	if info, err := os.Stat(filepath.Join(dest, "logs", "daemon.log")); err != nil {
		t.Fatal(err)
	} else if !info.ModTime().Equal(old) {
		t.Errorf("logs/daemon.log mtime = %s after the round trip, want %s", info.ModTime(), old)
	}
	if info, err := os.Stat(filepath.Join(dest, "logs")); err != nil {
		t.Fatal(err)
	} else if !info.ModTime().Equal(old) {
		t.Errorf("logs mtime = %s after the round trip, want %s", info.ModTime(), old)
	}
}

// TestImportRefusesUnsafeEntries drives every refusal an untrusted archive
// needs: a name that is absolute or climbs out, a symlink that escapes, a
// special file, a path outside the daemon-owned set, a symlink in the path,
// and a destination that already holds the set. Each names the entry and
// writes nothing.
func TestImportRefusesUnsafeEntries(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	cases := []struct {
		name      string
		entries   []archiveEntry
		merge     bool
		prepare   func(t *testing.T, dest string)
		want      []string
		wantFiles []string
	}{
		{
			name:    "an absolute name",
			entries: []archiveEntry{regularEntry("/absolute/escape", "x")},
			want:    []string{"/absolute/escape", "absolute"},
		},
		{
			name:    "a parent segment",
			entries: []archiveEntry{regularEntry("daemon/../../escape", "x")},
			want:    []string{`"daemon/../../escape"`, "contains .."},
		},
		{
			name: "a symlink to an absolute path",
			entries: []archiveEntry{{hdr: &tar.Header{
				Name: "daemon/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777, ModTime: time.Unix(0, 0),
			}}},
			want: []string{`"daemon/link"`, "absolute"},
		},
		{
			name: "a symlink leaving the town root",
			entries: []archiveEntry{{hdr: &tar.Header{
				Name: "daemon/link", Typeflag: tar.TypeSymlink, Linkname: "../../../escape", Mode: 0o777, ModTime: time.Unix(0, 0),
			}}},
			want: []string{`"daemon/link"`, "outside the town root"},
		},
		{
			name: "a fifo",
			entries: []archiveEntry{{hdr: &tar.Header{
				Name: "daemon/pipe", Typeflag: tar.TypeFifo, Mode: 0o644, ModTime: time.Unix(0, 0),
			}}},
			want: []string{`"daemon/pipe"`, "fifo"},
		},
		{
			name: "a device",
			entries: []archiveEntry{{hdr: &tar.Header{
				Name: "daemon/dev", Typeflag: tar.TypeChar, Mode: 0o644, ModTime: time.Unix(0, 0),
			}}},
			want: []string{`"daemon/dev"`, "character device"},
		},
		{
			name:    "a path outside the daemon-owned set",
			entries: []archiveEntry{regularEntry("settings/config.json", "x")},
			want:    []string{`"settings/config.json"`, "daemon-owned"},
		},
		{
			name:    "a destination that already holds the daemon-owned set",
			entries: []archiveEntry{regularEntry("daemon/maintenance_state.json", "new")},
			prepare: func(t *testing.T, dest string) {
				writeStateFile(t, dest, "daemon/maintenance_state.json", "old", 0o644)
			},
			want:      []string{`"daemon"`, "empty"},
			wantFiles: []string{"daemon/maintenance_state.json"},
		},
		{
			name:    "a path through an existing symlink",
			entries: []archiveEntry{regularEntry("daemon/x", "x")},
			merge:   true,
			prepare: func(t *testing.T, dest string) {
				if err := os.Symlink(outside, filepath.Join(dest, "daemon")); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{`"daemon/x"`, "symlink"},
		},
		{
			name: "a target through a link that points at an absolute path",
			entries: []archiveEntry{{hdr: &tar.Header{
				Name: "daemon/b", Typeflag: tar.TypeSymlink, Linkname: "abs", Mode: 0o777, ModTime: time.Unix(0, 0),
			}}},
			merge: true,
			prepare: func(t *testing.T, dest string) {
				if err := os.MkdirAll(filepath.Join(dest, "daemon"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/etc", filepath.Join(dest, "daemon", "abs")); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{`"daemon/b"`, "absolute"},
		},
		{
			name:    "a file over an existing directory",
			entries: []archiveEntry{regularEntry("daemon/x", "new")},
			merge:   true,
			prepare: func(t *testing.T, dest string) {
				if err := os.MkdirAll(filepath.Join(dest, "daemon", "x"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{`"daemon/x"`, "a directory", "one kind with another"},
		},
		{
			name: "a symlink over an existing file",
			entries: []archiveEntry{{hdr: &tar.Header{
				Name: "daemon/link", Typeflag: tar.TypeSymlink, Linkname: "inner", Mode: 0o777, ModTime: time.Unix(0, 0),
			}}},
			merge:     true,
			prepare:   func(t *testing.T, dest string) { writeStateFile(t, dest, "daemon/link", "x", 0o644) },
			want:      []string{`"daemon/link"`, "a regular file", "one kind with another"},
			wantFiles: []string{"daemon/link"},
		},
		{
			name: "a directory over an existing file",
			entries: []archiveEntry{{hdr: &tar.Header{
				Name: ".runtime/jev/sub/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Unix(0, 0),
			}}},
			merge:     true,
			prepare:   func(t *testing.T, dest string) { writeStateFile(t, dest, ".runtime/jev/sub", "x", 0o644) },
			want:      []string{`".runtime/jev/sub"`, "a regular file", "one kind with another"},
			wantFiles: []string{".runtime/jev/sub"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dest := t.TempDir()
			if tc.prepare != nil {
				tc.prepare(t, dest)
			}
			_, err := Import(bytes.NewReader(writeArchive(t, tc.entries...)), dest, Default(), ImportOptions{MergeNewer: tc.merge})
			if err == nil {
				t.Fatalf("Import accepted %s", tc.name)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Import error = %q, want it to hold %q", err, want)
				}
			}
			if got := regularFilesIn(t, dest); !slices.Equal(got, tc.wantFiles) {
				t.Errorf("the refused import left files %v in the destination, want %v", got, tc.wantFiles)
			}
			if entries, err := os.ReadDir(outside); err == nil && len(entries) > 0 {
				t.Errorf("the import wrote through the symlink: %v", entries)
			}
		})
	}
}

// TestImportRefusesSymlinksCreatedEarlierInTheArchive drives the links an
// archive makes earlier in the same stream: a name written through a link, a
// chain of links, and a target that climbs out through a link. Each is refused
// naming the entry, because the check follows the links as the tree stands.
func TestImportRefusesSymlinksCreatedEarlierInTheArchive(t *testing.T) {
	t.Parallel()
	dir := func(name string) archiveEntry {
		return archiveEntry{hdr: &tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Unix(0, 0)}}
	}
	link := func(name, target string) archiveEntry {
		return archiveEntry{hdr: &tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777, ModTime: time.Unix(0, 0)}}
	}
	// A chain of links, each dangling when it is written, then one that starts
	// the walk at the head: the walk meets more links than the bound allows.
	var chain []archiveEntry
	for i := range 42 {
		chain = append(chain, link(fmt.Sprintf("daemon/c%02d", i), fmt.Sprintf("c%02d", i+1)))
	}
	chain = append(chain, link("daemon/head", "c00"))
	cases := []struct {
		name    string
		entries []archiveEntry
		want    []string
	}{
		{
			name:    "a chain longer than the link bound",
			entries: chain,
			want:    []string{`"daemon/head"`, "passes more than 40 symlinks"},
		},
		{
			name:    "a file written through a link made earlier",
			entries: []archiveEntry{dir("daemon/ok/"), link("daemon/a", "ok"), regularEntry("daemon/a/x", "x")},
			want:    []string{`"daemon/a/x"`, "symlink"},
		},
		{
			name:    "a parent step through a link made earlier",
			entries: []archiveEntry{dir("daemon/ok/"), link("daemon/a", "ok"), regularEntry("daemon/a/../y", "x")},
			want:    []string{`"daemon/a/../y"`, "contains .."},
		},
		{
			name:    "a chain of links whose last one climbs to the root",
			entries: []archiveEntry{link("daemon/l1", "l2"), link("daemon/l2", ".."), regularEntry("daemon/l1/x", "x")},
			want:    []string{`"daemon/l2"`, "not under a daemon-owned path"},
		},
		{
			name: "a target that climbs out through a link made earlier",
			entries: []archiveEntry{
				dir("daemon/s/"),
				link("daemon/s/A", ".."),
				link("daemon/s/B", "A/../../x"),
			},
			want: []string{`"daemon/s/B"`, "outside the town root"},
		},
		{
			name:    "a target through a name that does not exist yet, then a climb",
			entries: []archiveEntry{link("daemon/l", "missing/..")},
			want:    []string{`"daemon/l"`, "does not exist yet"},
		},
		{
			name:    "a target that lands in a directory no daemon row names",
			entries: []archiveEntry{link("daemon/q", "../settings/config.json")},
			want:    []string{`"daemon/q"`, "not under a daemon-owned path"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Import(bytes.NewReader(writeArchive(t, tc.entries...)), t.TempDir(), Default(), ImportOptions{})
			if err == nil {
				t.Fatalf("Import accepted %s", tc.name)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Import error = %q, want it to hold %q", err, want)
				}
			}
		})
	}
}

// TestImportKeepsDirectoryModes restores a directory's permission bits as the
// archive carries them: a 0700 and a 0755 directory keep theirs, while the
// files beside them are masked to the town's two file modes.
func TestImportKeepsDirectoryModes(t *testing.T) {
	t.Parallel()
	dest := t.TempDir()
	at := time.Unix(0, 0)
	archive := writeArchive(t,
		archiveEntry{hdr: &tar.Header{Name: "daemon/private/", Typeflag: tar.TypeDir, Mode: 0o700, ModTime: at}},
		archiveEntry{hdr: &tar.Header{Name: "daemon/public/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: at}},
		archiveEntry{hdr: &tar.Header{Name: "daemon/private/secret", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1, ModTime: at}, data: []byte("x")},
		archiveEntry{hdr: &tar.Header{Name: "daemon/public/tool", Typeflag: tar.TypeReg, Mode: 0o751, Size: 1, ModTime: at}, data: []byte("x")},
	)
	if _, err := Import(bytes.NewReader(archive), dest, Default(), ImportOptions{}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	want := map[string]os.FileMode{
		"daemon/private":        0o700,
		"daemon/public":         0o755,
		"daemon/private/secret": 0o644,
		"daemon/public/tool":    0o755,
	}
	for rel, mode := range want {
		info, err := os.Stat(filepath.Join(dest, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("stat %s: %v", rel, err)
		}
		if got := info.Mode().Perm(); got != mode {
			t.Errorf("%s mode = %04o after the import, want %04o", rel, got, mode)
		}
	}
}

// TestImportSummaryBreaksDownByClass holds the import summary to one line per
// entry class, both for a fresh root and for a merge that keeps a newer file.
func TestImportSummaryBreaksDownByClass(t *testing.T) {
	t.Parallel()
	at := time.Unix(0, 0)
	archive := func() []byte {
		return writeArchive(t,
			archiveEntry{hdr: &tar.Header{Name: "daemon/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: at}},
			regularEntry("daemon/a.txt", "abc"),
			archiveEntry{hdr: &tar.Header{Name: "daemon/sub/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: at}},
			regularEntry("daemon/sub/b.txt", "de"),
			archiveEntry{hdr: &tar.Header{Name: "daemon/link", Typeflag: tar.TypeSymlink, Linkname: "a.txt", Mode: 0o777, ModTime: at}},
		)
	}
	t.Run("a fresh root", func(t *testing.T) {
		t.Parallel()
		sum, err := Import(bytes.NewReader(archive()), t.TempDir(), Default(), ImportOptions{})
		if err != nil {
			t.Fatalf("Import: %v", err)
		}
		for _, want := range []string{"files: 2 (5 bytes)", "directories: 2", "symlinks: 1", "skipped: none", "entries: daemon"} {
			if !strings.Contains(sum.String(), want) {
				t.Errorf("summary does not carry the line %q:\n%s", want, sum)
			}
		}
	})
	t.Run("a merge that keeps a newer file", func(t *testing.T) {
		t.Parallel()
		dest := t.TempDir()
		writeStateFile(t, dest, "daemon/a.txt", "newer", 0o644)
		sum, err := Import(bytes.NewReader(archive()), dest, Default(), ImportOptions{MergeNewer: true})
		if err != nil {
			t.Fatalf("Import: %v", err)
		}
		for _, want := range []string{"files: 1 (2 bytes)", "skipped: 1 existing files newer than the archive"} {
			if !strings.Contains(sum.String(), want) {
				t.Errorf("summary does not carry the line %q:\n%s", want, sum)
			}
		}
	})
}

// TestRigsComeFromTheSettings holds the rigs to the registry: a top-level
// directory that holds a config.json the registry does not name is not a rig,
// so its contents are not daemon-owned and neither the export nor an import
// writes them. A rig the registry names is learned from the archive's registry,
// which must come before the rig's rows.
func TestRigsComeFromTheSettings(t *testing.T) {
	t.Parallel()
	const registry = `{"registry":{"version":1,"rigs":{"gastown":{}}}}`
	town := t.TempDir()
	writeStateFile(t, town, "mayor/town.json", registry, 0o644)
	writeStateFile(t, town, "gastown/config.json", `{"name":"gastown"}`, 0o644)
	writeStateFile(t, town, "tools/config.json", `{"name":"tools"}`, 0o644)
	writeStateFile(t, town, "tools/notes.txt", "not a rig\n", 0o644)

	var stream bytes.Buffer
	if _, err := Export(&stream, town, Default(), ExportOptions{}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	names := archiveNames(t, stream.Bytes())
	if !slices.Contains(names, "gastown/config.json") {
		t.Errorf("the archive lacks the registered rig's config.json: %v", names)
	}
	for _, name := range names {
		if strings.HasPrefix(name, "tools/") {
			t.Errorf("the archive carries %q, which is not a rig the registry names", name)
		}
	}

	t.Run("a rig the archive's registry names is restored", func(t *testing.T) {
		t.Parallel()
		archive := writeArchive(t, regularEntry("mayor/town.json", registry), regularEntry("gastown/config.json", "{}"))
		if _, err := Import(bytes.NewReader(archive), t.TempDir(), Default(), ImportOptions{}); err != nil {
			t.Fatalf("Import: %v", err)
		}
	})
	t.Run("a config.json the registry does not name is refused", func(t *testing.T) {
		t.Parallel()
		archive := writeArchive(t, regularEntry("mayor/town.json", registry), regularEntry("tools/config.json", "{}"))
		_, err := Import(bytes.NewReader(archive), t.TempDir(), Default(), ImportOptions{})
		if err == nil || !strings.Contains(err.Error(), `"tools/config.json"`) {
			t.Fatalf("Import = %v, want a refusal naming tools/config.json", err)
		}
	})
	t.Run("a rig row before the registry is refused", func(t *testing.T) {
		t.Parallel()
		archive := writeArchive(t, regularEntry("gastown/config.json", "{}"), regularEntry("mayor/town.json", registry))
		_, err := Import(bytes.NewReader(archive), t.TempDir(), Default(), ImportOptions{})
		if err == nil || !strings.Contains(err.Error(), `"gastown/config.json"`) {
			t.Fatalf("Import = %v, want a refusal naming gastown/config.json", err)
		}
	})
	t.Run("a registry in the archive that does not decode is refused", func(t *testing.T) {
		t.Parallel()
		archive := writeArchive(t, regularEntry("mayor/town.json", "{not json"))
		_, err := Import(bytes.NewReader(archive), t.TempDir(), Default(), ImportOptions{})
		if err == nil || !strings.Contains(err.Error(), "reading the rigs the registry in the archive names") {
			t.Fatalf("Import = %v, want a refusal reading the archive's registry", err)
		}
	})
}

// regularFilesIn lists the regular files under root, sorted, relative to it.
func regularFilesIn(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	if err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			rel, relErr := filepath.Rel(root, p)
			if relErr != nil {
				return relErr
			}
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestExportSinceAndRollbackSet drives the --since filter and holds the
// rollback tag to the one-way set townstate.Default documents.
func TestExportSinceAndRollbackSet(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	town := t.TempDir()
	oldLog := writeStateFile(t, town, "logs/old.log", "old\n", 0o644)
	newLog := writeStateFile(t, town, "logs/current.log", "current\n", 0o644)
	if err := os.Chtimes(oldLog, now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newLog, now.Add(-time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	var stream bytes.Buffer
	sum, err := Export(&stream, town, Default(), ExportOptions{AllowNoRigs: true, Since: now.Add(-24 * time.Hour)})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	names := archiveNames(t, stream.Bytes())
	if !slices.Contains(names, "logs/current.log") {
		t.Errorf("--since dropped a file modified after it: %v", names)
	}
	if slices.Contains(names, "logs/old.log") {
		t.Errorf("--since kept a file modified before it: %v", names)
	}
	if !slices.Contains(names, "logs/") {
		t.Errorf("--since dropped the directory shape: %v", names)
	}
	if sum.Skipped[SkipOlderThan] != 1 {
		t.Errorf("summary skipped %d files older than --since, want 1", sum.Skipped[SkipOlderThan])
	}

	var got []string
	for _, e := range Default().Rollback() {
		got = append(got, e.Path)
	}
	want := []string{"daemon", "events", ".events.jsonl", ".feed.jsonl", ".runtime/landings", ".runtime/spec_dispatch", ".runtime/red-main"}
	if !slices.Equal(got, want) {
		t.Errorf("the rollback set = %v, want %v (the design's one-way list)", got, want)
	}
	for _, e := range Default().Rollback() {
		if e.Owner != OwnerDaemon {
			t.Errorf("the rollback row %q has owner %q, want daemon", e.Path, e.Owner)
		}
		if !strings.Contains(e.Why, "rollback") {
			t.Errorf("the rollback row %q does not say why it is replayed: %q", e.Path, e.Why)
		}
	}
	// The maintenance and patrol state the design names is below daemon, which
	// is why that row carries the tag.
	for _, p := range []string{"daemon/maintenance_state.json", "daemon/patrol_last_run.json"} {
		e, ok := Lookup(Default().Expand(nil, nil), p)
		if !ok || !e.Rollback {
			t.Errorf("%s is not covered by a rollback-tagged row (covered %v, tagged %v)", p, ok, e.Rollback)
		}
	}

	// A rollback or private tag on a row the volume never carries is refused.
	for _, tc := range []struct {
		name string
		e    Entry
		want string
	}{
		{name: "rollback on a session row", e: Entry{Path: "<rig>/crew", Owner: OwnerSession, Why: "a session tree", Rollback: true}, want: "rollback: true needs a daemon owner"},
		{name: "private on a shared row", e: Entry{Path: "settings", Owner: OwnerShared, Why: "edited by gt config", Private: true}, want: "private: true needs a daemon owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := &Manifest{Entries: []Entry{tc.e}}
			err := m.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate error = %q, want it to hold %q", err, tc.want)
			}
		})
	}
}

// TestExportNeverFollowsSymlinks drives the walk over a link that leaves the
// town and a link that stays inside: the escapee is skipped and its target is
// never read, the inside link is archived as a link, and the walk does not
// descend either.
func TestExportNeverFollowsSymlinks(t *testing.T) {
	t.Parallel()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("SECRET-TARGET-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}

	town := t.TempDir()
	writeStateFile(t, town, "logs/inner/file.txt", "inside\n", 0o644)
	writeStateFile(t, town, "mayor/town.json", "{}\n", 0o644)
	if err := os.Symlink(secret, filepath.Join(town, "mayor", "leak")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("inner", filepath.Join(town, "logs", "inner-link")); err != nil {
		t.Fatal(err)
	}

	var stream bytes.Buffer
	sum, err := Export(&stream, town, Default(), ExportOptions{AllowNoRigs: true})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if sum.Skipped[SkipSymlinkOut] != 1 {
		t.Errorf("summary skipped %d symlinks leaving the town root, want 1 (%s)", sum.Skipped[SkipSymlinkOut], sum)
	}

	var hasInsideLink bool
	for _, e := range readArchive(t, stream.Bytes()) {
		if strings.Contains(string(e.data), "SECRET-TARGET-CONTENT") {
			t.Fatalf("the export read the symlink's target into %q", e.hdr.Name)
		}
		switch e.hdr.Name {
		case "mayor/leak":
			t.Errorf("the export archived the escaping symlink")
		case "logs/inner-link":
			hasInsideLink = true
			if e.hdr.Typeflag != tar.TypeSymlink || e.hdr.Linkname != "inner" {
				t.Errorf("logs/inner-link = type %q target %q, want a symlink to inner", e.hdr.Typeflag, e.hdr.Linkname)
			}
		case "logs/inner-link/file.txt":
			t.Errorf("the export followed the symlink into its target")
		}
	}
	if !hasInsideLink {
		t.Errorf("the export dropped a symlink that stays inside the town root: %v", archiveNames(t, stream.Bytes()))
	}

	// The archive still imports cleanly: the inside link is recreated and the
	// escapee never existed to recreate.
	dest := t.TempDir()
	if _, err := Import(bytes.NewReader(stream.Bytes()), dest, Default(), ImportOptions{}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	target, err := os.Readlink(filepath.Join(dest, "logs", "inner-link"))
	if err != nil || target != "inner" {
		t.Errorf("the imported link = %q, %v; want inner", target, err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "mayor", "leak")); !os.IsNotExist(err) {
		t.Errorf("the import recreated the escaping symlink (err %v)", err)
	}
}

// TestExportRefusesSymlinkedAncestor drives the ancestor check: a daemon-owned
// row reached through an ancestor that is a symlink (.runtime -> an outside
// tree) or a regular file is refused before anything below it is read, and
// nothing is archived.
func TestExportRefusesSymlinkedAncestor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		bad  func(t *testing.T, town string)
	}{
		{
			name: "a symlinked ancestor",
			bad: func(t *testing.T, town string) {
				outside := t.TempDir()
				writeStateFile(t, outside, "landings/landing1.json", `{"landing":1}`, 0o644)
				if err := os.Symlink(outside, filepath.Join(town, ".runtime")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "an ancestor that is a regular file",
			bad: func(t *testing.T, town string) {
				writeStateFile(t, town, ".runtime", "not a directory\n", 0o644)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			tc.bad(t, town)

			var stream bytes.Buffer
			_, err := Export(&stream, town, Default(), ExportOptions{AllowNoRigs: true})
			if err == nil || !strings.Contains(err.Error(), ".runtime") || !strings.Contains(err.Error(), "not a directory") {
				t.Fatalf("Export over %s = %v, want a refusal naming .runtime", tc.name, err)
			}
			if stream.Len() != 0 {
				t.Errorf("the refused export wrote %d bytes; nothing below the ancestor may enter the archive", stream.Len())
			}
		})
	}
}

// TestExportSkipsLinksTheImportRefuses drives the one lookup Export and Import
// share: a link that lands in a session-owned directory is left out with the
// reason in the summary, and the rest of the archive still restores.
func TestExportSkipsLinksTheImportRefuses(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeStateFile(t, town, "daemon/maintenance_state.json", `{"db":"main"}`, 0o644)
	writeStateFile(t, town, "gastown/crew/eagle/notes.md", "crew work\n", 0o644)
	if err := os.Symlink("../gastown/crew/eagle", filepath.Join(town, "daemon", "x")); err != nil {
		t.Fatal(err)
	}

	var stream bytes.Buffer
	sum, err := Export(&stream, town, Default(), ExportOptions{AllowNoRigs: true})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if sum.Skipped[SkipSymlinkOut] != 1 {
		t.Errorf("summary skipped %d symlinks, want 1 (%s)", sum.Skipped[SkipSymlinkOut], sum)
	}
	if want := "daemon: a link that lands outside the daemon-owned rows"; !strings.Contains(sum.String(), want) {
		t.Errorf("the summary does not say why the link was left out (%q):\n%s", want, sum)
	}
	if slices.Contains(archiveNames(t, stream.Bytes()), "daemon/x") {
		t.Errorf("the export archived a link the import would refuse")
	}

	dest := t.TempDir()
	if _, err := Import(bytes.NewReader(stream.Bytes()), dest, Default(), ImportOptions{}); err != nil {
		t.Fatalf("Import of the rest: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "daemon", "maintenance_state.json"))
	if err != nil || string(got) != `{"db":"main"}` {
		t.Errorf("the restore of the rest did not complete: %q, %v", got, err)
	}
}

// TestExportJudgesLinksByTheArchiveNotTheSource drives the tree the export
// resolves a target through: the names written so far, as an import holds them
// at the same entry. A component the archive skips (.dolt-data), or one from a
// row the archive has not reached yet (.beads), is absent, so a link that climbs
// out of it is left out even though the source tree holds the name.
func TestExportJudgesLinksByTheArchiveNotTheSource(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeStateFile(t, town, ".beads/am/meta.json", "{}", 0o644)
	writeStateFile(t, town, ".beads/.dolt-data/db/config.yaml", "x", 0o644)
	if err := os.MkdirAll(filepath.Join(town, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	links := map[string]string{
		"l": "../.beads/.dolt-data/../am", // through a skipped class
		"m": "../.beads/am/../meta.json",  // through a row not written yet
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(town, "daemon", name)); err != nil {
			t.Fatal(err)
		}
	}

	var stream bytes.Buffer
	sum, err := Export(&stream, town, Default(), ExportOptions{AllowNoRigs: true})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if sum.Skipped[SkipSymlinkOut] != len(links) {
		t.Errorf("summary skipped %d symlinks, want %d (%s)", sum.Skipped[SkipSymlinkOut], len(links), sum)
	}
	if want := "a link that climbs out of a name that does not exist yet"; !strings.Contains(sum.String(), want) {
		t.Errorf("the summary does not carry the reason %q:\n%s", want, sum)
	}
	names := archiveNames(t, stream.Bytes())
	for name := range links {
		if slices.Contains(names, "daemon/"+name) {
			t.Errorf("the export archived daemon/%s, a link the import would refuse", name)
		}
	}
	if _, err := Import(bytes.NewReader(stream.Bytes()), t.TempDir(), Default(), ImportOptions{}); err != nil {
		t.Fatalf("Import of the rest: %v", err)
	}
}

// TestExportRefusesATownWithNoRigRegistry drives the silent-loss case: a town
// whose registry names no rig (neither a "registry" section in mayor/town.json
// nor mayor/rigs.json) yields no rig-scoped rows, so an archive of it looks
// complete and is not. The export refuses with ErrNoRigs and writes nothing;
// AllowNoRigs lets a town that really has no rigs through, and the summary then
// says how many rows are missing.
func TestExportRefusesATownWithNoRigRegistry(t *testing.T) {
	t.Parallel()

	// A town with daemon state under gastown/ but no registry anywhere.
	newTown := func(t *testing.T) string {
		t.Helper()
		town := t.TempDir()
		writeStateFile(t, town, "gastown/config.json", `{"name":"gastown"}`, 0o644)
		writeStateFile(t, town, "logs/daemon.log", "daemon log\n", 0o644)
		return town
	}

	t.Run("no registry is refused and writes nothing", func(t *testing.T) {
		t.Parallel()
		var stream bytes.Buffer
		sum, err := Export(&stream, newTown(t), Default(), ExportOptions{})
		if !errors.Is(err, ErrNoRigs) {
			t.Fatalf("Export error = %v, want ErrNoRigs", err)
		}
		if names := archiveNames(t, stream.Bytes()); len(names) != 0 {
			t.Errorf("a refused export wrote %v, want an empty archive", names)
		}
		if sum.RigRowsDropped == 0 {
			t.Errorf("summary RigRowsDropped = 0, want the count of rig-scoped rows the archive would lack")
		}
	})

	t.Run("an empty registry is refused the same way", func(t *testing.T) {
		t.Parallel()
		town := newTown(t)
		writeStateFile(t, town, "mayor/town.json", `{"registry":{"version":1,"rigs":{}}}`, 0o644)
		if _, err := Export(&bytes.Buffer{}, town, Default(), ExportOptions{}); !errors.Is(err, ErrNoRigs) {
			t.Fatalf("Export error = %v, want ErrNoRigs for a registry with no rigs", err)
		}
	})

	t.Run("AllowNoRigs exports and the summary says rows are missing", func(t *testing.T) {
		t.Parallel()
		var stream bytes.Buffer
		sum, err := Export(&stream, newTown(t), Default(), ExportOptions{AllowNoRigs: true})
		if err != nil {
			t.Fatalf("Export with AllowNoRigs: %v", err)
		}
		if names := archiveNames(t, stream.Bytes()); slices.Contains(names, "gastown/config.json") {
			t.Errorf("a town with no registry archived a rig row: %v", names)
		}
		if sum.RigRowsDropped == 0 || !strings.Contains(sum.String(), "WARNING: the town registers no rigs") {
			t.Errorf("summary = %q (RigRowsDropped %d), want the missing rows named", sum.String(), sum.RigRowsDropped)
		}
	})

	t.Run("a town with a registry is not affected", func(t *testing.T) {
		t.Parallel()
		town := newTown(t)
		writeStateFile(t, town, "mayor/town.json", `{"registry":{"version":1,"rigs":{"gastown":{}}}}`, 0o644)
		var stream bytes.Buffer
		sum, err := Export(&stream, town, Default(), ExportOptions{})
		if err != nil {
			t.Fatalf("Export: %v", err)
		}
		if sum.RigRowsDropped != 0 || strings.Contains(sum.String(), "WARNING") {
			t.Errorf("summary = %q, want no warning for a town with rigs", sum.String())
		}
		if names := archiveNames(t, stream.Bytes()); !slices.Contains(names, "gastown/config.json") {
			t.Errorf("a registered rig's row is missing from the archive: %v", names)
		}
	})
}
