package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/deploy/k8s"
)

// staticDoc is a source object whose Doc is the one document the allowlist
// judges and Render writes.
func staticDoc(kind, name, namespace string) k8s.StaticObject {
	doc := fmt.Sprintf("apiVersion: v1\nkind: %s\nmetadata:\n  name: %s\n", kind, name)
	if namespace != "" {
		doc += "  namespace: " + namespace + "\n"
	}
	return k8s.StaticObject{Kind: kind, Name: name, Namespace: namespace, Doc: []byte(doc)}
}

// TestStaticScopesMatchTheEmbeddedSet holds the daemon's kind table equal to the
// set k8s.Static returns: every embedded kind is judged with the scope the
// embedder gave it, and the table carries no kind the set cannot hold.
func TestStaticScopesMatchTheEmbeddedSet(t *testing.T) {
	t.Parallel()
	objects, err := k8s.Static()
	if err != nil {
		t.Fatalf("k8s.Static: %v", err)
	}
	seen := make(map[string]bool, len(staticScopes))
	for _, object := range objects {
		seen[object.Kind] = true
		cluster, known := staticScopes[object.Kind]
		if !known {
			t.Errorf("k8s.Static returns %s %s, whose kind the daemon's table does not carry",
				object.Kind, object.Name)
			continue
		}
		if cluster != object.Cluster {
			t.Errorf("the daemon judges %s as cluster-scoped=%t, but k8s.Static says %t",
				object.Kind, cluster, object.Cluster)
		}
	}
	for kind := range staticScopes {
		if !seen[kind] {
			t.Errorf("the daemon's table carries %s, which k8s.Static does not return", kind)
		}
	}
}

// streamParts names the goldens of one live render, in the order Render writes
// them. Every file predates the static set, so a comparison against them does
// not rest on a golden this change created.
func streamParts(v Values) []string {
	parts := []string{
		"namespace.yaml",
		"service-account.yaml",
		"town-state-claim.yaml",
		"landing-work-claim.yaml",
		"dolt-backup-claim.yaml",
		"forgejo-service.yaml",
	}
	for _, file := range v.Secrets {
		parts = append(parts, "secret-"+file.Name+".yaml")
	}
	return append(parts,
		"secret-"+CloudCheckKeySecretName+".yaml",
		"secret-openshell.yaml",
		"deployment.yaml",
		"rolebinding-sweep-runner.yaml",
		"rolebinding-ci-cache-trim.yaml",
		"clusterrolebinding-sweep-reader.yaml",
	)
}

// goldenStream joins golden files dir/parts in order the way Render separates
// documents.
func goldenStream(t *testing.T, dir string, parts []string) []byte {
	t.Helper()
	var buf bytes.Buffer
	for i, part := range parts {
		if i > 0 {
			buf.WriteString("---\n")
		}
		data, err := os.ReadFile(filepath.Join(dir, part))
		if err != nil {
			t.Fatalf("reading %s: %v", part, err)
		}
		buf.Write(data)
	}
	return buf.Bytes()
}

// assertStream compares a render with the bytes a stream is held to.
func assertStream(t *testing.T, got, want []byte, what string) {
	t.Helper()
	if bytes.Equal(got, want) {
		return
	}
	gotLines, wantLines := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
	for i := 0; i < len(gotLines) && i < len(wantLines); i++ {
		if gotLines[i] != wantLines[i] {
			t.Fatalf("%s differs at line %d:\n got: %q\nwant: %q", what, i+1, gotLines[i], wantLines[i])
		}
	}
	t.Fatalf("%s holds %d lines, want %d", what, len(gotLines), len(wantLines))
}

