package app

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"morde/server/internal/domain"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/task"
)

const (
	cloudDSHSkillReadToolName  = "nanomuse_skill_read"
	cloudDSHSkillBundleVersion = "nanomuse-builtin-v1"
)

// MaxCloudDSHSkillEventBytes bounds metadata saved in the client-visible Task event.
const MaxCloudDSHSkillEventBytes = 8 << 10

//go:embed cloud_dsh_skills/*.md
var cloudDSHSkillBundle embed.FS

type cloudDSHSkill struct {
	id          string
	version     string
	description string
	sourceRef   string
	resource    string
	deprecated  bool
}

var cloudDSHSkillCatalog = []cloudDSHSkill{
	{id: "amap", version: "1", description: "Find China places, routes, travel times, and weather when authorized tools are available."},
	{id: "app-creation", version: "1", description: "Create app source in the current Workspace; run it only through this Task's authorized Host tools.", sourceRef: "server/internal/app/cloud_dsh_skills/app-creation.md"},
	{id: "compare-options", version: "1", description: "Compare options against the user's criteria and source requirements."},
	{id: "email-management", version: "1", description: "Triage and draft from user-provided or authorized email sources; never claim mailbox access or send without approval.", sourceRef: "server/internal/app/cloud_dsh_skills/email-management.md"},
	{id: "feishu", version: "1", description: "Use available Feishu tools for work information."},
	{id: "inbox-triage", version: "1", description: "Legacy Muse inbox workflow retained for checkpoint recovery.", deprecated: true},
	{id: "knowledge-organization", version: "1", description: "Find and organize notes in the current authorized Workspace using verified files.", sourceRef: "server/internal/app/cloud_dsh_skills/knowledge-organization.md"},
	{id: "kuaidi100", version: "1", description: "Track parcels when authorized tools are available."},
	{id: "markdown-docs", version: "1", description: "Create and deliver verified Markdown artifacts.", sourceRef: "server/internal/app/cloud_dsh_skills/markdown-docs.md"},
	{id: "meeting-follow-up", version: "1", description: "Turn an authorized meeting record into decisions, actions, and open questions in the current Workspace.", sourceRef: "server/internal/app/cloud_dsh_skills/meeting-follow-up.md"},
	{id: "meeting-prep", version: "1", description: "Legacy Muse meeting workflow retained for checkpoint recovery.", deprecated: true},
	{id: "meeting-preparation", version: "1", description: "Prepare a meeting brief from user-provided information and authorized Task tools.", sourceRef: "server/internal/app/cloud_dsh_skills/meeting-preparation.md"},
	{id: "phone-messages", version: "1", description: "Review or draft messages through an authorized phone interface."},
	{id: "scheduling", version: "1", description: "Propose meeting times from authorized availability; do not book or notify without approval.", sourceRef: "server/internal/app/cloud_dsh_skills/scheduling.md"},
	{id: "task-management", version: "1", description: "Organize commitments in the current authorized Workspace without confusing them with active Morde Tasks.", sourceRef: "server/internal/app/cloud_dsh_skills/task-management.md"},
	{id: "tencent-meeting", version: "1", description: "Use authorized Tencent Meeting tools if available."},
	{id: "train-tickets", version: "2", description: "Find train options; booking needs explicit approval and available phone tools."},
	{id: "translation", version: "1", description: "Translate user-provided or authorized text faithfully and save Markdown only to the current Workspace when requested.", sourceRef: "server/internal/app/cloud_dsh_skills/translation.md"},
	{id: "trip-plan", version: "1", description: "Plan travel with an itinerary, packing list, budget, and available calendar tools."},
	{id: "weekly-review", version: "1", description: "Legacy Muse review workflow retained for checkpoint recovery.", deprecated: true},
	{id: "weekly-review-planning", version: "1", description: "Review the week using available Morde goals and schedules, user-provided context, and authorized connectors.", sourceRef: "server/internal/app/cloud_dsh_skills/weekly-review-planning.md"},
	{id: "wide-research", version: "1", description: "Research independent inputs with bounded DSH child Agents and sources.", sourceRef: "server/internal/app/cloud_dsh_skills/wide-research.md"},
}

