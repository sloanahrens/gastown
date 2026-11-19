package k8s

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// sbxWant is one expected gt-sbx document: the file it comes from and the
// object it holds.
type sbxWant struct {
	file       string
	apiVersion string
	kind       string
	name       string
	namespace  string
	cluster    bool
}

// sbxSourcedDocument is one document SbxStatic returns with the file it comes
// from, so a test can name the source file of every document in apply order.
type sbxSourcedDocument struct {
	file string
	obj  StaticObject
}

// The gt-sbx set applies in order: the priority classes OpenShell admission and
// eviction need, then the control-plane Services the sandbox policy names, the
// gateway NodePort, and the cache claim the sandbox pods mount.
func TestSbxStaticDocumentsByNameAndFile(t *testing.T) {
	t.Parallel()
	sched := "scheduling.k8s.io/v1"
	services := "values/kind/control-plane-services.yaml"
	wants := []sbxWant{
		{"priority-classes.yaml", sched, "PriorityClass", "gt-control-plane", "", true},
		{"priority-classes.yaml", sched, "PriorityClass", "gt-sandbox", "", true},
		{"priority-classes.yaml", sched, "PriorityClass", "gt-ci-gate", "", true},
		{"priority-classes.yaml", sched, "PriorityClass", "gt-ci", "", true},
		{services, "v1", "Service", "control-plane", "openshell", false},
		{services, "v1", "Service", "dolt", "openshell", false},
		{services, "v1", "Service", "forgejo", "openshell", false},
		{services, "v1", "Service", "tracker", "openshell", false},
		{"kind/gateway-nodeport.yaml", "v1", "Service", "openshell-nodeport", "openshell", false},
		{"gt-cache/claim.yaml", "v1", "PersistentVolumeClaim", "gt-cache", "openshell", false},
	}

	got := sbxDocumentsByFile(t)
	if problems := sbxDocumentProblems(got, wants); len(problems) > 0 {
		t.Fatalf("SbxStatic() documents:\n%s", strings.Join(problems, "\n"))
	}
	for i, doc := range got {
		apiVersion, kind, name, namespace := docObject(t, doc.obj.Doc)
		if apiVersion != doc.obj.APIVersion || kind != doc.obj.Kind || name != doc.obj.Name {
			t.Errorf("document %d (%s %s) Doc holds %s %s %s", i, doc.obj.Kind, doc.obj.Name, apiVersion, kind, name)
		}
		// A cluster-scoped object's document carries no namespace to disagree
		// with the empty Namespace SbxStatic reported.
		if doc.obj.Cluster && namespace != "" {
			t.Errorf("document %d (%s %s) Doc carries namespace %q", i, doc.obj.Kind, doc.obj.Name, namespace)
		}
	}

	t.Run("a dropped file's documents are reported", func(t *testing.T) {
		t.Parallel()
		problems := sbxDocumentProblems(got, slices.Delete(slices.Clone(wants), 4, 8))
		if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, services) }) {
			t.Fatalf("dropping %s from the table reported %q, want a problem naming the file", services, problems)
		}
	})

	t.Run("two swapped rows are reported", func(t *testing.T) {
		t.Parallel()
		swapped := slices.Clone(wants)
		swapped[8], swapped[9] = swapped[9], swapped[8]
		problems := sbxDocumentProblems(got, swapped)
		if !slices.ContainsFunc(problems, func(p string) bool {
			return strings.Contains(p, "kind/gateway-nodeport.yaml") && strings.Contains(p, "gt-cache/claim.yaml")
		}) {
			t.Fatalf("swapping the gateway NodePort and the claim rows reported %q, want a problem naming both files", problems)
		}
	})
}

// sbxDocumentsByFile returns SbxStatic's documents, each with its source file:
// the priority classes come from priority-classes.yaml, and the rest follow
// sbxStaticFileNames, the order SbxStatic reads them in. It fails when the files
// the structs are read from do not reproduce SbxStatic's output in order, so the
// file named for a document is the file it came from.
func sbxDocumentsByFile(t *testing.T) []sbxSourcedDocument {
	t.Helper()
	objects, err := SbxStatic()
	if err != nil {
		t.Fatalf("SbxStatic(): %v", err)
	}
	// The priority classes are the control cluster's file, read from the
	// control set's embed; every later document comes from the gt-sbx embed.
	type source struct {
		file string
		fsys fs.FS
	}
	sources := []source{{"priority-classes.yaml", staticManifests}}
	for _, file := range sbxStaticFileNames {
		sources = append(sources, source{file, sbxStaticManifests})
	}
	var sourced []sbxSourcedDocument
	for _, src := range sources {
		docs, err := staticObjects(src.fsys, []string{src.file})
		if err != nil {
			t.Fatalf("reading %s: %v", staticPath(src.file), err)
		}
		for _, obj := range docs {
			sourced = append(sourced, sbxSourcedDocument{file: src.file, obj: obj})
		}
	}
	if len(sourced) != len(objects) {
		t.Fatalf("the source files hold %d documents, SbxStatic() returned %d", len(sourced), len(objects))
	}
	for i := range sourced {
		if !bytes.Equal(sourced[i].obj.Doc, objects[i].Doc) {
			t.Fatalf("document %d from %s is not SbxStatic()'s document %d", i, staticPath(sourced[i].file), i)
		}
	}
	return sourced
}

