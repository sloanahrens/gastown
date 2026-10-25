package api

import "context"

type actorKey struct{}

// WithActor returns ctx carrying the actor a request acts for. The server
// puts the token's actor here; store writes that take no actor argument
// (close, delete) record it on their events.
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// ActorFrom is the actor WithActor stored, or "".
func ActorFrom(ctx context.Context) string {
	a, _ := ctx.Value(actorKey{}).(string)
	return a
}
