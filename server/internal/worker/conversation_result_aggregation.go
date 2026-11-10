package worker

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"

	"morde/server/internal/app"
)

type ConversationResultReviewWorker struct {
	store    app.ConversationResultReviewQueue
	interval time.Duration
	logger   *zap.Logger
}

func NewConversationResultReviewWorker(store app.ConversationResultReviewQueue, interval time.Duration, logger *zap.Logger) (*ConversationResultReviewWorker, error) {
	if store == nil || interval <= 0 || interval > 100*time.Millisecond || logger == nil {
		return nil, errors.New("conversation result worker dependencies and polling interval are required")
	}
	return &ConversationResultReviewWorker{store: store, interval: interval, logger: logger}, nil
}

// Run publishes persisted deadlines after restarts; each batch is committed by
// the Store in a transaction, so several workers can safely poll concurrently.
func (w *ConversationResultReviewWorker) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("conversation result worker context is required")
	}
	for ctx.Err() == nil {
		queued, err := w.store.QueueReadyConversationResultReviews(ctx, 100)
		if err != nil && ctx.Err() == nil {
			w.logger.Warn("conversation result review queue failed", zap.String("job", "conversation_result_review"))
		}
		if queued > 0 && err == nil {
			continue
		}
		timer := time.NewTimer(w.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return ctx.Err()
}
