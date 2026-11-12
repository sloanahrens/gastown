package main

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// writeMacFile writes a file under dir and returns its path.
func writeMacFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// TestRunFileArgRewrittenToStdin: through the kubernetes target, a Mac file a
// flag names reaches the target as the call's stdin with the flag's value "-",
// whether the path is absolute or relative to the caller's directory.
func TestRunFileArgRewrittenToStdin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		tool string
		args func(abs string) []string
		want []string
	}{
		{"bd create --body-file", "bd", func(abs string) []string { return []string{"create", "--title", "t", "--body-file", abs} },
			[]string{"bd", "create", "--title", "t", "--body-file", "-"}},
		{"bd update --body-file=", "bd", func(abs string) []string { return []string{"update", "gt-x", "--body-file=" + abs} },
			[]string{"bd", "update", "gt-x", "--body-file=-"}},
		{"bd comments add -f", "bd", func(abs string) []string { return []string{"comments", "add", "gt-x", "-f", abs} },
			[]string{"bd", "comments", "add", "gt-x", "-f", "-"}},
		{"bd note --file relative", "bd", func(string) []string { return []string{"note", "gt-x", "--file", "body.md"} },
			[]string{"bd", "note", "gt-x", "--file", "-"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.writeConfig(t, kubernetesConfig)
			path := writeMacFile(t, h.town, "body.md", "the bead text\n")
			if exit := h.run(append([]string{tc.tool}, tc.args(path)...)...); exit != 0 {
				t.Fatalf("run = %d, want 0; stderr: %s", exit, h.stderr)
			}
			if !slices.Equal(h.fake.call.Argv, tc.want) {
				t.Errorf("target argv = %q, want %q", h.fake.call.Argv, tc.want)
			}
			if h.fake.call.Stdin == nil {
				t.Fatal("the target got no stdin; the file was not fed")
			}
			if got, _ := io.ReadAll(h.fake.call.Stdin); string(got) != "the bead text\n" {
				t.Errorf("stdin = %q, want the file's bytes", got)
			}
		})
	}
}

// TestRunFileArgRefusals: a call the pod could not serve is refused before the
// target is called, with the cause on stderr and exit 1.
func TestRunFileArgRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args func(t *testing.T, dir string) []string
		want string
	}{
		{"two files", func(t *testing.T, dir string) []string {
			return []string{"bd", "create", "--body-file", writeMacFile(t, dir, "a.md", "a"), "--design-file", writeMacFile(t, dir, "b.md", "b")}
		}, "names two files"},
		{"file with --stdin", func(t *testing.T, dir string) []string {
			return []string{"bd", "create", "--stdin", "--body-file", writeMacFile(t, dir, "a.md", "a")}
		}, "stdin is already in use"},
		{"missing file", func(t *testing.T, dir string) []string {
			return []string{"bd", "create", "--body-file", filepath.Join(dir, "gone.md")}
		}, "cannot read"},
		{"directory", func(t *testing.T, dir string) []string {
			return []string{"bd", "create", "--body-file", dir}
		}, "cannot read"},
		{"over the size limit on disk", func(t *testing.T, dir string) []string {
			return []string{"bd", "create", "--body-file", writeMacFile(t, dir, "big.md", strings.Repeat("x", maxFileArgBytes+1))}
		}, "larger than the 1048576-byte limit"},
		{"not a regular file", func(t *testing.T, dir string) []string {
			link := filepath.Join(dir, "fifo.md")
			if err := syscall.Mkfifo(link, 0o600); err != nil {
				t.Fatalf("mkfifo: %v", err)
			}
			return []string{"bd", "create", "--body-file", link}
		}, "not a regular file"},
		{"file the tool opens as a path", func(t *testing.T, dir string) []string {
			return []string{"bd", "create", "-f", writeMacFile(t, dir, "plan.md", "p")}
		}, "names a Mac file the pod cannot read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.writeConfig(t, kubernetesConfig)
			exit := h.run(tc.args(t, h.town)...)
			if exit != 1 || h.fake.launched || !strings.Contains(h.stderr.String(), tc.want) {
				t.Fatalf("run = %d, launched %v, stderr %q; want exit 1, no launch, and %q", exit, h.fake.launched, h.stderr, tc.want)
			}
		})
	}
}

// TestRunNoFileArgUntouched: a call that names no file, and any call through the
// docker target, reaches the target with its arguments unchanged and no stdin of
// the wrapper's making.
func TestRunNoFileArgUntouched(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, kubernetesConfig)
	if exit := h.run("bd", "show", "gt-x"); exit != 0 {
		t.Fatalf("run = %d; stderr: %s", exit, h.stderr)
	}
	if want := []string{"bd", "show", "gt-x"}; !slices.Equal(h.fake.call.Argv, want) || h.fake.call.Stdin != nil {
		t.Errorf("target got argv %q, stdin %v; want %q and no stdin", h.fake.call.Argv, h.fake.call.Stdin, want)
	}

	d := newHarness(t)
	path := writeMacFile(t, d.town, "body.md", "x")
	if exit := d.run("bd", "create", "--body-file", path); exit != 0 {
		t.Fatalf("docker run = %d; stderr: %s", exit, d.stderr)
	}
	if want := []string{"bd", "create", "--body-file", path}; !slices.Equal(d.fake.call.Argv, want) || d.fake.call.Stdin != nil {
		t.Errorf("docker target got argv %q, stdin %v; want the call unchanged", d.fake.call.Argv, d.fake.call.Stdin)
	}
}

// TestRunFileArgSizeBoundIsCheckedOnTheBytesRead: a file whose stat says it is
// small but whose read returns more than the bound is refused, so a file that
// grows between the two reads is not sent.
func TestRunFileArgSizeBoundIsCheckedOnTheBytesRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, kubernetesConfig)
	path := writeMacFile(t, h.town, "grows.md", "small")
	read := h.surface.readFile
	h.surface.readFile = func(name string) ([]byte, error) {
		if name == path {
			return []byte(strings.Repeat("x", maxFileArgBytes+1)), nil
		}
		return read(name)
	}
	exit := h.run("bd", "create", "--body-file", path)
	if exit != 1 || h.fake.launched || !strings.Contains(h.stderr.String(), "cannot read") || !strings.Contains(h.stderr.String(), "byte limit") {
		t.Fatalf("run = %d, launched %v, stderr %q; want exit 1, no launch and the size refusal", exit, h.fake.launched, h.stderr)
	}
}

// TestRunFileArgSizeBoundAtTheLimitIsSent: a file of exactly the bound is sent whole.
func TestRunFileArgSizeBoundAtTheLimitIsSent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writeConfig(t, kubernetesConfig)
	path := writeMacFile(t, h.town, "edge.md", strings.Repeat("x", maxFileArgBytes))
	if exit := h.run("bd", "create", "--body-file", path); exit != 0 {
		t.Fatalf("run = %d; stderr: %s", exit, h.stderr)
	}
	if got, _ := io.ReadAll(h.fake.call.Stdin); len(got) != maxFileArgBytes {
		t.Errorf("stdin = %d bytes, want %d", len(got), maxFileArgBytes)
	}
}
