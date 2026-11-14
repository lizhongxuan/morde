package audit

type Action string

const (
	ActionLogin                        Action = "auth.login"
	ActionLoginFailed                  Action = "auth.login_failed"
	ActionLogout                       Action = "auth.logout"
	ActionHostCreate                   Action = "host.create"
	ActionHostDelete                   Action = "host.delete"
	ActionHostRenew                    Action = "host.renew"
	ActionHostEnter                    Action = "host.enter"
	ActionHostControl                  Action = "host.control"
	ActionHostResourcesUpdate          Action = "host.resources.update"
	ActionHostApplicationInstall       Action = "host.application.install"
	ActionHostApplicationLaunch        Action = "host.application.launch"
	ActionHostApplicationStop          Action = "host.application.stop"
	ActionHostMemberInvite             Action = "host.member.invite"
	ActionHostMemberRevoke             Action = "host.member.revoke"
	ActionExecutionControl             Action = "execution.control"
	ActionHostRelease                  Action = "host.release"
	ActionHostRestore                  Action = "host.restore"
	ActionHostRestoreRecovery          Action = "host.restore.recovery"
	ActionModelBudgetUpdate            Action = "model.budget.update"
	ActionModelDefaultUpdate           Action = "model.default.update"
	ActionModelUsageReconcile          Action = "model.usage.reconcile"
	ActionModelConnectionCreate        Action = "model.connection.create"
	ActionModelConnectionUpdate        Action = "model.connection.update"
	ActionModelConnectionDelete        Action = "model.connection.delete"
	ActionModelConnectionCheck         Action = "model.connection.check"
	ActionAPIKeyCreate                 Action = "apikey.create"
	ActionAPIKeyDelete                 Action = "apikey.delete"
	ActionDevicePair                   Action = "device.pair"
	ActionDeviceDeny                   Action = "device.deny"
	ActionDeviceRevoke                 Action = "device.revoke"
	ActionDeviceGrant                  Action = "device.workspace_grant"
	ActionClientPair                   Action = "client.pair"
	ActionClientDeny                   Action = "client.deny"
	ActionClientLogin                  Action = "client.login"
	ActionClientWebSession             Action = "client.web_session"
	ActionClientScheduleGrant          Action = "client.schedule_grant"
	ActionClientRevoke                 Action = "client.revoke"
	ActionCodexSessionGrant            Action = "codex.session.grant"
	ActionCodexSessionDelete           Action = "codex.session.delete"
	ActionCodexCommandCreate           Action = "codex.command.create"
	ActionCodexCommandClaim            Action = "codex.command.claim"
	ActionCodexCommandResult           Action = "codex.command.result"
	ActionCodexApprovalDecision        Action = "codex.approval.decision"
	ActionCodexApprovalResult          Action = "codex.approval.result"
	ActionWorkspaceDelete              Action = "workspace.delete"
	ActionUserCreate                   Action = "user.create"
	ActionUserUpdate                   Action = "user.update"
	ActionPersonalDataQuotaUpdate      Action = "personal_data.quota.update"
	ActionAdminNudgePolicyUpdate       Action = "admin.nudge.policy.update"
	ActionAdminControlRuleCreate       Action = "admin.control.rule.create"
	ActionAdminControlRuleUpdate       Action = "admin.control.rule.update"
	ActionAdminControlRuleDelete       Action = "admin.control.rule.delete"
	ActionAdminControlRuleFire         Action = "admin.control.rule.fire"
	ActionAdminControlEvaluate         Action = "admin.control.evaluate"
	ActionAdminControlNotifyTest       Action = "admin.control.notify_test"
	ActionRecordConsentRead            Action = "record.consent.read"
	ActionRecordConsentUpdate          Action = "record.consent.update"
	ActionRecordQuotaRead              Action = "record.quota.read"
	ActionRecordQuotaUpdate            Action = "record.quota.update"
	ActionRecordGrantUpdate            Action = "record.grant.update"
	ActionRecordSettingsRead           Action = "record.settings.read"
	ActionRecordAccessRead             Action = "record.access.read"
	ActionRecordList                   Action = "record.list"
	ActionRecordRead                   Action = "record.read"
	ActionRecordAttachmentsRead        Action = "record.attachments.read"
	ActionRecordAttachmentRead         Action = "record.attachment.read"
	ActionRecordExport                 Action = "record.export"
	ActionRecordDelete                 Action = "record.delete"
	ActionRecordDeleteComplete         Action = "record.delete.complete"
	ActionMCPCall                      Action = "mcp.call"
	ActionMCPConnectorCreate           Action = "mcp.connector.create"
	ActionMCPConnectorUpdate           Action = "mcp.connector.update"
	ActionMCPConnectorEnable           Action = "mcp.connector.enable"
	ActionMCPConnectorDisable          Action = "mcp.connector.disable"
	ActionMCPConnectorCredentialRotate Action = "mcp.connector.credential.rotate"
	ActionMCPConnectorDelete           Action = "mcp.connector.delete"
	ActionSystemLost                   Action = "system.lost"
	ActionOrphanDeleted                Action = "system.orphan_deleted"
	ActionSystemRenew                  Action = "system.renew"
)

