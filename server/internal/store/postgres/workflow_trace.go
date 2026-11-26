package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/evaluation"
)

const workflowTraceFactLimit = 16

type workflowTraceBuilder struct {
	trace        evaluation.WorkflowTrace
	sources      map[string]evaluation.WorkflowSource
	coverage     map[string]struct{}
	nodes        map[string]int
	edges        map[string]struct{}
	gaps         map[string]struct{}
	agentNodes   map[string]string
	routeNodes   map[string]string
	modelNodes   map[string]string
	modelSources map[string]string
	operations   []workflowTraceOperationRecord
}

type workflowTraceOperationRecord struct {
	nodeID, sourceID, approvalID, approvalHash, callID, toolName string
	executionKind, executionID, requestHash, connectorID         string
	taskRevision                                                 int64
	approvalVersion                                              int64
}

type workflowTraceDeliveryObservationTarget struct {
	sourceID     string
	taskRevision int64
	pcEligible   bool
}

func newWorkflowTraceBuilder(taskID string, revision, eventSequence int64, capturedAt time.Time) *workflowTraceBuilder {
	traceID := workflowTraceID(taskID, revision, eventSequence)
	return &workflowTraceBuilder{trace: evaluation.WorkflowTrace{Version: 1, TraceID: traceID,
		DataClass: evaluation.WorkflowDataLiveObserved, CapturedAt: capturedAt.UTC(), Coverage: evaluation.WorkflowCoverageUnknown,
		CoverageSourceIDs: make([]string, 0), Sources: make([]evaluation.WorkflowSource, 0), Nodes: make([]evaluation.WorkflowNode, 0),
		Edges: make([]evaluation.WorkflowEdge, 0), Gaps: make([]evaluation.WorkflowGap, 0)},
		sources: make(map[string]evaluation.WorkflowSource), coverage: make(map[string]struct{}), nodes: make(map[string]int),
		edges: make(map[string]struct{}), gaps: make(map[string]struct{}),
		agentNodes: make(map[string]string), routeNodes: make(map[string]string), modelNodes: make(map[string]string), modelSources: make(map[string]string)}
}

func (b *workflowTraceBuilder) addSource(kind evaluation.WorkflowSourceKind, locator string, fact any) (string, error) {
	encoded, err := json.Marshal(fact)
	if err != nil {
		return "", fmt.Errorf("encode workflow source fact: %w", err)
	}
	digest := sha256.Sum256(encoded)
	sha := hex.EncodeToString(digest[:])
	identity := string(kind) + "\n" + locator + "\n" + sha
	identityDigest := sha256.Sum256([]byte(identity))
	id := "src_" + hex.EncodeToString(identityDigest[:16])
	source := evaluation.WorkflowSource{ID: id, Kind: kind, Locator: locator, SHA256: sha}
	if prior, exists := b.sources[id]; exists && prior != source {
		return "", errors.New("workflow source identity collision")
	}
	b.sources[id] = source
	b.coverage[id] = struct{}{}
	return id, nil
}

func (b *workflowTraceBuilder) addNode(kind evaluation.WorkflowNodeKind, state evaluation.WorkflowNodeState, key string, sourceIDs ...string) string {
	id := workflowStableID("node_"+string(kind), key)
	if index, exists := b.nodes[id]; exists {
		node := &b.trace.Nodes[index]
		node.SourceIDs = uniqueWorkflowIDs(append(node.SourceIDs, sourceIDs...))
		return id
	} else {
		b.nodes[id] = len(b.trace.Nodes)
		b.trace.Nodes = append(b.trace.Nodes, evaluation.WorkflowNode{ID: id, Kind: kind, State: state,
			SourceIDs: uniqueWorkflowIDs(sourceIDs)})
	}
	return id
}

func (b *workflowTraceBuilder) addEdge(kind evaluation.WorkflowEdgeKind, from, to string, sourceIDs ...string) {
	id := workflowStableID("edge_"+string(kind), from+"\n"+to+"\n"+joinWorkflowIDs(uniqueWorkflowIDs(sourceIDs)))
	if _, exists := b.edges[id]; exists {
		return
	}
	b.edges[id] = struct{}{}
	b.trace.Edges = append(b.trace.Edges, evaluation.WorkflowEdge{ID: id, From: from, To: to, Kind: kind,
		SourceIDs: uniqueWorkflowIDs(sourceIDs)})
}

func (b *workflowTraceBuilder) addGap(stage evaluation.WorkflowStage, scope string, state evaluation.WorkflowGapState,
	count *int, sourceIDs ...string) {
	ids := uniqueWorkflowIDs(sourceIDs)
	key := string(stage) + "\n" + scope + "\n" + string(state) + "\n" + joinWorkflowIDs(ids)
	id := workflowStableID("gap", key)
	if _, exists := b.gaps[id]; exists {
		return
	}
	b.gaps[id] = struct{}{}
	b.trace.Gaps = append(b.trace.Gaps, evaluation.WorkflowGap{ID: id, Stage: stage, Scope: scope, State: state,
		Count: count, SourceIDs: ids})
}

func (b *workflowTraceBuilder) addCountSource(kind evaluation.WorkflowSourceKind, locator string, count int64) (string, error) {
	return b.addSource(kind, locator, struct {
		RowCount int64 `json:"row_count"`
	}{RowCount: count})
}

func (b *workflowTraceBuilder) finish() (evaluation.WorkflowTrace, error) {
	for id := range b.coverage {
		b.trace.CoverageSourceIDs = append(b.trace.CoverageSourceIDs, id)
	}
	for _, source := range b.sources {
		b.trace.Sources = append(b.trace.Sources, source)
	}
	sort.Strings(b.trace.CoverageSourceIDs)
	sort.Slice(b.trace.Sources, func(i, j int) bool { return b.trace.Sources[i].ID < b.trace.Sources[j].ID })
	sort.Slice(b.trace.Nodes, func(i, j int) bool { return b.trace.Nodes[i].ID < b.trace.Nodes[j].ID })
	sort.Slice(b.trace.Edges, func(i, j int) bool { return b.trace.Edges[i].ID < b.trace.Edges[j].ID })
	sort.Slice(b.trace.Gaps, func(i, j int) bool { return b.trace.Gaps[i].ID < b.trace.Gaps[j].ID })
	if err := evaluation.ValidateWorkflowTrace(b.trace); err != nil {
		return evaluation.WorkflowTrace{}, fmt.Errorf("validate ledger-backed workflow trace: %w", err)
	}
	return b.trace, nil
}

func workflowStableID(prefix, key string) string {
	digest := sha256.Sum256([]byte(prefix + "\n" + key))
	return prefix + "_" + hex.EncodeToString(digest[:16])
}

func workflowTraceID(taskID string, revision, sequence int64) string {
	return workflowStableID("trace", fmt.Sprintf("%s:%d:%d", taskID, revision, sequence))
}

func uniqueWorkflowIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

func joinWorkflowIDs(ids []string) string {
	result := ""
	for index, id := range ids {
		if index > 0 {
			result += ","
		}
		result += id
	}
	return result
}

