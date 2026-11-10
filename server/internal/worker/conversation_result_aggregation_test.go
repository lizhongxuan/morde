package worker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

type conversationResultWorkerStore struct {
	calls  atomic.Int32
	cancel context.CancelFunc
}

func (s *conversationResultWorkerStore) QueueReadyConversationResultReviews(context.Context, int) (int, error) {
	if s.calls.Add(1) == 1 {
		return 1, nil
	}
	s.cancel()
	return 0, nil
}

func TestConversationResultWorkerDrainsAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &conversationResultWorkerStore{cancel: cancel}
	worker, err := NewConversationResultReviewWorker(store, 100*time.Millisecond, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Run(ctx); !errors.Is(err, context.Canceled) || store.calls.Load() != 2 {
		t.Fatalf("result worker = calls %d, err %v", store.calls.Load(), err)
	}
}
