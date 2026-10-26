package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"morde/server/internal/app"
	"morde/server/internal/config"
)

const (
	apiPrefix    = "/v1"
	apiKeyHeader = "OPEN-SANDBOX-API-KEY"
	pageSize     = 100
	maxJSONBytes = 4 << 20
)

type Client struct {
	profile            config.Profile
	http               *http.Client
	requests           *prometheus.CounterVec
	duration           *prometheus.HistogramVec
	checkNetworkPolicy func(context.Context, string, string) error
}

func NewClient(profile config.Profile, httpClient *http.Client, reg prometheus.Registerer) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cloudhost_sandbox_requests_total",
		Help: "OpenSandbox calls by runtime profile, operation, and result.",
	}, []string{"profile", "op", "result"})
	duration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "cloudhost_sandbox_request_duration_seconds",
		Help:    "OpenSandbox call duration by runtime profile and operation.",
		Buckets: prometheus.DefBuckets,
	}, []string{"profile", "op"})
	if reg != nil {
		requests = registerCounter(reg, requests)
		duration = registerHistogram(reg, duration)
	}
	return &Client{
		profile: profile, http: httpClient, requests: requests, duration: duration,
		checkNetworkPolicy: CheckNetworkPolicy,
	}
}

func registerCounter(reg prometheus.Registerer, collector *prometheus.CounterVec) *prometheus.CounterVec {
	if err := reg.Register(collector); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			if existing, ok := already.ExistingCollector.(*prometheus.CounterVec); ok {
				return existing
			}
		}
	}
	return collector
}

func registerHistogram(reg prometheus.Registerer, collector *prometheus.HistogramVec) *prometheus.HistogramVec {
	if err := reg.Register(collector); err != nil {
		var already prometheus.AlreadyRegisteredError
		if errors.As(err, &already) {
			if existing, ok := already.ExistingCollector.(*prometheus.HistogramVec); ok {
				return existing
			}
		}
	}
	return collector
}

func (c *Client) Create(ctx context.Context, input app.SandboxCreateRequest) (result app.Sandbox, err error) {
	err = c.observe("create", func() error {
		if c.checkNetworkPolicy == nil || c.checkNetworkPolicy(ctx, c.profile.NetworkGuardPath, c.profile.BaseURL) != nil {
			return fmt.Errorf("check network policy guard: %w", app.ErrSandboxUnavailable)
		}
		request := createRequest{
			Image:            image{URI: input.Image},
			Entrypoint:       input.Entrypoint,
			Timeout:          int64(input.Timeout / time.Second),
			ResourceRequests: resource(input.Requests),
			ResourceLimits:   resource(input.Limits),
			Env:              nonNilMap(input.Env),
			Metadata:         nonNilMap(input.Metadata),
			Volumes:          make([]createVolume, 0, len(input.Volumes)),
		}
		for _, volume := range input.Volumes {
			request.Volumes = append(request.Volumes, createVolume{
				Name:      volume.Name,
				MountPath: volume.MountPath,
				PVC: pvc{
					ClaimName:                  volume.ClaimName,
					CreateIfNotExists:          true,
					DeleteOnSandboxTermination: false,
					Storage:                    volume.Storage,
				},
			})
		}
		body, callErr := json.Marshal(request)
		if callErr != nil {
			return fmt.Errorf("encode sandbox create request: %w", callErr)
		}
		response, callErr := c.do(ctx, http.MethodPost, c.urlFor("/sandboxes"), body)
		if callErr != nil {
			return callErr
		}
		defer func() { _ = response.Body.Close() }()
		result, callErr = decodeSandbox(response.Body)
		return callErr
	})
	return result, err
}

func (c *Client) Get(ctx context.Context, id string) (result app.Sandbox, err error) {
	err = c.observe("get", func() error {
		return c.retry(ctx, func() error {
			response, callErr := c.do(ctx, http.MethodGet, c.sandboxURL(id), nil)
			if callErr != nil {
				return callErr
			}
			defer func() { _ = response.Body.Close() }()
			result, callErr = decodeSandbox(response.Body)
			return callErr
		})
	})
	return result, err
}

func (c *Client) List(ctx context.Context, metadata map[string]string) (result []app.Sandbox, err error) {
	err = c.observe("list", func() error {
		result = make([]app.Sandbox, 0)
		metadataFilter := url.Values{}
		keys := make([]string, 0, len(metadata))
		for key := range metadata {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			metadataFilter.Set(key, metadata[key])
		}
		query := url.Values{}
		if len(metadataFilter) != 0 {
			query.Set("metadata", metadataFilter.Encode())
		}
		for page := 1; ; page++ {
			query.Set("page", strconv.Itoa(page))
			query.Set("pageSize", strconv.Itoa(pageSize))
			pageURL := c.urlFor("/sandboxes") + "?" + query.Encode()
			var response listResponse
			callErr := c.retry(ctx, func() error {
				resp, requestErr := c.do(ctx, http.MethodGet, pageURL, nil)
				if requestErr != nil {
					return requestErr
				}
				defer func() { _ = resp.Body.Close() }()
				return decodeJSON(resp.Body, &response)
			})
			if callErr != nil {
				return callErr
			}
			for _, item := range response.Items {
				result = append(result, item.sandbox())
			}
			if !response.Pagination.HasNextPage {
				return nil
			}
			if page == int(^uint(0)>>1) {
				return fmt.Errorf("opensandbox list page overflow")
			}
		}
	})
	return result, err
}