type workflowTraceTask struct {
	ID                   string
	OwnerID              string
	ConversationID       string
	HostID               string
	Status               string
	CurrentRevision      int64
	DesiredRevision      int64
	EventSequence        int64
	AgentGeneration      int64
	AgentSessionID       string
	AuthorizationKind    string
	AuthorizationTokenID string
	AuthorizationScopes  []string
	AuthorizationVersion int64
	ModelID              string
	Objective            string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// GetTaskWorkflowTrace takes one read-only database snapshot and builds a
// source-bound graph from the durable Task ledgers. It intentionally emits
// gaps for unrecorded PC observations and never computes a quality verdict.
func (s *Store) GetTaskWorkflowTrace(ctx context.Context, taskID, ownerID string) (evaluation.WorkflowTrace, error) {
	if !domain.ValidTaskID(taskID) || !domain.ValidUserID(ownerID) {
		return evaluation.WorkflowTrace{}, domain.NewError(domain.CodeNotFound, "task not found")
	}
	var trace evaluation.WorkflowTrace
	err := s.withTaskSnapshot(ctx, func(tx pgx.Tx) error {
		var taskRow workflowTraceTask
		err := tx.QueryRow(ctx, `SELECT id,owner_id,conversation_id,COALESCE(host_id,''),status,current_revision,desired_revision,event_seq,
agent_generation,COALESCE(agent_session_id,''),auth_kind,auth_token_id,auth_scopes,auth_version,model_id,objective,created_at,updated_at
FROM tasks WHERE id=$1 AND owner_id=$2`, taskID, ownerID).Scan(
			&taskRow.ID, &taskRow.OwnerID, &taskRow.ConversationID, &taskRow.HostID, &taskRow.Status, &taskRow.CurrentRevision,
			&taskRow.DesiredRevision, &taskRow.EventSequence, &taskRow.AgentGeneration, &taskRow.AgentSessionID,
			&taskRow.AuthorizationKind, &taskRow.AuthorizationTokenID, &taskRow.AuthorizationScopes,
			&taskRow.AuthorizationVersion, &taskRow.ModelID, &taskRow.Objective, &taskRow.CreatedAt, &taskRow.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return missingTaskError()
		}
		if err != nil {
			return fmt.Errorf("read workflow Task ledger: %w", err)
		}
		builder := newWorkflowTraceBuilder(taskRow.ID, taskRow.CurrentRevision, taskRow.EventSequence, time.Now())
		objectiveDigest := sha256.Sum256([]byte(taskRow.Objective))
		taskSource, err := builder.addSource(evaluation.WorkflowSourceTaskLedger, "db://tasks/"+taskRow.ID, struct {
			TaskID               string    `json:"task_id"`
			OwnerID              string    `json:"owner_id"`
			ConversationID       string    `json:"conversation_id"`
			HostID               string    `json:"host_id,omitempty"`
			Status               string    `json:"status"`
			CurrentRevision      int64     `json:"current_revision"`
			DesiredRevision      int64     `json:"desired_revision"`
			EventSequence        int64     `json:"event_sequence"`
			AuthorizationKind    string    `json:"authorization_kind"`
			AuthorizationTokenID string    `json:"authorization_token_id,omitempty"`
			AuthorizationScopes  []string  `json:"authorization_scopes"`
			AuthorizationVersion int64     `json:"authorization_version"`
			ModelID              string    `json:"model_id"`
			ObjectiveBytes       int       `json:"objective_bytes"`
			ObjectiveSHA256      string    `json:"objective_sha256"`
			CreatedAt            time.Time `json:"created_at"`
			UpdatedAt            time.Time `json:"updated_at"`
		}{taskRow.ID, taskRow.OwnerID, taskRow.ConversationID, taskRow.HostID, taskRow.Status,
			taskRow.CurrentRevision, taskRow.DesiredRevision, taskRow.EventSequence, taskRow.AuthorizationKind, taskRow.AuthorizationTokenID,
			taskRow.AuthorizationScopes, taskRow.AuthorizationVersion, taskRow.ModelID, len([]byte(taskRow.Objective)), hex.EncodeToString(objectiveDigest[:]), taskRow.CreatedAt, taskRow.UpdatedAt})
		if err != nil {
			return err
		}
		taskNode := builder.addNode(evaluation.WorkflowNodeTask, workflowTaskNodeState(taskRow.Status), taskRow.ID, taskSource)
		authSource, err := builder.addSource(evaluation.WorkflowSourceTaskLedger, "db://tasks/"+taskRow.ID+"/authorization", struct {
			AuthorizationKind    string   `json:"authorization_kind"`
			AuthorizationTokenID string   `json:"authorization_token_id,omitempty"`
			Scopes               []string `json:"scopes"`
			Version              int64    `json:"version"`
		}{taskRow.AuthorizationKind, taskRow.AuthorizationTokenID, taskRow.AuthorizationScopes, taskRow.AuthorizationVersion})
		if err != nil {
			return err
		}
		authNode := builder.addNode(evaluation.WorkflowNodeAuth, evaluation.WorkflowNodeSettled,
			fmt.Sprintf("task:%s:auth:%d", taskRow.ID, taskRow.AuthorizationVersion), authSource)
		builder.addEdge(evaluation.WorkflowEdgeAuthorized, taskNode, authNode, authSource)
		if err := addWorkflowTraceRoutes(ctx, tx, builder, taskRow, taskNode, taskSource); err != nil {
			return err
		}
		if err := addWorkflowTraceAgents(ctx, tx, builder, taskRow, taskNode, taskSource); err != nil {
			return err
		}
		if err := addWorkflowTraceModels(ctx, tx, builder, taskRow, taskNode); err != nil {
			return err
		}
		if err := addWorkflowTraceOperations(ctx, tx, builder, taskRow, taskNode); err != nil {
			return err
		}
		if err := addWorkflowTraceApprovals(ctx, tx, builder, taskRow, taskNode); err != nil {
			return err
		}
		if err := addWorkflowTraceValidation(ctx, tx, builder, taskRow, taskNode); err != nil {
			return err
		}
		if err := addWorkflowTraceDeliveries(ctx, tx, builder, taskRow, taskNode); err != nil {
			return err
		}
		if err := addWorkflowTraceHost(ctx, tx, builder, taskRow, taskNode, taskSource); err != nil {
			return err
		}
		if err := addWorkflowTraceRuntime(ctx, tx, builder, taskRow, taskNode, taskSource); err != nil {
			return err
		}
		if err := addWorkflowTraceDSHSessionRuntimeSnapshot(ctx, tx, builder, taskRow, taskNode, taskSource); err != nil {
			return err
		}
		if err := addWorkflowTraceCurrentAuthorization(ctx, tx, builder, taskRow); err != nil {
			return err
		}
		trace, err = builder.finish()
		return err
	})
	return trace, err
}

func workflowTaskNodeState(status string) evaluation.WorkflowNodeState {
	switch status {
	case "queued":
		return evaluation.WorkflowNodePending
	case "running":
		return evaluation.WorkflowNodeRunning
	case "waiting_user":
		return evaluation.WorkflowNodeWaitingUser
	case "pausing", "paused", "cancelling":
		return evaluation.WorkflowNodeWaiting
	case "completed":
		return evaluation.WorkflowNodeCompleted
	case "failed":
		return evaluation.WorkflowNodeFailed
	case "cancelled":
		return evaluation.WorkflowNodeCancelled
	default:
		return evaluation.WorkflowNodeUnknown
	}
}

func workflowCallNodeState(state string) evaluation.WorkflowNodeState {
	switch state {
	case "settled":
		return evaluation.WorkflowNodeSettled
	case "dispatched", "reserved":
		return evaluation.WorkflowNodeUnknown
	case "unknown":
		return evaluation.WorkflowNodeUnknown
	default:
		return evaluation.WorkflowNodeUnavailable
	}
}

func workflowOperationNodeState(state string) evaluation.WorkflowNodeState {
	switch state {
	case "prepared":
		return evaluation.WorkflowNodePending
	case "dispatched":
		return evaluation.WorkflowNodeUnknown
	case "succeeded":
		return evaluation.WorkflowNodeCompleted
	case "failed":
		return evaluation.WorkflowNodeFailed
	case "cancelled":
		return evaluation.WorkflowNodeCancelled
	case "unknown":
		return evaluation.WorkflowNodeUnknown
	default:
		return evaluation.WorkflowNodeUnavailable
	}
}

func workflowTruncatedGap(builder *workflowTraceBuilder, stage evaluation.WorkflowStage, scope string,
	count int64, sourceID string) {
	if count <= workflowTraceFactLimit {
		return
	}
	builder.addGap(stage, scope+": capped at 16 records", evaluation.WorkflowGapUnknown, nil, sourceID)
}

func addWorkflowTraceRoutes(ctx context.Context, tx pgx.Tx, builder *workflowTraceBuilder, taskRow workflowTraceTask,
	taskNode, taskSource string) error {
	var count int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM conversation_message_routes r
JOIN conversation_messages m ON m.id=r.message_id AND m.owner_id=r.owner_id AND m.conversation_id=r.conversation_id
WHERE m.task_id=$1 AND m.owner_id=$2 AND m.conversation_id=$3 AND m.role='user'`,
		taskRow.ID, taskRow.OwnerID, taskRow.ConversationID).Scan(&count); err != nil {
		return fmt.Errorf("count workflow route ledgers: %w", err)
	}
	countSource, err := builder.addCountSource(evaluation.WorkflowSourceRouteResult,
		"db://conversation_message_routes/task/"+taskRow.ID+"/count", count)
	if err != nil {
		return err
	}
	if count == 0 {
		zero := 0
		builder.addGap(evaluation.WorkflowStageRoute, "user-message route records", evaluation.WorkflowGapAbsent, &zero, countSource)
	}
	workflowTruncatedGap(builder, evaluation.WorkflowStageRoute, "user-message route records", count, countSource)
	rows, err := tx.Query(ctx, `SELECT r.message_id,r.actor_id,r.state,r.failure_reason,r.request_hash,
r.authorization_snapshot,r.created_at,r.updated_at,m.sequence,m.delivery_status,COALESCE(r.authorization_snapshot->>'Kind',''),
CASE WHEN COALESCE(r.authorization_snapshot->>'Version','') ~ '^[0-9]+$' THEN (r.authorization_snapshot->>'Version')::bigint ELSE 0 END
FROM conversation_message_routes r
JOIN conversation_messages m ON m.id=r.message_id AND m.owner_id=r.owner_id AND m.conversation_id=r.conversation_id
WHERE m.task_id=$1 AND m.owner_id=$2 AND m.conversation_id=$3 AND m.role='user'
ORDER BY m.sequence,r.message_id LIMIT $4`, taskRow.ID, taskRow.OwnerID, taskRow.ConversationID, workflowTraceFactLimit)
	if err != nil {
		return fmt.Errorf("read workflow route ledgers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var messageID, actorID, state, failureReason, deliveryStatus, authorizationKind string
		var requestHash, authorizationSnapshot []byte
		var createdAt, updatedAt time.Time
		var sequence, authorizationVersion int64
		if err := rows.Scan(&messageID, &actorID, &state, &failureReason, &requestHash, &authorizationSnapshot,
			&createdAt, &updatedAt, &sequence, &deliveryStatus, &authorizationKind, &authorizationVersion); err != nil {
			return err
		}
		authorizationDigest := sha256.Sum256(authorizationSnapshot)
		failureDigest := sha256.Sum256([]byte(failureReason))
		sourceID, err := builder.addSource(evaluation.WorkflowSourceRouteResult, "db://conversation_message_routes/message/"+messageID,
			struct {
				MessageID                   string    `json:"message_id"`
				ActorID                     string    `json:"actor_id"`
				State                       string    `json:"state"`
				FailureReasonSHA256         string    `json:"failure_reason_sha256"`
				RequestHash                 string    `json:"request_hash"`
				AuthorizationSnapshotSHA256 string    `json:"authorization_snapshot_sha256"`
				AuthorizationKind           string    `json:"authorization_kind,omitempty"`
				AuthorizationVersion        int64     `json:"authorization_version,omitempty"`
				Sequence                    int64     `json:"sequence"`
				DeliveryStatus              string    `json:"delivery_status"`
				CreatedAt                   time.Time `json:"created_at"`
				UpdatedAt                   time.Time `json:"updated_at"`
			}{messageID, actorID, state, hex.EncodeToString(failureDigest[:]), hex.EncodeToString(requestHash), hex.EncodeToString(authorizationDigest[:]),
				authorizationKind, authorizationVersion, sequence, deliveryStatus, createdAt, updatedAt})
		if err != nil {
			return err
		}
		routeNode := builder.addNode(evaluation.WorkflowNodeRoute, workflowRouteNodeState(state), messageID, sourceID)
		builder.routeNodes[messageID] = routeNode
		if authorizationVersion > 0 {
			authNode := builder.addNode(evaluation.WorkflowNodeAuth, evaluation.WorkflowNodeSettled,
				fmt.Sprintf("route:%s:auth:%d", messageID, authorizationVersion), sourceID)
			builder.addEdge(evaluation.WorkflowEdgeAuthorized, routeNode, authNode, sourceID)
		} else {
			builder.addGap(evaluation.WorkflowStageAuthorization, "route authorization version is missing: "+messageID,
				evaluation.WorkflowGapUnknown, nil, sourceID)
		}
		if state == "routed" {
			builder.addEdge(evaluation.WorkflowEdgeRouted, routeNode, taskNode, sourceID, taskSource)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate workflow route ledgers: %w", err)
	}
	return nil
}

func workflowRouteNodeState(state string) evaluation.WorkflowNodeState {
	switch state {
	case "routed":
		return evaluation.WorkflowNodeCompleted
	case "waiting_user":
		return evaluation.WorkflowNodeWaitingUser
	case "failed":
		return evaluation.WorkflowNodeFailed
	case "pending":
		return evaluation.WorkflowNodePending
	case "processing":
		return evaluation.WorkflowNodeRunning
	default:
		return evaluation.WorkflowNodeUnknown
	}
}

func addWorkflowTraceAgents(ctx context.Context, tx pgx.Tx, builder *workflowTraceBuilder, taskRow workflowTraceTask,
	taskNode, taskSource string) error {
	var count int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM task_agent_sessions WHERE task_id=$1 AND owner_id=$2`, taskRow.ID, taskRow.OwnerID).Scan(&count); err != nil {
		return fmt.Errorf("count workflow Agent ledgers: %w", err)
	}
	countSource, err := builder.addCountSource(evaluation.WorkflowSourceAgentLedger, "db://task_agent_sessions/task/"+taskRow.ID+"/count", count)
	if err != nil {
		return err
	}
	if count == 0 {
		zero := 0
		builder.addGap(evaluation.WorkflowStageAgent, "Agent session records", evaluation.WorkflowGapAbsent, &zero, countSource)
	}
	workflowTruncatedGap(builder, evaluation.WorkflowStageAgent, "Agent session records", count, countSource)
	rows, err := tx.Query(ctx, `SELECT s.session_id,COALESCE(s.parent_session_id,''),COALESCE(s.step_id,''),COALESCE(s.spawn_key,''),
COALESCE(encode(s.request_hash,'hex'),''),s.task_revision,COALESCE(s.activity_state,''),COALESCE(s.activity_phase,''),
COALESCE(s.activity_generation,0),COALESCE(s.activity_sequence,0),COALESCE(s.activity_id,''),COALESCE(s.allowed_tools::text,''),
COALESCE(st.status,''),COALESCE(st.task_revision,-1),s.created_at
FROM task_agent_sessions s LEFT JOIN task_steps st ON st.task_id=s.task_id AND st.id=s.step_id
WHERE s.task_id=$1 AND s.owner_id=$2 ORDER BY s.created_at,s.session_id LIMIT $3`, taskRow.ID, taskRow.OwnerID, workflowTraceFactLimit)
	if err != nil {
		return fmt.Errorf("read workflow Agent ledgers: %w", err)
	}
	type agentRecord struct {
		sessionID, parentID, stepID, spawnKey, requestHash, activityState, activityPhase, activityID, allowedTools, stepStatus string
		revision, activityGeneration, activitySequence, stepRevision                                                           int64
		createdAt                                                                                                              time.Time
		nodeID, sourceID                                                                                                       string
	}
	agents := make([]agentRecord, 0)
	for rows.Next() {
		var record agentRecord
		if err := rows.Scan(&record.sessionID, &record.parentID, &record.stepID, &record.spawnKey, &record.requestHash,
			&record.revision, &record.activityState, &record.activityPhase, &record.activityGeneration, &record.activitySequence,
			&record.activityID, &record.allowedTools, &record.stepStatus, &record.stepRevision, &record.createdAt); err != nil {
			rows.Close()
			return err
		}
		allowedToolsDigest := sha256.Sum256([]byte(record.allowedTools))
		record.sourceID, err = builder.addSource(evaluation.WorkflowSourceAgentLedger,
			"db://task_agent_sessions/"+taskRow.ID+"/"+record.sessionID, struct {
				SessionID          string    `json:"session_id"`
				ParentSessionID    string    `json:"parent_session_id,omitempty"`
				StepID             string    `json:"step_id,omitempty"`
				SpawnKey           string    `json:"spawn_key,omitempty"`
				RequestHash        string    `json:"request_hash,omitempty"`
				TaskRevision       int64     `json:"task_revision"`
				ActivityState      string    `json:"activity_state,omitempty"`
				ActivityPhase      string    `json:"activity_phase,omitempty"`
				ActivityID         string    `json:"activity_id,omitempty"`
				ActivityGeneration int64     `json:"activity_generation,omitempty"`
				ActivitySequence   int64     `json:"activity_sequence,omitempty"`
				AllowedToolsSHA256 string    `json:"allowed_tools_sha256"`
				StepStatus         string    `json:"step_status,omitempty"`
				StepRevision       int64     `json:"step_revision"`
				CreatedAt          time.Time `json:"created_at"`
			}{record.sessionID, record.parentID, record.stepID, record.spawnKey, record.requestHash, record.revision,
				record.activityState, record.activityPhase, record.activityID, record.activityGeneration, record.activitySequence,
				hex.EncodeToString(allowedToolsDigest[:]), record.stepStatus, record.stepRevision, record.createdAt})
		if err != nil {
			rows.Close()
			return err
		}
		record.nodeID = builder.addNode(evaluation.WorkflowNodeAgent, workflowAgentNodeState(record.activityState, record.stepStatus),
			taskRow.ID+":"+record.sessionID, record.sourceID, taskSource)
		builder.addGap(evaluation.WorkflowStageAuthorization, "Agent session has no persisted exact authorization version: "+record.sessionID,
			evaluation.WorkflowGapUnknown, nil, record.sourceID)
		agents = append(agents, record)
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return fmt.Errorf("iterate workflow Agent ledgers: %w", rowsErr)
	}
	bySession := make(map[string]agentRecord, len(agents))
	builder.agentNodes = make(map[string]string, len(agents))
	for _, record := range agents {
		bySession[record.sessionID] = record
	}
	for _, record := range agents {
		builder.agentNodes[record.sessionID] = record.nodeID
	}
	rootCount := 0
	for _, record := range agents {
		if record.parentID == "" {
			rootCount++
			builder.addEdge(evaluation.WorkflowEdgeCreated, taskNode, record.nodeID, record.sourceID, taskSource)
			continue
		}
		parent, exists := bySession[record.parentID]
		if !exists {
			builder.addGap(evaluation.WorkflowStageAgent, "parent session outside captured Agent ledger window", evaluation.WorkflowGapUnknown, nil, record.sourceID, countSource)
			continue
		}
		builder.addEdge(evaluation.WorkflowEdgeHandoff, parent.nodeID, record.nodeID, record.sourceID)
	}
	if count > 0 && rootCount == 0 {
		builder.addGap(evaluation.WorkflowStageAgent, "root Agent session missing from captured ledger", evaluation.WorkflowGapUnknown, nil, countSource)
	}
	return nil
}

