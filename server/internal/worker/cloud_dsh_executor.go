package worker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/task"
)

const cloudDSHMaxFrame = 4 << 20
const cloudDSHMaxText = 256 << 10
const cloudDSHEngineVersion = "0.2.0-rc.2"
const cloudDSHProfileVersion = "v1"

// ErrCloudDSHCompletionPending means dsh finished, but output and Task completion
// have not been independently verified and committed.
var ErrCloudDSHCompletionPending = errors.New("dsh completion requires output verification")

var cloudDSHID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// CloudDSHLaunch is passed only to the separately trusted supervisor. Nonce and
// fence must never be placed in the child environment or command line.
type CloudDSHLaunch struct {
	NodePath    string
	ScriptPath  string
	Environment []string
	Fence       app.AgentInstanceFence
	Nonce       []byte
	ExpiresAt   time.Time
}

// CloudDSHSupervisor must isolate the child from Portal credentials and its
// filesystem, enforce ExpiresAt independently of the worker, and supervise the
// entire process tree. Stop must return a signed, verifiable tree-stop receipt.
// No production implementation is supplied by this adapter.
type CloudDSHSupervisor interface {
	Start(context.Context, CloudDSHLaunch) (io.WriteCloser, io.ReadCloser, error)
	Renew(context.Context, app.AgentInstanceFence, time.Time) error
	Stop(context.Context, app.AgentInstanceFence, []byte) (app.AgentStopReceipt, error)
}

type CloudDSHStopFinalizer interface {
	AgentStopConfirmed(app.AgentInstanceFence, []byte)
}

type CloudDSHInstances interface {
	Start(context.Context, app.AgentInstanceStart) (app.AgentInstanceLease, bool, error)
	Renew(context.Context, app.AgentInstanceFence, time.Duration) (app.AgentInstanceLease, error)
	ConfirmStopped(context.Context, app.AgentStopReceipt) (bool, error)
}

type CloudDSHCheckpoints interface {
	Save(context.Context, app.AgentCheckpointCommand) (bool, error)
}

type CloudDSHModels interface {
	Complete(context.Context, app.ModelCall) (app.ModelResponse, error)
}

type CloudDSHExecutorOptions struct {
	NodePath     string
	ScriptPath   string
	SupervisorID string
	Supervisor   CloudDSHSupervisor
	Instances    CloudDSHInstances
	Checkpoints  CloudDSHCheckpoints
	Models       CloudDSHModels
}

type CloudDSHExecutor struct{ options CloudDSHExecutorOptions }

func NewCloudDSHExecutor(options CloudDSHExecutorOptions) (*CloudDSHExecutor, error) {
	if !filepath.IsAbs(options.NodePath) || !filepath.IsAbs(options.ScriptPath) ||
		!cloudDSHID.MatchString(options.SupervisorID) || options.Supervisor == nil ||
		options.Instances == nil || options.Checkpoints == nil || options.Models == nil {
		return nil, errors.New("dsh requires absolute paths and all trusted supervisor and service dependencies")
	}
	return &CloudDSHExecutor{options: options}, nil
}

