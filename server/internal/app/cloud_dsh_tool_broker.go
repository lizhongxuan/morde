package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

// CloudDSHToolBroker routes dsh calls through Morde's durable task gates. It does not run a model loop.
type CloudDSHToolBroker struct {
	store          TaskStore
	runtime        *RuntimeToolExecutor
	mcpCredentials MCPConnectorCredentialProvider
	mcpClient      MCPConnectorToolClient
}

type CloudDSHTaskCompletion struct {
	AgentInstanceID           string
	AgentGeneration           int64
	AuthorizationVersion      int64
	AgentSessionID            string
	CheckpointThroughSequence int64
	CheckpointDigest          []byte
	Result                    string
	Inputs                    []ModelInput
}

func NewCloudDSHToolBroker(store TaskStore, runtime *RuntimeToolExecutor, mcpCredentials MCPConnectorCredentialProvider, mcpClient MCPConnectorToolClient) (*CloudDSHToolBroker, error) {
	if store == nil || runtime == nil {
		return nil, errors.New("dsh tool broker dependencies are required")
	}
	if (mcpCredentials == nil) != (mcpClient == nil) {
		return nil, errors.New("MCP connector execution dependencies must be configured together")
	}
	return &CloudDSHToolBroker{store: store, runtime: runtime, mcpCredentials: mcpCredentials, mcpClient: mcpClient}, nil
}

func (b *CloudDSHToolBroker) Definitions(ctx context.Context, claimed task.Task) ([]AgentToolDefinition, error) {
	definitions := make([]AgentToolDefinition, 0)
	if claimed.HostID != "" {
		hostDefinitions, err := b.runtime.Definitions(ctx, claimed)
		if err != nil {
			return nil, err
		}
		definitions = append(definitions, hostDefinitions...)
	}
	mcpDefinitions, err := b.mcpDefinitions(ctx, claimed)
	if err != nil {
		return nil, err
	}
	definitions = append(definitions, mcpDefinitions...)
	if claimed.ExecutionLocation == "server" {
		details, err := b.currentServerTask(ctx, claimed)
		if err != nil {
			return nil, err
		}
		if b.authorizeSkillTask(ctx, details.Task) == nil {
			definitions = append(definitions, cloudDSHSkillReadDefinition())
		}
	}
	if len(definitions)+1 > maxCloudDSHToolDefinitions {
		return nil, domain.NewError(domain.CodeResultTooLarge, "the task has more tools than the model can accept")
	}
	seen := make(map[string]struct{}, len(definitions)+1)
	for _, definition := range definitions {
		if definition.Name == agentAskUserToolName {
			return nil, errors.New("host tool conflicts with the Agent clarification tool")
		}
		if _, exists := seen[definition.Name]; exists {
			return nil, errors.New("cloud dsh tools contain a duplicate name")
		}
		seen[definition.Name] = struct{}{}
	}
	return append(definitions, agentAskUserTool), nil
}

