package k8s

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"gopkg.in/yaml.v3"
)

// docObject reads the one document Doc holds and fails when it holds more, so a
// Doc that still carries its separator or a neighbouring document cannot pass.
func docObject(t *testing.T, doc []byte) (apiVersion, kind, name, namespace string) {
	t.Helper()
	var parsed struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Metadata   struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"metadata"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(doc))
	if err := dec.Decode(&parsed); err != nil {
		t.Fatalf("Doc is not a YAML object: %v", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("Doc holds more than one document")
	}
	if strings.Contains(string(doc), "\n---") {
		t.Fatalf("Doc still carries a separator:\n%s", doc)
	}
	return parsed.APIVersion, parsed.Kind, parsed.Metadata.Name, parsed.Metadata.Namespace
}

// The control cluster's static set: every document of the eight embedded files,
// in file order. sweep-rbac.yaml holds nine documents, not the eight the bead
// first counted: its eight "---" separators sit between nine documents.
func TestStaticObjectsDocuments(t *testing.T) {
	t.Parallel()
	type want struct {
		file       string
		apiVersion string
		kind       string
		name       string
		namespace  string
		cluster    bool
		secret     bool
	}
	admission := "admissionregistration.k8s.io/v1"
	rbac := "rbac.authorization.k8s.io/v1"
	sched := "scheduling.k8s.io/v1"
	wants := []want{
		{"bd-gateway-service-admission.yaml", admission, "ValidatingAdmissionPolicy", "gt-bd-gateway-service", "", true, false},
		{"bd-gateway-service-admission.yaml", admission, "ValidatingAdmissionPolicyBinding", "gt-bd-gateway-service", "", true, false},
		{"update-admission.yaml", admission, "ValidatingAdmissionPolicy", "gt-update-jobs", "", true, false},
		{"update-admission.yaml", admission, "ValidatingAdmissionPolicyBinding", "gt-update-jobs", "", true, false},
		{"update-rbac.yaml", "v1", "ServiceAccount", "gt-update", "gt", false, false},
		{"update-rbac.yaml", rbac, "Role", "gt-update", "gt", false, false},
		{"update-rbac.yaml", rbac, "RoleBinding", "gt-update", "gt", false, false},
		{"update-rbac.yaml", rbac, "Role", "gt-update-launch", "gt", false, false},
		{"update-rbac.yaml", rbac, "RoleBinding", "gt-update-launch", "gt", false, false},
		{"sweep-rbac.yaml", "v1", "Namespace", "gt-sweeps", "", true, false},
		{"sweep-rbac.yaml", "v1", "ServiceAccount", "sweep-runner", "gt-sweeps", false, false},
		{"sweep-rbac.yaml", rbac, "Role", "sweep-runner", "gt-sweeps", false, false},
		{"sweep-rbac.yaml", rbac, "RoleBinding", "sweep-runner", "gt-sweeps", false, false},
		{"sweep-rbac.yaml", "v1", "Secret", "sweep-runner-token", "gt-sweeps", false, true},
		{"sweep-rbac.yaml", rbac, "ClusterRole", "gt-sweep-reader", "", true, false},
		{"sweep-rbac.yaml", rbac, "ClusterRoleBinding", "gt-sweep-reader", "", true, false},
		{"sweep-rbac.yaml", rbac, "Role", "gt-ci-cache-trim", "forgejo", false, false},
		{"sweep-rbac.yaml", rbac, "RoleBinding", "gt-ci-cache-trim", "forgejo", false, false},
		{"mac-watch-rbac.yaml", "v1", "ServiceAccount", "mac-watch", "gt", false, false},
		{"mac-watch-rbac.yaml", rbac, "Role", "mac-watch", "gt", false, false},
		{"mac-watch-rbac.yaml", rbac, "RoleBinding", "mac-watch", "gt", false, false},
		{"priority-classes.yaml", sched, "PriorityClass", "gt-control-plane", "", true, false},
		{"priority-classes.yaml", sched, "PriorityClass", "gt-sandbox", "", true, false},
		{"priority-classes.yaml", sched, "PriorityClass", "gt-ci-gate", "", true, false},
		{"priority-classes.yaml", sched, "PriorityClass", "gt-ci", "", true, false},
		{"drift-rbac.yaml", rbac, "ClusterRole", driftReaderRole, "", true, false},
		{"drift-rbac.yaml", rbac, "ClusterRoleBinding", driftReaderRole, "", true, false},
		{"deployer-rbac.yaml", rbac, "Role", deployerApplyRole, deployerNamespace, false, false},
		{"deployer-rbac.yaml", rbac, "RoleBinding", deployerApplyRole, deployerNamespace, false, false},
		{"deployer-rbac.yaml", rbac, "ClusterRole", deployerPolicyRole, "", true, false},
		{"deployer-rbac.yaml", rbac, "ClusterRoleBinding", deployerPolicyRole, "", true, false},
		{"deployer-rbac.yaml", rbac, "ClusterRole", deployerReaderRole, "", true, false},
		{"deployer-rbac.yaml", rbac, "ClusterRoleBinding", deployerReaderRole, "", true, false},
	}

	objects, err := Static()
	if err != nil {
		t.Fatalf("Static(): %v", err)
	}
	if len(objects) != len(wants) {
		t.Fatalf("Static() returned %d objects, want %d", len(objects), len(wants))
	}
	perFile := map[string]int{}
	for i, w := range wants {
		got := objects[i]
		perFile[w.file]++
		if got.APIVersion != w.apiVersion || got.Kind != w.kind || got.Name != w.name {
			t.Errorf("object %d (%s) = %s %s %s, want %s %s %s",
				i, w.file, got.APIVersion, got.Kind, got.Name, w.apiVersion, w.kind, w.name)
		}
		if got.Namespace != w.namespace {
			t.Errorf("object %d (%s %s) namespace = %q, want %q", i, got.Kind, got.Name, got.Namespace, w.namespace)
		}
		if got.Cluster != w.cluster {
			t.Errorf("object %d (%s %s) Cluster = %v, want %v", i, got.Kind, got.Name, got.Cluster, w.cluster)
		}
		if got.SecretNoData != w.secret {
			t.Errorf("object %d (%s %s) SecretNoData = %v, want %v", i, got.Kind, got.Name, got.SecretNoData, w.secret)
		}
		apiVersion, kind, name, namespace := docObject(t, got.Doc)
		if apiVersion != got.APIVersion || kind != got.Kind || name != got.Name {
			t.Errorf("object %d (%s %s) Doc holds %s %s %s", i, got.Kind, got.Name, apiVersion, kind, name)
		}
		// A cluster-scoped object's document carries no namespace to disagree
		// with the empty Namespace Static reported.
		if got.Cluster && namespace != "" {
			t.Errorf("object %d (%s %s) Doc carries namespace %q", i, got.Kind, got.Name, namespace)
		}
	}
	if got := perFile["sweep-rbac.yaml"]; got != 9 {
		t.Errorf("sweep-rbac.yaml contributed %d documents, want 9", got)
	}
}

