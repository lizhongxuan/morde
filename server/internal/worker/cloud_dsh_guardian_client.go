package worker

import (
	"context"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/dshguardian"
)

type DockerCloudDSHGuardianRecovery interface {
	RecoverStopped(context.Context, app.AgentInstanceFence, string) (app.AgentGuardianStopAttestation, error)
}

type unixSocketDockerCloudDSHGuardian struct {
	client *dshguardian.Client
}

func NewUnixSocketDockerCloudDSHGuardian(socket string) (DockerCloudDSHGuardian, error) {
	client, err := dshguardian.NewClient(socket)
	if err != nil {
		return nil, err
	}
	return &unixSocketDockerCloudDSHGuardian{client: client}, nil
}

func guardianGuard(value DockerCloudDSHGuard) dshguardian.Guard {
	return dshguardian.Guard{
		ContainerID: value.ContainerID, ContainerName: value.ContainerName, Fence: value.Fence,
		NonceSHA256: value.NonceSHA256, ExpiresAt: value.ExpiresAt,
	}
}

func (g *unixSocketDockerCloudDSHGuardian) Arm(ctx context.Context, value DockerCloudDSHGuard) error {
	return g.client.Arm(ctx, guardianGuard(value))
}

func (g *unixSocketDockerCloudDSHGuardian) Started(ctx context.Context, value DockerCloudDSHGuard, pid int) error {
	return g.client.Started(ctx, guardianGuard(value), pid)
}

func (g *unixSocketDockerCloudDSHGuardian) Renew(ctx context.Context, value DockerCloudDSHGuard) error {
	return g.client.Renew(ctx, guardianGuard(value))
}

func (g *unixSocketDockerCloudDSHGuardian) ConfirmDestroyed(ctx context.Context, value DockerCloudDSHGuard) (bool, error) {
	return g.client.ConfirmDestroyed(ctx, guardianGuard(value))
}

func (g *unixSocketDockerCloudDSHGuardian) RecoverStopped(ctx context.Context, fence app.AgentInstanceFence, nonceSHA256 string) (app.AgentGuardianStopAttestation, error) {
	return g.client.RecoverStopped(ctx, fence, nonceSHA256)
}

func (g *unixSocketDockerCloudDSHGuardian) ArmAccountRuntime(ctx context.Context, guard dshguardian.AccountRuntimeGuard) error {
	return g.client.ArmAccountRuntime(ctx, guard)
}

func (g *unixSocketDockerCloudDSHGuardian) StartedAccountRuntime(ctx context.Context, guard dshguardian.AccountRuntimeGuard) error {
	return g.client.StartedAccountRuntime(ctx, guard)
}

func (g *unixSocketDockerCloudDSHGuardian) RenewAccountRuntimeByIdentity(ctx context.Context, fence app.AccountAgentRuntimeFence,
	nonceSHA256 string, expiresAt time.Time) error {
	return g.client.RenewAccountRuntimeByIdentity(ctx, fence, nonceSHA256, expiresAt)
}

func (g *unixSocketDockerCloudDSHGuardian) StopAccountRuntime(ctx context.Context, fence app.AccountAgentRuntimeFence,
	nonceSHA256 string) (app.AccountAgentRuntimeStopAttestation, error) {
	return g.client.StopAccountRuntime(ctx, fence, nonceSHA256)
}

func (g *unixSocketDockerCloudDSHGuardian) RecoverAccountRuntimeStopped(ctx context.Context, fence app.AccountAgentRuntimeFence,
	nonceSHA256 string) (app.AccountAgentRuntimeStopAttestation, error) {
	return g.client.RecoverAccountRuntimeStopped(ctx, fence, nonceSHA256)
}

func (g *unixSocketDockerCloudDSHGuardian) ArmTaskProcess(ctx context.Context, guard dshguardian.TaskProcessGuard) error {
	return g.client.ArmTaskProcess(ctx, guard)
}

func (g *unixSocketDockerCloudDSHGuardian) StartedTaskProcess(ctx context.Context, guard dshguardian.TaskProcessGuard) error {
	return g.client.StartedTaskProcess(ctx, guard)
}

func (g *unixSocketDockerCloudDSHGuardian) RenewTaskProcess(ctx context.Context, guard dshguardian.TaskProcessGuard) error {
	return g.client.RenewTaskProcess(ctx, guard)
}

func (g *unixSocketDockerCloudDSHGuardian) StopTaskProcess(ctx context.Context, guard dshguardian.TaskProcessGuard) (app.AccountAgentTaskStopAttestation, error) {
	return g.client.StopTaskProcess(ctx, guard)
}

func (g *unixSocketDockerCloudDSHGuardian) RecoverTaskProcessStopped(ctx context.Context, runtimeFence app.AccountAgentRuntimeFence,
	taskFence app.AgentInstanceFence, nonceSHA256 string) (app.AccountAgentTaskStopAttestation, error) {
	return g.client.RecoverTaskProcessStopped(ctx, runtimeFence, taskFence, nonceSHA256)
}

func (g *unixSocketDockerCloudDSHGuardian) AcknowledgeTaskProcessStopped(ctx context.Context, runtimeFence app.AccountAgentRuntimeFence,
	taskFence app.AgentInstanceFence, nonceSHA256, proofSHA256 string) error {
	return g.client.AcknowledgeTaskProcessStopped(ctx, runtimeFence, taskFence, nonceSHA256, proofSHA256)
}

func (g *unixSocketDockerCloudDSHGuardian) AcknowledgeAccountRuntimeStopped(ctx context.Context,
	fence app.AccountAgentRuntimeFence, nonceSHA256, proofSHA256 string) error {
	return g.client.AcknowledgeAccountRuntimeStopped(ctx, fence, nonceSHA256, proofSHA256)
}
