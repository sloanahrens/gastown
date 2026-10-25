package imageprune

import (
	"errors"
	"reflect"
	"testing"
)

func TestRemoveAllGoesOnPastAFailedRemoval(t *testing.T) {
	t.Parallel()
	plan := []Image{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	boom := errors.New("DeadlineExceeded")
	var tried []string
	removed, failed := RemoveAll(plan, func(id string) error {
		tried = append(tried, id)
		if id == "b" {
			return boom
		}
		return nil
	})
	if want := []string{"a", "b", "c"}; !reflect.DeepEqual(tried, want) {
		t.Errorf("tried %v, want %v: a failure must not stop the run", tried, want)
	}
	if want := []string{"a", "c"}; !reflect.DeepEqual(removed, want) {
		t.Errorf("removed %v, want %v", removed, want)
	}
	if !errors.Is(failed["b"], boom) || len(failed) != 1 {
		t.Errorf("failed = %v, want only b with its error", failed)
	}
}