// CloudDSHSkillMetadataForID exposes only metadata for the fixed embedded catalog.
type CloudDSHSkillMetadata struct {
	ID        string
	Version   string
	SourceRef string
}

func CloudDSHSkillMetadataForID(id string) (CloudDSHSkillMetadata, bool) {
	skill, ok := cloudDSHSkillForID(id)
	if !ok {
		return CloudDSHSkillMetadata{}, false
	}
	return CloudDSHSkillMetadata{ID: skill.id, Version: skill.version, SourceRef: skill.sourceRef}, true
}

func cloudDSHSkillForID(id string) (cloudDSHSkill, bool) {
	for _, skill := range cloudDSHSkillCatalog {
		if skill.id == id {
			if skill.sourceRef == "" {
				skill.sourceRef = "clients/nanoMuse/nanomuse/skills/builtin/" + skill.id + "/SKILL.md"
			}
			skill.resource = "cloud_dsh_skills/" + skill.id + ".md"
			return skill, true
		}
	}
	return cloudDSHSkill{}, false
}

type cloudDSHSkillReadArguments struct {
	SkillID string `json:"skill_id"`
}

type cloudDSHSkillReadOutput struct {
	SkillID        string `json:"skill_id"`
	Version        string `json:"version"`
	BundleVersion  string `json:"bundle_version"`
	SourceRef      string `json:"source_ref"`
	ContentSHA256  string `json:"content_sha256"`
	ExecutionNotes string `json:"execution_notes"`
	Content        string `json:"content"`
}

func cloudDSHSkillReadDefinition() AgentToolDefinition {
	ids := make([]string, 0, len(cloudDSHSkillCatalog))
	descriptions := make([]string, 0, len(cloudDSHSkillCatalog))
	for _, skill := range cloudDSHSkillCatalog {
		if skill.deprecated {
			continue
		}
		ids = append(ids, skill.id)
		descriptions = append(descriptions, skill.id+": "+skill.description)
	}
	parameters, _ := json.Marshal(map[string]any{
		"type":       "object",
		"properties": map[string]any{"skill_id": map[string]any{"type": "string", "enum": ids}},
		"required":   []string{"skill_id"}, "additionalProperties": false,
	})
	return AgentToolDefinition{
		ModelTool: ModelTool{Name: cloudDSHSkillReadToolName,
			Description: "Load a reviewed Morde skill only when its purpose matches. Skills do not add permissions; use this Task's authorized tools and Workspace. Available: " + strings.Join(descriptions, "; "),
			Parameters:  parameters},
		RequiredScope: identity.ScopeTaskRead,
	}
}

