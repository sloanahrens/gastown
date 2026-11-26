package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/update"
)

// clusterCaptureDeps is one capture's seams: the kubectl runner the object is
// read through, the clock the capture is stamped with, the cluster the capture
// names, and the directory the file is written under.
type clusterCaptureDeps struct {
	kubectl update.RunFunc
	now     func() time.Time
	cluster string
	workDir string
}

// captureFromCluster returns the capture file's bytes for kind/name: the live
// object through captureObject, as indented JSON ending in a newline. It writes
// no file and changes no cluster; an object the cluster does not hold is an
// error, not an empty capture.
func captureFromCluster(ctx context.Context, d clusterCaptureDeps, kind, name, namespace string) ([]byte, error) {
	if d.kubectl == nil {
		return nil, errors.New("no kubectl runner is wired, so the object cannot be read")
	}
	if d.now == nil {
		return nil, errors.New("no clock is wired, so the capture cannot be stamped")
	}
	resource := kind + "/" + name
	live, absent, err := readLiveObject(ctx, clusterDriftDeps{kubectl: d.kubectl}, resource, namespace, namespace == "")
	if err != nil {
		return nil, err
	}
	if absent {
		return nil, fmt.Errorf("the cluster does not hold the %s, so it was not captured", resource)
	}
	captured, err := captureObject(live, d.cluster, d.now())
	if err != nil {
		return nil, fmt.Errorf("capturing the %s: %w", resource, err)
	}
	return captureEncode(captured)
}

// captureEncode returns captured as indented JSON ending in a newline. HTML
// escaping is off so a captured config value's < and & read as written: the
// file exists so a person can read the difference the drift report names
// (gt-5jwtdg).
func captureEncode(captured map[string]any) ([]byte, error) {
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	// Encode reports a value JSON cannot hold. captureObject's output holds
	// none, but the error is returned rather than dropped so a value that could
	// not be encoded is reported instead of lost.
	if err := encoder.Encode(captured); err != nil {
		return nil, fmt.Errorf("encoding the capture: %w", err)
	}
	return body.Bytes(), nil
}