// ExecuteTask deliberately does not complete or resume a Task. A durable
// supervisor and a separate artifact/completion transaction remain mandatory.
func (e *CloudDSHExecutor) ExecuteTask(ctx context.Context, value task.Task) (retErr error) {
	if ctx == nil || !domain.ValidTaskID(value.ID) || !domain.ValidUserID(value.OwnerID) ||
		!domain.ValidModelConnectionID(value.ModelID) || value.Status != task.StatusRunning || value.ExecutionLocation != "server" ||
		value.Lease.Owner == "" || value.Lease.Version < 1 || value.Lease.ExpiresAt == nil || !value.Lease.ExpiresAt.After(time.Now()) ||
		value.Authorization.Version < 1 || value.Budget.Validate() != nil ||
		value.AgentInstanceID != "" || value.AgentGeneration != 0 || value.AgentSessionID != "" ||
		value.CheckpointThroughSequence != -1 || len(value.CheckpointDigest) != 0 ||
		!utf8.ValidString(value.Objective) || strings.TrimSpace(value.Objective) == "" || len(value.Objective) > cloudDSHMaxText {
		return errors.New("dsh task is invalid or requires unsupported recovery")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	instanceID, err := randomCloudDSHID("agent_")
	if err != nil {
		return err
	}
	sessionID, err := randomCloudDSHID("session_")
	if err != nil {
		return err
	}
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	fence := app.AgentInstanceFence{TaskID: value.ID, OwnerID: value.OwnerID, AgentInstanceID: instanceID,
		AgentSessionID: sessionID, SupervisorID: e.options.SupervisorID, LeaseOwner: value.Lease.Owner,
		LeaseVersion: value.Lease.Version, AuthorizationVersion: value.Authorization.Version,
		EngineVersion: cloudDSHEngineVersion, ProfileVersion: cloudDSHProfileVersion}
	lease, replayed, err := e.options.Instances.Start(ctx, app.AgentInstanceStart{Fence: fence, SupervisorNonce: nonce, LeaseDuration: app.MaxAgentLease})
	if err != nil {
		return err
	}
	fence.AgentGeneration = lease.Fence.AgentGeneration
	if replayed || fence.AgentGeneration < 1 || lease.Fence != fence || !validCloudDSHLease(lease.ExpiresAt) {
		return errors.New("dsh instance lease was not confirmed")
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		receipt, err := e.options.Supervisor.Stop(stopCtx, fence, nonce)
		if err == nil && (receipt.Fence != fence || subtle.ConstantTimeCompare(receipt.SupervisorNonce, nonce) != 1 || !receipt.TreeStopped) {
			err = errors.New("supervisor returned an invalid process-tree stop receipt")
		}
		if err == nil {
			_, err = e.options.Instances.ConfirmStopped(stopCtx, receipt)
			if err == nil {
				if finalizer, ok := e.options.Supervisor.(CloudDSHStopFinalizer); ok {
					finalizer.AgentStopConfirmed(fence, nonce)
				}
			}
		}
		if err != nil {
			if errors.Is(retErr, ErrCloudDSHCompletionPending) {
				retErr = nil
			}
			retErr = errors.Join(retErr, errors.New("dsh process-tree stop is unconfirmed"))
		}
	}()
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(value.Budget.ActiveSeconds)*time.Second)
	defer cancel()
	runCtx, cancelCause := context.WithCancelCause(runCtx)
	defer cancelCause(nil)
	input, output, err := e.options.Supervisor.Start(runCtx, CloudDSHLaunch{
		NodePath: e.options.NodePath, ScriptPath: e.options.ScriptPath,
		Environment: []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "NODE_ENV=production"},
		Fence:       fence, Nonce: append([]byte(nil), nonce...), ExpiresAt: lease.ExpiresAt,
	})
	if err != nil {
		return errors.New("dsh supervisor could not start the process")
	}
	if input != nil {
		defer func() { _ = input.Close() }()
	}
	if output != nil {
		defer func() { _ = output.Close() }()
	}
	if input == nil || output == nil {
		return errors.New("dsh supervisor did not provide private pipes")
	}
	if !lease.ExpiresAt.After(time.Now()) {
		return errors.New("dsh instance lease expired before process startup")
	}
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		e.renewCloudDSHLease(runCtx, cancelCause, fence, lease.ExpiresAt)
	}()
	err = e.runCloudDSH(runCtx, value, fence, input, output)
	cancelCause(context.Canceled)
	<-renewDone
	if errors.Is(err, ErrCloudDSHCompletionPending) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if cause := context.Cause(runCtx); cause != nil && !errors.Is(cause, context.Canceled) {
			return cause
		}
	}
	return err
}

func (e *CloudDSHExecutor) renewCloudDSHLease(ctx context.Context, cancel context.CancelCauseFunc, fence app.AgentInstanceFence, expires time.Time) {
	for {
		delay := time.Until(expires) / 3
		if delay <= 0 {
			cancel(errors.New("dsh instance lease expired"))
			return
		}
		if delay > 5*time.Second {
			delay = 5 * time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		renewCtx, cancelRenew := context.WithTimeout(ctx, 5*time.Second)
		lease, err := e.options.Instances.Renew(renewCtx, fence, app.MaxAgentLease)
		if err == nil && (lease.Fence != fence || !validCloudDSHLease(lease.ExpiresAt)) {
			err = errors.New("invalid dsh instance renewal")
		}
		if err == nil {
			err = e.options.Supervisor.Renew(renewCtx, fence, lease.ExpiresAt)
		}
		cancelRenew()
		if err != nil {
			cancel(errors.New("dsh instance lease renewal failed"))
			return
		}
		expires = lease.ExpiresAt
	}
}

func validCloudDSHLease(expires time.Time) bool {
	remaining := time.Until(expires)
	return remaining > 0 && remaining <= app.MaxAgentLease
}

func randomCloudDSHID(prefix string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw), nil
}

