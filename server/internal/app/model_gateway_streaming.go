package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

type ModelTextStreamingProbeStore interface {
	BeginModelTextStreamingProbe(context.Context, ModelCallReservation) (bool, error)
	FinishModelTextStreamingProbe(context.Context, ModelCallReservation) error
}

type ModelTaskTextStreamingCapabilityStore interface {
	TaskTextStreamingCapability(context.Context, task.Task) (bool, error)
}

// A stream is chosen atomically with dispatch; missing or stale proof becomes
// non-streaming before any provider request is made.
type ModelTaskStreamingDispatchStore interface {
	BeginModelCallDispatchWithTextStreaming(context.Context, ModelCallReservation) (bool, error)
}

// TaskTextStreamingCapability is a preflight hint; the dispatch transaction
// independently checks the exact model connection version before streaming.
func (s *ModelGatewayService) TaskTextStreamingCapability(ctx context.Context, value task.Task) (bool, error) {
	if ctx == nil || !domain.ValidTaskID(value.ID) || !domain.ValidUserID(value.OwnerID) ||
		!domain.ValidUserID(value.InitiatorID) || !domain.ValidModelConnectionID(value.ModelID) ||
		value.Lease.Owner == "" || value.Lease.Version < 1 {
		return false, domain.InvalidArgument("task", "task streaming identity is invalid")
	}
	store, ok := s.store.(ModelTaskTextStreamingCapabilityStore)
	if !ok {
		return false, nil
	}
	return store.TaskTextStreamingCapability(ctx, value)
}

// A streaming check is an explicit, metered inference; it never retries a
// provider call whose delivery or usage may already be uncertain.
func (s *ModelGatewayService) checkModelTextStreaming(ctx context.Context, principal identity.Principal, modelID, key string, endpoint ModelGatewayEndpoint) (ModelCapabilities, error) {
	store, ok := s.store.(ModelTextStreamingProbeStore)
	if !ok {
		return ModelCapabilities{}, domain.NewError(domain.CodeUnsupported, "model streaming verification is unavailable")
	}
	current, err := s.models.ResolveGatewayEndpoint(ctx, principal.UserID, modelID, false, false)
	if err != nil {
		return ModelCapabilities{}, err
	}
	if !current.Version.Equal(endpoint.Version) || current.Personal != endpoint.Personal {
		return ModelCapabilities{}, domain.NewError(domain.CodeConflict, "model connection changed during check")
	}
	digest := sha256.Sum256([]byte("morde-model-probe-v1\n" + principal.UserID + "\n" + key))
	callID := "probe_" + hex.EncodeToString(digest[:])
	reservation := ModelCallReservation{Source: ModelCallProbe, Principal: principal, OwnerID: principal.UserID,
		ModelID: modelID, ProviderModel: endpoint.Endpoint.ModelID, ConnectionVersion: endpoint.Version,
		PersonalModel: endpoint.Personal, CallID: callID}
	replay, err := store.BeginModelTextStreamingProbe(ctx, reservation)
	if err != nil {
		return ModelCapabilities{}, err
	}
	if replay {
		return s.currentTextStreamingCapabilities(ctx, principal, modelID)
	}
	var deltas strings.Builder
	request := ModelRequest{Instructions: "Reply exactly OK using text streaming.",
		Input: []ModelInput{{Type: "message", Role: "user", Text: "Reply exactly OK."}}, MaxOutputTokens: 64, DisableThinking: true,
		OnTextDelta: func(delta string) error {
			if len(delta) > 1024-deltas.Len() {
				return domain.NewError(domain.CodeResultTooLarge, "model streaming probe exceeded its response limit")
			}
			_, _ = deltas.WriteString(delta)
			return nil
		}}
	response, err := s.Complete(ctx, ModelCall{Source: ModelCallProbe, Principal: principal, ModelID: modelID, CallID: callID, Request: request})
	if err != nil {
		return ModelCapabilities{}, err
	}
	if !response.Usage.Known || len(response.ToolCalls) != 0 || response.StopReason != "stop" || deltas.Len() == 0 ||
		deltas.String() != response.Text || !strings.EqualFold(strings.TrimSpace(response.Text), "OK") {
		return ModelCapabilities{}, domain.NewError(domain.CodeUpstreamError, "model streaming capability was not verified")
	}
	if err := store.FinishModelTextStreamingProbe(ctx, reservation); err != nil {
		return ModelCapabilities{}, err
	}
	return s.currentTextStreamingCapabilities(ctx, principal, modelID)
}

func (s *ModelGatewayService) currentTextStreamingCapabilities(ctx context.Context, principal identity.Principal, modelID string) (ModelCapabilities, error) {
	reader, ok := s.models.(interface {
		Get(context.Context, identity.Principal, string) (ModelConnectionView, error)
	})
	if !ok {
		return ModelCapabilities{Text: true, TextStream: true, ModelID: modelID}, nil
	}
	view, err := reader.Get(ctx, principal, modelID)
	if err != nil {
		return ModelCapabilities{}, err
	}
	if !view.Capabilities.Text || !view.Capabilities.TextStream {
		return ModelCapabilities{}, domain.NewError(domain.CodeConflict, "model streaming verification changed")
	}
	return view.Capabilities, nil
}
