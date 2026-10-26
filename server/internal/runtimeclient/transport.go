package runtimeclient

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"morde-runtime/runtimeidentity"
)

// NewHostHTTPClient creates a private TLS transport pinned to one Host's
// runtime identity and retains only non-sensitive OpenSandbox routing headers.
// Proxy environment variables are deliberately ignored for this private hop.
func NewHostHTTPClient(credential, hostID string, routingHeaders map[string]string) (*http.Client, error) {
	tlsConfig, err := runtimeidentity.ClientTLSConfig(credential, hostID)
	if err != nil {
		return nil, errors.New("runtime TLS identity is invalid")
	}
	headers, err := cleanRoutingHeaders(routingHeaders)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy:                 nil,
		TLSClientConfig:       tlsConfig,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: requestTimeout,
	}
	return &http.Client{
		Transport: routingHeaderTransport{base: transport, headers: headers},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			// Runtime routing headers are scoped to the pinned Host endpoint.
			// Never forward them to a redirect target or downgrade HTTPS.
			return http.ErrUseLastResponse
		},
	}, nil
}

type routingHeaderTransport struct {
	base    http.RoundTripper
	headers http.Header
}

func (t routingHeaderTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	copy.Header = request.Header.Clone()
	for key, values := range t.headers {
		if copy.Header.Values(key) != nil {
			continue
		}
		for _, value := range values {
			copy.Header.Add(key, value)
		}
	}
	return t.base.RoundTrip(copy)
}

func cleanRoutingHeaders(input map[string]string) (http.Header, error) {
	result := make(http.Header, len(input))
	for key, value := range input {
		canonical := http.CanonicalHeaderKey(strings.TrimSpace(key))
		if canonical == "" || strings.ContainsAny(key, "\r\n:") || strings.ContainsAny(value, "\r\n") {
			return nil, errors.New("runtime routing header is invalid")
		}
		switch strings.ToLower(canonical) {
		case "authorization", "x-morde-runtime-protocol", "host", "content-length", "connection", "transfer-encoding":
			return nil, errors.New("runtime routing header conflicts with protocol headers")
		}
		result.Add(canonical, value)
	}
	return result, nil
}