func (c *Client) Delete(ctx context.Context, id string) error {
	return c.observe("delete", func() error {
		response, err := c.do(ctx, http.MethodDelete, c.sandboxURL(id), nil)
		if errors.Is(err, app.ErrSandboxNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return response.Body.Close()
	})
}

func (c *Client) Renew(ctx context.Context, id string, expiresAt time.Time) (result time.Time, err error) {
	err = c.observe("renew", func() error {
		request := struct {
			ExpiresAt time.Time `json:"expiresAt"`
		}{ExpiresAt: expiresAt}
		body, marshalErr := json.Marshal(request)
		if marshalErr != nil {
			return fmt.Errorf("encode sandbox renewal request: %w", marshalErr)
		}
		response, callErr := c.do(ctx, http.MethodPost, c.sandboxURL(id)+"/renew-expiration", body)
		if callErr != nil {
			return callErr
		}
		defer func() { _ = response.Body.Close() }()
		var output struct {
			ExpiresAt time.Time `json:"expiresAt"`
		}
		if callErr := decodeJSON(response.Body, &output); callErr != nil {
			return callErr
		}
		result = output.ExpiresAt
		return nil
	})
	return result, err
}

func (c *Client) Endpoint(ctx context.Context, id string, port int) (result app.Upstream, err error) {
	err = c.observe("endpoint", func() error {
		return c.retry(ctx, func() error {
			response, callErr := c.do(ctx, http.MethodGet, c.sandboxURL(id)+"/endpoints/"+strconv.Itoa(port), nil)
			if callErr != nil {
				return callErr
			}
			defer func() { _ = response.Body.Close() }()
			var output struct {
				Endpoint string            `json:"endpoint"`
				Headers  map[string]string `json:"headers"`
			}
			if callErr := decodeJSON(response.Body, &output); callErr != nil {
				return callErr
			}
			endpoint := strings.TrimSpace(output.Endpoint)
			if endpoint == "" {
				return fmt.Errorf("opensandbox endpoint is empty")
			}
			if !strings.Contains(endpoint, "://") {
				endpoint = "http://" + endpoint
			}
			parsed, parseErr := url.Parse(endpoint)
			if parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
				return fmt.Errorf("opensandbox endpoint is invalid")
			}
			if output.Headers == nil {
				output.Headers = map[string]string{}
			}
			result = app.Upstream{BaseURL: strings.TrimRight(parsed.String(), "/"), Headers: output.Headers}
			return nil
		})
	})
	return result, err
}

func (c *Client) observe(op string, call func() error) (err error) {
	start := time.Now()
	defer func() {
		result := "ok"
		switch {
		case errors.Is(err, app.ErrSandboxNotFound):
			result = "not_found"
		case errors.Is(err, app.ErrSandboxConflict):
			result = "conflict"
		case errors.Is(err, app.ErrSandboxUnsupported):
			result = "unsupported"
		case err != nil:
			result = "unavailable"
		}
		c.requests.WithLabelValues(c.profileName(), op, result).Inc()
		c.duration.WithLabelValues(c.profileName(), op).Observe(time.Since(start).Seconds())
	}()
	return call()
}

func (c *Client) profileName() string {
	return c.profile.Name
}

func (c *Client) retry(ctx context.Context, call func() error) error {
	for attempt := 0; attempt < 3; attempt++ {
		err := call()
		if !errors.Is(err, app.ErrSandboxUnavailable) || attempt == 2 {
			return err
		}
		delay := time.Duration(1<<attempt) * 50 * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, target string, body []byte) (*http.Response, error) {
	timeout := c.profile.RequestTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	req, err := http.NewRequestWithContext(requestCtx, method, target, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("build opensandbox request: %w", err)
	}
	if c.profile.APIKey != "" {
		req.Header.Set(apiKeyHeader, c.profile.APIKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(req)
	if err != nil {
		cancel()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s sandbox: %w", strings.ToLower(method), app.ErrSandboxUnavailable)
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, maxJSONBytes+1))
		_ = response.Body.Close()
		cancel()
		if readErr != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, fmt.Errorf("%s sandbox: %w", strings.ToLower(method), app.ErrSandboxUnavailable)
		}
		response.Body = io.NopCloser(bytes.NewReader(responseBody))
		return response, nil
	}
	defer cancel()
	defer func() { _ = response.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxJSONBytes))
	var targetErr error
	switch response.StatusCode {
	case http.StatusNotFound:
		targetErr = app.ErrSandboxNotFound
	case http.StatusConflict:
		targetErr = app.ErrSandboxConflict
	case http.StatusUnprocessableEntity:
		targetErr = app.ErrSandboxUnsupported
	default:
		if response.StatusCode >= 500 || response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusGatewayTimeout {
			targetErr = app.ErrSandboxUnavailable
		} else {
			targetErr = fmt.Errorf("opensandbox returned status %d", response.StatusCode)
		}
	}
	return nil, fmt.Errorf("%s sandbox: %w", strings.ToLower(method), targetErr)
}