func TestStaticObjectsEveryFileAccountedFor(t *testing.T) {
	t.Parallel()

	t.Run("the repository tree accounts for every file", func(t *testing.T) {
		t.Parallel()
		excluded, err := staticExcludedFiles(StaticExcluded)
		if err != nil {
			t.Fatalf("StaticExcluded: %v", err)
		}
		// drift-exceptions.yaml is embedded beside the manifests but is not one:
		// Static returns no object for it, so the accounting names it here.
		embedded := append(slices.Clone(staticFileNames), driftExceptionsFile)
		if problems := accountingProblems(walkStaticYAML(t, "."), embedded, excluded); len(problems) > 0 {
			t.Fatalf("deploy/k8s files not accounted for:\n%s", strings.Join(problems, "\n"))
		}
	})

	t.Run("a file the set neither embeds nor excludes is refused", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeFile(t, root, "static.yaml")
		writeFile(t, root, "hand-applied.yaml")
		problems := accountingProblems(walkStaticYAML(t, root), []string{"static.yaml"}, nil)
		if len(problems) != 1 || !strings.Contains(problems[0], "hand-applied.yaml") {
			t.Fatalf("problems = %q, want one naming hand-applied.yaml", problems)
		}
	})

	t.Run("an excluded file that is not there is refused", func(t *testing.T) {
		t.Parallel()
		problems := accountingProblems(nil, nil, []string{"control-priorities.yaml"})
		if len(problems) != 1 || !strings.Contains(problems[0], "control-priorities.yaml") {
			t.Fatalf("problems = %q, want one naming control-priorities.yaml", problems)
		}
	})

	t.Run("an embedded file that is also excluded is refused", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		writeFile(t, root, "sweep-rbac.yaml")
		problems := accountingProblems(walkStaticYAML(t, root), []string{"sweep-rbac.yaml"}, []string{"sweep-rbac.yaml"})
		if len(problems) != 1 || !strings.Contains(problems[0], "sweep-rbac.yaml") {
			t.Fatalf("problems = %q, want one naming sweep-rbac.yaml", problems)
		}
	})

	t.Run("the directories another package owns are not walked", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		for _, dir := range []string{"kind", "values", "daemon", "dolt", "forgejo-runner", "update-job", "update-job/testdata", "testdata"} {
			writeFile(t, root, filepath.Join(dir, "owned.yaml"))
		}
		writeFile(t, root, "root.yaml")
		writeFile(t, root, "policy/kind.yaml")
		writeFile(t, root, "gt-cache/claim.yaml")
		got := walkStaticYAML(t, root)
		want := []string{"gt-cache/claim.yaml", "policy/kind.yaml", "root.yaml"}
		if !slices.Equal(got, want) {
			t.Fatalf("walk found %q, want %q", got, want)
		}
	})

	t.Run("a row outside deploy/k8s is refused", func(t *testing.T) {
		t.Parallel()
		rows := []struct{ File, Owner, Reason string }{
			{"deploy/elsewhere.yaml", "overseer", "not under the static directory"},
		}
		_, err := staticExcludedFiles(rows)
		if err == nil || !strings.Contains(err.Error(), "deploy/elsewhere.yaml") {
			t.Fatalf("staticExcludedFiles(%q) = %v, want an error naming it", rows, err)
		}
	})

	t.Run("a row with no owner is refused", func(t *testing.T) {
		t.Parallel()
		rows := []struct{ File, Owner, Reason string }{
			{"deploy/k8s/hand-applied.yaml", "", "a hand-applied object"},
		}
		_, err := staticExcludedFiles(rows)
		if err == nil || !strings.Contains(err.Error(), "hand-applied.yaml") || !strings.Contains(err.Error(), "owner") {
			t.Fatalf("staticExcludedFiles(%q) = %v, want an error naming the file and its missing owner", rows, err)
		}
	})

	t.Run("a row with no reason is refused", func(t *testing.T) {
		t.Parallel()
		rows := []struct{ File, Owner, Reason string }{
			{"deploy/k8s/hand-applied.yaml", "overseer", ""},
		}
		_, err := staticExcludedFiles(rows)
		if err == nil || !strings.Contains(err.Error(), "hand-applied.yaml") || !strings.Contains(err.Error(), "reason") {
			t.Fatalf("staticExcludedFiles(%q) = %v, want an error naming the file and its missing reason", rows, err)
		}
	})
}

func TestStaticObjectsRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		body    string
		want    string
		notWant string
	}{
		{
			name: "no apiVersion",
			body: "kind: Role\nmetadata:\n  name: r\n",
			want: "no apiVersion",
		},
		{
			name: "no kind",
			body: "apiVersion: v1\nmetadata:\n  name: r\n",
			want: "no kind",
		},
		{
			name: "no metadata",
			body: "apiVersion: v1\nkind: Role\n",
			want: "no metadata.name",
		},
		{
			name: "no metadata.name",
			body: "apiVersion: v1\nkind: Role\nmetadata:\n  namespace: gt\n",
			want: "no metadata.name",
		},
		{
			name: "Secret with data",
			body: "apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\n  namespace: gt\ndata:\n  token: aGk=\n",
			want: "carries data",
		},
		{
			name: "Secret with stringData",
			body: "apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\n  namespace: gt\nstringData:\n  token: hi\n",
			want: "carries data",
		},
		{
			name: "a kind outside the static set",
			body: "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: d\n  namespace: gt\n",
			want: "Deployment is not in the static set",
		},
		{
			name: "a namespaced kind with no namespace",
			body: "apiVersion: v1\nkind: Role\nmetadata:\n  name: r\n",
			want: "no metadata.namespace",
		},
		{
			name: "a separator the splitter does not recognize hides a second document",
			body: "apiVersion: v1\nkind: Role\nmetadata: {name: a, namespace: gt}\n--- # second\napiVersion: v1\nkind: Role\nmetadata: {name: b, namespace: gt}\n",
			want: "more than one YAML document",
		},
		{
			name:    "a malformed second document reports its parse error",
			body:    "apiVersion: v1\nkind: Role\nmetadata: {name: a, namespace: gt}\n--- # second\na: b: c\n",
			want:    "second document",
			notWant: "more than one YAML document",
		},
		{
			name: "a document that is not a mapping",
			body: "- apiVersion: v1\n",
			want: "not a YAML object",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			const file = "planted.yaml"
			fsys := fstest.MapFS{file: &fstest.MapFile{Data: []byte(tt.body)}}
			_, err := staticObjects(fsys, []string{file})
			if err == nil {
				t.Fatalf("staticObjects() accepted a manifest with %s", tt.name)
			}
			if !strings.Contains(err.Error(), staticPath(file)) {
				t.Errorf("error %q does not name %s", err, staticPath(file))
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not say %q", err, tt.want)
			}
			if tt.notWant != "" && strings.Contains(err.Error(), tt.notWant) {
				t.Errorf("error %q reads as %q", err, tt.notWant)
			}
		})
	}
}

