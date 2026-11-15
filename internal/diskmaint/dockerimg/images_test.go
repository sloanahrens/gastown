package dockerimg

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// dockerHost is a placeholder daemon socket; every test checks it reaches docker.
const dockerHost = "unix:///placeholder/.colima/gt-sbx/docker.sock"

// imgID is an image id: a sha256 digest, the shape registry.ValidDigest accepts.
func imgID(c byte) string { return "sha256:" + strings.Repeat(string(c), 64) }

// call is one command the client ran and the environment it ran with.
type call struct {
	env  []string
	args []string
}

// fakeDocker answers the docker commands the client runs from canned output and
// records every one, so a test asserts what was asked for and never reaches a
// real daemon.
type fakeDocker struct {
	calls         []call
	images        string
	imagesErr     error
	containers    string
	containersErr error
	inspect       string
	inspectErr    error
}

func (f *fakeDocker) run(_ context.Context, env []string, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, call{env: env, args: args})
	if name != "docker" || len(args) == 0 {
		return nil, errors.New("unexpected command")
	}
	switch args[0] {
	case "image":
		return []byte(f.images), f.imagesErr
	case "ps":
		return []byte(f.containers), f.containersErr
	case "inspect":
		return []byte(f.inspect), f.inspectErr
	}
	return nil, errors.New("unexpected docker command")
}

func (f *fakeDocker) client() Client { return Client{Host: dockerHost, Run: f.run} }

// imageLines renders images as docker image ls --format {{json .}} prints them.
func imageLines(t *testing.T, images ...Image) string {
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

func TestImagesReadsTheDaemonListing(t *testing.T) {
	t.Parallel()
	want := Image{Repository: "gastown/app", Tag: "v1", ID: imgID('1'), Size: "200MB"}
	f := &fakeDocker{images: imageLines(t, want, Image{Repository: "<none>", Tag: "<none>", ID: imgID('2'), Size: "1.5GB"})}

	got, err := f.client().Images(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != want {
		t.Errorf("Images = %+v, want the two listed images, first %+v", got, want)
	}
	if len(f.calls) != 1 {
		t.Fatalf("made %d calls, want 1", len(f.calls))
	}
	if !slices.Equal(f.calls[0].args, []string{"image", "ls", "--no-trunc", "--format", "{{json .}}"}) {
		t.Errorf("args = %v, want the no-trunc JSON listing", f.calls[0].args)
	}
	if !slices.Equal(f.calls[0].env, []string{"DOCKER_HOST=" + dockerHost}) {
		t.Errorf("env = %v, want DOCKER_HOST=%s", f.calls[0].env, dockerHost)
	}
}

func TestImagesRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		f    fakeDocker
		want error
	}{
		{name: "unreadable list", f: fakeDocker{imagesErr: errors.New("SECRET stderr")}, want: ErrListImages},
		{name: "line that is not JSON", f: fakeDocker{images: "not json\n"}, want: ErrParse},
		{name: "image id that does not parse", f: fakeDocker{images: imageLines(t, Image{ID: "short"})}, want: ErrParse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.f
			got, err := f.client().Images(context.Background())
			if !errors.Is(err, tc.want) {
				t.Fatalf("Images err = %v, want %v", err, tc.want)
			}
			if got != nil {
				t.Errorf("Images = %+v, want none beside the error", got)
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Errorf("refusal echoes command output: %q", err)
			}
		})
	}
}

func TestInUseIsTheImagesOfEveryContainer(t *testing.T) {
	t.Parallel()
	used, other := imgID('1'), imgID('2')
	f := &fakeDocker{containers: "abc123abc123\ndef456def456\n", inspect: used + "\n" + other + "\n"}

	got, err := f.client().InUse(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[used] || !got[other] {
		t.Errorf("InUse = %v, want both images", got)
	}
	if len(f.calls) != 2 {
		t.Fatalf("made %d calls, want ps and inspect", len(f.calls))
	}
	if !slices.Equal(f.calls[0].args, []string{"ps", "-a", "-q"}) {
		t.Errorf("args = %v, want the full container list", f.calls[0].args)
	}
	if !slices.Equal(f.calls[1].args, []string{"inspect", "--format", "{{.Image}}", "abc123abc123", "def456def456"}) {
		t.Errorf("args = %v, want one inspect per container", f.calls[1].args)
	}
	for _, c := range f.calls {
		if !slices.Equal(c.env, []string{"DOCKER_HOST=" + dockerHost}) {
			t.Errorf("call %v ran with env %v, want DOCKER_HOST=%s", c.args, c.env, dockerHost)
		}
	}
}

func TestInUseIsEmptyWithoutContainers(t *testing.T) {
	t.Parallel()
	f := &fakeDocker{containers: ""}

	got, err := f.client().InUse(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("InUse = %v, want empty", got)
	}
	if len(f.calls) != 1 {
		t.Errorf("made %d calls, want only the container list", len(f.calls))
	}
}

func TestInUseRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		f         fakeDocker
		want      error
		wantCalls int
	}{
		// wantCalls is how far the client got before refusing, so each row
		// isolates its own guard: the container-id row hands back a valid
		// inspect answer, so only that guard can refuse it.
		{name: "unreadable container list", f: fakeDocker{containersErr: errors.New("SECRET stderr")}, want: ErrListContainers, wantCalls: 1},
		{name: "container id that does not parse", f: fakeDocker{containers: "zz; rm -rf /\n", inspect: imgID('1') + "\n"}, want: ErrParse, wantCalls: 1},
		{name: "unreadable inspect", f: fakeDocker{containers: "abc123abc123\n", inspectErr: errors.New("SECRET stderr")}, want: ErrInspect, wantCalls: 2},
		{name: "image id that does not parse", f: fakeDocker{containers: "abc123abc123\n", inspect: "not-a-digest\n"}, want: ErrParse, wantCalls: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.f
			got, err := f.client().InUse(context.Background())
			if !errors.Is(err, tc.want) {
				t.Fatalf("InUse err = %v, want %v", err, tc.want)
			}
			if got != nil {
				t.Errorf("InUse = %v, want none beside the error", got)
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Errorf("refusal echoes command output: %q", err)
			}
			if len(f.calls) != tc.wantCalls {
				t.Errorf("made %d docker calls before refusing, want %d", len(f.calls), tc.wantCalls)
			}
		})
	}
}

func TestClientRefusesWithoutRunnerOrHost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		client Client
		want   error
	}{
		{name: "no runner", client: Client{Host: dockerHost}, want: ErrNoRunner},
		{name: "no host", client: Client{Run: (&fakeDocker{}).run}, want: ErrNoDockerHost},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.client.Docker(context.Background(), "ps"); !errors.Is(err, tc.want) {
				t.Errorf("Docker err = %v, want %v", err, tc.want)
			}
			if _, err := tc.client.Images(context.Background()); !errors.Is(err, tc.want) {
				t.Errorf("Images err = %v, want %v", err, tc.want)
			}
			if _, err := tc.client.InUse(context.Background()); !errors.Is(err, tc.want) {
				t.Errorf("InUse err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestBytesParsesTheSizeDockerPrints(t *testing.T) {
	t.Parallel()
	cases := map[string]int64{
		"1.5GB":   1_500_000_000,
		"200MB":   200_000_000,
		"3kB":     3_000,
		"10KiB":   10_240,
		"0B":      0,
		"":        0,
		"garbage": 0,
		"12XB":    0,
		"-1MB":    0,
	}
	for size, want := range cases {
		if got := (Image{Size: size}).Bytes(); got != want {
			t.Errorf("Bytes(%q) = %d, want %d", size, got, want)
		}
	}
}