// TestRenderStaticOff holds the stream a render without Values.IncludeStatic
// writes. The render carries no static objects, and its bytes are the
// per-document goldens that existed before the static set, joined in render
// order: the set adds nothing to a render that does not ask for it.
func TestRenderStaticOff(t *testing.T) {
	t.Parallel()
	v := goldenValues(t)
	m := mustBuild(t, v)
	if len(m.Static) != 0 {
		t.Fatalf("a render without Values.IncludeStatic carries %d static objects, want none", len(m.Static))
	}
	var buf bytes.Buffer
	if err := Render(&buf, m); err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := goldenStream(t, filepath.Join(packageDir(t), "testdata"), streamParts(v))
	assertStream(t, buf.Bytes(), want, "the stream without Values.IncludeStatic")
}

// TestRenderStaticOrder holds the stream a render with Values.IncludeStatic
// writes (testdata/static-set.yaml for the static documents): they land right
// after the ServiceAccount, the cluster-scoped ones first, so every admission
// policy exists before the first Service it may judge, and every other document
// stays on the goldens that predate the set.
func TestRenderStaticOrder(t *testing.T) {
	t.Parallel()
	v := goldenValues(t)
	v.IncludeStatic = true
	m := mustBuild(t, v)
	if len(m.Static) == 0 {
		t.Fatal("Values.IncludeStatic is set but the render carries no static objects")
	}

	var staticStream bytes.Buffer
	docs := make([]any, 0, len(m.Static))
	for _, object := range m.Static {
		docs = append(docs, rawDocument(object.Doc))
	}
	if err := encodeStream(&staticStream, docs...); err != nil {
		t.Fatalf("encoding the static documents: %v", err)
	}
	dir := filepath.Join(packageDir(t), "testdata")
	compareGolden(t, dir, "static-set.yaml", staticStream.Bytes())

	var buf bytes.Buffer
	if err := Render(&buf, m); err != nil {
		t.Fatalf("Render: %v", err)
	}
	parts := streamParts(v)
	want := goldenStream(t, dir, parts[:2])
	want = append(want, []byte("---\n")...)
	want = append(want, staticStream.Bytes()...)
	want = append(want, []byte("---\n")...)
	want = append(want, goldenStream(t, dir, parts[2:])...)
	assertStream(t, buf.Bytes(), want, "the stream with Values.IncludeStatic")

	rendered := streamDocuments(t, buf.Bytes())
	if got, want := len(rendered), len(parts)+len(m.Static); got != want {
		t.Fatalf("the stream holds %d documents, want %d: %d other goldens and %d static", got, want, len(parts), len(m.Static))
	}
	if rendered[0].Kind != "Namespace" || rendered[1].Kind != "ServiceAccount" {
		t.Fatalf("the stream opens with %s and %s, want the Namespace and the ServiceAccount", rendered[0].Kind, rendered[1].Kind)
	}
	first := 2
	if namespace := rendered[first].Metadata.Namespace; namespace != "" {
		t.Errorf("the first static document, %s %s, names namespace %q, so the cluster-scoped objects do not come first",
			rendered[first].Kind, rendered[first].Metadata.Name, namespace)
	}
	namespaced := false
	for i := first; i < first+len(m.Static); i++ {
		doc := rendered[i]
		if doc.Metadata.Namespace != "" {
			namespaced = true
		} else if namespaced {
			t.Errorf("static document %d, %s %s, is cluster-scoped and follows a namespaced one", i-first, doc.Kind, doc.Metadata.Name)
		}
	}

	firstService := -1
	for i, doc := range rendered {
		if doc.Kind == "Service" {
			firstService = i
			break
		}
	}
	if firstService < 0 {
		t.Fatal("the stream holds no Service, so the admission policies' order is not shown")
	}
	policies := 0
	for i, doc := range rendered {
		switch doc.Kind {
		case "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding":
			policies++
			if i > firstService {
				t.Errorf("%s %s is document %d, after the first Service at document %d", doc.Kind, doc.Metadata.Name, i, firstService)
			}
		}
	}
	if policies == 0 {
		t.Fatal("the stream holds no admission policy, so the order is not shown")
	}
}