type cloudDSHIdentity struct {
	TaskID               string `json:"task_id"`
	OwnerID              string `json:"owner_id"`
	AgentID              string `json:"agent_id"`
	SessionID            string `json:"session_id"`
	Generation           int64  `json:"generation"`
	AuthorizationVersion int64  `json:"authorization_version"`
}

type cloudDSHFrame struct {
	Type string `json:"type"`
	cloudDSHIdentity
	Version   int             `json:"version,omitempty"`
	Engine    string          `json:"engine,omitempty"`
	EngineVer string          `json:"engine_version,omitempty"`
	ID        string          `json:"id,omitempty"`
	From      *int64          `json:"from_seq,omitempty"`
	Through   *int64          `json:"through_seq,omitempty"`
	Payload   string          `json:"payload,omitempty"`
	SHA256    string          `json:"sha256,omitempty"`
	Request   json.RawMessage `json:"request,omitempty"`
	State     string          `json:"state,omitempty"`
	Code      string          `json:"code,omitempty"`
}

type cloudDSHRead struct {
	frame []byte
	err   error
}

func (e *CloudDSHExecutor) runCloudDSH(ctx context.Context, value task.Task, fence app.AgentInstanceFence, input io.WriteCloser, output io.ReadCloser) error {
	identity := cloudDSHIdentity{TaskID: fence.TaskID, OwnerID: fence.OwnerID, AgentID: fence.AgentInstanceID,
		SessionID: fence.AgentSessionID, Generation: fence.AgentGeneration, AuthorizationVersion: fence.AuthorizationVersion}
	frames := make(chan cloudDSHRead, 1)
	go readCloudDSHFrames(ctx, output, frames)
	ready := false
	through, lastModel := int64(-1), int64(-1)
	checkpointIDs := make(map[string]struct{})
	for {
		var item cloudDSHRead
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case item = <-frames:
		}
		if item.err != nil {
			return errors.New("dsh connection ended before a verified result")
		}
		frame, err := decodeCloudDSHFrame(item.frame)
		if err != nil {
			return err
		}
		if !ready {
			if frame.Type != "ready" || frame.Version != 1 || frame.Engine != "dsh" || frame.EngineVer != cloudDSHEngineVersion {
				return errors.New("dsh startup handshake is invalid")
			}
			maxTokens := value.Budget.ModelTokens
			if maxTokens > 2048 {
				maxTokens = 2048
			}
			if err := writeCloudDSHFrame(ctx, input, map[string]any{"type": "start", "version": 1,
				"task_id": identity.TaskID, "owner_id": identity.OwnerID, "agent_id": identity.AgentID,
				"session_id": identity.SessionID, "generation": identity.Generation, "authorization_version": identity.AuthorizationVersion,
				"model": value.ModelID, "prompt": value.Objective, "max_tokens": maxTokens}); err != nil {
				return err
			}
			ready = true
			continue
		}
		if frame.Type == "failed" {
			return errors.New("dsh reported an unavailable runtime")
		}
		if frame.cloudDSHIdentity != identity {
			return errors.New("dsh frame identity is invalid")
		}
		switch frame.Type {
		case "checkpoint":
			if !cloudDSHID.MatchString(frame.ID) || frame.From == nil || frame.Through == nil ||
				*frame.From != through+1 || *frame.Through < *frame.From || *frame.Through > 1<<53-1 ||
				len(frame.Payload) == 0 || len(frame.Payload) > 1<<20 || !utf8.ValidString(frame.Payload) ||
				!uniqueCloudDSHJSON([]byte(frame.Payload)) || !cloudDSHDigest([]byte(frame.Payload), frame.SHA256) {
				return errors.New("dsh checkpoint is invalid")
			}
			if _, exists := checkpointIDs[frame.ID]; exists || len(checkpointIDs) >= 4096 {
				return errors.New("dsh checkpoint identity is repeated or exceeds the limit")
			}
			checkpointIDs[frame.ID] = struct{}{}
			digest, _ := hex.DecodeString(frame.SHA256)
			_, err := e.options.Checkpoints.Save(ctx, app.AgentCheckpointCommand{Fence: app.AgentCheckpointFence{
				TaskID: fence.TaskID, OwnerID: fence.OwnerID, AgentInstanceID: fence.AgentInstanceID,
				AgentGeneration: fence.AgentGeneration, AgentSessionID: fence.AgentSessionID, AuthorizationVersion: fence.AuthorizationVersion,
				LeaseOwner: fence.LeaseOwner, LeaseVersion: fence.LeaseVersion, EngineVersion: fence.EngineVersion, ProfileVersion: fence.ProfileVersion,
			}, From: *frame.From, Through: *frame.Through, Digest: digest, Payload: []byte(frame.Payload)})
			if err != nil {
				return errors.New("dsh checkpoint was not durably confirmed")
			}
			through = *frame.Through
			if err := writeCloudDSHFrame(ctx, input, map[string]any{"type": "checkpoint_ack", "id": frame.ID,
				"ok": true, "through_seq": through, "sha256": frame.SHA256}); err != nil {
				return err
			}
		case "model":
			if frame.Through == nil || *frame.Through != through || through <= lastModel || !cloudDSHDigest(frame.Request, frame.SHA256) ||
				!validCloudDSHModelRequest(frame.Request, value.ModelID) {
				return errors.New("dsh model request is invalid")
			}
			callID, err := app.DSHModelCallID(value.ID, identity.SessionID, through)
			if err != nil || frame.ID != callID {
				return errors.New("dsh model identity is invalid")
			}
			lastModel = through
			response, err := e.options.Models.Complete(ctx, app.ModelCall{Source: app.ModelCallTask,
				Task: &app.ModelTaskFence{TaskID: fence.TaskID, OwnerID: fence.OwnerID, LeaseOwner: fence.LeaseOwner,
					LeaseVersion: fence.LeaseVersion, AgentInstanceID: fence.AgentInstanceID, AgentGeneration: fence.AgentGeneration,
					AgentSessionID: fence.AgentSessionID, AuthorizationVersion: fence.AuthorizationVersion, ThroughSequence: through,
					EngineVersion: fence.EngineVersion, ProfileVersion: fence.ProfileVersion},
				ModelID: value.ModelID, CallID: frame.ID, RawRequest: append(json.RawMessage(nil), frame.Request...)})
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			if err != nil || !response.Usage.Known || len(response.ToolCalls) != 0 || response.StopReason != "stop" ||
				!utf8.ValidString(response.Text) || strings.TrimSpace(response.Text) == "" || len(response.Text) > cloudDSHMaxText {
				return errors.New("dsh model result is unavailable or requires an unsupported tool")
			}
			if err := writeCloudDSHModel(ctx, input, frame.ID, response); err != nil {
				return err
			}
		case "settled":
			if frame.Through == nil || *frame.Through != through || through < 0 || lastModel < 0 || frame.State != "completed_candidate" {
				return errors.New("dsh did not produce a completed candidate")
			}
			return ErrCloudDSHCompletionPending
		default:
			return errors.New("unsupported dsh frame")
		}
	}
}

