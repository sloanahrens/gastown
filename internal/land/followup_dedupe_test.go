package land

import (
	"strings"
	"testing"
)

// The paraphrase pair the similarity threshold is set against: gt-5mc21 and
// gt-me4vs, two beads filed six days apart for the same cd-around-the-guards
// bug. The titles are the ones the beads carry, "Found:" prefix included.
const (
	paraphraseA = "Found: the test guards judge every segment against the hook cwd, so a cd steps around them"
	paraphraseB = "Found: the idle gate judges every segment against the hook cwd, so a cd steps around it too"
)

// TestFingerprintStableAndNormalized: the fingerprint ignores case, spacing and
// punctuation on the title, keeps the path as given but for cleaning, and
// changes when the path or the finding changes. A fingerprint that moved with
// formatting would re-file every finding on the next review (gt-dhkhi).
func TestFingerprintStableAndNormalized(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		path       string
		title      string
		otherPath  string
		otherTitle string
		// same is whether the two spellings are the same finding.
		same bool
	}{
		{
			name:       "case, spacing and punctuation do not change it",
			path:       "internal/land/note.go",
			title:      "Found: The nil-map panic!",
			otherPath:  "internal/land/note.go",
			otherTitle: "  found  the   NIL map panic  ",
			same:       true,
		},
		{
			name:       "a leading ./ is the same file",
			path:       "./internal/land/note.go",
			title:      "nil map panic",
			otherPath:  "internal/land/note.go",
			otherTitle: "nil map panic",
			same:       true,
		},
		{
			name:       "a different path is a different finding",
			path:       "internal/land/note.go",
			title:      "nil map panic",
			otherPath:  "internal/land/gate.go",
			otherTitle: "nil map panic",
		},
		{
			name:       "a different finding at the same path differs",
			path:       "internal/land/note.go",
			title:      "nil map panic",
			otherPath:  "internal/land/note.go",
			otherTitle: "the retry loop never backs off",
		},
		{
			name:       "the path is not lowercased",
			path:       "internal/land/Note.go",
			title:      "nil map panic",
			otherPath:  "internal/land/note.go",
			otherTitle: "nil map panic",
		},
		{
			name:       "an empty path still fingerprints",
			path:       "",
			title:      "nil map panic",
			otherPath:  "internal/land/note.go",
			otherTitle: "nil map panic",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Fingerprint(tt.path, tt.title)
			if got != Fingerprint(tt.path, tt.title) {
				t.Errorf("Fingerprint(%q, %q) is not stable across runs", tt.path, tt.title)
			}
			other := Fingerprint(tt.otherPath, tt.otherTitle)
			if tt.same && got != other {
				t.Errorf("Fingerprint(%q, %q) = %q, want %q", tt.path, tt.title, got, other)
			}
			if !tt.same && got == other {
				t.Errorf("Fingerprint(%q, %q) = Fingerprint(%q, %q) = %q, want different", tt.path, tt.title, tt.otherPath, tt.otherTitle, got)
			}
		})
	}
}

// TestFingerprintShape: the fingerprint is the label prefix plus 12 hex
// characters, so it is safe to store as a bead label and readable in a listing.
func TestFingerprintShape(t *testing.T) {
	t.Parallel()
	got := Fingerprint("internal/land/note.go", "nil map panic")
	rest, ok := strings.CutPrefix(got, FingerprintPrefix)
	if !ok {
		t.Fatalf("Fingerprint = %q, want the prefix %q", got, FingerprintPrefix)
	}
	if len(rest) != 12 {
		t.Errorf("Fingerprint = %q, want 12 hex characters after the prefix", got)
	}
	for _, r := range rest {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Errorf("Fingerprint = %q, want lowercase hex", got)
		}
	}
}

// TestTitleSimilarity: the real paraphrase pair clears the default threshold
// and unrelated findings stay well under it, so Decide can tell a repeat from a
// new finding on titles alone (gt-dhkhi).
func TestTitleSimilarity(t *testing.T) {
	t.Parallel()
	exact := func(score float64) *float64 { return &score }
	tests := []struct {
		name string
		a    string
		b    string
		// want is the exact score when set, as a pointer so 0 is a score and
		// not "unset"; otherwise the bounds apply.
		want     *float64
		atLeast  float64
		lessThan float64
	}{
		{
			name: "the real paraphrase pair scores at least the acceptance threshold",
			a:    paraphraseA,
			b:    paraphraseB,
			// 0.5 is the acceptance threshold, written out so a lowered
			// constant cannot quietly pass this fixture.
			atLeast: 0.5,
		},
		{
			name:     "unrelated findings on unrelated paths share almost nothing",
			a:        "Found: dolt connection refused during landing",
			b:        paraphraseA,
			lessThan: 0.3,
		},
		{
			name: "identical titles score 1",
			a:    "Found: the retry loop never backs off",
			b:    "found the retry loop never backs off!",
			want: exact(1),
		},
		{
			name: "a title with no long word scores 0 against anything",
			a:    "a cd",
			b:    paraphraseA,
			want: exact(0),
		},
		{
			name: "two empty titles score 0, having nothing to compare",
			a:    "!",
			b:    "the a",
			want: exact(0),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := TitleSimilarity(tt.a, tt.b)
			if got != TitleSimilarity(tt.b, tt.a) {
				t.Errorf("TitleSimilarity is not symmetric: %v vs %v", got, TitleSimilarity(tt.b, tt.a))
			}
			if tt.want != nil && got != *tt.want {
				t.Errorf("TitleSimilarity(%q, %q) = %v, want %v", tt.a, tt.b, got, *tt.want)
			}
			if tt.atLeast != 0 && got < tt.atLeast {
				t.Errorf("TitleSimilarity(%q, %q) = %v, want at least %v", tt.a, tt.b, got, tt.atLeast)
			}
			if tt.lessThan != 0 && got >= tt.lessThan {
				t.Errorf("TitleSimilarity(%q, %q) = %v, want less than %v", tt.a, tt.b, got, tt.lessThan)
			}
		})
	}
}

