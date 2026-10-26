package worker

import (
	"context"
	"errors"
	"sync"
)

const createQueueCapacity = 256

// CreateQueue receives the post-commit host-created notifications from app.
// Pending IDs include queued and currently running work, so duplicate signals
// cannot start concurrent AdvanceCreate calls for one host.
type CreateQueue struct {
	jobs    chan string
	mu      sync.Mutex
	pending map[string]struct{}
}

func NewCreateQueue() *CreateQueue {
	return &CreateQueue{jobs: make(chan string, createQueueCapacity), pending: make(map[string]struct{})}
}

func (q *CreateQueue) Notify(ctx context.Context, hostID string) error {
	if ctx == nil || hostID == "" {
		return errors.New("create notification requires context and host id")
	}
	q.mu.Lock()
	if _, exists := q.pending[hostID]; exists {
		q.mu.Unlock()
		return nil
	}
	q.pending[hostID] = struct{}{}
	q.mu.Unlock()

	select {
	case q.jobs <- hostID:
		return nil
	case <-ctx.Done():
		q.mu.Lock()
		delete(q.pending, hostID)
		q.mu.Unlock()
		return ctx.Err()
	}
}

func (q *CreateQueue) next(ctx context.Context) (string, bool) {
	select {
	case hostID := <-q.jobs:
		return hostID, true
	case <-ctx.Done():
		return "", false
	}
}

func (q *CreateQueue) finish(hostID string) {
	q.mu.Lock()
	delete(q.pending, hostID)
	q.mu.Unlock()
}