func readCloudDSHFrames(ctx context.Context, source io.Reader, out chan<- cloudDSHRead) {
	reader := bufio.NewReader(source)
	for {
		var line []byte
		for {
			part, err := reader.ReadSlice('\n')
			if len(line)+len(part) > cloudDSHMaxFrame {
				select {
				case out <- cloudDSHRead{err: errors.New("frame too large")}:
				case <-ctx.Done():
				}
				return
			}
			line = append(line, part...)
			if errors.Is(err, bufio.ErrBufferFull) {
				continue
			}
			if err != nil {
				select {
				case out <- cloudDSHRead{err: err}:
				case <-ctx.Done():
				}
				return
			}
			break
		}
		select {
		case out <- cloudDSHRead{frame: line[:len(line)-1]}:
		case <-ctx.Done():
			return
		}
	}
}

func decodeCloudDSHFrame(raw []byte) (cloudDSHFrame, error) {
	var frame cloudDSHFrame
	if !utf8.Valid(raw) || !uniqueCloudDSHJSON(raw) {
		return frame, errors.New("invalid dsh JSON frame")
	}
	var keys map[string]json.RawMessage
	if json.Unmarshal(raw, &keys) != nil || keys == nil {
		return frame, errors.New("invalid dsh JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&frame) != nil || decoder.Decode(new(any)) != io.EOF {
		return cloudDSHFrame{}, errors.New("invalid dsh frame fields")
	}
	fields := map[string][]string{
		"ready":        {"type", "version", "engine", "engine_version"},
		"checkpoint":   {"type", "id", "task_id", "owner_id", "agent_id", "generation", "authorization_version", "session_id", "from_seq", "through_seq", "payload", "sha256"},
		"model":        {"type", "id", "task_id", "owner_id", "agent_id", "generation", "authorization_version", "session_id", "through_seq", "request", "sha256"},
		"model_cancel": {"type", "id", "task_id", "owner_id", "agent_id", "generation", "authorization_version", "session_id"},
		"settled":      {"type", "task_id", "owner_id", "agent_id", "generation", "authorization_version", "session_id", "state", "through_seq"},
		"failed":       {"type", "code"},
	}
	required, ok := fields[frame.Type]
	if !ok || len(keys) != len(required) {
		return cloudDSHFrame{}, errors.New("unsupported dsh frame fields")
	}
	for _, name := range required {
		if _, ok := keys[name]; !ok {
			return cloudDSHFrame{}, errors.New("missing dsh frame field")
		}
	}
	return frame, nil
}

// Reject duplicate keys at every depth before either side interprets a frame.
func uniqueCloudDSHJSON(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var walk func(int) bool
	walk = func(depth int) bool {
		if depth > 64 {
			return false
		}
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return true
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok {
					return false
				}
				if _, exists := seen[name]; exists {
					return false
				}
				seen[name] = struct{}{}
				if !walk(depth + 1) {
					return false
				}
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			for decoder.More() {
				if !walk(depth + 1) {
					return false
				}
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim(']')
		default:
			return false
		}
	}
	return walk(0) && decoder.Decode(new(any)) == io.EOF
}

func cloudDSHDigest(data []byte, encoded string) bool {
	want, err := hex.DecodeString(encoded)
	if err != nil || len(encoded) != 64 || encoded != strings.ToLower(encoded) {
		return false
	}
	sum := sha256.Sum256(data)
	return subtle.ConstantTimeCompare(sum[:], want) == 1
}

func validCloudDSHModelRequest(raw []byte, modelID string) bool {
	var request struct {
		Provider string            `json:"provider"`
		Model    string            `json:"model"`
		Tools    []json.RawMessage `json:"tools"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"messages"`
	}
	if !uniqueCloudDSHJSON(raw) || json.Unmarshal(raw, &request) != nil || request.Provider != "morde" ||
		request.Model != modelID || request.Tools == nil || len(request.Tools) != 0 || len(request.Messages) == 0 {
		return false
	}
	for _, message := range request.Messages {
		if message.Role != "system" && message.Role != "developer" && message.Role != "user" && message.Role != "assistant" {
			return false
		}
		for _, block := range message.Content {
			if block.Type != "text" {
				return false
			}
		}
	}
	return true
}

func writeCloudDSHFrame(ctx context.Context, target io.WriteCloser, frame any) error {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(frame); err != nil || buffer.Len() > cloudDSHMaxFrame {
		return errors.New("dsh output frame is invalid")
	}
	encoded := buffer.Bytes()
	done := make(chan error, 1)
	go func() {
		for len(encoded) > 0 {
			n, err := target.Write(encoded)
			if err != nil {
				done <- err
				return
			}
			if n < 1 || n > len(encoded) {
				done <- io.ErrShortWrite
				return
			}
			encoded = encoded[n:]
		}
		done <- nil
	}()
	select {
	case <-ctx.Done():
		_ = target.Close()
		return context.Cause(ctx)
	case err := <-done:
		if err != nil {
			return errors.New("dsh output connection failed")
		}
		return nil
	}
}

func writeCloudDSHModel(ctx context.Context, target io.WriteCloser, id string, response app.ModelResponse) error {
	if response.Usage.InputTokens < 0 || response.Usage.OutputTokens < 0 ||
		response.Usage.InputTokens > 1<<53-1 || response.Usage.OutputTokens > 1<<53-1 {
		return errors.New("dsh model usage is invalid")
	}
	chunks := []any{
		map[string]any{"type": "block-start", "index": 0, "blockType": "text"},
		map[string]any{"type": "text-delta", "index": 0, "text": response.Text},
		map[string]any{"type": "block-end", "index": 0, "block": map[string]any{"type": "text", "text": response.Text}},
		map[string]any{"type": "usage", "usage": map[string]any{"inputTokens": response.Usage.InputTokens, "outputTokens": response.Usage.OutputTokens}},
		map[string]any{"type": "finish", "reason": map[string]any{"kind": "stop"}},
	}
	for index, chunk := range chunks {
		if err := writeCloudDSHFrame(ctx, target, map[string]any{"type": "model_chunk", "id": id, "index": index, "chunk": chunk}); err != nil {
			return err
		}
	}
	return writeCloudDSHFrame(ctx, target, map[string]any{"type": "model_end", "id": id, "ok": true})
}

var _ TaskExecutor = (*CloudDSHExecutor)(nil)
