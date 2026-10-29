package dockerapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

const (
	minimumClientAPIVersion = 43
	maximumClientAPIVersion = 44
)

type EndpointResolver struct {
	client  *http.Client
	mu      sync.Mutex
	version string
}

func NewEndpointResolver(client *http.Client) *EndpointResolver {
	return &EndpointResolver{client: client}
}

func (r *EndpointResolver) Endpoint(ctx context.Context, path string) (string, error) {
	if r == nil || r.client == nil || !strings.HasPrefix(path, "/") {
		return "", errors.New("docker API client or path is invalid")
	}
	version, err := r.apiVersion(ctx)
	if err != nil {
		return "", err
	}
	return "http://docker/v" + version + path, nil
}

func (r *EndpointResolver) apiVersion(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.version != "" {
		return r.version, nil
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/version", nil)
	if err != nil {
		return "", errors.New("build Docker version request")
	}
	response, err := r.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("read Docker Engine API version: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("read Docker Engine API version: engine returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 16<<10))
	if err != nil || len(data) >= 16<<10 {
		return "", errors.New("docker Engine API version response is invalid")
	}
	var version struct {
		APIVersion    string `json:"ApiVersion"`
		MinAPIVersion string `json:"MinAPIVersion"`
	}
	if err := json.Unmarshal(data, &version); err != nil {
		return "", errors.New("docker Engine API version response is invalid")
	}
	serverMax, err := parseAPIVersion(version.APIVersion)
	if err != nil {
		return "", errors.New("docker Engine API maximum version is invalid")
	}
	serverMin := 12
	if version.MinAPIVersion != "" {
		serverMin, err = parseAPIVersion(version.MinAPIVersion)
		if err != nil {
			return "", errors.New("docker Engine API minimum version is invalid")
		}
	}
	compatibleMin := max(serverMin, minimumClientAPIVersion)
	compatibleMax := min(serverMax, maximumClientAPIVersion)
	if compatibleMin > compatibleMax {
		return "", errors.New("docker Engine API version is outside the supported range")
	}
	r.version = "1." + strconv.Itoa(compatibleMax)
	return r.version, nil
}

func parseAPIVersion(value string) (int, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 || parts[0] != "1" || parts[1] == "" {
		return 0, errors.New("invalid API version")
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil || minor < 0 {
		return 0, errors.New("invalid API version")
	}
	return minor, nil
}
