package daemonimage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/iotest"

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

func TestFetchedClientToolRefusesWhatItCannotUse(t *testing.T) {
	t.Parallel()
	good := []byte("body")
	truncatedTar := func() []byte {
		// A gzip holding a tar header that claims 1000 bytes and then ends.
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(zw)
		if err := tw.WriteHeader(&tar.Header{Name: "docker/docker", Mode: 0o755, Size: 1000, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte("short"))
		_ = zw.Close() // the tar writer is left unclosed on purpose
		return buf.Bytes()
	}()
	garbageTar := func() []byte {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(bytes.Repeat([]byte{0xff}, 600))
		_ = zw.Close()
		return buf.Bytes()
	}()
	for _, tc := range []struct {
		name  string
		pin   templates.ClientToolPin
		fetch func(context.Context, string) (io.ReadCloser, error)
		want  string
	}{
		{"a pin that is not a sha256", templates.ClientToolPin{Name: "k", URL: "https://x", SHA256: "abc"}, fetchOf(good), "not a sha256"},
		{"a failing fetch", templates.ClientToolPin{Name: "k", URL: "https://x", SHA256: sumHex(good)}, func(context.Context, string) (io.ReadCloser, error) { return nil, errors.New("offline") }, "cannot download k"},
		{"a body that errors midway", templates.ClientToolPin{Name: "k", URL: "https://x", SHA256: sumHex(good)}, func(context.Context, string) (io.ReadCloser, error) {
			return io.NopCloser(iotest.ErrReader(errors.New("reset"))), nil
		}, "cannot download k"},
		{"an archive that is not gzip", templates.ClientToolPin{Name: "d", URL: "https://x", SHA256: sumHex(good), Member: "m"}, fetchOf(good), "not gzip"},
		{"a gzip that is not a tar", templates.ClientToolPin{Name: "d", URL: "https://x", SHA256: sumHex(garbageTar), Member: "m"}, fetchOf(garbageTar), "does not read"},
		{"a member cut short", templates.ClientToolPin{Name: "d", URL: "https://x", SHA256: sumHex(truncatedTar), Member: "docker/docker"}, fetchOf(truncatedTar), "cannot read"},
	} {
		b, _ := imageBuilder(t)
		b.Fetch = tc.fetch
		if _, err := b.fetchClientTool(context.Background(), tc.pin, 1<<20); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want one containing %q", tc.name, err, tc.want)
		}
	}
}

func TestFetchedClientToolCannotBeStagedInAMissingDirectory(t *testing.T) {
	t.Parallel()
	b, _ := imageBuilder(t)
	b.Out = filepath.Join(b.Out, "gone", "away")
	body := []byte("kubectl")
	pin := templates.ClientToolPin{Name: "kubectl", URL: "https://x", SHA256: sumHex(body)}
	b.Fetch = fetchOf(body)
	if _, err := b.fetchClientTool(context.Background(), pin, 1<<20); err == nil || !strings.Contains(err.Error(), "cannot stage") {
		t.Fatalf("error = %v, want the staging refusal", err)
	}
}

// A member that verifies but cannot take its place (a directory holds the name)
// is refused.
func TestFetchedClientToolMemberCannotTakeAPathHeldByADirectory(t *testing.T) {
	t.Parallel()
	b, _ := imageBuilder(t)
	archive := tgzOf(t, map[string]string{"docker/docker": "client"}, []string{"docker/docker"})
	pin := templates.ClientToolPin{Name: "held", URL: "https://x", SHA256: sumHex(archive), Member: "docker/docker"}
	if err := os.MkdirAll(filepath.Join(b.Out, "held", "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	b.Fetch = fetchOf(archive)
	if _, err := b.fetchClientTool(context.Background(), pin, 1<<20); err == nil || !strings.Contains(err.Error(), "cannot write") {
		t.Fatalf("error = %v, want the write refusal", err)
	}
}

func TestFetchClientToolsStopsAtTheFirstFailingTool(t *testing.T) {
	t.Parallel()
	b, _ := imageBuilder(t)
	b.Fetch = fetchOf([]byte("not the pinned bytes"))
	if _, err := b.FetchClientTools(context.Background()); err == nil || !strings.Contains(err.Error(), "not the pinned") {
		t.Fatalf("error = %v, want the first tool's sha256 mismatch", err)
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
	if _, err := b.FetchClientTools(context.Background()); err == nil || !strings.Contains(err.Error(), "is not absolute") {
		t.Errorf("error = %v, want the relative-directory refusal", err)
	}
}

// A pinned archive that is small as a download but expands past the bound is
// refused, and nothing is placed.
func TestFetchedClientToolRefusesAMemberThatExpandsPastTheBound(t *testing.T) {
	t.Parallel()
	b, _ := imageBuilder(t)
	archive := tgzOf(t, map[string]string{"bomb": strings.Repeat("0", 1<<16)}, []string{"bomb"})
	max := int64(len(archive)) + 16
	if max >= 1<<16 {
		t.Fatalf("the archive is %d bytes; the test needs it smaller than the member", len(archive))
	}
	pin := templates.ClientToolPin{Name: "bomb", URL: "https://example.test/bomb.tgz", SHA256: sumHex(archive), Member: "bomb"}
	b.Fetch = fetchOf(archive)
	if _, err := b.fetchClientTool(context.Background(), pin, max); err == nil || !strings.Contains(err.Error(), "does not read") {
		t.Fatalf("error = %v, want the expansion refusal", err)
	}
	if _, err := os.Stat(filepath.Join(b.Out, "bomb")); !os.IsNotExist(err) {
		t.Errorf("a refused archive left a file in the build context (stat error %v)", err)
	}
}
