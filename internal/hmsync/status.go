// Package hmsync decides from history-man's status output whether the hm sync
// patrol raises or clears its stale-sources alert (gt-z6qre.14).
package hmsync

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// AlertKey is the attention alert key the hm sync patrol raises when no
// configured source is fresh.
const AlertKey = "hmsync:stale"

// Source is one history-man source as `hm status --json` reports it; the
// reported fields hmsync does not decide on are ignored.
type Source struct {
	Source     string `json:"source"`
	Rig        string `json:"rig"`
	Configured bool   `json:"configured"`
	Stale      bool   `json:"stale"`
	Failing    bool   `json:"failing"`
}

// Verdict is the alert change Evaluate asks the patrol to apply; a Verdict
// with neither Raise nor Clear asks for none.
type Verdict struct {
	Raise  bool
	Clear  bool
	Detail string
}

// ParseStatus decodes `hm status --json`, which must be an object holding a
// "sources" array, and fails on anything else so an unreadable status is never
// mistaken for a healthy one.
func ParseStatus(data []byte) ([]Source, error) {
	var doc struct {
		Sources json.RawMessage `json:"sources"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("hm status json: %w", err)
	}
	if len(doc.Sources) == 0 || bytes.Equal(bytes.TrimSpace(doc.Sources), []byte("null")) {
		return nil, errors.New("hm status json has no sources key")
	}
	var sources []Source
	if err := json.Unmarshal(doc.Sources, &sources); err != nil {
		return nil, fmt.Errorf("hm status json sources: %w", err)
	}
	return sources, nil
}

// Evaluate returns the change to apply to the alert: Raise when it is inactive
// and every configured source is stale or failing, Clear when it is active and
// any configured source is fresh, and neither otherwise. A status naming no
// configured source raises on the first read, since nothing is receiving
// updates.
func Evaluate(sources []Source, active bool) Verdict {
	var configured, stale, failing, fresh, down int
	for _, s := range sources {
		if !s.Configured {
			continue
		}
		configured++
		if s.Stale {
			stale++
		}
		if s.Failing {
			failing++
		}
		if s.Stale || s.Failing {
			down++
			continue
		}
		fresh++
	}
	if configured == 0 {
		return Verdict{
			Raise:  !active,
			Detail: "no configured hm source: nothing is syncing",
		}
	}
	v := Verdict{
		Detail: fmt.Sprintf("%d stale, %d failing, %d fresh of %d configured hm sources",
			stale, failing, fresh, configured),
	}
	switch {
	case !active && down == configured:
		v.Raise = true
	case active && fresh > 0:
		v.Clear = true
	}
	return v
}
