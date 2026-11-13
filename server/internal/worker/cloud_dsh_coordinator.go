package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

type CloudDSHCoordinatorModelGateway interface {
	ResolveConversationRouteModel(context.Context, app.ConversationRouteClaim) (string, error)
	RecoverConversationCoordinatorModelOutput(context.Context, app.ConversationRouteClaim, app.ConversationRouteContext) (string, error)
	CompleteConversationCoordinatorModel(context.Context, app.ConversationRouteClaim, app.ConversationRouteContext,
		string, int64, json.RawMessage) (app.ModelResponse, error)
}

type CloudDSHCoordinatorCheckpoints interface {
	Head(context.Context, app.ConversationRouteClaim, app.ConversationCoordinatorIdentity, []byte) (app.ConversationCoordinatorCheckpointHead, error)
	Save(context.Context, app.ConversationCoordinatorCheckpointCommand) (bool, error)
	ReadPage(context.Context, app.ConversationCoordinatorCheckpointPageRequest) (app.ConversationCoordinatorCheckpointPage, error)
}

type cloudDSHCoordinatorStream interface {
	read(context.Context) (cloudDSHCoordinatorFrame, error)
}

type cloudDSHCoordinatorTurn struct {
	stream        cloudDSHCoordinatorStream
	through       int64
	checkpointIDs map[string]struct{}
	readThrough   int64
	pageReadIDs   map[string]struct{}
}

func (t *cloudDSHCoordinatorTurn) read(ctx context.Context) (cloudDSHCoordinatorFrame, error) {
	return t.stream.read(ctx)
}

type cloudDSHCoordinatorRuntime interface {
	StartCoordinator(context.Context, app.ConversationRouteClaim, app.ConversationCoordinatorIdentity, map[string]any) (cloudDSHCoordinatorStream, error)
	WriteCoordinatorFrame(context.Context, app.ConversationCoordinatorIdentity, map[string]any) error
	StopCoordinator(context.Context, app.ConversationCoordinatorIdentity) error
}

type cloudDSHCoordinatorRuntimeManager interface {
	Acquire(context.Context, string, string) (cloudDSHCoordinatorRuntime, error)
}

type cloudDSHCoordinatorAccountRuntimeManager struct {
	manager *CloudDSHAccountRuntimeManager
}

type cloudDSHCoordinatorAccountRuntime struct {
	manager *CloudDSHAccountRuntimeManager
	session *cloudDSHAccountRuntimeSession
}

type cloudDSHCoordinatorAccountStream struct {
	coordinator *cloudDSHAccountCoordinator
}

func (m cloudDSHCoordinatorAccountRuntimeManager) Acquire(ctx context.Context, ownerID, conversationID string) (cloudDSHCoordinatorRuntime, error) {
	session, err := m.manager.Acquire(ctx, ownerID, conversationID)
	if err != nil {
		return nil, err
	}
	return &cloudDSHCoordinatorAccountRuntime{manager: m.manager, session: session}, nil
}

func (r *cloudDSHCoordinatorAccountRuntime) StartCoordinator(ctx context.Context, claim app.ConversationRouteClaim,
	identity app.ConversationCoordinatorIdentity, start map[string]any) (cloudDSHCoordinatorStream, error) {
	coordinator, err := r.session.StartCoordinator(ctx, claim, identity, start)
	if err != nil {
		return nil, err
	}
	return cloudDSHCoordinatorAccountStream{coordinator: coordinator}, nil
}

func (r *cloudDSHCoordinatorAccountRuntime) WriteCoordinatorFrame(ctx context.Context,
	identity app.ConversationCoordinatorIdentity, frame map[string]any) error {
	return r.session.writeCoordinatorFrame(ctx, identity, frame)
}

func (r *cloudDSHCoordinatorAccountRuntime) StopCoordinator(ctx context.Context,
	identity app.ConversationCoordinatorIdentity) error {
	return r.manager.StopCoordinator(ctx, r.session, identity)
}