// TestRenderStaticAllowlist drives the namespace refusal on the document the
// render writes: a namespaced document is refused unless it lands in the
// render's namespace, the sweep's or Forgejo's, and a document the allowlist
// cannot judge is refused rather than read as harmless. Every refusal names the
// object.
func TestRenderStaticAllowlist(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		objects         []k8s.StaticObject
		renderNamespace string
		wantErr         string
	}{
		{"the render's namespace", []k8s.StaticObject{staticDoc("Role", "stray-role", "gt")}, "gt", ""},
		{"a rehearsal's namespace", []k8s.StaticObject{staticDoc("Role", "stray-role", "gt-rehearsal")}, "gt-rehearsal", ""},
		{"the sweep's namespace", []k8s.StaticObject{staticDoc("Role", "stray-role", SweepNamespace)}, "gt", ""},
		{"Forgejo's namespace", []k8s.StaticObject{staticDoc("Role", "stray-role", CacheTrimNamespace)}, "gt", ""},
		{"a cluster-scoped object", []k8s.StaticObject{staticDoc("ClusterRole", "gt-sweep-reader", "")}, "gt", ""},
		{"a source field that disagrees with a cluster document", []k8s.StaticObject{
			{Kind: "Role", Name: "ignored", Namespace: "gt", Doc: staticDoc("ClusterRole", "gt-sweep-reader", "").Doc},
		}, "gt", ""},
		{"another namespace", []k8s.StaticObject{staticDoc("Role", "stray-role", "forgejo-trial")}, "gt", `Role stray-role is in namespace "forgejo-trial"`},
		{"a namespaced kind with no namespace", []k8s.StaticObject{staticDoc("Role", "stray-role", "")}, "gt", "namespaced but its document names no namespace"},
		{"a cluster kind naming a namespace", []k8s.StaticObject{staticDoc("ClusterRole", "gt-sweep-reader", "gt")}, "gt", "cluster-scoped but its document names namespace"},
		{"a cluster flag over a foreign document", []k8s.StaticObject{
			func() k8s.StaticObject {
				object := staticDoc("Role", "stray-role", "forgejo-trial")
				object.Cluster = true
				return object
			}(),
		}, "gt", `Role stray-role is in namespace "forgejo-trial"`},
		{"a kind the set does not carry", []k8s.StaticObject{staticDoc("ConfigMap", "stray", "gt")}, "gt", "holds a kind the static set does not carry"},
		{"a document that is not YAML", []k8s.StaticObject{{Doc: []byte("kind: [\n")}}, "gt", "not one YAML object"},
		{"a document holding two objects", []k8s.StaticObject{
			{Doc: []byte("kind: Role\nmetadata: {name: a, namespace: gt}\n---\nkind: Role\n")},
		}, "gt", "holds more than one YAML document"},
		{"a second document that does not parse", []k8s.StaticObject{
			{Doc: []byte("kind: Role\nmetadata: {name: a, namespace: gt}\n---\nkind: [\n")},
		}, "gt", "second document"},
		{"a document with no kind", []k8s.StaticObject{{Doc: []byte("metadata: {name: a}\n")}}, "gt", "names no kind"},
		{"a document with no name", []k8s.StaticObject{{Doc: []byte("kind: Role\nmetadata: {namespace: gt}\n")}}, "gt", "names no metadata.name"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := orderedStatic(tc.objects, tc.renderNamespace)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("orderedStatic refused %s: %v", tc.name, err)
				}
				if len(got) != len(tc.objects) {
					t.Errorf("orderedStatic returned %d objects, want %d", len(got), len(tc.objects))
				}
				return
			}
			if err == nil {
				t.Fatalf("orderedStatic accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("the refusal %q does not name %q", err, tc.wantErr)
			}
		})
	}
}

// TestStaticObjectsRefusesAFailedSource holds that a source that cannot be read
// fails the build rather than reading as an empty static set.
func TestStaticObjectsRefusesAFailedSource(t *testing.T) {
	t.Parallel()
	source := func() ([]k8s.StaticObject, error) {
		return nil, errors.New("the embed is unreadable")
	}
	if _, err := staticObjects(source, "gt"); err == nil {
		t.Fatal("staticObjects accepted a source that failed")
	} else if !strings.Contains(err.Error(), "the embed is unreadable") {
		t.Errorf("the refusal %q does not carry the source's error", err)
	}
}

