package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/diskmaint"
	"github.com/steveyegge/gastown/internal/diskmaint/dockerimg"
	"github.com/steveyegge/gastown/internal/diskmaint/registry"
)

// dockerHost is a placeholder socket, not the VM's own path; the host test runs
// a second, different one, so a target that ignored its config cannot pass it.
const dockerHost = "unix:///placeholder/.colima/cloud/docker.sock"

// imgID is an image id: a sha256 digest, the shape registry.ValidDigest accepts.
func imgID(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

// call is one command the target ran and the environment it ran with.
type call struct {
	env  []string
	args []string
}

// container is one container the fake daemon holds: its id, the image it
// references and its state.
type container struct {
	id, image, state string
}

// The container states a test drives. docker ps lists a stopped or created
// container only with -a, and docker refuses to remove an image either names.
const (
	stateRunning = "running"
	stateCreated = "created"
	stateStopped = "stopped"
)

// fakeDocker answers the docker commands the target runs from canned output and
// records every one, so a test asserts what was asked for and never reaches a
// real daemon.
type fakeDocker struct {
	calls      []call
	images     string
	imagesErr  error
	digests    map[string][]string
	digestsErr error
	// digestsRaw, when set, is the inspect output as given, so a test can drive
	// a read that answers for fewer images than it was asked about.
	digestsRaw string
	// containers is every container the daemon holds, whatever its state.
	containers    []container
	containersErr error
	// containersRaw, when set, is the container inspect output as given.
	containersRaw string
	// rmErr is the error a removal of one image id returns.
	rmErr map[string]error
}

func (f *fakeDocker) run(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, call{env: slices.Clone(env), args: slices.Clone(args)})
	if name != "docker" || len(args) == 0 {
		return nil, errors.New("unexpected command")
	}
	switch args[0] {
	case "image":
		if len(args) < 2 {
			return nil, errors.New("unexpected command")
		}
		switch args[1] {
		case "ls":
			return []byte(f.images), f.imagesErr
		case "inspect":
			return f.inspect(args[2:])
		case "rm":
			return f.remove(args[2:])
		}
	case "ps":
		return f.ps(args[1:])
	case "inspect":
		return f.containerImages(args[1:])
	}
	return nil, errors.New("unexpected command")
}

// ps answers docker ps: every container when the list is all (-a), and the
// running ones alone otherwise, as docker does.
func (f *fakeDocker) ps(args []string) ([]byte, error) {
	if f.containersErr != nil {
		return nil, f.containersErr
	}
	if !slices.Contains(args, "-q") {
		return nil, errors.New("unexpected docker ps")
	}
	all := slices.Contains(args, "-a")
	var b strings.Builder
	for _, c := range f.containers {
		if !all && c.state != stateRunning {
			continue
		}
		b.WriteString(c.id)
		b.WriteByte('\n')
	}
	return []byte(b.String()), nil
}

// containerImages answers docker inspect --format {{.Image}} for the container
// ids it was given, so the in-use set is built from what the fake holds.
func (f *fakeDocker) containerImages(args []string) ([]byte, error) {
	if f.containersRaw != "" {
		return []byte(f.containersRaw), nil
	}
	if len(args) < 3 || args[0] != "--format" || args[1] != "{{.Image}}" {
		return nil, errors.New("unexpected container inspect")
	}
	byID := make(map[string]string, len(f.containers))
	for _, c := range f.containers {
		byID[c.id] = c.image
	}
	var b strings.Builder
	for _, id := range args[2:] {
		b.WriteString(byID[id])
		b.WriteByte('\n')
	}
	return []byte(b.String()), nil
}

// remove answers docker image rm: the argv is spelled out here, so a removal
// that names a tag, drops -no-prune or removes several ids at once fails.
func (f *fakeDocker) remove(args []string) ([]byte, error) {
	if len(args) != 2 || args[0] != "--no-prune" || !registry.ValidDigest(args[1]) {
		return nil, errors.New("unexpected image rm")
	}
	return nil, f.rmErr[args[1]]
}

