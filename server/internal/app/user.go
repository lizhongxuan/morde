package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"morde/server/internal/domain"
	"morde/server/internal/domain/audit"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type CreateUserCommand struct {
	Name     string
	Password string
	Role     identity.Role
	Quota    *ResourceUsage
}

type UpdateUserCommand struct {
	Role     *identity.Role
	Password *string
	Disabled *bool
	Quota    *ResourceUsage
}

type UserService struct {
	users UserStore
	hosts HostStore
	audit AuditWriter
}

func NewUserService(users UserStore, hosts HostStore, writer AuditWriter) (*UserService, error) {
	if users == nil || hosts == nil || writer == nil {
		return nil, errors.New("user service dependencies are required")
	}
	return &UserService{users: users, hosts: hosts, audit: writer}, nil
}

func (s *UserService) List(ctx context.Context, principal identity.Principal) ([]UserSummary, error) {
	if err := authorizeAdmin(principal); err != nil {
		return nil, err
	}
	users, err := s.users.ListUsers(ctx)
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

func (s *UserService) Create(ctx context.Context, principal identity.Principal, command CreateUserCommand) (UserSummary, error) {
	if err := authorizeAdmin(principal); err != nil {
		return UserSummary{}, err
	}
	command.Name = strings.TrimSpace(command.Name)
	if command.Name == "" || !validRole(command.Role) {
		return UserSummary{}, domain.InvalidArgument("user", "name and valid role are required")
	}
	passwordHash, err := hashPassword(command.Password)
	if err != nil {
		return UserSummary{}, err
	}
	quota := ResourceUsage{Hosts: 3, CPUMilli: 4000, MemMiB: 8192}
	if command.Quota != nil {
		quota = *command.Quota
	}
	if !validQuota(quota) {
		return UserSummary{}, domain.InvalidArgument("quota", "quota values must be non-negative")
	}
	id, err := domain.NewUserID()
	if err != nil {
		return UserSummary{}, err
	}
	user, err := s.users.CreateUser(ctx, identity.User{
		ID: id, Name: command.Name, PasswordHash: string(passwordHash), Role: command.Role,
		QuotaHosts: quota.Hosts, QuotaCPUMilli: quota.CPUMilli, QuotaMemMiB: quota.MemMiB,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return UserSummary{}, domain.NewError(domain.CodeInvalidState, "username already exists")
		}
		return UserSummary{}, err
	}
	if err := s.audit.Write(ctx, audit.Entry{
		ActorID: adminActorID(principal), ActorKind: adminActorKind(principal), Action: audit.ActionUserCreate,
		Target: user.ID, Detail: map[string]any{"role": user.Role},
	}); err != nil {
		return UserSummary{}, fmt.Errorf("write user create audit: %w", err)
	}
	return userSummary(user, ResourceUsage{}), nil
}

func (s *UserService) Update(ctx context.Context, principal identity.Principal, userID string, command UpdateUserCommand) (UserSummary, error) {
	if err := authorizeAdmin(principal); err != nil {
		return UserSummary{}, err
	}
	if !domain.ValidUserID(userID) || (command.Role == nil && command.Password == nil && command.Disabled == nil && command.Quota == nil) {
		return UserSummary{}, domain.InvalidArgument("user", "user id and at least one field are required")
	}
	user, err := s.users.GetUserByID(ctx, userID)
	if err != nil {
		return UserSummary{}, err
	}
	changes := make(map[string]any)
	if command.Role != nil {
		if !validRole(*command.Role) {
			return UserSummary{}, domain.InvalidArgument("role", "invalid role")
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
	if command.Disabled != nil {
		user.Disabled = *command.Disabled
		changes["disabled"] = user.Disabled
	}
	if command.Password != nil {
		passwordHash, err := hashPassword(*command.Password)
		if err != nil {
			return UserSummary{}, err
		}
		user.PasswordHash = string(passwordHash)
		changes["password_changed"] = true
	}
	user, err = s.users.UpdateUser(ctx, adminActorID(principal), user)
	if err != nil {
		return UserSummary{}, err
	}
	if err := s.audit.Write(ctx, audit.Entry{
		ActorID: adminActorID(principal), ActorKind: adminActorKind(principal), Action: audit.ActionUserUpdate,
		Target: user.ID, Detail: changes,
	}); err != nil {
		return UserSummary{}, fmt.Errorf("write user update audit: %w", err)
	}
	hosts, err := s.hosts.ListHostsByOwner(ctx, user.ID)
	if err != nil {
		return UserSummary{}, err
	}
	usage := allocatedUsage(hosts)
	return userSummary(user, usage), nil
}

func authorizeAdmin(principal identity.Principal) error {
	if principal.Kind == identity.KindSystem && principal.Role == identity.RoleAdmin && principal.UserID == "" {
		return nil
	}
	if principal.Role != identity.RoleAdmin || principal.HostScope != "" || len(principal.HostIDs) != 0 || len(principal.Scopes) != 0 ||
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

func validQuota(quota ResourceUsage) bool {
	return quota.Hosts >= 0 && quota.CPUMilli >= 0 && quota.MemMiB >= 0
}

func hashPassword(password string) ([]byte, error) {
	if password == "" || len(password) > 72 {
		return nil, domain.InvalidArgument("password", "password must contain 1 to 72 bytes")
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