func workflowAgentNodeState(activity, step string) evaluation.WorkflowNodeState {
	switch activity {
	case "started":
		return evaluation.WorkflowNodeStarted
	case "waiting":
		return evaluation.WorkflowNodeWaiting
	case "completed":
		return evaluation.WorkflowNodeCompleted
	case "failed":
		return evaluation.WorkflowNodeFailed
	case "cancelled":
		return evaluation.WorkflowNodeCancelled
	}
	switch step {
	case "queued":
		return evaluation.WorkflowNodePending
	case "running":
		return evaluation.WorkflowNodeRunning
	case "waiting_user":
		return evaluation.WorkflowNodeWaitingUser
	case "succeeded":
		return evaluation.WorkflowNodeCompleted
	case "failed":
		return evaluation.WorkflowNodeFailed
	case "cancelled":
		return evaluation.WorkflowNodeCancelled
	default:
		return evaluation.WorkflowNodeUnknown
	}
}

func addWorkflowTraceModels(ctx context.Context, tx pgx.Tx, builder *workflowTraceBuilder, taskRow workflowTraceTask, taskNode string) error {
	var count int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM model_call_ledger WHERE task_id=$1 AND owner_id=$2 AND source='task'`, taskRow.ID, taskRow.OwnerID).Scan(&count); err != nil {
		return fmt.Errorf("count workflow Task model calls: %w", err)
	}
	countSource, err := builder.addCountSource(evaluation.WorkflowSourceModelCallLedger, "db://model_call_ledger/task/"+taskRow.ID+"/count", count)
	if err != nil {
		return err
	}
	if count == 0 {
		zero := 0
		builder.addGap(evaluation.WorkflowStageModelCall, "Task model-call records", evaluation.WorkflowGapAbsent, &zero, countSource)
	}
	workflowTruncatedGap(builder, evaluation.WorkflowStageModelCall, "Task model-call records", count, countSource)
	rows, err := tx.Query(ctx, `SELECT l.call_id,l.state,l.model_id,l.provider_model,encode(l.request_digest,'hex'),l.reserved_tokens,
COALESCE(l.input_tokens,0),COALESCE(l.output_tokens,0),l.created_at,COALESCE(l.settled_at,l.created_at),
COALESCE(r.agent_session_id,''),COALESCE(r.authorization_version,0),COALESCE(encode(r.response_digest,'hex'),'')
FROM model_call_ledger l LEFT JOIN task_model_call_responses r
 ON r.owner_id=l.owner_id AND r.source=l.source AND r.call_id=l.call_id AND r.task_id=l.task_id
WHERE l.task_id=$1 AND l.owner_id=$2 AND l.source='task' ORDER BY l.created_at,l.call_id LIMIT $3`,
		taskRow.ID, taskRow.OwnerID, workflowTraceFactLimit)
	if err != nil {
		return fmt.Errorf("read workflow Task model calls: %w", err)
	}
	for rows.Next() {
		var callID, state, modelID, providerModel, requestDigest, sessionID, responseDigest string
		var reserved, input, output, authVersion int64
		var createdAt, settledAt time.Time
		if err := rows.Scan(&callID, &state, &modelID, &providerModel, &requestDigest, &reserved, &input, &output, &createdAt, &settledAt, &sessionID, &authVersion, &responseDigest); err != nil {
			rows.Close()
			return err
		}
		sourceID, err := builder.addSource(evaluation.WorkflowSourceModelCallLedger, "db://model_call_ledger/task/"+taskRow.ID+"/call/"+callID, struct {
			CallID               string    `json:"call_id"`
			State                string    `json:"state"`
			ModelID              string    `json:"model_id"`
			ProviderModel        string    `json:"provider_model"`
			RequestDigest        string    `json:"request_digest"`
			Reserved             int64     `json:"reserved_tokens"`
			Input                int64     `json:"input_tokens"`
			Output               int64     `json:"output_tokens"`
			CreatedAt            time.Time `json:"created_at"`
			SettledAt            time.Time `json:"settled_at"`
			AgentSessionID       string    `json:"agent_session_id,omitempty"`
			AuthorizationVersion int64     `json:"authorization_version,omitempty"`
			ResponseDigest       string    `json:"response_digest,omitempty"`
		}{callID, state, modelID, providerModel, requestDigest, reserved, input, output, createdAt, settledAt, sessionID, authVersion, responseDigest})
		if err != nil {
			rows.Close()
			return err
		}
		nodeID := builder.addNode(evaluation.WorkflowNodeModelCall, workflowCallNodeState(state), taskRow.ID+":"+callID, sourceID)
		builder.modelNodes[callID], builder.modelSources[callID] = nodeID, sourceID
		builder.addEdge(evaluation.WorkflowEdgeInvoked, taskNode, nodeID, sourceID)
		if sessionID == "" {
			builder.addGap(evaluation.WorkflowStageAgent, "model call has no persisted response-to-Agent binding: "+callID, evaluation.WorkflowGapUnknown, nil, sourceID)
			builder.addGap(evaluation.WorkflowStageAuthorization, "model call authorization snapshot is unavailable: "+callID, evaluation.WorkflowGapUnknown, nil, sourceID)
			continue
		}
		if agentID, exists := builder.agentNodes[sessionID]; exists {
			builder.addEdge(evaluation.WorkflowEdgeInvoked, agentID, nodeID, sourceID)
		} else {
			builder.addGap(evaluation.WorkflowStageAgent, "model-call Agent binding outside captured ledger window: "+callID, evaluation.WorkflowGapUnknown, nil, sourceID)
		}
		authSource, err := builder.addSource(evaluation.WorkflowSourceModelCallLedger, "db://task_model_call_responses/task/"+taskRow.ID+"/call/"+callID, struct {
			CallID               string `json:"call_id"`
			AgentSessionID       string `json:"agent_session_id"`
			AuthorizationVersion int64  `json:"authorization_version"`
			ResponseDigest       string `json:"response_digest"`
		}{callID, sessionID, authVersion, responseDigest})
		if err != nil {
			rows.Close()
			return err
		}
		if authVersion == 0 {
			builder.addGap(evaluation.WorkflowStageAuthorization, "model-call authorization version missing: "+callID, evaluation.WorkflowGapUnknown, nil, authSource)
			continue
		}
		authNode := builder.addNode(evaluation.WorkflowNodeAuth, evaluation.WorkflowNodeSettled, fmt.Sprintf("task:%s:auth:%d", taskRow.ID, authVersion), authSource)
		builder.addEdge(evaluation.WorkflowEdgeAuthorized, nodeID, authNode, authSource)
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return fmt.Errorf("iterate workflow Task model calls: %w", rowsErr)
	}

	var routeCount int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM conversation_route_model_calls r WHERE r.owner_id=$1 AND r.conversation_id=$2
AND r.message_id IN (SELECT id FROM conversation_messages WHERE task_id=$3 AND owner_id=$1 AND conversation_id=$2)`, taskRow.OwnerID, taskRow.ConversationID, taskRow.ID).Scan(&routeCount); err != nil {
		return fmt.Errorf("count workflow route model calls: %w", err)
	}
	routeCountSource, err := builder.addCountSource(evaluation.WorkflowSourceModelCallLedger, "db://conversation_route_model_calls/task/"+taskRow.ID+"/count", routeCount)
	if err != nil {
		return err
	}
	if routeCount == 0 {
		zero := 0
		builder.addGap(evaluation.WorkflowStageModelCall, "route model-call records", evaluation.WorkflowGapAbsent, &zero, routeCountSource)
	}
	workflowTruncatedGap(builder, evaluation.WorkflowStageModelCall, "route model-call records", routeCount, routeCountSource)
	rows, err = tx.Query(ctx, `SELECT r.message_id,r.call_id,r.state,r.model_id,encode(l.request_digest,'hex'),l.state,
l.reserved_tokens,COALESCE(l.input_tokens,0),COALESCE(l.output_tokens,0),r.created_at,r.updated_at
FROM conversation_route_model_calls r JOIN model_call_ledger l ON l.owner_id=r.actor_id AND l.source=r.source AND l.call_id=r.call_id
WHERE r.owner_id=$1 AND r.conversation_id=$2 AND r.message_id IN
(SELECT id FROM conversation_messages WHERE task_id=$3 AND owner_id=$1 AND conversation_id=$2)
ORDER BY r.created_at,r.message_id LIMIT $4`, taskRow.OwnerID, taskRow.ConversationID, taskRow.ID, workflowTraceFactLimit)
	if err != nil {
		return fmt.Errorf("read workflow route model calls: %w", err)
	}
	for rows.Next() {
		var messageID, callID, routeState, modelID, requestDigest, ledgerState string
		var reserved, input, output int64
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&messageID, &callID, &routeState, &modelID, &requestDigest, &ledgerState, &reserved, &input, &output, &createdAt, &updatedAt); err != nil {
			rows.Close()
			return err
		}
		sourceID, err := builder.addSource(evaluation.WorkflowSourceModelCallLedger, "db://conversation_route_model_calls/message/"+messageID, struct {
			MessageID     string    `json:"message_id"`
			CallID        string    `json:"call_id"`
			RouteState    string    `json:"route_state"`
			ModelID       string    `json:"model_id"`
			RequestDigest string    `json:"request_digest"`
			LedgerState   string    `json:"ledger_state"`
			Reserved      int64     `json:"reserved_tokens"`
			Input         int64     `json:"input_tokens"`
			Output        int64     `json:"output_tokens"`
			CreatedAt     time.Time `json:"created_at"`
			UpdatedAt     time.Time `json:"updated_at"`
		}{messageID, callID, routeState, modelID, requestDigest, ledgerState, reserved, input, output, createdAt, updatedAt})
		if err != nil {
			rows.Close()
			return err
		}
		nodeID := builder.addNode(evaluation.WorkflowNodeModelCall, workflowCallNodeState(ledgerState), "route:"+messageID+":"+callID, sourceID)
		if routeNode, exists := builder.routeNodes[messageID]; exists {
			builder.addEdge(evaluation.WorkflowEdgeInvoked, routeNode, nodeID, sourceID)
		} else {
			builder.addGap(evaluation.WorkflowStageRoute, "model-call route node outside captured route window: "+callID, evaluation.WorkflowGapUnknown, nil, sourceID, routeCountSource)
		}
	}
	rowsErr = rows.Err()
	rows.Close()
	if rowsErr != nil {
		return fmt.Errorf("iterate workflow route model calls: %w", rowsErr)
	}
	return nil
}

