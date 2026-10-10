package atomicfile

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// onlyEntry fails unless dir holds exactly the named entry.
func onlyEntry(t *testing.T, dir, name string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	if len(entries) != 1 || entries[0].Name() != name {
		t.Errorf("dir holds %v, want only %q (a temp file survived)", names, name)
	}
}

func TestWriteJSON(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.json")

	data := map[string]string{"key": "value"}
	if err := WriteJSON(testFile, data); err != nil {
		t.Fatalf("WriteJSON error: %v", err)
	}

	if _, err := os.Stat(testFile); os.IsNotExist(err) {
		t.Fatal("File was not created")
	}

	entries, _ := os.ReadDir(tmpDir)
	for _, e := range entries {
		if e.Name() != "test.json" {
			t.Fatalf("Temp file was not cleaned up: %s", e.Name())
		}
	}

	content, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	if string(content) != "{\n  \"key\": \"value\"\n}" {
		t.Fatalf("Unexpected content: %s", content)
	}
}

func TestWriteFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")

	data := []byte("hello world")
	if err := WriteFile(testFile, data, 0644); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}

	content, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	if string(content) != "hello world" {
		t.Fatalf("Unexpected content: %s", content)
	}

	entries, _ := os.ReadDir(tmpDir)
	for _, e := range entries {
		if e.Name() != "test.txt" {
			t.Fatalf("Temp file was not cleaned up: %s", e.Name())
		}
	}
}

func TestWriteOverwrite(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.json")

	if err := WriteJSON(testFile, "first"); err != nil {
		t.Fatalf("First write error: %v", err)
	}

	if err := WriteJSON(testFile, "second"); err != nil {
		t.Fatalf("Second write error: %v", err)
	}

	content, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	if string(content) != "\"second\"" {
		t.Fatalf("Unexpected content: %s", content)
	}
}

func TestWriteFilePermissions(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")

	data := []byte("test data")
	if err := WriteFile(testFile, data, 0600); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}

	info, err := os.Stat(testFile)
	if err != nil {
		t.Fatalf("Stat error: %v", err)
	}
	perm := info.Mode().Perm()
	if perm&0600 != 0600 {
		t.Errorf("Expected owner read/write permissions, got %o", perm)
	}
}

func TestWriteFileEmpty(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "empty.txt")

	if err := WriteFile(testFile, []byte{}, 0644); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}

	content, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	if len(content) != 0 {
		t.Fatalf("Expected empty file, got %d bytes", len(content))
	}
}

func TestWriteJSONTypes(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	tests := []struct {
		name     string
		data     interface{}
		expected string
	}{
		{"string", "hello", `"hello"`},
		{"int", 42, "42"},
		{"float", 3.14, "3.14"},
		{"bool", true, "true"},
		{"null", nil, "null"},
		{"array", []int{1, 2, 3}, "[\n  1,\n  2,\n  3\n]"},
		{"nested", map[string]interface{}{"a": map[string]int{"b": 1}}, "{\n  \"a\": {\n    \"b\": 1\n  }\n}"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			testFile := filepath.Join(tmpDir, tc.name+".json")
			if err := WriteJSON(testFile, tc.data); err != nil {
				t.Fatalf("WriteJSON error: %v", err)
			}

			content, err := os.ReadFile(testFile)
			if err != nil {
				t.Fatalf("ReadFile error: %v", err)
			}
			if string(content) != tc.expected {
				t.Errorf("Expected %q, got %q", tc.expected, string(content))
			}
		})
	}
}

func TestWriteJSONUnmarshallable(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "unmarshallable.json")

	ch := make(chan int)
	err := WriteJSON(testFile, ch)
	if err == nil {
		t.Fatal("Expected error for unmarshallable type")
	}

	if _, statErr := os.Stat(testFile); !os.IsNotExist(statErr) {
		t.Fatal("File should not exist after marshal error")
	}

	entries, _ := os.ReadDir(tmpDir)
	for _, e := range entries {
		t.Fatalf("Unexpected file after marshal error: %s", e.Name())
	}
}

