package presubmit

import (
	"slices"
	"testing"
)

func TestDeclsTouching(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		src     string
		lines   []int
		want    []string
		wantErr bool
	}{
		{
			name:  "a line inside a function body",
			src:   "package p\n\nfunc f() {\n\tx := 1\n\t_ = x\n}\n",
			lines: []int{4},
			want:  []string{"f"},
		},
		{
			name:  "a method is named by its method name",
			src:   "package p\n\ntype T struct{}\n\nfunc (t T) Foo() {\n\t_ = t\n}\n",
			lines: []int{6},
			want:  []string{"Foo"},
		},
		{
			name:  "a line in a doc comment names nothing",
			src:   "package p\n\n// Doc for f.\nfunc f() {}\n",
			lines: []int{3},
		},
		{
			name:  "a line in a grouped var block names only its own spec",
			src:   "package p\n\nvar (\n\ta = 1\n\tb = 2\n)\n",
			lines: []int{5},
			want:  []string{"b"},
		},
		{
			name:  "two lines in one function name it once",
			src:   "package p\n\nfunc f() {\n\ta := 1\n\tb := 2\n\t_, _ = a, b\n}\n",
			lines: []int{4, 5},
			want:  []string{"f"},
		},
		{
			name:  "a function between two others names only itself",
			src:   "package p\n\nfunc a() {}\n\nfunc b() {\n\t_ = 1\n}\n\nfunc c() {}\n",
			lines: []int{6},
			want:  []string{"b"},
		},
		{
			name:  "a type is named by its type name",
			src:   "package p\n\ntype Thing struct {\n\tN int\n}\n",
			lines: []int{4},
			want:  []string{"Thing"},
		},
		{
			name:  "a const is named by its name",
			src:   "package p\n\nconst K = 1\n",
			lines: []int{3},
			want:  []string{"K"},
		},
		{
			name:  "a var spec names every name it declares",
			src:   "package p\n\nvar a, b = 1, 2\n",
			lines: []int{3},
			want:  []string{"a", "b"},
		},
		{
			name: "no lines is no error",
			src:  "package p\n\nfunc f() {}\n",
		},
		{
			name:  "a line outside every declaration names nothing",
			src:   "package p\n\nfunc f() {}\n",
			lines: []int{2},
		},
		{
			name:    "a source that does not parse is an error",
			src:     "package p\n\nfunc f( {\n",
			lines:   []int{3},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := declsTouching([]byte(tt.src), tt.lines)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("declsTouching(%q, %v) error = nil, want an error", tt.src, tt.lines)
				}
				if len(got) != 0 {
					t.Errorf("declsTouching(%q, %v) names = %v on error, want none", tt.src, tt.lines, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("declsTouching(%q, %v) error = %v", tt.src, tt.lines, err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("declsTouching(%q, %v) = %v, want %v", tt.src, tt.lines, got, tt.want)
			}
		})
	}
}
