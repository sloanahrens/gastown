package bdgateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// testWorkspaceFiles is a valid document's files: the town's routes, one rig's
// redirect, and the rig's own workspace files, laid out as the Phase 0 proof
// (workspace_proof_integration_test.go) shows a real bd reading. season varies
// every content, so a push of another season differs byte for byte.
func testWorkspaceFiles(season string) map[string]string {
	return map[string]string{
		".beads/routes.jsonl":                  fmt.Sprintf(`{"path":".","prefix":"hq-"%s}`+"\n", ""),
		"gt/.beads/redirect":                   "/ws/gt/mayor/rig/.beads\n",
		"gt/mayor/rig/.beads/metadata.json":    fmt.Sprintf(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"gt","season":%q}`+"\n", season),
		"gt/mayor/rig/.beads/config.yaml":      fmt.Sprintf("issue-prefix: \"gt-\"\nseason: %s\n", season),
		"gt/mayor/rig/.beads/dolt-server.port": "3307",
	}
}

// openTestStore opens a store whose root is a fresh directory under the test's
// temp dir, and returns it with that root.
func openTestStore(t *testing.T) (*WorkspaceStore, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "ws")
	s, err := OpenWorkspaceStore(root)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	return s, root
}

// pushWorkspace pushes files as a valid document and returns the manifest.
func pushWorkspace(t *testing.T, s *WorkspaceStore, files map[string]string) string {
	t.Helper()
	manifest, err := s.Put(WorkspaceDocument{ManifestSHA: WorkspaceManifest(files), Files: files})
	if err != nil {
		t.Fatalf("pushing the workspace: %v", err)
	}
	return manifest
}

// readWorkspace reads the live tree at root into the document's shape.
func readWorkspace(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(content)
		return nil
	})
	if err != nil {
		t.Fatalf("reading the workspace tree: %v", err)
	}
	return files
}

// siblingNames returns the names of the entries beside the live tree, which a
// push must never leave half-written or accumulating without bound.
func siblingNames(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(root))
	if err != nil {
		t.Fatalf("reading the store directory: %v", err)
	}
	var names []string
	for _, entry := range entries {
		if entry.Name() != filepath.Base(root) {
			names = append(names, entry.Name())
		}
	}
	return names
}

// TestWorkspaceValidation drives every refusal a push makes and checks that a
// refused document leaves the live tree and its manifest exactly as they were.
func TestWorkspaceValidation(t *testing.T) {
	base := testWorkspaceFiles("one")
	cases := []struct {
		name     string
		files    func() map[string]string
		manifest func(files map[string]string) string // nil: the files' own hash
		want     error
	}{
		{
			name: "a path that climbs out of the tree",
			files: func() map[string]string {
				files := maps.Clone(base)
				files["gt/../evil/.beads/metadata.json"] = "x"
				return files
			},
			want: ErrWorkspacePath,
		},
		{
			name: "a path that starts with a parent",
			files: func() map[string]string {
				files := maps.Clone(base)
				files["../evil/.beads/metadata.json"] = "x"
				return files
			},
			want: ErrWorkspacePath,
		},
		{
			name: "an absolute path",
			files: func() map[string]string {
				files := maps.Clone(base)
				files["/ws/gt/.beads/redirect"] = "x"
				return files
			},
			want: ErrWorkspacePath,
		},
		{
			name: "a backslash path",
			files: func() map[string]string {
				files := maps.Clone(base)
				files[`gt\mayor\rig\.beads\metadata.json`] = "x"
				return files
			},
			want: ErrWorkspacePath,
		},
		{
			name: "a path with a NUL byte",
			files: func() map[string]string {
				files := maps.Clone(base)
				files["gt/.beads/metadata\x00.json"] = "x"
				return files
			},
			want: ErrWorkspacePath,
		},
		{
			name: "an empty path",
			files: func() map[string]string {
				files := maps.Clone(base)
				files[""] = "x"
				return files
			},
			want: ErrWorkspacePath,
		},
		{
			name: "a double slash",
			files: func() map[string]string {
				files := maps.Clone(base)
				files["gt//.beads/redirect"] = "x"
				return files
			},
			want: ErrWorkspacePath,
		},
		{
			name: "a dot component",
			files: func() map[string]string {
				files := maps.Clone(base)
				files["gt/./.beads/redirect"] = "x"
				return files
			},
			want: ErrWorkspacePath,
		},
		{
			name: "a file name the gateway does not serve",
			files: func() map[string]string {
				files := maps.Clone(base)
				files["gt/.beads/README"] = "x"
				return files
			},
			want: ErrWorkspaceFileName,
		},
		{
			name: "a workspace file outside a .beads directory",
			files: func() map[string]string {
				files := maps.Clone(base)
				files["gt/metadata.json"] = "x"
				return files
			},
			want: ErrWorkspaceFileName,
		},
		{
			name: "a file over the per-file cap",
			files: func() map[string]string {
				files := maps.Clone(base)
				files["gt/mayor/rig/.beads/metadata.json"] = strings.Repeat("x", MaxWorkspaceFileBytes+1)
				return files
			},
			want: ErrWorkspaceFileTooLarge,
		},
		{
			name: "a document over the total cap",
			files: func() map[string]string {
				files := maps.Clone(base)
				for _, name := range []string{
					"gt/.beads/redirect",
					"gt/mayor/rig/.beads/metadata.json",
					"gt/mayor/rig/.beads/config.yaml",
					"gt/mayor/rig/.beads/dolt-server.port",
				} {
					files[name] = strings.Repeat("x", MaxWorkspaceFileBytes)
				}
				// Four files at the per-file cap plus one byte of routes is
				// over the total cap while every file stays under its own.
				files[".beads/routes.jsonl"] = "y"
				return files
			},
			want: ErrWorkspaceTooLarge,
		},
		{
			name:  "a document with no files",
			files: func() map[string]string { return map[string]string{} },
			want:  ErrWorkspaceNoFiles,
		},
		{
			name:     "a manifest that does not match the files",
			files:    func() map[string]string { return maps.Clone(base) },
			manifest: func(map[string]string) string { return "00" },
			want:     ErrWorkspaceManifestMismatch,
		},
		{
			name:     "a document with no manifest",
			files:    func() map[string]string { return maps.Clone(base) },
			manifest: func(map[string]string) string { return "" },
			want:     ErrWorkspaceManifestMismatch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, root := openTestStore(t)
			before := pushWorkspace(t, s, base)
			beforeTree := readWorkspace(t, root)

			files := tc.files()
			doc := WorkspaceDocument{ManifestSHA: WorkspaceManifest(files), Files: files}
			if tc.manifest != nil {
				doc.ManifestSHA = tc.manifest(files)
			}
			if _, err := s.Put(doc); !errors.Is(err, tc.want) {
				t.Fatalf("Put error = %v, want %v", err, tc.want)
			}

			if after := readWorkspace(t, root); !maps.Equal(beforeTree, after) {
				t.Errorf("a refused push changed the live tree:\nbefore: %v\nafter:  %v", beforeTree, after)
			}
			if after := s.Manifest(); after != before {
				t.Errorf("manifest after a refused push = %s, want %s", after, before)
			}
		})
	}
}

// TestWorkspaceAtomicSwap checks that a push replaces the whole tree and that a
// failure part-way leaves the previous tree readable and served.
func TestWorkspaceAtomicSwap(t *testing.T) {
	first := testWorkspaceFiles("one")

	t.Run("a push replaces the whole tree", func(t *testing.T) {
		s, root := openTestStore(t)
		pushWorkspace(t, s, first)

		second := testWorkspaceFiles("two")
		delete(second, "gt/mayor/rig/.beads/config.yaml") // a dropped file must vanish
		manifest := pushWorkspace(t, s, second)

		if got := readWorkspace(t, root); !maps.Equal(second, got) {
			t.Errorf("live tree = %v, want %v", got, second)
		}
		if s.Manifest() != manifest {
			t.Errorf("Manifest() = %s, want the push's %s", s.Manifest(), manifest)
		}
	})

	t.Run("a push sweeps the tree it replaced", func(t *testing.T) {
		s, root := openTestStore(t)
		pushWorkspace(t, s, first)
		pushWorkspace(t, s, testWorkspaceFiles("two"))
		pushWorkspace(t, s, testWorkspaceFiles("three"))

		if siblings := siblingNames(t, root); len(siblings) != 1 {
			t.Errorf("siblings after three pushes = %v, want only the one tree the last push replaced", siblings)
		}
	})

	t.Run("a write failure leaves the old tree", func(t *testing.T) {
		s, root := openTestStore(t)
		before := pushWorkspace(t, s, first)
		beforeTree := readWorkspace(t, root)
		s.fs.writeFile = func(string, []byte, os.FileMode) error { return errors.New("disk full") }

		if _, err := s.Put(WorkspaceDocument{ManifestSHA: WorkspaceManifest(testWorkspaceFiles("two")), Files: testWorkspaceFiles("two")}); !errors.Is(err, ErrWorkspaceSwap) {
			t.Fatalf("Put error = %v, want %v", err, ErrWorkspaceSwap)
		}
		if got := readWorkspace(t, root); !maps.Equal(beforeTree, got) {
			t.Errorf("live tree after a write failure = %v, want %v", got, beforeTree)
		}
		if s.Manifest() != before {
			t.Errorf("manifest after a write failure = %s, want %s", s.Manifest(), before)
		}
		if siblings := siblingNames(t, root); len(siblings) != 0 {
			t.Errorf("an abandoned temp tree survived the failure: %v", siblings)
		}
	})

	t.Run("a publish failure restores the old tree", func(t *testing.T) {
		s, root := openTestStore(t)
		before := pushWorkspace(t, s, first)
		beforeTree := readWorkspace(t, root)
		calls := 0
		s.fs.rename = func(oldpath, newpath string) error {
			calls++
			if calls == 2 { // the root moved aside; publishing the new tree fails
				return errors.New("rename refused")
			}
			return os.Rename(oldpath, newpath)
		}

		if _, err := s.Put(WorkspaceDocument{ManifestSHA: WorkspaceManifest(testWorkspaceFiles("two")), Files: testWorkspaceFiles("two")}); !errors.Is(err, ErrWorkspaceSwap) {
			t.Fatalf("Put error = %v, want %v", err, ErrWorkspaceSwap)
		}
		if got := readWorkspace(t, root); !maps.Equal(beforeTree, got) {
			t.Errorf("live tree after a publish failure = %v, want %v", got, beforeTree)
		}
		if s.Manifest() != before {
			t.Errorf("manifest after a publish failure = %s, want %s", s.Manifest(), before)
		}
		if siblings := siblingNames(t, root); len(siblings) != 0 {
			t.Errorf("the failed push left %v beside the restored tree", siblings)
		}
	})

	t.Run("a live tree that cannot be read is refused", func(t *testing.T) {
		s, root := openTestStore(t)
		before := pushWorkspace(t, s, first)
		beforeTree := readWorkspace(t, root)
		s.fs.lstat = func(string) (os.FileInfo, error) { return nil, errors.New("permission denied") }

		if _, err := s.Put(WorkspaceDocument{ManifestSHA: WorkspaceManifest(testWorkspaceFiles("two")), Files: testWorkspaceFiles("two")}); !errors.Is(err, ErrWorkspaceSwap) {
			t.Fatalf("Put error = %v, want %v", err, ErrWorkspaceSwap)
		}
		if got := readWorkspace(t, root); !maps.Equal(beforeTree, got) {
			t.Errorf("live tree after an unreadable root = %v, want %v", got, beforeTree)
		}
		if s.Manifest() != before {
			t.Errorf("manifest after an unreadable root = %s, want %s", s.Manifest(), before)
		}
		if siblings := siblingNames(t, root); len(siblings) != 0 {
			t.Errorf("an abandoned temp tree survived the failure: %v", siblings)
		}
	})

	t.Run("a sweep failure refuses the push", func(t *testing.T) {
		s, root := openTestStore(t)
		pushWorkspace(t, s, first)
		second := testWorkspaceFiles("two")
		before := pushWorkspace(t, s, second)
		beforeTree := readWorkspace(t, root)
		s.fs.removeAll = func(string) error { return errors.New("permission denied") }

		if _, err := s.Put(WorkspaceDocument{ManifestSHA: WorkspaceManifest(testWorkspaceFiles("three")), Files: testWorkspaceFiles("three")}); !errors.Is(err, ErrWorkspaceSwap) {
			t.Fatalf("Put error = %v, want %v", err, ErrWorkspaceSwap)
		}
		if got := readWorkspace(t, root); !maps.Equal(beforeTree, got) {
			t.Errorf("live tree after a sweep failure = %v, want %v", got, beforeTree)
		}
		if s.Manifest() != before {
			t.Errorf("manifest after a sweep failure = %s, want %s", s.Manifest(), before)
		}
	})

	t.Run("a store directory that cannot be read is refused", func(t *testing.T) {
		s, root := openTestStore(t)
		before := pushWorkspace(t, s, first)
		beforeTree := readWorkspace(t, root)
		s.fs.readDir = func(string) ([]os.DirEntry, error) { return nil, errors.New("permission denied") }

		if _, err := s.Put(WorkspaceDocument{ManifestSHA: WorkspaceManifest(testWorkspaceFiles("two")), Files: testWorkspaceFiles("two")}); !errors.Is(err, ErrWorkspaceSwap) {
			t.Fatalf("Put error = %v, want %v", err, ErrWorkspaceSwap)
		}
		if got := readWorkspace(t, root); !maps.Equal(beforeTree, got) {
			t.Errorf("live tree after an unreadable store directory = %v, want %v", got, beforeTree)
		}
		if s.Manifest() != before {
			t.Errorf("manifest after an unreadable store directory = %s, want %s", s.Manifest(), before)
		}
	})

	t.Run("a restore failure is reported and the old tree stays readable", func(t *testing.T) {
		s, root := openTestStore(t)
		pushWorkspace(t, s, first)
		beforeTree := readWorkspace(t, root)
		calls := 0
		s.fs.rename = func(oldpath, newpath string) error {
			calls++
			if calls >= 2 { // publishing fails, and so does moving the old tree back
				return errors.New("rename refused")
			}
			return os.Rename(oldpath, newpath)
		}

		_, err := s.Put(WorkspaceDocument{ManifestSHA: WorkspaceManifest(testWorkspaceFiles("two")), Files: testWorkspaceFiles("two")})
		if !errors.Is(err, ErrWorkspaceSwap) {
			t.Fatalf("Put error = %v, want %v", err, ErrWorkspaceSwap)
		}
		if !strings.Contains(err.Error(), "restoring the previous tree") {
			t.Errorf("error does not say the restore failed: %v", err)
		}
		if got := readWorkspace(t, root+suffixOld); !maps.Equal(beforeTree, got) {
			t.Errorf("the tree moved aside = %v, want the previous tree %v", got, beforeTree)
		}
	})

	t.Run("racing pushes leave one whole tree", func(t *testing.T) {
		s, root := openTestStore(t)
		pushWorkspace(t, s, first)
		docs := make([]map[string]string, 8)
		for i := range docs {
			docs[i] = testWorkspaceFiles(fmt.Sprintf("race-%d", i))
		}
		errs := make([]error, len(docs))
		var wg sync.WaitGroup
		for i, files := range docs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = s.Put(WorkspaceDocument{ManifestSHA: WorkspaceManifest(files), Files: files})
			}()
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("racing push %d failed: %v", i, err)
			}
		}
		live := readWorkspace(t, root)
		matches := false
		for _, files := range append(docs, first) {
			if maps.Equal(files, live) {
				matches = true
				break
			}
		}
		if !matches {
			t.Errorf("the live tree is not any one push's tree: %v", live)
		}
		if s.Manifest() != WorkspaceManifest(live) {
			t.Errorf("manifest %s does not hash the live tree %s", s.Manifest(), WorkspaceManifest(live))
		}
	})
}

// suffixOld is the name a replacement gives the tree it moves aside.
const suffixOld = ".old"

// TestWorkspaceManifestHeader checks the manifest hash's two jobs: it follows
// the content exactly, and every answer names the live tree's hash.
func TestWorkspaceManifestHeader(t *testing.T) {
	s, root := openTestStore(t)
	first := testWorkspaceFiles("one")
	want := WorkspaceManifest(first)
	if got := pushWorkspace(t, s, first); got != want {
		t.Fatalf("Put returned %s, want the hash of the files %s", got, want)
	}
	if got := s.Manifest(); got != want {
		t.Errorf("Manifest() = %s, want %s", got, want)
	}
	if got := WorkspaceManifest(nil); got == want {
		t.Errorf("an empty workspace hashes to the pushed tree's %s", got)
	}

	// The same content built in another map, in another insertion order, is the
	// same workspace.
	reordered := map[string]string{}
	for _, name := range []string{
		"gt/mayor/rig/.beads/dolt-server.port",
		"gt/mayor/rig/.beads/config.yaml",
		"gt/mayor/rig/.beads/metadata.json",
		"gt/.beads/redirect",
		".beads/routes.jsonl",
	} {
		reordered[name] = first[name]
	}
	if got := WorkspaceManifest(reordered); got != want {
		t.Errorf("the same files in another order hash to %s, want %s", got, want)
	}

	// A restart recomputes the live tree's hash from the tree itself.
	reopened, err := OpenWorkspaceStore(root)
	if err != nil {
		t.Fatalf("reopening the store: %v", err)
	}
	if got := reopened.Manifest(); got != want {
		t.Errorf("a reopened store reports %s, want %s", got, want)
	}

	changed := testWorkspaceFiles("two")
	if got := WorkspaceManifest(changed); got == want {
		t.Errorf("changed content kept the hash %s", got)
	}
	manifest := pushWorkspace(t, s, changed)
	if manifest == want {
		t.Errorf("the new push kept the old hash %s", manifest)
	}

	// The 200 body and the header both name the tree now served.
	if got := s.Manifest(); got != manifest {
		t.Fatalf("Manifest() = %s, want %s", got, manifest)
	}
	encoded, err := json.Marshal(changed)
	if err != nil {
		t.Fatalf("encoding the files: %v", err)
	}
	doc := fmt.Sprintf(`{"manifest_sha":%q,"files":%s}`, manifest, encoded)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/workspace", strings.NewReader(doc)))
	if rec.Code != http.StatusOK {
		t.Fatalf("re-pushing the same content answered %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get(WorkspaceHashHeader); got != manifest {
		t.Errorf("%s = %q, want %q", WorkspaceHashHeader, got, manifest)
	}
	if !strings.Contains(rec.Body.String(), manifest) {
		t.Errorf("the 200 body does not name %s: %s", manifest, rec.Body.String())
	}
}

// TestWorkspaceHandler checks the HTTP answers around the store: the manifest
// header on success and on every refusal, the statuses a bad request gets, and
// that a store failure is never reported as a successful push.
func TestWorkspaceHandler(t *testing.T) {
	body := func(t *testing.T, files map[string]string) string {
		t.Helper()
		encoded, err := json.Marshal(files)
		if err != nil {
			t.Fatalf("encoding the files: %v", err)
		}
		return fmt.Sprintf(`{"manifest_sha":%q,"files":%s}`, WorkspaceManifest(files), encoded)
	}
	push := func(t *testing.T, s *WorkspaceStore) {
		t.Helper()
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/workspace", strings.NewReader(body(t, testWorkspaceFiles("one")))))
		if rec.Code != http.StatusOK {
			t.Fatalf("a valid push answered %d: %s", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get(WorkspaceHashHeader); got != WorkspaceManifest(testWorkspaceFiles("one")) {
			t.Fatalf("a valid push answered with header %q, want the pushed hash", got)
		}
	}

	t.Run("a valid push answers with the manifest", func(t *testing.T) {
		s, _ := openTestStore(t)
		push(t, s)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/workspace", strings.NewReader(body(t, testWorkspaceFiles("two")))))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
		}
		manifest := WorkspaceManifest(testWorkspaceFiles("two"))
		if got := rec.Header().Get(WorkspaceHashHeader); got != manifest {
			t.Errorf("header = %q, want %q", got, manifest)
		}
		if !strings.Contains(rec.Body.String(), manifest) {
			t.Errorf("body does not name %s: %s", manifest, rec.Body.String())
		}
	})

	t.Run("a refusal carries the unchanged manifest", func(t *testing.T) {
		s, _ := openTestStore(t)
		push(t, s)
		before := s.Manifest()
		files := maps.Clone(testWorkspaceFiles("one"))
		files["../evil/.beads/metadata.json"] = "x"
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/workspace", strings.NewReader(body(t, files))))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get(WorkspaceHashHeader); got != before {
			t.Errorf("refusal header = %q, want the live hash %q", got, before)
		}
		if !strings.Contains(rec.Body.String(), "workspace_path_refused") {
			t.Errorf("body does not name the refusal code: %s", rec.Body.String())
		}
	})

	t.Run("a method other than PUT is refused", func(t *testing.T) {
		s, _ := openTestStore(t)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/workspace", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != http.MethodPut {
			t.Errorf("Allow = %q, want PUT", got)
		}
	})

	t.Run("a malformed body is refused", func(t *testing.T) {
		s, _ := openTestStore(t)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/workspace", strings.NewReader("{")))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "workspace_document_malformed") {
			t.Errorf("body does not name the refusal code: %s", rec.Body.String())
		}
	})

	t.Run("a body over the document cap is refused", func(t *testing.T) {
		s, _ := openTestStore(t)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/workspace", strings.NewReader(strings.Repeat("x", MaxWorkspaceDocumentBytes+1))))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", rec.Code)
		}
	})

	t.Run("a store failure is not reported as a push", func(t *testing.T) {
		s, _ := openTestStore(t)
		push(t, s)
		before := s.Manifest()
		s.fs.rename = func(string, string) error { return errors.New("volume gone") }
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/workspace", strings.NewReader(body(t, testWorkspaceFiles("two")))))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "workspace_store_failed") {
			t.Errorf("body does not name the refusal code: %s", rec.Body.String())
		}
		if got := rec.Header().Get(WorkspaceHashHeader); got != before {
			t.Errorf("failure header = %q, want the live hash %q", got, before)
		}
	})
}

// TestOpenWorkspaceStore checks the tree the store takes ownership of: a
// missing root is empty, and a tree it would not have written is refused rather
// than hashed and served.
func TestOpenWorkspaceStore(t *testing.T) {
	t.Run("a missing root is an empty workspace", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "ws")
		s, err := OpenWorkspaceStore(root)
		if err != nil {
			t.Fatalf("opening a missing root: %v", err)
		}
		if got, want := s.Manifest(), WorkspaceManifest(map[string]string{}); got != want {
			t.Errorf("Manifest() = %s, want the empty tree's %s", got, want)
		}
		if _, err := os.Stat(root); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("opening created the root before any push: %v", err)
		}
	})

	t.Run("an empty root path", func(t *testing.T) {
		if _, err := OpenWorkspaceStore(""); !errors.Is(err, ErrWorkspaceRoot) {
			t.Fatalf(`OpenWorkspaceStore("") error = %v, want %v`, err, ErrWorkspaceRoot)
		}
	})

	cases := []struct {
		name  string
		build func(t *testing.T, root string)
	}{
		{
			name: "a root that is a file",
			build: func(t *testing.T, root string) {
				if err := os.WriteFile(root, []byte("x"), 0o600); err != nil {
					t.Fatalf("writing the root file: %v", err)
				}
			},
		},
		{
			name: "a tree with a file the store does not serve",
			build: func(t *testing.T, root string) {
				writeFileIn(t, root, "gt/.beads/README", "x")
			},
		},
		{
			name: "a tree with an oversized file",
			build: func(t *testing.T, root string) {
				writeFileIn(t, root, "gt/.beads/redirect", strings.Repeat("x", MaxWorkspaceFileBytes+1))
			},
		},
		{
			name: "a tree over the total cap",
			build: func(t *testing.T, root string) {
				for _, path := range []string{
					".beads/routes.jsonl",
					"gt/.beads/redirect",
					"gt/mayor/rig/.beads/metadata.json",
					"gt/mayor/rig/.beads/config.yaml",
					"gt/mayor/rig/.beads/dolt-server.port",
				} {
					writeFileIn(t, root, path, strings.Repeat("x", MaxWorkspaceFileBytes))
				}
			},
		},
		{
			name: "a tree with a symlink",
			build: func(t *testing.T, root string) {
				writeFileIn(t, root, "gt/.beads/target", "x")
				if err := os.Symlink(filepath.Join(root, "gt/.beads/target"), filepath.Join(root, "gt/.beads/redirect")); err != nil {
					t.Fatalf("creating the symlink: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "ws")
			tc.build(t, root)
			if _, err := OpenWorkspaceStore(root); !errors.Is(err, ErrWorkspaceRoot) {
				t.Fatalf("OpenWorkspaceStore(%q) error = %v, want %v", root, err, ErrWorkspaceRoot)
			}
		})
	}

	t.Run("a root that cannot be read", func(t *testing.T) {
		fsys := osWorkspaceFS()
		fsys.lstat = func(string) (os.FileInfo, error) { return nil, errors.New("permission denied") }
		if _, err := openWorkspaceStore(filepath.Join(t.TempDir(), "ws"), fsys); !errors.Is(err, ErrWorkspaceRoot) {
			t.Fatalf("error = %v, want %v", err, ErrWorkspaceRoot)
		}
	})

	t.Run("a tree file that cannot be read", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "ws")
		writeFileIn(t, root, "gt/.beads/redirect", "x")
		fsys := osWorkspaceFS()
		fsys.readFile = func(string) ([]byte, error) { return nil, errors.New("permission denied") }
		if _, err := openWorkspaceStore(root, fsys); !errors.Is(err, ErrWorkspaceRoot) {
			t.Fatalf("error = %v, want %v", err, ErrWorkspaceRoot)
		}
	})
}

// writeFileIn creates path under root and writes content to it.
func writeFileIn(t *testing.T, root, path, content string) {
	t.Helper()
	target := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(target), err)
	}
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", target, err)
	}
}
