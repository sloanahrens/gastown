package cmd

import (
	"strings"
	"testing"
)

// TestClusterCaptureArgsRef holds parseClusterCaptureRef to the <kind>/<name>
// shape: one slash, both parts non-empty and no whitespace. A refused ref
// returns no kind or name and an error that names the ref.
func TestClusterCaptureArgsRef(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		ref      string
		wantKind string
		wantName string
		wantErr  bool
	}{
		{name: "a deployment ref parses to kind and name", ref: "deployment/daemon", wantKind: "deployment", wantName: "daemon"},
		{name: "a name may hold dots and dashes", ref: "job/gt.daemon-1", wantKind: "job", wantName: "gt.daemon-1"},
		{name: "a ref with no slash is refused", ref: "deployment", wantErr: true},
		{name: "an empty ref is refused", ref: "", wantErr: true},
		{name: "a ref with two slashes is refused", ref: "deployment/daemon/x", wantErr: true},
		{name: "a ref with an empty kind is refused", ref: "/daemon", wantErr: true},
		{name: "a ref with an empty name is refused", ref: "deployment/", wantErr: true},
		{name: "a ref with only a slash is refused", ref: "/", wantErr: true},
		{name: "a ref with a space in its name is refused", ref: "deployment/dae mon", wantErr: true},
		{name: "a ref with a leading space is refused", ref: " deployment/daemon", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			kind, name, err := parseClusterCaptureRef(c.ref)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseClusterCaptureRef(%q) = %q, %q, nil; want an error", c.ref, kind, name)
				}
				if !strings.Contains(err.Error(), c.ref) {
					t.Errorf("the error %q does not name the ref %q", err, c.ref)
				}
				if kind != "" || name != "" {
					t.Errorf("a refused ref returned kind %q and name %q; want both empty", kind, name)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseClusterCaptureRef(%q) error: %v", c.ref, err)
			}
			if kind != c.wantKind || name != c.wantName {
				t.Errorf("parseClusterCaptureRef(%q) = %q, %q; want %q, %q", c.ref, kind, name, c.wantKind, c.wantName)
			}
		})
	}
}

// TestClusterCaptureArgsOutPath holds clusterCaptureOutPath to the paths a
// capture may write: inside an absolute work dir, resolved and cleaned. A
// refused path returns an empty path and an error.
func TestClusterCaptureArgsOutPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		workDir string
		out     string
		want    string
		wantErr bool
	}{
		{name: "a relative out resolves under workDir", workDir: "/work/rig", out: "capture.yaml", want: "/work/rig/capture.yaml"},
		{name: "a relative out is cleaned and stays under workDir", workDir: "/work/rig", out: "a//b/../c.yaml", want: "/work/rig/a/c.yaml"},
		{name: "an absolute out inside workDir is kept", workDir: "/work/rig", out: "/work/rig/out/c.yaml", want: "/work/rig/out/c.yaml"},
		{name: "a name that starts with two dots is inside workDir", workDir: "/work/rig", out: "..foo", want: "/work/rig/..foo"},
		{name: "a workDir with a trailing slash is cleaned", workDir: "/work/rig/", out: "c.yaml", want: "/work/rig/c.yaml"},
		{name: "a dot-dot escape is refused", workDir: "/work/rig", out: "../x", wantErr: true},
		{name: "a dot-dot escape after a directory is refused", workDir: "/work/rig", out: "a/../../x", wantErr: true},
		{name: "an absolute path elsewhere is refused", workDir: "/work/rig", out: "/etc/passwd", wantErr: true},
		{name: "an absolute dot-dot escape is refused", workDir: "/work/rig", out: "/work/rig/../other", wantErr: true},
		{name: "a sibling that shares workDir's prefix is refused", workDir: "/work/rig", out: "/work/rigx/c", wantErr: true},
		{name: "workDir itself as a relative out is refused", workDir: "/work/rig", out: ".", wantErr: true},
		{name: "workDir itself as an absolute out is refused", workDir: "/work/rig", out: "/work/rig", wantErr: true},
		{name: "a relative out that resolves to workDir is refused", workDir: "/work/rig", out: "a/..", wantErr: true},
		{name: "an empty out is refused", workDir: "/work/rig", out: "", wantErr: true},
		{name: "a relative workDir is refused", workDir: "work/rig", out: "c.yaml", wantErr: true},
		{name: "an empty workDir is refused", workDir: "", out: "c.yaml", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := clusterCaptureOutPath(c.workDir, c.out)
			if c.wantErr {
				if err == nil {
					t.Fatalf("clusterCaptureOutPath(%q, %q) = %q, nil; want an error", c.workDir, c.out, got)
				}
				if got != "" {
					t.Errorf("a refused path returned %q; want an empty path", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("clusterCaptureOutPath(%q, %q) error: %v", c.workDir, c.out, err)
			}
			if got != c.want {
				t.Errorf("clusterCaptureOutPath(%q, %q) = %q; want %q", c.workDir, c.out, got, c.want)
			}
		})
	}
}
