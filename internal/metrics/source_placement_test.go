package metrics

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// placementEnv serves one placement file (or the error) at the town's path.
func placementEnv(content string, readErr error) Env {
	env := envAt(fixedNow())
	env.Town = "/town"
	env.ReadFile = func(path string) ([]byte, error) {
		if path != filepath.Join("/town", "settings", "placement.json") {
			return nil, errors.New("read of an unexpected path: " + path)
		}
		if readErr != nil {
			return nil, readErr
		}
		return []byte(content), nil
	}
	return env
}

func TestSourcePlacementReadsTheExampleFile(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("testdata", "placement.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	cols, err := placementSource{}.Collect(context.Background(), placementEnv(string(data), nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if cols["dolt"] != "mac-host" || cols["forgejo"] != "docker-desktop" || len(cols) != 9 {
		t.Fatalf("placement = %v, want the nine components of the example", cols)
	}
}

func TestSourcePlacementMissingFileIsNoPlacementAndNoError(t *testing.T) {
	t.Parallel()
	cols, err := placementSource{}.Collect(context.Background(), placementEnv("", fs.ErrNotExist))
	if err != nil || cols != nil {
		t.Fatalf("Collect = %v, %v; want nil, nil (no placement known)", cols, err)
	}
	env := placementEnv("", fs.ErrNotExist)
	rec := Collect(context.Background(), env, []Source{placementSource{}}, SourceTimeout)
	if rec.Placement != nil || len(rec.Errors) != 0 {
		t.Fatalf("record placement = %v, errors = %v; want null and none", rec.Placement, rec.Errors)
	}
}

func TestSourcePlacementRefusesWhatIsNotTheSchema(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		readErr error
		want    string
	}{
		{name: "an unknown top-level field", content: `{"v":1,"places":{"dolt":"mac-host"},"extra":1}`, want: "unknown field"},
		{name: "another schema version", content: `{"v":2,"places":{"dolt":"mac-host"}}`, want: "schema version 2"},
		{name: "no version", content: `{"places":{"dolt":"mac-host"}}`, want: "schema version 0"},
		{name: "malformed JSON", content: `{"v":1,"places":`, want: "decoding"},
		{name: "data after the object", content: `{"v":1,"places":{}} {"v":1}`, want: "data after"},
		{name: "no places object", content: `{"v":1}`, want: "places"},
		{name: "a place that is not a string", content: `{"v":1,"places":{"dolt":7}}`, want: "decoding"},
		{name: "an empty place", content: `{"v":1,"places":{"dolt":""}}`, want: "empty"},
		{name: "an empty component name", content: `{"v":1,"places":{"":"mac-host"}}`, want: "empty"},
		{name: "a read failure other than a missing file", readErr: fs.ErrPermission, want: "permission"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cols, err := placementSource{}.Collect(context.Background(), placementEnv(tc.content, tc.readErr))
			if err == nil || cols != nil {
				t.Fatalf("Collect = %v, %v; want an error and no placement", cols, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
			rec := Collect(context.Background(), placementEnv(tc.content, tc.readErr), []Source{placementSource{}}, SourceTimeout)
			if rec.Placement != nil || len(rec.Errors) != 1 || rec.Errors[0].Group != PlacementGroup {
				t.Errorf("record placement = %v, errors = %v; want null and one placement error", rec.Placement, rec.Errors)
			}
		})
	}
}

func TestSourcePlacementAnEmptyPlacesObjectIsKnown(t *testing.T) {
	t.Parallel()
	cols, err := placementSource{}.Collect(context.Background(), placementEnv(`{"v":1,"places":{}}`, nil))
	if err != nil || cols == nil || len(cols) != 0 {
		t.Fatalf("Collect = %v, %v; want an empty non-nil map (a file that places nothing)", cols, err)
	}
}
