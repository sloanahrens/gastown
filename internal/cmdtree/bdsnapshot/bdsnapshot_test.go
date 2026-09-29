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

func TestSubcommandOnly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		help string
		want bool
		err  bool
	}{
		{"not runnable", "Manage molecules\n\nUsage:\n  bd mol [command]\n\nAvailable Commands:\n", true, false},
		{"runnable parent", "Usage:\n  bd mol wisp [proto-id] [flags]\n  bd mol wisp [command]\n\n", false, false},
		{"no usage section", "something else\n", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SubcommandOnly(tc.help)
			if (err != nil) != tc.err || got != tc.want {
				t.Fatalf("SubcommandOnly = %v, %v; want %v, err=%v", got, err, tc.want, tc.err)
			}
		})
	}
}

func TestMarkSubcommandOnly(t *testing.T) {
	t.Parallel()
	snap := Snapshot{Commands: []Command{{Path: "mol"}, {Path: "mol wisp"}, {Path: "mol wisp list"}, {Path: "show"}}}
	var asked []string
	help := func(path string) (string, error) {
		asked = append(asked, path)
		if path == "mol" {
			return "Usage:\n  bd mol [command]\n", nil
		}
		return "Usage:\n  bd mol wisp [id] [flags]\n  bd mol wisp [command]\n", nil
	}
	if err := MarkSubcommandOnly(&snap, help); err != nil {
		t.Fatal(err)
	}
	if strings.Join(asked, ",") != "mol,mol wisp" {
		t.Errorf("help asked for %v; want only the parents", asked)
	}
	if !snap.Commands[0].SubcommandOnly || snap.Commands[1].SubcommandOnly {
		t.Errorf("commands = %+v", snap.Commands)
	}
}
