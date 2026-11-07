package app

import (
	"context"
	"errors"
)

// ErrTaskRevisionInterrupted is emitted only after the old lease and its
// revision stop request have been verified against durable authorization.
var ErrTaskRevisionInterrupted = errors.New("task interrupted for a newer revision")

type modelTaskLifecycleKey struct{}
type modelExplicitCancellationKey struct{}

// WithModelTaskLifecycle carries the independent worker/service lifetime so a
// model gateway can stop all remaining work when that lifetime ends.
func WithModelTaskLifecycle(ctx, lifecycle context.Context) context.Context {
	if ctx == nil || lifecycle == nil {
		return ctx
	}
	return context.WithValue(ctx, modelTaskLifecycleKey{}, lifecycle)
}

func ModelTaskLifecycle(ctx context.Context) context.Context {
	if ctx == nil {
		return nil
	}
	value, _ := ctx.Value(modelTaskLifecycleKey{}).(context.Context)
	return value
}

// WithModelExplicitCancellation keeps a direct DSH model_cancel distinct from
// a revision cause that may already have cancelled the parent context.
func WithModelExplicitCancellation(ctx, explicit context.Context) context.Context {
	if ctx == nil || explicit == nil {
		return ctx
	}
	return context.WithValue(ctx, modelExplicitCancellationKey{}, explicit)
}

func ModelExplicitCancellation(ctx context.Context) context.Context {
	if ctx == nil {
		return nil
	}
	value, _ := ctx.Value(modelExplicitCancellationKey{}).(context.Context)
	return value
}
