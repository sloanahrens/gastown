package imageprune

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	oldImage  = `{"images":[{"id":"sha256:old","repoTags":[],"repoDigests":["localhost:5001/gastown-polecat@sha256:f627"]}]}`
	devImage  = `{"images":[{"id":"sha256:dev","repoTags":["localhost:5001/gastown-polecat:dev"],"repoDigests":[]}]}`
	created   = `{"info":{"imageSpec":{"created":"2026-10-25T16:00:00Z"}}}`
	noneInUse = `{"containers":[]}`
)

// fakeNode answers crictl for one node. fail names the subcommand that errors;
// rmiFail is the image id whose removal errors. It records every rmi.
type fakeNode struct {
	images, inspect, ps string
	fail                string
	rmiFail             string
	rmis                []string
}

func (f *fakeNode) crictl(args ...string) ([]byte, error) {
	if args[0] == f.fail {
		return nil, errors.New("boom: " + args[0])
	}
	switch args[0] {
	case "images":
		return []byte(f.images), nil
	case "inspecti":
		return []byte(f.inspect), nil
	case "ps":
		return []byte(f.ps), nil
	case "rmi":
		f.rmis = append(f.rmis, args[1])
		if args[1] == f.rmiFail {
			return nil, errors.New("DeadlineExceeded")
		}
		return nil, nil
	}
	return nil, errors.New("unexpected " + strings.Join(args, " "))
}

func healthy() *fakeNode {
	return &fakeNode{images: oldImage, inspect: created, ps: noneInUse}
}

func TestPruneNodeRemovesThePlannedImage(t *testing.T) {
	t.Parallel()
	f := healthy()
	res := PruneNode("gt-sbx-worker", f.crictl, 0, false)
	if !res.Ok() || !reflect.DeepEqual(f.rmis, []string{"sha256:old"}) {
		t.Errorf("PruneNode = %+v rmis=%v, want the old image removed", res, f.rmis)
	}
}

func TestPruneNodeDryRunRemovesNothing(t *testing.T) {
	t.Parallel()
	f := healthy()
	res := PruneNode("gt-sbx-worker", f.crictl, 0, true)
	if len(res.Planned) != 1 || len(f.rmis) != 0 {
		t.Errorf("dry run planned %d and removed %v, want one planned and no removals", len(res.Planned), f.rmis)
	}
}

// A node whose images cannot be listed is left alone: nothing is removed there.
func TestPruneNodeFailsClosedWhenImagesCannotBeListed(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.fail = "images"
	res := PruneNode("gt-sbx-worker", f.crictl, 0, false)
	if res.Err == nil || len(f.rmis) != 0 || res.Ok() {
		t.Errorf("PruneNode with no image list = %+v rmis=%v, want an error and no removal", res, f.rmis)
	}
}

// A node whose containers cannot be listed is left alone: an image a container
// may use must not be removed when the containers are unknown.
func TestPruneNodeFailsClosedWhenContainersCannotBeListed(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.fail = "ps"
	res := PruneNode("gt-sbx-worker", f.crictl, 0, false)
	if res.Err == nil || len(f.rmis) != 0 {
		t.Errorf("PruneNode with no container list = %+v rmis=%v, want an error and no removal", res, f.rmis)
	}
}

// Output InUse cannot parse is a node whose containers are unknown, just as a
// failed list is: reading it as "no containers" would plan every image for
// removal, so the node is left alone.
func TestPruneNodeFailsClosedWhenTheContainerListIsUnparseable(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.ps = "crictl: ps is not json"
	res := PruneNode("gt-sbx-worker", f.crictl, 0, false)
	if res.Err == nil || len(f.rmis) != 0 || res.Ok() {
		t.Errorf("PruneNode with an unparseable container list = %+v rmis=%v, want an error and no removal", res, f.rmis)
	}
}

// An image whose creation time cannot be read is kept, not removed.
func TestPruneNodeKeepsAnImageWhoseAgeIsUnknown(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.fail = "inspecti"
	res := PruneNode("gt-sbx-worker", f.crictl, 0, false)
	if len(f.rmis) != 0 || res.Err != nil {
		t.Errorf("PruneNode with unreadable age = %+v rmis=%v, want the image kept", res, f.rmis)
	}
}

// A failed removal is recorded for that image and the node is not Ok.
func TestPruneNodeRecordsAFailedRemoval(t *testing.T) {
	t.Parallel()
	f := healthy()
	f.rmiFail = "sha256:old"
	res := PruneNode("gt-sbx-worker", f.crictl, 0, false)
	if res.Ok() || res.Err != nil || res.Failed["sha256:old"] == nil {
		t.Errorf("PruneNode with a failed removal = %+v, want it recorded in Failed and the node not Ok", res)
	}
}

func TestPruneAllContinuesPastANodeThatCannotBeRead(t *testing.T) {
	t.Parallel()
	bad, good := healthy(), healthy()
	bad.fail = "images"
	fakes := map[string]*fakeNode{"gt-sbx-control-plane": bad, "gt-sbx-worker": good}
	results, err := PruneAll([]string{"gt-sbx-worker", "gt-sbx-control-plane"}, func(n string) Crictl { return fakes[n].crictl }, 0, false)
	if err != nil {
		t.Fatalf("PruneAll: %v", err)
	}
	if len(results) != 2 || results[0].Node != "gt-sbx-control-plane" || results[0].Err == nil || !results[1].Ok() {
		t.Errorf("PruneAll results = %+v, want the failed node reported and the other pruned", results)
	}
	if AllOk(results) {
		t.Error("AllOk = true with a node that could not be read; the run must report failure")
	}
}

