package cmd

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// townStateBackupArchive writes a gzip tar archive holding each entry as a
// regular file under a fresh directory, and returns its path and the total
// bytes its entries hold.
func townStateBackupArchive(t *testing.T, entries [][2]string) (string, int64) {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	var total int64
	for _, e := range entries {
		hdr := &tar.Header{Name: e[0], Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(e[1]))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("writing header %s: %v", e[0], err)
		}
		if _, err := tw.Write([]byte(e[1])); err != nil {
			t.Fatalf("writing %s: %v", e[0], err)
		}
		total += int64(len(e[1]))
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing the tar stream: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing the gzip stream: %v", err)
	}
	p := filepath.Join(t.TempDir(), "state.tar.gz")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p, total
}

// TestTownStateBackupVerifyAcceptsAGoodArchive drives the accepted path: the
// archive's file and byte counts are printed and the command returns nil.
func TestTownStateBackupVerifyAcceptsAGoodArchive(t *testing.T) {
	t.Parallel()
	archive, total := townStateBackupArchive(t, [][2]string{
		{"mayor/town.json", `{"name":"town"}`},
		{"settings/config.json", `{"theme":"dark"}`},
	})
	var out bytes.Buffer
	if err := runTownStateBackupVerifyIn(townStateBackupVerifyEnv{archive: archive, out: &out}); err != nil {
		t.Fatalf("runTownStateBackupVerifyIn: %v", err)
	}
	want := fmt.Sprintf("%s: 2 files, %d bytes", archive, total)
	if !strings.Contains(out.String(), want) {
		t.Errorf("output = %q, want it to hold %q", out.String(), want)
	}
}

// TestTownStateBackupVerifyRefusesASecretNamedEntry drives the secret-named
// entry refusal: the error names the rule, the entry's content is never
// printed, and no counts are.
func TestTownStateBackupVerifyRefusesASecretNamedEntry(t *testing.T) {
	t.Parallel()
	const value = "GT_CONTAINER_REGISTRY_PASSWORD=hunter2-not-a-real-value"
	archive, _ := townStateBackupArchive(t, [][2]string{
		{"mayor/town.json", `{"name":"town"}`},
		{"settings/daemon.env", value},
	})
	var out bytes.Buffer
	err := runTownStateBackupVerifyIn(townStateBackupVerifyEnv{archive: archive, out: &out})
	if err == nil {
		t.Fatal("runTownStateBackupVerifyIn accepted an archive with settings/daemon.env")
	}
	if !strings.Contains(err.Error(), "has the shape of a secret") {
		t.Errorf("error = %q, want it to name the secret-shape rule", err)
	}
	if strings.Contains(err.Error(), "hunter2-not-a-real-value") {
		t.Errorf("error = %q, want it to print no value from the entry", err)
	}
	if out.Len() != 0 {
		t.Errorf("a refusal printed %q, want no counts", out.String())
	}
}

// hostPathToken matches an absolute or home-anchored path token in a help
// string. A deployment value is a setting, not a path: the help names the
// setting and leaves where it points to docs/architecture.md.
var hostPathToken = regexp.MustCompile(`(^|[\s(])~?/`)

// docsPointer matches the "docs/architecture.md (<section>)" pointer the help
// carries and captures the section it names.
var docsPointer = regexp.MustCompile(`docs/architecture\.md \(([^)]+)\)`)

// TestTownStateBackupHelpNamesNoMountPath: the help names the deployment setting
// and no host path, and its docs pointer resolves to a heading the doc has, so a
// pointer to a section the doc lacks turns the test red.
func TestTownStateBackupHelpNamesNoMountPath(t *testing.T) {
	t.Parallel()
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "architecture.md"))
	if err != nil {
		t.Fatalf("reading docs/architecture.md: %v", err)
	}
	for _, c := range []*cobra.Command{townStateBackupCmd, townStateBackupVerifyCmd} {
		help := c.Long + "\n" + c.Example
		if !strings.Contains(help, townStateBackupRootSetting) {
			t.Errorf("%s help = %q, want it to name %s", c.Name(), help, townStateBackupRootSetting)
		}
		if hostPathToken.MatchString(help) {
			t.Errorf("%s help = %q, want no host path", c.Name(), help)
		}
		m := docsPointer.FindStringSubmatch(help)
		if m == nil {
			t.Errorf("%s help = %q, want a docs/architecture.md (<section>) pointer", c.Name(), help)
			continue
		}
		if !strings.Contains(string(doc), "\n### "+m[1]+"\n") {
			t.Errorf("%s points at docs/architecture.md (%s), which has no such heading", c.Name(), m[1])
		}
	}
}

// TestTownStateBackupVerifyRefusesAtTheCommand drives the command's two
// refusals: an archive with no mayor/town.json and a file that is not a gzip
// stream. Each returns the command's wrap, names the rule and prints no counts.
func TestTownStateBackupVerifyRefusesAtTheCommand(t *testing.T) {
	t.Parallel()
	noIdentity, _ := townStateBackupArchive(t, [][2]string{{"settings/config.json", `{}`}})
	notGzip := filepath.Join(t.TempDir(), "state.tar.gz")
	if err := os.WriteFile(notGzip, []byte("not a gzip stream"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, archive, want string }{
		{"no mayor/town.json", noIdentity, "has no mayor/town.json"},
		{"not a gzip stream", notGzip, "not a readable gzip stream"},
	} {
		var out bytes.Buffer
		cmd := &cobra.Command{Use: "verify"}
		cmd.SetOut(&out)
		err := runTownStateBackupVerify(cmd, []string{tc.archive})
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "town state backup verify:") || out.Len() != 0 {
			t.Errorf("%s: err = %v, out = %q; want the command's refusal naming %q and no counts", tc.name, err, out.String(), tc.want)
		}
	}
}

// TestTownStateBackupVerifyNamesAMissingPath drives the missing path refusal:
// the error names the path and no counts are printed.
func TestTownStateBackupVerifyNamesAMissingPath(t *testing.T) {
	t.Parallel()
	archive := filepath.Join(t.TempDir(), "2026-11-18", "state.tar.gz")
	var out bytes.Buffer
	err := runTownStateBackupVerifyIn(townStateBackupVerifyEnv{archive: archive, out: &out})
	if err == nil {
		t.Fatal("runTownStateBackupVerifyIn accepted a missing archive")
	}
	if !strings.Contains(err.Error(), archive) {
		t.Errorf("error = %q, want it to name %s", err, archive)
	}
	if out.Len() != 0 {
		t.Errorf("a refusal printed %q, want no counts", out.String())
	}
}
