package mcpconnector

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	connectorResponseLimit = 1 << 20
	connectorToolLimit     = 256
	connectorPageLimit     = 8
	connectorCallTimeout   = 30 * time.Second
)

var connectorDNSName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)

type ConnectorTool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

type ConnectorToolResult struct {
	IsError            bool
	UnsupportedContent bool
	StructuredContent  json.RawMessage
	Text               []string
}

type ConnectorClient struct {
	transport http.RoundTripper
}

func NewConnectorClient() *ConnectorClient { return &ConnectorClient{} }

func (c *ConnectorClient) ListTools(ctx context.Context, endpoint string, headers http.Header) ([]ConnectorTool, error) {
	session, closeSession, err := c.connect(ctx, endpoint, headers)
	if err != nil {
		return nil, err
	}
	defer closeSession()

	var tools []ConnectorTool
	cursor := ""
	for page := 0; page < connectorPageLimit; page++ {
		result, err := session.ListTools(ctx, &sdk.ListToolsParams{Cursor: cursor})
		if err != nil || result == nil || len(result.Tools) > connectorToolLimit-len(tools) {
			return nil, errors.New("MCP connector tool listing failed or exceeded its limit")
		}
		for _, tool := range result.Tools {
			if tool == nil || tool.Name == "" || len(tool.Name) > 128 || !utf8.ValidString(tool.Name) ||
				len(tool.Description) > 4096 || !utf8.ValidString(tool.Description) || tool.InputSchema == nil {
				return nil, errors.New("MCP connector returned an invalid tool definition")
			}
			schema, err := json.Marshal(tool.InputSchema)
			if err != nil || len(schema) == 0 || len(schema) > 64<<10 || !jsonObject(schema) {
				return nil, errors.New("MCP connector returned an invalid tool schema")
			}
			for _, existing := range tools {
				if existing.Name == tool.Name {
					return nil, errors.New("MCP connector returned duplicate tool names")
				}
			}
			tools = append(tools, ConnectorTool{Name: tool.Name, Description: tool.Description, InputSchema: schema})
		}
		if result.NextCursor == "" {
			return tools, nil
		}
		if len(result.NextCursor) > 4096 || result.NextCursor == cursor {
			return nil, errors.New("MCP connector returned an invalid page cursor")
		}
		cursor = result.NextCursor
	}
	return nil, errors.New("MCP connector tool listing exceeded its page limit")
}

func (c *ConnectorClient) CallTool(ctx context.Context, endpoint string, headers http.Header, name string, arguments json.RawMessage) (ConnectorToolResult, error) {
	if name == "" || len(name) > 128 || !utf8.ValidString(name) || !jsonObject(arguments) || len(arguments) > 32<<10 {
		return ConnectorToolResult{}, errors.New("MCP connector tool call is invalid")
	}
	session, closeSession, err := c.connect(ctx, endpoint, headers)
	if err != nil {
		return ConnectorToolResult{}, err
	}
	defer closeSession()

	var input any
	decoder := json.NewDecoder(strings.NewReader(string(arguments)))
	decoder.UseNumber()
	if err := decoder.Decode(&input); err != nil {
		return ConnectorToolResult{}, errors.New("MCP connector tool arguments are invalid")
	}
	result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: input})
	if err != nil || result == nil {
		return ConnectorToolResult{}, errors.New("MCP connector tool call failed without a confirmed result")
	}
	value := ConnectorToolResult{IsError: result.IsError, Text: make([]string, 0, len(result.Content))}
	if len(result.Content) > 256 {
		value.UnsupportedContent = true
		return value, nil
	}
	resultBytes := 0
	for _, content := range result.Content {
		text, ok := content.(*sdk.TextContent)
		if !ok || text == nil {
			value.UnsupportedContent = true
			continue
		}
		if len(text.Text) > 64<<10 || !utf8.ValidString(text.Text) {
			value.UnsupportedContent = true
			value.Text = nil
			return value, nil
		}
		resultBytes += len(text.Text)
		if resultBytes > 64<<10 {
			value.UnsupportedContent = true
			value.Text = nil
			return value, nil
		}
		value.Text = append(value.Text, text.Text)
	}
	if result.StructuredContent != nil {
		encoded, err := json.Marshal(result.StructuredContent)
		if err != nil || len(encoded) > 64<<10 || !utf8.Valid(encoded) {
			value.UnsupportedContent = true
			value.Text = nil
			return value, nil
		}
		resultBytes += len(encoded)
		if resultBytes > 64<<10 {
			value.UnsupportedContent = true
			value.Text = nil
			return value, nil
		}
		value.StructuredContent = encoded
	}
	return value, nil
}

