package dockerimg

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

// The client's other refusals and the byte values of a size docker prints are
// covered by the colima target's tests (internal/diskmaint/colima/target_test.go),
// so this package does not pin them a second time. Two refusals stay here. The
// container-id refusal: the colima row refuses on its empty inspect answer
// whether or not the id is checked, so only this test shows the id refused
// before it reaches the inspect command line. The size refusal: Bytes reports
// an unreadable size as 0, so a caller of Bytes never takes it.

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

func TestImagesReadsTheCreationTime(t *testing.T) {
	t.Parallel()
	// The listing as docker prints it, so the key this reads is docker's own: the
	// cloud plan ranks tags by it.
	f := &fakeDocker{images: `{"Repository":"fractals-nextjs","Tag":"abc1234","ID":"` + imgID('1') +
		`","Size":"200MB","CreatedAt":"2026-11-14 22:16:03 +0000 UTC"}` + "\n"}

	got, err := f.client().Images(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := "2026-11-14 22:16:03 +0000 UTC"; len(got) != 1 || got[0].CreatedAt != want {
		t.Errorf("Images = %+v, want the listing's created time %q", got, want)
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

func TestInUseRefusesAContainerIDBeforeInspect(t *testing.T) {
	t.Parallel()
	// The inspect answer is valid, so only the container-id guard can refuse this.
	f := &fakeDocker{containers: "zz; rm -rf /\n", inspect: imgID('1') + "\n"}

	got, err := f.client().InUse(context.Background())
	if !errors.Is(err, ErrParse) {
		t.Fatalf("InUse err = %v, want %v", err, ErrParse)
	}
	if got != nil {
		t.Errorf("InUse = %v, want none beside the error", got)
	}
	if len(f.calls) != 1 {
		t.Errorf("made %d docker calls before refusing, want only the container list", len(f.calls))
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

func TestParseSizeRefusesASizeItCannotRead(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"no digits":              "",
		"no leading number":      "garbage",
		"two points":             "1.2.3MB",
		"unit docker cannot use": "12XB",
	}
	for name, size := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if n, err := ParseSize(size); !errors.Is(err, ErrSize) {
				t.Errorf("ParseSize(%q) = %d, %v, want 0, %v", size, n, err, ErrSize)
			}
		})
	}
}