// inspect answers docker image inspect for the ids of the given argv: every
// asked id's registry refs, or an empty list for an image that carries none, as
// docker prints a local-only image's RepoDigests. The format is spelled out here,
// so a plan that asked docker for another field fails.
func (f *fakeDocker) inspect(args []string) ([]byte, error) {
	if f.digestsErr != nil {
		return nil, f.digestsErr
	}
	if len(args) < 3 || args[0] != "--format" || args[1] != "{{.Id}} {{json .RepoDigests}}" {
		return nil, errors.New("unexpected image inspect")
	}
	if f.digestsRaw != "" {
		return []byte(f.digestsRaw), nil
	}
	var b strings.Builder
	for _, id := range args[2:] {
		refs := f.digests[id]
		if refs == nil {
			refs = []string{}
		}
		line, err := json.Marshal(refs)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "%s %s\n", id, line)
	}
	return []byte(b.String()), nil
}

// ledger is a throwaway ledger; the skeleton's Measure reads none of it.
func ledger(t *testing.T) *diskmaint.Ledger {
	t.Helper()
	return diskmaint.NewLedger(filepath.Join(t.TempDir(), "ledger.jsonl"), 0)
}

// newTarget builds a target with the placeholder socket that answers from f.
func newTarget(t *testing.T, f *fakeDocker) *Target {
	t.Helper()
	target, err := NewTarget(Config{DockerHost: dockerHost, Run: f.run, Ledger: ledger(t)})
	if err != nil {
		t.Fatalf("NewTarget: %v", err)
	}
	return target
}

// imageLines renders images as docker image ls --format {{json .}} prints them.
func imageLines(t *testing.T, images ...dockerimg.Image) string {
	t.Helper()
	var b strings.Builder
	for _, im := range images {
		line, err := json.Marshal(im)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String()
}

func TestNewTargetRefusesIncompleteConfig(t *testing.T) {
	t.Parallel()
	run := (&fakeDocker{}).run
	cases := []struct {
		name    string
		cfg     Config
		wantErr error
	}{
		{name: "no docker host", cfg: Config{Run: run, Ledger: ledger(t)}, wantErr: errNoDockerHost},
		{name: "no runner", cfg: Config{DockerHost: dockerHost, Ledger: ledger(t)}, wantErr: errNoRunner},
		{name: "no ledger", cfg: Config{DockerHost: dockerHost, Run: run}, wantErr: errNoLedger},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, err := NewTarget(tc.cfg)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("NewTarget err = %v, want %v", err, tc.wantErr)
			}
			if target != nil {
				t.Error("NewTarget returned a target beside the error")
			}
		})
	}
}

func TestNewTargetAcceptsACompleteConfig(t *testing.T) {
	t.Parallel()
	target, err := NewTarget(Config{DockerHost: dockerHost, Run: (&fakeDocker{}).run, Ledger: ledger(t)})
	if err != nil {
		t.Fatalf("NewTarget err = %v", err)
	}
	if target == nil {
		t.Fatal("NewTarget returned no target and no error")
	}
}

func TestNameIsCloud(t *testing.T) {
	t.Parallel()
	if got := newTarget(t, &fakeDocker{}).Name(); got != "cloud" {
		t.Errorf("Name() = %q, want cloud", got)
	}
}

