package bdshim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryHandlerIsAllowlisted: every verb the registry answers must name a
// path in deploy/bd/verbs.json, or the gateway's policy refuses the call before
// it reaches the handler. The test sits beside the registry so adding a handler
// is what fails the gate; internal/bdgateway tests the file against bd's
// command tree.
func TestEveryHandlerIsAllowlisted(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "bd", "verbs.json"))
	if err != nil {
		t.Fatalf("read deploy/bd/verbs.json: %v", err)
	}
	var doc struct {
		Verbs []struct {
			Path string `json:"path"`
		} `json:"verbs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse deploy/bd/verbs.json: %v", err)
	}

	first := make(map[string]bool, len(doc.Verbs))
	for _, v := range doc.Verbs {
		words := strings.Fields(v.Path)
		if len(words) == 0 {
			t.Errorf("deploy/bd/verbs.json holds a verb with no path")
			continue
		}
		first[words[0]] = true
	}
	for verb := range handlers {
		if !first[verb] {
			t.Errorf("bdshim answers %q, but deploy/bd/verbs.json has no path that starts with it", verb)
		}
	}
}