func (c *Client) urlFor(path string) string {
	base, err := url.Parse(strings.TrimRight(c.profile.BaseURL, "/"))
	if err != nil {
		return strings.TrimRight(c.profile.BaseURL, "/") + apiPrefix + path
	}
	base.Path = strings.TrimRight(base.Path, "/") + apiPrefix + path
	base.RawPath = ""
	return base.String()
}

func (c *Client) sandboxURL(id string) string {
	base, err := url.Parse(strings.TrimRight(c.profile.BaseURL, "/"))
	if err != nil {
		return c.urlFor("/sandboxes/" + url.PathEscape(id))
	}
	prefixPath := strings.TrimRight(base.Path, "/") + apiPrefix + "/sandboxes/"
	prefixRaw := strings.TrimRight(base.EscapedPath(), "/") + apiPrefix + "/sandboxes/"
	base.Path = prefixPath + id
	base.RawPath = prefixRaw + url.PathEscape(id)
	return base.String()
}

func decodeSandbox(reader io.Reader) (app.Sandbox, error) {
	var output sandboxResponse
	if err := decodeJSON(reader, &output); err != nil {
		return app.Sandbox{}, err
	}
	return output.sandbox(), nil
}

func decodeJSON(reader io.Reader, target any) error {
	decoder := json.NewDecoder(io.LimitReader(reader, maxJSONBytes))
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode opensandbox response: %w", err)
	}
	return nil
}

type image struct {
	URI string `json:"uri"`
}

type resourceLimits struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
}

type pvc struct {
	ClaimName                  string `json:"claimName"`
	CreateIfNotExists          bool   `json:"createIfNotExists"`
	DeleteOnSandboxTermination bool   `json:"deleteOnSandboxTermination"`
	Storage                    string `json:"storage"`
}

type createVolume struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	PVC       pvc    `json:"pvc"`
}

type createRequest struct {
	Image            image             `json:"image"`
	Entrypoint       []string          `json:"entrypoint"`
	Timeout          int64             `json:"timeout"`
	ResourceRequests resourceLimits    `json:"resourceRequests"`
	ResourceLimits   resourceLimits    `json:"resourceLimits"`
	Env              map[string]string `json:"env"`
	Metadata         map[string]string `json:"metadata"`
	Volumes          []createVolume    `json:"volumes"`
}

type sandboxStatus struct {
	State  string `json:"state"`
	Reason string `json:"reason"`
}

type sandboxResponse struct {
	ID        string            `json:"id"`
	Status    sandboxStatus     `json:"status"`
	Metadata  map[string]string `json:"metadata"`
	ExpiresAt time.Time         `json:"expiresAt"`
	CreatedAt time.Time         `json:"createdAt"`
}

func (s sandboxResponse) sandbox() app.Sandbox {
	return app.Sandbox{
		ID:        s.ID,
		State:     mapState(s.Status.State),
		Reason:    s.StateReason(),
		Metadata:  nonNilMap(s.Metadata),
		ExpiresAt: s.ExpiresAt,
		CreatedAt: s.CreatedAt,
	}
}

type listResponse struct {
	Items      []sandboxResponse `json:"items"`
	Pagination struct {
		HasNextPage bool `json:"hasNextPage"`
	} `json:"pagination"`
}

func (s sandboxResponse) StateReason() string {
	if mapState(s.Status.State) == app.SandboxUnknown {
		if s.Status.Reason != "" {
			return s.Status.Reason + ": " + s.Status.State
		}
		return s.Status.State
	}
	return s.Status.Reason
}

func mapState(state string) app.SandboxState {
	switch state {
	case "Pending":
		return app.SandboxPending
	case "Running":
		return app.SandboxRunning
	case "Stopping", "Terminated":
		return app.SandboxTerminated
	case "Failed":
		return app.SandboxFailed
	default:
		return app.SandboxUnknown
	}
}

func resource(value app.SandboxResources) resourceLimits {
	return resourceLimits{CPU: value.CPU, Memory: value.Memory}
}

func nonNilMap(value map[string]string) map[string]string {
	if value == nil {
		return map[string]string{}
	}
	return value
}

var _ app.SandboxClient = (*Client)(nil)
