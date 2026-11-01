package daemonimage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/templates"
)

func sumHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// tgzOf is a .tar.gz holding the given files, in order.
func tgzOf(t *testing.T, files map[string]string, order []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, name := range order {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func fetchOf(body []byte) func(context.Context, string) (io.ReadCloser, error) {
	return func(context.Context, string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
}

func outEntries(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func TestDaemonImagePinsCarryASha256ForEveryTool(t *testing.T) {
	t.Parallel()
	hex64 := regexp.MustCompile(`^[0-9a-f]{64}$`)
	want := map[string]bool{"openshell": false, "docker": false, "kubectl": false}
	for _, p := range templates.DaemonClientTools {
		if _, ok := want[p.Name]; !ok {
			t.Errorf("unexpected client tool %q", p.Name)
		}
		want[p.Name] = true
		if !hex64.MatchString(p.SHA256) {
			t.Errorf("%s pin sha256 = %q, want 64 lower-case hex digits", p.Name, p.SHA256)
		}
		if !strings.HasPrefix(p.URL, "https://") {
			t.Errorf("%s pin URL = %q, want https", p.Name, p.URL)
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("client tool %s has no pin", name)
		}
	}
}

func TestFetchedClientToolIsVerifiedAgainstItsPin(t *testing.T) {
	t.Parallel()
	b, _ := imageBuilder(t)
	pin := templates.ClientToolPin{Name: "kubectl", URL: "https://example.test/kubectl", SHA256: sumHex([]byte("good"))}
	b.Fetch = fetchOf([]byte("evil"))
	before := outEntries(t, b.Out)
	if _, err := b.fetchClientTool(context.Background(), pin, 1<<20); err == nil || !strings.Contains(err.Error(), "not the pinned") {
		t.Fatalf("error = %v, want a sha256 mismatch", err)
	}
	if after := outEntries(t, b.Out); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("a refused download left files: before %v, after %v", before, after)
	}
}

func TestFetchedClientToolIsPlacedWhenItVerifies(t *testing.T) {
	t.Parallel()
	b, _ := imageBuilder(t)
	body := []byte("kubectl-bytes")
	pin := templates.ClientToolPin{Name: "kubectl", URL: "https://example.test/kubectl", SHA256: sumHex(body)}
	b.Fetch = fetchOf(body)
	path, err := b.fetchClientTool(context.Background(), pin, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(body) || filepath.Base(path) != "kubectl" {
		t.Errorf("placed %s = %q (%v), want the downloaded bytes named kubectl", path, got, err)
	}
	for _, n := range outEntries(t, b.Out) {
		if strings.HasPrefix(n, ".") {
			t.Errorf("a staging file %s was left behind", n)
		}
	}
}

func TestFetchedClientToolTakesOneMemberFromAnArchive(t *testing.T) {
	t.Parallel()
	b, _ := imageBuilder(t)
	archive := tgzOf(t, map[string]string{"docker/dockerd": "daemon", "docker/docker": "client"}, []string{"docker/dockerd", "docker/docker"})
	pin := templates.ClientToolPin{Name: "docker", URL: "https://example.test/docker.tgz", SHA256: sumHex(archive), Member: "docker/docker"}
	b.Fetch = fetchOf(archive)
	path, err := b.fetchClientTool(context.Background(), pin, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "client" {
		t.Errorf("the placed docker = %q, want the docker/docker member's bytes", got)
	}
}

func TestFetchedClientToolRefusesAnArchiveWithoutTheMember(t *testing.T) {
	t.Parallel()
	b, _ := imageBuilder(t)
	archive := tgzOf(t, map[string]string{"docker/dockerd": "daemon"}, []string{"docker/dockerd"})
	pin := templates.ClientToolPin{Name: "docker", URL: "https://example.test/docker.tgz", SHA256: sumHex(archive), Member: "docker/docker"}
	b.Fetch = fetchOf(archive)
	before := outEntries(t, b.Out)
	if _, err := b.fetchClientTool(context.Background(), pin, 1<<20); err == nil || !strings.Contains(err.Error(), "has no file") {
		t.Fatalf("error = %v, want one naming the missing member", err)
	}
	if after := outEntries(t, b.Out); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("a refused archive left files: before %v, after %v", before, after)
	}
}

func TestFetchedClientToolRefusesADownloadOverTheBound(t *testing.T) {
	t.Parallel()
	b, _ := imageBuilder(t)
	body := bytes.Repeat([]byte("x"), 100)
	pin := templates.ClientToolPin{Name: "kubectl", URL: "https://example.test/kubectl", SHA256: sumHex(body)}
	b.Fetch = fetchOf(body)
	if _, err := b.fetchClientTool(context.Background(), pin, 10); err == nil || !strings.Contains(err.Error(), "over the 10-byte bound") {
		t.Fatalf("error = %v, want the bound refusal", err)
	}
}

func TestFetchClientToolsRefusesWithoutAFetcherOrAbsoluteOut(t *testing.T) {
	t.Parallel()
	b, _ := imageBuilder(t)
	b.Fetch = nil
	if _, err := b.FetchClientTools(context.Background()); err == nil || !strings.Contains(err.Error(), "no fetcher") {
		t.Errorf("error = %v, want the no-fetcher refusal", err)
	}
	b.Fetch = fetchOf(nil)
	b.Out = "rel/out"
	if _, err := b.FetchClientTools(context.Background()); err == nil || !strings.Contains(err.Error(), "not an absolute directory") {
		t.Errorf("error = %v, want the relative-directory refusal", err)
	}
}
