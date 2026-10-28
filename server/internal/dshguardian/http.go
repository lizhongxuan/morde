package dshguardian

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"morde/server/internal/app"
)

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/guards", s.handleArm)
	mux.HandleFunc("/v1/guards/recover-stop", s.handleRecoverStopped)
	mux.HandleFunc("/v1/guards/", s.handleLease)
	mux.HandleFunc("/v1/account-runtimes", s.handleAccountRuntimeArm)
	mux.HandleFunc("/v1/account-runtimes/", s.handleAccountRuntime)
	mux.HandleFunc("/v1/task-processes", s.handleTaskProcessArm)
	mux.HandleFunc("/v1/task-processes/", s.handleTaskProcess)
	return mux
}

func (s *Service) handleAccountRuntimeArm(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/account-runtimes" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var guard AccountRuntimeGuard
	if err := readBody(r, &guard); err != nil {
		writeError(w, http.StatusBadRequest, "invalid account Runtime request")
		return
	}
	if err := s.ArmAccountRuntime(r.Context(), guard); err != nil {
		writeError(w, http.StatusConflict, "guardian could not arm account Runtime")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleAccountRuntime(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/account-runtimes/ack-stop" {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Fence       app.AccountAgentRuntimeFence `json:"fence"`
			NonceSHA256 string                       `json:"nonce_sha256"`
			ProofSHA256 string                       `json:"proof_sha256"`
		}
		if err := readBody(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid account Runtime proof acknowledgement")
			return
		}
		if err := s.AcknowledgeAccountRuntimeStopped(r.Context(), request.Fence, request.NonceSHA256, request.ProofSHA256); err != nil {
			writeError(w, http.StatusConflict, "guardian could not acknowledge account Runtime proof")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path == "/v1/account-runtimes/lookup" {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Fence       app.AccountAgentRuntimeFence `json:"fence"`
			NonceSHA256 string                       `json:"nonce_sha256"`
		}
		if err := readBody(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid account Runtime lookup request")
			return
		}
		guard, err := s.LookupAccountRuntime(r.Context(), request.Fence, request.NonceSHA256)
		if err != nil {
			writeError(w, http.StatusConflict, "guardian could not resolve account Runtime identity")
			return
		}
		writeJSON(w, http.StatusOK, guard)
		return
	}
	if r.URL.Path == "/v1/account-runtimes/renew-by-identity" {
		if r.Method != http.MethodPut {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Fence       app.AccountAgentRuntimeFence `json:"fence"`
			NonceSHA256 string                       `json:"nonce_sha256"`
			ExpiresAt   time.Time                    `json:"expires_at"`
		}
		if err := readBody(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid account Runtime renewal request")
			return
		}
		if err := s.RenewAccountRuntimeByIdentity(r.Context(), request.Fence, request.NonceSHA256, request.ExpiresAt); err != nil {
			writeError(w, http.StatusConflict, "guardian could not renew account Runtime")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path == "/v1/account-runtimes/stop" || r.URL.Path == "/v1/account-runtimes/recover-stop" {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var request struct {
			Fence       app.AccountAgentRuntimeFence `json:"fence"`
			NonceSHA256 string                       `json:"nonce_sha256"`
		}
		if err := readBody(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid account Runtime stop request")
			return
		}
		var attestation app.AccountAgentRuntimeStopAttestation
		var err error
		if r.URL.Path == "/v1/account-runtimes/stop" {
			attestation, err = s.StopAccountRuntime(r.Context(), request.Fence, request.NonceSHA256)
		} else {
			attestation, err = s.RecoverAccountRuntimeStopped(r.Context(), request.Fence, request.NonceSHA256)
		}
		if err != nil {
			writeError(w, http.StatusConflict, "guardian could not verify account Runtime destruction")
			return
		}
		writeJSON(w, http.StatusOK, attestation)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/account-runtimes/"), "/")
	if len(parts) != 2 || !containerIDRE.MatchString(parts[0]) {
		http.NotFound(w, r)
		return
	}
	switch parts[1] {
	case "started":
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var guard AccountRuntimeGuard
		if err := readBody(r, &guard); err != nil || guard.ContainerID != parts[0] {
			writeError(w, http.StatusBadRequest, "invalid account Runtime start request")
			return
		}
		if err := s.StartedAccountRuntime(r.Context(), guard); err != nil {
			writeError(w, http.StatusConflict, "guardian could not record account Runtime process evidence")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case "renew":
		if r.Method != http.MethodPut {
			http.NotFound(w, r)
			return
		}
		var guard AccountRuntimeGuard
		if err := readBody(r, &guard); err != nil || guard.ContainerID != parts[0] {
			writeError(w, http.StatusBadRequest, "invalid account Runtime renewal request")
			return
		}
		if err := s.RenewAccountRuntime(r.Context(), guard); err != nil {
			writeError(w, http.StatusConflict, "guardian could not renew account Runtime")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (s *Service) handleTaskProcessArm(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/task-processes" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var guard TaskProcessGuard
	if err := readBody(r, &guard); err != nil {
		writeError(w, http.StatusBadRequest, "invalid Task process request")
		return
	}
	if err := s.ArmTaskProcess(r.Context(), guard); err != nil {
		writeError(w, http.StatusConflict, "guardian could not arm Task cgroup")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleTaskProcess(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/task-processes/ack-stop" {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var request struct {
			RuntimeFence app.AccountAgentRuntimeFence `json:"runtime_fence"`
			TaskFence    app.AgentInstanceFence       `json:"task_fence"`
			NonceSHA256  string                       `json:"nonce_sha256"`
			ProofSHA256  string                       `json:"proof_sha256"`
		}
		if err := readBody(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid Task process proof acknowledgement")
			return
		}
		if err := s.AcknowledgeTaskProcessStopped(r.Context(), request.RuntimeFence, request.TaskFence,
			request.NonceSHA256, request.ProofSHA256); err != nil {
			writeError(w, http.StatusConflict, "guardian could not acknowledge Task process proof")
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.URL.Path == "/v1/task-processes/recover-stop" {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var request struct {
			RuntimeFence app.AccountAgentRuntimeFence `json:"runtime_fence"`
			TaskFence    app.AgentInstanceFence       `json:"task_fence"`
			NonceSHA256  string                       `json:"nonce_sha256"`
		}
		if err := readBody(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "invalid Task process recovery request")
			return
		}
		attestation, err := s.RecoverTaskProcessStopped(r.Context(), request.RuntimeFence, request.TaskFence, request.NonceSHA256)
		if err != nil {
			writeError(w, http.StatusConflict, "guardian could not verify Task cgroup destruction")
			return
		}
		writeJSON(w, http.StatusOK, attestation)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/task-processes/"), "/")
	if len(parts) != 3 || !containerIDRE.MatchString(parts[0]) || !validTaskKey(parts[1]) {
		http.NotFound(w, r)
		return
	}
	var guard TaskProcessGuard
	if err := readBody(r, &guard); err != nil || guard.RuntimeContainerID != parts[0] || taskProcessKey(guard) != parts[1] {
		writeError(w, http.StatusBadRequest, "invalid Task process operation request")
		return
	}
	switch parts[2] {
	case "started":
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if err := s.StartedTaskProcess(r.Context(), guard); err != nil {
			writeError(w, http.StatusConflict, "guardian could not record Task process start")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case "renew":
		if r.Method != http.MethodPut {
			http.NotFound(w, r)
			return
		}
		if err := s.RenewTaskProcess(r.Context(), guard); err != nil {
			writeError(w, http.StatusConflict, "guardian could not renew Task process lease")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case "stop":
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		attestation, err := s.StopTaskProcess(r.Context(), guard)
		if err != nil {
			writeError(w, http.StatusConflict, "guardian could not verify Task cgroup destruction")
			return
		}
		writeJSON(w, http.StatusOK, attestation)
	default:
		http.NotFound(w, r)
	}
}

func (s *Service) handleRecoverStopped(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/guards/recover-stop" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var request struct {
		Fence       app.AgentInstanceFence `json:"fence"`
		NonceSHA256 string                 `json:"nonce_sha256"`
	}
	if err := readBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid recovery request")
		return
	}
	attestation, err := s.RecoverStopped(r.Context(), request.Fence, request.NonceSHA256)
	if err != nil {
		writeError(w, http.StatusConflict, "guardian could not verify Agent destruction")
		return
	}
	writeJSON(w, http.StatusOK, attestation)
}

func (s *Service) handleArm(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/guards" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var guard Guard
	if err := readBody(r, &guard); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.Arm(r.Context(), guard); err != nil {
		writeError(w, http.StatusConflict, "guardian could not arm the lease")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleLease(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/guards/"), "/")
	if len(parts) != 2 || !containerIDRE.MatchString(parts[0]) {
		http.NotFound(w, r)
		return
	}
	switch {
	case parts[1] == "started" && r.Method == http.MethodPost:
		var request struct {
			Guard Guard `json:"guard"`
			PID   int   `json:"pid"`
		}
		if err := readBody(r, &request); err != nil || request.Guard.ContainerID != parts[0] {
			writeError(w, http.StatusBadRequest, "invalid started request")
			return
		}
		if err := s.Started(r.Context(), request.Guard, request.PID); err != nil {
			writeError(w, http.StatusConflict, "guardian could not record process evidence")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case parts[1] == "renew" && r.Method == http.MethodPut:
		var guard Guard
		if err := readBody(r, &guard); err != nil || guard.ContainerID != parts[0] {
			writeError(w, http.StatusBadRequest, "invalid renewal request")
			return
		}
		if err := s.Renew(r.Context(), guard); err != nil {
			writeError(w, http.StatusConflict, "guardian could not renew the lease")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case parts[1] == "confirm-destroyed" && r.Method == http.MethodPost:
		var guard Guard
		if err := readBody(r, &guard); err != nil || guard.ContainerID != parts[0] {
			writeError(w, http.StatusBadRequest, "invalid confirmation request")
			return
		}
		destroyed, err := s.ConfirmDestroyed(r.Context(), guard)
		if err != nil {
			writeError(w, http.StatusConflict, "guardian could not verify destruction")
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Destroyed bool `json:"destroyed"`
		}{Destroyed: destroyed})
	default:
		http.NotFound(w, r)
	}
}

func readBody(r *http.Request, target any) error {
	if r.Body == nil {
		return errors.New("request body is required")
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxRequestBytes {
		return errors.New("request body is invalid")
	}
	return decodeStrictJSON(data, target)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{Error: message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// Client provides the local API used by the Portal-side adapter.
type Client struct {
	http *http.Client
}

func NewClient(socket string) (*Client, error) {
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || socket == "/" {
		return nil, errors.New("guardian socket must be a clean absolute path")
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", socket)
		},
		DisableCompression: true,
	}
	return &Client{http: &http.Client{Transport: transport, Timeout: 60 * time.Second}}, nil
}

func (c *Client) Arm(ctx context.Context, guard Guard) error {
	return c.do(ctx, http.MethodPost, "/v1/guards", guard, nil, http.StatusNoContent)
}

func (c *Client) Started(ctx context.Context, guard Guard, pid int) error {
	request := struct {
		Guard Guard `json:"guard"`
		PID   int   `json:"pid"`
	}{Guard: guard, PID: pid}
	return c.do(ctx, http.MethodPost, "/v1/guards/"+guard.ContainerID+"/started", request, nil, http.StatusNoContent)
}

func (c *Client) Renew(ctx context.Context, guard Guard) error {
	return c.do(ctx, http.MethodPut, "/v1/guards/"+guard.ContainerID+"/renew", guard, nil, http.StatusNoContent)
}

func (c *Client) ConfirmDestroyed(ctx context.Context, guard Guard) (bool, error) {
	var result struct {
		Destroyed bool `json:"destroyed"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/guards/"+guard.ContainerID+"/confirm-destroyed", guard, &result, http.StatusOK); err != nil {
		return false, err
	}
	return result.Destroyed, nil
}

func (c *Client) RecoverStopped(ctx context.Context, fence app.AgentInstanceFence, nonceSHA256 string) (app.AgentGuardianStopAttestation, error) {
	request := struct {
		Fence       app.AgentInstanceFence `json:"fence"`
		NonceSHA256 string                 `json:"nonce_sha256"`
	}{Fence: fence, NonceSHA256: nonceSHA256}
	var result app.AgentGuardianStopAttestation
	if err := c.do(ctx, http.MethodPost, "/v1/guards/recover-stop", request, &result, http.StatusOK); err != nil {
		return app.AgentGuardianStopAttestation{}, err
	}
	return result, nil
}

func (c *Client) ArmAccountRuntime(ctx context.Context, guard AccountRuntimeGuard) error {
	return c.do(ctx, http.MethodPost, "/v1/account-runtimes", guard, nil, http.StatusNoContent)
}

func (c *Client) StartedAccountRuntime(ctx context.Context, guard AccountRuntimeGuard) error {
	return c.do(ctx, http.MethodPost, "/v1/account-runtimes/"+guard.ContainerID+"/started", guard, nil, http.StatusNoContent)
}

func (c *Client) RenewAccountRuntime(ctx context.Context, guard AccountRuntimeGuard) error {
	return c.do(ctx, http.MethodPut, "/v1/account-runtimes/"+guard.ContainerID+"/renew", guard, nil, http.StatusNoContent)
}

func (c *Client) LookupAccountRuntime(ctx context.Context, fence app.AccountAgentRuntimeFence, nonceSHA256 string) (AccountRuntimeGuard, error) {
	request := struct {
		Fence       app.AccountAgentRuntimeFence `json:"fence"`
		NonceSHA256 string                       `json:"nonce_sha256"`
	}{Fence: fence, NonceSHA256: nonceSHA256}
	var guard AccountRuntimeGuard
	if err := c.do(ctx, http.MethodPost, "/v1/account-runtimes/lookup", request, &guard, http.StatusOK); err != nil {
		return AccountRuntimeGuard{}, err
	}
	return guard, nil
}

func (c *Client) RenewAccountRuntimeByIdentity(ctx context.Context, fence app.AccountAgentRuntimeFence, nonceSHA256 string, expiresAt time.Time) error {
	request := struct {
		Fence       app.AccountAgentRuntimeFence `json:"fence"`
		NonceSHA256 string                       `json:"nonce_sha256"`
		ExpiresAt   time.Time                    `json:"expires_at"`
	}{Fence: fence, NonceSHA256: nonceSHA256, ExpiresAt: expiresAt}
	return c.do(ctx, http.MethodPut, "/v1/account-runtimes/renew-by-identity", request, nil, http.StatusNoContent)
}

func (c *Client) StopAccountRuntime(ctx context.Context, fence app.AccountAgentRuntimeFence, nonceSHA256 string) (app.AccountAgentRuntimeStopAttestation, error) {
	return c.accountRuntimeStop(ctx, "/v1/account-runtimes/stop", fence, nonceSHA256)
}

func (c *Client) RecoverAccountRuntimeStopped(ctx context.Context, fence app.AccountAgentRuntimeFence, nonceSHA256 string) (app.AccountAgentRuntimeStopAttestation, error) {
	return c.accountRuntimeStop(ctx, "/v1/account-runtimes/recover-stop", fence, nonceSHA256)
}

func (c *Client) AcknowledgeAccountRuntimeStopped(ctx context.Context, runtimeFence app.AccountAgentRuntimeFence,
	nonceSHA256, proofSHA256 string) error {
	request := struct {
		Fence       app.AccountAgentRuntimeFence `json:"fence"`
		NonceSHA256 string                       `json:"nonce_sha256"`
		ProofSHA256 string                       `json:"proof_sha256"`
	}{Fence: runtimeFence, NonceSHA256: nonceSHA256, ProofSHA256: proofSHA256}
	return c.do(ctx, http.MethodPost, "/v1/account-runtimes/ack-stop", request, nil, http.StatusNoContent)
}

func (c *Client) accountRuntimeStop(ctx context.Context, path string, fence app.AccountAgentRuntimeFence, nonceSHA256 string) (app.AccountAgentRuntimeStopAttestation, error) {
	request := struct {
		Fence       app.AccountAgentRuntimeFence `json:"fence"`
		NonceSHA256 string                       `json:"nonce_sha256"`
	}{Fence: fence, NonceSHA256: nonceSHA256}
	var result app.AccountAgentRuntimeStopAttestation
	if err := c.do(ctx, http.MethodPost, path, request, &result, http.StatusOK); err != nil {
		return app.AccountAgentRuntimeStopAttestation{}, err
	}
	return result, nil
}

func (c *Client) ArmTaskProcess(ctx context.Context, guard TaskProcessGuard) error {
	return c.do(ctx, http.MethodPost, "/v1/task-processes", guard, nil, http.StatusNoContent)
}

func (c *Client) StartedTaskProcess(ctx context.Context, guard TaskProcessGuard) error {
	return c.do(ctx, http.MethodPost, taskProcessPath(guard, "started"), guard, nil, http.StatusNoContent)
}

func (c *Client) RenewTaskProcess(ctx context.Context, guard TaskProcessGuard) error {
	return c.do(ctx, http.MethodPut, taskProcessPath(guard, "renew"), guard, nil, http.StatusNoContent)
}

func (c *Client) StopTaskProcess(ctx context.Context, guard TaskProcessGuard) (app.AccountAgentTaskStopAttestation, error) {
	var result app.AccountAgentTaskStopAttestation
	if err := c.do(ctx, http.MethodPost, taskProcessPath(guard, "stop"), guard, &result, http.StatusOK); err != nil {
		return app.AccountAgentTaskStopAttestation{}, err
	}
	return result, nil
}

func (c *Client) RecoverTaskProcessStopped(ctx context.Context, runtimeFence app.AccountAgentRuntimeFence, taskFence app.AgentInstanceFence, nonceSHA256 string) (app.AccountAgentTaskStopAttestation, error) {
	request := struct {
		RuntimeFence app.AccountAgentRuntimeFence `json:"runtime_fence"`
		TaskFence    app.AgentInstanceFence       `json:"task_fence"`
		NonceSHA256  string                       `json:"nonce_sha256"`
	}{RuntimeFence: runtimeFence, TaskFence: taskFence, NonceSHA256: nonceSHA256}
	var result app.AccountAgentTaskStopAttestation
	if err := c.do(ctx, http.MethodPost, "/v1/task-processes/recover-stop", request, &result, http.StatusOK); err != nil {
		return app.AccountAgentTaskStopAttestation{}, err
	}
	return result, nil
}

func (c *Client) AcknowledgeTaskProcessStopped(ctx context.Context, runtimeFence app.AccountAgentRuntimeFence,
	taskFence app.AgentInstanceFence, nonceSHA256, proofSHA256 string) error {
	request := struct {
		RuntimeFence app.AccountAgentRuntimeFence `json:"runtime_fence"`
		TaskFence    app.AgentInstanceFence       `json:"task_fence"`
		NonceSHA256  string                       `json:"nonce_sha256"`
		ProofSHA256  string                       `json:"proof_sha256"`
	}{RuntimeFence: runtimeFence, TaskFence: taskFence, NonceSHA256: nonceSHA256, ProofSHA256: proofSHA256}
	return c.do(ctx, http.MethodPost, "/v1/task-processes/ack-stop", request, nil, http.StatusNoContent)
}

func taskProcessPath(guard TaskProcessGuard, operation string) string {
	return "/v1/task-processes/" + guard.RuntimeContainerID + "/" + taskProcessKey(guard) + "/" + operation
}

func (c *Client) do(ctx context.Context, method, path string, body any, result any, wantStatus int) error {
	if ctx == nil {
		return errors.New("guardian client context is required")
	}
	data, err := json.Marshal(body)
	if err != nil {
		return errors.New("encode guardian request")
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://guardian"+path, bytes.NewReader(data))
	if err != nil {
		return errors.New("build guardian request")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("guardian Unix socket request failed: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	responseData, err := io.ReadAll(io.LimitReader(response.Body, maxRequestBytes+1))
	if err != nil || len(responseData) > maxRequestBytes {
		return errors.New("guardian response is invalid")
	}
	if response.StatusCode != wantStatus {
		return fmt.Errorf("guardian returned status %d", response.StatusCode)
	}
	if result != nil {
		if err := decodeStrictJSON(responseData, result); err != nil {
			return errors.New("guardian response JSON is invalid")
		}
	}
	return nil
}
func unixListener(path string, socketMode os.FileMode) (*net.UnixListener, func(), error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, nil, errors.New("guardian socket path must be a clean absolute path")
	}
	if socketMode != 0o600 && socketMode != 0o660 {
		return nil, nil, errors.New("guardian socket mode must be 0600 or 0660")
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 || parent.Mode().Perm()&0o022 != 0 {
		return nil, nil, errors.New("guardian socket parent must be a trusted non-writable directory")
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, nil, errors.New("guardian socket path already exists; inspect it before removing it")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, fmt.Errorf("inspect guardian socket path: %w", err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, nil, fmt.Errorf("listen on guardian Unix socket: %w", err)
	}
	listener.SetUnlinkOnClose(false)
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		_ = listener.Close()
		return nil, nil, errors.New("created guardian endpoint is not a Unix socket")
	}
	if err := os.Chmod(path, socketMode); err != nil {
		_ = listener.Close()
		current, statErr := os.Lstat(path)
		if statErr == nil && os.SameFile(info, current) {
			_ = os.Remove(path)
		}
		return nil, nil, fmt.Errorf("restrict guardian socket permissions: %w", err)
	}
	cleanup := func() {
		_ = listener.Close()
		current, statErr := os.Lstat(path)
		if statErr == nil && os.SameFile(info, current) {
			_ = os.Remove(path)
		}
	}
	return listener, cleanup, nil
}

func (s *Service) Serve(ctx context.Context, socket string, socketMode os.FileMode, interval time.Duration, onError func(error)) error {
	if ctx == nil {
		return errors.New("guardian server context is required")
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	listener, cleanup, err := unixListener(socket, socketMode)
	if err != nil {
		return err
	}
	defer cleanup()
	server := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	serverErrors := make(chan error, 1)
	go func() { serverErrors <- server.Serve(listener) }()
	runErrors := make(chan error, 1)
	go func() { runErrors <- s.Run(runCtx, interval, onError) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return ctx.Err()
	case err := <-serverErrors:
		cancelRun()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("guardian HTTP server stopped: %w", err)
	case err := <-runErrors:
		cancelRun()
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("guardian expiry monitor stopped: %w", err)
	}
}
