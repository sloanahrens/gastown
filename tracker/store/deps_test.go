package store

import (
	"errors"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
)

func TestParseExternalRef(t *testing.T) {
	t.Parallel() // unit tier: pure
	for ref, want := range map[string]bool{
		"external:om:om-7": true, "external:hq:hq-wisp-abc": true,
		"external:om": false, "external::x": false, "external:om:": false, "external:a:b:c": true, "om-7": false,
	} {
		if _, _, ok := parseExternalRef(ref); ok != want {
			t.Errorf("parseExternalRef(%q) ok = %v, want %v", ref, ok, want)
		}
	}
	rig, id, _ := parseExternalRef("external:om:om-7")
	if rig != "om" || id != "om-7" {
		t.Errorf("parsed %q %q, want om om-7", rig, id)
	}
}

func TestValidateDepType(t *testing.T) {
	t.Parallel()
	for typ, ok := range map[string]bool{
		"blocks": true, "parent-child": true, "tracks": true, "relates-to": true, "discovered-from": true, "conditional-blocks": true,
		"": false, "Blocks": false, "has space": false, "x;drop": false,
	} {
		err := validateDepType(typ)
		if (err == nil) != ok {
			t.Errorf("validateDepType(%q) = %v, want ok=%v", typ, err, ok)
		}
		if err != nil && !errors.Is(err, api.ErrInvalid) {
			t.Errorf("validateDepType(%q) error = %v, want api.ErrInvalid", typ, err)
		}
	}
}

func TestIsCycleChecked(t *testing.T) {
	t.Parallel()
	for typ, want := range map[string]bool{"blocks": true, "conditional-blocks": true, "parent-child": true, "tracks": false, "relates-to": false, "discovered-from": false} {
		if got := isCycleChecked(typ); got != want {
			t.Errorf("isCycleChecked(%q) = %v, want %v", typ, got, want)
		}
	}
}
