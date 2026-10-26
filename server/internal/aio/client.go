package aio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"morde/server/internal/app"
)

const HeaderAPIKey = "X-AIO-API-Key"

const maxJSONResponseBytes = 64 << 10

type Client struct {
	http *http.Client
}

func New(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{http: httpClient}
}

func (c *Client) DoJSON(ctx context.Context, upstream app.Upstream, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode aio request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, path, body)
	if err != nil {
		return fmt.Errorf("build aio request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.Do(ctx, upstream, req)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("aio returned status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxJSONResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read aio response: %w", err)
	}
	if len(data) > maxJSONResponseBytes {
		return errors.New("aio response is too large")
	}
	var envelope struct {
		Success bool            `json:"success"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
		Hint    string          `json:"hint"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("decode aio response: %w", err)
	}
	if !envelope.Success {
		message := strings.TrimSpace(envelope.Message)
		if upstream.APIKey != "" {
			message = strings.ReplaceAll(message, upstream.APIKey, "[redacted]")
		}
		if message == "" {
			message = "request failed"
		}
		return fmt.Errorf("aio request failed: %s", message)
	}
	if output != nil && len(envelope.Data) != 0 && string(envelope.Data) != "null" {
		if err := json.Unmarshal(envelope.Data, output); err != nil {
			return fmt.Errorf("decode aio data: %w", err)
		}
	}
	return nil
}

func (c *Client) Do(ctx context.Context, upstream app.Upstream, request *http.Request) (*http.Response, error) {
	if request == nil || request.URL == nil {
		return nil, errors.New("aio request is required")
	}
	url, err := joinURL(upstream.BaseURL, request.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid aio upstream: %w", err)
	}
	cloned := request.Clone(ctx)
	cloned.URL = url
	cloned.Host = ""
	cloned.Header = request.Header.Clone()
	for name, value := range upstream.Headers {
		cloned.Header.Set(name, value)
	}
	if upstream.APIKey != "" {
		cloned.Header.Set(HeaderAPIKey, upstream.APIKey)
	}
	response, err := c.http.Do(cloned)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("aio %s request failed: %w", strings.ToLower(cloned.Method), err)
	}
	return response, nil
}

func (c *Client) Ready(ctx context.Context, upstream app.Upstream) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "/v1/shell/sessions", nil)
	if err != nil {
		return fmt.Errorf("build aio readiness request: %w", err)
	}
	response, err := c.Do(ctx, upstream, request)
	if err != nil {
		return fmt.Errorf("check aio readiness: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("aio readiness returned status %d", response.StatusCode)
	}
	return nil
}

// TerminalSessionID asks AIO to create a terminal and accepts only its fixed
// same-origin /terminal path with one safe session_id query parameter.
func (c *Client) TerminalSessionID(ctx context.Context, upstream app.Upstream) (string, error) {
	var terminalURL string
	if err := c.DoJSON(ctx, upstream, http.MethodGet, "/v1/shell/terminal-url", nil, &terminalURL); err != nil {
		return "", fmt.Errorf("create aio terminal session: %w", err)
	}
	sessionID, err := parseTerminalSessionID(upstream.BaseURL, terminalURL)
	if err != nil {
		return "", fmt.Errorf("invalid aio terminal url: %w", err)
	}
	return sessionID, nil
}

func parseTerminalSessionID(baseURL, raw string) (string, error) {
	base, err := url.Parse(baseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil {
		return "", errors.New("invalid aio base url")
	}
	terminal, err := url.Parse(raw)
	if err != nil || terminal.Opaque != "" || terminal.Fragment != "" || terminal.Path != "/terminal" || terminal.EscapedPath() != "/terminal" {
		return "", errors.New("terminal path is invalid")
	}
	if terminal.IsAbs() && (terminal.User != nil || !strings.EqualFold(terminal.Scheme, base.Scheme) || !strings.EqualFold(terminal.Host, base.Host)) {
		return "", errors.New("terminal origin is invalid")
	}
	if terminal.Host != "" && !terminal.IsAbs() {
		return "", errors.New("terminal origin is invalid")
	}
	query, err := url.ParseQuery(terminal.RawQuery)
	if err != nil || len(query) != 1 || len(query["session_id"]) != 1 {
		return "", errors.New("terminal query is invalid")
	}
	sessionID := query.Get("session_id")
	if !validTerminalSessionID(sessionID) {
		return "", errors.New("terminal session id is invalid")
	}
	return sessionID, nil
}

func validTerminalSessionID(value string) bool {
	if len(value) == 0 || len(value) > 255 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune("-_.~", r):
		default:
			return false
		}
	}
	return true
}

func joinURL(baseURL string, requestURL *url.URL) (*url.URL, error) {
	base, err := url.Parse(baseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || base.User != nil {
		return nil, errors.New("base url must be an absolute http(s) url")
	}
	if requestURL.IsAbs() || requestURL.Host != "" {
		return nil, errors.New("request path must be relative to the upstream")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/" + strings.TrimLeft(requestURL.Path, "/")
	base.RawPath = ""
	base.RawQuery = requestURL.RawQuery
	base.Fragment = ""
	return base, nil
}

var _ app.HostProbe = (*Client)(nil)
