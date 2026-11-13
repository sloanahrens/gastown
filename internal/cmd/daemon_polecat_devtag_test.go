package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/diskmaint/registry"
	"github.com/steveyegge/gastown/internal/imagevalues"
)

// podDevTagImage is the manifest the fake registry serves at the dev tag, and
// the config blob that manifest names.
func podDevTagImage() (manifest, config []byte) {
	config = []byte(`{"architecture":"arm64","config":{"Labels":{"` + imagevalues.CommitLabel + `":"0123456789ab"}}}`)
	manifest = []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"` + testDigestOf(config) + `"}}`)
	return manifest, config
}

// podDevTagRegistry answers the registry client's two endpoints in process: the
// dev tag serves manifest with its own digest in the header, and the config blob
// the manifest names is served beside it. A nil manifest is a 404 for the tag;
// status, when set, answers every request with it. Every path is recorded.
type podDevTagRegistry struct {
	manifest []byte
	config   []byte
	status   int
	paths    []string
}

func (r *podDevTagRegistry) RoundTrip(req *http.Request) (*http.Response, error) {
	r.paths = append(r.paths, req.URL.Path)
	header := http.Header{}
	if r.status != 0 {
		return &http.Response{StatusCode: r.status, Header: header, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	}
	switch req.URL.Path {
	case "/v2/" + imagevalues.PolecatImage + "/manifests/" + podPolecatTag:
		if r.manifest == nil {
			break
		}
		header.Set("Docker-Content-Digest", testDigestOf(r.manifest))
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(bytes.NewReader(r.manifest)), Request: req}, nil
	case "/v2/" + imagevalues.PolecatImage + "/blobs/" + testDigestOf(r.config):
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(bytes.NewReader(r.config)), Request: req}, nil
	}
	return &http.Response{StatusCode: http.StatusNotFound, Header: header, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
}

// polecatDevTagPatrol is daemon settings naming host as the registry the daemon
// publishes the dev tag to.
func polecatDevTagPatrol(host string) *config.DaemonPatrolConfig {
	return &config.DaemonPatrolConfig{
		Patrols: &config.PatrolsConfig{
			TierSweep: &config.TierSweepConfig{
				Runner: &config.TierSweepRunnerConfig{ImageRegistry: host},
			},
		},
	}
}

// TestDaemonPolecatDevTag holds the subcommand the pod pair check runs to the
// settings and registry it reads: the tag's digest, an absent tag, a registry
// error, and each input that cannot be read. The answer is one line of JSON,
// and the registry base is built from the setting.
func TestDaemonPolecatDevTag(t *testing.T) {
	t.Parallel()
	const host = "localhost:5001"
	manifest, configBytes := podDevTagImage()
	wantDigest := testDigestOf(manifest)
	rows := []struct {
		name     string
		manifest []byte
		status   int
		patrol   func() (*config.DaemonPatrolConfig, error)
		townErr  error
		want     polecatDevTagRead
		wantErr  string
	}{
		{
			name:     "the registry holds the dev tag",
			manifest: manifest,
			want:     polecatDevTagRead{Present: true, Digest: wantDigest},
		},
		{
			name: "the registry holds no dev tag",
		},
		{
			name:    "the registry answers an error",
			status:  http.StatusInternalServerError,
			wantErr: "reading gastown-polecat:dev in localhost:5001",
		},
		{
			name:    "the settings name no image_registry",
			patrol:  func() (*config.DaemonPatrolConfig, error) { return &config.DaemonPatrolConfig{}, nil },
			wantErr: "name no patrols.tier_sweep.runner.image_registry",
		},
		{
			name:    "the settings do not load",
			patrol:  func() (*config.DaemonPatrolConfig, error) { return nil, errors.New("the settings do not parse") },
			wantErr: "reading the daemon settings",
		},
		{
			name:    "the town root is not found",
			townErr: errors.New("not in a Gas Town workspace"),
			wantErr: "finding the town root",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			patrol := row.patrol
			if patrol == nil {
				patrol = func() (*config.DaemonPatrolConfig, error) { return polecatDevTagPatrol(host), nil }
			}
			fake := &podDevTagRegistry{manifest: row.manifest, config: configBytes, status: row.status}
			var bases []string
			seams := daemonPolecatDevTagSeams{
				FindTownRoot: func() (string, error) {
					if row.townErr != nil {
						return "", row.townErr
					}
					return t.TempDir(), nil
				},
				LoadPatrol: func(string) (*config.DaemonPatrolConfig, error) { return patrol() },
				NewClient: func(base string) *registry.Client {
					bases = append(bases, base)
					return &registry.Client{Base: base, HTTP: &http.Client{Transport: fake}}
				},
			}
			c := newDaemonPolecatDevTagCmd(seams)
			var out, errOut bytes.Buffer
			c.SetOut(&out)
			c.SetErr(&errOut)
			err := c.ExecuteContext(context.Background())
			if row.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), row.wantErr) {
					t.Fatalf("error = %v, want one naming %q", err, row.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("command: %v", err)
			}
			var got polecatDevTagRead
			if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &got); err != nil {
				t.Fatalf("output %q is not JSON: %v", out.String(), err)
			}
			if got != row.want {
				t.Errorf("answer = %+v, want %+v", got, row.want)
			}
			if len(bases) != 1 || bases[0] != "http://"+host {
				t.Errorf("registry base = %q, want http://%s", bases, host)
			}
		})
	}
}
