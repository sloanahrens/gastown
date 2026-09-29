package bdsnapshot

import (
	"strings"
	"testing"
)

func TestReduce(t *testing.T) {
	t.Parallel()
	in := `{"version":"1.2.2","commit":"abc","contract_version":1,"error_kinds":{"internal":1},
	"commands":[
	 {"path":"close","aliases":["done"],"flags":[
	   {"name":"reason","shorthand":"r","type":"string","default":"","persistent":false},
	   {"name":"json","type":"bool","default":"false","persistent":true}]},
	 {"path":"mol wisp","aliases":[],"hidden":true,"flags":[{"name":"json","type":"bool","default":"false","persistent":true}]}]}`
	snap, err := Reduce([]byte(in), "fork origin/main")
	if err != nil {
		t.Fatalf("Reduce: %v", err)
	}
	if snap.Source != "fork origin/main" || snap.Commit != "abc" || snap.ContractVersion != 1 {
		t.Fatalf("provenance = %+v", snap)
	}
	if got := strings.Join(snap.PersistentFlags, ","); got != "json" {
		t.Errorf("persistent flags = %q; want json", got)
	}
	if len(snap.Commands) != 2 || strings.Join(snap.Commands[0].Flags, ",") != "reason" || !snap.Commands[1].Hidden {
		t.Errorf("commands = %+v", snap.Commands)
	}
}