// Complete only commits a dsh result after the worker has confirmed its
// process tree stopped. Host tasks must also prove file writes through the
// operation ledger, Host runtime, and committed Morde Workspace.
func (b *CloudDSHToolBroker) Complete(ctx context.Context, claimed task.Task, completion CloudDSHTaskCompletion) error {
	if ctx == nil || len(completion.CheckpointDigest) != 32 || completion.CheckpointThroughSequence < 0 ||
		len(completion.Inputs) > int(task.MaxToolCalls)*2 || len(completion.Result) > agentUserVisibleResultBytes {
		return domain.InvalidArgument("dsh_completion", "cloud dsh completion evidence is invalid")
	}
	inputBytes := len(completion.Result)
	for _, input := range completion.Inputs {
		inputBytes += len(input.Arguments) + len(input.Output) + len(input.CallID) + len(input.Name) + len(input.Type)
		if inputBytes > 12<<20 {
			return domain.InvalidArgument("dsh_completion", "cloud dsh completion evidence exceeds its limit")
		}
	}
	agent := &Agent{store: b.store}
	details, err := agent.refreshTask(ctx, claimed)
	if err != nil {
		return err
	}
	current := details.Task
	if current.Status != task.StatusRunning || current.ExecutionLocation != "server" ||
		current.AgentInstanceID != completion.AgentInstanceID || current.AgentGeneration != completion.AgentGeneration ||
		current.Authorization.Version != completion.AuthorizationVersion || current.AgentSessionID != completion.AgentSessionID ||
		current.CheckpointThroughSequence != completion.CheckpointThroughSequence ||
		!bytes.Equal(current.CheckpointDigest, completion.CheckpointDigest) {
		return domain.NewError(domain.CodeInvalidState, "cloud dsh completion checkpoint changed")
	}
	fileIntent := current.FileDeliveryIntent
	if !fileIntent.Valid() {
		fileIntent = ClassifyFileDeliveryIntent(current.Objective, current.WorkspaceID != "")
	}
	if current.HostID == "" {
		if cloudDSHHasHostToolHistory(completion.Inputs) {
			return domain.NewError(domain.CodeInvalidState, "hostless dsh task contains computer tool history")
		}
		if fileIntent != task.FileDeliveryNone {
			return b.pauseForFileDelivery(ctx, current, details.Steps, fileIntent, false)
		}
		step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "result", task.StepSucceeded,
			"Complete cloud task", "The cloud dsh session completed and its process tree stopped.")
		if err != nil {
			return err
		}
		_, err = agent.completeWithArtifacts(ctx, current, step, current.Usage, completion.Result,
			map[string]any{"source": "cloud_dsh"}, "", nil)
		return err
	}

	evidence, evidenceErr := agentFileWriteEvidence(completion.Inputs, nil)
	if evidenceErr == nil && !cloudDSHHasUnverifiedFileWrite(completion.Inputs, evidence) && len(evidence) == 0 {
		if fileIntent != task.FileDeliveryNone {
			return b.pauseForFileDelivery(ctx, current, details.Steps, fileIntent, current.WorkspaceID != "")
		}
		step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "result", task.StepSucceeded,
			"Complete cloud task", "The cloud dsh session completed without producing a Workspace file.")
		if err != nil {
			return err
		}
		_, err = agent.completeWithArtifacts(ctx, current, step, current.Usage, completion.Result,
			map[string]any{"source": "cloud_dsh"}, "", nil)
		return err
	}
	step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "verification", task.StepRunning,
		"Verify cloud task output", "Checking successful file writes against the Host and committed Workspace.")
	if err != nil {
		return err
	}
	if err := b.commitProgress(ctx, current, step, current.Usage, task.StatusRunning, "", "task.step", map[string]any{
		"phase": "verify", "source": "cloud_dsh",
	}); err != nil {
		return err
	}
	if evidenceErr != nil || cloudDSHHasUnverifiedFileWrite(completion.Inputs, evidence) || len(evidence) == 0 {
		return b.pauseCompletion(ctx, current, step, "output_verification_pending", "The task output has no verifiable successful Workspace file write.")
	}
	fence, err := b.runtime.readFence(ctx, current, nil)
	if err != nil {
		return b.pauseCompletion(ctx, current, step, "output_verification_pending", "Host authorization or control changed before output verification.")
	}
	verification, verifyErr := b.runtime.Verify(ctx, AgentVerificationRequest{
		Task: current, StepID: step.ID, Claim: completion.Result, Evidence: evidence, Fence: fence,
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if verifyErr != nil || verification.Status != AgentVerified || validateAgentVerificationArtifacts(verification) != nil || len(verification.Artifacts) == 0 {
		return b.pauseCompletion(ctx, current, step, "output_verification_pending", "Host or committed Workspace could not verify the task output.")
	}
	artifacts, err := agent.persistVerifiedArtifacts(ctx, current, step, nil, verification.Artifacts)
	if err != nil {
		return err
	}
	step.Status = task.StepSucceeded
	step.Summary = safeStepSummary(verification.Summary, "Host and committed Workspace verified the task output.")
	_, err = agent.completeWithArtifacts(ctx, current, step, current.Usage, completion.Result,
		map[string]any{"source": "cloud_dsh", "verification": verification.Summary}, "", artifacts)
	return err
}

// shortcut: fixed phrase matching can miss uncommon wording, upgrade when broader language coverage is required.
func ClassifyFileDeliveryIntent(objective string, hasWorkspace bool) task.FileDeliveryIntent {
	text := strings.ToLower(strings.TrimSpace(objective))
	if text == "" {
		return task.FileDeliveryUnclear
	}
	noFileRequested := false
	for _, phrase := range []string{"不要生成文件", "不要创建文件", "不需要生成文件", "无需生成文件", "不用生成文件", "do not create a file", "don't create a file", "do not generate a file", "don't generate a file"} {
		if strings.Contains(text, phrase) {
			noFileRequested = true
			text = strings.ReplaceAll(text, phrase, " ")
		}
	}
	noFileRequested = noFileRequested || containsAny(text, "不需要文件", "无需文件", "不用文件", "不要文件", "无需附件", "只要文字", "只需要文字", "直接回答", "直接回复文字", "在聊天里回答", "no file needed", "no need for a file", "no attachment", "no file", "without a file", "text only", "text-only", "just answer", "answer in chat", "reply in chat")
	outputAction := containsAny(text, "生成", "创建", "新建", "制作", "导出", "下载", "保存", "另存", "交付", "提供", "输出", "上传", "写", "撰写", "整理成", "draft", "generate", "create", "make", "produce", "export", "download", "save", "deliver", "attach", "provide", "output", "write", "prepare")
	fileTarget := containsAny(text, "文件", "文档", "附件", "下载", "file", "document", "attachment", "download", ".pdf", ".csv", ".xlsx", ".docx", ".pptx", " pdf", " csv", " xlsx", " docx", " pptx", "excel", "word", "powerpoint", "ppt", "spreadsheet", "workbook", "presentation", "slide deck")
	workspaceDestination := containsAny(text, "在工作区", "到工作区", "写入工作区", "保存到工作区", "保存至工作区", "in the workspace", "in the attached workspace", "in the cloud workspace", "to the workspace", "to the attached workspace", "to the cloud workspace", "into the workspace", "into the attached workspace", "into the cloud workspace")
	if outputAction && workspaceDestination {
		if noFileRequested {
			return task.FileDeliveryUnclear
		}
		return task.FileDeliveryRequired
	}
	if outputAction && fileTarget {
		if noFileRequested {
			return task.FileDeliveryUnclear
		}
		return task.FileDeliveryRequired
	}
	deliverable := containsAny(text, "报告", "方案", "计划", "总结", "论文", "表格", "幻灯片", "演示文稿", "代码", "脚本", "源码", "项目", "仓库", "report", "plan", "summary", "spreadsheet", "slides", "code", "script", "project", "repository", "repo", "deliverable", "artifact")
	editAction := containsAny(text, "修改", "修复", "实现", "开发", "重构", "编辑", "更新", "改造", "fix", "implement", "develop", "refactor", "edit", "modify", "update")
	codeTarget := containsAny(text, "代码", "源码", "脚本", "code", "source", "script")
	projectTarget := containsAny(text, "项目", "仓库", "project", "repository", "repo")
	if hasWorkspace && ((editAction && (codeTarget || projectTarget)) || (outputAction && codeTarget)) {
		if noFileRequested {
			return task.FileDeliveryUnclear
		}
		return task.FileDeliveryRequired
	}
	if noFileRequested {
		return task.FileDeliveryNone
	}
	if (outputAction || editAction) && deliverable {
		return task.FileDeliveryUnclear
	}
	if fileTarget && !containsAny(text, "读取", "阅读", "查看", "审阅", "检查", "分析", "总结", "解释", "read", "review", "inspect", "analyze", "summarize", "explain") {
		return task.FileDeliveryUnclear
	}
	if outputAction {
		return task.FileDeliveryUnclear
	}
	return task.FileDeliveryNone
}

func containsAny(text string, values ...string) bool {
	for _, value := range values {
		if strings.Contains(text, value) {
			return true
		}
	}
	return false
}

func (b *CloudDSHToolBroker) pauseForFileDelivery(ctx context.Context, current task.Task, steps []task.Step, intent task.FileDeliveryIntent, hasWorkspace bool) error {
	step, err := newAgentStep(current.ID, maxStepSequence(steps)+1, "verification", task.StepRunning,
		"Check requested file delivery", "")
	if err != nil {
		return err
	}
	summary := "The task text does not clearly say whether a file is required; confirm before completion."
	reason := "file_delivery_unclear"
	if intent == task.FileDeliveryRequired {
		summary = "The task requests a file deliverable, but no verified Workspace file was produced."
		reason = "file_delivery_required"
		if !hasWorkspace {
			summary = "The task requests a file deliverable, but no Host Workspace is available to store it."
		}
	}
	return b.pauseCompletion(ctx, current, step, reason, summary)
}

func cloudDSHHasUnverifiedFileWrite(inputs []ModelInput, evidence []AgentFileEvidence) bool {
	attempts := make(map[string]int)
	for _, input := range inputs {
		if input.Type == "function_call" && input.Name == runtimeToolFileWrite {
			attempts[input.CallID]++
		}
	}
	if len(attempts) == 0 {
		return len(evidence) != 0
	}
	callByOperation := make(map[string]string, len(attempts))
	for callID, count := range attempts {
		if callID == "" || count != 1 {
			return true
		}
		callByOperation[agentOperationKey(callID)] = callID
	}
	verified := make(map[string]struct{}, len(evidence))
	for _, item := range evidence {
		callID, exists := callByOperation[item.OperationKey]
		if !exists {
			return true
		}
		if _, duplicate := verified[callID]; duplicate {
			return true
		}
		verified[callID] = struct{}{}
	}
	return len(verified) != len(attempts)
}

func cloudDSHHasHostToolHistory(inputs []ModelInput) bool {
	for _, input := range inputs {
		if input.Type != "function_call" {
			continue
		}
		if input.Name == agentAskUserToolName || input.Name == cloudDSHSkillReadToolName || strings.HasPrefix(input.Name, cloudDSHMCPToolPrefix) {
			continue
		}
		return true
	}
	return false
}

func (b *CloudDSHToolBroker) pauseCompletion(ctx context.Context, current task.Task, step task.Step, reason, summary string) error {
	step.Status, step.Summary = task.StepUnknown, summary
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentPersistenceTimeout)
	defer cancel()
	return b.commitProgress(persistCtx, current, step, current.Usage, task.StatusWaitingUser, reason, "task.waiting_user", map[string]any{
		"phase": "verify", "source": "cloud_dsh",
	})
}

