package worker

import (
	"context"
	"strings"
	"sync"
	"unicode/utf8"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

type CloudDSHTextStreamingCapability interface {
	TaskTextStreamingCapability(context.Context, task.Task) (bool, error)
}

type CloudDSHTextStreamStore interface {
	AppendCloudDSHTextDelta(context.Context, app.CloudDSHTextDelta) (bool, error)
}

// A rejected or failed stream is optional: the provider continues its one
// existing request and the ordinary verified completion path remains usable.
type cloudDSHTextStream struct {
	mu       sync.Mutex
	ctx      context.Context
	store    CloudDSHTextStreamStore
	delta    app.CloudDSHTextDelta
	content  strings.Builder
	accepted bool
	disabled bool
}

func newCloudDSHTextStream(ctx context.Context, store CloudDSHTextStreamStore, fence app.ModelTaskFence, callID string,
	proof *app.CloudDSHStreamPersonalProof) *cloudDSHTextStream {
	messageID, err := domain.NewMessageID()
	if err != nil {
		return nil
	}
	turnID, err := domain.NewStepID()
	if err != nil {
		return nil
	}
	return &cloudDSHTextStream{ctx: ctx, store: store,
		delta: app.CloudDSHTextDelta{Fence: fence, CallID: callID, MessageID: messageID, TurnID: turnID, PersonalProof: proof}}
}

func (s *cloudDSHTextStream) append(text string) error {
	if text == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled || s.ctx.Err() != nil || !utf8.ValidString(text) || strings.ContainsRune(text, '\x00') ||
		s.content.Len()+len(text) > app.MaxAssistantMessageBytes {
		s.disabled = true
		return nil
	}
	if app.HasUnverifiedFileDeliveryClaim(s.content.String() + text) {
		// Stop only display; the single provider request must still settle.
		s.disabled = true
		return nil
	}
	delta := s.delta
	delta.Text = text
	appended, err := s.store.AppendCloudDSHTextDelta(s.ctx, delta)
	if err != nil || !appended {
		s.disabled = true
		return nil
	}
	s.accepted = true
	s.content.WriteString(text)
	return nil
}

func (s *cloudDSHTextStream) matchingResult(result string) (string, string, bool) {
	if s == nil {
		return "", "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	visible := s.content.String()
	if !s.accepted || visible == "" || !strings.HasPrefix(result, visible) {
		return "", "", false
	}
	return s.delta.MessageID, s.delta.TurnID, true
}

func (s *cloudDSHTextStream) hasVisibleText() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepted && s.content.Len() != 0
}
