package daemon

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/deploy/k8s"
)

// TestRenderStaticOff holds the stream a render without Values.IncludeStatic
// writes: the static set adds nothing, so it is byte-for-byte the stream the
// renderer wrote before the static set existed (testdata/stream.yaml).
func TestRenderStaticOff(t *testing.T) {
	t.Parallel()
	m := mustBuild(t, goldenValues(t))
	if len(m.Static) != 0 {
		t.Fatalf("a render without Values.IncludeStatic carries %d static objects, want none", len(m.Static))
	}
	var buf bytes.Buffer
	if err := Render(&buf, m); err != nil {
		t.Fatalf("Render: %v", err)
	}
	compareGolden(t, filepath.Join(packageDir(t), "testdata"), "stream.yaml", buf.Bytes())
}

// TestRenderStaticOrder holds the stream a render with Values.IncludeStatic
// writes: the static documents land right after the ServiceAccount, the
// cluster-scoped ones first, so every admission policy exists before the first
// Service it may refuse (testdata/stream-include-static.yaml).
func TestRenderStaticOrder(t *testing.T) {
	t.Parallel()
	v := goldenValues(t)
	v.IncludeStatic = true
	m := mustBuild(t, v)
	if len(m.Static) == 0 {
		t.Fatal("Values.IncludeStatic is set but the render carries no static objects")
	}

	var buf bytes.Buffer
	if err := Render(&buf, m); err != nil {
		t.Fatalf("Render: %v", err)
	}
	compareGolden(t, filepath.Join(packageDir(t), "testdata"), "stream-include-static.yaml", buf.Bytes())
	docs := streamDocuments(t, buf.Bytes())

	start := -1
	for i, doc := range docs {
		if doc.Kind == "ServiceAccount" && doc.Metadata.Name == ServiceAccountName {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("the stream holds no ServiceAccount %s to place the static set after", ServiceAccountName)
	}
	if start+len(m.Static) > len(docs) {
		t.Fatalf("the stream holds %d documents after the ServiceAccount, fewer than the %d static objects", len(docs)-start, len(m.Static))
	}
	for i, object := range m.Static {
		doc := docs[start+i]
		if doc.Kind != object.Kind || doc.Metadata.Name != object.Name || doc.Metadata.Namespace != object.Namespace {
			t.Errorf("static document %d after the ServiceAccount is %s/%s in %q, want %s/%s in %q",
				i, doc.Kind, doc.Metadata.Name, doc.Metadata.Namespace, object.Kind, object.Name, object.Namespace)
		}
	}
	if !m.Static[0].Cluster {
		t.Errorf("the first static object is %s %s, which is namespaced", m.Static[0].Kind, m.Static[0].Name)
	}
	namespaced := false
	for i, object := range m.Static {
		if !object.Cluster {
			namespaced = true
		}
		if object.Cluster && namespaced {
			t.Errorf("static object %d %s %s is cluster-scoped and follows a namespaced object", i, object.Kind, object.Name)
		}
	}

	firstService := -1
	for i, doc := range docs {
		if doc.Kind == "Service" {
			firstService = i
			break
		}
	}
	if firstService < 0 {
		t.Fatal("the stream holds no Service, so the admission policies' order is not shown")
	}
	policies := 0
	for i, doc := range docs {
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

// TestRenderStaticAllowlist drives the namespace refusal: a namespaced static
// object is refused unless it lands in the render's namespace, the sweep's or
// Forgejo's, and every refusal names the object.
func TestRenderStaticAllowlist(t *testing.T) {
	t.Parallel()
	role := func(namespace string) []k8s.StaticObject {
		return []k8s.StaticObject{{Kind: "Role", Name: "stray-role", Namespace: namespace}}
	}
	tests := []struct {
		name            string
		objects         []k8s.StaticObject
		renderNamespace string
		wantErr         string
	}{
		{"the render's namespace", role("gt"), "gt", ""},
		{"a rehearsal's namespace", role("gt-rehearsal"), "gt-rehearsal", ""},
		{"the sweep's namespace", role(SweepNamespace), "gt", ""},
		{"Forgejo's namespace", role(CacheTrimNamespace), "gt", ""},
		{"a cluster-scoped object", []k8s.StaticObject{{Kind: "ClusterRole", Name: "gt-sweep-reader", Cluster: true}}, "gt", ""},
		{"another namespace", role("forgejo-trial"), "gt", "Role stray-role"},
		{"no namespace", role(""), "gt", "Role stray-role"},
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

// TestOrderedStaticPutsClusterScopedFirst holds the render order: the
// cluster-scoped objects first, then the namespaced ones, each group in the
// order it arrived.
func TestOrderedStaticPutsClusterScopedFirst(t *testing.T) {
	t.Parallel()
	objects := []k8s.StaticObject{
		{Kind: "ServiceAccount", Name: "a", Namespace: "gt"},
		{Kind: "ValidatingAdmissionPolicy", Name: "p", Cluster: true},
		{Kind: "Role", Name: "b", Namespace: "gt"},
		{Kind: "PriorityClass", Name: "c", Cluster: true},
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
	if !slices.Equal(names, want) {
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
