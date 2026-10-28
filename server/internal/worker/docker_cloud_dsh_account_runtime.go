package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/dshguardian"
)

const (
	dockerAccountRuntimeFenceLabel = "morde.account-agent-runtime.fence"
	dockerAccountRuntimeNonceLabel = "morde.account-agent-runtime.nonce-sha256"
)

var dockerAccountRuntimeNonceDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

type dockerAccountRuntimeGuardian interface {
	ArmAccountRuntime(context.Context, dshguardian.AccountRuntimeGuard) error
	StartedAccountRuntime(context.Context, dshguardian.AccountRuntimeGuard) error
	RenewAccountRuntimeByIdentity(context.Context, app.AccountAgentRuntimeFence, string, time.Time) error
	StopAccountRuntime(context.Context, app.AccountAgentRuntimeFence, string) (app.AccountAgentRuntimeStopAttestation, error)
	RecoverAccountRuntimeStopped(context.Context, app.AccountAgentRuntimeFence, string) (app.AccountAgentRuntimeStopAttestation, error)
}

type dockerCloudDSHAccountRuntimeSupervisor struct {
	docker   *DockerCloudDSHSupervisor
	guardian dockerAccountRuntimeGuardian
}

func NewDockerCloudDSHAccountRuntimeSupervisor(docker *DockerCloudDSHSupervisor) (CloudDSHAccountRuntimeSupervisor, error) {
	if docker == nil {
		return nil, errors.New("account Agent Runtime Docker supervisor is required")
	}
	guardian, ok := docker.options.Guardian.(dockerAccountRuntimeGuardian)
	if !ok {
		return nil, errors.New("account Runtime Guardian operations are unavailable")
	}
	return &dockerCloudDSHAccountRuntimeSupervisor{docker: docker, guardian: guardian}, nil
}

func (s *dockerCloudDSHAccountRuntimeSupervisor) StartAccountRuntime(ctx context.Context, fence app.AccountAgentRuntimeFence,
	nonce []byte, expires time.Time) (CloudDSHAccountRuntimeProcess, error) {
	if ctx == nil || fence.Validate() != nil || len(nonce) != 32 || bytes.Equal(nonce, make([]byte, 32)) || !validCloudDSHLease(expires) {
		return CloudDSHAccountRuntimeProcess{}, errors.New("account Agent Runtime launch identity is invalid")
	}
	scriptPath := filepath.Join(filepath.Dir(s.docker.options.ScriptPath), "bin-account.mjs")
	if !cleanDockerDSHPath(scriptPath) {
		return CloudDSHAccountRuntimeProcess{}, errors.New("account Agent Runtime script path is invalid")
	}
	nonceDigest := sha256.Sum256(nonce)
	nonceHash := hex.EncodeToString(nonceDigest[:])
	encodedFence, err := json.Marshal(fence)
	if err != nil {
		return CloudDSHAccountRuntimeProcess{}, errors.New("account Agent Runtime fence cannot be encoded")
	}
	identity := sha256.Sum256(append(append(encodedFence, 0), nonceDigest[:]...))
	name := "morde-account-agent-" + hex.EncodeToString(identity[:])
	labels := map[string]string{dockerAccountRuntimeFenceLabel: string(encodedFence), dockerAccountRuntimeNonceLabel: nonceHash}
	guard := dshguardian.AccountRuntimeGuard{ContainerName: name, Fence: fence, NonceSHA256: nonceHash, ExpiresAt: expires}
	if err := s.docker.validateImage(ctx); err != nil {
		return CloudDSHAccountRuntimeProcess{}, errors.New("account Agent Runtime image is unavailable")
	}
	create := s.docker.containerCreateForScript(labels, scriptPath)
	var created struct {
		ID string `json:"Id"`
	}
	if err := s.docker.requestJSON(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(name), create, http.StatusCreated, &created); err != nil || !dockerDSHContainerID.MatchString(created.ID) {
		return CloudDSHAccountRuntimeProcess{}, errors.New("docker could not create the account Agent Runtime container")
	}
	guard.ContainerID = created.ID
	preflight, err := s.docker.inspect(ctx, created.ID)
	if err != nil || validateDockerAccountRuntime(preflight, guard, false) != nil || *preflight.State.Running || *preflight.State.PID != 0 {
		return CloudDSHAccountRuntimeProcess{}, errors.New("created account Agent Runtime container does not match its identity")
	}
	if err := s.guardian.ArmAccountRuntime(ctx, guard); err != nil {
		return CloudDSHAccountRuntimeProcess{}, errors.New("guardian could not arm the account Agent Runtime")
	}
	input, output, err := s.docker.attach(ctx, created.ID)
	if err != nil {
		_, _ = s.guardian.StopAccountRuntime(context.WithoutCancel(ctx), fence, nonceHash)
		return CloudDSHAccountRuntimeProcess{}, errors.New("account Agent Runtime pipes could not be attached")
	}
	if err := s.docker.requestJSON(ctx, http.MethodPost, "/containers/"+created.ID+"/start", nil, http.StatusNoContent, nil); err != nil {
		_ = input.Close()
		_ = output.Close()
		_, _ = s.guardian.StopAccountRuntime(context.WithoutCancel(ctx), fence, nonceHash)
		return CloudDSHAccountRuntimeProcess{}, errors.New("account Agent Runtime container could not be started")
	}
	started, err := s.docker.inspect(ctx, created.ID)
	if err != nil || validateDockerAccountRuntime(started, guard, true) != nil || !*started.State.Running || *started.State.PID < 2 {
		_ = input.Close()
		_ = output.Close()
		_, _ = s.guardian.StopAccountRuntime(context.WithoutCancel(ctx), fence, nonceHash)
		return CloudDSHAccountRuntimeProcess{}, errors.New("started account Agent Runtime container does not match its identity")
	}
	if err := s.guardian.StartedAccountRuntime(ctx, guard); err != nil {
		_ = input.Close()
		_ = output.Close()
		_, _ = s.guardian.StopAccountRuntime(context.WithoutCancel(ctx), fence, nonceHash)
		return CloudDSHAccountRuntimeProcess{}, errors.New("guardian could not verify the account Agent Runtime process")
	}
	if ctx.Err() != nil || !expires.After(time.Now()) {
		_ = input.Close()
		_ = output.Close()
		_, _ = s.guardian.StopAccountRuntime(context.WithoutCancel(ctx), fence, nonceHash)
		return CloudDSHAccountRuntimeProcess{}, errors.New("account Agent Runtime expired during startup")
	}
	return CloudDSHAccountRuntimeProcess{ContainerID: created.ID, ContainerName: name, Input: input, Output: output}, nil
}

