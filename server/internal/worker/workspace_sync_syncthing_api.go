package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	syncthingAPIRequestLimit  = 1 << 20
	syncthingAPIResponseLimit = 2 << 20
)

var (
	errSyncthingAPIURL         = errors.New("invalid syncthing API URL")
	errSyncthingAPIKey         = errors.New("syncthing API key is required")
	errSyncthingAPIRequest     = errors.New("syncthing API request failed")
	errSyncthingAPIRequestMax  = errors.New("syncthing API request exceeds limit")
	errSyncthingAPIResponseMax = errors.New("syncthing API response exceeds limit")
	errSyncthingAPIJSON        = errors.New("syncthing API returned invalid JSON")
)

// SyncthingAPIClient talks only to a loopback or private IPv4 Syncthing GUI/API endpoint.
type SyncthingAPIClient struct {
	baseURL *url.URL
	apiKey  string
	client  *http.Client
}

// NewSyncthingAPIClient creates a client bound to a validated local address.
func NewSyncthingAPIClient(rawURL, apiKey string) (*SyncthingAPIClient, error) {
	if strings.TrimSpace(apiKey) == "" || strings.ContainsAny(apiKey, "\r\n") {
		return nil, errSyncthingAPIKey
	}

	endpoint, addresses, err := parseSyncthingLocalURL(rawURL)
	if err != nil {
		return nil, errSyncthingAPIURL
	}

	dialer := &net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		DisableCompression:    true,
		MaxIdleConns:          2,
		MaxIdleConnsPerHost:   1,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   2 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: time.Second,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil || !strings.EqualFold(host, endpoint.Hostname()) || port != endpoint.Port() {
				return nil, errors.New("syncthing API connection failed")
			}
			for _, ip := range addresses {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
			}
			return nil, errors.New("syncthing API connection failed")
		},
	}
	return &SyncthingAPIClient{
		baseURL: endpoint,
		apiKey:  apiKey,
		client: &http.Client{
			Transport: transport,
			Timeout:   15 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func parseSyncthingLocalURL(rawURL string) (*url.URL, []net.IP, error) {
	u, err := url.Parse(rawURL)
	if err != nil || !u.IsAbs() || !strings.EqualFold(u.Scheme, "http") || u.Opaque != "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" || u.Host == "" || strings.HasSuffix(u.Host, ":") {
		return nil, nil, errSyncthingAPIURL
	}

	host := u.Hostname()
	if host == "" {
		return nil, nil, errSyncthingAPIURL
	}
	port := u.Port()
	if port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return nil, nil, errSyncthingAPIURL
		}
	}

	var addresses []net.IP
	switch {
	case host == "127.0.0.1":
		addresses = []net.IP{net.IPv4(127, 0, 0, 1)}
	case host == "::1":
		addresses = []net.IP{net.ParseIP("::1")}
	case strings.EqualFold(host, "localhost"):
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		resolved, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(resolved) == 0 {
			return nil, nil, errSyncthingAPIURL
		}
		for _, address := range resolved {
			if address.IP == nil || !address.IP.IsLoopback() {
				return nil, nil, errSyncthingAPIURL
			}
			addresses = append(addresses, address.IP)
		}
	default:
		ip := net.ParseIP(host)
		if ip == nil || ip.To4() == nil || !ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() ||
			ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
			return nil, nil, errSyncthingAPIURL
		}
		addresses = []net.IP{ip.To4()}
	}

	if port == "" {
		port = "80"
	}
	endpoint := &url.URL{Scheme: "http", Host: net.JoinHostPort(host, port)}
	if host == "::1" {
		endpoint.Host = "[::1]:" + port
	}
	return endpoint, addresses, nil
}

