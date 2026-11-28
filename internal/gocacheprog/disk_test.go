package gocacheprog

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

var diskNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newDisk(t *testing.T) (*Disk, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "store")
	d, err := NewDisk(dir, func() time.Time { return diskNow })
	if err != nil {
		t.Fatalf("NewDisk: %v", err)
	}
	return d, dir
}

func writeDiskFile(t *testing.T, path string, data []byte) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(t, os.WriteFile(path, data, 0o644))
}

func TestDiskPutGet(t *testing.T) {
	t.Parallel()
	d, _ := newDisk(t)
	path, err := d.Put([]byte("aa"), []byte("bb"), []byte("hello"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("body = %q, want hello", body)
	}
	got, err := d.Get([]byte("aa"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := Entry{OutputID: []byte("bb"), Size: 5, Time: diskNow, DiskPath: path}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Get = %+v, want %+v", got, want)
	}
}

func TestDiskPutReusesBody(t *testing.T) {
	t.Parallel()
	d, _ := newDisk(t)
	first, err := d.Put([]byte("aa"), []byte("bb"), []byte("hello"))
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	stamp := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	must(t, os.Chtimes(first, stamp, stamp))
	second, err := d.Put([]byte("cc"), []byte("bb"), []byte("hello"))
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}
	fi, err := os.Stat(first)
	must(t, err)
	if second != first || !fi.ModTime().Equal(stamp) {
		t.Errorf("second Put = %q at %v, want reuse of %q untouched at %v", second, fi.ModTime(), first, stamp)
	}
}

func TestDisk(t *testing.T) {
	t.Parallel()
	const index = `{"OutputID":"YmI=","Size":3}`
	get := func(id string) func(*Disk) error {
		return func(d *Disk) error { _, err := d.Get([]byte(id)); return err }
	}
	put := func(action, output string) func(*Disk) error {
		return func(d *Disk) error { _, err := d.Put([]byte(action), []byte(output), []byte("body")); return err }
	}
	write := func(rel, data string) func(*testing.T, string) {
		return func(t *testing.T, dir string) { writeDiskFile(t, filepath.Join(dir, rel), []byte(data)) }
	}
	mkdir := func(rel string) func(*testing.T, string) {
		return func(t *testing.T, dir string) { must(t, os.MkdirAll(filepath.Join(dir, rel), 0o755)) }
	}
	bodyUnderFile := func(t *testing.T, dir string) {
		writeDiskFile(t, filepath.Join(dir, "aa/aa-a"), []byte(index))
		writeDiskFile(t, filepath.Join(dir, "bb"), nil)
	}
	tests := []struct {
		name  string
		setup func(*testing.T, string)
		call  func(*Disk) error
		miss  bool
	}{
		{"absent index", nil, get("aa"), true},
		{"undecodable index", write("aa/aa-a", "{"), get("aa"), true},
		{"index names a bad id", write("aa/aa-a", `{"Size":3}`), get("aa"), true},
		{"absent body", write("aa/aa-a", index), get("aa"), true},
		{"wrong-sized body", func(t *testing.T, dir string) {
			writeDiskFile(t, filepath.Join(dir, "aa/aa-a"), []byte(index))
			writeDiskFile(t, filepath.Join(dir, "bb/bb-d"), []byte("body!"))
		}, get("aa"), true},
		{"get with an empty id", nil, get(""), false},
		{"get with a non-hex id", nil, get("zz"), false},
		{"put with a non-hex output id", nil, put("aa", "zz"), false},
		{"put with an empty action id", nil, put("", "bb"), false},
		{"unreadable index", mkdir("aa/aa-a"), get("aa"), false},
		{"get: body stat fails", bodyUnderFile, get("aa"), false},
		{"put: body stat fails", bodyUnderFile, put("aa", "bb"), false},
		{"put: shard cannot be created", func(t *testing.T, dir string) {
			must(t, os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(dir, "bb")))
		}, put("aa", "bb"), false},
		{"put: body cannot be written", mkdir("bb/bb-d"), put("aa", "bb"), false},
		{"put: index cannot be written", mkdir("aa/aa-a"), put("aa", "bb"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d, dir := newDisk(t)
			if tt.setup != nil {
				tt.setup(t, dir)
			}
			err := tt.call(d)
			if tt.miss && !errors.Is(err, ErrMiss) {
				t.Errorf("call = %v, want ErrMiss", err)
			}
			if !tt.miss && err == nil {
				t.Error("call succeeded, want an error")
			}
		})
	}
}

func TestDiskNewDiskFails(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "file")
	writeDiskFile(t, file, nil)
	if _, err := NewDisk(filepath.Join(file, "store"), func() time.Time { return diskNow }); err == nil {
		t.Error("NewDisk under a file succeeded, want an error")
	}
}