func (a Action) Valid() bool {
	switch a {
	case ActionLogin, ActionLoginFailed, ActionLogout,
		ActionHostCreate, ActionHostDelete, ActionHostRenew, ActionHostEnter, ActionHostControl, ActionHostResourcesUpdate, ActionHostApplicationInstall, ActionHostApplicationLaunch, ActionHostApplicationStop, ActionHostMemberInvite, ActionHostMemberRevoke, ActionExecutionControl, ActionHostRelease, ActionHostRestore, ActionModelBudgetUpdate, ActionModelDefaultUpdate, ActionModelUsageReconcile,
		ActionModelConnectionCreate, ActionModelConnectionUpdate, ActionModelConnectionDelete, ActionModelConnectionCheck,
		ActionAPIKeyCreate, ActionAPIKeyDelete, ActionDevicePair, ActionDeviceDeny, ActionDeviceRevoke, ActionDeviceGrant,
		ActionClientPair, ActionClientDeny, ActionClientLogin, ActionClientWebSession, ActionClientScheduleGrant, ActionClientRevoke,
		ActionCodexSessionGrant, ActionCodexSessionDelete, ActionCodexCommandCreate, ActionCodexCommandClaim, ActionCodexCommandResult,
		ActionCodexApprovalDecision, ActionCodexApprovalResult,
		ActionHostRestoreRecovery, ActionWorkspaceDelete, ActionUserCreate, ActionUserUpdate, ActionPersonalDataQuotaUpdate, ActionAdminNudgePolicyUpdate,
		ActionAdminControlRuleCreate, ActionAdminControlRuleUpdate, ActionAdminControlRuleDelete,
		ActionAdminControlRuleFire, ActionAdminControlEvaluate, ActionAdminControlNotifyTest, ActionMCPCall,
		ActionRecordConsentRead, ActionRecordConsentUpdate, ActionRecordQuotaRead, ActionRecordQuotaUpdate,
		ActionRecordGrantUpdate, ActionRecordSettingsRead, ActionRecordAccessRead, ActionRecordList,
		ActionRecordRead, ActionRecordAttachmentsRead, ActionRecordAttachmentRead, ActionRecordExport,
		ActionRecordDelete, ActionRecordDeleteComplete,
		ActionMCPConnectorCreate, ActionMCPConnectorUpdate, ActionMCPConnectorEnable,
		ActionMCPConnectorDisable, ActionMCPConnectorCredentialRotate, ActionMCPConnectorDelete,
		ActionSystemLost, ActionOrphanDeleted, ActionSystemRenew:
		return true
	default:
		return false
	}
}

type ActorKind string

const (
	ActorSession    ActorKind = "session"
	ActorAPIKey     ActorKind = "api_key"
	ActorClient     ActorKind = "client"
	ActorHostCookie ActorKind = "host_cookie"
	ActorSystem     ActorKind = "system"
)

type Entry struct {
	ActorID   string
	ActorKind ActorKind
	Action    Action
	Target    string
	Detail    map[string]any
	IP        string
}
