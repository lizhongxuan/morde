package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"unicode/utf8"

	"morde/server/internal/app"
	"morde/server/internal/domain"
)

type cloudDSHCoordinatorIdentityWire struct {
	Role                 string `json:"context_role"`
	ConversationID       string `json:"conversation_id"`
	MessageID            string `json:"message_id"`
	OwnerID              string `json:"owner_id"`
	ActorID              string `json:"actor_id"`
	AgentID              string `json:"agent_id"`
	Generation           int64  `json:"generation"`
	AuthorizationVersion int64  `json:"authorization_version"`
	SessionID            string `json:"session_id"`
	Purpose              string `json:"purpose"`
	OperationID          string `json:"operation_id"`
}

type cloudDSHCoordinatorFrame struct {
	Type                 string          `json:"type"`
	Role                 string          `json:"context_role"`
	ConversationID       string          `json:"conversation_id"`
	MessageID            string          `json:"message_id"`
	OwnerID              string          `json:"owner_id"`
	ActorID              string          `json:"actor_id"`
	AgentID              string          `json:"agent_id"`
	Generation           int64           `json:"generation"`
	AuthorizationVersion int64           `json:"authorization_version"`
	SessionID            string          `json:"session_id"`
	Purpose              string          `json:"purpose"`
	OperationID          string          `json:"operation_id"`
	ID                   string          `json:"id,omitempty"`
	From                 *int64          `json:"from_seq,omitempty"`
	Through              *int64          `json:"through_seq,omitempty"`
	Payload              string          `json:"payload,omitempty"`
	SHA256               string          `json:"sha256,omitempty"`
	Request              json.RawMessage `json:"request,omitempty"`
	State                string          `json:"state,omitempty"`
	Result               string          `json:"result,omitempty"`
	Code                 string          `json:"code,omitempty"`
	Reason               string          `json:"reason,omitempty"`
	ProcessExited        bool            `json:"process_exited,omitempty"`
}

func cloudDSHCoordinatorFrameBaseKeys() []string {
	return []string{"type", "server_instance_id", "owner_id", "runtime_id", "runtime_generation",
		"context_role", "conversation_id", "message_id", "actor_id", "agent_id", "generation", "authorization_version", "session_id", "purpose", "operation_id"}
}

func cloudDSHExactFrameKeys(frame map[string]json.RawMessage, expected []string) bool {
	if len(frame) != len(expected) {
		return false
	}
	for _, key := range expected {
		if _, ok := frame[key]; !ok {
			return false
		}
	}
	return true
}

func validCloudDSHCoordinatorRuntimeFrame(kind string, frame map[string]json.RawMessage) bool {
	base := cloudDSHCoordinatorFrameBaseKeys()
	switch kind {
	case "coordinator_process_ready":
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "runtime_sequence", "container_pid"))
	case "coordinator_started":
		return cloudDSHExactFrameKeys(frame, base)
	case "checkpoint":
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "id", "from_seq", "through_seq", "payload", "sha256"))
	case "checkpoint_read":
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "id", "from_seq"))
	case "model":
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "id", "through_seq", "request", "sha256"))
	case "model_cancel":
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "id"))
	case "coordinator_settled":
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "state", "through_seq", "result"))
	case "coordinator_failed":
		if cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "code")) {
			return true
		}
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "code", "reason", "process_exited"))
	case "coordinator_terminated":
		return cloudDSHExactFrameKeys(frame, append(append([]string(nil), base...), "reason", "state", "process_exited"))
	default:
		return false
	}
}

