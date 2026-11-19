package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// adminKubeconfigText is a kubeconfig whose control-cluster context carries an
// embedded token. The token's value must never reach the command's output.
const adminKubeconfigText = `apiVersion: v1
kind: Config
clusters:
  - name: control
    cluster:
      server: https://10.20.30.40:6443
contexts:
  - name: colima-gt-control
    context:
      cluster: control
      user: control-admin
users:
  - name: control-admin
    user:
      token: token-value-never-printed
`

// cleanKubeconfigText is the sealed form: the control-cluster user references
// its certificate and key by path, so no credential is embedded.
const cleanKubeconfigText = `apiVersion: v1
kind: Config
clusters:
  - name: control
    cluster:
      server: https://10.20.30.40:6443
contexts:
  - name: colima-gt-control
    context:
      cluster: control
      user: control-deployer
users:
  - name: control-deployer
    user:
      client-certificate: deployer.crt
      client-key: deployer.key
`

// writeKubeconfig writes text as a kubeconfig in a new temp dir and returns
// its path, so no test reads a real home directory.
func writeKubeconfig(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// adminCheckGetenv answers from vars alone, so a test never reads the host's
// own environment.
func adminCheckGetenv(vars map[string]string) func(string) string {
	return func(name string) string { return vars[name] }
}

// TestTownDeployerAdminCheck: the command exits 1 on a kubeconfig with an admin
// credential and 0 on a clean one, and prints only the context and user names.
func TestTownDeployerAdminCheck(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		kubeconfig string
		wantErr    bool
		wantOut    []string
		wantNotOut []string
	}{
		{
			name:       "an embedded admin credential exits 1 with names only",
			kubeconfig: adminKubeconfigText,
			wantErr:    true,
			wantOut:    []string{"colima-gt-control", "control-admin", "an embedded token"},
			wantNotOut: []string{"token-value-never-printed"},
		},
		{
			name:       "a clean kubeconfig exits 0",
			kubeconfig: cleanKubeconfigText,
			wantOut:    []string{"No kubeconfig context reaches the control cluster"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := runTownDeployerAdminCheckIn(townDeployerAdminCheckEnv{
				kubeconfig: writeKubeconfig(t, tc.kubeconfig),
				getenv:     adminCheckGetenv(nil),
				out:        &out,
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("runTownDeployerAdminCheckIn = %v, wantErr %v", err, tc.wantErr)
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output does not hold %q:\n%s", want, out.String())
				}
			}
			for _, notWant := range tc.wantNotOut {
				if strings.Contains(out.String(), notWant) {
					t.Errorf("output holds the credential value %q:\n%s", notWant, out.String())
				}
			}
		})
	}
}

// TestTownDeployerAdminCheckPod: the kubeconfig is the host's, so the command
// refuses inside a pod before it reads one.
func TestTownDeployerAdminCheckPod(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runTownDeployerAdminCheckIn(townDeployerAdminCheckEnv{
		kubeconfig: writeKubeconfig(t, cleanKubeconfigText),
		getenv:     adminCheckGetenv(map[string]string{podEnv: "10.20.30.40"}),
		out:        &out,
	})
	if err == nil || !strings.Contains(err.Error(), "pod") {
		t.Fatalf("runTownDeployerAdminCheckIn = %v, want a pod refusal", err)
	}
	if out.Len() != 0 {
		t.Errorf("output = %q, want none: no kubeconfig is read inside a pod", out.String())
	}
}

