package runtimebrowser

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultEndpoint     = "http://127.0.0.1:9222"
	maxCDPMessageBytes  = 8 << 20
	maxCDPHandshakeBody = 64 << 10
	cdpConnectTimeout   = 3 * time.Second
	cdpCloseWait        = 2 * time.Second
)

type targetInfo struct {
	TargetID       string `json:"targetId"`
	Type           string `json:"type"`
	Title          string `json:"title"`
	URL            string `json:"url"`
	BrowserContext string `json:"browserContextId,omitempty"`
	OpenerID       string `json:"openerId,omitempty"`
	Attached       bool   `json:"attached,omitempty"`
}

type browserConnection interface {
	Version(context.Context) (string, error)
	BrowserContexts(context.Context) ([]string, error)
	CreateBrowserContext(context.Context, bool) (string, error)
	DisposeBrowserContext(context.Context, string) error
	Targets(context.Context) ([]targetInfo, error)
	CreateTarget(context.Context, string, string, bool) (string, error)
	ActivateTarget(context.Context, string) error
	AttachTarget(context.Context, string) (targetConnection, error)
	Close() error
}

type targetConnection interface {
	Call(context.Context, string, any, any) error
	Close() error
}

type browserConnector interface {
	Connect(context.Context) (browserConnection, error)
}