func addWorkflowTraceOperations(ctx context.Context, tx pgx.Tx, builder *workflowTraceBuilder, taskRow workflowTraceTask, taskNode string) error {
	builder.operations = make([]workflowTraceOperationRecord, 0)
	var count int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM task_operations operation
JOIN tasks task ON task.id=operation.task_id AND task.owner_id=$2 WHERE operation.task_id=$1`, taskRow.ID, taskRow.OwnerID).Scan(&count); err != nil {
		return fmt.Errorf("count legacy workflow operations: %w", err)
	}
	countSource, err := builder.addCountSource(evaluation.WorkflowSourceOperationLedger, "db://task_operations/task/"+taskRow.ID+"/count", count)
	if err != nil {
		return err
	}
	if count == 0 {
		zero := 0
		builder.addGap(evaluation.WorkflowStageOperation, "legacy host operation records", evaluation.WorkflowGapAbsent, &zero, countSource)
	}
	workflowTruncatedGap(builder, evaluation.WorkflowStageOperation, "legacy host operation records", count, countSource)
	rows, err := tx.Query(ctx, `SELECT operation.id,operation.status,COALESCE(operation.task_revision,-1),COALESCE(operation.call_id,''),COALESCE(operation.tool_name,''),
COALESCE(operation.approval_id,''),COALESCE(operation.approval_version,0),COALESCE(encode(operation.approval_request_hash,'hex'),''),encode(operation.request_hash,'hex'),
	operation.host_id,operation.runtime_generation,operation.control_version,operation.created_at,operation.updated_at
FROM task_operations operation JOIN tasks task ON task.id=operation.task_id AND task.owner_id=$2
WHERE operation.task_id=$1 ORDER BY operation.created_at,operation.id LIMIT $3`, taskRow.ID, taskRow.OwnerID, workflowTraceFactLimit)
	if err != nil {
		return fmt.Errorf("read legacy workflow operations: %w", err)
	}
	for rows.Next() {
		var id, status, callID, toolName, approvalID, approvalHash, requestHash, hostID string
		var revision, approvalVersion, generation, controlVersion int64
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&id, &status, &revision, &callID, &toolName, &approvalID, &approvalVersion, &approvalHash, &requestHash, &hostID, &generation, &controlVersion, &createdAt, &updatedAt); err != nil {
			rows.Close()
			return err
		}
		sourceID, err := builder.addSource(evaluation.WorkflowSourceOperationLedger, "db://task_operations/"+taskRow.ID+"/"+id, struct {
			ID                string    `json:"id"`
			Status            string    `json:"status"`
			Revision          int64     `json:"task_revision"`
			CallID            string    `json:"call_id,omitempty"`
			ToolName          string    `json:"tool_name,omitempty"`
			ApprovalID        string    `json:"approval_id,omitempty"`
			ApprovalVersion   int64     `json:"approval_version,omitempty"`
			ApprovalHash      string    `json:"approval_request_hash,omitempty"`
			RequestHash       string    `json:"request_hash"`
			HostID            string    `json:"host_id"`
			RuntimeGeneration int64     `json:"runtime_generation"`
			ControlVersion    int64     `json:"control_version"`
			CreatedAt         time.Time `json:"created_at"`
			UpdatedAt         time.Time `json:"updated_at"`
		}{id, status, revision, callID, toolName, approvalID, approvalVersion, approvalHash, requestHash, hostID, generation, controlVersion, createdAt, updatedAt})
		if err != nil {
			rows.Close()
			return err
		}
		nodeID := builder.addNode(evaluation.WorkflowNodeOperation, workflowOperationNodeState(status), "legacy:"+taskRow.ID+":"+id, sourceID)
		builder.addEdge(evaluation.WorkflowEdgeCreated, taskNode, nodeID, sourceID)
		builder.operations = append(builder.operations, workflowTraceOperationRecord{nodeID: nodeID, sourceID: sourceID, approvalID: approvalID, approvalVersion: approvalVersion, approvalHash: approvalHash, callID: callID, toolName: toolName, executionKind: "operation", executionID: id, requestHash: requestHash, taskRevision: revision})
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return fmt.Errorf("iterate legacy workflow operations: %w", rowsErr)
	}

	if err := tx.QueryRow(ctx, `SELECT count(*) FROM task_hub_operations WHERE task_id=$1 AND owner_id=$2`, taskRow.ID, taskRow.OwnerID).Scan(&count); err != nil {
		return fmt.Errorf("count Hub workflow operations: %w", err)
	}
	countSource, err = builder.addCountSource(evaluation.WorkflowSourceOperationLedger, "db://task_hub_operations/task/"+taskRow.ID+"/count", count)
	if err != nil {
		return err
	}
	if count == 0 {
		zero := 0
		builder.addGap(evaluation.WorkflowStageOperation, "Task Hub operation records", evaluation.WorkflowGapAbsent, &zero, countSource)
	}
	workflowTruncatedGap(builder, evaluation.WorkflowStageOperation, "Task Hub operation records", count, countSource)
	rows, err = tx.Query(ctx, `SELECT operation_key,tool_name,target_installation_id,action,task_authorization_version,target_grant_version,
encode(request_hash,'hex'),COALESCE(encode(result_hash,'hex'),''),status,agent_session_id,call_id,plan_task_revision,plan_revision,plan_step_id,updated_at
FROM task_hub_operations WHERE task_id=$1 AND owner_id=$2 ORDER BY created_at,operation_key LIMIT $3`, taskRow.ID, taskRow.OwnerID, workflowTraceFactLimit)
	if err != nil {
		return fmt.Errorf("read Hub workflow operations: %w", err)
	}
	for rows.Next() {
		var key, tool, target, action, requestHash, resultHash, status, sessionID, callID, stepID string
		var authVersion, grantVersion, taskRevision, planRevision int64
		var updatedAt time.Time
		if err := rows.Scan(&key, &tool, &target, &action, &authVersion, &grantVersion, &requestHash, &resultHash, &status, &sessionID, &callID, &taskRevision, &planRevision, &stepID, &updatedAt); err != nil {
			rows.Close()
			return err
		}
		sourceID, err := builder.addSource(evaluation.WorkflowSourceOperationLedger, "db://task_hub_operations/"+taskRow.ID+"/"+key, struct {
			OperationKey         string    `json:"operation_key"`
			ToolName             string    `json:"tool_name"`
			TargetInstallationID string    `json:"target_installation_id"`
			Action               string    `json:"action"`
			AuthorizationVersion int64     `json:"task_authorization_version"`
			TargetGrantVersion   int64     `json:"target_grant_version"`
			RequestHash          string    `json:"request_hash"`
			ResultHash           string    `json:"result_hash,omitempty"`
			Status               string    `json:"status"`
			AgentSessionID       string    `json:"agent_session_id"`
			CallID               string    `json:"call_id"`
			TaskRevision         int64     `json:"plan_task_revision"`
			PlanRevision         int64     `json:"plan_revision"`
			PlanStepID           string    `json:"plan_step_id"`
			UpdatedAt            time.Time `json:"updated_at"`
		}{key, tool, target, action, authVersion, grantVersion, requestHash, resultHash, status, sessionID, callID, taskRevision, planRevision, stepID, updatedAt})
		if err != nil {
			rows.Close()
			return err
		}
		nodeID := builder.addNode(evaluation.WorkflowNodeOperation, workflowOperationNodeState(status), "hub:"+taskRow.ID+":"+key, sourceID)
		builder.addEdge(evaluation.WorkflowEdgeCreated, taskNode, nodeID, sourceID)
		if agentID, ok := builder.agentNodes[sessionID]; ok {
			builder.addEdge(evaluation.WorkflowEdgeInvoked, agentID, nodeID, sourceID)
		} else {
			builder.addGap(evaluation.WorkflowStageAgent, "Hub operation Agent binding is missing or outside the captured window: "+key, evaluation.WorkflowGapUnknown, nil, sourceID)
		}
		authNode := builder.addNode(evaluation.WorkflowNodeAuth, evaluation.WorkflowNodeSettled, fmt.Sprintf("task:%s:auth:%d", taskRow.ID, authVersion), sourceID)
		builder.addEdge(evaluation.WorkflowEdgeAuthorized, nodeID, authNode, sourceID)
		builder.addGap(evaluation.WorkflowStageAuthorization, "Hub target grant snapshot version is recorded but grant details are not in the Task ledger: "+key, evaluation.WorkflowGapUnknown, nil, sourceID)
	}
	rowsErr = rows.Err()
	rows.Close()
	if rowsErr != nil {
		return fmt.Errorf("iterate Hub workflow operations: %w", rowsErr)
	}

	if err := tx.QueryRow(ctx, `SELECT count(*) FROM task_mcp_connector_operations WHERE task_id=$1 AND owner_id=$2`, taskRow.ID, taskRow.OwnerID).Scan(&count); err != nil {
		return fmt.Errorf("count connector workflow operations: %w", err)
	}
	countSource, err = builder.addCountSource(evaluation.WorkflowSourceOperationLedger, "db://task_mcp_connector_operations/task/"+taskRow.ID+"/count", count)
	if err != nil {
		return err
	}
	if count == 0 {
		zero := 0
		builder.addGap(evaluation.WorkflowStageOperation, "MCP connector operation records", evaluation.WorkflowGapAbsent, &zero, countSource)
	}
	workflowTruncatedGap(builder, evaluation.WorkflowStageOperation, "MCP connector operation records", count, countSource)
	rows, err = tx.Query(ctx, `SELECT id,COALESCE(connector_id,'builtin:' || builtin_service),connector_revision,grant_version,operation_key,tool_name,COALESCE(call_id,''),
COALESCE(task_revision,-1),COALESCE(approval_id,''),COALESCE(approval_version,0),COALESCE(encode(approval_request_hash,'hex'),''),
encode(request_hash,'hex'),status,updated_at FROM task_mcp_connector_operations WHERE task_id=$1 AND owner_id=$2 ORDER BY created_at,id LIMIT $3`, taskRow.ID, taskRow.OwnerID, workflowTraceFactLimit)
	if err != nil {
		return fmt.Errorf("read connector workflow operations: %w", err)
	}
	for rows.Next() {
		var id, connectorID, key, tool, callID, approvalID, approvalHash, requestHash, status string
		var connectorRevision, grantVersion, revision, approvalVersion int64
		var updatedAt time.Time
		if err := rows.Scan(&id, &connectorID, &connectorRevision, &grantVersion, &key, &tool, &callID, &revision, &approvalID, &approvalVersion, &approvalHash, &requestHash, &status, &updatedAt); err != nil {
			rows.Close()
			return err
		}
		sourceID, err := builder.addSource(evaluation.WorkflowSourceOperationLedger, "db://task_mcp_connector_operations/"+taskRow.ID+"/"+id, struct {
			ID                string    `json:"id"`
			ConnectorID       string    `json:"connector_id"`
			ConnectorRevision int64     `json:"connector_revision"`
			GrantVersion      int64     `json:"grant_version"`
			OperationKey      string    `json:"operation_key"`
			ToolName          string    `json:"tool_name"`
			CallID            string    `json:"call_id,omitempty"`
			TaskRevision      int64     `json:"task_revision"`
			ApprovalID        string    `json:"approval_id,omitempty"`
			ApprovalVersion   int64     `json:"approval_version,omitempty"`
			ApprovalHash      string    `json:"approval_request_hash,omitempty"`
			RequestHash       string    `json:"request_hash"`
			Status            string    `json:"status"`
			UpdatedAt         time.Time `json:"updated_at"`
		}{id, connectorID, connectorRevision, grantVersion, key, tool, callID, revision, approvalID, approvalVersion, approvalHash, requestHash, status, updatedAt})
		if err != nil {
			rows.Close()
			return err
		}
		nodeID := builder.addNode(evaluation.WorkflowNodeOperation, workflowOperationNodeState(status), "mcp:"+taskRow.ID+":"+id, sourceID)
		builder.addEdge(evaluation.WorkflowEdgeCreated, taskNode, nodeID, sourceID)
		builder.operations = append(builder.operations, workflowTraceOperationRecord{nodeID: nodeID, sourceID: sourceID, approvalID: approvalID, approvalVersion: approvalVersion, approvalHash: approvalHash, callID: callID, toolName: tool, executionKind: "mcp_connector", executionID: id, requestHash: requestHash, connectorID: connectorID, taskRevision: revision})
	}
	rowsErr = rows.Err()
	rows.Close()
	if rowsErr != nil {
		return fmt.Errorf("iterate connector workflow operations: %w", rowsErr)
	}
	if err := addWorkflowTracePlanOperationBindings(ctx, tx, builder, taskRow); err != nil {
		return err
	}
	return nil
}

func addWorkflowTracePlanOperationBindings(ctx context.Context, tx pgx.Tx, builder *workflowTraceBuilder, taskRow workflowTraceTask) error {
	for _, operation := range builder.operations {
		if operation.executionKind == "" {
			continue
		}
		if operation.taskRevision < 0 {
			builder.addGap(evaluation.WorkflowStageAgent, "operation has no task revision for a plan binding: "+operation.executionID, evaluation.WorkflowGapUnknown, nil, operation.sourceID)
			builder.addGap(evaluation.WorkflowStageAuthorization, "operation has no task revision for a plan binding: "+operation.executionID, evaluation.WorkflowGapUnknown, nil, operation.sourceID)
			continue
		}
		rows, err := tx.Query(ctx, `SELECT plan_revision,plan_step_id,agent_session_id,call_id,tool_name,connector_id,
encode(request_digest,'hex'),authorization_version,count(*) OVER()
FROM task_plan_step_execution_bindings WHERE owner_id=$1 AND task_id=$2 AND task_revision=$3
AND execution_kind=$4 AND execution_id=$5 ORDER BY plan_revision DESC LIMIT 2`, taskRow.OwnerID, taskRow.ID, operation.taskRevision, operation.executionKind, operation.executionID)
		if err != nil {
			return fmt.Errorf("read workflow plan operation binding: %w", err)
		}
		type bindingFact struct {
			planRevision, authorizationVersion, total                     int64
			stepID, sessionID, callID, toolName, connectorID, requestHash string
		}
		bindings := make([]bindingFact, 0, 2)
		for rows.Next() {
			var binding bindingFact
			if err := rows.Scan(&binding.planRevision, &binding.stepID, &binding.sessionID, &binding.callID, &binding.toolName, &binding.connectorID, &binding.requestHash, &binding.authorizationVersion, &binding.total); err != nil {
				rows.Close()
				return err
			}
			bindings = append(bindings, binding)
		}
		rowsErr := rows.Err()
		rows.Close()
		if rowsErr != nil {
			return fmt.Errorf("iterate workflow plan operation bindings: %w", rowsErr)
		}
		if len(bindings) == 0 {
			builder.addGap(evaluation.WorkflowStageAgent, "operation has no matching durable Agent plan binding: "+operation.executionID, evaluation.WorkflowGapUnknown, nil, operation.sourceID)
			builder.addGap(evaluation.WorkflowStageAuthorization, "operation authorization version is absent without a plan binding: "+operation.executionID, evaluation.WorkflowGapUnknown, nil, operation.sourceID)
			continue
		}
		if len(bindings) != 1 || bindings[0].total != 1 {
			builder.addGap(evaluation.WorkflowStageAgent, "operation has multiple plan bindings and cannot be joined uniquely: "+operation.executionID, evaluation.WorkflowGapUnknown, nil, operation.sourceID)
			builder.addGap(evaluation.WorkflowStageAuthorization, "operation authorization binding is ambiguous: "+operation.executionID, evaluation.WorkflowGapUnknown, nil, operation.sourceID)
			continue
		}
		binding := bindings[0]
		sourceID, err := builder.addSource(evaluation.WorkflowSourceOperationLedger, "db://task_plan_step_execution_bindings/"+taskRow.ID+"/"+operation.executionKind+"/"+operation.executionID, struct {
			TaskRevision         int64  `json:"task_revision"`
			PlanRevision         int64  `json:"plan_revision"`
			PlanStepID           string `json:"plan_step_id"`
			ExecutionKind        string `json:"execution_kind"`
			ExecutionID          string `json:"execution_id"`
			AgentSessionID       string `json:"agent_session_id"`
			CallID               string `json:"call_id"`
			ToolName             string `json:"tool_name"`
			ConnectorID          string `json:"connector_id,omitempty"`
			RequestDigest        string `json:"request_digest"`
			AuthorizationVersion int64  `json:"authorization_version"`
		}{operation.taskRevision, binding.planRevision, binding.stepID, operation.executionKind, operation.executionID, binding.sessionID, binding.callID, binding.toolName, binding.connectorID, binding.requestHash, binding.authorizationVersion})
		if err != nil {
			return err
		}
		if binding.callID != operation.callID || binding.toolName != operation.toolName || binding.requestHash != operation.requestHash || operation.connectorID != "" && binding.connectorID != operation.connectorID {
			builder.addGap(evaluation.WorkflowStageAgent, "operation and plan binding call, tool, connector, or digest do not match: "+operation.executionID, evaluation.WorkflowGapUnknown, nil, operation.sourceID, sourceID)
			builder.addGap(evaluation.WorkflowStageAuthorization, "operation authorization binding does not match its operation digest: "+operation.executionID, evaluation.WorkflowGapUnknown, nil, operation.sourceID, sourceID)
			continue
		}
		if agentID, ok := builder.agentNodes[binding.sessionID]; ok {
			builder.addEdge(evaluation.WorkflowEdgeInvoked, agentID, operation.nodeID, operation.sourceID, sourceID)
		} else {
			builder.addGap(evaluation.WorkflowStageAgent, "plan-bound operation Agent is outside the captured session window: "+operation.executionID, evaluation.WorkflowGapUnknown, nil, operation.sourceID, sourceID)
		}
		if binding.authorizationVersion < 1 {
			builder.addGap(evaluation.WorkflowStageAuthorization, "plan-bound operation authorization version is invalid: "+operation.executionID, evaluation.WorkflowGapUnknown, nil, operation.sourceID, sourceID)
			continue
		}
		authNode := builder.addNode(evaluation.WorkflowNodeAuth, evaluation.WorkflowNodeSettled, fmt.Sprintf("task:%s:auth:%d", taskRow.ID, binding.authorizationVersion), sourceID)
		builder.addEdge(evaluation.WorkflowEdgeAuthorized, operation.nodeID, authNode, operation.sourceID, sourceID)
	}
	return nil
}

func addWorkflowTraceApprovals(ctx context.Context, tx pgx.Tx, builder *workflowTraceBuilder, taskRow workflowTraceTask, _ string) error {
	var count int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM task_pending_requests request
JOIN tasks task ON task.id=request.task_id AND task.owner_id=$2 WHERE request.task_id=$1`, taskRow.ID, taskRow.OwnerID).Scan(&count); err != nil {
		return fmt.Errorf("count workflow approvals: %w", err)
	}
	countSource, err := builder.addCountSource(evaluation.WorkflowSourceApprovalLedger, "db://task_pending_requests/task/"+taskRow.ID+"/count", count)
	if err != nil {
		return err
	}
	if count == 0 {
		zero := 0
		builder.addGap(evaluation.WorkflowStageApproval, "approval request records", evaluation.WorkflowGapAbsent, &zero, countSource)
	}
	workflowTruncatedGap(builder, evaluation.WorkflowStageApproval, "approval request records", count, countSource)
	rows, err := tx.Query(ctx, `SELECT request.id,request.version,request.kind,request.call_id,request.tool_name,encode(request.request_hash,'hex'),request.status,request.decision,COALESCE(request.task_revision,-1),
COALESCE(request.responded_by_user_id,''),request.created_at,COALESCE(request.responded_at,request.created_at)
FROM task_pending_requests request JOIN tasks task ON task.id=request.task_id AND task.owner_id=$2
WHERE request.task_id=$1 ORDER BY request.version,request.id LIMIT $3`, taskRow.ID, taskRow.OwnerID, workflowTraceFactLimit)
	if err != nil {
		return fmt.Errorf("read workflow approval ledger: %w", err)
	}
	type approvalFact struct {
		id                                       string
		version                                  int64
		callID, toolName, hash, status, decision string
		nodeID, sourceID                         string
	}
	approvals := make(map[string]approvalFact)
	for rows.Next() {
		var fact approvalFact
		var kind string
		var revision int64
		var responder string
		var createdAt, respondedAt time.Time
		if err := rows.Scan(&fact.id, &fact.version, &kind, &fact.callID, &fact.toolName, &fact.hash, &fact.status, &fact.decision, &revision, &responder, &createdAt, &respondedAt); err != nil {
			rows.Close()
			return err
		}
		fact.sourceID, err = builder.addSource(evaluation.WorkflowSourceApprovalLedger, "db://task_pending_requests/"+taskRow.ID+"/"+fact.id+"/"+fmt.Sprint(fact.version), struct {
			ID           string    `json:"id"`
			Version      int64     `json:"version"`
			Kind         string    `json:"kind"`
			CallID       string    `json:"call_id"`
			ToolName     string    `json:"tool_name"`
			RequestHash  string    `json:"request_hash"`
			Status       string    `json:"status"`
			Decision     string    `json:"decision"`
			TaskRevision int64     `json:"task_revision"`
			RespondedBy  string    `json:"responded_by_user_id,omitempty"`
			CreatedAt    time.Time `json:"created_at"`
			RespondedAt  time.Time `json:"responded_at"`
		}{fact.id, fact.version, kind, fact.callID, fact.toolName, fact.hash, fact.status, fact.decision, revision, responder, createdAt, respondedAt})
		if err != nil {
			rows.Close()
			return err
		}
		fact.nodeID = builder.addNode(evaluation.WorkflowNodeApproval, workflowApprovalNodeState(fact.status, fact.decision), taskRow.ID+":"+fact.id+":"+fmt.Sprint(fact.version), fact.sourceID)
		approvals[fact.id+":"+fmt.Sprint(fact.version)] = fact
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return fmt.Errorf("iterate workflow approval ledger: %w", rowsErr)
	}
	for _, operation := range builder.operations {
		if operation.approvalID == "" {
			continue
		}
		key := operation.approvalID + ":" + fmt.Sprint(operation.approvalVersion)
		approval, exists := approvals[key]
		if !exists {
			builder.addGap(evaluation.WorkflowStageApproval, "operation approval reference is missing from captured approval ledger: "+key, evaluation.WorkflowGapUnknown, nil, operation.sourceID, countSource)
			continue
		}
		if operation.approvalHash != approval.hash || operation.callID != approval.callID || operation.toolName != "" && operation.toolName != approval.toolName {
			builder.addGap(evaluation.WorkflowStageApproval, "operation and approval binding hashes or call identity do not match: "+key, evaluation.WorkflowGapUnknown, nil, operation.sourceID, approval.sourceID)
			continue
		}
		builder.addEdge(evaluation.WorkflowEdgeApprovalRequested, operation.nodeID, approval.nodeID, operation.sourceID, approval.sourceID)
		switch approval.status {
		case "approved":
			builder.addEdge(evaluation.WorkflowEdgeApproved, approval.nodeID, operation.nodeID, approval.sourceID)
		case "rejected":
			builder.addEdge(evaluation.WorkflowEdgeRejected, approval.nodeID, operation.nodeID, approval.sourceID)
		case "expired", "cancelled":
			builder.addGap(evaluation.WorkflowStageApproval, "approval was "+approval.status+"; no matching outcome edge is defined", evaluation.WorkflowGapUnknown, nil, operation.sourceID, approval.sourceID)
		}
	}
	return nil
}

