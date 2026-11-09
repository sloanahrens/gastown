package townstate

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHelper is an Engine whose helper answers one canned stdout, and for an
// import either reads the stream or returns without reading it.
type fakeHelper struct {
	stdout     string
	err        error
	skipStdin  bool
	listing    []string
	listingErr error
	// raw, when set, is the listing's stdout in place of a tar of listing.
	raw []byte
}

func (f fakeHelper) Run(_ string, argv []string, stdin io.Reader, stdout io.Writer) error {
	switch strings.Join(argv, " ") {
	case strings.Join(ImportArgv, " "):
		if !f.skipStdin {
			if _, err := io.Copy(io.Discard, stdin); err != nil {
				return err
			}
		}
		return f.err
	case strings.Join(ListArgv, " "):
		if f.raw != nil {
			if _, err := stdout.Write(f.raw); err != nil {
				return err
			}
			return f.listingErr
		}
		tw := tar.NewWriter(stdout)
		for _, name := range f.listing {
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
				return err
			}
		}
		if err := tw.Close(); err != nil {
			return err
		}
		return f.listingErr
	}
	if _, err := io.WriteString(stdout, f.stdout); err != nil {
		return err
	}
	return f.err
}

func (f fakeHelper) HasVolume(string) (bool, error) { return true, nil }

func (f fakeHelper) CreateVolume(string) error { return nil }

// TestTownStateMigrateFreeBytesReadsDfAvailable: the free space is the available
// column of the volume's df line, in bytes; a df that does not give one is an
// error, never zero bytes.
func TestTownStateMigrateFreeBytesReadsDfAvailable(t *testing.T) {
	t.Parallel()
	const header = "Filesystem 1024-blocks Used Available Capacity Mounted on\n"
	cases := []struct {
		name    string
		helper  fakeHelper
		want    int64
		wantErr bool
	}{
		{name: "available column", helper: fakeHelper{stdout: header + "vol 1000 0 2048 0% /state\n"}, want: 2048 * 1024},
		{name: "no volume line", helper: fakeHelper{stdout: header}, wantErr: true},
		{name: "short volume line", helper: fakeHelper{stdout: header + "vol 1000\n"}, wantErr: true},
		{name: "available is not a count", helper: fakeHelper{stdout: header + "vol 1000 0 lots 0% /state\n"}, wantErr: true},
		{name: "helper fails", helper: fakeHelper{err: errors.New("df exited 1")}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := FreeBytes(tc.helper, "gt-state")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("FreeBytes = %d, want an error", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("FreeBytes = %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

// TestTownStateMigrateVolumeEmptyAnswers: a volume holding only its root is
// empty; one holding any entry below the root is not; a listing that fails is an
// error, not an empty volume.
func TestTownStateMigrateVolumeEmptyAnswers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		helper  fakeHelper
		want    bool
		wantErr bool
	}{
		{name: "root only", helper: fakeHelper{listing: []string{"./"}}, want: true},
		{name: "one entry below the root", helper: fakeHelper{listing: []string{"./", "./daemon/"}}, want: false},
		{name: "listing fails", helper: fakeHelper{listingErr: errors.New("tar exited 2")}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := VolumeEmpty(tc.helper, "gt-state")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("VolumeEmpty = %v, want an error", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("VolumeEmpty = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

// TestTownStateMigrateSeedReportsItsFailures: a seed names the volume as partial
// for each way it can end short, and names the export as the cause when the
// export is.
func TestTownStateMigrateSeedReportsItsFailures(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeStateFile(t, root, "daemon/maintenance_state.json", "x\n", 0o644)
	cases := []struct {
		name   string
		root   string
		helper fakeHelper
		want   string
	}{
		{name: "export of a missing town", root: filepath.Join(t.TempDir(), "absent"), helper: fakeHelper{}, want: "the export failed"},
		{name: "helper returns without reading", root: root, helper: fakeHelper{skipStdin: true}, want: "ended the stream early"},
		{name: "helper fails", root: root, helper: fakeHelper{err: errors.New("import exited 1")}, want: "the import helper failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := SeedVolume(tc.helper, "gt-state", tc.root, Default(), ExportOptions{AllowNoRigs: true}, ImportArgv, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) ||
				!strings.Contains(err.Error(), "holds a partial copy and is not to be used") {
				t.Fatalf("SeedVolume = %v, want a partial-copy refusal naming %q", err, tc.want)
			}
		})
	}
}

// TestTownStateMigrateExportCountsASecretWithoutNamingIt: an excluded file is not
// written, is counted under SkipSecret, and its name appears in no summary line.
func TestTownStateMigrateExportCountsASecretWithoutNamingIt(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeStateFile(t, root, "daemon/maintenance_state.json", "keep\n", 0o644)
	writeStateFile(t, root, "daemon/forgejo-landing.env", "SECRET\n", 0o600)
	exclude := func(name string) bool { return filepath.Base(name) == "forgejo-landing.env" }

	var stream bytes.Buffer
	sum, err := Export(&stream, root, Default(), ExportOptions{AllowNoRigs: true, Exclude: exclude})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if sum.Skipped[SkipSecret] != 1 {
		t.Errorf("Skipped[SkipSecret] = %d, want 1", sum.Skipped[SkipSecret])
	}
	if strings.Contains(sum.String(), "forgejo-landing.env") || strings.Contains(sum.String(), "SECRET") {
		t.Errorf("the summary names or shows the secret: %s", sum.String())
	}
	tr := tar.NewReader(&stream)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("reading the archive: %v", err)
		}
		if strings.HasSuffix(hdr.Name, "forgejo-landing.env") {
			t.Fatalf("the archive carries the secret %q", hdr.Name)
		}
	}
}