// Every crictl read of the node needs an explicit deadline: with crictl's
// default 2 second timeout a run failed one time in three at 208 images
// (1.2 to 2.3 seconds a call). Timed puts --timeout before the call's own
// arguments, where crictl accepts it.
func TestTimedPrependsTheTimeout(t *testing.T) {
	t.Parallel()
	var got []string
	want := []byte(`{"images":[]}`)
	boom := errors.New("DeadlineExceeded")
	timed := Timed(func(args ...string) ([]byte, error) {
		got = append([]string(nil), args...)
		return want, boom
	})
	out, err := timed("images", "-o", "json")
	if wantArgs := []string{"--timeout", CrictlTimeout, "images", "-o", "json"}; !reflect.DeepEqual(got, wantArgs) {
		t.Errorf("Timed passed %v, want %v", got, wantArgs)
	}
	if !reflect.DeepEqual(out, want) || !errors.Is(err, boom) {
		t.Errorf("Timed returned (%q, %v), want the fake's output and error unchanged", out, err)
	}
}

func TestPruneAllRefusesAnEmptyNodeList(t *testing.T) {
	t.Parallel()
	_, err := PruneAll(nil, func(string) Crictl { return healthy().crictl }, 0, false)
	if !errors.Is(err, ErrNoNodes) {
		t.Errorf("PruneAll with no nodes = %v, want ErrNoNodes (an empty list must not read as a clean run)", err)
	}
}

// timedFake answers PruneNodeRepos's crictl calls with serve. Each call must
// carry the deadline Timed prepends; a call without it fails the test.
func timedFake(t *testing.T, serve Crictl) Crictl {
	t.Helper()
	return func(args ...string) ([]byte, error) {
		if len(args) < 2 || args[0] != "--timeout" || args[1] != CrictlTimeout {
			t.Errorf("crictl %v carries no explicit timeout", args)
			return nil, errors.New("no timeout")
		}
		return serve(args[2:]...)
	}
}

// The 2026-11-20 control node: 72 gastown-polecat, 83 gastown-daemon, 42
// gastown-bd and 11 other images. A container uses three old ones; with keep 3
// the plan is every unused image beyond the newest three of each repository.
func TestPruneNodeReposSelectsTheUnusedBeyondNewest(t *testing.T) {
	t.Parallel()
	repos := []string{"gastown-polecat", "gastown-daemon", "gastown-bd"}
	counts := map[string]int{"gastown-polecat": 72, "gastown-daemon": 83, "gastown-bd": 42}
	busy := map[string]bool{"sha256:gastown-polecat-40": true, "sha256:gastown-daemon-50": true, "sha256:gastown-bd-20": true}
	base := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	var images, containers, want []string
	created := map[string]string{}
	for _, repo := range repos {
		for i := 0; i < counts[repo]; i++ {
			id := fmt.Sprintf("sha256:%s-%d", repo, i)
			images = append(images, fmt.Sprintf(`{"id":%q,"repoTags":["localhost:5001/%s:t%d"],"repoDigests":["localhost:5001/%s@%s"]}`, id, repo, i, repo, id))
			created[id] = fmt.Sprintf(`{"info":{"imageSpec":{"created":%q}}}`, base.Add(-time.Duration(i)*time.Hour).Format(time.RFC3339))
			switch {
			case busy[id]:
				containers = append(containers, fmt.Sprintf(`{"image":{"image":%q}}`, id))
			case i >= 3:
				want = append(want, id)
			}
		}
	}
	for i := 0; i < 11; i++ {
		id := fmt.Sprintf("sha256:other-%d", i)
		images = append(images, fmt.Sprintf(`{"id":%q,"repoTags":["localhost:5001/kube-proxy:v%d"],"repoDigests":[]}`, id, i))
		created[id] = `{"info":{"imageSpec":{"created":"2026-01-01T00:00:00Z"}}}`
	}
	var removed []string
	serve := func(args ...string) ([]byte, error) {
		switch args[0] {
		case "images":
			return []byte(`{"images":[` + strings.Join(images, ",") + `]}`), nil
		case "inspecti":
			return []byte(created[args[len(args)-1]]), nil
		case "ps":
			return []byte(`{"containers":[` + strings.Join(containers, ",") + `]}`), nil
		case "rmi":
			removed = append(removed, args[1])
			return nil, nil
		}
		return nil, errors.New("unexpected " + strings.Join(args, " "))
	}
	res := PruneNodeRepos("gt-control", timedFake(t, serve), 3, repos, false)
	var planned []string
	for _, im := range res.Planned {
		planned = append(planned, im.ID)
	}
	if !res.Ok() || !reflect.DeepEqual(planned, want) || !reflect.DeepEqual(removed, want) {
		t.Errorf("PruneNodeRepos planned %d and removed %d, want the %d unused images beyond the newest three of each repository", len(planned), len(removed), len(want))
	}
}

// PruneNodeRepos reads the node as PruneNode does: a failed read, or a dry run,
// removes nothing.
func TestPruneNodeReposFailsClosed(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		fail, images string
		dry, wantErr bool
	}{
		"image list fails":     {fail: "images", wantErr: true},
		"image list unparsed":  {images: "crictl: not json", wantErr: true},
		"container list fails": {fail: "ps", wantErr: true},
		"dry run":              {dry: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := healthy()
			f.fail = tc.fail
			if tc.images != "" {
				f.images = tc.images
			}
			res := PruneNodeRepos("gt-control", timedFake(t, f.crictl), 0, []string{Repo}, tc.dry)
			if (res.Err != nil) != tc.wantErr || len(f.rmis) != 0 || (len(res.Planned) == 1) == tc.wantErr {
				t.Errorf("PruneNodeRepos = %+v rmis=%v, want error %v, the image planned only on a dry run, and no removal", res, f.rmis, tc.wantErr)
			}
		})
	}
}
