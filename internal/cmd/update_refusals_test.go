package cmd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/update"
)

// TestUpdateRollbackRefusesAFailingSeam holds each failure the rollback command
// reports before it moves the daemon: the town, the deps, the lock, the swap and
// the registry each have a refusal, and none changes the daemon.
func TestUpdateRollbackRefusesAFailingSeam(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		prepare func(t *testing.T, f *imageRollbackFixture)
		mutate  func(seams *updateSeams)
		wantErr string
	}{
		{
			name:    "the town root cannot be found",
			mutate:  func(s *updateSeams) { s.FindTownRoot = func() (string, error) { return "", errors.New("no town") } },
			wantErr: "finding the town root",
		},
		{
			name: "the deps cannot be prepared",
			mutate: func(s *updateSeams) {
				s.DepsFactory = func(string) (update.Deps, error) { return update.Deps{}, errors.New("no deps") }
			},
			wantErr: "preparing",
		},
		{
			name: "the update lock cannot be taken",
			prepare: func(t *testing.T, f *imageRollbackFixture) {
				// The runtime directory is a file, so the lock's directory cannot be made.
				if err := os.RemoveAll(filepath.Join(f.townRoot, ".runtime")); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(f.townRoot, ".runtime"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "taking the update lock",
		},
		{
			name:    "no daemon image swap is wired",
			mutate:  func(s *updateSeams) { s.ImageSwap = nil },
			wantErr: "no daemon image swap or escalation sink is wired",
		},
		{
			name: "the daemon image swap cannot be built",
			mutate: func(s *updateSeams) {
				s.ImageSwap = func(string) (update.ImageSwap, error) { return nil, errors.New("no compose project") }
			},
			wantErr: "no compose project",
		},
		{
			name:    "no registry is wired",
			mutate:  func(s *updateSeams) { s.Images.Registry = nil },
			wantErr: "no registry is wired",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newImageRollbackFixture(t, true)
			f.record(t, update.ImageRecord{Current: f.running, Previous: f.previous})
			if tt.prepare != nil {
				tt.prepare(t, f)
			}
			_, err := f.runRollbackWith(t, tt.mutate)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("gt update rollback error = %v, want %q", err, tt.wantErr)
			}
			if len(f.swap.applied) != 0 {
				t.Errorf("the daemon was put on %+v, want no swap", f.swap.applied)
			}
		})
	}
}

// TestUpdateImagesFlagRefusesAnUnwiredOrFailingSwap holds the install's refusals
// when --images names a good file but the daemon swap cannot be had: nothing
// runs, so no polecat is drained.
func TestUpdateImagesFlagRefusesAnUnwiredOrFailingSwap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		imageSwap func(townRoot string) (update.ImageSwap, error)
		wantErr   string
	}{
		{
			name:    "no daemon image swap is wired",
			wantErr: "no daemon image swap or escalation sink is wired",
		},
		{
			name: "the daemon image swap cannot be built",
			imageSwap: func(string) (update.ImageSwap, error) {
				return nil, errors.New("no compose project")
			},
			wantErr: "preparing the daemon image install",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			images := filepath.Join(townRoot, "images.yaml")
			if err := imagevalues.Write(images, imagevalues.Values{Daemon: swapDaemonRef, Polecat: swapPolecatRef, Commit: rollbackNewCommit}); err != nil {
				t.Fatal(err)
			}
			clock := &rollbackTestClock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
			ran := false
			seams := updateSeams{
				FindTownRoot: func() (string, error) { return townRoot, nil },
				DepsFactory: func(string) (update.Deps, error) {
					return update.Deps{Store: &update.Store{Dir: update.Dir(townRoot)}, Clock: clock, Sleep: clock.sleep}, nil
				},
				Run: func(context.Context, update.Deps, update.Options) (update.State, error) {
					ran = true
					return update.State{}, nil
				},
				ImageSwap: tt.imageSwap,
				Notifier:  func(string) notify.Notifier { return notifyfake.New() },
			}
			c := newUpdateCmdWith(seams)
			c.SetArgs([]string{"--images", images})
			c.SetOut(new(strings.Builder))
			c.SetErr(new(strings.Builder))
			err := c.Execute()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("gt update --images error = %v, want %q", err, tt.wantErr)
			}
			if ran {
				t.Error("the run started before the daemon image install was wired")
			}
		})
	}
}

