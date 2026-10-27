// Package landtest holds the verdict-store fixtures the land and cmd tests share,
// so both build the store in the layout the landing worker writes (gt-lh0sc).
package landtest

import (
	"encoding/json"
	"path"
	"testing"
	"testing/fstest"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// The heads are full 40-digit shas. HeadB shares seven digits with HeadA and
// differs after them, so a seven-digit prefix names both and a twelve-digit one
// names HeadA. HeadC shares nothing with the others.
const (
	HeadA = "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"
	HeadB = "ab12ab1f" + "00000000000000000000000000000000"
	HeadC = "cd34cd34cd34" + "0000000000000000000000000000"
)

// T0 is the judging time the fixtures start from.
var T0 = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

// OldOM is an om verdict that asked for changes and has no findings.
const OldOM = `{"verdict":"request_changes","score":0.42,"summary":"old round","findings":[]}`

// NewOM is an om verdict that approved, with one finding and its detail.
const NewOM = `{"verdict":"approve","score":0.91,"summary":"new round","findings":[` +
	`{"id":"F1","severity":"minor","path":"a.go","line":3,"title":"nit","detail":"explain the nit"}]}`

// Record is one stored review of bead at head, judged at at, with om's verdict
// JSON as given. The head is a full sha: the store names its file by the first
// twelve digits.
func Record(bead, head string, at time.Time, omJSON string) land.VerdictRecord {
	return land.VerdictRecord{Bead: bead, Rig: "gastown", Head: head, At: at,
		OMVersion: "v9.9.9", Flags: []string{"-C", "--base"}, Verdict: json.RawMessage(omJSON)}
}

// Store is an in-memory verdict store holding recs under the names the store
// writes: <rig>/<bead>/<head12>.json.
func Store(t testing.TB, recs ...land.VerdictRecord) fstest.MapFS {
	t.Helper()
	m := fstest.MapFS{}
	for _, r := range recs {
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		m[path.Join(r.Rig, r.Bead, r.Head[:12]+".json")] = &fstest.MapFile{Data: data}
	}
	return m
}