func (c *ConnectorClient) connect(ctx context.Context, endpoint string, headers http.Header) (*sdk.ClientSession, func(), error) {
	parsed, err := validConnectorEndpoint(endpoint)
	if err != nil || ctx == nil {
		return nil, nil, errors.New("MCP connector endpoint or request context is invalid")
	}
	if err := validateConnectorHeaders(headers); err != nil {
		return nil, nil, err
	}
	transport := c.transport
	if transport == nil {
		transport = newConnectorTransport(parsed.Hostname())
	}
	client := &http.Client{
		Transport: connectorHTTPRoundTripper{next: transport, headers: headers.Clone()},
		Timeout:   connectorCallTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	clientTransport := &sdk.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: client, DisableStandaloneSSE: true, MaxRetries: -1,
		MaxEventSize: connectorResponseLimit,
	}
	mcpClient := sdk.NewClient(&sdk.Implementation{Name: "morde-cloud", Version: "1"}, &sdk.ClientOptions{
		Capabilities:   &sdk.ClientCapabilities{},
		MultiRoundTrip: &sdk.MultiRoundTripOptions{Disabled: true},
	})
	callCtx, cancel := context.WithTimeout(ctx, connectorCallTimeout)
	session, err := mcpClient.Connect(callCtx, clientTransport, nil)
	cancel()
	if err != nil {
		return nil, nil, errors.New("MCP connector connection failed")
	}
	return session, func() { _ = session.Close() }, nil
}

func validConnectorEndpoint(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Opaque != "" || parsed.Hostname() == "" || parsed.Port() != "" && parsed.Port() != "443" {
		return nil, errors.New("invalid MCP connector endpoint")
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host != parsed.Hostname() || !validConnectorDNSName(host) || net.ParseIP(host) != nil {
		return nil, errors.New("invalid MCP connector host")
	}
	return parsed, nil
}

func validateConnectorHeaders(headers http.Header) error {
	count, total := 0, 0
	for name, values := range headers {
		canonical := http.CanonicalHeaderKey(name)
		if canonical == "" || canonical != name || !validConnectorHeaderName(name) || canonical == "Host" || canonical == "Cookie" || canonical == "Set-Cookie" ||
			canonical == "Content-Length" || canonical == "Transfer-Encoding" || canonical == "Connection" ||
			canonical == "Proxy-Authorization" || strings.HasPrefix(canonical, "Proxy-") || len(values) != 1 {
			return errors.New("MCP connector credential headers are invalid")
		}
		count++
		for _, value := range values {
			if len(value) > 4096 {
				return errors.New("MCP connector credential headers are invalid")
			}
			for _, char := range value {
				if char < 0x20 && char != '\t' || char == 0x7f {
					return errors.New("MCP connector credential headers are invalid")
				}
			}
			total += len(name) + len(value)
		}
	}
	if count > 4 || total > 8192 {
		return errors.New("MCP connector credential headers exceed their limit")
	}
	return nil
}

func validConnectorHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, char := range name {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' ||
			strings.ContainsRune("!#$%&'*+-.^_`|~", char) {
			continue
		}
		return false
	}
	return true
}

type connectorHTTPRoundTripper struct {
	next    http.RoundTripper
	headers http.Header
}

func (r connectorHTTPRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	for name, values := range r.headers {
		if clone.Header.Values(name) != nil {
			return nil, errors.New("MCP connector request contains a conflicting credential header")
		}
		for _, value := range values {
			clone.Header.Add(name, value)
		}
	}
	response, err := r.next.RoundTrip(clone)
	if err != nil || response == nil {
		return response, err
	}
	if response.ContentLength > connectorResponseLimit {
		_ = response.Body.Close()
		return nil, errors.New("MCP connector response exceeds its limit")
	}
	response.Body = &connectorLimitedBody{ReadCloser: response.Body}
	return response, nil
}

type connectorLimitedBody struct {
	io.ReadCloser
	read int64
}

func (body *connectorLimitedBody) Read(value []byte) (int, error) {
	remaining := int64(connectorResponseLimit+1) - body.read
	if remaining <= 0 {
		return 0, errors.New("MCP connector response exceeds its limit")
	}
	if int64(len(value)) > remaining {
		value = value[:remaining]
	}
	count, err := body.ReadCloser.Read(value)
	body.read += int64(count)
	if body.read > connectorResponseLimit {
		return 0, errors.New("MCP connector response exceeds its limit")
	}
	return count, err
}

func newConnectorTransport(host string) http.RoundTripper {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			resolvedHost, port, err := net.SplitHostPort(address)
			if err != nil || strings.ToLower(resolvedHost) != host || port != "443" {
				return nil, errors.New("MCP connector address is invalid")
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil || len(ips) == 0 {
				return nil, errors.New("MCP connector DNS lookup failed")
			}
			for _, address := range ips {
				if !publicConnectorIP(address) {
					return nil, errors.New("MCP connector DNS resolved to a non-public address")
				}
			}
			var lastErr error
			for _, address := range ips {
				connection, err := dialer.DialContext(ctx, network, net.JoinHostPort(address.String(), port))
				if err == nil {
					return connection, nil
				}
				lastErr = err
			}
			return nil, lastErr
		},
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   2,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
}

func publicConnectorIP(value netip.Addr) bool {
	value = value.Unmap()
	if !value.IsValid() || !value.IsGlobalUnicast() || value.IsPrivate() || value.IsLoopback() ||
		value.IsLinkLocalUnicast() || value.IsMulticast() || value.IsUnspecified() {
		return false
	}
	for _, prefix := range connectorNonPublicPrefixes {
		if prefix.Contains(value) {
			return false
		}
	}
	return true
}

var connectorNonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("168.63.129.16/32"),
	netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("100::/64"), netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
}

func jsonObject(value []byte) bool {
	var object map[string]json.RawMessage
	return len(value) > 0 && utf8.Valid(value) && json.Unmarshal(value, &object) == nil && object != nil
}

func validConnectorDNSName(host string) bool {
	if !connectorDNSName.MatchString(host) {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
	}
	return true
}
