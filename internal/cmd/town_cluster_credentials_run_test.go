package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clusterCredentialsKubeconfig is the kubeconfig the fake runner prints, with a self-signed certificate.
func clusterCredentialsKubeconfig(t *testing.T, commonName string) []byte {
	t.Helper()
	certPEM := deployerTestCertificate(t, commonName)
	return []byte(fmt.Sprintf("users:\n- name: %s\n  user:\n    client-certificate-data: %q\n",
		commonName, base64.StdEncoding.EncodeToString(certPEM)))
}

// TestClusterCredentialsRun drives runClusterCredentials: a role writes and prints, a refusal does neither.
func TestClusterCredentialsRun(t *testing.T) {
	t.Parallel()

	t.Run("a role writes the kubeconfig to the out path and prints the formatted line", func(t *testing.T) {
		t.Parallel()
		role, clientName := "apply", "gt-deployer-apply"
		doc := clusterCredentialsKubeconfig(t, clientName)
		outPath := filepath.Join(t.TempDir(), "kubeconfig.yaml")
		var argv []string
		run := func(_ context.Context, got []string) ([]byte, error) {
			argv = got
			return doc, nil
		}
		var out bytes.Buffer
		if err := runClusterCredentials(context.Background(), run, &out, clusterSbx, role, outPath); err != nil {
			t.Fatalf("runClusterCredentials(%s, %s): %v", clusterSbx, role, err)
		}
		if !strings.Contains(strings.Join(argv, " "), "--client-name "+clientName) {
			t.Errorf("runner argv = %v, want the %s client name", argv, clientName)
		}
		written, err := os.ReadFile(outPath)
		if err != nil {
			t.Fatalf("kubeconfig was not written to the out path: %v", err)
		}
		if !bytes.Equal(written, doc) {
			t.Errorf("kubeconfig written to the out path differs from the runner's output")
		}
		want := role + "\tCN=" + clientName + "\t" + time.Unix(1<<31-1, 0).UTC().Format(time.RFC3339) + "\n"
		if out.String() != want {
			t.Errorf("printed %q, want %q", out.String(), want)
		}
	})

	t.Run("a credentialRequest refusal is returned before run and prints nothing", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name, cluster, role string
			hasOut              bool
			want                string
		}{
			{"a wrong cluster", "control", "apply", true, "is not allowed"},
			{"a missing role", clusterSbx, "", true, "a missing role"},
			{"an unknown role", clusterSbx, "admin", true, "unknown role"},
			{"a missing out", clusterSbx, "apply", false, "a missing out"},
		} {
			outPath := ""
			if tc.hasOut {
				outPath = filepath.Join(t.TempDir(), "kubeconfig.yaml")
			}
			run := func(_ context.Context, argv []string) ([]byte, error) {
				t.Errorf("%s: runner reached with argv %v, want the refusal before it", tc.name, argv)
				return nil, nil
			}
			var out bytes.Buffer
			err := runClusterCredentials(context.Background(), run, &out, tc.cluster, tc.role, outPath)
			if err == nil {
				t.Errorf("%s: runClusterCredentials() = nil, want a refusal", tc.name)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s: error %q does not name %q, so it is not the credentialRequest refusal", tc.name, err, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("%s: printed %q beside the refusal, want nothing", tc.name, out.String())
			}
			if outPath != "" {
				if _, err := os.Stat(outPath); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("%s: out path exists after the refusal (stat error %v), want none", tc.name, err)
				}
			}
		}
	})
}