// TestWriteFileMissingDir: when the temp file cannot be created (here the
// target directory does not exist), WriteFile fails and creates nothing.
func TestWriteFileMissingDir(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "missing", "test.txt")

	if err := WriteFile(testFile, []byte("test"), 0644); err == nil {
		t.Fatal("Expected an error for a missing directory")
	}
	if _, statErr := os.Stat(testFile); !os.IsNotExist(statErr) {
		t.Fatal("File should not exist after the failed write")
	}
}

// TestWriteFileRenameFailureCleansUp: when the final rename fails (the target
// is a non-empty directory), the temp file is removed and the target is left
// as it was.
func TestWriteFileRenameFailureCleansUp(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "target")
	if err := os.MkdirAll(filepath.Join(target, "occupant"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := WriteFile(target, []byte("data"), 0644); err == nil {
		t.Fatal("Expected an error renaming over a non-empty directory")
	}
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "target" || !entries[0].IsDir() {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("dir after failed rename = %v, want only the untouched target directory", names)
	}
}

func TestWriteFileConcurrent(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "concurrent.txt")

	if err := WriteFile(testFile, []byte("initial"), 0644); err != nil {
		t.Fatalf("Initial write error: %v", err)
	}

	const numWriters = 10
	var wg sync.WaitGroup
	wg.Add(numWriters)

	for i := 0; i < numWriters; i++ {
		go func(n int) {
			defer wg.Done()
			data := []byte(string(rune('A' + n)))
			_ = WriteFile(testFile, data, 0644)
		}(i)
	}

	wg.Wait()

	content, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	if len(content) != 1 {
		t.Errorf("Expected single character, got %q", content)
	}

	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatalf("ReadDir error: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "concurrent.txt" {
			t.Errorf("Temp file left behind: %s", e.Name())
		}
	}
}

// TestWritePreservesOnFailure: a write that fails leaves the previous
// contents in place. The target's name is the longest a directory entry can
// be (NAME_MAX, 255 bytes), so its temp name (name + ".tmp.<random>") is too
// long to create and the write fails before touching the target.
func TestWritePreservesOnFailure(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, strings.Repeat("p", 255))

	initialContent := []byte("original content")
	if err := WriteFile(testFile, initialContent, 0644); err == nil {
		t.Fatal("Expected the temp name to be too long; the fixture no longer forces a failure")
	}
	// Seed the original directly: WriteFile itself cannot create this name.
	if err := os.WriteFile(testFile, initialContent, 0644); err != nil {
		t.Fatalf("seed original: %v", err)
	}

	if err := WriteFile(testFile, []byte("new content"), 0644); err == nil {
		t.Fatal("Expected an error when the temp file cannot be created")
	}

	content, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	if string(content) != string(initialContent) {
		t.Errorf("Original content not preserved: got %q", content)
	}
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("dir holds %d entries after the failed write, want only the original", len(entries))
	}
}

func TestWriteJSONStruct(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "struct.json")

	type TestStruct struct {
		Name    string   `json:"name"`
		Count   int      `json:"count"`
		Enabled bool     `json:"enabled"`
		Tags    []string `json:"tags"`
	}

	data := TestStruct{
		Name:    "test",
		Count:   42,
		Enabled: true,
		Tags:    []string{"a", "b"},
	}

	if err := WriteJSON(testFile, data); err != nil {
		t.Fatalf("WriteJSON error: %v", err)
	}

	content, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}

	var result TestStruct
	if err := json.Unmarshal(content, &result); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if result.Name != data.Name || result.Count != data.Count ||
		result.Enabled != data.Enabled || len(result.Tags) != len(data.Tags) {
		t.Errorf("Data mismatch: got %+v, want %+v", result, data)
	}
}

func TestWriteFileLargeData(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "large.bin")

	size := 1024 * 1024
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 256)
	}

	if err := WriteFile(testFile, data, 0644); err != nil {
		t.Fatalf("WriteFile error: %v", err)
	}

	content, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	if len(content) != size {
		t.Errorf("Size mismatch: got %d, want %d", len(content), size)
	}
	for i := 0; i < size; i++ {
		if content[i] != byte(i%256) {
			t.Errorf("Content mismatch at byte %d", i)
			break
		}
	}
}

