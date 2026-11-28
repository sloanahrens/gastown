package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestForgejoServerFlag(t *testing.T) {
	t.Parallel()
	const hosts = "values/lab/hosts.yaml"
	read := fakeRead(map[string]string{hosts: "control_plane_host: cp.example\n"})

	t.Run("prints the manifest with the hosts file's database host", func(t *testing.T) {
		t.Parallel()
		var out, errw bytes.Buffer
		if code := run([]string{"--forgejo-server", hosts}, read, &out, &errw); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, errw.String())
		}
		if n := strings.Count(out.String(), "\nkind: "); n != 4 {
			t.Errorf("%d documents, want 4:\n%s", n, out.String())
		}
		if !strings.Contains(out.String(), "cp.example:5442") || strings.Contains(out.String(), "CONTROL_PLANE_HOST:5442") {
			t.Errorf("the database host is not filled from the hosts file:\n%s", out.String())
		}
	})

	t.Run("refusals exit non-zero with empty stdout", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name string
			args []string
			file string
			code int
			want string // a substring of the reason on stderr
		}{
			{"no host", []string{"--forgejo-server", hosts}, "registry_host: r\n", 1, "no control_plane_host"},
			{"host with a newline", []string{"--forgejo-server", hosts}, "control_plane_host: \"a\\n---\\nkind: Secret\"\n", 1, "not a host name"},
			{"the unfilled placeholder", []string{"--forgejo-server", hosts}, "control_plane_host: CONTROL_PLANE_HOST\n", 1, "unfilled placeholder"},
			{"unreadable file", []string{"--forgejo-server", "missing/hosts.yaml"}, "", 1, "cannot read"},
			{"no operand", []string{"--forgejo-server"}, "", 2, "usage:"},
			{"two operands", []string{"--forgejo-server", hosts, hosts}, "", 2, "usage:"},
			{"flag after the file", []string{hosts, "--forgejo-server"}, "", 2, "usage:"},
		}
		for _, tc := range cases {
			var out, errw bytes.Buffer
			if code := run(tc.args, fakeRead(map[string]string{hosts: tc.file}), &out, &errw); code != tc.code {
				t.Errorf("%s: exit %d, want %d (stderr %q)", tc.name, code, tc.code, errw.String())
			}
			if out.Len() != 0 {
				t.Errorf("%s: stdout is not empty: %q", tc.name, out.String())
			}
			if !strings.Contains(errw.String(), tc.want) {
				t.Errorf("%s: stderr %q does not name %q", tc.name, errw.String(), tc.want)
			}
		}
	})
}