func decodeCloudDSHCoordinatorIdentity(frame map[string]json.RawMessage) (app.ConversationCoordinatorIdentity, error) {
	var wire cloudDSHCoordinatorIdentityWire
	identityFrame := make(map[string]json.RawMessage, 11)
	for _, key := range []string{"context_role", "conversation_id", "message_id", "owner_id", "actor_id", "agent_id", "generation", "authorization_version", "session_id", "purpose", "operation_id"} {
		if value, ok := frame[key]; ok {
			identityFrame[key] = value
		}
	}
	encoded, err := json.Marshal(identityFrame)
	if err != nil {
		return app.ConversationCoordinatorIdentity{}, errors.New("account Agent Runtime coordinator identity is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&wire) != nil || decoder.Decode(new(any)) != io.EOF || wire.Role != "coordinator" ||
		!cloudDSHID.MatchString(wire.OwnerID) || !cloudDSHID.MatchString(wire.ActorID) || !cloudDSHID.MatchString(wire.AgentID) ||
		!cloudDSHID.MatchString(wire.SessionID) || wire.Generation < 1 || wire.AuthorizationVersion < 1 ||
		(wire.Purpose != "conversation" && wire.Purpose != "compaction") ||
		(wire.Purpose == "conversation" && wire.OperationID != "") ||
		(wire.Purpose == "compaction" && !domain.ValidOperationID(wire.OperationID)) {
		return app.ConversationCoordinatorIdentity{}, errors.New("account Agent Runtime coordinator identity is invalid")
	}
	// The service instance ID is inherited from the account Runtime fence, so
	// callers fill it after validating the outer Runtime identity.
	if !domain.ValidConversationID(wire.ConversationID) || !domain.ValidMessageID(wire.MessageID) {
		return app.ConversationCoordinatorIdentity{}, errors.New("account Agent Runtime coordinator scope is invalid")
	}
	return app.ConversationCoordinatorIdentity{OwnerID: wire.OwnerID, ActorID: wire.ActorID,
		ConversationID: wire.ConversationID, MessageID: wire.MessageID, AgentID: wire.AgentID,
		Generation: wire.Generation, AuthorizationVersion: wire.AuthorizationVersion, SessionID: wire.SessionID,
		Purpose: wire.Purpose, OperationID: wire.OperationID}, nil
}

func (c *cloudDSHAccountCoordinator) identityMatches(value app.ConversationCoordinatorIdentity) bool {
	return c.identity == value
}

func (c *cloudDSHAccountCoordinator) read(ctx context.Context) (cloudDSHCoordinatorFrame, error) {
	if ctx == nil {
		return cloudDSHCoordinatorFrame{}, errors.New("coordinator read context is required")
	}
	select {
	case <-ctx.Done():
		return cloudDSHCoordinatorFrame{}, ctx.Err()
	case <-c.ctx.Done():
		return cloudDSHCoordinatorFrame{}, c.ctx.Err()
	case item := <-c.events:
		if item.err != nil {
			return cloudDSHCoordinatorFrame{}, item.err
		}
		frame, err := decodeCloudDSHCoordinatorFrame(item.frame)
		frameIdentity := frame.identity()
		frameIdentity.ServiceInstanceID = c.identity.ServiceInstanceID
		if err != nil || !c.identityMatches(frameIdentity) {
			return cloudDSHCoordinatorFrame{}, errors.New("DSH coordinator frame identity is invalid")
		}
		return frame, nil
	}
}

func (f cloudDSHCoordinatorFrame) identity() app.ConversationCoordinatorIdentity {
	return app.ConversationCoordinatorIdentity{ServiceInstanceID: "", OwnerID: f.OwnerID, ActorID: f.ActorID,
		ConversationID: f.ConversationID, MessageID: f.MessageID, AgentID: f.AgentID,
		Generation: f.Generation, AuthorizationVersion: f.AuthorizationVersion, SessionID: f.SessionID,
		Purpose: f.Purpose, OperationID: f.OperationID}
}

func decodeCloudDSHCoordinatorFrame(raw []byte) (cloudDSHCoordinatorFrame, error) {
	var frame cloudDSHCoordinatorFrame
	if !uniqueCloudDSHJSON(raw) || !utf8.Valid(raw) {
		return frame, errors.New("DSH coordinator frame JSON is invalid")
	}
	var keys map[string]json.RawMessage
	if json.Unmarshal(raw, &keys) != nil || keys == nil {
		return frame, errors.New("DSH coordinator frame is not an object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&frame) != nil || decoder.Decode(new(any)) != io.EOF || frame.Role != "coordinator" ||
		!cloudDSHID.MatchString(frame.OwnerID) || !cloudDSHID.MatchString(frame.ActorID) || !cloudDSHID.MatchString(frame.AgentID) || !cloudDSHID.MatchString(frame.SessionID) ||
		(frame.Purpose != "conversation" && frame.Purpose != "compaction") ||
		(frame.Purpose == "conversation" && frame.OperationID != "") || (frame.Purpose == "compaction" && !domain.ValidOperationID(frame.OperationID)) ||
		frame.Generation < 1 || frame.AuthorizationVersion < 1 || frame.ConversationID == "" || frame.MessageID == "" {
		return cloudDSHCoordinatorFrame{}, errors.New("DSH coordinator frame fields are invalid")
	}
	base := []string{"type", "context_role", "conversation_id", "message_id", "owner_id", "actor_id", "agent_id", "generation", "authorization_version", "session_id", "purpose", "operation_id"}
	var expected []string
	switch frame.Type {
	case "checkpoint":
		expected = append(append([]string(nil), base...), "id", "from_seq", "through_seq", "payload", "sha256")
	case "checkpoint_read":
		expected = append(append([]string(nil), base...), "id", "from_seq")
	case "model":
		expected = append(append([]string(nil), base...), "id", "through_seq", "request", "sha256")
	case "model_cancel":
		expected = append(append([]string(nil), base...), "id")
	case "coordinator_settled":
		expected = append(append([]string(nil), base...), "state", "through_seq", "result")
	case "coordinator_failed":
		expected = append(append([]string(nil), base...), "code")
		if !cloudDSHExactFrameKeys(keys, expected) {
			expected = append(append([]string(nil), base...), "code", "reason", "process_exited")
		}
	case "coordinator_terminated":
		expected = append(append([]string(nil), base...), "reason", "state", "process_exited")
	default:
		return cloudDSHCoordinatorFrame{}, errors.New("DSH coordinator frame has unsupported type")
	}
	if !cloudDSHExactFrameKeys(keys, expected) {
		return cloudDSHCoordinatorFrame{}, errors.New("DSH coordinator frame has unsupported fields")
	}
	switch frame.Type {
	case "coordinator_settled":
		if frame.State != "completed" {
			return cloudDSHCoordinatorFrame{}, errors.New("DSH coordinator settlement state is invalid")
		}
	case "coordinator_failed":
		if frame.Code == "" || len(frame.Code) > 128 || (frame.ProcessExited != (frame.Reason != "")) {
			return cloudDSHCoordinatorFrame{}, errors.New("DSH coordinator failure details are invalid")
		}
	case "coordinator_terminated":
		if frame.Reason == "" || len(frame.Reason) > 128 || frame.State != "cancelled" || !frame.ProcessExited {
			return cloudDSHCoordinatorFrame{}, errors.New("DSH coordinator termination details are invalid")
		}
	}
	return frame, nil
}

type cloudDSHAccountCoordinator struct {
	ctx           context.Context
	session       *cloudDSHAccountRuntimeSession
	identity      app.ConversationCoordinatorIdentity
	sequence      int64
	events        chan cloudDSHRead
	startedResult chan error
	finished      chan struct{}
	slots         chan struct{}
	mu            sync.Mutex
	startOnce     sync.Once
	finishOnce    sync.Once
	slotOnce      sync.Once
	checkpointIDs map[string]struct{}
	pageReadIDs   map[string]struct{}
	through       int64
	readThrough   int64
	admitted      bool
	started       bool
	terminal      bool
	failure       error
}

func (c *cloudDSHAccountCoordinator) releaseSlot() {
	c.slotOnce.Do(func() {
		if c.slots != nil {
			select {
			case <-c.slots:
			default:
			}
		}
	})
}

func (c *cloudDSHAccountCoordinator) markTerminal() {
	c.mu.Lock()
	if c.terminal {
		c.mu.Unlock()
		return
	}
	c.terminal = true
	c.mu.Unlock()
	c.finishOnce.Do(func() { close(c.finished) })
	c.releaseSlot()
}

func (c *cloudDSHAccountCoordinator) fail(err error) {
	if err == nil {
		err = errors.New("DSH coordinator runtime failed")
	}
	c.mu.Lock()
	if c.failure == nil {
		c.failure = err
	}
	c.mu.Unlock()
	c.startOnce.Do(func() { c.startedResult <- err })
	c.markTerminal()
	select {
	case c.events <- cloudDSHRead{err: err}:
	default:
	}
}

func (s *cloudDSHAccountRuntimeSession) removeCoordinator(identity app.ConversationCoordinatorIdentity,
	coordinator *cloudDSHAccountCoordinator) error {
	s.mu.Lock()
	if current := s.coordinators[identity.SessionID]; current == coordinator && current.identity == identity {
		delete(s.coordinators, identity.SessionID)
	}
	s.mu.Unlock()
	coordinator.releaseSlot()
	return nil
}

func (m *CloudDSHAccountRuntimeManager) StopCoordinator(ctx context.Context, session *cloudDSHAccountRuntimeSession,
	identity app.ConversationCoordinatorIdentity) error {
	if ctx == nil || session == nil {
		return errors.New("coordinator stop context and runtime are required")
	}
	session.mu.Lock()
	coordinator := session.coordinators[identity.SessionID]
	session.mu.Unlock()
	if coordinator == nil {
		return nil
	}
	if !coordinator.identityMatches(identity) {
		return errors.New("coordinator stop identity does not match")
	}
	coordinator.mu.Lock()
	terminal := coordinator.terminal
	coordinator.mu.Unlock()
	if !terminal {
		if err := session.writeCoordinatorFrame(ctx, identity, map[string]any{"type": "coordinator_cancel",
			"context_role": "coordinator", "conversation_id": identity.ConversationID, "message_id": identity.MessageID,
			"owner_id": identity.OwnerID, "actor_id": identity.ActorID, "agent_id": identity.AgentID, "generation": identity.Generation,
			"authorization_version": identity.AuthorizationVersion, "session_id": identity.SessionID}); err != nil {
			return err
		}
		select {
		case <-coordinator.finished:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return session.removeCoordinator(identity, coordinator)
}