func (s cloudDSHCoordinatorAccountStream) read(ctx context.Context) (cloudDSHCoordinatorFrame, error) {
	return s.coordinator.read(ctx)
}

type CloudDSHConversationCoordinatorOptions struct {
	AccountRuntimes *CloudDSHAccountRuntimeManager
	Models          CloudDSHCoordinatorModelGateway
	Checkpoints     CloudDSHCoordinatorCheckpoints
	SummaryStore    app.ConversationSummaryStore
}

// CloudDSHConversationCoordinator runs one isolated DSH turn for one persisted
// message route. It has no Task identity and exposes no tools.
type CloudDSHConversationCoordinator struct {
	options  CloudDSHConversationCoordinatorOptions
	runtimes cloudDSHCoordinatorRuntimeManager
}

func NewCloudDSHConversationCoordinator(options CloudDSHConversationCoordinatorOptions) (*CloudDSHConversationCoordinator, error) {
	if options.AccountRuntimes == nil || options.Models == nil || options.Checkpoints == nil {
		return nil, errors.New("DSH conversation coordinator requires runtime, model gateway and checkpoint services")
	}
	return &CloudDSHConversationCoordinator{options: options,
		runtimes: cloudDSHCoordinatorAccountRuntimeManager{manager: options.AccountRuntimes}}, nil
}

func (c *CloudDSHConversationCoordinator) ResolveConversationRouteModel(ctx context.Context, claim app.ConversationRouteClaim) (string, error) {
	return c.options.Models.ResolveConversationRouteModel(ctx, claim)
}