// TestRealImageSwapRefusesATownWithoutAContainerDaemon holds the production swap's
// refusals: a settings file that does not parse, and a town that names no
// compose project, so there is no container daemon to move.
func TestRealImageSwapRefusesATownWithoutAContainerDaemon(t *testing.T) {
	t.Parallel()
	t.Run("a daemon settings file that does not parse is refused", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		path := config.DaemonPatrolConfigPath(townRoot)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := realImageSwap(townRoot); err == nil {
			t.Fatal("realImageSwap over unparseable settings returned no error")
		}
	})
	t.Run("a town that names no compose project is refused", func(t *testing.T) {
		t.Parallel()
		if _, err := realImageSwap(t.TempDir()); err == nil || !strings.Contains(err.Error(), "compose project") {
			t.Fatalf("realImageSwap error = %v, want the missing compose project named", err)
		}
	})
}

// TestComposeDaemonSwapRefusesAFailingStep holds each step of the swap that can
// fail before it is done: the compose file read and rewrite, the settings the
// pin is written to, and the recreate of the daemon.
func TestComposeDaemonSwapRefusesAFailingStep(t *testing.T) {
	t.Parallel()
	pair := update.ImagePair{Daemon: swapDaemonRef, Polecat: swapPolecatRef, Commit: rollbackNewCommit}
	t.Run("a compose file that cannot be read is refused", func(t *testing.T) {
		t.Parallel()
		f := newSwapFixture(t, true)
		s := f.swap("town")
		s.ComposeFile = filepath.Join(filepath.Dir(f.compose), "missing.yml")
		if err := s.Apply(context.Background(), pair); err == nil || !strings.Contains(err.Error(), "reading the compose file") {
			t.Fatalf("Apply error = %v, want the compose read refusal", err)
		}
	})
	t.Run("a compose file with no daemon service is refused", func(t *testing.T) {
		t.Parallel()
		f := newSwapFixture(t, true)
		if err := os.WriteFile(f.compose, []byte("services:\n  dolt:\n    image: \"dolt:2\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := f.swap("town").Apply(context.Background(), pair); err == nil || !strings.Contains(err.Error(), "names no image line") {
			t.Fatalf("Apply error = %v, want the missing daemon service named", err)
		}
		if len(f.calls) != 0 {
			t.Errorf("commands = %+v, want none", f.calls)
		}
	})
	t.Run("a daemon settings file that does not exist is refused", func(t *testing.T) {
		t.Parallel()
		f := newSwapFixture(t, true)
		s := f.swap("town")
		s.ConfigPath = filepath.Join(filepath.Dir(f.config), "missing.json")
		if err := s.Apply(context.Background(), pair); err == nil || !strings.Contains(err.Error(), "do not exist") {
			t.Fatalf("Apply error = %v, want the missing settings named", err)
		}
	})
	t.Run("a compose file that cannot be rewritten is refused with the settings unchanged", func(t *testing.T) {
		t.Parallel()
		f := newSwapFixture(t, true)
		// The write is refused by the seam, not by a mode bit, so the case holds
		// when the suite runs as root, which ignores the mode bits.
		s := f.swap("town")
		s.writeCompose = func(string, []byte) error { return errors.New("read-only file system") }
		beforeCfg, err := os.ReadFile(f.config)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Apply(context.Background(), pair); err == nil || !strings.Contains(err.Error(), "writing the compose file") {
			t.Fatalf("Apply error = %v, want the compose write refusal", err)
		}
		if len(f.calls) != 0 {
			t.Errorf("commands = %+v, want none after a failed write", f.calls)
		}
		if after := readSwapFile(t, f.config); after != string(beforeCfg) {
			t.Error("the daemon settings changed although the compose write failed first")
		}
	})
	t.Run("a daemon settings file that does not parse is refused before anything changes", func(t *testing.T) {
		t.Parallel()
		f := newSwapFixture(t, true)
		if err := os.WriteFile(f.config, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		before := readSwapFile(t, f.compose)
		if err := f.swap("town").Apply(context.Background(), pair); err == nil || !strings.Contains(err.Error(), "reading the daemon settings") {
			t.Fatalf("Apply error = %v, want the unreadable settings named", err)
		}
		if got := readSwapFile(t, f.compose); got != before {
			t.Error("the compose file changed although the settings could not be read")
		}
		if len(f.calls) != 0 {
			t.Errorf("commands = %+v, want none", f.calls)
		}
	})
	t.Run("a settings file that is gone when the pin is set puts the compose file back", func(t *testing.T) {
		t.Parallel()
		f := newSwapFixture(t, true)
		s := f.swap("town")
		s.writeSettings = func(_ string, mutate func(*config.DaemonPatrolConfig, bool) error) error {
			return mutate(nil, false)
		}
		before := readSwapFile(t, f.compose)
		if err := s.Apply(context.Background(), pair); err == nil || !strings.Contains(err.Error(), "do not exist") {
			t.Fatalf("Apply error = %v, want the missing settings named", err)
		}
		if got := readSwapFile(t, f.compose); got != before {
			t.Error("the compose file was not put back after the settings were found missing")
		}
		if len(f.calls) != 0 {
			t.Errorf("commands = %+v, want none", f.calls)
		}
	})
	t.Run("a settings file that names no runner when the pin is set puts the compose file back", func(t *testing.T) {
		t.Parallel()
		f := newSwapFixture(t, true)
		s := f.swap("town")
		s.writeSettings = func(_ string, mutate func(*config.DaemonPatrolConfig, bool) error) error {
			return mutate(&config.DaemonPatrolConfig{}, true)
		}
		before := readSwapFile(t, f.compose)
		if err := s.Apply(context.Background(), pair); err == nil || !strings.Contains(err.Error(), "no tier_sweep runner") {
			t.Fatalf("Apply error = %v, want the missing runner named", err)
		}
		if got := readSwapFile(t, f.compose); got != before {
			t.Error("the compose file was not put back after the runner was found missing")
		}
		if len(f.calls) != 0 {
			t.Errorf("commands = %+v, want none", f.calls)
		}
	})
}

// TestImagePairRegistryCheckRefusesAnUnwiredOrUnreadableRegistry holds the
// rollback's registry check: each seam it needs, the registry it names, and the
// credential it reads must be there before any digest is asked for.
func TestImagePairRegistryCheckRefusesAnUnwiredOrUnreadableRegistry(t *testing.T) {
	t.Parallel()
	//testpolicy:allow no-network — the registry check is built against an httptest server: it answers in-process on loopback, and no request reaches it in these refusals
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	address := testRegistryFor(t, server)
	base := func() updateImagesSeams {
		return updateImagesSeams{
			Registry: func(string) (imagevalues.Registry, error) { return address, nil },
			Credentials: func() (string, string, error) {
				return testUpdateImagesUser, testUpdateImagesToken, nil
			},
			HTTPClient: server.Client(),
		}
	}
	tests := []struct {
		name    string
		mutate  func(s *updateImagesSeams)
		wantErr string
	}{
		{name: "no registry is wired", mutate: func(s *updateImagesSeams) { s.Registry = nil }, wantErr: "no registry is wired"},
		{name: "no credential reader is wired", mutate: func(s *updateImagesSeams) { s.Credentials = nil }, wantErr: "no credential reader is wired"},
		{name: "no HTTP client is wired", mutate: func(s *updateImagesSeams) { s.HTTPClient = nil }, wantErr: "no HTTP client is wired"},
		{
			name: "the registry cannot be resolved",
			mutate: func(s *updateImagesSeams) {
				s.Registry = func(string) (imagevalues.Registry, error) { return imagevalues.Registry{}, errors.New("no forgejo") }
			},
			wantErr: "no forgejo",
		},
		{
			name: "the registry address is not a bare host",
			mutate: func(s *updateImagesSeams) {
				s.Registry = func(string) (imagevalues.Registry, error) {
					return imagevalues.Registry{Scheme: "https", Host: "bad host", Owner: testUpdateImagesOwner}, nil
				}
			},
			wantErr: "not a bare host:port",
		},
		{
			name: "the viewer credential cannot be read",
			mutate: func(s *updateImagesSeams) {
				s.Credentials = func() (string, string, error) { return "", "", errors.New("no token file") }
			},
			wantErr: "reading the viewer credential",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			seams := base()
			tt.mutate(&seams)
			if _, err := imagePairRegistryCheck(seams, "/town"); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("imagePairRegistryCheck error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// TestRegistryDigestCheckRefusesADigestTheRegistryDoesNotHold holds each way the
// digest read can fail: a digest that is not well formed, a registry that fails
// the read, one that answers another digest, and one whose bytes do not hash to
// the digest it names. A registry that holds the pair is accepted.
func TestRegistryDigestCheckRefusesADigestTheRegistryDoesNotHold(t *testing.T) {
	t.Parallel()
	good := rollbackManifest(imagevalues.DaemonImage)
	goodDigest := testDigestOf(good)
	tests := []struct {
		name    string
		digest  string
		serve   func(w http.ResponseWriter, r *http.Request, digest string)
		wantErr string
	}{
		{
			name:    "a digest that is not sha256 hex is refused without a request",
			digest:  "sha256:short",
			serve:   func(http.ResponseWriter, *http.Request, string) {},
			wantErr: "not sha256:<64 lowercase hex>",
		},
		{
			name:   "a registry that fails the read is refused",
			digest: goodDigest,
			serve: func(w http.ResponseWriter, _ *http.Request, _ string) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantErr: "reading the daemon image",
		},
		{
			name:   "a registry that answers another digest is refused",
			digest: goodDigest,
			serve: func(w http.ResponseWriter, _ *http.Request, _ string) {
				w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("e", 64))
				_, _ = w.Write(good)
			},
			wantErr: "answered digest",
		},
		{
			name:   "a registry whose bytes do not hash to the digest is refused",
			digest: goodDigest,
			serve: func(w http.ResponseWriter, _ *http.Request, digest string) {
				w.Header().Set("Docker-Content-Digest", digest)
				_, _ = w.Write([]byte(`{"schemaVersion":2,"image":"tampered"}`))
			},
			wantErr: "hash to",
		},
		{
			name:   "a registry that holds the digest is accepted",
			digest: goodDigest,
			serve: func(w http.ResponseWriter, _ *http.Request, digest string) {
				w.Header().Set("Docker-Content-Digest", digest)
				_, _ = w.Write(good)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			//testpolicy:allow no-network — the digest check reads a manifest from an httptest server: it answers in-process on loopback, and the command's only client is the one this test wires to it
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tt.serve(w, r, tt.digest)
			}))
			t.Cleanup(server.Close)
			address := testRegistryFor(t, server)
			seams := updateImagesSeams{
				Registry:    func(string) (imagevalues.Registry, error) { return address, nil },
				Credentials: func() (string, string, error) { return testUpdateImagesUser, testUpdateImagesToken, nil },
				HTTPClient:  server.Client(),
			}
			check, err := imagePairRegistryCheck(seams, "/town")
			if err != nil {
				t.Fatalf("imagePairRegistryCheck: %v", err)
			}
			pair := update.ImagePair{
				Daemon:  address.Ref(imagevalues.DaemonImage, tt.digest),
				Polecat: address.Ref(imagevalues.PolecatImage, tt.digest),
				Commit:  rollbackNewCommit,
			}
			err = check(context.Background(), pair)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("check of a held pair: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("check error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
