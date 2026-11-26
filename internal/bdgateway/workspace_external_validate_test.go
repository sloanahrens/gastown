package bdgateway

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// externalConfig is a rig config whose external project resolves to a path in
// the tree. The rig's .beads sits under gt/mayor/rig, so the path climbs to the
// tree root and names beads/mayor/rig.
const externalConfig = "issue-prefix: \"gt-\"\nexternal_projects:\n  beads: ../../../beads/mayor/rig\n"

// withRigConfig returns the valid document with the rig's config replaced by config.
func withRigConfig(config string) map[string]string {
	files := maps.Clone(testWorkspaceFiles("one"))
	files["gt/mayor/rig/.beads/config.yaml"] = config
	return files
}

// TestWorkspaceRefusesExternalProject: a document whose rig config names an
// external project that is not in the tree is refused by Put with
// ErrWorkspaceExternalProject, and the live tree and manifest stay as they were.
func TestWorkspaceRefusesExternalProject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		files func() map[string]string
	}{
		{
			name:  "a configured project that is not in the tree",
			files: func() map[string]string { return withRigConfig(externalConfig) },
		},
		{
			name: "a configured project whose path climbs above the tree root",
			files: func() map[string]string {
				return withRigConfig("external_projects:\n  beads: ../../../../../beads\n")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, root := openTestStore(t)
			before := pushWorkspace(t, s, testWorkspaceFiles("one"))
			beforeTree := readWorkspace(t, root)

			files := tc.files()
			doc := WorkspaceDocument{ManifestSHA: WorkspaceManifest(files), Files: files}
			if _, err := s.Put(doc); !errors.Is(err, ErrWorkspaceExternalProject) {
				t.Fatalf("Put error = %v, want %v", err, ErrWorkspaceExternalProject)
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

// TestWorkspaceAcceptsResolvableExternalProject: a document whose external
// project resolves to a .beads directory in the tree, and a document with no
// external project, are accepted as before. The push handler answers a missing
// project with the status of the other validation refusals.
func TestWorkspaceAcceptsResolvableExternalProject(t *testing.T) {
	t.Parallel()

	t.Run("a project that resolves to a .beads directory is accepted", func(t *testing.T) {
		s, root := openTestStore(t)
		files := withRigConfig(externalConfig)
		files["beads/mayor/rig/.beads/config.yaml"] = "issue-prefix: \"be-\"\n"
		manifest, err := s.Put(WorkspaceDocument{ManifestSHA: WorkspaceManifest(files), Files: files})
		if err != nil {
			t.Fatalf("Put of a resolvable external project: %v", err)
		}
		if manifest != WorkspaceManifest(files) {
			t.Errorf("manifest = %s, want %s", manifest, WorkspaceManifest(files))
		}
		if got := readWorkspace(t, root); !maps.Equal(got, files) {
			t.Errorf("the live tree does not hold the pushed files:\ngot:  %v\nwant: %v", got, files)
		}
	})

	t.Run("a document with no external project is accepted", func(t *testing.T) {
		s, _ := openTestStore(t)
		files := testWorkspaceFiles("one")
		if _, err := s.Put(WorkspaceDocument{ManifestSHA: WorkspaceManifest(files), Files: files}); err != nil {
			t.Fatalf("Put of a document with no external project: %v", err)
		}
	})

	t.Run("the push handler answers a missing project with the status of a manifest refusal", func(t *testing.T) {
		s, _ := openTestStore(t)
		g := newTestGateway(t, s, newTestCallLog(t), &stubRunner{exit: 0})
		handler := NewPushHandler(g)

		push := func(doc WorkspaceDocument) *httptest.ResponseRecorder {
			t.Helper()
			body, err := json.Marshal(doc)
			if err != nil {
				t.Fatalf("encoding the document: %v", err)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, pushRequestFor(http.MethodPut, workspacePath, testDaemonToken, string(body)))
			return rec
		}

		files := withRigConfig(externalConfig)
		missing := push(WorkspaceDocument{ManifestSHA: WorkspaceManifest(files), Files: files})
		mismatch := push(WorkspaceDocument{ManifestSHA: "00", Files: files})

		if missing.Code != http.StatusBadRequest {
			t.Fatalf("missing project = %d, want 400: %s", missing.Code, missing.Body.String())
		}
		if missing.Code != mismatch.Code {
			t.Errorf("missing project = %d, manifest refusal = %d; want the same status", missing.Code, mismatch.Code)
		}
		if body := missing.Body.String(); !strings.Contains(body, "workspace_external_project_refused") {
			t.Errorf("body does not name the refusal code: %s", body)
		}
	})
}