// GetFolder reads one Syncthing folder configuration.
func (c *SyncthingAPIClient) GetFolder(ctx context.Context, id string) (json.RawMessage, error) {
	if !validSyncthingPathSegment(id) {
		return nil, errors.New("invalid syncthing configuration ID")
	}
	body, err := c.request(ctx, http.MethodGet, "/rest/config/folders/"+url.PathEscape(id), nil, nil, true)
	return json.RawMessage(body), err
}

// PutFolder writes one complete Syncthing folder configuration.
func (c *SyncthingAPIClient) PutFolder(ctx context.Context, id string, config json.RawMessage) error {
	if err := validateSyncthingConfig(id, config, "id"); err != nil {
		return err
	}
	_, err := c.request(ctx, http.MethodPut, "/rest/config/folders/"+url.PathEscape(id), nil, config, false)
	return err
}

// GetDevice reads one Syncthing device configuration.
func (c *SyncthingAPIClient) GetDevice(ctx context.Context, id string) (json.RawMessage, error) {
	if !validSyncthingPathSegment(id) {
		return nil, errors.New("invalid syncthing configuration ID")
	}
	body, err := c.request(ctx, http.MethodGet, "/rest/config/devices/"+url.PathEscape(id), nil, nil, true)
	return json.RawMessage(body), err
}

// GetDevices reads every configured remote device.
func (c *SyncthingAPIClient) GetDevices(ctx context.Context) ([]json.RawMessage, error) {
	return c.getConfigList(ctx, "/rest/config/devices", "deviceID")
}

// GetFolders reads every configured folder.
func (c *SyncthingAPIClient) GetFolders(ctx context.Context) ([]json.RawMessage, error) {
	return c.getConfigList(ctx, "/rest/config/folders", "id")
}

func (c *SyncthingAPIClient) getConfigList(ctx context.Context, path, identityField string) ([]json.RawMessage, error) {
	body, err := c.request(ctx, http.MethodGet, path, nil, nil, true)
	if err != nil {
		return nil, err
	}
	var items []json.RawMessage
	if err := json.Unmarshal(body, &items); err != nil || items == nil || !json.Valid(body) {
		return nil, errSyncthingAPIJSON
	}
	for _, item := range items {
		var value map[string]json.RawMessage
		if err := json.Unmarshal(item, &value); err != nil || len(value) == 0 {
			return nil, errSyncthingAPIJSON
		}
		var id string
		if err := json.Unmarshal(value[identityField], &id); err != nil || !validSyncthingPathSegment(id) {
			return nil, errSyncthingAPIJSON
		}
	}
	return items, nil
}

// GetDefaultFolder reads Syncthing's template for newly added folders.
func (c *SyncthingAPIClient) GetDefaultFolder(ctx context.Context) (json.RawMessage, error) {
	body, err := c.request(ctx, http.MethodGet, "/rest/config/defaults/folder", nil, nil, true)
	return json.RawMessage(body), err
}

// GetDefaultDevice reads Syncthing's template for newly added devices.
func (c *SyncthingAPIClient) GetDefaultDevice(ctx context.Context) (json.RawMessage, error) {
	body, err := c.request(ctx, http.MethodGet, "/rest/config/defaults/device", nil, nil, true)
	return json.RawMessage(body), err
}

// PutDevice writes one complete Syncthing device configuration.
func (c *SyncthingAPIClient) PutDevice(ctx context.Context, id string, config json.RawMessage) error {
	if err := validateSyncthingConfig(id, config, "deviceID"); err != nil {
		return err
	}
	_, err := c.request(ctx, http.MethodPut, "/rest/config/devices/"+url.PathEscape(id), nil, config, false)
	return err
}

// PauseFolder pauses folder synchronization without replacing its other settings.
func (c *SyncthingAPIClient) PauseFolder(ctx context.Context, id string) error {
	return c.setFolderPaused(ctx, id, true)
}

// ResumeFolder resumes folder synchronization without replacing its other settings.
func (c *SyncthingAPIClient) ResumeFolder(ctx context.Context, id string) error {
	return c.setFolderPaused(ctx, id, false)
}

