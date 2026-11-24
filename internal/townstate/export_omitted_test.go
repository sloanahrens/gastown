package townstate

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// omittedToken is token-shaped and built from parts, so no credential literal
// sits in this file.
const omittedToken = "sk-" + "abcdefghijklmnop1234"

// omittedManifest has one row that both the daemon's export and the backup walk,
// so one town is read both ways.
func omittedManifest(t *testing.T) *Manifest {
	t.Helper()
	m := &Manifest{Entries: []Entry{
		{Path: "settings", Owner: OwnerDaemon, Backup: true, Why: "test: a row the daemon export and the backup both walk"},
		{Path: "alpha", Owner: OwnerDaemon, Backup: true, Why: "test: a second row, walked after the first, so the list needs sorting"},
	}}
	if err := m.Validate(); err != nil {
		t.Fatalf("the test manifest: %v", err)
	}
	return m
}

func omittedTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	writeRegistry(t, town)
	writeStateFile(t, town, "settings/clean.json", `{"theme":"dark"}`, 0o644)
	writeStateFile(t, town, "settings/notes.json", `{"note":"run with `+omittedToken+` please"}`, 0o644)
	writeStateFile(t, town, "settings/a-notes.json", `{"note":"also `+omittedToken+`"}`, 0o644)
	writeStateFile(t, town, "alpha/x.json", `{"note":"`+omittedToken+`"}`, 0o644)
	return town
}

// TestExportBackupOmitsAndListsAFlaggedFile: a backup export leaves out a file whose
// content FlaggedContent flags, counts it under the secret class, lists its name
// in Omitted (sorted) and writes nothing of it; a clean file is written; the
// summary prints the count and no name; a non-backup export of the same town
// writes both files.
func TestExportBackupOmitsAndListsAFlaggedFile(t *testing.T) {
	t.Parallel()
	town := omittedTown(t)

	var backup bytes.Buffer
	sum, err := Export(&backup, town, omittedManifest(t), ExportOptions{Backup: true})
	if err != nil {
		t.Fatalf("Export with Backup: %v", err)
	}
	names := archiveNames(t, backup.Bytes())
	if !slices.Contains(names, "settings/clean.json") || slices.Contains(names, "settings/notes.json") || slices.Contains(names, "settings/a-notes.json") {
		t.Errorf("backup archive = %v, want the clean file and not the flagged one", names)
	}
	if !slices.Equal(sum.Omitted, []string{"alpha/x.json", "settings/a-notes.json", "settings/notes.json"}) || sum.Skipped[SkipSecret] != 3 {
		t.Errorf("Omitted = %v, SkipSecret = %d; want both flagged files listed in order and counted", sum.Omitted, sum.Skipped[SkipSecret])
	}
	line := sum.String()
	if !strings.Contains(line, "omitted 3 files whose content is flagged") || strings.Contains(line, "notes.json") || strings.Contains(line, omittedToken) {
		t.Errorf("summary = %q, want the omitted count and no name or value", line)
	}
	if bytes.Contains(backup.Bytes(), []byte(omittedToken)) {
		t.Error("the backup archive holds the credential")
	}

	var plain bytes.Buffer
	if _, err := Export(&plain, town, omittedManifest(t), ExportOptions{}); err != nil {
		t.Fatalf("Export without Backup: %v", err)
	}
	if got := archiveNames(t, plain.Bytes()); !slices.Contains(got, "settings/notes.json") || !slices.Contains(got, "settings/a-notes.json") || !slices.Contains(got, "settings/clean.json") {
		t.Errorf("non-backup archive = %v, want both files", got)
	}
}

// TestExportSummaryPrintsTheOmittedLine: the omitted line says the content is
// flagged, not that it holds a credential, with the singular noun for one file,
// and it never names a file.
func TestExportSummaryPrintsTheOmittedLine(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		omitted []string
		want    string
	}{
		{name: "one file", omitted: []string{"settings/notes.json"}, want: "omitted 1 file whose content is flagged"},
		{name: "three files", omitted: []string{"alpha/x.json", "settings/a-notes.json", "settings/notes.json"}, want: "omitted 3 files whose content is flagged"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			line := ExportSummary{Omitted: c.omitted}.String()
			if !strings.Contains(line, c.want) {
				t.Errorf("summary = %q, want %q", line, c.want)
			}
			for _, name := range c.omitted {
				if strings.Contains(line, name) {
					t.Errorf("summary = %q, names %s; want no name", line, name)
				}
			}
			if strings.Contains(line, "credential") {
				t.Errorf("summary = %q, claims a credential", line)
			}
		})
	}
}

// TestExportBackupFailsOnAFileItCannotRead: a file whose content cannot be read to
// check is neither written nor silently left out: the export fails naming the
// file and no value.
func TestExportBackupFailsOnAFileItCannotRead(t *testing.T) {
	t.Parallel()
	town := omittedTown(t)
	failing := func(o *townOps) {
		real := osOps().open
		opens := 0
		o.open = func(p string) (io.ReadCloser, error) {
			// The first read fails and a second would succeed: an unchecked copy
			// after the failed check would pass the export, so only the error stops it.
			if strings.HasSuffix(filepath.ToSlash(p), "/settings/clean.json") {
				if opens++; opens == 1 {
					return io.NopCloser(refuseOmittedReader{}), nil
				}
			}
			return real(p)
		}
	}

	_, err := Export(io.Discard, town, omittedManifest(t), ExportOptions{Backup: true, ops: opsWith(failing)})

	if err == nil || !strings.Contains(err.Error(), "settings/clean.json") || strings.Contains(err.Error(), omittedToken) {
		t.Fatalf("Export error = %v, want it to name settings/clean.json and hold no value", err)
	}
}

