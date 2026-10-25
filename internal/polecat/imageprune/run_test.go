package imageprune

import (
	"errors"
	"reflect"
	"strings"
	"testing"
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

func TestPruneAllRefusesAnEmptyNodeList(t *testing.T) {
	t.Parallel()
	_, err := PruneAll(nil, func(string) Crictl { return healthy().crictl }, 0, false)
	if !errors.Is(err, ErrNoNodes) {
		t.Errorf("PruneAll with no nodes = %v, want ErrNoNodes (an empty list must not read as a clean run)", err)
	}
}
