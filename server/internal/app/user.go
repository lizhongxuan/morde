package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type UpdateUserCommand struct {
	ExpectedStatus identity.AccountStatus
	Status         *identity.AccountStatus
	Role           *identity.Role
	Quota          *ResourceUsage
}

type UserService struct {
	users         UserStore
	hosts         HostStore
	registrations loginLimiter
}

func NewUserService(users UserStore, hosts HostStore) (*UserService, error) {
	if users == nil || hosts == nil {
		return nil, errors.New("user service dependencies are required")
	}
	return &UserService{users: users, hosts: hosts, registrations: loginLimiter{attempts: make(map[string]loginAttempt)}}, nil
}

func (s *UserService) Register(ctx context.Context, name, password, ip string) (UserSummary, error) {
	name = strings.TrimSpace(name)
	if len(name) < 3 || len(name) > 64 || len(password) < 8 || len(password) > 72 {
		return UserSummary{}, domain.InvalidArgument("registration", "name must be 3-64 bytes and password 8-72 bytes")
	}
	if _, ok := s.registrations.allow(ip, time.Now(), 5, time.Minute); !ok {
		return UserSummary{}, domain.NewError(domain.CodeRateLimited, "registration rate limit reached")
	}
	if strings.EqualFold(name, "admin") {
		return UserSummary{}, domain.NewError(domain.CodeConflict, "username already exists")
	}
	passwordHash, err := hashPassword(password)
	if err != nil {
		return UserSummary{}, err
	}
	id, err := domain.NewUserID()
	if err != nil {
		return UserSummary{}, err
	}
	user, err := s.users.CreateUser(ctx, identity.User{
		ID: id, Name: name, PasswordHash: string(passwordHash), Role: identity.RoleUser,
		Status: identity.StatusPending, QuotaHosts: 3, QuotaCPUMilli: 4000, QuotaMemMiB: 8192,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return UserSummary{}, domain.NewError(domain.CodeConflict, "username already exists")
		}
		return UserSummary{}, err
	}
	return userSummary(user, ResourceUsage{}), nil
}

func (s *UserService) List(ctx context.Context, principal identity.Principal, status *identity.AccountStatus) ([]UserSummary, error) {
	if err := authorizeAdmin(principal); err != nil {
		return nil, err
	}
	if status != nil && !validStatus(*status) {
		return nil, domain.InvalidArgument("status", "invalid account status")
	}
	users, err := s.users.ListUsersByStatus(ctx, status)
	if err != nil {
		return nil, err
	}
	values := make([]UserSummary, 0, len(users))
	for _, user := range users {
		hosts, err := s.hosts.ListHostsByOwner(ctx, user.ID)
		if err != nil {
			return nil, err
		}
		usage := allocatedUsage(hosts)
		values = append(values, userSummary(user, usage))
	}
	return values, nil
}