// TestTownDeployerAdminCheckResolvesPath: the flag wins, then $KUBECONFIG, then
// $HOME/.kube/config, and the resolved file is the one checked. Empty
// $KUBECONFIG entries are skipped: each row below reads a file that the test
// can tell apart (an admin kubeconfig fails, a clean one passes), so an empty
// entry that became an empty path would show as a read of no file.
func TestTownDeployerAdminCheckResolvesPath(t *testing.T) {
	t.Parallel()
	adminPath := writeKubeconfig(t, adminKubeconfigText)
	cleanPath := writeKubeconfig(t, cleanKubeconfigText)
	sep := string(filepath.ListSeparator)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".kube"), 0o755); err != nil {
		t.Fatal(err)
	}
	homePath := filepath.Join(home, ".kube", "config")
	if err := os.WriteFile(homePath, []byte(adminKubeconfigText), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		kubeconfig string
		env        map[string]string
		wantErr    bool
	}{
		{
			name:       "the flag wins over KUBECONFIG",
			kubeconfig: cleanPath,
			env:        map[string]string{"KUBECONFIG": adminPath},
		},
		{
			name:    "KUBECONFIG names the file",
			env:     map[string]string{"KUBECONFIG": adminPath},
			wantErr: true,
		},
		{
			name:    "HOME names the default",
			env:     map[string]string{"HOME": home},
			wantErr: true,
		},
		{
			name:    "a leading empty KUBECONFIG entry is skipped and the file after it is checked",
			env:     map[string]string{"KUBECONFIG": sep + adminPath},
			wantErr: true,
		},
		{
			name:    "a trailing empty KUBECONFIG entry is skipped and the file before it is checked",
			env:     map[string]string{"KUBECONFIG": adminPath + sep},
			wantErr: true,
		},
		{
			name: "empty entries around a clean file leave the clean file checked",
			env:  map[string]string{"KUBECONFIG": sep + cleanPath + sep},
		},
		{
			name:    "a KUBECONFIG of empty entries only reads as unset and falls back to HOME",
			env:     map[string]string{"KUBECONFIG": sep + sep, "HOME": home},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := runTownDeployerAdminCheckIn(townDeployerAdminCheckEnv{
				kubeconfig: tc.kubeconfig,
				getenv:     adminCheckGetenv(tc.env),
				out:        &out,
			})
			if (err != nil) != tc.wantErr {
				t.Fatalf("runTownDeployerAdminCheckIn = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestTownDeployerAdminCheckRefusals: a kubeconfig path that names no single
// file is refused before the check reads anything, and a file the check cannot
// read is an error rather than a pass.
func TestTownDeployerAdminCheckRefusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		flag    string
		env     map[string]string
		want    string
		checked bool
	}{
		{
			name: "KUBECONFIG names two files",
			env:  map[string]string{"KUBECONFIG": "/kube/one.config:/kube/two.config"},
			want: "names 2 files",
		},
		{
			name: "KUBECONFIG names two files beside an empty entry",
			env:  map[string]string{"KUBECONFIG": "/kube/one.config::/kube/two.config"},
			want: "names 2 files",
		},
		{
			name: "HOME is not set",
			want: "HOME is not set",
		},
		{
			name:    "the kubeconfig does not exist",
			flag:    filepath.Join(t.TempDir(), "missing"),
			want:    "did not run",
			checked: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := runTownDeployerAdminCheckIn(townDeployerAdminCheckEnv{
				kubeconfig: tc.flag,
				getenv:     adminCheckGetenv(tc.env),
				out:        &out,
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runTownDeployerAdminCheckIn = %v, want an error holding %q", err, tc.want)
			}
			if !tc.checked && out.Len() != 0 {
				t.Errorf("output = %q, want none: the check ran without a path to read", out.String())
			}
		})
	}
}

// TestTownDeployerAdminCheckPrintFailure: a result the command cannot print is
// an error, so a run whose output was lost does not exit 0.
func TestTownDeployerAdminCheckPrintFailure(t *testing.T) {
	t.Parallel()
	err := runTownDeployerAdminCheckIn(townDeployerAdminCheckEnv{
		kubeconfig: writeKubeconfig(t, cleanKubeconfigText),
		getenv:     adminCheckGetenv(nil),
		out:        failingDeployerWriter{},
	})
	if err == nil || !strings.Contains(err.Error(), "printing the admin-kubeconfig result") {
		t.Fatalf("runTownDeployerAdminCheckIn = %v, want a print failure", err)
	}
}

// TestTownDeployerAdminCheckFlagFailure: a command that does not define
// --kubeconfig is refused rather than checked with an empty path.
func TestTownDeployerAdminCheckFlagFailure(t *testing.T) {
	t.Parallel()
	err := runTownDeployerAdminCheck(&cobra.Command{Use: "admin-check"}, nil)
	if err == nil || !strings.Contains(err.Error(), "reading --kubeconfig") {
		t.Fatalf("runTownDeployerAdminCheck = %v, want a flag read failure", err)
	}
}
