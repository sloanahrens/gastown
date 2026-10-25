package beads

import (
	"errors"
	"strings"
	"testing"
)

// The kv read pair carries the rig's memories to prime, so both verbs keep
// their argv and read bd's machine envelope (gt-p85pw.9).
func TestKVMethodsSendTheirArgv(t *testing.T) {
	t.Parallel()
	rec := newRecorder(func(args []string) reply {
		switch strings.Join(args, " ") {
		case "kv list --json":
			return reply{stdout: `{"schema_version":1,"contract_version":1,"data":{"gt.project.note":"keep me"},"pagination":null,"error":null}`}
		case "kv get gt.project.note --json":
			return reply{stdout: `{"schema_version":1,"contract_version":1,"data":{"found":true,"key":"gt.project.note","value":"keep me"},"pagination":null,"error":null}`}
		}
		return reply{}
	})
	b := newRecordedBeads(t.TempDir(), rec)

	if got, err := b.KVList(); err != nil || got["gt.project.note"] != "keep me" {
		t.Errorf("KVList = %v, %v; want the note", got, err)
	}
	if got, err := b.KVGet("gt.project.note"); err != nil || got != "keep me" {
		t.Errorf("KVGet = %q, %v; want keep me", got, err)
	}
	if want := []string{"kv list --json", "kv get gt.project.note --json"}; strings.Join(rec.argvs(), " | ") != strings.Join(want, " | ") {
		t.Errorf("bd argv = %q, want %q", rec.argvs(), want)
	}
}

// A key the store does not hold is ErrNotFound, never an empty value a caller
// cannot tell from a stored "".
func TestKVGetOfAnUnsetKeyIsNotFound(t *testing.T) {
	t.Parallel()
	rec := newRecorder(func([]string) reply {
		return reply{stdout: `{"found":false,"key":"custom.probe","schema_version":1,"value":""}`}
	})
	got, err := newRecordedBeads(t.TempDir(), rec).KVGet("custom.probe")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("KVGet(unset) = %q, %v; want ErrNotFound", got, err)
	}
}

// The list keeps every value readable: strings as themselves, other JSON as
// its compact text, and null as no entry at all (gt-p85pw.9).
func TestParseKVListJSON(t *testing.T) {
	t.Parallel()
	got, err := ParseKVListJSON([]byte(`{
		"gt.project.note":"keep me",
		"gt.project.empty":"",
		"gt.project.count":12,
		"gt.project.enabled":true,
		"gt.project.tags":["one"],
		"gt.project.config":{"nested":"value"},
		"gt.project.null":null
	}`))
	if err != nil {
		t.Fatalf("ParseKVListJSON() error = %v", err)
	}

	want := map[string]string{
		"gt.project.note":    "keep me",
		"gt.project.empty":   "",
		"gt.project.count":   "12",
		"gt.project.enabled": "true",
		"gt.project.tags":    `["one"]`,
		"gt.project.config":  `{"nested":"value"}`,
	}
	if len(got) != len(want) {
		t.Fatalf("ParseKVListJSON() returned %d entries, want %d: %#v", len(got), len(want), got)
	}
	for k, wantValue := range want {
		if got[k] != wantValue {
			t.Errorf("ParseKVListJSON()[%q] = %q, want %q", k, got[k], wantValue)
		}
	}
	if _, ok := got["gt.project.null"]; ok {
		t.Error("ParseKVListJSON() kept a null value")
	}
}

// Both parsers take the machine envelope or the bare --json payload a bd from
// before machine mode prints (gt-p85pw.9).
func TestKVParsersAcceptEitherForm(t *testing.T) {
	t.Parallel()
	envelope := `{"schema_version":1,"contract_version":1,"data":{"found":true,"key":"k","value":"v"},"pagination":null,"error":null}`
	for _, out := range []string{envelope, `{"found":true,"key":"k","value":"v"}`} {
		got, err := ParseKVGetJSON([]byte(out))
		if err != nil || got != "v" {
			t.Errorf("ParseKVGetJSON(%s) = %q, %v; want v", out, got, err)
		}
	}

	listEnvelope := `{"schema_version":1,"contract_version":1,"data":{"k":"v"},"pagination":null,"error":null}`
	for _, out := range []string{listEnvelope, `{"k":"v"}`} {
		got, err := ParseKVListJSON([]byte(out))
		if err != nil || got["k"] != "v" {
			t.Errorf("ParseKVListJSON(%s) = %v, %v; want k=v", out, got, err)
		}
	}
}

func TestParseKVListJSONMalformed(t *testing.T) {
	t.Parallel()
	if _, err := ParseKVListJSON([]byte(`{"gt.project.note":`)); err == nil {
		t.Fatal("ParseKVListJSON() error = nil, want malformed JSON error")
	}
}

// An envelope with no data is an empty store, not the envelope read as a map
// of entries (gt-p85pw.9).
func TestParseKVListJSONEnvelopeWithNoDataIsEmpty(t *testing.T) {
	t.Parallel()
	got, err := ParseKVListJSON([]byte(`{"schema_version":1,"contract_version":1,"data":null,"pagination":null,"error":null}`))
	if err != nil {
		t.Fatalf("ParseKVListJSON: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("entries = %v, want none", got)
	}
}

// A non-string value under a memory key is kept as its JSON text; the same
// value under bd's own metadata key is dropped (gt-p85pw.9).
func TestParseKVListJSONKeepsNonStringValuesOnlyUnderMemoryKeys(t *testing.T) {
	t.Parallel()
	got, err := ParseKVListJSON([]byte(`{"gt.memory.a":{"n":1},"bd.meta":{"n":2},"gt.note":"text","nul":null}`))
	if err != nil {
		t.Fatalf("ParseKVListJSON: %v", err)
	}
	if got["gt.memory.a"] != `{"n":1}` || got["gt.note"] != "text" {
		t.Errorf("memory and string entries = %v", got)
	}
	if _, ok := got["bd.meta"]; ok {
		t.Errorf("metadata key bd.meta kept: %v", got)
	}
	if _, ok := got["nul"]; ok {
		t.Errorf("null entry kept: %v", got)
	}
}

// A key bd would read as a flag never reaches argv (gt-p85pw.9).
func TestKVGetRefusesAFlagLikeKey(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"", "--force", "-x"} {
		if _, err := (&Beads{}).KVGet(key); err == nil {
			t.Errorf("KVGet(%q) returned no error", key)
		}
	}
}