func (b *CloudDSHToolBroker) Execute(ctx context.Context, claimed task.Task, call ModelToolCall) (AgentToolResult, error) {
	return b.execute(ctx, claimed, call, false, nil)
}

// Reconcile handles a tool call reconstructed from a pinned dsh session. An
// existing operation is read from the same durable ledger without consuming
// another tool-call budget unit. A missing result may be run only when no
// operation exists; a checkpointed result must match a terminal ledger result.
func (b *CloudDSHToolBroker) Reconcile(ctx context.Context, claimed task.Task, call ModelToolCall, checkpointOutput *string) (AgentToolResult, error) {
	return b.execute(ctx, claimed, call, true, checkpointOutput)
}

func (b *CloudDSHToolBroker) execute(ctx context.Context, claimed task.Task, call ModelToolCall, recovering bool, checkpointOutput *string) (AgentToolResult, error) {
	if ctx == nil || call.CallID == "" || len(call.CallID) > 256 || len(call.Name) == 0 || len(call.Name) > 128 {
		return AgentToolResult{}, domain.InvalidArgument("tool_call", "dsh tool call identity is invalid")
	}
	if call.Name == agentAskUserToolName {
		return b.clarify(ctx, claimed, call, recovering)
	}
	if call.Name == cloudDSHSkillReadToolName {
		return b.executeSkillRead(ctx, claimed, call, recovering, checkpointOutput)
	}
	if b.mcpClient != nil && strings.HasPrefix(call.Name, cloudDSHMCPToolPrefix) {
		details, err := b.currentServerTask(ctx, claimed)
		if err != nil {
			return AgentToolResult{}, err
		}
		arguments, err := validateAgentToolArguments(call.Arguments)
		if err != nil {
			return AgentToolResult{}, domain.InvalidArgument("tool_call.arguments", "dsh tool arguments are invalid")
		}
		definitions, err := b.mcpDefinitions(ctx, details.Task)
		if err != nil {
			return AgentToolResult{}, err
		}
		for _, definition := range definitions {
			if definition.Name == call.Name && definition.RequiredScope == identity.ScopeMCPControl {
				return b.executeMCP(ctx, details, call, arguments, definition, recovering, checkpointOutput)
			}
		}
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "MCP connector tool is not available for this task")
	}
	if recovering {
		result, handled, err := b.reconcileCheckpointedCall(ctx, claimed, call, checkpointOutput)
		if err != nil || handled {
			return result, err
		}
	}
	current, err := b.runtime.currentTask(ctx, claimed)
	if err != nil {
		return AgentToolResult{}, err
	}
	arguments, err := validateAgentToolArguments(call.Arguments)
	if err != nil {
		return AgentToolResult{}, domain.InvalidArgument("tool_call.arguments", "dsh tool arguments are invalid")
	}
	definitions, err := b.Definitions(ctx, current)
	if err != nil {
		return AgentToolResult{}, err
	}
	var definition AgentToolDefinition
	for _, candidate := range definitions {
		if candidate.Name == call.Name {
			definition = candidate
			break
		}
	}
	if definition.Name == "" {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "dsh tool is not available for this Host")
	}
	fence, err := b.runtime.readFence(ctx, current, nil)
	if err != nil {
		return AgentToolResult{}, err
	}
	workspaceID := ""
	if mapping, ok := runtimeToolMappingByName(call.Name); ok {
		workspaceID = runtimeToolWorkspaceID(mapping, arguments)
	}
	request := AgentToolRequest{
		Task: current, Call: call, Arguments: arguments, OperationKey: agentOperationKey(call.CallID),
		RequiredScope: definition.RequiredScope, Fence: fence, WorkspaceID: workspaceID,
	}
	hash, err := agentOperationHash(call.Name, arguments)
	if err != nil {
		return AgentToolResult{}, err
	}
	details, err := b.store.GetTask(ctx, current.ID, current.OwnerID)
	if err != nil {
		return AgentToolResult{}, err
	}
	pendingApproval := details.PendingRequest
	approvalCall := pendingApproval != nil && pendingApproval.CallID == call.CallID
	if approvalCall && (pendingApproval.TaskID != current.ID || pendingApproval.Kind != "approval" || pendingApproval.ToolName != call.Name ||
		!bytes.Equal(pendingApproval.RequestHash, hash[:]) ||
		(pendingApproval.Status == task.PendingRequestApproved && pendingApproval.Decision != task.PendingDecisionApprove) ||
		(pendingApproval.Status == task.PendingRequestRejected && pendingApproval.Decision != task.PendingDecisionReject)) {
		return b.commitApprovalMismatch(ctx, current, call.Name, details.Steps)
	}
	usage := cloneTaskUsage(current.Usage)
	step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "tool", task.StepRunning,
		"Run cloud dsh tool: "+call.Name, "Checking current authorization, budget, and Host control.")
	if err != nil {
		return AgentToolResult{}, err
	}
	if approvalCall && pendingApproval.Status == task.PendingRequestApproved {
		request.Approval = &AgentToolApprovalDecision{PendingID: pendingApproval.ID, Version: pendingApproval.Version,
			Decision: task.PendingDecisionApprove, RequestHash: append([]byte(nil), hash[:]...)}
	}
	recoveryOperation := false
	if recovering {
		operation, lookupErr := b.store.GetOperation(ctx, current.OwnerID, current.ID, request.OperationKey)
		switch {
		case domain.CodeOf(lookupErr) == domain.CodeNotFound:
			if checkpointOutput != nil {
				return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "A checkpointed tool result has no matching operation ledger entry.")
			}
		case lookupErr != nil:
			return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The prior tool operation could not be read safely.")
		default:
			if operation.ID == "" || operation.TaskID != current.ID || operation.Key != request.OperationKey || operation.HostID != current.HostID ||
				!bytes.Equal(operation.RequestHash, hash[:]) {
				return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The stored tool operation does not match this exact checkpointed request.")
			}
			recoveryOperation = true
		}
	}
	if approvalCall && pendingApproval.Status == task.PendingRequestRejected {
		if recoveryOperation {
			return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "A rejected approval has a conflicting operation ledger entry.")
		}
		return b.rejectApprovedToolCall(ctx, current, step, usage, request)
	}
	if approvalCall && pendingApproval.Status == task.PendingRequestPending {
		step.Status, step.Title, step.Summary = task.StepWaitingUser, "Approval required: "+call.Name, pendingApproval.Content
		if err := b.commitProgress(ctx, current, step, usage, task.StatusWaitingUser, "approval_required", "task.waiting_user", AgentApprovalRequest{
			Object: pendingApproval.Object, Content: pendingApproval.Content, Impact: pendingApproval.Impact,
		}); err != nil {
			return AgentToolResult{}, err
		}
		return AgentToolResult{Status: AgentToolNeedsUser}, nil
	}
	if !recoveryOperation && !approvalCall && usage.ToolCalls >= current.Budget.ToolCalls {
		step.Status, step.Summary = task.StepWaitingUser, "The task tool call budget is exhausted."
		if err := b.commitProgress(ctx, current, step, usage, task.StatusWaitingUser, "tool_call_budget", "task.waiting_user", nil); err != nil {
			return AgentToolResult{}, err
		}
		return AgentToolResult{Status: AgentToolNeedsUser}, nil
	}
	if !recoveryOperation && !approvalCall {
		usage.ToolCalls++
		if err := b.commitProgress(ctx, current, step, usage, task.StatusRunning, "", "task.step", map[string]any{
			"tool": call.Name, "operation_key": request.OperationKey, "phase": "prepare",
		}); err != nil {
			return AgentToolResult{}, err
		}
	}
	current.Usage = usage
	request.Task = current
	preparation, err := b.runtime.Prepare(ctx, request)
	if err != nil {
		if recoveryOperation {
			return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The prior tool operation is no longer authorized for safe recovery.")
		}
		if domain.CodeOf(err) == domain.CodeWorkspaceConflict {
			return b.pauseForWorkspaceInputChange(ctx, current, step, usage, call.Name)
		}
		return b.failBeforeDispatch(ctx, current, step, usage, call.Name, err)
	}
	if preparation.Status == AgentToolApprovalRequired {
		return b.pauseForApproval(ctx, current, step, usage, request, hash[:], preparation, details.PendingRequest)
	}
	if preparation.Status != AgentToolReady || !sameAgentControlFence(preparation.Fence, fence) {
		return b.failBeforeDispatch(ctx, current, step, usage, call.Name, domain.NewError(domain.CodeInvalidState, "Host control fence changed"))
	}
	request.Fence, request.WorkspaceID = preparation.Fence, preparation.WorkspaceID
	operationID, err := domain.NewOperationID()
	if err != nil {
		return AgentToolResult{}, fmt.Errorf("generate dsh operation ID: %w", err)
	}
	var inputRevision *int64
	if preparation.WorkspaceID != "" {
		inputRevision = current.InputRevision
	}
	operation, replayed, err := b.store.CreateOperation(ctx, task.Operation{
		ID: operationID, TaskID: current.ID, Key: request.OperationKey, RequestHash: hash[:], HostID: current.HostID,
		WorkspaceID: preparation.WorkspaceID, BindingVersion: preparation.BindingVersion,
		InputRevision:     inputRevision,
		RuntimeGeneration: request.Fence.RuntimeGeneration, RuntimeInstanceID: request.Fence.RuntimeInstanceID,
		ControlVersion: request.Fence.ControlVersion, LeaseVersion: current.Lease.Version,
		RuntimeLeaseVersion: current.Lease.Version, Status: task.OperationPrepared,
	}, current.Lease.Owner)
	if err != nil {
		if recoveryOperation {
			return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The stored tool operation could not be matched safely.")
		}
		if domain.CodeOf(err) == domain.CodeWorkspaceConflict {
			return b.pauseForWorkspaceInputChange(ctx, current, step, usage, call.Name)
		}
		return b.failBeforeDispatch(ctx, current, step, usage, call.Name, err)
	}
	if recovering && recoveryOperation && !replayed {
		return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The exact prior operation disappeared during recovery.")
	}
	if operation.ID == "" || operation.HostID != current.HostID || operation.WorkspaceID != preparation.WorkspaceID ||
		operation.BindingVersion != preparation.BindingVersion || !bytes.Equal(operation.RequestHash, hash[:]) {
		return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The stored tool operation does not match this exact request.")
	}
	request.OperationID = operation.ID
	if replayed {
		var result AgentToolResult
		var terminal bool
		operation, result, terminal, err = b.reconcile(ctx, request, operation, step, usage)
		if err != nil || terminal {
			if terminal && err == nil && result.Status != AgentToolUnknown && result.Status != AgentToolNeedsUser {
				if recovering && checkpointOutput != nil && *checkpointOutput != dshToolResultText(result) {
					return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The checkpointed tool result differs from the exact ledger result.")
				}
				return b.commitResult(ctx, current, step, usage, call.Name, result)
			}
			return result, err
		}
	}
	if recovering && checkpointOutput != nil {
		return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The checkpointed tool result is not backed by a terminal ledger entry.")
	}
	if operation.Status != task.OperationPrepared {
		return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The prior tool operation cannot be proven safe to dispatch again.")
	}
	if operation.LeaseVersion != current.Lease.Version {
		operation, err = b.store.UpdateOperation(ctx, OperationUpdate{
			TaskID: current.ID, OperationKey: request.OperationKey, LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
			RuntimeGeneration: request.Fence.RuntimeGeneration, RuntimeInstanceID: request.Fence.RuntimeInstanceID,
			ControlVersion: request.Fence.ControlVersion, RuntimeLeaseVersion: current.Lease.Version,
			AdoptPrepared: true, Status: task.OperationPrepared,
		})
		if err != nil {
			return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The prepared tool operation could not be safely adopted.")
		}
	}
	if _, err := b.store.UpdateOperation(ctx, OperationUpdate{
		TaskID: current.ID, OperationKey: request.OperationKey, LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
		RuntimeGeneration: request.Fence.RuntimeGeneration, RuntimeInstanceID: request.Fence.RuntimeInstanceID,
		ControlVersion: request.Fence.ControlVersion, RuntimeLeaseVersion: current.Lease.Version, Status: task.OperationDispatched,
	}); err != nil {
		return AgentToolResult{}, err
	}
	result, executeErr := b.runtime.Execute(ctx, request, preparation)
	if executeErr != nil || result.Status == AgentToolUnknown || len(result.Output) > maxAgentToolOutputBytes ||
		!validCloudDSHToolImages(call.Name, result) {
		if err := b.markUnknown(ctx, current, request.OperationKey); err != nil {
			return AgentToolResult{}, errors.Join(executeErr, err)
		}
		return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The tool result is unknown; the action will not be replayed.")
	}
	if result.Status == AgentToolNeedsUser {
		if _, err := b.store.UpdateOperation(ctx, OperationUpdate{
			TaskID: current.ID, OperationKey: request.OperationKey, LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
			Status: task.OperationFailed, ExternalID: boundedExternalID(result.ExternalID), Result: boundedToolOutput(result.Output),
		}); err != nil {
			return AgentToolResult{}, err
		}
		reason := result.WaitingReason
		var message string
		switch reason {
		case "workspace_input_changed":
			message = "Workspace changed after this task was submitted. Submit a new task to use the updated files."
		case "desktop_focus_changed":
			message = "The target application lost focus. No input was sent; focus the target application before continuing."
		default:
			reason, message = "tool_needs_user", "The Host requires user attention before the task can continue."
		}
		step.Status, step.Summary = task.StepWaitingUser, message
		if err := b.commitProgress(ctx, current, step, usage, task.StatusWaitingUser, reason, "task.waiting_user", map[string]any{
			"tool": call.Name, "operation_key": request.OperationKey, "message": message,
		}); err != nil {
			return AgentToolResult{}, err
		}
		return result, nil
	}
	if result.Status != AgentToolSucceeded && result.Status != AgentToolFailed {
		if err := b.markUnknown(ctx, current, request.OperationKey); err != nil {
			return AgentToolResult{}, err
		}
		return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The tool returned an unsupported result; the action will not be replayed.")
	}
	operationStatus, stepStatus := task.OperationSucceeded, task.StepSucceeded
	summary := "The cloud dsh tool completed and its result was saved."
	if result.Status == AgentToolFailed {
		operationStatus, stepStatus = task.OperationFailed, task.StepFailed
		summary = "The Host runtime reported a definitive tool failure."
	}
	output := boundedToolOutput(result.Output)
	if _, err := b.store.UpdateOperation(ctx, OperationUpdate{
		TaskID: current.ID, OperationKey: request.OperationKey, LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
		Status: operationStatus, ExternalID: boundedExternalID(result.ExternalID), Result: output,
	}); err != nil {
		if persistErr := b.markUnknown(ctx, current, request.OperationKey); persistErr != nil {
			return AgentToolResult{}, errors.Join(err, persistErr)
		}
		return b.pauseUnknown(ctx, current, step, usage, request.OperationKey, "The Host action completed but its result could not be committed safely.")
	}
	step.Status, step.Summary = stepStatus, summary
	if err := b.commitProgress(ctx, current, step, usage, task.StatusRunning, "", "task.step", map[string]any{
		"tool": call.Name, "operation_key": request.OperationKey, "operation_status": operationStatus, "output": output,
	}); err != nil {
		return AgentToolResult{}, err
	}
	return AgentToolResult{Status: result.Status, ExternalID: result.ExternalID, Output: output, Images: result.Images}, nil
}