func workflowApprovalNodeState(status, decision string) evaluation.WorkflowNodeState {
	switch status {
	case "approved":
		return evaluation.WorkflowNodeApproved
	case "rejected":
		return evaluation.WorkflowNodeRejected
	case "pending":
		return evaluation.WorkflowNodePending
	case "expired":
		return evaluation.WorkflowNodeExpired
	case "cancelled":
		return evaluation.WorkflowNodeCancelled
	default:
		if decision == "approve" {
			return evaluation.WorkflowNodeApproved
		}
		if decision == "reject" {
			return evaluation.WorkflowNodeRejected
		}
		return evaluation.WorkflowNodeUnknown
	}
}

func addWorkflowTraceValidation(ctx context.Context, tx pgx.Tx, builder *workflowTraceBuilder, taskRow workflowTraceTask, taskNode string) error {
	var count int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM task_validation_reviews WHERE task_id=$1 AND owner_id=$2`, taskRow.ID, taskRow.OwnerID).Scan(&count); err != nil {
		return fmt.Errorf("count workflow validation reviews: %w", err)
	}
	countSource, err := builder.addCountSource(evaluation.WorkflowSourceValidationLedger, "db://task_validation_reviews/task/"+taskRow.ID+"/count", count)
	if err != nil {
		return err
	}
	if count == 0 {
		if taskRow.Status == "completed" {
			zero := 0
			builder.addGap(evaluation.WorkflowStageVerification, "validation review records", evaluation.WorkflowGapAbsent, &zero, countSource)
		} else {
			builder.addGap(evaluation.WorkflowStageVerification, "validation review not yet scheduled for current Task state", evaluation.WorkflowGapNotRun, nil, countSource)
		}
	}
	workflowTruncatedGap(builder, evaluation.WorkflowStageVerification, "validation review records", count, countSource)
	rows, err := tx.Query(ctx, `SELECT task_revision,result_event_sequence,encode(result_sha256,'hex'),state,validation_state,
COALESCE(execution_model_call_id,''),COALESCE(reviewed_at,updated_at),created_at,updated_at
FROM task_validation_reviews WHERE task_id=$1 AND owner_id=$2 ORDER BY task_revision DESC LIMIT $3`, taskRow.ID, taskRow.OwnerID, workflowTraceFactLimit)
	if err != nil {
		return fmt.Errorf("read workflow validation reviews: %w", err)
	}
	for rows.Next() {
		var revision, eventSequence int64
		var resultHash, state, verdict, callID string
		var reviewedAt, createdAt, updatedAt time.Time
		if err := rows.Scan(&revision, &eventSequence, &resultHash, &state, &verdict, &callID, &reviewedAt, &createdAt, &updatedAt); err != nil {
			rows.Close()
			return err
		}
		sourceID, err := builder.addSource(evaluation.WorkflowSourceValidationLedger, "db://task_validation_reviews/"+taskRow.ID+"/revision/"+fmt.Sprint(revision), struct {
			TaskRevision         int64     `json:"task_revision"`
			ResultEventSequence  int64     `json:"result_event_sequence"`
			ResultSHA256         string    `json:"result_sha256"`
			ReviewState          string    `json:"review_state"`
			Verdict              string    `json:"validation_state"`
			ExecutionModelCallID string    `json:"execution_model_call_id,omitempty"`
			ReviewedAt           time.Time `json:"reviewed_at"`
			CreatedAt            time.Time `json:"created_at"`
			UpdatedAt            time.Time `json:"updated_at"`
		}{revision, eventSequence, resultHash, state, verdict, callID, reviewedAt, createdAt, updatedAt})
		if err != nil {
			rows.Close()
			return err
		}
		nodeID := builder.addNode(evaluation.WorkflowNodeValidation, workflowValidationNodeState(state, verdict), taskRow.ID+":"+fmt.Sprint(revision), sourceID)
		if state == "reviewed" && (verdict == "PASS" || verdict == "PASS+NOTES") {
			builder.addEdge(evaluation.WorkflowEdgeVerified, taskNode, nodeID, sourceID)
		}
		if callID != "" {
			if modelNode, exists := builder.modelNodes[callID]; exists {
				builder.addEdge(evaluation.WorkflowEdgeReturned, modelNode, nodeID, builder.modelSources[callID])
			} else {
				builder.addGap(evaluation.WorkflowStageModelCall, "validation execution model-call is outside the captured model ledger window: "+callID, evaluation.WorkflowGapUnknown, nil, sourceID)
			}
		} else if state == "reviewed" {
			builder.addGap(evaluation.WorkflowStageModelCall, "reviewed validation has no execution model-call binding: revision "+fmt.Sprint(revision), evaluation.WorkflowGapUnknown, nil, sourceID)
		}
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return fmt.Errorf("iterate workflow validation reviews: %w", rowsErr)
	}
	return nil
}

func workflowValidationNodeState(state, verdict string) evaluation.WorkflowNodeState {
	switch {
	case state == "reviewed" && verdict == "FAIL":
		return evaluation.WorkflowNodeFailed
	case state == "reviewed" && (verdict == "PASS" || verdict == "PASS+NOTES"):
		return evaluation.WorkflowNodeCompleted
	case state == "processing":
		return evaluation.WorkflowNodeRunning
	case state == "pending":
		return evaluation.WorkflowNodePending
	case state == "failed":
		return evaluation.WorkflowNodeFailed
	case state == "superseded":
		return evaluation.WorkflowNodeUnknown
	default:
		return evaluation.WorkflowNodeUnknown
	}
}

func addWorkflowTraceDeliveries(ctx context.Context, tx pgx.Tx, builder *workflowTraceBuilder, taskRow workflowTraceTask, taskNode string) error {
	deliveryTargets := make(map[string]workflowTraceDeliveryObservationTarget)
	var count int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM conversation_messages WHERE task_id=$1 AND owner_id=$2 AND conversation_id=$3 AND role='assistant'`, taskRow.ID, taskRow.OwnerID, taskRow.ConversationID).Scan(&count); err != nil {
		return fmt.Errorf("count workflow assistant deliveries: %w", err)
	}
	countSource, err := builder.addCountSource(evaluation.WorkflowSourceConversationOutput, "db://conversation_messages/task/"+taskRow.ID+"/assistant-count", count)
	if err != nil {
		return err
	}
	if count == 0 {
		zero := 0
		builder.addGap(evaluation.WorkflowStageDelivery, "assistant conversation output records", evaluation.WorkflowGapAbsent, &zero, countSource)
	}
	workflowTruncatedGap(builder, evaluation.WorkflowStageDelivery, "assistant conversation output records", count, countSource)
	rows, err := tx.Query(ctx, `SELECT message.id,message.sequence,message.delivery_status,COALESCE(message.output_status,''),COALESCE(message.output_task_revision,-1),
COALESCE(message.output_result_event_sequence,0),octet_length(message.content)::bigint,encode(sha256(convert_to(message.content,'UTF8')),'hex'),message.created_at,
EXISTS (SELECT 1 FROM task_events result WHERE result.task_id=message.task_id AND result.sequence=message.output_result_event_sequence
  AND result.task_revision=message.output_task_revision AND result.event_type='task.result')
FROM conversation_messages message WHERE message.task_id=$1 AND message.owner_id=$2 AND message.conversation_id=$3 AND message.role='assistant'
ORDER BY message.sequence,message.id LIMIT $4`, taskRow.ID, taskRow.OwnerID, taskRow.ConversationID, workflowTraceFactLimit)
	if err != nil {
		return fmt.Errorf("read workflow assistant deliveries: %w", err)
	}
	for rows.Next() {
		var id, deliveryStatus, outputStatus, contentDigest string
		var sequence, revision, resultEventSequence, contentBytes int64
		var resultEventMatched bool
		var createdAt time.Time
		if err := rows.Scan(&id, &sequence, &deliveryStatus, &outputStatus, &revision, &resultEventSequence, &contentBytes, &contentDigest, &createdAt, &resultEventMatched); err != nil {
			rows.Close()
			return err
		}
		sourceID, err := builder.addSource(evaluation.WorkflowSourceConversationOutput, "db://conversation_messages/"+taskRow.ID+"/"+id, struct {
			MessageID           string    `json:"message_id"`
			Sequence            int64     `json:"sequence"`
			DeliveryStatus      string    `json:"delivery_status"`
			OutputStatus        string    `json:"output_status,omitempty"`
			TaskRevision        int64     `json:"output_task_revision"`
			ResultEventSequence int64     `json:"output_result_event_sequence,omitempty"`
			ContentBytes        int       `json:"content_bytes"`
			ContentSHA256       string    `json:"content_sha256"`
			CreatedAt           time.Time `json:"created_at"`
		}{id, sequence, deliveryStatus, outputStatus, revision, resultEventSequence, int(contentBytes), contentDigest, createdAt})
		if err != nil {
			rows.Close()
			return err
		}
		state := workflowDeliveryNodeState(deliveryStatus, outputStatus)
		nodeID := builder.addNode(evaluation.WorkflowNodeDelivery, state, taskRow.ID+":message:"+id, sourceID)
		deliveryTargets[id] = workflowTraceDeliveryObservationTarget{sourceID: sourceID, taskRevision: revision,
			pcEligible: deliveryStatus == "applied" && outputStatus == "completed" && revision >= 0 && resultEventSequence > 0 && resultEventMatched}
		if deliveryStatus == "applied" && (outputStatus == "completed" || outputStatus == "") {
			builder.addEdge(evaluation.WorkflowEdgeDelivered, taskNode, nodeID, sourceID)
		} else {
			builder.addGap(evaluation.WorkflowStageDelivery, "assistant output is not confirmed applied and complete: "+id, evaluation.WorkflowGapUnknown, nil, sourceID)
		}
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return fmt.Errorf("iterate workflow assistant deliveries: %w", rowsErr)
	}

	if err := tx.QueryRow(ctx, `SELECT count(*) FROM task_terminal_delivery_outbox WHERE task_id=$1 AND owner_id=$2`, taskRow.ID, taskRow.OwnerID).Scan(&count); err != nil {
		return fmt.Errorf("count workflow terminal delivery records: %w", err)
	}
	countSource, err = builder.addCountSource(evaluation.WorkflowSourceConversationOutput, "db://task_terminal_delivery_outbox/task/"+taskRow.ID+"/count", count)
	if err != nil {
		return err
	}
	if count == 0 {
		zero := 0
		builder.addGap(evaluation.WorkflowStageDelivery, "terminal delivery outbox records", evaluation.WorkflowGapAbsent, &zero, countSource)
	}
	workflowTruncatedGap(builder, evaluation.WorkflowStageDelivery, "terminal delivery outbox records", count, countSource)
	rows, err = tx.Query(ctx, `SELECT o.id,o.task_revision,o.authorization_version,o.source_kind,o.source_event_sequence,o.source_event_type,
encode(o.source_sha256,'hex'),COALESCE(encode(o.result_sha256,'hex'),''),COALESCE(encode(o.response_sha256,'hex'),''),o.state,COALESCE(o.model_call_id,''),
COALESCE(o.published_message_id,''),COALESCE(m.delivery_status,''),o.created_at,o.updated_at
FROM task_terminal_delivery_outbox o LEFT JOIN conversation_messages m ON m.id=o.published_message_id AND m.task_id=o.task_id
AND m.owner_id=o.owner_id AND m.conversation_id=o.conversation_id
WHERE o.task_id=$1 AND o.owner_id=$2 ORDER BY o.id LIMIT $3`, taskRow.ID, taskRow.OwnerID, workflowTraceFactLimit)
	if err != nil {
		return fmt.Errorf("read workflow terminal delivery records: %w", err)
	}
	for rows.Next() {
		var id, kind, eventType, sourceHash, resultHash, responseHash, state, callID, messageID, messageStatus string
		var revision, authVersion, eventSequence int64
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&id, &revision, &authVersion, &kind, &eventSequence, &eventType, &sourceHash, &resultHash, &responseHash, &state, &callID, &messageID, &messageStatus, &createdAt, &updatedAt); err != nil {
			rows.Close()
			return err
		}
		sourceID, err := builder.addSource(evaluation.WorkflowSourceConversationOutput, "db://task_terminal_delivery_outbox/"+taskRow.ID+"/"+id, struct {
			ID                    string    `json:"id"`
			TaskRevision          int64     `json:"task_revision"`
			AuthorizationVersion  int64     `json:"authorization_version"`
			SourceKind            string    `json:"source_kind"`
			SourceEventSequence   int64     `json:"source_event_sequence"`
			SourceEventType       string    `json:"source_event_type"`
			SourceSHA256          string    `json:"source_sha256"`
			ResultSHA256          string    `json:"result_sha256,omitempty"`
			ResponseSHA256        string    `json:"response_sha256,omitempty"`
			State                 string    `json:"state"`
			ModelCallID           string    `json:"model_call_id,omitempty"`
			PublishedMessageID    string    `json:"published_message_id,omitempty"`
			MessageDeliveryStatus string    `json:"message_delivery_status,omitempty"`
			CreatedAt             time.Time `json:"created_at"`
			UpdatedAt             time.Time `json:"updated_at"`
		}{id, revision, authVersion, kind, eventSequence, eventType, sourceHash, resultHash, responseHash, state, callID, messageID, messageStatus, createdAt, updatedAt})
		if err != nil {
			rows.Close()
			return err
		}
		nodeID := builder.addNode(evaluation.WorkflowNodeDelivery, workflowOutboxDeliveryState(state, messageStatus), taskRow.ID+":outbox:"+id, sourceID)
		if state == "published" && messageID != "" && messageStatus == "applied" {
			builder.addEdge(evaluation.WorkflowEdgeDelivered, taskNode, nodeID, sourceID)
		} else {
			builder.addGap(evaluation.WorkflowStageDelivery, "terminal delivery lacks a confirmed published conversation message: "+id, evaluation.WorkflowGapUnknown, nil, sourceID)
		}
		if authVersion != taskRow.AuthorizationVersion {
			builder.addGap(evaluation.WorkflowStageAuthorization, "terminal delivery uses a different persisted authorization version: "+id, evaluation.WorkflowGapUnknown, nil, sourceID)
		}
	}
	rowsErr = rows.Err()
	rows.Close()
	if rowsErr != nil {
		return fmt.Errorf("iterate workflow terminal delivery records: %w", rowsErr)
	}
	return addWorkflowTracePCDeliveryObservations(ctx, tx, builder, taskRow, deliveryTargets)
}