func (c *CloudDSHConversationCoordinator) ClassifyConversationRoute(ctx context.Context, claim app.ConversationRouteClaim,
	value app.ConversationRouteContext) (app.ConversationRouteDecision, error) {
	if ctx == nil || value.ModelID == "" || claim.Message.ID == "" || value.ActorID != claim.Message.ActorID || value.Content != claim.Message.Content {
		return app.ConversationRouteDecision{}, domain.InvalidArgument("coordinator", "coordinator route context is invalid")
	}
	identity := cloudDSHCoordinatorIdentity(claim, value)
	if err := app.ValidateConversationCoordinatorIdentity(claim, identity); err != nil {
		return app.ConversationRouteDecision{}, err
	}
	contextDigest, err := app.ConversationCoordinatorContextDigest(value)
	if err != nil {
		return app.ConversationRouteDecision{}, err
	}
	head, err := c.options.Checkpoints.Head(ctx, claim, identity, contextDigest)
	if err != nil {
		return app.ConversationRouteDecision{}, err
	}
	if head.Through >= 0 && (len(head.ContextDigest) != sha256.Size || !bytes.Equal(head.ContextDigest, contextDigest)) {
		return app.ConversationRouteDecision{}, domain.NewError(domain.CodeConflict, "coordinator context changed since its checkpoint")
	}
	request, err := app.ConversationRouteModelRequest(value)
	if err != nil {
		return app.ConversationRouteDecision{}, err
	}
	if len(request.Input) != 1 || request.Input[0].Text == "" || len(request.Input[0].Images) != 0 || len(request.Tools) != 0 {
		return app.ConversationRouteDecision{}, domain.InvalidArgument("coordinator", "coordinator context must be text-only")
	}
	start := map[string]any{
		"type": "coordinator_start", "version": 1, "context_role": "coordinator",
		"conversation_id": identity.ConversationID, "message_id": identity.MessageID,
		"owner_id": identity.OwnerID, "actor_id": identity.ActorID, "agent_id": identity.AgentID, "generation": identity.Generation,
		"authorization_version": identity.AuthorizationVersion, "session_id": identity.SessionID,
		"purpose": identity.Purpose, "operation_id": identity.OperationID,
		"model": value.ModelID, "tools": []any{}, "system_prompt": request.Instructions,
		"resume": head.Through >= 0,
	}
	if head.Through >= 0 {
		start["checkpoint_through_seq"] = head.Through
		start["checkpoint_sha256"] = hex.EncodeToString(head.Digest)
	} else {
		start["prompt"] = request.Input[0].Text
	}
	runtime, err := c.runtimes.Acquire(ctx, claim.Message.OwnerID, claim.Message.ConversationID)
	if err != nil {
		return app.ConversationRouteDecision{}, err
	}
	stream, err := runtime.StartCoordinator(ctx, claim, identity, start)
	if err != nil {
		return app.ConversationRouteDecision{}, err
	}
	session := &cloudDSHCoordinatorTurn{stream: stream, through: head.Through,
		checkpointIDs: make(map[string]struct{}),
		readThrough:   -1, pageReadIDs: make(map[string]struct{})}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_ = runtime.StopCoordinator(stopCtx, identity)
	}()

	var lastModel *app.ModelResponse
	for {
		frame, readErr := session.read(ctx)
		if readErr != nil {
			return app.ConversationRouteDecision{}, readErr
		}
		switch frame.Type {
		case "checkpoint":
			if frame.ID == "" || frame.From == nil || frame.Through == nil || *frame.From != session.through+1 ||
				*frame.Through < *frame.From || *frame.Through > 1<<53-1 || len(frame.Payload) == 0 ||
				len(frame.Payload) > 1<<20 || !utf8.ValidString(frame.Payload) || !uniqueCloudDSHJSON([]byte(frame.Payload)) ||
				!cloudDSHDigest([]byte(frame.Payload), frame.SHA256) {
				return app.ConversationRouteDecision{}, errors.New("DSH coordinator checkpoint is invalid")
			}
			if _, exists := session.checkpointIDs[frame.ID]; exists || len(session.checkpointIDs) >= 4096 {
				return app.ConversationRouteDecision{}, errors.New("DSH coordinator checkpoint identity is repeated")
			}
			digest, _ := hex.DecodeString(frame.SHA256)
			_, err := c.options.Checkpoints.Save(ctx, app.ConversationCoordinatorCheckpointCommand{Claim: claim, Identity: identity,
				ContextDigest: contextDigest, From: *frame.From, Through: *frame.Through, Digest: digest, Payload: []byte(frame.Payload)})
			if err != nil {
				return app.ConversationRouteDecision{}, err
			}
			session.checkpointIDs[frame.ID] = struct{}{}
			session.through = *frame.Through
			if err := runtime.WriteCoordinatorFrame(ctx, identity, map[string]any{"type": "checkpoint_ack", "context_role": "coordinator",
				"conversation_id": identity.ConversationID, "message_id": identity.MessageID, "owner_id": identity.OwnerID, "actor_id": identity.ActorID,
				"agent_id": identity.AgentID, "generation": identity.Generation, "authorization_version": identity.AuthorizationVersion,
				"session_id": identity.SessionID, "purpose": identity.Purpose, "operation_id": identity.OperationID,
				"id": frame.ID, "ok": true, "through_seq": session.through, "sha256": frame.SHA256}); err != nil {
				return app.ConversationRouteDecision{}, err
			}
		case "checkpoint_read":
			if frame.ID == "" || frame.From == nil || *frame.From < 0 || *frame.From > head.Through ||
				*frame.From != session.readThrough+1 || len(session.pageReadIDs) >= 100_000 {
				return app.ConversationRouteDecision{}, errors.New("DSH coordinator checkpoint read is invalid")
			}
			if _, exists := session.pageReadIDs[frame.ID]; exists {
				return app.ConversationRouteDecision{}, errors.New("DSH coordinator checkpoint read identity is repeated")
			}
			page, err := c.options.Checkpoints.ReadPage(ctx, app.ConversationCoordinatorCheckpointPageRequest{Claim: claim,
				Identity: identity, ContextDigest: contextDigest, From: *frame.From, Through: head.Through, ThroughDigest: head.Digest})
			if err != nil || page.From != *frame.From || page.Through < page.From || page.Through > head.Through ||
				len(page.Payload) == 0 || !cloudDSHDigest(page.Payload, hex.EncodeToString(page.Digest)) {
				return app.ConversationRouteDecision{}, errors.New("DSH coordinator checkpoint page is unavailable")
			}
			if page.Through == head.Through && !cloudDSHDigest(page.Payload, hex.EncodeToString(head.Digest)) {
				return app.ConversationRouteDecision{}, errors.New("DSH coordinator checkpoint digest is invalid")
			}
			if err := runtime.WriteCoordinatorFrame(ctx, identity, map[string]any{"type": "checkpoint_page", "context_role": "coordinator",
				"conversation_id": identity.ConversationID, "message_id": identity.MessageID, "owner_id": identity.OwnerID, "actor_id": identity.ActorID,
				"agent_id": identity.AgentID, "generation": identity.Generation, "authorization_version": identity.AuthorizationVersion,
				"session_id": identity.SessionID, "purpose": identity.Purpose, "operation_id": identity.OperationID,
				"id": frame.ID, "ok": true, "from_seq": page.From, "through_seq": page.Through,
				"payload": string(page.Payload), "sha256": hex.EncodeToString(page.Digest)}); err != nil {
				return app.ConversationRouteDecision{}, err
			}
			session.pageReadIDs[frame.ID] = struct{}{}
			session.readThrough = page.Through
		case "model":
			if frame.ID == "" || frame.Through == nil || *frame.Through != session.through || session.through < 0 ||
				!cloudDSHDigest(frame.Request, frame.SHA256) || len(frame.Request) == 0 || lastModel != nil {
				return app.ConversationRouteDecision{}, errors.New("DSH coordinator model request is invalid")
			}
			callID, err := app.DSHConversationCoordinatorModelCallID(identity.ConversationID, identity.MessageID, identity.SessionID, session.through)
			if err != nil || frame.ID != callID {
				return app.ConversationRouteDecision{}, errors.New("DSH coordinator model identity is invalid")
			}
			response, err := c.options.Models.CompleteConversationCoordinatorModel(ctx, claim, value, identity.SessionID, session.through, frame.Request)
			if err != nil || !validCoordinatorModelResponse(response, app.MaxConversationRouteModelOutputBytes) {
				if err == nil {
					err = errors.New("DSH coordinator model response is invalid")
				}
				return app.ConversationRouteDecision{}, err
			}
			lastModel = &response
			if err := writeCloudDSHCoordinatorModel(ctx, runtime, identity, frame.ID, response,
				app.MaxConversationRouteModelOutputBytes); err != nil {
				return app.ConversationRouteDecision{}, err
			}
		case "model_cancel":
			if lastModel == nil || frame.ID == "" {
				return app.ConversationRouteDecision{}, errors.New("DSH coordinator model cancellation identity is invalid")
			}
		case "coordinator_settled":
			if frame.State != "completed" || frame.Through == nil || *frame.Through != session.through ||
				!app.ValidConversationRouteModelOutput(frame.Result) || strings.TrimSpace(frame.Result) == "" {
				return app.ConversationRouteDecision{}, errors.New("DSH coordinator did not settle a verified model result")
			}
			if lastModel == nil {
				output, err := c.options.Models.RecoverConversationCoordinatorModelOutput(ctx, claim, value)
				if err != nil {
					return app.ConversationRouteDecision{}, err
				}
				lastModel = &app.ModelResponse{Text: output, StopReason: "stop", Usage: app.ModelUsage{Known: true}}
			}
			if frame.Result != lastModel.Text {
				return app.ConversationRouteDecision{}, errors.New("DSH coordinator result does not match its metered model output")
			}
			decision := app.ParseConversationRouteDecisionForContext(frame.Result, value)
			decision.ModelOutput = frame.Result
			return decision, nil
		case "coordinator_failed", "coordinator_terminated", "failed":
			return app.ConversationRouteDecision{}, errors.New("DSH coordinator turn did not complete")
		default:
			return app.ConversationRouteDecision{}, fmt.Errorf("unsupported DSH coordinator frame %q", frame.Type)
		}
	}
}