func (b *CloudDSHToolBroker) executeSkillRead(ctx context.Context, claimed task.Task, call ModelToolCall, recovering bool, checkpointOutput *string) (AgentToolResult, error) {
	if ctx == nil || len(call.Arguments) == 0 || len(call.Arguments) > maxAgentToolArgsBytes ||
		!validAgentJSONObject([]byte(call.Arguments), maxAgentToolArgsBytes) {
		return AgentToolResult{}, domain.InvalidArgument("tool_call.arguments", "dsh skill arguments are invalid")
	}
	var arguments cloudDSHSkillReadArguments
	if decodeStrictRuntimeResult([]byte(call.Arguments), &arguments) != nil {
		return AgentToolResult{}, domain.InvalidArgument("tool_call.arguments", "dsh skill arguments are invalid")
	}
	skill, ok := cloudDSHSkillForID(arguments.SkillID)
	if !ok {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "Morde skill is not in the fixed allowlist")
	}
	if skill.deprecated && !recovering {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "deprecated Morde skill is available only to recover an existing DSH checkpoint")
	}
	details, err := b.currentServerTask(ctx, claimed)
	if err != nil {
		return AgentToolResult{}, err
	}
	current := details.Task
	if current.AgentSessionID == "" || current.Authorization.Version < 1 ||
		current.Authorization.Version != claimed.Authorization.Version ||
		(claimed.AgentSessionID != "" && current.AgentSessionID != claimed.AgentSessionID) ||
		(claimed.AgentInstanceID != "" && current.AgentInstanceID != claimed.AgentInstanceID) ||
		(claimed.AgentGeneration != 0 && current.AgentGeneration != claimed.AgentGeneration) ||
		!sameCloudDSHAuthorization(current.Authorization, claimed.Authorization) {
		return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "cloud dsh skill task authorization changed")
	}
	if err := b.authorizeSkillTask(ctx, current); err != nil {
		return AgentToolResult{}, domain.NewError(domain.CodeForbidden, "task authorization does not allow Morde skills")
	}
	content, err := fs.ReadFile(cloudDSHSkillBundle, skill.resource)
	if err != nil || len(content) == 0 || len(content) > maxAgentToolOutputBytes || !utf8.Valid(content) {
		return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "fixed Morde skill bundle is invalid")
	}
	contentHash := sha256.Sum256(content)
	output, err := json.Marshal(cloudDSHSkillReadOutput{
		SkillID: skill.id, Version: skill.version, BundleVersion: cloudDSHSkillBundleVersion,
		SourceRef: skill.sourceRef, ContentSHA256: hex.EncodeToString(contentHash[:]),
		ExecutionNotes: "Use only tools listed for this task. If web_search, web_fetch, or recall are unavailable, use only suitable already-authorized browser and Workspace tools or state the limitation; never invent tools or access paths outside the authorized Workspace.",
		Content:        string(content),
	})
	if err != nil || len(output) > maxAgentToolOutputBytes {
		return AgentToolResult{}, domain.NewError(domain.CodeResultTooLarge, "Morde skill exceeds the dsh tool output limit")
	}
	outputHash := sha256.Sum256(output)
	commit := CloudDSHSkillReadCommit{
		TaskID: current.ID, OwnerID: current.OwnerID, AgentSessionID: current.AgentSessionID,
		AuthorizationVersion: current.Authorization.Version,
		CallID:               call.CallID, SkillID: skill.id, SkillVersion: skill.version, BundleVersion: cloudDSHSkillBundleVersion,
		SourceRef: skill.sourceRef, ContentSHA256: hex.EncodeToString(contentHash[:]),
		OutputSHA256: hex.EncodeToString(outputHash[:]),
	}
	commit.FingerprintSHA256 = cloudDSHSkillReadFingerprint(commit)
	usage := cloneTaskUsage(current.Usage)
	if usage.ToolCalls == math.MaxInt64 {
		return AgentToolResult{}, domain.NewError(domain.CodeQuotaExceeded, "task tool call budget is exhausted")
	}
	usage.ToolCalls++
	step, err := newAgentStep(current.ID, maxStepSequence(details.Steps)+1, "tool", task.StepSucceeded,
		"Load Morde skill: "+skill.id, "Loaded the fixed, read-only skill bundle into the dsh session.")
	if err != nil {
		return AgentToolResult{}, err
	}
	detail := map[string]any{
		"tool": cloudDSHSkillReadToolName, "phase": "skill_read", "source": "nanomuse_builtin",
		"skill_call_id": commit.CallID, "agent_session_id": commit.AgentSessionID,
		"authorization_version": commit.AuthorizationVersion, "skill_id": commit.SkillID,
		"skill_version": commit.SkillVersion, "bundle_version": commit.BundleVersion,
		"source_ref": commit.SourceRef, "content_sha256": commit.ContentSHA256,
		"output_sha256": commit.OutputSHA256, "fingerprint_sha256": commit.FingerprintSHA256,
	}
	payload, err := agentStepEvent(step, usage, detail)
	if err != nil || len(payload) > MaxCloudDSHSkillEventBytes {
		return AgentToolResult{}, domain.NewError(domain.CodeResultTooLarge, "Morde skill event exceeds its limit")
	}
	if recovering && checkpointOutput != nil && *checkpointOutput != string(output) {
		return AgentToolResult{}, domain.NewError(domain.CodeInvalidState, "checkpointed Morde skill output changed")
	}
	step, toolCall, err := b.prepareTaskToolStep(ctx, current, step)
	if err != nil {
		return AgentToolResult{}, err
	}
	if _, err := b.store.CommitTaskProgress(ctx, TaskProgress{
		TaskID: current.ID, LeaseOwner: current.Lease.Owner, LeaseVersion: current.Lease.Version,
		Status: task.StatusRunning, CurrentStep: step.ID, Result: current.Result, Usage: usage,
		Steps: []task.Step{step}, ToolCall: toolCall, Event: task.Event{TaskID: current.ID, Type: "task.step", Payload: payload},
		CloudDSHSkillRead: &commit,
	}); err != nil {
		return AgentToolResult{}, err
	}
	return AgentToolResult{Status: AgentToolSucceeded, Output: string(output)}, nil
}

