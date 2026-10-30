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
  "created_at": "2026-01-01T00:00:00Z",
  "created_by": "bdshim-record",
  "id": "fx-new",
  "owner": "bdshim-record",
  "title": "fixture-created",
  "updated_at": "2026-01-01T00:00:00Z"
}
`,
		},
		{
			name: "a show output carries a revision and timestamps",
			in:   `{"id":"fx-1","revision":42,"created_at":"2026-10-30T18:59:04-05:00","closed_at":"2026-10-01T00:00:00Z","dependencies":[{"issue_id":"fx-1","depends_on_id":"fx-2","created_at":"2026-10-30T18:59:04Z"}]}`,
			want: `{
  "closed_at": "2026-01-01T00:00:00Z",
  "created_at": "2026-01-01T00:00:00Z",
  "dependencies": [
    {
      "created_at": "2026-01-01T00:00:00Z",
      "depends_on_id": "fx-2",
      "issue_id": "fx-1"
    }
  ],
  "id": "fx-1",
  "revision": 0
}
`,
		},
		{
			name: "a comment id the fixture holds is kept and its timestamp fixed",
			in:   `{"comments":[{"id":"fx-1-comment","issue_id":"fx-1","created_at":"2026-10-30T18:59:04Z"}]}`,
			want: `{
  "comments": [
    {
      "created_at": "2026-01-01T00:00:00Z",
      "id": "fx-1-comment",
      "issue_id": "fx-1"
    }
  ]
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
  "created_at": "yesterday",
  "id": "fx-1",
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
