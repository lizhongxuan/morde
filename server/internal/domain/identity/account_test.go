package identity

import "testing"

func TestRoleIsAdmin(t *testing.T) {
	for role, want := range map[Role]bool{
		RoleUser: false, RoleAdmin: true, RoleSuperAdmin: true, "": false,
	} {
		if got := role.IsAdmin(); got != want {
			t.Errorf("Role(%q).IsAdmin() = %t, want %t", role, got, want)
		}
	}
}

func TestAccountStatusValid(t *testing.T) {
	for status, want := range map[AccountStatus]bool{
		StatusPending: true, StatusRejected: true, StatusApproved: true, StatusDisabled: true, "": false, "active": false,
	} {
		if got := status.Valid(); got != want {
			t.Errorf("AccountStatus(%q).Valid() = %t, want %t", status, got, want)
		}
	}
}

func TestAccountStatusCanTransitionTo(t *testing.T) {
	allowed := map[AccountStatus][]AccountStatus{
		StatusPending:  {StatusApproved, StatusRejected},
		StatusRejected: {StatusApproved},
		StatusApproved: {StatusDisabled},
		StatusDisabled: {StatusApproved},
	}
	for _, current := range []AccountStatus{StatusPending, StatusRejected, StatusApproved, StatusDisabled, "invalid"} {
		for _, next := range []AccountStatus{StatusPending, StatusRejected, StatusApproved, StatusDisabled, "invalid"} {
			want := false
			for _, candidate := range allowed[current] {
				if next == candidate {
					want = true
					break
				}
			}
			if got := current.CanTransitionTo(next); got != want {
				t.Errorf("%q.CanTransitionTo(%q) = %t, want %t", current, next, got, want)
			}
		}
	}
}
