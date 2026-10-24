package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"sort"
)

// The placement group: where each component of the town runs, read from
// <town>/settings/placement.json. The file is configuration that an operator,
// the overseer or a crew session edits when a component moves; the metrics
// tool only reads it. Schema version 1:
//
//	{"v": 1, "updated": "2026-10-24",
//	 "places": {"dolt": "mac-host", "forgejo": "docker-desktop", "sandbox": "town-cluster"}}
//
// The place values are free text in the town's own names (mac-host,
// docker-desktop, town-cluster, tenant-cluster), so a new place needs no code;
// the component keys are the resource sampler's role names and the loadprof
// names. The decode is strict: an unknown field, a version other than 1, a
// place that is empty or not a string, and anything after the object are
// errors, and an error leaves the record's placement null with an errors entry
// named "placement".
//
// A MISSING file is not an error: no placement is known, the record's
// placement stays null, and gt metrics snapshot says so in its receipt.
//
// Nothing else reads settings/*.json by directory scan, so the file does not
// disturb any loader; this source opens exactly this one path.

// PlacementFile is the file's path under the town root.
var PlacementFile = filepath.Join("settings", "placement.json")

// placementVersion is the only schema version the source reads.
const placementVersion = 1

// placementFileDoc is the file's shape.
type placementFileDoc struct {
	V       int               `json:"v"`
	Updated string            `json:"updated"`
	Places  map[string]string `json:"places"`
}

type placementSource struct{}

func (placementSource) Group() string { return PlacementGroup }

func (placementSource) Collect(_ context.Context, env Env) (map[string]any, error) {
	data, err := env.ReadFile(filepath.Join(env.Town, PlacementFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", PlacementFile, err)
	}
	places, err := parsePlacement(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", PlacementFile, err)
	}
	out := make(map[string]any, len(places))
	for component, place := range places {
		out[component] = place
	}
	return out, nil
}

// parsePlacement decodes a placement file strictly and returns its places.
func parsePlacement(data []byte) (map[string]string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var doc placementFileDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decoding: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("decoding: data after the top-level object")
	}
	if doc.V != placementVersion {
		return nil, fmt.Errorf("schema version %d, want %d", doc.V, placementVersion)
	}
	if doc.Places == nil {
		return nil, errors.New(`no "places" object`)
	}
	components := make([]string, 0, len(doc.Places))
	for component := range doc.Places {
		components = append(components, component)
	}
	sort.Strings(components)
	for _, component := range components {
		if component == "" || doc.Places[component] == "" {
			return nil, fmt.Errorf("component %q has an empty name or place", component)
		}
	}
	return doc.Places, nil
}