// sbxDocumentProblems returns one line per document that differs from the row
// at its position, and one line when the counts differ: a dropped file or two
// swapped rows move every document after them.
func sbxDocumentProblems(got []sbxSourcedDocument, want []sbxWant) []string {
	var problems []string
	if len(got) != len(want) {
		problems = append(problems, fmt.Sprintf("SbxStatic() returned %d documents, want %d", len(got), len(want)))
	}
	for i := 0; i < len(got) && i < len(want); i++ {
		g, w := got[i].obj, want[i]
		if got[i].file != w.file || g.APIVersion != w.apiVersion || g.Kind != w.kind ||
			g.Name != w.name || g.Namespace != w.namespace || g.Cluster != w.cluster {
			problems = append(problems, fmt.Sprintf(
				"document %d = %s %s %s in %s (namespace %q, cluster %v), want %s %s %s in %s (namespace %q, cluster %v)",
				i, g.Kind, g.Name, g.APIVersion, got[i].file, g.Namespace, g.Cluster, w.kind, w.name, w.apiVersion, w.file, w.namespace, w.cluster))
		}
	}
	return problems
}

func TestSbxStaticRefusals(t *testing.T) {
	t.Parallel()
	t.Run("a control set with no priority class is refused", func(t *testing.T) {
		t.Parallel()
		control := func() ([]StaticObject, error) {
			return []StaticObject{{Kind: "Role", Name: "r", Namespace: "gt"}}, nil
		}
		_, err := sbxStaticObjects(control, fstest.MapFS{}, nil)
		if err == nil || !strings.Contains(err.Error(), "PriorityClass") {
			t.Fatalf("sbxStaticObjects() = %v, want a refusal naming the missing PriorityClass", err)
		}
	})

	t.Run("an unreadable control set is refused", func(t *testing.T) {
		t.Parallel()
		control := func() ([]StaticObject, error) { return nil, errors.New("planted failure") }
		_, err := sbxStaticObjects(control, fstest.MapFS{}, nil)
		if err == nil || !strings.Contains(err.Error(), "planted failure") {
			t.Fatalf("sbxStaticObjects() = %v, want the control set's error", err)
		}
		if !strings.Contains(err.Error(), "control cluster's static set") {
			t.Errorf("error %q does not say which set failed", err)
		}
	})

	t.Run("an unreadable gt-sbx file is refused", func(t *testing.T) {
		t.Parallel()
		control := func() ([]StaticObject, error) {
			return []StaticObject{{Kind: "PriorityClass", Name: "p", Cluster: true}}, nil
		}
		_, err := sbxStaticObjects(control, fstest.MapFS{}, []string{"gone.yaml"})
		if err == nil || !strings.Contains(err.Error(), staticPath("gone.yaml")) {
			t.Fatalf("sbxStaticObjects() = %v, want an error naming %s", err, staticPath("gone.yaml"))
		}
	})
}

