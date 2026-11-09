package townstate

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// listingEngine is an Engine whose helper writes a canned listing for ListArgv,
// so a test chooses each entry type the volume holds.
type listingEngine struct {
	listing []tar.Header
}

func (f listingEngine) Run(_ string, argv []string, _ io.Reader, stdout io.Writer) error {
	if strings.Join(argv, " ") != strings.Join(ListArgv, " ") {
		return nil
	}
	tw := tar.NewWriter(stdout)
	for i := range f.listing {
		if err := tw.WriteHeader(&f.listing[i]); err != nil {
			return err
		}
		if f.listing[i].Typeflag == tar.TypeReg {
			if _, err := io.Copy(tw, bytes.NewReader([]byte(regBody(f.listing[i].Name)))); err != nil {
				return err
			}
		}
	}
	return tw.Close()
}

func (f listingEngine) HasVolume(string) (bool, error) { return true, nil }

func (f listingEngine) CreateVolume(string) error { return nil }

// regBody is the content every regular file in the listing carries: the same
// bytes the host town holds, so a clean listing matches.
func regBody(name string) string {
	return "body of " + filepath.Base(name) + "\n"
}

// deltaTown builds a town with one daemon file and one daemon link into it.
func deltaTown(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeStateFile(t, root, "daemon/maintenance_state.json", regBody("daemon/maintenance_state.json"), 0o644)
	if err := os.Symlink("maintenance_state.json", filepath.Join(root, "daemon", "link")); err != nil {
		t.Fatalf("creating the daemon link: %v", err)
	}
	return root
}

// cleanListing is the volume listing a correct copy of deltaTown gives.
func cleanListing() []tar.Header {
	return []tar.Header{
		{Name: "daemon/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "daemon/maintenance_state.json", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(regBody("daemon/maintenance_state.json")))},
		{Name: "daemon/link", Typeflag: tar.TypeSymlink, Linkname: "maintenance_state.json", Mode: 0o777},
	}
}

// TestTownStateMigrateVerifyComparesEveryEntryType: verify compares a link by its
// target, names a volume entry that is neither a file, a directory nor a link,
// and counts a secret-named link of any type without naming it.
func TestTownStateMigrateVerifyComparesEveryEntryType(t *testing.T) {
	t.Parallel()
	exclude := func(name string) bool { return strings.HasSuffix(name, "secret.env") }
	opts := ExportOptions{AllowNoRigs: true, Exclude: exclude}

	cases := []struct {
		name    string
		listing []tar.Header
		want    []Mismatch
		secrets int
	}{
		{name: "clean", listing: cleanListing()},
		{name: "link retargeted", listing: func() []tar.Header {
			l := cleanListing()
			l[2].Linkname = "elsewhere.json"
			return l
		}(), want: []Mismatch{{Path: "daemon/link", Reason: ReasonChanged}}},
		{name: "fifo on the volume", listing: append(cleanListing(), tar.Header{Name: "daemon/pipe", Typeflag: tar.TypeFifo, Mode: 0o600}),
			want: []Mismatch{{Path: "daemon/pipe", Reason: ReasonNotRegular}}},
		{name: "secret link of any type", listing: append(cleanListing(), tar.Header{Name: "daemon/secret.env", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}),
			secrets: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, err := VerifyVolume(listingEngine{listing: tc.listing}, "gt-state", deltaTown(t), Default(), opts)
			if err != nil {
				t.Fatalf("VerifyVolume: %v", err)
			}
			if !sameMismatches(res.Mismatches, tc.want) {
				t.Errorf("mismatches = %+v, want %+v", res.Mismatches, tc.want)
			}
			if res.SecretsOnVolume != tc.secrets {
				t.Errorf("SecretsOnVolume = %d, want %d", res.SecretsOnVolume, tc.secrets)
			}
			for _, m := range res.Mismatches {
				if strings.Contains(m.Path, "secret") {
					t.Errorf("a secret's name is listed: %+v", m)
				}
			}
		})
	}
}

// sameMismatches reports whether got and want hold the same entries.
func sameMismatches(got, want []Mismatch) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestTownStateMigrateExportAndSurveyKeepOnlyTheDelta: a delta export writes the
// files its Only selects, still writes the directories, and counts the rest under
// SkipUnchanged; a survey of the same town reads the same way.
func TestTownStateMigrateExportAndSurveyKeepOnlyTheDelta(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeStateFile(t, root, "daemon/maintenance_state.json", "a\n", 0o644)
	writeStateFile(t, root, "daemon/other.json", "b\n", 0o644)
	only := func(name string) bool { return name == "daemon/other.json" }
	opts := ExportOptions{AllowNoRigs: true, Only: only}

	var buf bytes.Buffer
	sum, err := Export(&buf, root, Default(), opts)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if sum.Files != 1 || sum.Skipped[SkipUnchanged] != 1 {
		t.Fatalf("delta export: files %d, unchanged %d; want 1 and 1", sum.Files, sum.Skipped[SkipUnchanged])
	}
	names := tarNames(t, buf.Bytes())
	if !contains(names, "daemon/other.json") || contains(names, "daemon/maintenance_state.json") {
		t.Errorf("delta archive holds %v; want only daemon/other.json among files", names)
	}
	if !contains(names, "daemon/") {
		t.Errorf("delta archive lacks the directory it writes into: %v", names)
	}

	inv, err := Survey(root, Default(), ExportOptions{AllowNoRigs: true})
	if err != nil {
		t.Fatalf("Survey: %v", err)
	}
	if inv.Files != 2 {
		t.Errorf("survey counts %d files, want the full set of 2", inv.Files)
	}
}

// tarNames lists the member names of a tar stream.
func tarNames(t *testing.T, data []byte) []string {
	t.Helper()
	var names []string
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatalf("reading the archive: %v", err)
		}
		names = append(names, hdr.Name)
	}
}

func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
