package app

import "context"

type expectedServiceInstanceKey struct{}

// WithExpectedServiceInstance carries an outbox's original service identity
// through all database transactions that implement a personal-data operation.
func WithExpectedServiceInstance(ctx context.Context, instanceID string) context.Context {
	return context.WithValue(ctx, expectedServiceInstanceKey{}, instanceID)
}

func ExpectedServiceInstance(ctx context.Context) string {
	value, _ := ctx.Value(expectedServiceInstanceKey{}).(string)
	return value
}
