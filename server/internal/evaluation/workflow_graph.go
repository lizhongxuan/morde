package evaluation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// WorkflowDataClass keeps real observations separate from scripted and
// synthetic evaluation data.
type WorkflowDataClass string

const (
	WorkflowDataSynthetic    WorkflowDataClass = "synthetic"
	WorkflowDataLiveScripted WorkflowDataClass = "live-scripted"
	WorkflowDataLiveObserved WorkflowDataClass = "live-observed"
)

type WorkflowSourceKind string

const (
	WorkflowSourceTaskLedger         WorkflowSourceKind = "task_ledger"
	WorkflowSourceAgentLedger        WorkflowSourceKind = "agent_ledger"
	WorkflowSourceModelCallLedger    WorkflowSourceKind = "model_call_ledger"
	WorkflowSourceOperationLedger    WorkflowSourceKind = "operation_ledger"
	WorkflowSourceApprovalLedger     WorkflowSourceKind = "approval_ledger"
	WorkflowSourceValidationLedger   WorkflowSourceKind = "validation_ledger"
	WorkflowSourceConversationOutput WorkflowSourceKind = "conversation_output"
	WorkflowSourcePCObservation      WorkflowSourceKind = "pc_observation"
	WorkflowSourceRuntimeSnapshot    WorkflowSourceKind = "runtime_snapshot"
	WorkflowSourceHostSnapshot       WorkflowSourceKind = "host_snapshot"
	WorkflowSourceRouteResult        WorkflowSourceKind = "route_result"
	WorkflowSourceLocalTest          WorkflowSourceKind = "local_test"
	WorkflowSourceCodeInspection     WorkflowSourceKind = "code_inspection"
	WorkflowSourceEvaluationRun      WorkflowSourceKind = "evaluation_run"
)

type WorkflowNodeKind string

const (
	WorkflowNodeRoute      WorkflowNodeKind = "route"
	WorkflowNodeTask       WorkflowNodeKind = "task"
	WorkflowNodeAgent      WorkflowNodeKind = "agent"
	WorkflowNodeModelCall  WorkflowNodeKind = "model_call"
	WorkflowNodeOperation  WorkflowNodeKind = "operation"
	WorkflowNodeApproval   WorkflowNodeKind = "approval"
	WorkflowNodeValidation WorkflowNodeKind = "validation"
	WorkflowNodeDelivery   WorkflowNodeKind = "delivery"
	WorkflowNodeHost       WorkflowNodeKind = "host"
)

type WorkflowNodeState string

const (
	WorkflowNodeStarted     WorkflowNodeState = "started"
	WorkflowNodeRunning     WorkflowNodeState = "running"
	WorkflowNodeSettled     WorkflowNodeState = "settled"
	WorkflowNodeCompleted   WorkflowNodeState = "completed"
	WorkflowNodeFailed      WorkflowNodeState = "failed"
	WorkflowNodeWaiting     WorkflowNodeState = "waiting"
	WorkflowNodeWaitingUser WorkflowNodeState = "waiting_user"
	WorkflowNodePending     WorkflowNodeState = "pending"
	WorkflowNodeApproved    WorkflowNodeState = "approved"
	WorkflowNodeRejected    WorkflowNodeState = "rejected"
	WorkflowNodeExpired     WorkflowNodeState = "expired"
	WorkflowNodeCancelled   WorkflowNodeState = "cancelled"
	WorkflowNodeUnknown     WorkflowNodeState = "unknown"
	WorkflowNodeUnavailable WorkflowNodeState = "unavailable"
)

type WorkflowEdgeKind string