func TestEveryDaemonCallCarriesTheConfiguredHost(t *testing.T) {
	t.Parallel()
	for _, host := range []string{
		"unix:///placeholder/.colima/cloud/docker.sock",
		"unix:///other/.colima/cloud/docker.sock",
	} {
		f := &fakeDocker{images: imageLines(t, dockerimg.Image{Repository: "<none>", Tag: "<none>", ID: imgID('1'), Size: "1MB"})}
		target, err := NewTarget(Config{DockerHost: host, Run: f.run, Ledger: ledger(t)})
		if err != nil {
			t.Fatalf("NewTarget: %v", err)
		}

		if _, err := target.Measure(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(f.calls) != 1 {
			t.Fatalf("Measure made %d docker calls, want 1", len(f.calls))
		}
		if want := []string{"DOCKER_HOST=" + host}; !slices.Equal(f.calls[0].env, want) {
			t.Errorf("docker call %v ran with env %v, want %v", f.calls[0].args, f.calls[0].env, want)
		}
	}
}

func TestMeasureSumsEveryImage(t *testing.T) {
	t.Parallel()
	f := &fakeDocker{images: imageLines(t,
		dockerimg.Image{Repository: "<none>", Tag: "<none>", ID: imgID('1'), Size: "1.5GB"},
		dockerimg.Image{Repository: "fractals-nextjs", Tag: "abc1234", ID: imgID('2'), Size: "200MB"},
		dockerimg.Image{Repository: "<none>", Tag: "<none>", ID: imgID('3'), Size: "0B"},
	)}
	target := newTarget(t, f)

	m, err := target.Measure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Used != 1_700_000_000 {
		t.Errorf("Used = %d, want 1700000000", m.Used)
	}
	if m.Capacity != 0 {
		t.Errorf("Capacity = %d, want 0: docker reports no disk capacity", m.Capacity)
	}
}

func TestMeasureCountsAnImageOnceForEachTag(t *testing.T) {
	t.Parallel()
	// docker prints one row per tag, each carrying the image's id and size.
	f := &fakeDocker{images: imageLines(t,
		dockerimg.Image{Repository: "fractals-nextjs", Tag: "abc1234", ID: imgID('1'), Size: "200MB"},
		dockerimg.Image{Repository: "fractals-nextjs", Tag: "latest", ID: imgID('1'), Size: "200MB"},
		dockerimg.Image{Repository: "<none>", Tag: "<none>", ID: imgID('2'), Size: "50MB"},
	)}
	target := newTarget(t, f)

	m, err := target.Measure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if m.Used != 250_000_000 {
		t.Errorf("Used = %d, want 250000000 (the shared image once, plus the dangling one)", m.Used)
	}
}

func TestMeasureNamesTheCloudVMDisk(t *testing.T) {
	t.Parallel()
	f := &fakeDocker{images: imageLines(t,
		dockerimg.Image{Repository: "<none>", Tag: "<none>", ID: imgID('1'), Size: "1MB"},
		dockerimg.Image{Repository: "fractals-nextjs", Tag: "abc1234", ID: imgID('2'), Size: "2MB"},
	)}
	target := newTarget(t, f)

	m, err := target.Measure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := "cloud-vm: 2 images on the daemon"; m.Detail != want {
		t.Errorf("Detail = %q, want %q", m.Detail, want)
	}
}

func TestMeasureRefusesAnUnreadableDisk(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		f    fakeDocker
		want error
	}{
		{name: "failed image list", f: fakeDocker{imagesErr: errors.New("SECRET stderr")}, want: dockerimg.ErrListImages},
		{name: "image line that does not parse", f: fakeDocker{images: "not json\n"}, want: dockerimg.ErrParse},
		{
			name: "image size that does not parse",
			f:    fakeDocker{images: imageLines(t, dockerimg.Image{Repository: "<none>", Tag: "<none>", ID: imgID('1'), Size: "12XB"})},
			want: dockerimg.ErrSize,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.f
			target := newTarget(t, &f)

			m, err := target.Measure(context.Background())
			if !errors.Is(err, tc.want) {
				t.Fatalf("Measure err = %v, want %v", err, tc.want)
			}
			if m != (diskmaint.Measurement{}) {
				t.Errorf("Measurement = %+v, want the zero value: a failed read is not a reading", m)
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Errorf("refusal echoes command output: %v", err)
			}
		})
	}
}