// TestBuildRefusesAFailedStaticSource drives the Build call site: a source that
// cannot be read fails the build rather than yielding a Manifests whose Static
// is empty. k8s.Static cannot fail, so the test injects the failing source.
func TestBuildRefusesAFailedStaticSource(t *testing.T) {
	t.Parallel()
	v := goldenValues(t)
	v.IncludeStatic = true
	v.staticSource = func() ([]k8s.StaticObject, error) {
		return nil, errors.New("the embed is unreadable")
	}
	m, err := Build(v)
	if err == nil {
		t.Fatal("Build accepted a static source that failed")
	}
	if !strings.Contains(err.Error(), "the embed is unreadable") {
		t.Errorf("the refusal %q does not carry the source's error", err)
	}
	if len(m.Static) != 0 {
		t.Errorf("a refused build returned %d static objects", len(m.Static))
	}
}

// errorWriter is a writer whose every write fails.
type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

// TestRenderRefusesAFailedWrite drives the render's write refusal on a stream
// carrying the static set: a writer that fails returns the error rather than
// reporting the stream written.
func TestRenderRefusesAFailedWrite(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, goldenValues(t))
	m.Static = []k8s.StaticObject{staticDoc("Role", "stray-role", DefaultNamespace)}
	err := Render(errorWriter{errors.New("disk full")}, m)
	if err == nil {
		t.Fatal("Render reported a stream written to a writer that failed")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("the refusal %q does not carry the writer's error", err)
	}
}

// TestEncodeStreamRefusesADocumentItCannotEncode drives the encoder's failure
// branch: a document the YAML encoder cannot marshal fails the stream. Render
// cannot be driven into it, since every document it writes comes from a
// Manifests field the encoder accepts, so the test calls the seam with a node
// of an unknown kind, which the encoder refuses.
func TestEncodeStreamRefusesADocumentItCannotEncode(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	err := encodeStream(&buf, &yaml.Node{Kind: 99})
	if err == nil {
		t.Fatal("encodeStream wrote a document the YAML encoder cannot marshal")
	}
	if !strings.Contains(err.Error(), "encoding the manifest") {
		t.Errorf("the refusal %q does not say the encoder failed", err)
	}
	if buf.Len() != 0 {
		t.Errorf("the refused stream wrote %d bytes, want none", buf.Len())
	}
}

// TestOrderedStaticPutsClusterScopedFirst holds the render order: the
// cluster-scoped documents first, then the namespaced ones, each group in the
// order it arrived, with the scope read from each document.
func TestOrderedStaticPutsClusterScopedFirst(t *testing.T) {
	t.Parallel()
	objects := []k8s.StaticObject{
		staticDoc("ServiceAccount", "a", "gt"),
		staticDoc("ValidatingAdmissionPolicy", "p", ""),
		staticDoc("Role", "b", "gt"),
		staticDoc("PriorityClass", "c", ""),
	}
	got, err := orderedStatic(objects, "gt")
	if err != nil {
		t.Fatalf("orderedStatic: %v", err)
	}
	want := []string{"ValidatingAdmissionPolicy/p", "PriorityClass/c", "ServiceAccount/a", "Role/b"}
	var names []string
	for _, object := range got {
		names = append(names, object.Kind+"/"+object.Name)
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("orderedStatic returned %q, want %q", names, want)
	}
}

// streamDocument is the part of a rendered document the order test reads.
type streamDocument struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
}

// streamDocuments decodes every document of a rendered stream.
func streamDocuments(t *testing.T, stream []byte) []streamDocument {
	t.Helper()
	var docs []streamDocument
	dec := yaml.NewDecoder(bytes.NewReader(stream))
	for {
		var doc streamDocument
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs
		}
		if err != nil {
			t.Fatalf("decoding the rendered stream: %v", err)
		}
		docs = append(docs, doc)
	}
}