func (c *SyncthingAPIClient) setFolderPaused(ctx context.Context, id string, paused bool) error {
	if !validSyncthingPathSegment(id) {
		return errors.New("invalid syncthing configuration ID")
	}
	body := []byte(`{"paused":false}`)
	if paused {
		body = []byte(`{"paused":true}`)
	}
	_, err := c.request(ctx, http.MethodPatch, "/rest/config/folders/"+url.PathEscape(id), nil, body, false)
	return err
}

// RemoveFolder removes one Syncthing folder configuration.
func (c *SyncthingAPIClient) RemoveFolder(ctx context.Context, id string) error {
	if !validSyncthingPathSegment(id) {
		return errors.New("invalid syncthing configuration ID")
	}
	_, err := c.request(ctx, http.MethodDelete, "/rest/config/folders/"+url.PathEscape(id), nil, nil, false)
	return err
}

// RemoveDevice removes one Syncthing device configuration.
func (c *SyncthingAPIClient) RemoveDevice(ctx context.Context, id string) error {
	if !validSyncthingPathSegment(id) {
		return errors.New("invalid syncthing configuration ID")
	}
	_, err := c.request(ctx, http.MethodDelete, "/rest/config/devices/"+url.PathEscape(id), nil, nil, false)
	return err
}

// PauseDevice pauses a configured device and closes its active Syncthing session.
func (c *SyncthingAPIClient) PauseDevice(ctx context.Context, id string) error {
	return c.setDevicePaused(ctx, id, true)
}

// ResumeDevice resumes a configured device.
func (c *SyncthingAPIClient) ResumeDevice(ctx context.Context, id string) error {
	return c.setDevicePaused(ctx, id, false)
}

func (c *SyncthingAPIClient) setDevicePaused(ctx context.Context, id string, paused bool) error {
	if !validSyncthingPathSegment(id) {
		return errors.New("invalid syncthing configuration ID")
	}
	query := url.Values{"device": []string{id}}
	path := "/rest/system/pause"
	if !paused {
		path = "/rest/system/resume"
	}
	_, err := c.request(ctx, http.MethodPost, path, query, nil, false)
	return err
}

// SystemStatus reads Syncthing's system status.
func (c *SyncthingAPIClient) SystemStatus(ctx context.Context) (json.RawMessage, error) {
	body, err := c.request(ctx, http.MethodGet, "/rest/system/status", nil, nil, true)
	return json.RawMessage(body), err
}

// Connections reads Syncthing's current connection state by device ID.
func (c *SyncthingAPIClient) Connections(ctx context.Context) (json.RawMessage, error) {
	body, err := c.request(ctx, http.MethodGet, "/rest/system/connections", nil, nil, true)
	return json.RawMessage(body), err
}

// NodeID reads this Syncthing node's device ID from the system status.
func (c *SyncthingAPIClient) NodeID(ctx context.Context) (string, error) {
	status, err := c.SystemStatus(ctx)
	if err != nil {
		return "", err
	}
	var value struct {
		ID string `json:"myID"`
	}
	if err := json.Unmarshal(status, &value); err != nil || !validSyncthingPathSegment(value.ID) {
		return "", errSyncthingAPIJSON
	}
	return value.ID, nil
}

// GetOptions reads Syncthing's global options.
func (c *SyncthingAPIClient) GetOptions(ctx context.Context) (json.RawMessage, error) {
	body, err := c.request(ctx, http.MethodGet, "/rest/config/options", nil, nil, true)
	return json.RawMessage(body), err
}

