package forgejoprobe

import (
	"errors"
	"io/fs"
	"strconv"
	"strings"
	"testing"
	"time"
)

// recordEntry is one file the in-memory FS holds: its content and its mode.
type recordEntry struct {
	data string
	mode fs.FileMode
}

// recordFS is an in-memory FS. A step named in fail returns its error, so a
// test makes any one step fail. It keeps every file, temporary or stored, so a
// test sees the record and the absence of a temporary file.
type recordFS struct {
	files map[string]*recordEntry
	made  []string
	fail  map[string]error
}

func newRecordFS() *recordFS {
	return &recordFS{files: map[string]*recordEntry{}, fail: map[string]error{}}
}

func (f *recordFS) MkdirAll(path string, perm fs.FileMode) error {
	if err := f.fail["mkdir"]; err != nil {
		return err
	}
	f.made = append(f.made, path)
	return nil
}

func (f *recordFS) CreateTemp(dir, pattern string) (TempFile, error) {
	if err := f.fail["create"]; err != nil {
		return nil, err
	}
	name := dir + "/" + strings.Replace(pattern, "*", "424242", 1)
	e := &recordEntry{}
	f.files[name] = e
	return &recordTemp{fsys: f, name: name, entry: e}, nil
}

func (f *recordFS) Rename(oldpath, newpath string) error {
	if err := f.fail["rename"]; err != nil {
		return err
	}
	e, ok := f.files[oldpath]
	if !ok {
		return fs.ErrNotExist
	}
	delete(f.files, oldpath)
	f.files[newpath] = e
	return nil
}

func (f *recordFS) Remove(name string) error {
	if err := f.fail["remove"]; err != nil {
		return err
	}
	if _, ok := f.files[name]; !ok {
		return fs.ErrNotExist
	}
	delete(f.files, name)
	return nil
}

// recordTemp is a temporary file of recordFS. Its steps fail as the fail map
// names them.
type recordTemp struct {
	fsys  *recordFS
	name  string
	entry *recordEntry
}

func (t *recordTemp) Name() string { return t.name }

func (t *recordTemp) Chmod(mode fs.FileMode) error {
	if err := t.fsys.fail["chmod"]; err != nil {
		return err
	}
	t.entry.mode = mode
	return nil
}

func (t *recordTemp) Write(p []byte) (int, error) {
	if err := t.fsys.fail["write"]; err != nil {
		return 0, err
	}
	t.entry.data += string(p)
	return len(p), nil
}

func (t *recordTemp) Close() error { return t.fsys.fail["close"] }

// recordWhen is the instant the fake clock reads for the record tests.
var recordWhen = time.Date(2026, 10, 30, 2, 51, 25, 0, time.UTC)

// writeTestRecord writes the green record of the gastown probe under configDir
// "cfg", with the fake clock reading recordWhen.
func writeTestRecord(fsys FS) (string, error) {
	return WriteRecord(fsys, &fakeClock{now: recordWhen}, "cfg", "gastown", "acme/gastown", testSHA, "forgejo/gastown")
}

func TestWriteRecordContent(t *testing.T) {
	t.Parallel()
	fsys := newRecordFS()
	path, err := writeTestRecord(fsys)
	if err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	if want := "cfg/probe-gastown.record"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	want := "rig=gastown\n" +
		"repo=acme/gastown\n" +
		"commit=" + testSHA + "\n" +
		"context=forgejo/gastown\n" +
		"verdict=green\n" +
		"time=2026-10-30T02:51:25Z\n" +
		"epoch=" + strconv.FormatInt(recordWhen.Unix(), 10) + "\n"
	if len(fsys.files) != 1 {
		t.Fatalf("files after a successful write = %d, want only the record; the temporary file must be renamed away", len(fsys.files))
	}
	got, ok := fsys.files[path]
	if !ok {
		t.Fatalf("no file at %s", path)
	}
	if got.data != want {
		t.Errorf("record content =\n%q\nwant\n%q", got.data, want)
	}
	if got.mode != 0o600 {
		t.Errorf("record mode = %o, want 600", got.mode)
	}
}

