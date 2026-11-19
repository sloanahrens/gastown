package deployerinbox_test

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/deployerinbox"
)

// goodID is a request id of the shape the schema requires.
const goodID = "20261119T181450Z-0123abcd"

// goodFile is the file name a request carrying goodID must have.
const goodFile = goodID + ".json"

// paddedRequest is a valid plan request padded with spaces to size bytes, so
// the only check a file of that size can fail is the size limit.
func paddedRequest(size int) string {
	body := `{"id":"` + goodID + `","kind":"plan","plan":"p"}`
	return body + strings.Repeat(" ", size-len(body))
}

func TestDeployerInboxParse(t *testing.T) {
	t.Parallel()

	accept := []struct {
		name string
		file string
		data string
		want deployerinbox.Request
	}{
		{
			name: "plan kind",
			file: goodFile,
			data: `{"id":"` + goodID + `","kind":"plan","plan":"2026-11-19-first"}`,
			want: deployerinbox.Request{ID: goodID, Kind: "plan", Plan: "2026-11-19-first"},
		},
		{
			name: "apply kind",
			file: goodFile,
			data: `{"id":"` + goodID + `","kind":"apply","plan":"control-a"}`,
			want: deployerinbox.Request{ID: goodID, Kind: "apply", Plan: "control-a"},
		},
		{
			name: "rollback kind",
			file: goodFile,
			data: `{"id":"` + goodID + `","kind":"rollback","plan":"x9-0"}`,
			want: deployerinbox.Request{ID: goodID, Kind: "rollback", Plan: "x9-0"},
		},
		{
			name: "at the size limit",
			file: goodFile,
			data: paddedRequest(deployerinbox.MaxRequestBytes),
			want: deployerinbox.Request{ID: goodID, Kind: "plan", Plan: "p"},
		},
	}
	for _, tc := range accept {
		tc := tc
		t.Run("accepts "+tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := deployerinbox.ParseRequest(tc.file, []byte(tc.data))
			if err != nil {
				t.Fatalf("ParseRequest(%s) = %v, want no error", tc.file, err)
			}
			if got != tc.want {
				t.Fatalf("ParseRequest(%s) = %+v, want %+v", tc.file, got, tc.want)
			}
		})
	}

	refuse := []struct {
		name string
		file string
		data string
		// want is a substring the error must carry, for the refusals a later
		// schema check would otherwise produce on its own.
		want string
	}{
		{
			name: "an unknown key",
			file: goodFile,
			data: `{"id":"` + goodID + `","kind":"plan","plan":"p","cluster":"control"}`,
		},
		{
			name: "a parent-directory plan",
			file: goodFile,
			data: `{"id":"` + goodID + `","kind":"plan","plan":"../control"}`,
		},
		{
			name: "an absolute plan",
			file: goodFile,
			data: `{"id":"` + goodID + `","kind":"plan","plan":"/control"}`,
		},
		{
			name: "an upper-case plan",
			file: goodFile,
			data: `{"id":"` + goodID + `","kind":"plan","plan":"Control"}`,
		},
		{
			name: "a plan over 64 characters",
			file: goodFile,
			data: `{"id":"` + goodID + `","kind":"plan","plan":"` + strings.Repeat("a", 65) + `"}`,
		},
		{
			name: "a file over 4096 bytes",
			file: goodFile,
			data: paddedRequest(deployerinbox.MaxRequestBytes + 1),
		},
		{
			name: "an id that is a number",
			file: goodFile,
			data: `{"id":7,"kind":"plan","plan":"p"}`,
			want: "not a JSON string",
		},
		{
			name: "a kind that is null",
			file: goodFile,
			data: `{"id":"` + goodID + `","kind":null,"plan":"p"}`,
			want: "not a JSON string",
		},
		{
			name: "a plan that is an object",
			file: goodFile,
			data: `{"id":"` + goodID + `","kind":"plan","plan":{"name":"p"}}`,
			want: "not a JSON string",
		},
		{
			name: "an unknown kind",
			file: goodFile,
			data: `{"id":"` + goodID + `","kind":"destroy","plan":"p"}`,
		},
		{
			name: "an id that names another file",
			file: "20261119T181451Z-0123abcd.json",
			data: `{"id":"` + goodID + `","kind":"plan","plan":"p"}`,
		},
		{
			name: "a file name without .json",
			file: goodID,
			data: `{"id":"` + goodID + `","kind":"plan","plan":"p"}`,
		},
		{
			name: "a file name with a directory",
			file: "inbox/" + goodFile,
			data: `{"id":"` + goodID + `","kind":"plan","plan":"p"}`,
		},
		{
			name: "an id that is not a UTC stamp",
			file: "abc.json",
			data: `{"id":"abc","kind":"plan","plan":"p"}`,
		},
		{
			name: "no id",
			file: goodFile,
			data: `{"kind":"plan","plan":"p"}`,
		},
		{
			name: "no kind",
			file: goodFile,
			data: `{"id":"` + goodID + `","plan":"p"}`,
		},
		{
			name: "no plan",
			file: goodFile,
			data: `{"id":"` + goodID + `","kind":"plan"}`,
		},
		{
			name: "malformed JSON",
			file: goodFile,
			data: `{"id":`,
		},
		{
			name: "a top-level array",
			file: goodFile,
			data: `[]`,
		},
		{
			name: "trailing data",
			file: goodFile,
			data: `{"id":"` + goodID + `","kind":"plan","plan":"p"}{}`,
		},
		{
			name: "an empty file",
			file: goodFile,
			data: ``,
		},
	}
	for _, tc := range refuse {
		tc := tc
		t.Run("refuses "+tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := deployerinbox.ParseRequest(tc.file, []byte(tc.data))
			if err == nil {
				t.Fatalf("ParseRequest(%s, %d bytes) = %+v, want an error", tc.file, len(tc.data), got)
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseRequest(%s) = %v, want an error naming %q", tc.file, err, tc.want)
			}
		})
	}
}