// PatchOptions updates selected global options while preserving all other values.
func (c *SyncthingAPIClient) PatchOptions(ctx context.Context, patch json.RawMessage) error {
	if len(patch) > syncthingAPIRequestLimit {
		return errSyncthingAPIRequestMax
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(patch, &fields); err != nil || !json.Valid(patch) || len(fields) == 0 {
		return errSyncthingAPIJSON
	}
	_, err := c.request(ctx, http.MethodPatch, "/rest/config/options", nil, patch, false)
	return err
}

// ConfigurePrivateRelay disables discovery and NAT and selects only the given static relay.
func (c *SyncthingAPIClient) ConfigurePrivateRelay(ctx context.Context, relayAddress string) error {
	if !validStaticRelayAddress(relayAddress) {
		return errors.New("invalid private relay address")
	}
	patch, err := json.Marshal(struct {
		ListenAddresses       []string `json:"listenAddresses"`
		GlobalAnnounceServers []string `json:"globalAnnounceServers"`
		StunServers           []string `json:"stunServers"`
		GlobalAnnounceEnabled bool     `json:"globalAnnounceEnabled"`
		LocalAnnounceEnabled  bool     `json:"localAnnounceEnabled"`
		NATEnabled            bool     `json:"natEnabled"`
		RelaysEnabled         bool     `json:"relaysEnabled"`
	}{
		ListenAddresses:       []string{relayAddress},
		GlobalAnnounceServers: []string{},
		StunServers:           []string{},
		GlobalAnnounceEnabled: false,
		LocalAnnounceEnabled:  false,
		NATEnabled:            false,
		RelaysEnabled:         true,
	})
	if err != nil {
		return errSyncthingAPIJSON
	}
	return c.PatchOptions(ctx, patch)
}

func validStaticRelayAddress(rawAddress string) bool {
	u, err := url.Parse(rawAddress)
	if err != nil || !u.IsAbs() || !strings.EqualFold(u.Scheme, "relay") || u.Opaque != "" ||
		u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawFragment != "" ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" || strings.HasSuffix(u.Host, ":") {
		return false
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(query) != 2 || len(query["id"]) != 1 || len(query["token"]) != 1 ||
		!validSyncthingDeviceID(query.Get("id")) || !workspaceSyncRelayTokenPattern.MatchString(query.Get("token")) {
		return false
	}
	port := u.Port()
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return false
	}
	return true
}

func validateSyncthingConfig(id string, config json.RawMessage, identityField string) error {
	if !validSyncthingPathSegment(id) {
		return errors.New("invalid syncthing configuration ID")
	}
	if len(config) > syncthingAPIRequestLimit {
		return errSyncthingAPIRequestMax
	}
	var value struct {
		ID       string `json:"id"`
		DeviceID string `json:"deviceID"`
	}
	if err := json.Unmarshal(config, &value); err != nil {
		return errSyncthingAPIJSON
	}
	if !json.Valid(config) || (identityField == "id" && value.ID != id) ||
		(identityField == "deviceID" && value.DeviceID != id) {
		return errors.New("syncthing configuration ID does not match")
	}
	return nil
}

func validSyncthingPathSegment(value string) bool {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\") {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || strings.ContainsRune("-._~", r)) {
			return false
		}
	}
	return true
}

func (c *SyncthingAPIClient) request(ctx context.Context, method, path string, query url.Values, body []byte, expectJSON bool) ([]byte, error) {
	if ctx == nil {
		return nil, errors.New("syncthing API request context is required")
	}
	if len(body) > syncthingAPIRequestLimit {
		return nil, errSyncthingAPIRequestMax
	}
	endpoint := *c.baseURL
	endpoint.Path = path
	if query != nil {
		endpoint.RawQuery = query.Encode()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
	if err != nil {
		return nil, errSyncthingAPIRequest
	}
	req.Header.Set("X-API-Key", c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errSyncthingAPIRequest
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("syncthing API returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, syncthingAPIResponseLimit+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errSyncthingAPIRequest
	}
	if len(data) > syncthingAPIResponseLimit {
		return nil, errSyncthingAPIResponseMax
	}
	if expectJSON && !json.Valid(data) {
		return nil, errSyncthingAPIJSON
	}
	return data, nil
}
