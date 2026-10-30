package main

import "testing"

// TestNormalizeGoldenRewritesWhatTheRunDecides: a golden holds a fixed id, fixed timestamps and a
// fixed revision in place of the values bd decides at run time, keeps the ids the fixture file
// itself holds, and leaves output that is not one JSON value exactly as bd printed it (gt-p85pw.103).
func TestNormalizeGoldenRewritesWhatTheRunDecides(t *testing.T) {
	t.Parallel()
	keep := map[string]bool{
		fixtureID:      true,
		fixtureDepID:   true,
		fixtureParent:  true,
		"fx-1-comment": true,
		"fx-agent":     true,
	}
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "a create output names the bead it made",
			in:   `{"id":"fx-a1b2c3","title":"fixture-created","created_at":"2026-10-30T18:59:04Z","updated_at":"2026-10-30T18:59:04.123456Z","owner":"bdshim-record","created_by":"bdshim-record"}`,
			want: `{
  "id": "fx-new",
  "title": "fixture-created",
  "created_at": "2026-01-01T00:00:00Z",
  "updated_at": "2026-01-01T00:00:00Z",
  "owner": "bdshim-record",
  "created_by": "bdshim-record"
}
`,
		},
		{
			name: "a show output carries a revision and timestamps",
			in:   `{"id":"fx-1","revision":42,"created_at":"2026-10-30T18:59:04-05:00","closed_at":"2026-10-01T00:00:00Z","dependencies":[{"issue_id":"fx-1","depends_on_id":"fx-2","created_at":"2026-10-30T18:59:04Z"}]}`,
			want: `{
  "id": "fx-1",
  "revision": 0,
  "created_at": "2026-01-01T00:00:00Z",
  "closed_at": "2026-01-01T00:00:00Z",
  "dependencies": [
    {
      "issue_id": "fx-1",
      "depends_on_id": "fx-2",
      "created_at": "2026-01-01T00:00:00Z"
    }
  ]
}
`,
		},
		{
			name: "a comment id the fixture holds is kept and its timestamp fixed",
			in:   `{"comments":[{"id":"fx-1-comment","issue_id":"fx-1","created_at":"2026-10-30T18:59:04Z"}]}`,
			want: `{
  "comments": [
    {
      "id": "fx-1-comment",
      "issue_id": "fx-1",
      "created_at": "2026-01-01T00:00:00Z"
    }
  ]
}
`,
		},
		{
			name: "an id bd minted inside an array is rewritten wherever an id field holds it",
			in:   `{"dependencies":[{"issue_id":"fx-1","depends_on_id":"fx-a1b2c3"}],"blocked_by":["fx-a1b2c3"]}`,
			want: `{
  "dependencies": [
    {
      "issue_id": "fx-1",
      "depends_on_id": "fx-new"
    }
  ],
  "blocked_by": [
    "fx-new"
  ]
}
`,
		},
		{
			name: "fixture text that begins with the prefix is not a bead id",
			in:   `{"id":"fx-1","title":"fx-2 is blocked","description":"fx-a1b2c3 appears here"}`,
			want: `{"id":"fx-1","title":"fx-2 is blocked","description":"fx-a1b2c3 appears here"}`,
		},
		{
			name: "a title keeps the characters encoding/json would escape",
			in:   `{"id":"fx-a1b2c3","title":"a & b < c > d"}`,
			want: `{
  "id": "fx-new",
  "title": "a & b < c > d"
}
`,
		},
		{
			name: "a fixture id and the fields bd filled from the fixture are left alone",
			in:   `{"id":"fx-agent",  "labels":["gt:agent"],  "status":"open"}`,
			want: `{"id":"fx-agent",  "labels":["gt:agent"],  "status":"open"}`,
		},
		{
			name: "a timestamp that is not RFC 3339 is left as printed",
			in:   `{"id":"fx-1","created_at":"yesterday","revision":0}`,
			want: `{
  "id": "fx-1",
  "created_at": "yesterday",
  "revision": 0
}
`,
		},
		{
			name: "output that is not JSON is left alone",
			in:   "created bead fixture-created\n",
			want: "created bead fixture-created\n",
		},
		{
			name: "output of two JSON values is left alone",
			in:   "{\"id\":\"fx-1\"}\n{\"id\":\"fx-2\"}\n",
			want: "{\"id\":\"fx-1\"}\n{\"id\":\"fx-2\"}\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := string(normalizeGolden([]byte(tc.in), keep)); got != tc.want {
				t.Errorf("normalizeGolden(%s) =\n%s\nwant:\n%s", tc.name, got, tc.want)
			}
		})
	}
}

// TestNormalizeGoldenKeepsBdMemberOrder: a golden reads the way bd answered, so a shim compared
// against it is not taught a key order bd never prints (gt-p85pw.103).
func TestNormalizeGoldenKeepsBdMemberOrder(t *testing.T) {
	t.Parallel()
	in := `{"zeta":1,"alpha":{"second":"2026-10-30T18:59:04Z","first":"a","created_at":"2026-10-30T18:59:04Z"},"mid":[1,2]}`
	want := `{
  "zeta": 1,
  "alpha": {
    "second": "2026-10-30T18:59:04Z",
    "first": "a",
    "created_at": "2026-01-01T00:00:00Z"
  },
  "mid": [
    1,
    2
  ]
}
`
	if got := string(normalizeGolden([]byte(in), nil)); got != want {
		t.Errorf("normalizeGolden kept %s, want bd's member order:\n%s", got, want)
	}
}
