package main

import (
	"context"

	"morde/server/internal/app"
	"morde/server/internal/domain"
	"morde/server/internal/storage/workspacecurrent"
	"morde/server/internal/store/postgres"
)

type agentWorkspaceHostPathResolver struct {
	store            *postgres.Store
	current          *workspacecurrent.Store
	hostMountEnabled bool
}

func (r agentWorkspaceHostPathResolver) ResolveAgentWorkspaceHostPath(ctx context.Context, ownerID, workspaceID string) (app.AgentWorkspaceHostPathResolution, error) {
	value, _, err := r.store.GetAgentWorkspace(ctx, ownerID)
	if domain.CodeOf(err) == domain.CodeNotFound {
		return app.AgentWorkspaceHostPathResolution{}, nil
	}
	if err != nil {
		return app.AgentWorkspaceHostPathResolution{}, err
	}
	if value.ID != workspaceID {
		return app.AgentWorkspaceHostPathResolution{}, nil
	}
	if !r.hostMountEnabled {
		return app.AgentWorkspaceHostPathResolution{}, domain.NewError(domain.CodeWorkspaceUnavailable,
			"Agent Workspace Host mount is disabled until shared storage and write boundaries are verified")
	}
	path, err := r.current.WorkspacePath(ownerID, workspaceID)
	if err != nil {
		return app.AgentWorkspaceHostPathResolution{}, err
	}
	return app.AgentWorkspaceHostPathResolution{IsAgentWorkspace: true, HostPath: path}, nil
}

func (r agentWorkspaceHostPathResolver) WorkspacePath(ctx context.Context, ownerID string) (string, error) {
	value, _, err := r.store.GetAgentWorkspace(ctx, ownerID)
	if err != nil {
		return "", err
	}
	return r.current.WorkspacePath(ownerID, value.ID)
}