func (s *UserService) Update(ctx context.Context, principal identity.Principal, userID string, command UpdateUserCommand) (UserSummary, error) {
	if err := authorizeAdmin(principal); err != nil {
		return UserSummary{}, err
	}
	if !domain.ValidUserID(userID) || !validStatus(command.ExpectedStatus) ||
		(command.Role == nil && command.Status == nil && command.Quota == nil) {
		return UserSummary{}, domain.InvalidArgument("user", "user id and at least one field are required")
	}
	user, err := s.users.GetUserByID(ctx, userID)
	if err != nil {
		return UserSummary{}, err
	}
	if user.Role == identity.RoleSuperAdmin || strings.EqualFold(user.Name, "admin") {
		return UserSummary{}, domain.NewError(domain.CodeForbidden, "the super administrator cannot be managed")
	}
	if user.Status != command.ExpectedStatus {
		return UserSummary{}, domain.NewError(domain.CodeConflict, "account status changed; refresh the user list")
	}
	currentStatus := user.Status
	changes := make(map[string]any)
	if command.Status != nil {
		if !user.Status.CanTransitionTo(*command.Status) {
			return UserSummary{}, domain.NewError(domain.CodeConflict, "account status transition is not allowed")
		}
		if principal.UserID == user.ID {
			return UserSummary{}, domain.NewError(domain.CodeForbidden, "administrators cannot change their own status")
		}
		user.Status = *command.Status
		changes["status"] = user.Status
	}
	if command.Role != nil {
		if !validRole(*command.Role) {
			return UserSummary{}, domain.InvalidArgument("role", "invalid role")
		}
		if currentStatus != identity.StatusApproved || user.Status != identity.StatusApproved {
			return UserSummary{}, domain.NewError(domain.CodeConflict, "only approved accounts can change role")
		}
		if principal.UserID == user.ID {
			return UserSummary{}, domain.NewError(domain.CodeForbidden, "administrators cannot change their own role")
		}
		user.Role = *command.Role
		changes["role"] = user.Role
	}
	if command.Quota != nil {
		if !validQuota(*command.Quota) {
			return UserSummary{}, domain.InvalidArgument("quota", "quota values must be non-negative")
		}
		user.QuotaHosts = command.Quota.Hosts
		user.QuotaCPUMilli = command.Quota.CPUMilli
		user.QuotaMemMiB = command.Quota.MemMiB
		changes["quota"] = map[string]int{"hosts": user.QuotaHosts, "cpu_milli": user.QuotaCPUMilli, "mem_mib": user.QuotaMemMiB}
	}
	user, err = s.users.UpdateUserWithAudit(ctx, adminActorID(principal), command.ExpectedStatus, user, audit.Entry{
		ActorID: adminActorID(principal), ActorKind: adminActorKind(principal), Action: audit.ActionUserUpdate,
		Target: user.ID, Detail: changes,
	})
	if err != nil {
		return UserSummary{}, err
	}
	hosts, err := s.hosts.ListHostsByOwner(ctx, user.ID)
	if err != nil {
		return UserSummary{}, err
	}
	usage := allocatedUsage(hosts)
	return userSummary(user, usage), nil
}

func authorizeAdmin(principal identity.Principal) error {
	if principal.Kind == identity.KindSystem && principal.Role.IsAdmin() && principal.UserID == "" {
		return nil
	}
	if !principal.Role.IsAdmin() || principal.HostScope != "" || len(principal.HostIDs) != 0 || len(principal.Scopes) != 0 ||
		principal.Kind != identity.KindSession ||
		!domain.ValidUserID(principal.UserID) || !domain.ValidTokenID(principal.TokenID) {
		return domain.NewError(domain.CodeForbidden, "admin access required")
	}
	return nil
}

func adminActorID(principal identity.Principal) string {
	if principal.Kind == identity.KindSystem {
		return "system"
	}
	return principal.UserID
}

func adminActorKind(principal identity.Principal) audit.ActorKind {
	if principal.Kind == identity.KindSystem {
		return audit.ActorSystem
	}
	return audit.ActorKind(principal.Kind)
}

func validRole(role identity.Role) bool {
	return role == identity.RoleUser || role == identity.RoleAdmin
}

func validStatus(status identity.AccountStatus) bool {
	return status == identity.StatusPending || status == identity.StatusRejected || status == identity.StatusApproved || status == identity.StatusDisabled
}

func validQuota(quota ResourceUsage) bool {
	return quota.Hosts >= 0 && quota.CPUMilli >= 0 && quota.MemMiB >= 0
}

func hashPassword(password string) ([]byte, error) {
	if len(password) < 8 || len(password) > 72 {
		return nil, domain.InvalidArgument("password", "password must contain 8 to 72 bytes")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	return hash, nil
}

func allocatedUsage(values []host.Host) ResourceUsage {
	usage := ResourceUsage{}
	for _, value := range values {
		allocated := value.State == host.StateCreating || value.State == host.StateRunning || value.State == host.StateStopping || value.State == host.StateDeleting
		if value.State == host.StateFailed {
			allocated = value.SandboxID != ""
			if !allocated {
				for _, binding := range value.Workspaces {
					if binding.State != workspace.BindingDetached {
						allocated = true
						break
					}
				}
			}
		}
		if allocated {
			usage.Hosts++
			usage.CPUMilli += value.CPUMilli
			usage.MemMiB += value.MemMiB
		}
	}
	return usage
}