func addWorkflowTracePCDeliveryObservations(ctx context.Context, tx pgx.Tx, builder *workflowTraceBuilder,
	taskRow workflowTraceTask, deliveryTargets map[string]workflowTraceDeliveryObservationTarget) error {
	var count int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM task_pc_delivery_observations
WHERE task_id=$1 AND owner_id=$2 AND conversation_id=$3`, taskRow.ID, taskRow.OwnerID, taskRow.ConversationID).Scan(&count); err != nil {
		return fmt.Errorf("count workflow PC delivery observations: %w", err)
	}
	countSource, err := builder.addCountSource(evaluation.WorkflowSourcePCObservation,
		"db://task_pc_delivery_observations/task/"+taskRow.ID+"/count", count)
	if err != nil {
		return err
	}
	workflowTruncatedGap(builder, evaluation.WorkflowStageDelivery, "PC delivery observations", count, countSource)
	rows, err := tx.Query(ctx, `SELECT observation.client_installation_id,observation.message_id,observation.task_revision,observation.observed_at,
message.role,COALESCE(message.delivery_status,''),COALESCE(message.output_status,''),COALESCE(message.output_task_revision,-1),
COALESCE(message.output_result_event_sequence,0),COALESCE(result.sequence,0)
FROM task_pc_delivery_observations observation
LEFT JOIN conversation_messages message ON message.id=observation.message_id
  AND message.task_id=observation.task_id AND message.owner_id=observation.owner_id AND message.conversation_id=observation.conversation_id
LEFT JOIN task_events result ON result.task_id=message.task_id AND result.sequence=message.output_result_event_sequence
  AND result.task_revision=message.output_task_revision AND result.event_type='task.result'
WHERE observation.task_id=$1 AND observation.owner_id=$2 AND observation.conversation_id=$3
ORDER BY observation.observed_at,observation.client_installation_id,observation.message_id LIMIT $4`,
		taskRow.ID, taskRow.OwnerID, taskRow.ConversationID, workflowTraceFactLimit)
	if err != nil {
		return fmt.Errorf("read workflow PC delivery observations: %w", err)
	}
	observed := make(map[string]struct{})
	for rows.Next() {
		var installationID, messageID, role, deliveryStatus, outputStatus string
		var revision, messageRevision, resultEventSequence, matchedResultEventSequence int64
		var observedAt time.Time
		if err := rows.Scan(&installationID, &messageID, &revision, &observedAt, &role, &deliveryStatus, &outputStatus,
			&messageRevision, &resultEventSequence, &matchedResultEventSequence); err != nil {
			rows.Close()
			return err
		}
		sourceID, err := builder.addSource(evaluation.WorkflowSourcePCObservation,
			"db://task_pc_delivery_observations/"+taskRow.ID+"/"+messageID+"/"+installationID+"/"+fmt.Sprint(revision), struct {
				TaskID               string    `json:"task_id"`
				MessageID            string    `json:"message_id"`
				TaskRevision         int64     `json:"task_revision"`
				ClientInstallationID string    `json:"client_installation_id"`
				ObservedAt           time.Time `json:"observed_at"`
			}{taskRow.ID, messageID, revision, installationID, observedAt})
		if err != nil {
			rows.Close()
			return err
		}
		target, exists := deliveryTargets[messageID]
		if !exists || !target.pcEligible || revision != target.taskRevision || revision != messageRevision ||
			role != "assistant" || deliveryStatus != "applied" || outputStatus != "completed" ||
			resultEventSequence < 1 || resultEventSequence != matchedResultEventSequence {
			builder.addGap(evaluation.WorkflowStageDelivery, "PC observation does not match a published assistant Task result: "+messageID,
				evaluation.WorkflowGapUnknown, nil, sourceID)
			continue
		}
		builder.addNode(evaluation.WorkflowNodeDelivery, evaluation.WorkflowNodeCompleted,
			taskRow.ID+":message:"+messageID, sourceID)
		observed[messageID] = struct{}{}
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return fmt.Errorf("iterate workflow PC delivery observations: %w", rowsErr)
	}
	for messageID, target := range deliveryTargets {
		if target.pcEligible {
			if _, ok := observed[messageID]; !ok {
				builder.addGap(evaluation.WorkflowStageDelivery, "PC visible-area observation is absent for published reply: "+messageID,
					evaluation.WorkflowGapUnknown, nil, target.sourceID, countSource)
			}
		}
	}
	return nil
}

func workflowDeliveryNodeState(deliveryStatus, outputStatus string) evaluation.WorkflowNodeState {
	switch {
	case deliveryStatus == "applied" && (outputStatus == "completed" || outputStatus == ""):
		return evaluation.WorkflowNodeCompleted
	case deliveryStatus == "pending":
		return evaluation.WorkflowNodePending
	case deliveryStatus == "unknown" || outputStatus == "interrupted":
		return evaluation.WorkflowNodeUnknown
	default:
		return evaluation.WorkflowNodeUnavailable
	}
}

func workflowOutboxDeliveryState(state, messageStatus string) evaluation.WorkflowNodeState {
	switch {
	case state == "published" && messageStatus == "applied":
		return evaluation.WorkflowNodeCompleted
	case state == "pending" || state == "reply_ready":
		return evaluation.WorkflowNodePending
	case state == "claimed":
		return evaluation.WorkflowNodeRunning
	case state == "invalidated":
		return evaluation.WorkflowNodeCancelled
	case state == "unknown":
		return evaluation.WorkflowNodeUnknown
	default:
		return evaluation.WorkflowNodeUnavailable
	}
}

func addWorkflowTraceHost(ctx context.Context, tx pgx.Tx, builder *workflowTraceBuilder, taskRow workflowTraceTask, taskNode, taskSource string) error {
	if taskRow.HostID == "" {
		sourceID, err := builder.addCountSource(evaluation.WorkflowSourceHostSnapshot, "db://hosts/task/"+taskRow.ID+"/count", 0)
		if err != nil {
			return err
		}
		zero := 0
		builder.addGap(evaluation.WorkflowStageHost, "Task has no persisted host target", evaluation.WorkflowGapAbsent, &zero, sourceID)
		builder.addGap(evaluation.WorkflowStageHost, "live runtime snapshot is not part of the Host metadata adapter", evaluation.WorkflowGapUnavailable, nil, sourceID)
		return nil
	}
	var count int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM hosts WHERE id=$1`, taskRow.HostID).Scan(&count); err != nil {
		return fmt.Errorf("count workflow host snapshots: %w", err)
	}
	countSource, err := builder.addCountSource(evaluation.WorkflowSourceHostSnapshot, "db://hosts/"+taskRow.HostID+"/count", count)
	if err != nil {
		return err
	}
	if count == 0 {
		zero := 0
		builder.addGap(evaluation.WorkflowStageHost, "Task host snapshot is absent", evaluation.WorkflowGapAbsent, &zero, countSource)
		builder.addGap(evaluation.WorkflowStageHost, "live runtime snapshot is not part of the Host metadata adapter", evaluation.WorkflowGapUnavailable, nil, countSource)
		return nil
	}
	var id, ownerID, state, reason string
	var generation, controlVersion int64
	var updatedAt time.Time
	err = tx.QueryRow(ctx, `SELECT id,owner_id,state,state_reason,runtime_generation,control_version,updated_at FROM hosts WHERE id=$1`, taskRow.HostID).Scan(&id, &ownerID, &state, &reason, &generation, &controlVersion, &updatedAt)
	if err != nil {
		return fmt.Errorf("read workflow host snapshot: %w", err)
	}
	reasonDigest := sha256.Sum256([]byte(reason))
	sourceID, err := builder.addSource(evaluation.WorkflowSourceHostSnapshot, "db://hosts/"+id, struct {
		HostID            string    `json:"host_id"`
		OwnerID           string    `json:"owner_id"`
		State             string    `json:"state"`
		StateReasonSHA256 string    `json:"state_reason_sha256"`
		RuntimeGeneration int64     `json:"runtime_generation"`
		ControlVersion    int64     `json:"control_version"`
		UpdatedAt         time.Time `json:"updated_at"`
	}{id, ownerID, state, hex.EncodeToString(reasonDigest[:]), generation, controlVersion, updatedAt})
	if err != nil {
		return err
	}
	hostNode := builder.addNode(evaluation.WorkflowNodeHost, workflowHostNodeState(state), taskRow.HostID, sourceID)
	builder.addEdge(evaluation.WorkflowEdgeTargets, taskNode, hostNode, taskSource, sourceID)
	builder.addGap(evaluation.WorkflowStageHost, "live runtime snapshot is not part of the Host metadata adapter", evaluation.WorkflowGapUnavailable, nil, sourceID)
	return nil
}