func sameCloudDSHAuthorization(current, claimed task.Authorization) bool {
	if current.Kind != claimed.Kind || current.TokenID != claimed.TokenID || current.ClientInstallationID != claimed.ClientInstallationID ||
		current.Version != claimed.Version || len(current.Scopes) != len(claimed.Scopes) {
		return false
	}
	if (current.ExpiresAt == nil) != (claimed.ExpiresAt == nil) ||
		(current.ExpiresAt != nil && !current.ExpiresAt.Equal(*claimed.ExpiresAt)) {
		return false
	}
	for index := range current.Scopes {
		if current.Scopes[index] != claimed.Scopes[index] {
			return false
		}
	}
	return true
}

func (b *CloudDSHToolBroker) authorizeSkillTask(ctx context.Context, current task.Task) error {
	authorization := current.Authorization
	if !domain.ValidTokenID(authorization.TokenID) || authorization.Version < 1 || authorization.ExpiresAt == nil || !authorization.ExpiresAt.After(time.Now()) {
		return domain.NewError(domain.CodeUnauthenticated, "task authorization is unavailable")
	}
	// Session and client task scopes are defined by Principal.HasScope, while API-key scopes are not task execution scopes.
	principal := identity.Principal{Kind: identity.Kind(authorization.Kind), Scopes: agentScopeSnapshot(authorization.Scopes)}
	if !principal.HasScope(identity.ScopeTaskRead) || !principal.HasScope(identity.ScopeTaskSubmit) {
		return domain.NewError(domain.CodeForbidden, "task read and submit scopes are required")
	}
	switch principal.Kind {
	case identity.KindSession:
	case identity.KindClient:
		if !domain.ValidClientInstallationID(authorization.ClientInstallationID) {
			return domain.NewError(domain.CodeForbidden, "task submission authorization is unavailable")
		}
	default:
		return domain.NewError(domain.CodeForbidden, "task authorization kind cannot execute Morde skills")
	}
	if current.HostID == "" {
		return nil
	}
	if b.runtime.authorizer == nil {
		return domain.NewError(domain.CodeForbidden, "task authorizer is unavailable")
	}
	return b.runtime.authorizer.AuthorizeTaskAction(ctx, current.InitiatorID, identity.Kind(authorization.Kind), authorization.TokenID,
		current.HostID, agentScopeSnapshot(authorization.Scopes), identity.ScopeTaskRead)
}

func cloudDSHSkillReadFingerprint(value CloudDSHSkillReadCommit) string {
	value.FingerprintSHA256 = ""
	encoded, _ := json.Marshal(value)
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}
