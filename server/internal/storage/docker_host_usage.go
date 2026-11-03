package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"morde/server/internal/app"
	"morde/server/internal/dockerapi"
	"morde/server/internal/domain/host"
)

const (
	dockerHostUsageTimeout = 5 * time.Second
	maxDockerHostUsageList = 8 << 20
	maxDockerHostUsageStat = 64 << 10
)

var dockerContainerIDPattern = regexp.MustCompile(`^[a-f0-9]{12,64}$`)

// SampleHostUsage reads one cgroup-scoped point-in-time snapshot from the
// unique running Docker container that carries the expected OpenSandbox and
// Morde identity labels. It deliberately does not convert a single snapshot
// into usage or a price.
func (d *Docker) SampleHostUsage(ctx context.Context, _ string, instance, hostID string, generation int64, sandboxID string) (host.ResourceMetrics, error) {
	if d == nil || d.client == nil || d.client.Transport == nil || ctx == nil || instance == "" || hostID == "" || generation < 1 || sandboxID == "" {
		return host.ResourceMetrics{}, app.ErrHostUsageUnavailable
	}

	client := *d.client
	client.Timeout = dockerHostUsageTimeout
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resolver := dockerapi.NewEndpointResolver(&client)
	requestCtx, cancel := context.WithTimeout(ctx, dockerHostUsageTimeout)
	defer cancel()

	var containers []dockerUsageContainer
	if err := getDockerHostUsageJSON(requestCtx, &client, resolver, "/containers/json?all=1", maxDockerHostUsageList, &containers); err != nil {
		return host.ResourceMetrics{}, app.ErrHostUsageUnavailable
	}
	containerID, err := uniqueRunningUsageContainer(containers, instance, hostID, generation, sandboxID)
	if err != nil {
		return host.ResourceMetrics{}, err
	}

	path := "/containers/" + url.PathEscape(containerID) + "/stats?stream=false"
	var raw dockerUsageStats
	if err := getDockerHostUsageJSON(requestCtx, &client, resolver, path, maxDockerHostUsageStat, &raw); err != nil {
		return host.ResourceMetrics{}, app.ErrHostUsageUnavailable
	}
	metrics, err := parseDockerHostUsageStats(raw)
	if err != nil {
		return host.ResourceMetrics{}, app.ErrHostUsageUnavailable
	}
	metrics.ContainerID = containerID
	if err := metrics.Validate(); err != nil {
		return host.ResourceMetrics{}, app.ErrHostUsageUnavailable
	}
	return metrics, nil
}

type dockerUsageContainer struct {
	ID     string            `json:"Id"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

func uniqueRunningUsageContainer(containers []dockerUsageContainer, instance, hostID string, generation int64, sandboxID string) (string, error) {
	var running []dockerUsageContainer
	for _, container := range containers {
		if container.Labels["opensandbox.io/id"] != sandboxID || container.State != "running" {
			continue
		}
		running = append(running, container)
	}
	if len(running) != 1 {
		return "", app.ErrHostUsageUnavailable
	}
	container := running[0]
	if !dockerContainerIDPattern.MatchString(container.ID) {
		return "", app.ErrHostUsageUnavailable
	}
	if container.Labels["ch-host"] != hostID || container.Labels["ch-env"] != instance {
		return "", app.ErrHostUsageUnavailable
	}
	if container.Labels["ch-generation"] != strconv.FormatInt(generation, 10) {
		return "", app.ErrHostUsageGenerationChanged
	}
	return container.ID, nil
}

type dockerUsageStats struct {
	Read        string          `json:"read"`
	CPUStats    json.RawMessage `json:"cpu_stats"`
	MemoryStats json.RawMessage `json:"memory_stats"`
}

func parseDockerHostUsageStats(raw dockerUsageStats) (host.ResourceMetrics, error) {
	if raw.Read == "" || len(raw.CPUStats) == 0 || len(raw.MemoryStats) == 0 {
		return host.ResourceMetrics{}, errors.New("Docker usage snapshot is incomplete")
	}
	sampledAt, err := time.Parse(time.RFC3339Nano, raw.Read)
	if err != nil || sampledAt.IsZero() {
		return host.ResourceMetrics{}, errors.New("Docker usage snapshot time is invalid")
	}
	var cpuStats struct {
		CPUUsage json.RawMessage `json:"cpu_usage"`
	}
	if err := json.Unmarshal(raw.CPUStats, &cpuStats); err != nil || len(cpuStats.CPUUsage) == 0 {
		return host.ResourceMetrics{}, errors.New("Docker CPU usage snapshot is invalid")
	}
	var cpuUsage struct {
		TotalUsage json.RawMessage `json:"total_usage"`
	}
	if err := json.Unmarshal(cpuStats.CPUUsage, &cpuUsage); err != nil || len(cpuUsage.TotalUsage) == 0 {
		return host.ResourceMetrics{}, errors.New("Docker CPU counter is missing")
	}
	var memoryStats struct {
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(raw.MemoryStats, &memoryStats); err != nil || len(memoryStats.Usage) == 0 {
		return host.ResourceMetrics{}, errors.New("Docker memory usage snapshot is invalid")
	}
	cpuUsageTotalNS, err := parseDockerUnsignedCounter(cpuUsage.TotalUsage)
	if err != nil || cpuUsageTotalNS > uint64(^uint64(0)>>1) {
		return host.ResourceMetrics{}, errors.New("Docker CPU counter is outside supported range")
	}
	memoryUsageBytes, err := parseDockerUnsignedCounter(memoryStats.Usage)
	if err != nil || memoryUsageBytes > uint64(^uint64(0)>>1) {
		return host.ResourceMetrics{}, errors.New("Docker memory usage is outside supported range")
	}
	metrics := host.ResourceMetrics{
		SampledAt: sampledAt.UTC(), CPUUsageTotalNS: int64(cpuUsageTotalNS), MemoryUsageBytes: int64(memoryUsageBytes),
	}
	metrics.RawPayload, err = json.Marshal(struct {
		CPUUsageTotalNS  uint64 `json:"cpu_usage_total_ns"`
		MemoryUsageBytes uint64 `json:"memory_usage_bytes"`
	}{cpuUsageTotalNS, memoryUsageBytes})
	if err != nil {
		return host.ResourceMetrics{}, errors.New("encode Docker usage sample")
	}
	return metrics, nil
}

func parseDockerUnsignedCounter(raw json.RawMessage) (uint64, error) {
	value := strings.TrimSpace(string(raw))
	if value == "" || strings.HasPrefix(value, "-") || strings.ContainsAny(value, ".eE+\"") {
		return 0, errors.New("Docker usage counter is not an unsigned integer")
	}
	return strconv.ParseUint(value, 10, 64)
}

func getDockerHostUsageJSON(ctx context.Context, client *http.Client, resolver *dockerapi.EndpointResolver, path string, maxBytes int64, destination any) error {
	endpoint, err := resolver.Endpoint(ctx, path)
	if err != nil {
		return fmt.Errorf("resolve Docker usage endpoint: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("build Docker usage request")
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("request Docker usage: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return fmt.Errorf("Docker usage endpoint returned HTTP %d", response.StatusCode)
	}
	contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		return errors.New("Docker usage endpoint returned a non-JSON response")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil || len(body) == 0 || int64(len(body)) > maxBytes {
		return errors.New("Docker usage response is empty, unreadable, or too large")
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return errors.New("Docker usage response is invalid JSON")
	}
	return nil
}

var _ app.HostUsageSampler = (*Docker)(nil)