func connectUntilReady(ctx context.Context, connector browserConnector, retryInterval time.Duration) (browserConnection, error) {
	if ctx == nil || connector == nil {
		return nil, errors.New("CDP connector and context are required")
	}
	if retryInterval <= 0 {
		retryInterval = 200 * time.Millisecond
	}
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			if lastErr != nil {
				return nil, fmt.Errorf("CDP endpoint did not become ready before the startup deadline: %w", lastErr)
			}
			return nil, err
		}
		connection, err := connector.Connect(ctx)
		if err == nil {
			return connection, nil
		}
		lastErr = err
		timer := time.NewTimer(retryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

type cdpConnector struct {
	endpoint string
	host     string
	port     string
	client   *http.Client
}

func newCDPConnector(rawEndpoint string) (*cdpConnector, error) {
	if rawEndpoint == "" {
		rawEndpoint = defaultEndpoint
	}
	parsed, err := url.Parse(rawEndpoint)
	if err != nil || parsed == nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" && parsed.Path != "/" || !loopbackHost(parsed.Hostname()) {
		return nil, errors.New("CDP endpoint must be a loopback HTTP URL")
	}
	base := strings.TrimRight(parsed.String(), "/")
	port := parsed.Port()
	if port == "" {
		port = "80"
	}
	return &cdpConnector{
		endpoint: base,
		host:     strings.ToLower(parsed.Hostname()),
		port:     port,
		client: &http.Client{
			Timeout: cdpConnectTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("CDP endpoint redirects are disabled")
			},
		},
	}, nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *cdpConnector) Connect(ctx context.Context) (browserConnection, error) {
	if ctx == nil {
		return nil, errors.New("CDP context is required")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/json/version", nil)
	if err != nil {
		return nil, errors.New("CDP version request could not be created")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, errors.New("CDP endpoint is unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("CDP version endpoint rejected the request")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxCDPHandshakeBody+1))
	if err != nil || len(body) == 0 || len(body) > maxCDPHandshakeBody {
		return nil, errors.New("CDP version response is invalid")
	}
	var version struct {
		Browser              string `json:"Browser"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if json.Unmarshal(body, &version) != nil || version.Browser == "" || version.WebSocketDebuggerURL == "" {
		return nil, errors.New("CDP version response is invalid")
	}
	webSocketURL, err := url.Parse(version.WebSocketDebuggerURL)
	if err != nil || webSocketURL == nil || !sameCDPAuthority(c.host, c.port, webSocketURL) {
		return nil, errors.New("CDP WebSocket authority does not match the configured endpoint")
	}
	ws, err := dialWebSocket(ctx, version.WebSocketDebuggerURL)
	if err != nil {
		return nil, errors.New("CDP browser connection failed")
	}
	return &cdpBrowser{ws: ws, browserVersion: version.Browser}, nil
}

func sameCDPAuthority(configuredHost, configuredPort string, webSocketURL *url.URL) bool {
	if webSocketURL == nil || webSocketURL.Scheme != "ws" || webSocketURL.User != nil || !loopbackHost(webSocketURL.Hostname()) {
		return false
	}
	webSocketPort := webSocketURL.Port()
	if webSocketPort == "" {
		webSocketPort = "80"
	}
	if webSocketPort != configuredPort {
		return false
	}
	advertisedHost := strings.ToLower(webSocketURL.Hostname())
	if advertisedHost == configuredHost {
		return true
	}
	// Chromium may render localhost as its canonical loopback IP (or vice
	// versa), depending on how the debugging endpoint was started. Keep this
	// alias narrow so a different loopback address cannot redirect CDP control
	// to another local service.
	if configuredHost == "localhost" {
		return isCanonicalLoopbackIP(advertisedHost)
	}
	if advertisedHost == "localhost" {
		return isCanonicalLoopbackIP(configuredHost)
	}
	return false
}

func isCanonicalLoopbackIP(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && (ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback))
}

type cdpBrowser struct {
	ws             *webSocket
	browserVersion string
}

func (c *cdpBrowser) Version(context.Context) (string, error) { return c.browserVersion, nil }

func (c *cdpBrowser) BrowserContexts(ctx context.Context) ([]string, error) {
	var response struct {
		BrowserContextIDs []string `json:"browserContextIds"`
	}
	if err := c.ws.call(ctx, "Target.getBrowserContexts", map[string]any{}, "", &response); err != nil {
		return nil, err
	}
	return response.BrowserContextIDs, nil
}

func (c *cdpBrowser) CreateBrowserContext(ctx context.Context, disposeOnDetach bool) (string, error) {
	var response struct {
		BrowserContextID string `json:"browserContextId"`
	}
	if err := c.ws.call(ctx, "Target.createBrowserContext", map[string]any{"disposeOnDetach": disposeOnDetach}, "", &response); err != nil {
		return "", err
	}
	if !validContextID(response.BrowserContextID) {
		return "", errors.New("CDP returned an invalid browser context ID")
	}
	return response.BrowserContextID, nil
}

func (c *cdpBrowser) DisposeBrowserContext(ctx context.Context, contextID string) error {
	if !validContextID(contextID) {
		return errors.New("browser context ID is invalid")
	}
	if err := c.ws.call(ctx, "Target.disposeBrowserContext", map[string]any{"browserContextId": contextID}, "", nil); err != nil {
		return err
	}
	return nil
}

func (c *cdpBrowser) Targets(ctx context.Context) ([]targetInfo, error) {
	var response struct {
		TargetInfos []targetInfo `json:"targetInfos"`
	}
	if err := c.ws.call(ctx, "Target.getTargets", map[string]any{}, "", &response); err != nil {
		return nil, err
	}
	return response.TargetInfos, nil
}

func (c *cdpBrowser) CreateTarget(ctx context.Context, contextID, pageURL string, background bool) (string, error) {
	if !validContextID(contextID) || pageURL == "" {
		return "", errors.New("target creation request is invalid")
	}
	var response struct {
		TargetID string `json:"targetId"`
	}
	params := map[string]any{"url": pageURL, "browserContextId": contextID, "background": background}
	if err := c.ws.call(ctx, "Target.createTarget", params, "", &response); err != nil {
		return "", err
	}
	if !validTargetID(response.TargetID) {
		return "", errors.New("CDP returned an invalid target ID")
	}
	return response.TargetID, nil
}

func (c *cdpBrowser) ActivateTarget(ctx context.Context, targetID string) error {
	if !validTargetID(targetID) {
		return errors.New("target ID is invalid")
	}
	return c.ws.call(ctx, "Target.activateTarget", map[string]any{"targetId": targetID}, "", nil)
}

func (c *cdpBrowser) AttachTarget(ctx context.Context, targetID string) (targetConnection, error) {
	if !validTargetID(targetID) {
		return nil, errors.New("target ID is invalid")
	}
	var response struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.ws.call(ctx, "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, "", &response); err != nil {
		return nil, err
	}
	if response.SessionID == "" {
		return nil, errors.New("CDP returned an invalid target session")
	}
	return &cdpTarget{ws: c.ws, sessionID: response.SessionID}, nil
}

func (c *cdpBrowser) Close() error { return c.ws.close() }

type cdpTarget struct {
	ws        *webSocket
	sessionID string
}

func (t *cdpTarget) Call(ctx context.Context, method string, params any, response any) error {
	return t.ws.call(ctx, method, params, t.sessionID, response)
}

func (t *cdpTarget) Close() error {
	if t == nil || t.ws == nil {
		return nil
	}
	return nil // the owning browser connection is closed by its caller
}

type webSocket struct {
	conn      net.Conn
	reader    *bufio.Reader
	writeMu   sync.Mutex
	callMu    sync.Mutex
	nextID    atomic.Uint64
	closeOnce sync.Once
	closeErr  error
}

func dialWebSocket(ctx context.Context, rawURL string) (*webSocket, error) {
	if ctx == nil {
		return nil, errors.New("CDP WebSocket context is required")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed == nil || parsed.Scheme != "ws" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path == "" || !loopbackHost(parsed.Hostname()) {
		return nil, errors.New("CDP WebSocket URL is invalid")
	}
	address := parsed.Host
	if _, _, err := net.SplitHostPort(address); err != nil {
		if strings.Contains(address, ":") {
			return nil, errors.New("CDP WebSocket address is invalid")
		}
		address = net.JoinHostPort(address, "80")
	}
	dialer := net.Dialer{Timeout: cdpConnectTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancel()
	deadline := time.Now().Add(cdpConnectTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, err
	}
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		_ = conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	if _, err := fmt.Fprintf(conn,
		"GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n",
		parsed.RequestURI(), parsed.Host, key); err != nil {
		_ = conn.Close()
		return nil, err
	}
	reader := bufio.NewReaderSize(conn, 16<<10)
	request := &http.Request{Method: http.MethodGet}
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if response.StatusCode != http.StatusSwitchingProtocols || !headerHasToken(response.Header, "Upgrade", "websocket") || !headerHasToken(response.Header, "Connection", "upgrade") {
		_ = conn.Close()
		return nil, errors.New("CDP WebSocket handshake was rejected")
	}
	digest := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	wantAccept := base64.StdEncoding.EncodeToString(digest[:])
	if response.Header.Get("Sec-WebSocket-Accept") != wantAccept {
		_ = conn.Close()
		return nil, errors.New("CDP WebSocket handshake is invalid")
	}
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if !stopCancel() {
		_ = conn.Close()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("CDP WebSocket handshake was interrupted")
	}
	return &webSocket{conn: conn, reader: reader}, nil
}

func headerHasToken(header http.Header, key, token string) bool {
	for _, value := range header.Values(key) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func (w *webSocket) call(ctx context.Context, method string, params any, sessionID string, target any) error {
	if ctx == nil || method == "" || w == nil || w.conn == nil {
		return errors.New("CDP request is invalid")
	}
	w.callMu.Lock()
	defer w.callMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	id := w.nextID.Add(1)
	request := struct {
		ID        uint64 `json:"id"`
		Method    string `json:"method"`
		Params    any    `json:"params,omitempty"`
		SessionID string `json:"sessionId,omitempty"`
	}{ID: id, Method: method, Params: params, SessionID: sessionID}
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > 1<<20 {
		return errors.New("CDP request is too large")
	}
	stopClose := context.AfterFunc(ctx, func() { _ = w.close() })
	defer stopClose()
	if deadline, ok := ctx.Deadline(); ok {
		_ = w.conn.SetDeadline(deadline)
	} else {
		_ = w.conn.SetDeadline(time.Now().Add(cdpConnectTimeout))
	}
	if err := w.writeFrame(0x1, encoded); err != nil {
		return errors.New("CDP request write failed")
	}
	for {
		payload, opcode, err := w.readMessage()
		if err != nil {
			return errors.New("CDP response read failed")
		}
		if opcode != 0x1 {
			continue
		}
		var envelope struct {
			ID        uint64          `json:"id"`
			SessionID string          `json:"sessionId"`
			Result    json.RawMessage `json:"result"`
			Error     *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(payload, &envelope) != nil || envelope.ID == 0 {
			continue // ignore asynchronous protocol events
		}
		if envelope.ID != id || envelope.SessionID != sessionID {
			continue
		}
		if envelope.Error != nil {
			return fmt.Errorf("CDP method %s failed with code %d", method, envelope.Error.Code)
		}
		if target == nil || len(envelope.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(envelope.Result, target); err != nil {
			return errors.New("CDP response shape is invalid")
		}
		return nil
	}
}

func (w *webSocket) writeFrame(opcode byte, payload []byte) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	if len(payload) > maxCDPMessageBytes {
		return errors.New("WebSocket message is too large")
	}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	frame := make([]byte, 0, len(payload)+14)
	frame = append(frame, 0x80|opcode)
	switch {
	case len(payload) < 126:
		frame = append(frame, 0x80|byte(len(payload)))
	case len(payload) <= 0xffff:
		frame = append(frame, 0x80|126, byte(len(payload)>>8), byte(len(payload)))
	default:
		frame = append(frame, 0x80|127)
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(payload)))
		frame = append(frame, length[:]...)
	}
	frame = append(frame, mask...)
	for index, value := range payload {
		frame = append(frame, value^mask[index&3])
	}
	for len(frame) > 0 {
		written, err := w.conn.Write(frame)
		if err != nil || written == 0 {
			return err
		}
		frame = frame[written:]
	}
	return nil
}

func (w *webSocket) readMessage() ([]byte, byte, error) {
	var message []byte
	var firstOpcode byte
	for {
		final, opcode, payload, err := w.readFrame()
		if err != nil {
			return nil, 0, err
		}
		switch opcode {
		case 0x8:
			return nil, 0, io.EOF
		case 0x9:
			if err := w.writeFrame(0xA, payload); err != nil {
				return nil, 0, err
			}
			continue
		case 0xA:
			continue
		case 0x1, 0x2:
			if firstOpcode != 0 || len(message) > 0 {
				return nil, 0, errors.New("interleaved WebSocket messages are unsupported")
			}
			firstOpcode = opcode
		case 0x0:
			if firstOpcode == 0 {
				return nil, 0, errors.New("unexpected WebSocket continuation")
			}
		default:
			return nil, 0, errors.New("unknown WebSocket frame opcode")
		}
		if len(message)+len(payload) > maxCDPMessageBytes {
			return nil, 0, errors.New("WebSocket message is too large")
		}
		message = append(message, payload...)
		if final {
			return message, firstOpcode, nil
		}
	}
}

func (w *webSocket) readFrame() (bool, byte, []byte, error) {
	first, err := w.reader.ReadByte()
	if err != nil {
		return false, 0, nil, err
	}
	second, err := w.reader.ReadByte()
	if err != nil {
		return false, 0, nil, err
	}
	final := first&0x80 != 0
	if first&0x70 != 0 {
		return false, 0, nil, errors.New("WebSocket extensions are unsupported")
	}
	opcode := first & 0x0f
	masked := second&0x80 != 0
	length := uint64(second & 0x7f)
	if length == 126 {
		var extended [2]byte
		if _, err := io.ReadFull(w.reader, extended[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(extended[:]))
	} else if length == 127 {
		var extended [8]byte
		if _, err := io.ReadFull(w.reader, extended[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(extended[:])
	}
	if length > maxCDPMessageBytes || (opcode >= 0x8 && (!final || length > 125)) {
		return false, 0, nil, errors.New("WebSocket frame is invalid or too large")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(w.reader, mask[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(w.reader, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for index := range payload {
			payload[index] ^= mask[index&3]
		}
	}
	return final, opcode, payload, nil
}

func (w *webSocket) close() error {
	w.closeOnce.Do(func() {
		if w.conn != nil {
			w.closeErr = w.conn.Close()
		}
	})
	return w.closeErr
}

func jsonEOF(decoder *json.Decoder) bool {
	var trailing any
	return errors.Is(decoder.Decode(&trailing), io.EOF)
}