const (
	WorkflowEdgeCreated   WorkflowEdgeKind = "created"
	WorkflowEdgeHandoff   WorkflowEdgeKind = "handoff"
	WorkflowEdgeInvoked   WorkflowEdgeKind = "invoked"
	WorkflowEdgeReturned  WorkflowEdgeKind = "returned"
	WorkflowEdgeApproved  WorkflowEdgeKind = "approved"
	WorkflowEdgeRejected  WorkflowEdgeKind = "rejected"
	WorkflowEdgeVerified  WorkflowEdgeKind = "verified"
	WorkflowEdgeDelivered WorkflowEdgeKind = "delivered"
	WorkflowEdgeResumed   WorkflowEdgeKind = "resumed"
	WorkflowEdgeRouted    WorkflowEdgeKind = "routed"
)

type WorkflowStage string

const (
	WorkflowStageRoute         WorkflowStage = "route"
	WorkflowStageTask          WorkflowStage = "task"
	WorkflowStageAgent         WorkflowStage = "agent"
	WorkflowStageModelCall     WorkflowStage = "model_call"
	WorkflowStageOperation     WorkflowStage = "operation"
	WorkflowStageApproval      WorkflowStage = "approval"
	WorkflowStageVerification  WorkflowStage = "verification"
	WorkflowStageDelivery      WorkflowStage = "delivery"
	WorkflowStageHost          WorkflowStage = "host"
	WorkflowStageAuthorization WorkflowStage = "authorization"
	WorkflowStageProvenance    WorkflowStage = "provenance"
)

type WorkflowGapState string

const (
	WorkflowGapAbsent      WorkflowGapState = "absent"
	WorkflowGapNotRun      WorkflowGapState = "not_run"
	WorkflowGapUnavailable WorkflowGapState = "unavailable"
	WorkflowGapUnknown     WorkflowGapState = "unknown"
)

type WorkflowCoverage string

const (
	WorkflowCoverageDeclaredComplete WorkflowCoverage = "declared_complete"
	WorkflowCoveragePartial          WorkflowCoverage = "partial"
	WorkflowCoverageUnknown          WorkflowCoverage = "unknown"
)

// WorkflowTrace is a source-bound graph of observed execution facts. Coverage
// describes the submitted source scope; declared_complete is not an
// independently verified completeness proof. The type is deliberately
// separate from Run and RunSummary: structural validity does not assign a
// quality score or establish that the Task succeeded.
type WorkflowTrace struct {
	Version           int               `json:"version"`
	TraceID           string            `json:"trace_id"`
	RunID             string            `json:"run_id,omitempty"`
	DataClass         WorkflowDataClass `json:"data_class"`
	CapturedAt        time.Time         `json:"captured_at"`
	Coverage          WorkflowCoverage  `json:"coverage"`
	CoverageSourceIDs []string          `json:"coverage_source_ids"`
	Sources           []WorkflowSource  `json:"sources"`
	Nodes             []WorkflowNode    `json:"nodes"`
	Edges             []WorkflowEdge    `json:"edges"`
	Gaps              []WorkflowGap     `json:"gaps"`
}

// WorkflowSource identifies the durable ledger, PC observation, or local
// artifact that supports a node, edge, or explicit coverage gap. Model prose
// is intentionally not a source kind.
type WorkflowSource struct {
	ID      string             `json:"id"`
	Kind    WorkflowSourceKind `json:"kind"`
	Locator string             `json:"locator"`
	SHA256  string             `json:"sha256,omitempty"`
}

type WorkflowNode struct {
	ID        string            `json:"id"`
	Kind      WorkflowNodeKind  `json:"kind"`
	State     WorkflowNodeState `json:"state"`
	SourceIDs []string          `json:"source_ids"`
}

type WorkflowEdge struct {
	ID        string           `json:"id"`
	From      string           `json:"from"`
	To        string           `json:"to"`
	Kind      WorkflowEdgeKind `json:"kind"`
	SourceIDs []string         `json:"source_ids"`
}

// WorkflowGap records a scoped fact that is absent, was not run, is
// unavailable, or remains unknown. Only an explicit zero with a source
// reference can be declared absent; validation does not inspect that source.
type WorkflowGap struct {
	ID        string           `json:"id"`
	Stage     WorkflowStage    `json:"stage"`
	Scope     string           `json:"scope"`
	State     WorkflowGapState `json:"state"`
	Count     *int             `json:"count,omitempty"`
	SourceIDs []string         `json:"source_ids"`
}