func (b *CloudDSHToolBroker) pauseForWorkspaceInputChange(ctx context.Context, current task.Task, step task.Step, usage task.Usage, tool string) (AgentToolResult, error) {
	message := "Workspace changed after this task was submitted. Submit a new task to use the updated files."
	step.Status, step.Summary = task.StepWaitingUser, message
	if err := b.commitProgress(ctx, current, step, usage, task.StatusWaitingUser, "workspace_input_changed", "task.waiting_user", map[string]any{
		"tool": tool, "message": message,
	}); err != nil {
		return AgentToolResult{}, err
	}
	return AgentToolResult{Status: AgentToolNeedsUser, WaitingReason: "workspace_input_changed", Output: message}, nil
}

func validCloudDSHToolImages(name string, result AgentToolResult) bool {
	if result.Status != AgentToolSucceeded {
		return len(result.Images) == 0
	}
	if name != runtimeToolDesktopScreenshot && name != runtimeToolBrowserScreenshot {
		return len(result.Images) == 0
	}
	return len(result.Images) == 1 && result.Images[0].MediaType == "image/jpeg" &&
		len(result.Images[0].Data) >= 4 && len(result.Images[0].Data) <= 512<<10
}

func (b *CloudDSHToolBroker) clarify(ctx context.Context, claimed task.Task, call ModelToolCall, recovering bool) (AgentToolResult, error) {
	question, err := parseAgentClarificationQuestion(call.Arguments)
	if err != nil || !utf8.ValidString(call.CallID) {
		return AgentToolResult{}, domain.InvalidArgument("clarification_request", "dsh clarification call is invalid")
	}
	details, err := b.store.GetTask(ctx, claimed.ID, claimed.OwnerID)
	if err != nil {
		return AgentToolResult{}, err
	}
	current := details.Task
	if current.ID != claimed.ID || current.OwnerID != claimed.OwnerID || current.HostID != claimed.HostID {
		return AgentToolResult{Status: AgentToolUnknown}, nil
	}
	var matched *task.ClarificationRequest
	var hasPending bool
	for i := range details.ClarificationRequests {
		request := &details.ClarificationRequests[i]
		hasPending = hasPending || request.Status == task.ClarificationPending
		if request.TaskID != current.ID || request.CallID != call.CallID {
			continue
		}
		if matched != nil || request.Question != question {
			return AgentToolResult{Status: AgentToolUnknown}, nil
		}
		matched = request
	}
	if matched != nil {
		switch matched.Status {
		case task.ClarificationPending:
			if matched.Answer != "" || matched.RespondedAt != nil {
				return AgentToolResult{Status: AgentToolUnknown}, nil
			}
			return AgentToolResult{Status: AgentToolNeedsUser}, nil
		case task.ClarificationAnswered:
			if matched.Answer == "" || matched.RespondedAt == nil || !utf8.ValidString(matched.Answer) || utf8.RuneCountInString(matched.Answer) > 16384 ||
				current.Status != task.StatusRunning || current.Lease.Owner != claimed.Lease.Owner ||
				current.Lease.Version != claimed.Lease.Version || current.Lease.ExpiresAt == nil || !current.Lease.ExpiresAt.After(time.Now()) {
				return AgentToolResult{Status: AgentToolUnknown}, nil
			}
			return AgentToolResult{Status: AgentToolSucceeded, Output: matched.Answer}, nil
		default:
			return AgentToolResult{Status: AgentToolUnknown}, nil
		}
	}
	if recovering || hasPending {
		return AgentToolResult{Status: AgentToolUnknown}, nil
	}
	if current.Status != task.StatusRunning || current.Lease.Owner == "" || current.Lease.Owner != claimed.Lease.Owner ||
		current.Lease.Version != claimed.Lease.Version || current.Lease.ExpiresAt == nil || !current.Lease.ExpiresAt.After(time.Now()) {
		return AgentToolResult{Status: AgentToolUnknown}, nil
	}
	sequence := maxStepSequence(details.Steps)
	if current.Usage.ToolCalls >= current.Budget.ToolCalls {
		if sequence == int64(^uint64(0)>>1) {
			return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "task step sequence is exhausted")
		}
		step, err := newAgentStep(current.ID, sequence+1, "budget", task.StepWaitingUser,
			"Tool call budget reached", "The task is paused before another tool call.")
		if err != nil {
			return AgentToolResult{}, err
		}
		if err := b.commitProgress(ctx, current, step, current.Usage, task.StatusWaitingUser, "tool_call_budget", "task.waiting_user", nil); err != nil {
			return AgentToolResult{}, err
		}
		return AgentToolResult{Status: AgentToolNeedsUser}, nil
	}
	if sequence > int64(^uint64(0)>>1)-2 {
		return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "task step sequence is exhausted")
	}
	version := nextClarificationVersion(details.ClarificationRequests)
	if version < 1 {
		return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "clarification version is exhausted")
	}
	id, err := domain.NewPendingRequestID()
	if err != nil {
		return AgentToolResult{}, fmt.Errorf("generate dsh clarification ID: %w", err)
	}
	modelStep, err := newAgentStep(current.ID, sequence+1, "model", task.StepSucceeded,
		"DSH model asked for clarification", "The DSH model paused before continuing the task.")
	if err != nil {
		return AgentToolResult{}, err
	}
	step, err := newAgentStep(current.ID, sequence+2, "clarification", task.StepWaitingUser,
		"The Agent needs one detail", question)
	if err != nil {
		return AgentToolResult{}, err
	}
	request := &task.ClarificationRequest{
		ID: id, TaskID: current.ID, Version: version, ModelStepID: modelStep.ID,
		CallID: call.CallID, Question: question, Status: task.ClarificationPending,
	}
	usage := cloneTaskUsage(current.Usage)
	usage.ToolCalls++
	payload, err := agentStepEvent(step, usage, map[string]any{
		"clarification_id": request.ID, "clarification_version": request.Version,
		"call_id": call.CallID, "source": "cloud_dsh",
	})
	if err != nil {
		return AgentToolResult{}, err
	}
	_, err = b.store.CommitTaskProgress(ctx, TaskProgress{
		TaskID: current.ID, LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
		Status: task.StatusWaitingUser, CurrentStep: step.ID, Result: current.Result,
		WaitingReason: "clarification_required", Usage: usage,
		Steps: []task.Step{modelStep, step}, Event: task.Event{TaskID: current.ID, Type: "task.waiting_user", Payload: payload},
		ClarificationRequest: request,
	})
	if err != nil {
		return AgentToolResult{}, err
	}
	return AgentToolResult{Status: AgentToolNeedsUser}, nil
}