func cloudDSHCoordinatorIdentity(claim app.ConversationRouteClaim, value app.ConversationRouteContext) app.ConversationCoordinatorIdentity {
	seed := strings.Join([]string{"morde-dsh-coordinator-session-v1", claim.ServiceInstanceID, claim.Message.OwnerID,
		claim.Message.ActorID, claim.Message.ConversationID, claim.Message.ID}, "\n")
	if value.ResultReview != nil {
		if value.ResultReview.RetryAttempt > 0 {
			// Retries must not resume a completed DSH turn from the previous attempt.
			seed += fmt.Sprintf("\nresult-review-retry:%d", value.ResultReview.RetryAttempt)
		}
	} else if claim.RouteAttempt > 0 {
		seed += fmt.Sprintf("\nroute-attempt:%d", claim.RouteAttempt)
	}
	digest := sha256.Sum256([]byte(seed))
	hexDigest := hex.EncodeToString(digest[:])
	return app.ConversationCoordinatorIdentity{ServiceInstanceID: claim.ServiceInstanceID, OwnerID: claim.Message.OwnerID,
		ActorID: claim.Message.ActorID, ConversationID: claim.Message.ConversationID, MessageID: claim.Message.ID,
		AgentID: "coord_" + hexDigest[:32], Generation: 1, AuthorizationVersion: claim.Authorization.Version,
		SessionID: "coord_session_" + hexDigest[:40], Purpose: "conversation"}
}

