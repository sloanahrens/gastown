package repoguard

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestPluginRunShTree holds a staged plugins tree to a staged allowlist
// through the lister the guard runs in a checkout.
//
// Every file it reads is one this test wrote under t.TempDir, which has no
// repository around it and is gone when the test ends: the check is hermetic,
// and its verdict cannot depend on the live checkout, the working directory or
// anything outside the tree it stages (gt-tpg0p.2). The checkout itself is
// held by TestIntegrationPluginRunShTree, which the integration tier runs.
func TestPluginRunShTree(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{
			name: "a shim and an allowlisted script agree with the list",
			files: map[string]string{
				"plugins/good/run.sh":    shimBody,
				"plugins/good/plugin.md": pluginMD(declaredBinaryID),
				"plugins/old/run.sh":     logicBody,
				"plugins/old/plugin.md":  pluginMD(""),
				allowlist:                "plugins/old/run.sh\tports in gt-tpg0p.3\n",
			},
		},
		{
			name: "a script over the line cap with no line",
			files: map[string]string{
				"plugins/old/run.sh":    logicBody,
				"plugins/old/plugin.md": pluginMD(""),
			},
			want: []string{
				"plugins/old/run.sh: it holds 3 commands and a shim holds one exec; " + shimBoundAlarm,
			},
		},
		{
			name: "a shim whose plugin.md names no binary",
			files: map[string]string{
				"plugins/good/run.sh":    shimBody,
				"plugins/good/plugin.md": pluginMD(""),
			},
			want: []string{
				`plugins/good/run.sh: its exec target "$(dirname $0)/gt" has no binary evidence: the plugin.md [execution] block names no binary, so the guard cannot tell it from a script; ` + shimBoundAlarm,
			},
		},
		{
			name: "a binary key outside [execution] is not evidence",
			files: map[string]string{
				"plugins/good/run.sh":    shimBody,
				"plugins/good/plugin.md": "+++\nname = \"good\"\n\n[gate]\nbinary = \"gt\"\n\n[execution]\ntype = \"script\"\n+++\n",
			},
			want: []string{
				`plugins/good/run.sh: its exec target "$(dirname $0)/gt" has no binary evidence: the plugin.md [execution] block names no binary, so the guard cannot tell it from a script; ` + shimBoundAlarm,
			},
		},
		{
			name: "an extensionless sibling that is not the declared binary",
			files: map[string]string{
				"plugins/good/run.sh":    runSh("exec \"$(dirname \"$0\")/helper\"\n"),
				"plugins/good/plugin.md": pluginMD(declaredBinaryID),
			},
			want: []string{
				`plugins/good/run.sh: its exec target "$(dirname $0)/helper" is not the binary plugin.md declares ("gt"); ` + shimBoundAlarm,
			},
		},
		{
			name: "a line whose script left the tree",
			files: map[string]string{
				"plugins/good/run.sh":    shimBody,
				"plugins/good/plugin.md": pluginMD(declaredBinaryID),
				allowlist:                "plugins/gone/run.sh\tdropped in gt-98pgy\n",
			},
			want: []string{"plugins/gone/run.sh: " + staleLineAlarm},
		},
		{
			name: "a line with no reason",
			files: map[string]string{
				"plugins/old/run.sh":    logicBody,
				"plugins/old/plugin.md": pluginMD(""),
				allowlist:               "plugins/old/run.sh\n",
			},
			want: []string{"plugins/old/run.sh: " + noReasonAlarm},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := stagePluginTree(t, tc.files)
			allowed, err := ReadPluginRunShAllowlist(filepath.Join(root, filepath.FromSlash(allowlist)))
			if err != nil {
				t.Fatalf("reading the staged allowlist: %v", err)
			}
			findings, err := CheckPluginRunSh(PluginRunShTree(root), allowed)
			if err != nil {
				t.Fatalf("listing the staged tree: %v", err)
			}
			if got, want := findings, tc.want; !slices.Equal(got, want) {
				t.Errorf("findings = %q, want %q", got, want)
			}
		})
	}
}

// TestPluginRunShTreeAcceptsAPluginsDirWithNoShim: once every shim is ported
// the plugins directory holds no run.sh, and that is the clean end state the
// allowlist shrinks toward, not an unreadable tree. A README beside the plugin
// directories is not a plugin and does not stop the listing. The guard reads it
// through its production caller and finds nothing (gt-tpg0p.22).
func TestPluginRunShTreeAcceptsAPluginsDirWithNoShim(t *testing.T) {
	t.Parallel()
	root := stagePluginTree(t, map[string]string{
		"plugins/README.md":             "the plugins\n",
		"plugins/app-promote/plugin.md": pluginMD(""),
	})
	findings, err := CheckPluginRunSh(PluginRunShTree(root), map[string]string{})
	if err != nil {
		t.Fatalf("CheckPluginRunSh(plugins with no run.sh) error = %v, want none", err)
	}
	if len(findings) != 0 {
		t.Errorf("CheckPluginRunSh(plugins with no run.sh) findings = %q, want none", findings)
	}
}