// The split and the kind map on inputs the repository files do not carry.
func TestStaticObjectsSplit(t *testing.T) {
	t.Parallel()
	t.Run("blank documents are skipped", func(t *testing.T) {
		t.Parallel()
		body := "---\n\napiVersion: v1\nkind: Role\nmetadata: {name: r, namespace: gt}\n---\n\n---\n"
		objects, err := staticObjects(fstest.MapFS{"m.yaml": &fstest.MapFile{Data: []byte(body)}}, []string{"m.yaml"})
		if err != nil {
			t.Fatalf("staticObjects(): %v", err)
		}
		if len(objects) != 1 || objects[0].Name != "r" {
			t.Fatalf("objects = %+v, want the one Role r", objects)
		}
	})

	t.Run("a cluster-scoped kind reports no namespace", func(t *testing.T) {
		t.Parallel()
		body := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: n\n  namespace: ignored\n"
		objects, err := staticObjects(fstest.MapFS{"m.yaml": &fstest.MapFile{Data: []byte(body)}}, []string{"m.yaml"})
		if err != nil {
			t.Fatalf("staticObjects(): %v", err)
		}
		if len(objects) != 1 {
			t.Fatalf("staticObjects() returned %d objects, want 1", len(objects))
		}
		if !objects[0].Cluster || objects[0].Namespace != "" {
			t.Fatalf("Namespace object = Cluster %v namespace %q, want cluster-scoped with no namespace",
				objects[0].Cluster, objects[0].Namespace)
		}
	})

	t.Run("a missing file is refused", func(t *testing.T) {
		t.Parallel()
		_, err := staticObjects(fstest.MapFS{}, []string{"gone.yaml"})
		if err == nil || !strings.Contains(err.Error(), staticPath("gone.yaml")) {
			t.Fatalf("staticObjects() = %v, want an error naming %s", err, staticPath("gone.yaml"))
		}
	})
}

// walkStaticYAML returns every YAML file under root, relative to root with
// slash separators, skipping the directories whose files another package owns.
func walkStaticYAML(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && staticOwnedDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".yaml", ".yml":
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	sort.Strings(files)
	return files
}

// staticOwnedDirs hold files another package reads and tests; none of them is a
// control-cluster object the static set renders.
var staticOwnedDirs = map[string]bool{
	"kind":           true,
	"values":         true,
	"daemon":         true,
	"dolt":           true,
	"forgejo-runner": true,
	"update-job":     true,
	"testdata":       true,
}

// accountingProblems returns one line per file the static set does not account
// for, and per listed file that is not on disk: a new hand-applied manifest
// cannot be added without a decision.
func accountingProblems(files, embedded, excluded []string) []string {
	onDisk := map[string]bool{}
	for _, f := range files {
		onDisk[f] = true
	}
	isEmbedded := map[string]bool{}
	for _, f := range embedded {
		isEmbedded[f] = true
	}
	isExcluded := map[string]bool{}
	for _, f := range excluded {
		isExcluded[f] = true
	}
	var problems []string
	for _, f := range files {
		switch {
		case isEmbedded[f] && isExcluded[f]:
			problems = append(problems, fmt.Sprintf("%s is both embedded and excluded", staticPath(f)))
		case !isEmbedded[f] && !isExcluded[f]:
			problems = append(problems, fmt.Sprintf("%s is neither embedded nor in StaticExcluded", staticPath(f)))
		}
	}
	for _, f := range append(slices.Clone(embedded), excluded...) {
		if !onDisk[f] {
			problems = append(problems, fmt.Sprintf("%s is listed but not on disk", staticPath(f)))
		}
	}
	return problems
}

// writeFile plants a YAML file under root.
func writeFile(t *testing.T, root, rel string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("apiVersion: v1\n"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
