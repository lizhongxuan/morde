package mcp

import (
	"context"
	"strconv"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/domain/host"
	"morde/server/internal/domain/identity"
	"morde/server/internal/domain/workspace"
)

type hostOutput struct {
	ID           string                   `json:"id"`
	Name         string                   `json:"name"`
	TemplateID   string                   `json:"template_id"`
	Isolation    string                   `json:"isolation"`
	State        host.State               `json:"state"`
	StateReason  string                   `json:"state_reason"`
	URL          string                   `json:"url"`
	Workspaces   []workspaceBindingOutput `json:"workspaces"`
	Resources    resourceOutput           `json:"resources"`
	ExpiresAt    *time.Time               `json:"expires_at"`
	LastActiveAt *time.Time               `json:"last_active_at"`
	CreatedAt    time.Time                `json:"created_at"`
}

type workspaceBindingOutput struct {
	ID              string                 `json:"id"`
	Workspace       workspaceOutput        `json:"workspace"`
	Directory       string                 `json:"directory"`
	State           workspace.BindingState `json:"state"`
	BindingVersion  int64                  `json:"binding_version"`
	DesiredRevision int64                  `json:"desired_revision"`
	AppliedRevision int64                  `json:"applied_revision"`
}

type workspaceOutput struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	State      workspace.State `json:"state"`
	Revision   int64           `json:"revision"`
	UsedBytes  int64           `json:"used_bytes"`
	QuotaBytes int64           `json:"quota_bytes"`
}

type resourceOutput struct {
	CPUMilli      int `json:"cpu_milli"`
	MemMiB        int `json:"mem_mib"`
	CPULimitMilli int `json:"cpu_limit_milli"`
	MemLimitMiB   int `json:"mem_limit_mib"`
}

func (s *service) lifecycle(ctx context.Context, p identity.Principal, name string, args map[string]any) (any, error) {
	switch name {
	case "host_create":
		workspaceIDs := make([]string, 0)
		for _, item := range arrayArg(args, "workspace_ids") {
			if workspaceID, ok := item.(string); ok {
				workspaceIDs = append(workspaceIDs, workspaceID)
			}
		}
		value, err := s.deps.Hosts.Create(ctx, p, app.CreateHostCommand{
			TemplateID: stringArg(args, "template_id"), Name: stringArg(args, "name"), WorkspaceIDs: workspaceIDs,
			IdempotencyKey: stringArg(args, "idempotency_key"),
		})
		if err != nil {
			return nil, err
		}
		return s.hostOutput(value), nil
	case "host_list":
		values, err := s.deps.Hosts.List(ctx, p)
		if err != nil {
			return nil, err
		}
		items := make([]hostOutput, 0, len(values))
		for _, value := range values {
			items = append(items, s.hostOutput(value))
		}
		return map[string]any{"items": items}, nil
	case "host_get":
		value, err := s.deps.Hosts.Get(ctx, p, stringArg(args, "host_id"))
		if err != nil {
			return nil, err
		}
		return s.hostOutput(value), nil
	case "host_delete":
		value, err := s.deps.Hosts.Delete(ctx, p, app.DeleteHostCommand{HostID: stringArg(args, "host_id")})
		if err != nil {
			return nil, err
		}
		return s.hostOutput(value), nil
	case "host_renew":
		value, err := s.deps.Hosts.Renew(ctx, p, stringArg(args, "host_id"))
		if err != nil {
			return nil, err
		}
		return s.hostOutput(value), nil
	default:
		return nil, domain.NewError(domain.CodeInternal, "unknown lifecycle tool")
	}
}

func (s *service) hostOutput(value host.Host) hostOutput {
	workspaces := make([]workspaceBindingOutput, 0, len(value.Workspaces))
	for _, binding := range value.Workspaces {
		workspaces = append(workspaces, workspaceBindingOutput{
			ID: binding.ID,
			Workspace: workspaceOutput{
				ID: binding.Workspace.ID, Name: binding.Workspace.Name, State: binding.Workspace.State,
				Revision: binding.Workspace.Revision, UsedBytes: binding.Workspace.UsedBytes, QuotaBytes: binding.Workspace.QuotaBytes,
			},
			Directory: binding.Directory, State: binding.State, BindingVersion: binding.BindingVersion,
			DesiredRevision: binding.DesiredRevision, AppliedRevision: binding.AppliedRevision,
		})
	}
	return hostOutput{
		ID: value.ID, Name: value.Name, TemplateID: value.TemplateID, Isolation: value.RuntimeProfile,
		State: value.State, StateReason: value.StateReason,
		URL:        s.deps.Domains.Scheme + "://" + value.ID + "." + s.deps.Domains.HostSuffix,
		Workspaces: workspaces,
		Resources:  resourceOutput{CPUMilli: value.CPUMilli, MemMiB: value.MemMiB, CPULimitMilli: value.CPULimitMilli, MemLimitMiB: value.MemLimitMiB},
		ExpiresAt:  value.ExpiresAt, LastActiveAt: value.LastActiveAt, CreatedAt: value.CreatedAt,
	}
}

func arrayArg(args map[string]any, name string) []any {
	value, _ := args[name].([]any)
	return value
}

func (s *service) preview(ctx context.Context, p identity.Principal, args map[string]any) (any, error) {
	hostID := stringArg(args, "host_id")
	if _, err := s.computerHost(ctx, p, hostID, identity.ScopeComputerObserve); err != nil {
		return nil, err
	}
	port, _ := intArg(args, "port")
	if !domain.ValidPreviewTargetPort(int(port)) || port < int64(s.deps.PreviewPortMin) {
		return nil, domain.InvalidArgument("port", "preview port is not allowed")
	}
	for _, denied := range s.deps.PreviewPortDeny {
		if port == int64(denied) {
			return nil, domain.InvalidArgument("port", "preview port is not allowed")
		}
	}
	url := s.deps.Domains.Scheme + "://" + strconv.FormatInt(port, 10) + "-" + hostID + "." + s.deps.Domains.HostSuffix
	return map[string]any{"url": url}, nil
}