// reconcileCheckpointedCall resolves an operation already present in the
// durable Morde ledger without running ordinary Prepare or charging another
// tool call. It returns handled=false only when a fresh call or a previously
// prepared operation can continue through the normal budgeted path.
func (b *CloudDSHToolBroker) reconcileCheckpointedCall(ctx context.Context, claimed task.Task, call ModelToolCall, checkpointOutput *string) (AgentToolResult, bool, error) {
	current, err := b.runtime.currentTaskForOperationReconciliation(ctx, claimed)
	if err != nil {
		return AgentToolResult{}, true, err
	}
	arguments, err := validateAgentToolArguments(call.Arguments)
	if err != nil {
		return AgentToolResult{}, true, domain.InvalidArgument("tool_call.arguments", "dsh tool arguments are invalid")
	}
	mapping, ok := runtimeToolMappingByName(call.Name)
	if !ok || !validRuntimeToolArguments(mapping, arguments) {
		return AgentToolResult{}, true, domain.NewError(domain.CodeForbidden, "dsh tool is not available for this Host")
	}
	operationKey := agentOperationKey(call.CallID)
	hash, err := agentOperationHash(call.Name, arguments)
	if err != nil {
		return AgentToolResult{}, true, err
	}
	details, err := b.store.GetTask(ctx, current.ID, current.OwnerID)
	if err != nil {
		return AgentToolResult{}, true, err
	}
	step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "tool", task.StepRunning,
		"Recover cloud dsh tool: "+call.Name, "Checking the exact prior Host operation and current authorization.")
	if err != nil {
		return AgentToolResult{}, true, err
	}
	usage := cloneTaskUsage(current.Usage)
	pause := func(summary string) (AgentToolResult, bool, error) {
		result, err := b.pauseUnknown(ctx, current, step, usage, operationKey, summary)
		return result, true, err
	}
	pendingApproval := details.PendingRequest
	if pendingApproval != nil && pendingApproval.CallID == call.CallID {
		if pendingApproval.TaskID != current.ID || pendingApproval.Kind != "approval" || pendingApproval.ToolName != call.Name ||
			!bytes.Equal(pendingApproval.RequestHash, hash[:]) {
			return pause("The saved approval does not match this exact checkpointed tool call.")
		}
		if pendingApproval.Status == task.PendingRequestRejected {
			if pendingApproval.Decision != task.PendingDecisionReject {
				return pause("The saved rejection decision is invalid.")
			}
			result := AgentToolResult{Status: AgentToolFailed, Output: "The user rejected this exact action. It was not executed."}
			if checkpointOutput != nil && *checkpointOutput != dshToolResultText(result) {
				return pause("The checkpointed rejection result differs from the saved user decision.")
			}
			_, lookupErr := b.store.GetOperation(ctx, current.OwnerID, current.ID, operationKey)
			if domain.CodeOf(lookupErr) != domain.CodeNotFound {
				return pause("A rejected approval has a conflicting operation ledger entry.")
			}
			if checkpointOutput == nil {
				step.Status, step.Summary = task.StepFailed, "The user rejected this exact action; it was not dispatched."
				if err := b.commitProgress(ctx, current, step, usage, task.StatusRunning, "", "task.step", map[string]any{
					"tool": call.Name, "operation_key": operationKey, "operation_status": result.Status, "approval_decision": "reject",
				}); err != nil {
					return AgentToolResult{}, true, err
				}
			}
			return result, true, nil
		}
	}
	operation, err := b.store.GetOperation(ctx, current.OwnerID, current.ID, operationKey)
	if domain.CodeOf(err) == domain.CodeNotFound {
		if checkpointOutput != nil {
			return pause("A checkpointed tool result has no matching operation ledger entry.")
		}
		return AgentToolResult{}, false, nil
	}
	if err != nil {
		return pause("The prior tool operation could not be read safely.")
	}
	workspaceID := runtimeToolWorkspaceID(mapping, arguments)
	if operation.ID == "" || !domain.ValidOperationID(operation.ID) || operation.TaskID != current.ID || operation.Key != operationKey ||
		operation.HostID != current.HostID || operation.WorkspaceID != workspaceID || !bytes.Equal(operation.RequestHash, hash[:]) {
		return pause("The stored tool operation does not match this exact checkpointed request.")
	}
	if operation.Status == task.OperationPrepared {
		if checkpointOutput != nil {
			return pause("A checkpointed tool result is not backed by a dispatched ledger operation.")
		}
		return AgentToolResult{}, false, nil
	}
	request := AgentToolRequest{
		Task: current, Call: call, Arguments: arguments, OperationKey: operationKey, OperationID: operation.ID,
		RequiredScope: mapping.scope, WorkspaceID: workspaceID,
	}
	if operation.Status == task.OperationDispatched {
		operation, err = b.store.UpdateOperation(ctx, OperationUpdate{
			TaskID: current.ID, OperationKey: operationKey, LeaseOwner: current.Lease.Owner,
			LeaseVersion: current.Lease.Version, Status: task.OperationUnknown,
		})
		if err != nil {
			return pause("The dispatched operation could not be durably marked for reconciliation.")
		}
	}
	if operation.Status != task.OperationUnknown && operation.Status != task.OperationSucceeded && operation.Status != task.OperationFailed {
		return pause("The prior tool operation is not in a recoverable ledger state.")
	}
	operation, result, terminal, err := b.reconcile(ctx, request, operation, step, usage)
	if err != nil || !terminal {
		return result, true, err
	}
	if result.Status == AgentToolUnknown || result.Status == AgentToolNeedsUser {
		return result, true, nil
	}
	if checkpointOutput != nil && *checkpointOutput != dshToolResultText(result) {
		return pause("The checkpointed tool result differs from the exact ledger result.")
	}
	result, err = b.commitResult(ctx, current, step, usage, call.Name, result)
	return result, true, err
}