// TestFollowupSimilarityDefaultClearsTheKnownPair: the default threshold has to
// sit at or below what the known paraphrase pair scores, or Decide would file a
// repeat as a new bead. This is the tie between the constant and the fixture
// the threshold was chosen from (gt-dhkhi).
func TestFollowupSimilarityDefaultClearsTheKnownPair(t *testing.T) {
	t.Parallel()
	score := TitleSimilarity(paraphraseA, paraphraseB)
	if score < DefaultFollowupSimilarity {
		t.Errorf("the known paraphrase pair scores %v, below DefaultFollowupSimilarity %v", score, DefaultFollowupSimilarity)
	}
}

// TestFollowupDecide: the fingerprint label wins, then a similar title on the same
// path, and everything else is a new bead. A closed match still comments,
// because refiling a fixed finding hides that it came back (gt-dhkhi).
func TestFollowupDecide(t *testing.T) {
	t.Parallel()
	const path = "internal/land/note.go"
	const title = "Found: the retry loop never backs off"
	fingerprint := Fingerprint(path, title)
	tests := []struct {
		name     string
		c        FollowupCandidate
		existing []ExistingFollowup
		// sameSimilarity is the threshold; 0 means the default.
		sameSimilarity float64
		want           Decision
	}{
		{
			name: "the fingerprint label on an existing bead wins",
			c:    FollowupCandidate{Path: path, Title: title},
			existing: []ExistingFollowup{
				{ID: "gt-1111", Title: "something else entirely", Description: "internal/land/other.go", Labels: []string{fingerprint}},
			},
			want: Decision{Action: DedupeComment, Target: "gt-1111", Reason: "fingerprint"},
		},
		{
			name: "the fingerprint label wins even with a similarity match ahead of it",
			c:    FollowupCandidate{Path: path, Title: title},
			existing: []ExistingFollowup{
				{ID: "gt-1111", Title: title, Description: path},
				{ID: "gt-2222", Title: title, Description: path, Labels: []string{fingerprint}},
			},
			want: Decision{Action: DedupeComment, Target: "gt-2222", Reason: "fingerprint"},
		},
		{
			name: "a similar title naming the same path is the same finding",
			c:    FollowupCandidate{Path: path, Title: paraphraseA},
			existing: []ExistingFollowup{
				{ID: "gt-1111", Title: paraphraseB, Description: "seen at " + path},
			},
			want: Decision{Action: DedupeComment, Target: "gt-1111", Reason: "same-path similarity"},
		},
		{
			name: "the same words on a different path are a new finding",
			c:    FollowupCandidate{Path: path, Title: paraphraseA},
			existing: []ExistingFollowup{
				{ID: "gt-1111", Title: paraphraseB, Description: "internal/land/other.go"},
			},
			want: Decision{Action: DedupeFile},
		},
		{
			name: "a closed match still comments",
			c:    FollowupCandidate{Path: path, Title: title},
			existing: []ExistingFollowup{
				{ID: "gt-1111", Title: title, Description: path, Status: "closed"},
			},
			want: Decision{Action: DedupeComment, Target: "gt-1111", Reason: "same-path similarity"},
		},
		{
			name: "nothing to compare against files",
			c:    FollowupCandidate{Path: path, Title: title},
			want: Decision{Action: DedupeFile},
		},
		{
			name: "a finding with no path files rather than matching on a blank path",
			c:    FollowupCandidate{Title: title},
			existing: []ExistingFollowup{
				{ID: "gt-1111", Title: title, Description: path},
			},
			want: Decision{Action: DedupeFile},
		},
		{
			name: "the same path with an unrelated title is a new finding",
			c:    FollowupCandidate{Path: path, Title: "the retry loop never backs off"},
			existing: []ExistingFollowup{
				{ID: "gt-1111", Title: "dolt connection refused during landing", Description: path},
			},
			want: Decision{Action: DedupeFile},
		},
		{
			name: "a raised threshold lets a paraphrase through as a new finding",
			c:    FollowupCandidate{Path: path, Title: paraphraseA},
			existing: []ExistingFollowup{
				{ID: "gt-1111", Title: paraphraseB, Description: path},
			},
			sameSimilarity: 0.99,
			want:           Decision{Action: DedupeFile},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Decide(tt.c, tt.existing, tt.sameSimilarity)
			if got != tt.want {
				t.Errorf("Decide(%+v) = %+v, want %+v", tt.c, got, tt.want)
			}
		})
	}
}

// TestDecideToleratesNilAndEmpty: the landing worker calls this with whatever
// it read, so a nil slice and a zero threshold must not panic and must not
// turn a blank candidate into a match (gt-dhkhi).
func TestFollowupDecideToleratesNilAndEmpty(t *testing.T) {
	t.Parallel()
	if got := Decide(FollowupCandidate{}, nil, 0); got != (Decision{Action: DedupeFile}) {
		t.Errorf("Decide with an empty candidate and no existing beads = %+v, want a file decision", got)
	}
	if got := Decide(FollowupCandidate{Title: "x"}, []ExistingFollowup{{}}, 0); got.Action != DedupeFile {
		t.Errorf("Decide against an empty existing bead = %+v, want a file decision", got)
	}
}
