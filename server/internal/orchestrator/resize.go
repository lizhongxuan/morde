package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"morde/server/internal/app"
)

type liveResourceLimits struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

type resourceStatusResponse struct {
	ResourceLimits liveResourceLimits `json:"resourceLimits"`
	ContainerID    string             `json:"containerId"`
	StartedAt      string             `json:"startedAt"`
}

func parseResourceStatus(response resourceStatusResponse) (app.SandboxResourceStatus, error) {
	cpuText, ok := strings.CutSuffix(response.ResourceLimits.CPU, "m")
	if !ok {
		return app.SandboxResourceStatus{}, app.ErrSandboxUnavailable
	}
	cpu, cpuErr := strconv.Atoi(cpuText)
	memoryText, ok := strings.CutSuffix(response.ResourceLimits.Memory, "Mi")
	if !ok {
		return app.SandboxResourceStatus{}, app.ErrSandboxUnavailable
	}
	memory, memoryErr := strconv.Atoi(memoryText)
	if cpuErr != nil || memoryErr != nil || cpu < 1 || memory < 1 || response.ContainerID == "" || response.StartedAt == "" {
		return app.SandboxResourceStatus{}, app.ErrSandboxUnavailable
	}
	return app.SandboxResourceStatus{
		Resources:   app.HostResources{CPUMilli: cpu, MemMiB: memory},
		ContainerID: response.ContainerID, StartedAt: response.StartedAt,
	}, nil
}

func (c *Client) ReadResources(ctx context.Context, id string) (result app.SandboxResourceStatus, err error) {
	err = c.observe("read_resources", func() error {
		if c.checkNetworkPolicy == nil || c.checkNetworkPolicy(ctx, c.profile.NetworkGuardPath, c.profile.BaseURL) != nil {
			return app.ErrSandboxUnavailable
		}
		response, callErr := c.do(ctx, http.MethodGet, c.sandboxURL(id)+"/resources", nil)
		if callErr != nil {
			return callErr
		}
		defer func() { _ = response.Body.Close() }()
		var body resourceStatusResponse
		if callErr = decodeJSON(response.Body, &body); callErr != nil {
			return callErr
		}
		result, callErr = parseResourceStatus(body)
		return callErr
	})
	return result, err
}

func (c *Client) UpdateResources(ctx context.Context, id string, expected app.SandboxResourceStatus, target app.HostResources) (result app.SandboxResourceStatus, err error) {
	err = c.observe("update_resources", func() error {
		if c.checkNetworkPolicy == nil || c.checkNetworkPolicy(ctx, c.profile.NetworkGuardPath, c.profile.BaseURL) != nil {
			return app.ErrSandboxUnavailable
		}
		if target.CPUMilli < 1 || target.MemMiB < 1 || expected.Resources.CPUMilli < 1 || expected.Resources.MemMiB < 1 || expected.ContainerID == "" || expected.StartedAt == "" {
			return app.ErrSandboxConflict
		}
		limits := func(resources app.HostResources) liveResourceLimits {
			return liveResourceLimits{CPU: fmt.Sprintf("%dm", resources.CPUMilli), Memory: fmt.Sprintf("%dMi", resources.MemMiB)}
		}
		body, callErr := json.Marshal(struct {
			ResourceLimits         liveResourceLimits `json:"resourceLimits"`
			ExpectedResourceLimits liveResourceLimits `json:"expectedResourceLimits"`
			ExpectedContainerID    string             `json:"expectedContainerId"`
			ExpectedStartedAt      string             `json:"expectedStartedAt"`
		}{limits(target), limits(expected.Resources), expected.ContainerID, expected.StartedAt})
		if callErr != nil {
			return callErr
		}
		response, callErr := c.do(ctx, http.MethodPut, c.sandboxURL(id)+"/resources", body)
		if callErr != nil {
			return callErr
		}
		defer func() { _ = response.Body.Close() }()
		var decoded resourceStatusResponse
		if callErr = decodeJSON(response.Body, &decoded); callErr != nil {
			return callErr
		}
		result, callErr = parseResourceStatus(decoded)
		return callErr
	})
	return result, err
}

var _ app.SandboxResourceClient = (*Client)(nil)
