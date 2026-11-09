package townstate

import (
	"archive/tar"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// truncatedMember is a tar header declaring 100 bytes followed by only 5 of them:
// the stream ends inside the member, as a helper cut off mid-copy leaves it.
func truncatedMember(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "./daemon/cut.json", Mode: 0o644, Size: 100, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("short")); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestTownStateMigrateVerifyFailsOnAListingItCannotRead: a listing that cannot be
// made, or cannot be read, is an error. It is never an empty copy set or a
// listing that compares as equal.
func TestTownStateMigrateVerifyFailsOnAListingItCannotRead(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeStateFile(t, root, "daemon/maintenance_state.json", "x\n", 0o644)
	cases := []struct {
		name   string
		root   string
		helper fakeHelper
		want   string
	}{
		{name: "the host's export of a missing town", root: filepath.Join(t.TempDir(), "absent"), helper: fakeHelper{}, want: "listing the host's copy set"},
		{name: "the volume's listing fails", root: root, helper: fakeHelper{listingErr: errors.New("tar exited 2")}, want: "listing volume gt-state"},
		{name: "the volume's listing is not a tar", root: root, helper: fakeHelper{raw: []byte(strings.Repeat("prose, not a tar header ", 40))}, want: "reading the archive"},
		{name: "a volume member is cut short", root: root, helper: fakeHelper{raw: truncatedMember(t)}, want: "hashing daemon/cut.json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, err := VerifyVolume(tc.helper, "gt-state", tc.root, Default(), ExportOptions{AllowNoRigs: true})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("VerifyVolume = %+v, %v; want an error naming %q", res, err, tc.want)
			}
		})
	}
}

// TestTownStateMigrateVolumeEmptyFailsOnABrokenListing: an unreadable listing is an
// error, never an empty volume that a copy could then write into.
func TestTownStateMigrateVolumeEmptyFailsOnABrokenListing(t *testing.T) {
	t.Parallel()
	empty, err := VolumeEmpty(fakeHelper{raw: []byte(strings.Repeat("not a tar header ", 40))}, "gt-state")
	if err == nil || empty {
		t.Fatalf("VolumeEmpty on a broken listing = %v, %v; want an error", empty, err)
	}
}

// TestTownStateMigrateSurveyRefusals: a survey of the town's metadata refuses what
// the export would refuse, and fails on every metadata read it cannot make.
func TestTownStateMigrateSurveyRefusals(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeStateFile(t, root, "daemon/maintenance_state.json", "x\n", 0o644)
	writeStateFile(t, root, "daemon/nested/notes.json", "x\n", 0o644)

	badRegistry := t.TempDir()
	writeStateFile(t, badRegistry, "mayor/town.json", "[]", 0o644)
	writeStateFile(t, badRegistry, "daemon/maintenance_state.json", "x\n", 0o644)

	failLstat := func(suffix string) func(string) (fs.FileInfo, error) {
		return func(p string) (fs.FileInfo, error) {
			if strings.HasSuffix(p, filepath.FromSlash(suffix)) {
				return nil, errors.New("lstat refused")
			}
			return os.Lstat(p)
		}
	}
	failReadDir := func(suffix string) func(string) ([]string, error) {
		return func(p string) ([]string, error) {
			if strings.HasSuffix(p, filepath.FromSlash(suffix)) {
				return nil, errors.New("readdir refused")
			}
			entries, err := os.ReadDir(p)
			if err != nil {
				return nil, err
			}
			names := make([]string, 0, len(entries))
			for _, e := range entries {
				names = append(names, e.Name())
			}
			return names, nil
		}
	}

	cases := []struct {
		name string
		root string
		m    *Manifest
		opts ExportOptions
		want string
	}{
		{name: "a nil manifest", root: root, m: nil, want: "nil manifest"},
		{name: "a manifest row that names no town path", root: root, m: &Manifest{Entries: []Entry{{Path: "/abs/path", Owner: OwnerDaemon, Why: "test"}}}, opts: ExportOptions{AllowNoRigs: true}, want: "/abs/path"},
		{name: "a missing town", root: filepath.Join(t.TempDir(), "absent"), m: Default(), opts: ExportOptions{AllowNoRigs: true}, want: "reading the town root"},
		{name: "a town with rig rows and no rigs", root: root, m: Default(), want: "the town registers no rigs"},
		{name: "a registry that is not a JSON object", root: badRegistry, m: Default(), opts: ExportOptions{AllowNoRigs: true}, want: "reading the town's rigs"},
		{name: "an ancestor that is a file", root: root, m: &Manifest{Entries: []Entry{{Path: "daemon/maintenance_state.json/x", Owner: OwnerDaemon, Why: "test"}}}, opts: ExportOptions{AllowNoRigs: true}, want: "not a directory"},
		{name: "a row that cannot be stat'ed", root: root, m: Default(), opts: ExportOptions{AllowNoRigs: true, ops: townOps{lstat: failLstat("daemon")}}, want: "reading daemon"},
		{name: "a directory that cannot be listed", root: root, m: Default(), opts: ExportOptions{AllowNoRigs: true, ops: townOps{readDir: failReadDir("daemon")}}, want: "reading daemon"},
		{name: "a child that cannot be stat'ed", root: root, m: Default(), opts: ExportOptions{AllowNoRigs: true, ops: townOps{lstat: failLstat("daemon/maintenance_state.json")}}, want: "reading daemon/maintenance_state.json"},
		{name: "a subdirectory that cannot be listed", root: root, m: Default(), opts: ExportOptions{AllowNoRigs: true, ops: townOps{readDir: failReadDir("daemon/nested")}}, want: "reading daemon/nested"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := Survey(tc.root, tc.m, tc.opts)
			if err == nil {
				t.Fatal("Survey = nil, want a refusal")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Survey = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}
