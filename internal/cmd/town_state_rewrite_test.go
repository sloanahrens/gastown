package cmd

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/townstate"
)

const (
	rewriteCmdOldRoot = "/Users/sloan/gt"
	rewriteCmdNewRoot = "/town"
)

// writeRewriteCmdTown writes a scratch town for the rewrite command: one class
// file that carries the old root, and a log that carries it too and is named as
// skipped.
func writeRewriteCmdTown(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"settings/config.json": `{"town_root":"` + rewriteCmdOldRoot + `"}`,
		// A file in a class the pass's list does not carry, and a log the walk
		// leaves alone; both hold the old root.
		"settings/notes.txt": rewriteCmdOldRoot + " notes\n",
		"logs/keep.log":      rewriteCmdOldRoot + " started\n",
	}
	for rel, data := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
			t.Fatalf("writing %s: %v", rel, err)
		}
	}
	return root
}

// rewriteCmdTree reads the scratch town, keyed by slash path.
func rewriteCmdTree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(full string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("reading the scratch town: %v", err)
	}
	return files
}

// TestTownStateRewriteCommand drives the command end to end: dry run writes
// nothing, --apply writes the class file and then exits non-zero naming a file
// left holding the old root by a class the pass does not carry, and --undo
// restores the kept copy.
func TestTownStateRewriteCommand(t *testing.T) {
	t.Parallel()
	root := writeRewriteCmdTown(t)
	before := rewriteCmdTree(t, root)

	// Dry run is the default: it reports the file and writes nothing.
	var dry bytes.Buffer
	err := runTownStateRewriteIn(townStateRewriteEnv{
		town:     root,
		fromRoot: rewriteCmdOldRoot,
		toRoot:   rewriteCmdNewRoot,
		out:      &dry,
	})
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !strings.Contains(dry.String(), "would change settings/config.json (1)") {
		t.Errorf("dry run output %q does not report the settings file", dry.String())
	}
	if !strings.Contains(dry.String(), "left alone:") || !strings.Contains(dry.String(), "logs") {
		t.Errorf("dry run output %q does not name the skipped classes", dry.String())
	}
	if !strings.Contains(dry.String(), "classes: town settings") {
		t.Errorf("dry run output %q does not name the classes it walked", dry.String())
	}
	if after := rewriteCmdTree(t, root); !mapsSame(after, before) {
		t.Fatalf("the dry run wrote the town: %v", after)
	}

	// --apply writes, then the scan catches a class the pass leaves out.
	scanClasses := append(townstate.RewriteClasses(nil), townstate.RewriteClass{
		Name:  "notes",
		Match: func(rel string) bool { return strings.HasSuffix(rel, ".txt") },
	})
	var applied bytes.Buffer
	err = runTownStateRewriteIn(townStateRewriteEnv{
		town:        root,
		fromRoot:    rewriteCmdOldRoot,
		toRoot:      rewriteCmdNewRoot,
		apply:       true,
		scanClasses: scanClasses,
		out:         &applied,
	})
	if err == nil || !strings.Contains(err.Error(), "settings/notes.txt") {
		t.Fatalf("apply with a class still holding the root = %v, want a refusal naming settings/notes.txt", err)
	}
	tree := rewriteCmdTree(t, root)
	if got := tree["settings/config.json"]; got != `{"town_root":"`+rewriteCmdNewRoot+`"}` {
		t.Errorf("apply did not rewrite the settings: %s", got)
	}
	if got := tree["settings/config.json"+townstate.RewriteBackupSuffix]; got != before["settings/config.json"] {
		t.Errorf("apply kept %q, want the old settings %q", got, before["settings/config.json"])
	}

	// --undo puts the original back and removes the kept copy.
	var undone bytes.Buffer
	if err := runTownStateRewriteIn(townStateRewriteEnv{town: root, undo: true, out: &undone}); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if !strings.Contains(undone.String(), "restored settings/config.json") {
		t.Errorf("undo output %q does not name the restored file", undone.String())
	}
	if after := rewriteCmdTree(t, root); !mapsSame(after, before) {
		t.Errorf("undo did not restore the town: %v", after)
	}
}

// TestTownStateRewriteReadsItsFlags drives the one failure each flag read can
// have: a flag registered with the type the command does not read. The command
// refuses before it touches the town, so a wrong-typed flag cannot silent-default.
func TestTownStateRewriteReadsItsFlags(t *testing.T) {
	t.Parallel()
	root := writeRewriteCmdTown(t)
	values := map[string]string{"town": root, "from-root": rewriteCmdOldRoot, "to-root": rewriteCmdNewRoot}
	for _, target := range []string{"town", "from-root", "to-root", "from-owner", "to-owner", "apply", "undo"} {
		t.Run(target, func(t *testing.T) {
			t.Parallel()
			cmd := &cobra.Command{}
			for _, name := range []string{"town", "from-root", "to-root", "from-owner", "to-owner", "apply", "undo"} {
				wrongType := name == target
				switch name {
				case "apply", "undo":
					if wrongType {
						cmd.Flags().String(name, "", "")
					} else {
						cmd.Flags().Bool(name, false, "")
					}
				default:
					if wrongType {
						cmd.Flags().Bool(name, false, "")
					} else {
						cmd.Flags().String(name, values[name], "")
					}
				}
			}
			err := runTownStateRewrite(cmd, nil)
			if err == nil || !strings.Contains(err.Error(), "reading --"+target) {
				t.Fatalf("runTownStateRewrite = %v, want a refusal naming --%s", err, target)
			}
		})
	}
}

