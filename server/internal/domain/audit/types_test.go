package audit

import "testing"

func TestActionValid(t *testing.T) {
	for _, action := range []Action{
		ActionLogin, ActionLoginFailed, ActionLogout,
		ActionHostCreate, ActionHostDelete, ActionHostRenew, ActionHostEnter,
		ActionHostApplicationInstall,
		ActionHostApplicationLaunch, ActionHostApplicationStop,
		ActionAPIKeyCreate, ActionAPIKeyDelete,
		ActionWorkspaceDelete, ActionUserCreate, ActionUserUpdate, ActionMCPCall,
		ActionSystemLost, ActionOrphanDeleted, ActionSystemRenew,
	} {
		if !action.Valid() {
			t.Errorf("%q.Valid() = false", action)
		}
	}
	for _, action := range []Action{"", "host.unknown", "system.delete"} {
		if action.Valid() {
			t.Errorf("%q.Valid() = true", action)
		}
	}
}