// TestPluginRunShTreeRefusesAnUnreadableTree: a root with no plugins directory
// is an error, never a clean tree, so a moved plugins directory cannot pass by
// matching nothing.
func TestPluginRunShTreeRefusesAnUnreadableTree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if _, err := PluginRunShTree(root)(); err == nil {
		t.Fatal("PluginRunShTree(no plugins directory) = no error, want one")
	}
}

// TestPluginRunShTreeRefusesARunShWithoutPluginMD: the declaration lives in
// plugin.md, so a run.sh without one cannot be judged and fails the listing
// rather than being read as having no binary.
func TestPluginRunShTreeRefusesARunShWithoutPluginMD(t *testing.T) {
	t.Parallel()
	root := stagePluginTree(t, map[string]string{"plugins/good/run.sh": shimBody})
	if _, err := PluginRunShTree(root)(); err == nil {
		t.Fatal("PluginRunShTree(run.sh without plugin.md) = no error, want one")
	}
}

// TestPluginRunShTreeRefusesARunShItCannotRead: a directory named run.sh is an
// entry the listing finds and cannot read, and a listing that skipped it would
// hold the rest of the tree to a guard that never saw all of it.
func TestPluginRunShTreeRefusesARunShItCannotRead(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "plugins", "good", "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := PluginRunShTree(root)(); err == nil {
		t.Fatal("PluginRunShTree(a run.sh directory) = no error, want one")
	}
}

// TestPluginRunShTreeRefusesAPluginsPathThatIsAFile: a plugins entry that is
// not a directory is an error, never an empty tree, since listing it finds no
// plugins and the guard would read that as clean.
func TestPluginRunShTreeRefusesAPluginsPathThatIsAFile(t *testing.T) {
	t.Parallel()
	root := stagePluginTree(t, map[string]string{"plugins": "not a directory\n"})
	if _, err := PluginRunShTree(root)(); err == nil {
		t.Fatal("PluginRunShTree(plugins is a file) = no error, want one")
	}
}

// TestPluginRunShTreeRefusesAnUnlistablePluginsDir: a plugins directory that
// exists but cannot be listed is an error, never an empty tree. The reader is
// injected because a chmod fixture would pass for a user that reads through
// permission bits, and the refusal is what the lister must return.
func TestPluginRunShTreeRefusesAnUnlistablePluginsDir(t *testing.T) {
	t.Parallel()
	root := stagePluginTree(t, map[string]string{"plugins/good/plugin.md": pluginMD("")})
	denied := func(string) ([]fs.DirEntry, error) { return nil, fs.ErrPermission }
	_, err := pluginRunShTree(root, denied)()
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("pluginRunShTree(unlistable plugins) error = %v, want fs.ErrPermission", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(root, "plugins")) {
		t.Errorf("error %q does not name the plugins directory", err)
	}
}

// TestPluginRunShTreeRefusesAPluginEntryItCannotLookUp: an entry that is a
// symlink to a file is neither a plain file nor a directory, so its run.sh is
// looked up beneath a path that is not a directory. That lookup fails with a
// reason other than a missing run.sh, and the listing returns it rather than
// treating the plugin as having no shim.
func TestPluginRunShTreeRefusesAPluginEntryItCannotLookUp(t *testing.T) {
	t.Parallel()
	root := stagePluginTree(t, map[string]string{
		"plugins/good/plugin.md": pluginMD(""),
		"elsewhere":              "not a plugin\n",
	})
	if err := os.Symlink(filepath.Join(root, "elsewhere"), filepath.Join(root, "plugins", "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := PluginRunShTree(root)(); err == nil {
		t.Fatal("PluginRunShTree(a plugin entry that is a file symlink) = no error, want one")
	}
}

// allowlist is where a staged tree carries its allowlist, the same path the
// checkout uses.
const allowlist = "internal/repoguard/plugin-run-sh.txt"

// stagePluginTree writes files, keyed by slash-separated repo-relative path,
// under a fresh t.TempDir, and returns its root.
func stagePluginTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	if _, ok := files[allowlist]; !ok {
		files[allowlist] = "# no ported-backlog entry in this fixture\n"
	}
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// pluginMD is a plugin.md whose [execution] block names binary, or declares
// only the type when binary is "".
func pluginMD(binary string) string {
	block := "+++\nname = \"fixture\"\n\n[execution]\ntype = \"script\"\n"
	if binary != "" {
		block += "binary = \"" + binary + "\"\n"
	}
	return block + "+++\n"
}