// TestTownStateRewriteUndoAndScanFailures drives the two errors the command
// hands back from townstate: an undo it cannot run, and a post-apply scan that
// cannot read a file it was asked to check.
func TestTownStateRewriteUndoAndScanFailures(t *testing.T) {
	t.Parallel()
	root := writeRewriteCmdTown(t)

	if err := runTownStateRewriteIn(townStateRewriteEnv{
		town: filepath.Join(root, "absent"),
		undo: true,
		out:  &bytes.Buffer{},
	}); err == nil || !strings.Contains(err.Error(), "reading the town root") {
		t.Errorf("undo of a missing town = %v, want a refusal naming the read", err)
	}

	binary := filepath.Join(root, "settings", "blob.bin")
	if err := os.WriteFile(binary, []byte(rewriteCmdOldRoot+"\x00"), 0o644); err != nil {
		t.Fatalf("writing the binary file: %v", err)
	}
	err := runTownStateRewriteIn(townStateRewriteEnv{
		town:     root,
		fromRoot: rewriteCmdOldRoot,
		toRoot:   rewriteCmdNewRoot,
		apply:    true,
		scanClasses: append(townstate.RewriteClasses(nil), townstate.RewriteClass{
			Name:  "blobs",
			Match: func(rel string) bool { return strings.HasSuffix(rel, ".bin") },
		}),
		out: &bytes.Buffer{},
	})
	if err == nil || !strings.Contains(err.Error(), "blob.bin holds a NUL byte") {
		t.Fatalf("apply whose scan reads a binary file = %v, want a refusal naming the NUL byte", err)
	}
}

// TestTownStateRewriteCommandRefuses drives every refusal the command makes
// before it writes: a query that cannot name a rewrite, and a town it cannot
// read.
func TestTownStateRewriteCommandRefuses(t *testing.T) {
	t.Parallel()
	root := writeRewriteCmdTown(t)
	file := filepath.Join(root, "settings", "config.json")
	notADir := filepath.Join(root, "settings", "config.json")

	cases := []struct {
		name string
		edit func(*townStateRewriteEnv)
		want string
	}{
		{name: "equal roots", edit: func(e *townStateRewriteEnv) { e.toRoot = e.fromRoot }, want: "are the same"},
		{name: "new root under the old one", edit: func(e *townStateRewriteEnv) { e.toRoot = rewriteCmdOldRoot + "/town" }, want: "is under --from-root"},
		{name: "relative to-root", edit: func(e *townStateRewriteEnv) { e.toRoot = "town" }, want: "is not an absolute path"},
		{name: "relative from-root", edit: func(e *townStateRewriteEnv) { e.fromRoot = "Users/sloan/gt" }, want: "is not an absolute path"},
		{name: "trailing slash", edit: func(e *townStateRewriteEnv) { e.fromRoot = rewriteCmdOldRoot + "/" }, want: "ends with a slash"},
		{name: "missing from-root", edit: func(e *townStateRewriteEnv) { e.fromRoot = "" }, want: "--from-root is required"},
		{name: "missing to-root", edit: func(e *townStateRewriteEnv) { e.toRoot = "" }, want: "--to-root is required"},
		{name: "one owner", edit: func(e *townStateRewriteEnv) { e.fromOwner = "sloan" }, want: "given together"},
		{name: "owner shape", edit: func(e *townStateRewriteEnv) { e.fromOwner, e.toOwner = "Sloan", "gastown" }, want: "not one lowercase account name"},
		{name: "missing town", edit: func(e *townStateRewriteEnv) { e.town = filepath.Join(root, "absent") }, want: "reading the town root"},
		{name: "relative town", edit: func(e *townStateRewriteEnv) { e.town = "gt" }, want: "--town"},
		{name: "town is a file", edit: func(e *townStateRewriteEnv) { e.town = notADir }, want: "is not a directory"},
		{name: "apply and undo", edit: func(e *townStateRewriteEnv) { e.apply, e.undo = true, true }, want: "--apply and --undo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := townStateRewriteEnv{
				town:     root,
				fromRoot: rewriteCmdOldRoot,
				toRoot:   rewriteCmdNewRoot,
				out:      &bytes.Buffer{},
			}
			tc.edit(&env)
			err := runTownStateRewriteIn(env)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runTownStateRewriteIn = %v, want a refusal naming %q", err, tc.want)
			}
			if data, readErr := os.ReadFile(file); readErr != nil || string(data) != `{"town_root":"`+rewriteCmdOldRoot+`"}` {
				t.Errorf("a refused run wrote the settings: %q, %v", data, readErr)
			}
		})
	}
}

// mapsSame reports whether two string-keyed maps hold the same entries.
func mapsSame(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