func workflowHostNodeState(state string) evaluation.WorkflowNodeState {
	switch state {
	case "running":
		return evaluation.WorkflowNodeRunning
	case "creating":
		return evaluation.WorkflowNodePending
	case "failed", "deleted", "deleting":
		return evaluation.WorkflowNodeUnavailable
	default:
		return evaluation.WorkflowNodeUnknown
	}
}

func addWorkflowTraceRuntime(ctx context.Context, tx pgx.Tx, builder *workflowTraceBuilder, taskRow workflowTraceTask,
	taskNode, taskSource string) error {
	locator := "db://account_agent_runtimes/conversation/" + taskRow.ConversationID + "/current"
	var serviceInstanceID string
	if err := tx.QueryRow(ctx, `SELECT instance_id::text FROM service_identity WHERE singleton=true`).Scan(&serviceInstanceID); errors.Is(err, pgx.ErrNoRows) {
		sourceID, addErr := builder.addSource(evaluation.WorkflowSourceRuntimeSnapshot, locator, struct {
			Available bool `json:"available"`
		}{false})
		if addErr != nil {
			return addErr
		}
		builder.addGap(evaluation.WorkflowStageRuntime, "current account Runtime record is unavailable", evaluation.WorkflowGapUnavailable, nil, sourceID)
		return nil
	} else if err != nil {
		return fmt.Errorf("read workflow Runtime service identity: %w", err)
	}

	var runtimeID, state string
	var generation, runtimeSequence, controlVersion int64
	var leaseExpiresAt pgtype.Timestamptz
	var leaseActive bool
	var updatedAt time.Time
	err := tx.QueryRow(ctx, `SELECT runtime_id,runtime_generation,state,runtime_sequence,control_version,
lease_expires_at,COALESCE(lease_expires_at>transaction_timestamp(),false),updated_at
FROM account_agent_runtimes
WHERE service_instance_id=$1::uuid AND owner_id=$2 AND conversation_id=$3`,
		serviceInstanceID, taskRow.OwnerID, taskRow.ConversationID).Scan(
		&runtimeID, &generation, &state, &runtimeSequence, &controlVersion, &leaseExpiresAt, &leaseActive, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		sourceID, addErr := builder.addSource(evaluation.WorkflowSourceRuntimeSnapshot, locator, struct {
			Available         bool   `json:"available"`
			ServiceInstanceID string `json:"service_instance_id"`
		}{false, serviceInstanceID})
		if addErr != nil {
			return addErr
		}
		builder.addGap(evaluation.WorkflowStageRuntime, "current conversation Runtime snapshot is unavailable", evaluation.WorkflowGapUnavailable, nil, sourceID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read workflow Runtime snapshot: %w", err)
	}

	expiry := ""
	if leaseExpiresAt.Valid {
		expiry = leaseExpiresAt.Time.UTC().Format(time.RFC3339Nano)
	}
	sourceID, err := builder.addSource(evaluation.WorkflowSourceRuntimeSnapshot,
		"db://account_agent_runtimes/"+serviceInstanceID+"/"+runtimeID+"/"+fmt.Sprint(generation), struct {
			ServiceInstanceID string    `json:"service_instance_id"`
			ConversationID    string    `json:"conversation_id"`
			RuntimeID         string    `json:"runtime_id"`
			Generation        int64     `json:"runtime_generation"`
			State             string    `json:"state"`
			RuntimeSequence   int64     `json:"runtime_sequence"`
			ControlVersion    int64     `json:"control_version"`
			LeaseExpiresAt    string    `json:"lease_expires_at,omitempty"`
			LeaseActive       bool      `json:"lease_active"`
			UpdatedAt         time.Time `json:"updated_at"`
		}{serviceInstanceID, taskRow.ConversationID, runtimeID, generation, state, runtimeSequence,
			controlVersion, expiry, leaseActive, updatedAt})
	if err != nil {
		return err
	}
	runtimeNode := builder.addNode(evaluation.WorkflowNodeRuntime, workflowRuntimeNodeState(state, leaseActive),
		taskRow.ConversationID+":"+runtimeID+":"+fmt.Sprint(generation), sourceID)
	builder.addEdge(evaluation.WorkflowEdgeTargets, taskNode, runtimeNode, taskSource, sourceID)
	return nil
}

func addWorkflowTraceDSHSessionRuntimeSnapshot(ctx context.Context, tx pgx.Tx, builder *workflowTraceBuilder,
	taskRow workflowTraceTask, taskNode, taskSource string) error {
	locator := "db://task_agent_sessions/" + taskRow.ID + "/" + taskRow.AgentSessionID + "/current"
	if taskRow.AgentSessionID == "" || taskRow.AgentGeneration < 1 {
		return addWorkflowTraceDSHSessionRuntimeUnavailable(builder, taskRow, taskNode, taskSource, locator)
	}
	var activityStatus, activityPhase, activityState, agentState string
	var activityGeneration, activitySequence, activityRevision int64
	var activityObservedAt, heartbeatAt pgtype.Timestamptz
	var leaseActive bool
	err := tx.QueryRow(ctx, `SELECT COALESCE(session.activity_status,''),COALESCE(session.activity_generation,0),
COALESCE(session.activity_sequence,0),COALESCE(session.activity_revision,-1),COALESCE(session.activity_phase,''),
COALESCE(session.activity_state,''),session.activity_observed_at,session.activity_heartbeat_at,
COALESCE(instance.state,''),COALESCE(instance.lease_expires_at>transaction_timestamp(),false)
FROM task_agent_sessions session
LEFT JOIN task_agent_instances instance ON instance.task_id=session.task_id AND instance.owner_id=session.owner_id
  AND instance.agent_generation=session.activity_generation
WHERE session.task_id=$1 AND session.owner_id=$2 AND session.session_id=$3 AND session.parent_session_id IS NULL`,
		taskRow.ID, taskRow.OwnerID, taskRow.AgentSessionID).Scan(&activityStatus, &activityGeneration, &activitySequence,
		&activityRevision, &activityPhase, &activityState, &activityObservedAt, &heartbeatAt, &agentState, &leaseActive)
	if errors.Is(err, pgx.ErrNoRows) {
		return addWorkflowTraceDSHSessionRuntimeUnavailable(builder, taskRow, taskNode, taskSource, locator)
	}
	if err != nil {
		return fmt.Errorf("read workflow DSH session heartbeat snapshot: %w", err)
	}
	heartbeatFresh := heartbeatAt.Valid && !heartbeatAt.Time.After(builder.trace.CapturedAt) &&
		!heartbeatAt.Time.Before(builder.trace.CapturedAt.Add(-2*app.MaxAgentLease))
	identityMatches := activityGeneration == taskRow.AgentGeneration && activityRevision == taskRow.CurrentRevision &&
		activitySequence > 0 && (activityStatus == "idle" || activityStatus == "running") && heartbeatAt.Valid
	var activityObservedAtValue, heartbeatAtValue *time.Time
	if activityObservedAt.Valid {
		value := activityObservedAt.Time
		activityObservedAtValue = &value
	}
	if heartbeatAt.Valid {
		value := heartbeatAt.Time
		heartbeatAtValue = &value
	}
	snapshot := struct {
		TaskID                string     `json:"task_id"`
		SessionID             string     `json:"session_id"`
		TaskRevision          int64      `json:"task_revision"`
		AgentGeneration       int64      `json:"agent_generation"`
		ActivityGeneration    int64      `json:"activity_generation"`
		ActivityRevision      int64      `json:"activity_revision"`
		ActivitySequence      int64      `json:"activity_sequence"`
		ActivityStatus        string     `json:"activity_status,omitempty"`
		ActivityPhase         string     `json:"activity_phase,omitempty"`
		ActivityState         string     `json:"activity_state,omitempty"`
		ActivityObservedAt    *time.Time `json:"activity_observed_at,omitempty"`
		HeartbeatAt           *time.Time `json:"heartbeat_at,omitempty"`
		HeartbeatFresh        bool       `json:"heartbeat_fresh"`
		HeartbeatFreshnessSec int        `json:"heartbeat_freshness_seconds"`
		AgentInstanceState    string     `json:"agent_instance_state,omitempty"`
		AgentLeaseActive      bool       `json:"agent_lease_active"`
		IdentityMatches       bool       `json:"identity_matches_task"`
	}{
		TaskID: taskRow.ID, SessionID: taskRow.AgentSessionID, TaskRevision: taskRow.CurrentRevision,
		AgentGeneration: taskRow.AgentGeneration, ActivityGeneration: activityGeneration, ActivityRevision: activityRevision,
		ActivitySequence: activitySequence, ActivityStatus: activityStatus, ActivityPhase: activityPhase, ActivityState: activityState,
		HeartbeatFresh: heartbeatFresh, HeartbeatFreshnessSec: int((2 * app.MaxAgentLease).Seconds()),
		AgentInstanceState: agentState, AgentLeaseActive: leaseActive, IdentityMatches: identityMatches,
	}
	snapshot.ActivityObservedAt = activityObservedAtValue
	snapshot.HeartbeatAt = heartbeatAtValue
	sourceID, err := builder.addSource(evaluation.WorkflowSourceRuntimeSnapshot, locator, snapshot)
	if err != nil {
		return err
	}
	runtimeNode := builder.addNode(evaluation.WorkflowNodeRuntime, evaluation.WorkflowNodeUnknown,
		taskRow.ID+":dsh-session:"+taskRow.AgentSessionID+":"+fmt.Sprint(taskRow.AgentGeneration), sourceID)
	builder.addEdge(evaluation.WorkflowEdgeTargets, taskNode, runtimeNode, taskSource, sourceID)
	if !identityMatches {
		builder.addGap(evaluation.WorkflowStageRuntime, "root DSH session heartbeat does not exactly match the current Task revision and Agent generation",
			evaluation.WorkflowGapUnknown, nil, sourceID)
	} else if !heartbeatFresh {
		builder.addGap(evaluation.WorkflowStageRuntime, "root DSH session heartbeat is missing, stale or newer than the captured snapshot",
			evaluation.WorkflowGapUnknown, nil, sourceID)
	} else if agentState != "active" || !leaseActive {
		builder.addGap(evaluation.WorkflowStageRuntime, "recent root DSH session heartbeat conflicts with the Agent instance state or lease",
			evaluation.WorkflowGapUnknown, nil, sourceID)
	}
	builder.addGap(evaluation.WorkflowStageRuntime, "root DSH session heartbeat records recent runtime activity, not direct process-tree state or PID",
		evaluation.WorkflowGapUnknown, nil, sourceID)
	return nil
}

func addWorkflowTraceDSHSessionRuntimeUnavailable(builder *workflowTraceBuilder, taskRow workflowTraceTask,
	taskNode, taskSource, locator string) error {
	sourceID, err := builder.addSource(evaluation.WorkflowSourceRuntimeSnapshot, locator, struct {
		Available       bool   `json:"available"`
		TaskID          string `json:"task_id"`
		SessionID       string `json:"session_id,omitempty"`
		TaskRevision    int64  `json:"task_revision"`
		AgentGeneration int64  `json:"agent_generation"`
	}{false, taskRow.ID, taskRow.AgentSessionID, taskRow.CurrentRevision, taskRow.AgentGeneration})
	if err != nil {
		return err
	}
	nodeKey := taskRow.ID + ":dsh-session:root"
	if taskRow.AgentSessionID != "" {
		nodeKey = taskRow.ID + ":dsh-session:" + taskRow.AgentSessionID + ":" + fmt.Sprint(taskRow.AgentGeneration)
	}
	runtimeNode := builder.addNode(evaluation.WorkflowNodeRuntime, evaluation.WorkflowNodeUnavailable, nodeKey, sourceID)
	builder.addEdge(evaluation.WorkflowEdgeTargets, taskNode, runtimeNode, taskSource, sourceID)
	builder.addGap(evaluation.WorkflowStageRuntime, "root DSH session heartbeat snapshot is unavailable",
		evaluation.WorkflowGapUnavailable, nil, sourceID)
	return nil
}

func workflowRuntimeNodeState(state string, leaseActive bool) evaluation.WorkflowNodeState {
	if !leaseActive && (state == "starting" || state == "running" || state == "stopping") {
		return evaluation.WorkflowNodeUnknown
	}
	switch state {
	case "starting":
		return evaluation.WorkflowNodePending
	case "running":
		return evaluation.WorkflowNodeRunning
	case "stopping":
		return evaluation.WorkflowNodeWaiting
	case "stopped":
		return evaluation.WorkflowNodeUnavailable
	case "failed":
		return evaluation.WorkflowNodeFailed
	default:
		return evaluation.WorkflowNodeUnknown
	}
}
