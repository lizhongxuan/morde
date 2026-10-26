package audit

type Action string

const (
	ActionLogin                  Action = "auth.login"
	ActionLoginFailed            Action = "auth.login_failed"
	ActionLogout                 Action = "auth.logout"
	ActionHostCreate             Action = "host.create"
	ActionHostDelete             Action = "host.delete"
	ActionHostRenew              Action = "host.renew"
	ActionHostEnter              Action = "host.enter"
	ActionHostControl            Action = "host.control"
	ActionHostApplicationInstall Action = "host.application.install"
	ActionHostApplicationLaunch  Action = "host.application.launch"
	ActionHostApplicationStop    Action = "host.application.stop"
	ActionHostMemberInvite       Action = "host.member.invite"
	ActionHostMemberRevoke       Action = "host.member.revoke"
	ActionExecutionControl       Action = "execution.control"
	ActionHostRelease            Action = "host.release"
	ActionHostRestore            Action = "host.restore"
	ActionAPIKeyCreate           Action = "apikey.create"
	ActionAPIKeyDelete           Action = "apikey.delete"
	ActionDevicePair             Action = "device.pair"
	ActionDeviceDeny             Action = "device.deny"
	ActionDeviceRevoke           Action = "device.revoke"
	ActionDeviceGrant            Action = "device.workspace_grant"
	ActionClientPair             Action = "client.pair"
	ActionClientDeny             Action = "client.deny"
	ActionClientRevoke           Action = "client.revoke"
	ActionWorkspaceDelete        Action = "workspace.delete"
	ActionUserCreate             Action = "user.create"
	ActionUserUpdate             Action = "user.update"
	ActionMCPCall                Action = "mcp.call"
	ActionSystemLost             Action = "system.lost"
	ActionOrphanDeleted          Action = "system.orphan_deleted"
	ActionSystemRenew            Action = "system.renew"
)

func (a Action) Valid() bool {
	switch a {
	case ActionLogin, ActionLoginFailed, ActionLogout,
		ActionHostCreate, ActionHostDelete, ActionHostRenew, ActionHostEnter, ActionHostControl, ActionHostApplicationInstall, ActionHostApplicationLaunch, ActionHostApplicationStop, ActionHostMemberInvite, ActionHostMemberRevoke, ActionExecutionControl, ActionHostRelease, ActionHostRestore,
		ActionAPIKeyCreate, ActionAPIKeyDelete, ActionDevicePair, ActionDeviceDeny, ActionDeviceRevoke, ActionDeviceGrant,
		ActionClientPair, ActionClientDeny, ActionClientRevoke,
		ActionWorkspaceDelete, ActionUserCreate, ActionUserUpdate, ActionMCPCall,
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