type refuseOmittedReader struct{}

func (refuseOmittedReader) Read([]byte) (int, error) {
	return 0, errors.New("the disk refused the read")
}

// TestExportBackupWritesTheBytesItChecked: the file is read once and the
// checked bytes are the ones archived. A file that is clean at the check and holds a
// gzip-nested credential at any later read cannot reach the archive, and a file
// that grew after the stat is archived at the checked length.
func TestExportBackupWritesTheBytesItChecked(t *testing.T) {
	t.Parallel()
	town := omittedTown(t)
	var nested bytes.Buffer
	outer := gzip.NewWriter(&nested)
	inner := gzip.NewWriter(outer)
	_, _ = inner.Write([]byte(`{"note":"` + omittedToken + `"}`))
	_ = inner.Close()
	_ = outer.Close()
	// The first read is longer than the file's stat size, as if it grew after the stat.
	first := `{"theme":"dark"}` + strings.Repeat(" ", 32)
	later := append(append([]byte{}, nested.Bytes()...), make([]byte, 64)...)
	changing := func(o *townOps) {
		real := osOps().open
		opens := 0
		o.open = func(p string) (io.ReadCloser, error) {
			if strings.HasSuffix(filepath.ToSlash(p), "/settings/clean.json") {
				if opens++; opens == 1 {
					return io.NopCloser(strings.NewReader(first)), nil
				}
				return io.NopCloser(bytes.NewReader(later)), nil
			}
			return real(p)
		}
	}

	var stream bytes.Buffer
	if _, err := Export(&stream, town, omittedManifest(t), ExportOptions{Backup: true, ops: opsWith(changing)}); err != nil {
		t.Fatalf("Export: %v", err)
	}

	for _, e := range readArchive(t, stream.Bytes()) {
		if e.hdr.Name == "settings/clean.json" && string(e.data) != first {
			t.Fatalf("archived %q, want the checked bytes %q", e.data, first)
		}
	}
	if bytes.Contains(stream.Bytes(), nested.Bytes()) {
		t.Fatal("the archive holds the bytes of a later read")
	}
}

// TestExportBackupReadsAKnownSizeIntoOneBuffer: a file the walk statted is read
// into a first buffer of its stat size plus one byte, so a large file is not
// copied through io.ReadAll's doubling.
func TestExportBackupReadsAKnownSizeIntoOneBuffer(t *testing.T) {
	t.Parallel()
	town := omittedTown(t)
	const size = 8192
	content := strings.Repeat("a", size)
	writeStateFile(t, town, "settings/clean.json", content, 0o644)

	reads := &recordReadBuffers{reader: strings.NewReader(content)}
	open := func(o *townOps) {
		real := osOps().open
		o.open = func(p string) (io.ReadCloser, error) {
			if strings.HasSuffix(filepath.ToSlash(p), "/settings/clean.json") {
				return io.NopCloser(reads), nil
			}
			return real(p)
		}
	}

	var stream bytes.Buffer
	if _, err := Export(&stream, town, omittedManifest(t), ExportOptions{Backup: true, ops: opsWith(open)}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(reads.lengths) == 0 || reads.lengths[0] != size+1 {
		t.Fatalf("read buffer lengths = %v, want the first to be %d", reads.lengths, size+1)
	}
	for _, n := range reads.lengths {
		if n > size+1 {
			t.Fatalf("read buffer lengths = %v, want none over %d, so the read never grew", reads.lengths, size+1)
		}
	}
	for _, e := range readArchive(t, stream.Bytes()) {
		if e.hdr.Name == "settings/clean.json" && string(e.data) != content {
			t.Fatalf("archived %q, want the file's content", e.data)
		}
	}
}

// recordReadBuffers records the length of each buffer handed to Read.
type recordReadBuffers struct {
	reader  io.Reader
	lengths []int
}

func (r *recordReadBuffers) Read(p []byte) (int, error) {
	r.lengths = append(r.lengths, len(p))
	return r.reader.Read(p)
}

// TestExportBackupOmitsAFileOverTheBound: a file larger than maxFlaggedBytes is
// left out and listed, never streamed unchecked.
func TestExportBackupOmitsAFileOverTheBound(t *testing.T) {
	t.Parallel()
	town := omittedTown(t)
	big := func(o *townOps) {
		real := osOps().open
		o.open = func(p string) (io.ReadCloser, error) {
			if strings.HasSuffix(filepath.ToSlash(p), "/settings/clean.json") {
				return io.NopCloser(io.LimitReader(zeroReader{}, maxFlaggedBytes+1)), nil
			}
			return real(p)
		}
	}

	var stream bytes.Buffer
	sum, err := Export(&stream, town, omittedManifest(t), ExportOptions{Backup: true, ops: opsWith(big)})

	if err != nil || !slices.Contains(sum.Omitted, "settings/clean.json") || slices.Contains(archiveNames(t, stream.Bytes()), "settings/clean.json") {
		t.Fatalf("Export = %v, omitted %v: want the oversize file omitted and listed", err, sum.Omitted)
	}
}
