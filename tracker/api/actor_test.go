package api

import (
	"context"
	"testing"
)

func TestActorRoundTripsThroughTheContext(t *testing.T) {
	t.Parallel() // unit tier: pure
	if got := ActorFrom(context.Background()); got != "" {
		t.Errorf("ActorFrom(empty) = %q, want \"\"", got)
	}
	if got := ActorFrom(WithActor(context.Background(), "town/crew/dipper")); got != "town/crew/dipper" {
		t.Errorf("ActorFrom = %q, want the actor stored", got)
	}
}