// The //go:embed directive is the one list of what is embedded, and
// sbxStaticFileNames holds only the order. This holds the two equal, so a file
// added to the directive cannot be left out of the order and never applied.
func TestSbxStaticEmbeddedFilesWalk(t *testing.T) {
	t.Parallel()
	var embedded []string
	err := fs.WalkDir(sbxStaticManifests, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			embedded = append(embedded, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the embedded files: %v", err)
	}
	want := slices.Clone(sbxStaticFileNames)
	sort.Strings(embedded)
	sort.Strings(want)
	if !slices.Equal(embedded, want) {
		t.Fatalf("the embedded files are %q, want the files sbxStaticFileNames names %q", embedded, want)
	}
}

// Every file under the gt-sbx set's paths is either embedded or listed in
// SbxExcluded, so a new bootstrap or hand-applied file cannot be added silently.
func TestSbxStaticEveryFileAccountedFor(t *testing.T) {
	t.Parallel()

	t.Run("the repository tree accounts for every file", func(t *testing.T) {
		t.Parallel()
		excluded, err := excludedFiles("SbxExcluded", SbxExcluded)
		if err != nil {
			t.Fatalf("SbxExcluded: %v", err)
		}
		if problems := accountingProblemsFor(walkSbxFiles(t, "."), sbxStaticFileNames, excluded, "SbxExcluded"); len(problems) > 0 {
			t.Fatalf("gt-sbx files not accounted for:\n%s", strings.Join(problems, "\n"))
		}
	})

	t.Run("a new file under kind is refused", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		for _, rel := range []string{
			"kind/gateway-nodeport.yaml",
			"values/kind/control-plane-services.yaml",
			"gt-cache/claim.yaml",
		} {
			writeFile(t, root, rel)
		}
		for _, rel := range []string{
			"kind/up.sh",
			"values/kind/hosts.yaml",
			"values/kind.yaml",
			// A Go file is not a manifest the set applies.
			"gt-cache/claim_test.go",
		} {
			writeFileText(t, root, rel, "")
		}
		writeFile(t, root, "kind/new.yaml")
		excluded := []string{"kind/up.sh", "values/kind/hosts.yaml", "values/kind.yaml"}
		problems := accountingProblemsFor(walkSbxFiles(t, root), sbxStaticFileNames, excluded, "SbxExcluded")
		if len(problems) != 1 || !strings.Contains(problems[0], "kind/new.yaml") {
			t.Fatalf("problems = %q, want one naming kind/new.yaml", problems)
		}
	})

	t.Run("an excluded file that is not there is refused", func(t *testing.T) {
		t.Parallel()
		problems := accountingProblemsFor(nil, nil, []string{"kind/up.sh"}, "SbxExcluded")
		if len(problems) != 1 || !strings.Contains(problems[0], "kind/up.sh") {
			t.Fatalf("problems = %q, want one naming kind/up.sh", problems)
		}
	})

	t.Run("a row with no owner is refused", func(t *testing.T) {
		t.Parallel()
		rows := []struct{ File, Owner, Reason string }{
			{"deploy/k8s/kind/up.sh", "", "creates the cluster"},
		}
		_, err := excludedFiles("SbxExcluded", rows)
		if err == nil || !strings.Contains(err.Error(), "SbxExcluded") || !strings.Contains(err.Error(), "owner") {
			t.Fatalf("excludedFiles(%q) = %v, want a refusal naming the list and the missing owner", rows, err)
		}
	})
}

// The claim moved to SbxStatic: the control cluster's exclusion list no longer
// names it, and Static still returns its eight files' documents in the order
// staticFileNames names, with no PersistentVolumeClaim among them.
func TestSbxStaticKeepsControlSet(t *testing.T) {
	t.Parallel()

	t.Run("the claim is no longer excluded", func(t *testing.T) {
		t.Parallel()
		for _, row := range StaticExcluded {
			if row.File == "deploy/k8s/gt-cache/claim.yaml" {
				t.Fatalf("StaticExcluded still lists %s, which SbxStatic embeds", row.File)
			}
		}
	})

	t.Run("the eight control files are read in order", func(t *testing.T) {
		t.Parallel()
		want := []string{
			"bd-gateway-service-admission.yaml",
			"update-admission.yaml",
			"update-rbac.yaml",
			"sweep-rbac.yaml",
			"mac-watch-rbac.yaml",
			"priority-classes.yaml",
			"drift-rbac.yaml",
			"deployer-rbac.yaml",
		}
		if !slices.Equal(staticFileNames, want) {
			t.Fatalf("staticFileNames = %q, want %q", staticFileNames, want)
		}
		objects, err := Static()
		if err != nil {
			t.Fatalf("Static(): %v", err)
		}
		var joined []StaticObject
		for _, file := range want {
			docs, err := staticObjects(staticManifests, []string{file})
			if err != nil {
				t.Fatalf("reading %s: %v", staticPath(file), err)
			}
			if len(docs) == 0 {
				t.Fatalf("%s holds no document", staticPath(file))
			}
			for _, obj := range docs {
				if obj.Kind == "PersistentVolumeClaim" {
					t.Errorf("%s holds %s %s, which is a gt-sbx object", staticPath(file), obj.Kind, obj.Name)
				}
			}
			joined = append(joined, docs...)
		}
		if len(objects) != len(joined) {
			t.Fatalf("Static() returned %d documents, want the %d the eight files hold", len(objects), len(joined))
		}
		for i := range objects {
			if !bytes.Equal(objects[i].Doc, joined[i].Doc) {
				t.Fatalf("document %d of Static() is not document %d of the eight files", i, i)
			}
		}
	})
}

// sbxWalkedPaths are the paths the gt-sbx set accounts for, relative to the
// package directory: the kind bootstrap's directory and the chart values'
// kind directory and file, plus the shared cache claim's package.
var sbxWalkedPaths = []string{"kind", "values/kind", "values/kind.yaml", "gt-cache"}

// walkSbxFiles returns every manifest and bootstrap script under the gt-sbx
// set's paths, relative to root with slash separators. A path the set names
// must exist: a directory that vanished would otherwise read as fully
// accounted for.
func walkSbxFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	for _, rel := range sbxWalkedPaths {
		path := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("checking %s: %v", path, err)
		}
		if !info.IsDir() {
			files = append(files, rel)
			continue
		}
		err = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			switch strings.ToLower(filepath.Ext(p)) {
			case ".yaml", ".yml", ".sh":
				r, err := filepath.Rel(root, p)
				if err != nil {
					return err
				}
				files = append(files, filepath.ToSlash(r))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", path, err)
		}
	}
	sort.Strings(files)
	return files
}