func validateDockerAccountRuntime(value dockerDSHInspect, guard dshguardian.AccountRuntimeGuard, running bool) error {
	encodedFence, err := json.Marshal(guard.Fence)
	if err != nil || value.State == nil || value.State.Running == nil || value.State.Restarting == nil || value.State.Paused == nil || value.State.PID == nil ||
		value.ID != guard.ContainerID || value.Name != "/"+guard.ContainerName ||
		value.Config.Labels[dockerAccountRuntimeFenceLabel] != string(encodedFence) ||
		value.Config.Labels[dockerAccountRuntimeNonceLabel] != guard.NonceSHA256 || *value.State.Restarting || *value.State.Paused ||
		(running && !*value.State.Running) {
		return errors.New("account Agent Runtime container identity mismatch")
	}
	return nil
}

func (s *dockerCloudDSHAccountRuntimeSupervisor) RenewAccountRuntime(ctx context.Context, fence app.AccountAgentRuntimeFence,
	nonceSHA256 string, expires time.Time) error {
	if ctx == nil || fence.Validate() != nil || !dockerAccountRuntimeNonceDigest.MatchString(nonceSHA256) || !validCloudDSHLease(expires) {
		return errors.New("account Agent Runtime renewal identity is invalid")
	}
	return s.guardian.RenewAccountRuntimeByIdentity(ctx, fence, nonceSHA256, expires)
}

func (s *dockerCloudDSHAccountRuntimeSupervisor) StopAccountRuntime(ctx context.Context, fence app.AccountAgentRuntimeFence,
	nonceSHA256 string) (app.AccountAgentRuntimeStopAttestation, error) {
	return s.guardian.StopAccountRuntime(ctx, fence, nonceSHA256)
}

func (s *dockerCloudDSHAccountRuntimeSupervisor) RecoverAccountRuntimeStopped(ctx context.Context, fence app.AccountAgentRuntimeFence,
	nonceSHA256 string) (app.AccountAgentRuntimeStopAttestation, error) {
	return s.guardian.RecoverAccountRuntimeStopped(ctx, fence, nonceSHA256)
}

var _ CloudDSHAccountRuntimeSupervisor = (*dockerCloudDSHAccountRuntimeSupervisor)(nil)
