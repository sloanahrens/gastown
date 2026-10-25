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
)

// The placement group: where each component of the town runs, read from
// <town>/settings/placement.json. The file is configuration that an operator,
// the overseer or a crew session edits when a component moves; the metrics
// tool only reads it. Place values are free text in the town's own names, so a
// new place needs no code; the component keys are the resource sampler's role
// names and the loadprof names. The decode is strict, so a file that does not
// match the schema is an errors entry for the group.
//
// A MISSING file is not an error: no placement is known, the record's
// placement stays null, and gt metrics snapshot says so in its receipt.

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
	for component, place := range doc.Places {
		if component == "" || place == "" {
			return nil, fmt.Errorf("component %q has an empty name or place", component)
		}
	}
	return doc.Places, nil
}
