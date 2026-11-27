package rehearsal

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// failWriter refuses every write, standing in for a closed pipe.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("write refused") }

func TestRehearsalFinish(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		edit func(*Record)
		w    io.Writer
		code int
		want string
	}{
		{name: "passing record prints pass and returns 0", want: "pass\n"},
		{name: "failing record prints each reason and returns 1", code: 1, edit: func(r *Record) { r.Stranded, r.FailedCalls = []string{"seat-3"}, []string{"sling: boom"} }, want: "fail\nstranded seat seat-3\nfailed call: sling: boom\n"},
		{name: "a refused write returns 1", w: failWriter{}, code: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := withinBars()
			if tt.edit != nil {
				tt.edit(&r)
			}
			var out strings.Builder
			w := tt.w
			if w == nil {
				w = &out
			}
			if code := Finish(r, w); code != tt.code {
				t.Fatalf("Finish = %d; want %d", code, tt.code)
			}
			if got := out.String(); got != tt.want {
				t.Errorf("Finish printed %q; want %q", got, tt.want)
			}
		})
	}
}