const (
	maxWorkflowSources    = 256
	maxWorkflowNodes      = 1024
	maxWorkflowEdges      = 2048
	maxWorkflowGaps       = 256
	maxWorkflowTraceBytes = 8 << 20
)

// LoadWorkflowTrace strictly decodes a versioned workflow trace and validates
// its graph structure before returning it to a renderer or report builder.
func LoadWorkflowTrace(raw []byte) (WorkflowTrace, error) {
	if len(raw) == 0 || len(raw) > maxWorkflowTraceBytes {
		return WorkflowTrace{}, errors.New("workflow trace size is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var trace WorkflowTrace
	if err := decoder.Decode(&trace); err != nil {
		return WorkflowTrace{}, fmt.Errorf("decode workflow trace: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return WorkflowTrace{}, errors.New("workflow trace has trailing JSON")
		}
		return WorkflowTrace{}, fmt.Errorf("decode workflow trace trailer: %w", err)
	}
	if err := ValidateWorkflowTrace(trace); err != nil {
		return WorkflowTrace{}, err
	}
	return trace, nil
}

// ValidateWorkflowTrace checks graph identities and source bindings. It does
// not query the referenced ledgers, validate an external source digest, or
// derive an overall success verdict.
func ValidateWorkflowTrace(trace WorkflowTrace) error {
	if trace.Version != 1 || !validWorkflowToken(trace.TraceID) || (trace.RunID != "" && !validWorkflowToken(trace.RunID)) ||
		!validWorkflowDataClass(trace.DataClass) || trace.CapturedAt.IsZero() ||
		!validWorkflowCoverage(trace.Coverage) || len(trace.Sources) == 0 || len(trace.Sources) > maxWorkflowSources ||
		len(trace.Nodes) == 0 || len(trace.Nodes) > maxWorkflowNodes || len(trace.Edges) > maxWorkflowEdges || len(trace.Gaps) > maxWorkflowGaps {
		return errors.New("workflow trace identity or collection sizes are invalid")
	}

	sources := make(map[string]WorkflowSource, len(trace.Sources))
	for _, source := range trace.Sources {
		if !validWorkflowToken(source.ID) || !validWorkflowSourceKind(source.Kind) ||
			!validBoundedText(source.Locator, 1024) || (source.SHA256 != "" && !validSHA256(source.SHA256)) ||
			(trace.DataClass == WorkflowDataLiveObserved && source.Kind == WorkflowSourceLocalTest) {
			return fmt.Errorf("workflow trace source %q is invalid", source.ID)
		}
		if _, duplicate := sources[source.ID]; duplicate {
			return fmt.Errorf("workflow trace source %q is duplicated", source.ID)
		}
		sources[source.ID] = source
	}

	nodes := make(map[string]WorkflowNode, len(trace.Nodes))
	for _, node := range trace.Nodes {
		if !validWorkflowToken(node.ID) || !validWorkflowNodeKind(node.Kind) || !validWorkflowNodeState(node.State) ||
			!workflowSourceIDsExist(node.SourceIDs, sources) || !workflowNodeSourcesMatch(trace.DataClass, node.Kind, node.SourceIDs, sources) {
			return fmt.Errorf("workflow trace node %q is invalid or lacks source bindings", node.ID)
		}
		if _, duplicate := nodes[node.ID]; duplicate {
			return fmt.Errorf("workflow trace node %q is duplicated", node.ID)
		}
		nodes[node.ID] = node
	}

	edgeIDs := make(map[string]struct{}, len(trace.Edges))
	for _, edge := range trace.Edges {
		from, fromExists := nodes[edge.From]
		to, toExists := nodes[edge.To]
		if !validWorkflowToken(edge.ID) || !validWorkflowEdgeKind(edge.Kind) || !fromExists || !toExists ||
			!workflowSourceIDsExist(edge.SourceIDs, sources) || !workflowEdgeSourcesMatch(trace.DataClass, edge.Kind, edge.SourceIDs, sources) {
			return fmt.Errorf("workflow trace edge %q is invalid or lacks source bindings", edge.ID)
		}
		if _, duplicate := edgeIDs[edge.ID]; duplicate {
			return fmt.Errorf("workflow trace edge %q is duplicated", edge.ID)
		}
		edgeIDs[edge.ID] = struct{}{}
		if edge.Kind == WorkflowEdgeHandoff && (from.Kind != WorkflowNodeAgent || to.Kind != WorkflowNodeAgent || edge.From == edge.To) {
			return fmt.Errorf("workflow handoff edge %q must connect distinct agents", edge.ID)
		}
	}

	gapIDs := make(map[string]struct{}, len(trace.Gaps))
	for _, gap := range trace.Gaps {
		if !validWorkflowToken(gap.ID) || !validWorkflowStage(gap.Stage) || !validBoundedText(gap.Scope, 512) ||
			!validWorkflowGapState(gap.State) || !workflowSourceIDsExist(gap.SourceIDs, sources) ||
			!workflowGapSourcesMatch(trace.DataClass, gap.Stage, gap.State, gap.SourceIDs, sources) {
			return fmt.Errorf("workflow trace gap %q is invalid or lacks source bindings", gap.ID)
		}
		if _, duplicate := gapIDs[gap.ID]; duplicate {
			return fmt.Errorf("workflow trace gap %q is duplicated", gap.ID)
		}
		gapIDs[gap.ID] = struct{}{}
		if gap.State == WorkflowGapAbsent {
			if gap.Count == nil || *gap.Count != 0 {
				return fmt.Errorf("workflow trace absent gap %q must bind a zero count", gap.ID)
			}
		} else if gap.Count != nil {
			return fmt.Errorf("workflow trace non-absent gap %q cannot claim a count", gap.ID)
		}
	}

	if !workflowSourceIDsExist(trace.CoverageSourceIDs, sources) {
		return errors.New("workflow trace coverage is not source-bound")
	}
	switch trace.Coverage {
	case WorkflowCoverageDeclaredComplete:
		if len(trace.Gaps) != 0 || len(trace.CoverageSourceIDs) == 0 {
			return errors.New("complete workflow trace requires scoped coverage sources and no gaps")
		}
	case WorkflowCoveragePartial, WorkflowCoverageUnknown:
		if len(trace.Gaps) == 0 {
			return errors.New("partial or unknown workflow trace must record its gaps")
		}
	}
	return nil
}

func workflowSourceIDsExist(ids []string, sources map[string]WorkflowSource) bool {
	if len(ids) == 0 {
		return false
	}
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if !validWorkflowToken(id) {
			return false
		}
		if _, duplicate := seen[id]; duplicate {
			return false
		}
		if _, exists := sources[id]; !exists {
			return false
		}
		seen[id] = struct{}{}
	}
	return true
}

func workflowNodeSourcesMatch(dataClass WorkflowDataClass, kind WorkflowNodeKind, ids []string, sources map[string]WorkflowSource) bool {
	switch kind {
	case WorkflowNodeRoute:
		return workflowSourceIDsMatch(dataClass, ids, sources, WorkflowSourceRouteResult, WorkflowSourcePCObservation)
	case WorkflowNodeTask:
		return workflowSourceIDsMatch(dataClass, ids, sources, WorkflowSourceTaskLedger, WorkflowSourcePCObservation)
	case WorkflowNodeAgent:
		return workflowSourceIDsMatch(dataClass, ids, sources, WorkflowSourceAgentLedger, WorkflowSourcePCObservation)
	case WorkflowNodeModelCall:
		return workflowSourceIDsMatch(dataClass, ids, sources, WorkflowSourceModelCallLedger)
	case WorkflowNodeOperation:
		return workflowSourceIDsMatch(dataClass, ids, sources, WorkflowSourceOperationLedger)
	case WorkflowNodeApproval:
		return workflowSourceIDsMatch(dataClass, ids, sources, WorkflowSourceApprovalLedger)
	case WorkflowNodeValidation:
		return workflowSourceIDsMatch(dataClass, ids, sources, WorkflowSourceValidationLedger)
	case WorkflowNodeDelivery:
		return workflowSourceIDsMatch(dataClass, ids, sources, WorkflowSourceConversationOutput, WorkflowSourcePCObservation)
	case WorkflowNodeHost:
		return workflowSourceIDsMatch(dataClass, ids, sources, WorkflowSourceHostSnapshot, WorkflowSourceRuntimeSnapshot, WorkflowSourcePCObservation)
	default:
		return false
	}
}

func workflowEdgeSourcesMatch(dataClass WorkflowDataClass, kind WorkflowEdgeKind, ids []string, sources map[string]WorkflowSource) bool {
	var allowed []WorkflowSourceKind
	switch kind {
	case WorkflowEdgeCreated, WorkflowEdgeResumed:
		allowed = []WorkflowSourceKind{WorkflowSourceTaskLedger, WorkflowSourceAgentLedger, WorkflowSourceOperationLedger}
	case WorkflowEdgeHandoff:
		allowed = []WorkflowSourceKind{WorkflowSourceAgentLedger}
	case WorkflowEdgeInvoked, WorkflowEdgeReturned:
		allowed = []WorkflowSourceKind{WorkflowSourceModelCallLedger, WorkflowSourceOperationLedger}
	case WorkflowEdgeApproved, WorkflowEdgeRejected:
		allowed = []WorkflowSourceKind{WorkflowSourceApprovalLedger}
	case WorkflowEdgeVerified:
		allowed = []WorkflowSourceKind{WorkflowSourceValidationLedger}
	case WorkflowEdgeDelivered:
		allowed = []WorkflowSourceKind{WorkflowSourceConversationOutput}
	case WorkflowEdgeRouted:
		allowed = []WorkflowSourceKind{WorkflowSourceRouteResult}
	default:
		return false
	}
	return workflowSourceIDsMatch(dataClass, ids, sources, allowed...)
}

func workflowGapSourcesMatch(dataClass WorkflowDataClass, stage WorkflowStage, state WorkflowGapState, ids []string,
	sources map[string]WorkflowSource) bool {
	if state != WorkflowGapAbsent {
		return true
	}
	var allowed []WorkflowSourceKind
	switch stage {
	case WorkflowStageRoute:
		allowed = []WorkflowSourceKind{WorkflowSourceRouteResult}
	case WorkflowStageTask:
		allowed = []WorkflowSourceKind{WorkflowSourceTaskLedger}
	case WorkflowStageAgent:
		allowed = []WorkflowSourceKind{WorkflowSourceAgentLedger}
	case WorkflowStageModelCall:
		allowed = []WorkflowSourceKind{WorkflowSourceModelCallLedger}
	case WorkflowStageOperation:
		allowed = []WorkflowSourceKind{WorkflowSourceOperationLedger}
	case WorkflowStageApproval:
		allowed = []WorkflowSourceKind{WorkflowSourceApprovalLedger}
	case WorkflowStageVerification:
		allowed = []WorkflowSourceKind{WorkflowSourceValidationLedger}
	case WorkflowStageDelivery:
		allowed = []WorkflowSourceKind{WorkflowSourceConversationOutput}
	case WorkflowStageHost:
		allowed = []WorkflowSourceKind{WorkflowSourceHostSnapshot}
	case WorkflowStageAuthorization:
		allowed = []WorkflowSourceKind{WorkflowSourceTaskLedger, WorkflowSourceApprovalLedger}
	case WorkflowStageProvenance:
		allowed = []WorkflowSourceKind{WorkflowSourceEvaluationRun}
	default:
		return false
	}
	return workflowSourceIDsMatch(dataClass, ids, sources, allowed...)
}

func workflowSourceIDsMatch(dataClass WorkflowDataClass, ids []string, sources map[string]WorkflowSource,
	allowed ...WorkflowSourceKind) bool {
	if len(allowed) == 0 {
		return false
	}
	for _, id := range ids {
		source := sources[id]
		if source.Kind == WorkflowSourceLocalTest && dataClass != WorkflowDataLiveObserved {
			return true
		}
		for _, kind := range allowed {
			if source.Kind == kind {
				return true
			}
		}
	}
	return false
}

func validWorkflowDataClass(value WorkflowDataClass) bool {
	return value == WorkflowDataSynthetic || value == WorkflowDataLiveScripted || value == WorkflowDataLiveObserved
}

func validWorkflowSourceKind(value WorkflowSourceKind) bool {
	switch value {
	case WorkflowSourceTaskLedger, WorkflowSourceAgentLedger, WorkflowSourceModelCallLedger,
		WorkflowSourceOperationLedger, WorkflowSourceApprovalLedger, WorkflowSourceValidationLedger,
		WorkflowSourceConversationOutput, WorkflowSourcePCObservation, WorkflowSourceRuntimeSnapshot,
		WorkflowSourceHostSnapshot, WorkflowSourceRouteResult, WorkflowSourceLocalTest,
		WorkflowSourceCodeInspection, WorkflowSourceEvaluationRun:
		return true
	default:
		return false
	}
}

func validWorkflowNodeKind(value WorkflowNodeKind) bool {
	switch value {
	case WorkflowNodeRoute, WorkflowNodeTask, WorkflowNodeAgent, WorkflowNodeModelCall,
		WorkflowNodeOperation, WorkflowNodeApproval, WorkflowNodeValidation, WorkflowNodeDelivery, WorkflowNodeHost:
		return true
	default:
		return false
	}
}

func validWorkflowNodeState(value WorkflowNodeState) bool {
	switch value {
	case WorkflowNodeStarted, WorkflowNodeRunning, WorkflowNodeSettled, WorkflowNodeCompleted,
		WorkflowNodeFailed, WorkflowNodeWaiting, WorkflowNodeWaitingUser, WorkflowNodePending,
		WorkflowNodeApproved, WorkflowNodeRejected, WorkflowNodeExpired, WorkflowNodeCancelled,
		WorkflowNodeUnknown, WorkflowNodeUnavailable:
		return true
	default:
		return false
	}
}

func validWorkflowEdgeKind(value WorkflowEdgeKind) bool {
	switch value {
	case WorkflowEdgeCreated, WorkflowEdgeHandoff, WorkflowEdgeInvoked, WorkflowEdgeReturned,
		WorkflowEdgeApproved, WorkflowEdgeRejected, WorkflowEdgeVerified, WorkflowEdgeDelivered,
		WorkflowEdgeResumed, WorkflowEdgeRouted:
		return true
	default:
		return false
	}
}

func validWorkflowStage(value WorkflowStage) bool {
	switch value {
	case WorkflowStageRoute, WorkflowStageTask, WorkflowStageAgent, WorkflowStageModelCall,
		WorkflowStageOperation, WorkflowStageApproval, WorkflowStageVerification, WorkflowStageDelivery,
		WorkflowStageHost, WorkflowStageAuthorization, WorkflowStageProvenance:
		return true
	default:
		return false
	}
}

func validWorkflowGapState(value WorkflowGapState) bool {
	switch value {
	case WorkflowGapAbsent, WorkflowGapNotRun, WorkflowGapUnavailable, WorkflowGapUnknown:
		return true
	default:
		return false
	}
}

func validWorkflowCoverage(value WorkflowCoverage) bool {
	return value == WorkflowCoverageDeclaredComplete || value == WorkflowCoveragePartial || value == WorkflowCoverageUnknown
}

func validWorkflowToken(value string) bool {
	if value == "" || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("-_.:/", char) {
			continue
		}
		return false
	}
	return true
}