func TestWriteJSONWithPerm(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "perm.json")

	if err := WriteJSONWithPerm(testFile, map[string]int{"n": 1}, 0600); err != nil {
		t.Fatalf("WriteJSONWithPerm error: %v", err)
	}

	info, err := os.Stat(testFile)
	if err != nil {
		t.Fatalf("Stat error: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("Expected 0600, got %o", info.Mode().Perm())
	}

	var got map[string]int
	content, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	if err := json.Unmarshal(content, &got); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}
	if got["n"] != 1 {
		t.Errorf("Expected {n:1}, got %v", got)
	}
}

func TestEnsureDirAndWriteJSON(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	// Target sits two directory levels below tmpDir — neither exists yet.
	testFile := filepath.Join(tmpDir, "a", "b", "cfg.json")

	if err := EnsureDirAndWriteJSON(testFile, map[string]string{"k": "v"}); err != nil {
		t.Fatalf("EnsureDirAndWriteJSON error: %v", err)
	}

	content, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	if string(content) != "{\n  \"k\": \"v\"\n}" {
		t.Errorf("Unexpected content: %s", content)
	}
}

func TestEnsureDirAndWriteJSONWithPerm(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "x", "y", "cfg.json")

	if err := EnsureDirAndWriteJSONWithPerm(testFile, map[string]string{"k": "v"}, 0600); err != nil {
		t.Fatalf("EnsureDirAndWriteJSONWithPerm error: %v", err)
	}

	info, err := os.Stat(testFile)
	if err != nil {
		t.Fatalf("Stat error: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("Expected 0600, got %o", info.Mode().Perm())
	}
}

func TestWriteJSONWithPermUnmarshallable(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "unmarshallable.json")

	if err := WriteJSONWithPerm(testFile, make(chan int), 0600); err == nil {
		t.Fatal("Expected error for unmarshallable type")
	}

	if _, statErr := os.Stat(testFile); !os.IsNotExist(statErr) {
		t.Fatal("File should not exist after marshal error")
	}
}

func TestEnsureDirAndWriteJSONMkdirFailure(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	// A regular file where a directory is expected in the target's ancestry:
	// MkdirAll fails because "blocker" exists as a file.
	blocker := filepath.Join(tmpDir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	testFile := filepath.Join(blocker, "sub", "cfg.json")

	if err := EnsureDirAndWriteJSON(testFile, map[string]string{"k": "v"}); err == nil {
		t.Fatal("Expected MkdirAll error, got nil")
	}
}

func TestEnsureDirAndWriteJSONWithPermMkdirFailure(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	blocker := filepath.Join(tmpDir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0644); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	testFile := filepath.Join(blocker, "sub", "cfg.json")

	if err := EnsureDirAndWriteJSONWithPerm(testFile, map[string]string{"k": "v"}, 0600); err == nil {
		t.Fatal("Expected MkdirAll error, got nil")
	}
}

func TestWriteFileConcurrentIntegrity(t *testing.T) {
	t.Parallel()
	// Concurrent writers to the same path must each produce self-consistent
	// content (no cross-writer byte mixing).
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "integrity.txt")

	const numWriters = 20
	const dataSize = 1024

	var wg sync.WaitGroup
	errs := make([]error, numWriters)
	wg.Add(numWriters)

	for i := 0; i < numWriters; i++ {
		go func(n int) {
			defer wg.Done()
			data := make([]byte, dataSize)
			for j := range data {
				data[j] = byte(n)
			}
			errs[n] = WriteFile(testFile, data, 0644)
		}(i)
	}

	wg.Wait()

	anySuccess := false
	for _, err := range errs {
		if err == nil {
			anySuccess = true
			break
		}
	}
	if !anySuccess {
		t.Fatal("All concurrent writes failed")
	}

	content, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	if len(content) != dataSize {
		t.Fatalf("Expected %d bytes, got %d", dataSize, len(content))
	}
	expected := content[0]
	for i, b := range content {
		if b != expected {
			t.Fatalf("Data corruption at byte %d: expected %d, got %d (cross-writer contamination)", i, expected, b)
		}
	}
}