func dshToolResultText(result AgentToolResult) string {
	if result.Status == AgentToolFailed {
		return "Tool failed: " + result.Output
	}
	return result.Output
}

// PauseRecovery moves an unresolved restored session out of the worker claim
// queue. It records no checkpoint contents or tool arguments in the task event.
func (b *CloudDSHToolBroker) PauseRecovery(ctx context.Context, claimed task.Task) error {
	if ctx == nil {
		return domain.InvalidArgument("context", "recovery context is required")
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentPersistenceTimeout)
	defer cancel()
	details, err := b.store.GetTask(persistCtx, claimed.ID, claimed.OwnerID)
	if err != nil {
		return err
	}
	current := details.Task
	if current.Status != task.StatusRunning || current.Lease.Owner != claimed.Lease.Owner || current.Lease.Version != claimed.Lease.Version ||
		current.Lease.ExpiresAt == nil || !current.Lease.ExpiresAt.After(time.Now()) {
		return nil
	}
	step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "verification", task.StepUnknown,
		"Review cloud task recovery", "The previous cloud task checkpoint could not be reconciled safely.")
	if err != nil {
		return err
	}
	return b.commitProgress(persistCtx, current, step, current.Usage, task.StatusWaitingUser, "dsh_recovery_unresolved", "task.waiting_user", map[string]any{
		"phase": "recovery", "source": "cloud_dsh",
	})
}