func validCoordinatorModelResponse(response app.ModelResponse, maxOutputBytes int) bool {
	return response.StopReason == "stop" && len(response.ToolCalls) == 0 && response.Usage.Known &&
		response.Usage.InputTokens >= 0 && response.Usage.OutputTokens >= 0 &&
		utf8.ValidString(response.Text) && len(response.Text) > 0 && len(response.Text) <= maxOutputBytes
}

func writeCloudDSHCoordinatorModel(ctx context.Context, runtime cloudDSHCoordinatorRuntime,
	identity app.ConversationCoordinatorIdentity, id string, response app.ModelResponse, maxOutputBytes int) error {
	if !validCoordinatorModelResponse(response, maxOutputBytes) || id == "" {
		return errors.New("DSH coordinator model response cannot be encoded")
	}
	base := map[string]any{"context_role": "coordinator", "conversation_id": identity.ConversationID,
		"message_id": identity.MessageID, "owner_id": identity.OwnerID, "actor_id": identity.ActorID, "agent_id": identity.AgentID,
		"generation": identity.Generation, "authorization_version": identity.AuthorizationVersion,
		"session_id": identity.SessionID, "purpose": identity.Purpose, "operation_id": identity.OperationID, "id": id}
	chunks := []any{
		map[string]any{"type": "block-start", "index": 0, "blockType": "text"},
		map[string]any{"type": "text-delta", "index": 0, "text": response.Text},
		map[string]any{"type": "block-end", "index": 0, "block": map[string]any{"type": "text", "text": response.Text}},
		map[string]any{"type": "usage", "usage": map[string]any{"inputTokens": response.Usage.InputTokens, "outputTokens": response.Usage.OutputTokens}},
		map[string]any{"type": "finish", "reason": map[string]any{"kind": "stop"}},
	}
	for index, chunk := range chunks {
		frame := map[string]any{"type": "model_chunk", "index": index, "chunk": chunk}
		for key, value := range base {
			frame[key] = value
		}
		if err := runtime.WriteCoordinatorFrame(ctx, identity, frame); err != nil {
			return err
		}
	}
	frame := map[string]any{"type": "model_end", "ok": true}
	for key, value := range base {
		frame[key] = value
	}
	return runtime.WriteCoordinatorFrame(ctx, identity, frame)
}