// TestWriteFileSyncOrder: the temp file is synced before the rename and the
// parent directory after it, so the destination does not exist yet when the
// file sync runs and already holds the new bytes when the directory sync runs
// (gt-9rrnq).
func TestWriteFileSyncOrder(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "ordered.txt")

	var events []string
	syncFile := func(f *os.File) error {
		events = append(events, "file")
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Errorf("destination visible during the file sync (stat err = %v); the rename ran first", err)
		}
		return osSyncFile(f)
	}
	syncDir := func(dir string) error {
		events = append(events, "dir")
		got, err := os.ReadFile(target)
		if err != nil {
			t.Errorf("destination unreadable during the directory sync: %v", err)
		} else if string(got) != "durable" {
			t.Errorf("destination during the directory sync = %q, want %q; the rename ran after the sync", got, "durable")
		}
		return osSyncDir(dir)
	}

	if err := writeFile(target, []byte("durable"), 0644, syncFile, syncDir); err != nil {
		t.Fatalf("writeFile error: %v", err)
	}

	want := []string{"file", "dir"}
	if len(events) != len(want) || events[0] != want[0] || events[1] != want[1] {
		t.Errorf("sync order = %v, want %v", events, want)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	if string(content) != "durable" {
		t.Errorf("content = %q, want %q", content, "durable")
	}
	onlyEntry(t, tmpDir, "ordered.txt")
}

// TestWriteFileFileSyncFailurePreservesDestination: a failed file sync returns
// the error, removes the temp file and leaves the previous contents in place,
// so the caller can retry (gt-9rrnq).
func TestWriteFileFileSyncFailurePreservesDestination(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "kept.txt")
	if err := os.WriteFile(target, []byte("previous"), 0644); err != nil {
		t.Fatalf("seed destination: %v", err)
	}

	syncErr := errors.New("sync: simulated failure")
	err := writeFile(target, []byte("new"), 0644,
		func(*os.File) error { return syncErr },
		func(string) error {
			t.Error("directory sync ran after the file sync failed; the rename should not have happened")
			return nil
		},
	)
	if !errors.Is(err, syncErr) {
		t.Fatalf("writeFile error = %v, want the sync error %v", err, syncErr)
	}
	content, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("ReadFile error: %v", readErr)
	}
	if string(content) != "previous" {
		t.Errorf("destination = %q, want the old contents %q", content, "previous")
	}
	onlyEntry(t, tmpDir, "kept.txt")
}

// TestWriteFileUnsupportedDirSyncIgnored: a filesystem that will not sync a
// directory (EINVAL on macOS and the BSDs, ENOTSUP where it is stated) is not
// a failed write — the rename is still atomic and the new contents stand
// (gt-9rrnq).
func TestWriteFileUnsupportedDirSyncIgnored(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"EINVAL", syscall.EINVAL},
		{"ENOTSUP", syscall.ENOTSUP},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tmpDir := t.TempDir()
			target := filepath.Join(tmpDir, "unsupported.txt")

			err := writeFile(target, []byte("written"), 0644,
				func(f *os.File) error { return f.Sync() },
				func(string) error { return tc.err },
			)
			if err != nil {
				t.Fatalf("writeFile error = %v, want nil for an unsupported directory sync", err)
			}
			content, err := os.ReadFile(target)
			if err != nil {
				t.Fatalf("ReadFile error: %v", err)
			}
			if string(content) != "written" {
				t.Errorf("content = %q, want %q", content, "written")
			}
			onlyEntry(t, tmpDir, "unsupported.txt")
		})
	}
}

// TestWriteFileDirSyncErrorReturned: any other directory sync failure is
// reported, not swallowed, so the caller learns the rename may not survive a
// power loss (gt-9rrnq).
func TestWriteFileDirSyncErrorReturned(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	target := filepath.Join(tmpDir, "reported.txt")

	syncErr := errors.New("dir sync: simulated failure")
	err := writeFile(target, []byte("written"), 0644,
		func(f *os.File) error { return f.Sync() },
		func(string) error { return syncErr },
	)
	if !errors.Is(err, syncErr) {
		t.Fatalf("writeFile error = %v, want the directory sync error %v", err, syncErr)
	}
	content, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("ReadFile error: %v", readErr)
	}
	if string(content) != "written" {
		t.Errorf("content = %q, want %q", content, "written")
	}
	onlyEntry(t, tmpDir, "reported.txt")
}