func TestWriteRecordFailures(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	for _, tc := range []struct {
		step string
		want string
	}{
		{"mkdir", "could not create cfg for the probe record"},
		{"create", "could not create a probe record under cfg"},
		{"chmod", "could not write the probe record under cfg"},
		{"write", "could not write the probe record under cfg"},
		{"close", "could not write the probe record under cfg"},
		{"rename", "could not store the probe record at cfg/probe-gastown.record"},
	} {
		t.Run(tc.step, func(t *testing.T) {
			fsys := newRecordFS()
			fsys.fail[tc.step] = boom
			path, err := writeTestRecord(fsys)
			if err == nil {
				t.Fatalf("WriteRecord returned %q with no error, want the %s failure", path, tc.step)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
			if !errors.Is(err, boom) {
				t.Errorf("error = %q does not wrap the step's own error", err)
			}
			if len(fsys.files) != 0 {
				t.Errorf("files after a failed %s = %d, want none: the temporary file must be removed and no record stored", tc.step, len(fsys.files))
			}
		})
	}
}

// TestWriteRecordNamesAFailedTempRemoval makes the rename fail and the removal of
// the temporary file fail too: the error names the file the removal left behind.
func TestWriteRecordNamesAFailedTempRemoval(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	fsys := newRecordFS()
	fsys.fail["rename"] = boom
	fsys.fail["remove"] = boom
	_, err := writeTestRecord(fsys)
	if err == nil {
		t.Fatal("WriteRecord returned no error with the rename and the removal failing")
	}
	if want := "could not remove the temporary probe record cfg/.probe-gastown.424242"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to name the temporary file: %q", err, want)
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %q does not wrap the failing step's own error", err)
	}
}

// TestWriteRecordRigRule holds the rig to the rule the command line applies:
// a rig it would refuse writes and makes nothing.
func TestWriteRecordRigRule(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		rig string
		ok  bool
	}{
		{rig: "good-rig", ok: true},
		{rig: "a/b"},
		{rig: ".."},
		{rig: "."},
		{rig: ""},
		{rig: `a\b`},
	} {
		t.Run(tc.rig, func(t *testing.T) {
			t.Parallel()
			fsys := newRecordFS()
			path, err := WriteRecord(fsys, &fakeClock{now: recordWhen}, "cfg", tc.rig, "acme/x", testSHA, "ctx")
			if tc.ok {
				if err != nil {
					t.Fatalf("WriteRecord(%q): %v", tc.rig, err)
				}
				if want := "cfg/probe-" + tc.rig + ".record"; path != want {
					t.Errorf("path = %q, want %q", path, want)
				}
				return
			}
			if err == nil {
				t.Fatalf("WriteRecord(%q) returned %q with no error", tc.rig, path)
			}
			if len(fsys.made) != 0 || len(fsys.files) != 0 {
				t.Errorf("an invalid rig made %v and left %d files; it must write and make nothing", fsys.made, len(fsys.files))
			}
		})
	}
}

func TestWriteRecordRemoveRecord(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	for _, tc := range []struct {
		name    string
		present bool
		rig     string
		fail    bool
		wantErr bool
		removed bool
	}{
		{name: "a stale record is removed", present: true, rig: "gastown", removed: true},
		{name: "an absent record is success", rig: "gastown"},
		{name: "a failed removal is an error that names the path", present: true, rig: "gastown", fail: true, wantErr: true},
		{name: "an invalid rig removes nothing", present: true, rig: "a/b", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fsys := newRecordFS()
			if tc.present {
				fsys.files[finishRecord] = &recordEntry{data: staleRecord, mode: 0o600}
			}
			if tc.fail {
				fsys.fail["remove"] = boom
			}
			err := RemoveRecord(fsys, "cfg", tc.rig)
			if (err != nil) != tc.wantErr {
				t.Fatalf("RemoveRecord error = %v, want error %v", err, tc.wantErr)
			}
			if tc.fail && (!strings.Contains(err.Error(), finishRecord) || !errors.Is(err, boom)) {
				t.Errorf("error = %q, want it to name %s and wrap the removal's error", err, finishRecord)
			}
			_, stillThere := fsys.files[finishRecord]
			if stillThere != (tc.present && !tc.removed) {
				t.Errorf("record still present = %v, want %v", stillThere, tc.present && !tc.removed)
			}
		})
	}
}