func (b *CloudDSHToolBroker) commitResult(ctx context.Context, current task.Task, step task.Step, usage task.Usage, name string, result AgentToolResult) (AgentToolResult, error) {
	step.Status, step.Summary = task.StepSucceeded, "The cloud dsh tool completed and its result was saved."
	if result.Status == AgentToolFailed {
		step.Status, step.Summary = task.StepFailed, "The Host runtime reported a definitive tool failure."
	}
	if err := b.commitProgress(ctx, current, step, usage, task.StatusRunning, "", "task.step", map[string]any{
		"tool": name, "operation_status": result.Status, "output": boundedToolOutput(result.Output), "reused": true,
	}); err != nil {
		return AgentToolResult{}, err
	}
	result.Output = boundedToolOutput(result.Output)
	return result, nil
}

func (b *CloudDSHToolBroker) reconcile(ctx context.Context, request AgentToolRequest, operation task.Operation, step task.Step, usage task.Usage) (task.Operation, AgentToolResult, bool, error) {
	if operation.Status == task.OperationDispatched {
		var err error
		operation, err = b.store.UpdateOperation(ctx, OperationUpdate{
			TaskID: request.Task.ID, OperationKey: request.OperationKey, LeaseOwner: request.Task.Lease.Owner,
			LeaseVersion: request.Task.Lease.Version, Status: task.OperationUnknown,
		})
		if err != nil {
			return task.Operation{}, AgentToolResult{}, false, err
		}
	}
	if operation.Status == task.OperationUnknown || operation.Status == task.OperationSucceeded || operation.Status == task.OperationFailed {
		result, err := b.runtime.ReconcileOperation(ctx, request, operation)
		if err != nil || result.Status == AgentToolUnknown {
			paused, pauseErr := b.pauseUnknown(ctx, request.Task, step, usage, request.OperationKey, "The prior operation could not be safely reconciled.")
			return operation, paused, true, errors.Join(err, pauseErr)
		}
		if result.Status == AgentToolNotFound {
			if operation.Status != task.OperationUnknown {
				paused, pauseErr := b.pauseUnknown(ctx, request.Task, step, usage, request.OperationKey, "The terminal operation no longer matches the authenticated Host runtime.")
				return operation, paused, true, pauseErr
			}
			operation, err = b.store.UpdateOperation(ctx, OperationUpdate{
				TaskID: request.Task.ID, OperationKey: request.OperationKey, LeaseOwner: request.Task.Lease.Owner,
				LeaseVersion: request.Task.Lease.Version, RuntimeGeneration: operation.RuntimeGeneration,
				RuntimeInstanceID: operation.RuntimeInstanceID, ControlVersion: operation.ControlVersion,
				RuntimeLeaseVersion: request.Task.Lease.Version, RearmAfterNotFound: true, Status: task.OperationPrepared,
			})
			if err != nil {
				paused, pauseErr := b.pauseUnknown(ctx, request.Task, step, usage, request.OperationKey, "The confirmed absent operation could not be safely rearmed.")
				return task.Operation{}, paused, true, errors.Join(err, pauseErr)
			}
			return operation, AgentToolResult{}, false, nil
		}
		if operation.Status == task.OperationSucceeded || operation.Status == task.OperationFailed {
			wantStatus := AgentToolSucceeded
			if operation.Status == task.OperationFailed {
				wantStatus = AgentToolFailed
			}
			if (result.Status != wantStatus && result.Status != AgentToolNeedsUser) ||
				!validCloudDSHToolImages(request.Call.Name, result) {
				paused, pauseErr := b.pauseUnknown(ctx, request.Task, step, usage, request.OperationKey, "The terminal ledger result changed during recovery.")
				return operation, paused, true, pauseErr
			}
			return operation, result, true, nil
		}
		operationStatus := task.OperationSucceeded
		if result.Status == AgentToolFailed {
			operationStatus = task.OperationFailed
		} else if result.Status != AgentToolSucceeded || len(result.Output) > maxAgentToolOutputBytes ||
			!validCloudDSHToolImages(request.Call.Name, result) {
			paused, pauseErr := b.pauseUnknown(ctx, request.Task, step, usage, request.OperationKey, "The reconciled operation result is unsupported.")
			return task.Operation{}, paused, true, pauseErr
		}
		operation, err = b.store.UpdateOperation(ctx, OperationUpdate{
			TaskID: request.Task.ID, OperationKey: request.OperationKey, LeaseOwner: request.Task.Lease.Owner,
			LeaseVersion: request.Task.Lease.Version, Status: operationStatus,
			ExternalID: boundedExternalID(result.ExternalID), Result: boundedToolOutput(result.Output),
		})
		if err != nil {
			return task.Operation{}, AgentToolResult{}, false, err
		}
		result.Output = boundedToolOutput(result.Output)
		return operation, result, true, nil
	}
	return operation, AgentToolResult{}, false, nil
}

func (b *CloudDSHToolBroker) failBeforeDispatch(ctx context.Context, current task.Task, step task.Step, usage task.Usage, name string, cause error) (AgentToolResult, error) {
	step.Status, step.Summary = task.StepFailed, "The Host tool request could not be prepared."
	if err := b.commitProgress(ctx, current, step, usage, task.StatusRunning, "", "task.step", map[string]any{
		"tool": name, "phase": "prepare", "failed": true,
	}); err != nil {
		return AgentToolResult{}, errors.Join(cause, err)
	}
	return AgentToolResult{Status: AgentToolFailed, Output: "The Host tool request was not executed because authorization or Host readiness could not be confirmed."}, nil
}

func (b *CloudDSHToolBroker) pauseForApproval(ctx context.Context, current task.Task, step task.Step, usage task.Usage, request AgentToolRequest, hash []byte, preparation AgentToolPreparation, previous *task.PendingRequest) (AgentToolResult, error) {
	version := int64(1)
	if previous != nil {
		version = previous.Version + 1
	}
	pending, err := newPendingApproval(current, request.Call, hash, preparation.Approval, preparation.Fence, version)
	if err != nil {
		return AgentToolResult{}, err
	}
	step.Status, step.Title, step.Summary = task.StepWaitingUser, "Approval required: "+request.Call.Name, preparation.Approval.Content
	if err := b.commitProgress(ctx, current, step, usage, task.StatusWaitingUser, "approval_required", "task.waiting_user", preparation.Approval, pending); err != nil {
		return AgentToolResult{}, err
	}
	return AgentToolResult{Status: AgentToolNeedsUser}, nil
}

func (b *CloudDSHToolBroker) commitApprovalMismatch(ctx context.Context, current task.Task, name string, steps []task.Step) (AgentToolResult, error) {
	step, err := newAgentStep(current.ID, maxStepSequence(steps)+1, "tool", task.StepFailed,
		"Approval no longer matches tool request", "The approved Host action changed; no action was executed.")
	if err != nil {
		return AgentToolResult{}, err
	}
	result := AgentToolResult{Status: AgentToolFailed, Output: "The saved approval no longer matches this exact action. It was not executed."}
	if err := b.commitProgress(ctx, current, step, current.Usage, task.StatusRunning, "", "task.step", map[string]any{
		"tool": name, "operation_status": result.Status, "approval_decision": "mismatch",
	}); err != nil {
		return AgentToolResult{}, err
	}
	return result, nil
}

func (b *CloudDSHToolBroker) rejectApprovedToolCall(ctx context.Context, current task.Task, step task.Step, usage task.Usage, request AgentToolRequest) (AgentToolResult, error) {
	step.Status, step.Summary = task.StepFailed, "The user rejected this exact action; it was not dispatched."
	result := AgentToolResult{Status: AgentToolFailed, Output: "The user rejected this exact action. It was not executed."}
	if err := b.commitProgress(ctx, current, step, usage, task.StatusRunning, "", "task.step", map[string]any{
		"tool": request.Call.Name, "operation_key": request.OperationKey, "operation_status": result.Status, "approval_decision": "reject",
	}); err != nil {
		return AgentToolResult{}, err
	}
	return result, nil
}

func (b *CloudDSHToolBroker) pauseUnknown(ctx context.Context, current task.Task, step task.Step, usage task.Usage, operationKey, summary string) (AgentToolResult, error) {
	step.Status, step.Summary = task.StepUnknown, summary
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentPersistenceTimeout)
	defer cancel()
	err := b.commitProgress(persistCtx, current, step, usage, task.StatusWaitingUser, "tool_result_unknown", "task.waiting_user", map[string]any{
		"operation_key": operationKey, "operation_status": task.OperationUnknown,
	})
	return AgentToolResult{Status: AgentToolUnknown}, err
}

func (b *CloudDSHToolBroker) markUnknown(ctx context.Context, current task.Task, operationKey string) error {
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), agentPersistenceTimeout)
	defer cancel()
	_, err := b.store.UpdateOperation(persistCtx, OperationUpdate{
		TaskID: current.ID, OperationKey: operationKey, LeaseOwner: current.Lease.Owner,
		LeaseVersion: current.Lease.Version, Status: task.OperationUnknown,
	})
	return err
}

func (b *CloudDSHToolBroker) commitProgress(ctx context.Context, current task.Task, step task.Step, usage task.Usage, status task.Status, waitingReason, eventType string, detail any, pending ...*task.PendingRequest) error {
	payload, err := agentStepEvent(step, usage, detail)
	if err != nil {
		return err
	}
	progress := TaskProgress{
		TaskID: current.ID, LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
		Status: status, CurrentStep: step.ID, Result: current.Result, WaitingReason: waitingReason,
		Usage: usage, Steps: []task.Step{step}, Event: task.Event{TaskID: current.ID, Type: eventType, Payload: payload},
	}
	if len(pending) > 1 {
		return domain.InvalidArgument("pending_request", "at most one pending request may be committed")
	}
	if len(pending) == 1 {
		progress.PendingRequest = pending[0]
	}
	_, err = b.store.CommitTaskProgress(ctx, progress)
	return err
}
