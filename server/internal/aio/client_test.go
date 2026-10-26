package aio

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"morde/server/internal/app"
)

func TestDoJSONUnwrapsEnvelopeAndInjectsHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sandbox/v1/shell/sessions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("X-Route") != "route-secret" || r.Header.Get(HeaderAPIKey) != "aio-secret" {
			t.Errorf("upstream headers missing")
		}
		_, _ = io.WriteString(w, `{"success":true,"message":"ok","data":{"count":2},"hint":""}`)
	}))
	defer server.Close()

	var output struct {
		Count int `json:"count"`
	}
	err := New(nil).DoJSON(context.Background(), app.Upstream{
		BaseURL: strings.TrimRight(server.URL, "/") + "/sandbox", Headers: map[string]string{"X-Route": "route-secret"}, APIKey: "aio-secret",
	}, http.MethodGet, "/v1/shell/sessions", nil, &output)
	if err != nil || output.Count != 2 {
		t.Fatalf("DoJSON() = %#v, %v", output, err)
	}
}

func TestDoJSONUnsuccessfulDoesNotEchoAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"success":false,"message":"bad aio-secret","data":null,"hint":""}`)
	}))
	defer server.Close()
	err := New(nil).DoJSON(context.Background(), app.Upstream{BaseURL: server.URL, APIKey: "aio-secret"}, http.MethodGet, "/v1/x", nil, nil)
	if err == nil || strings.Contains(err.Error(), "aio-secret") || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDoStreamsAndReadyRequires200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/binary" {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte{0, 1, 2})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client := New(nil)
	request, _ := http.NewRequest(http.MethodGet, "/v1/binary", nil)
	response, err := client.Do(context.Background(), app.Upstream{BaseURL: server.URL}, request)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || len(data) != 3 {
		t.Fatalf("stream data = %v, %v", data, err)
	}
	if err := client.Ready(context.Background(), app.Upstream{BaseURL: server.URL}); err == nil {
		t.Fatal("Ready accepted 204")
	}
}

func TestDoRejectsExternalRequestURL(t *testing.T) {
	request, _ := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	response, err := New(nil).Do(context.Background(), app.Upstream{BaseURL: "http://127.0.0.1"}, request)
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "request path") {
		t.Fatalf("Do() error = %v", err)
	}
}

func TestDoJSONHandlesRequestAndEnvelopeBoundaries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/input":
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Accept") != "application/json" {
				t.Errorf("request headers = %s %#v", r.Method, r.Header)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil || string(body) != "{\"name\":\"Gem\"}" {
				t.Errorf("request body = %s, %v", body, err)
			}
			_, _ = io.WriteString(w, "{\"success\":true,\"data\":{\"name\":\"Ada\"}}")
		case "/failed":
			_, _ = io.WriteString(w, "{\"success\":false,\"message\":\"  \",\"data\":null}")
		case "/invalid":
			_, _ = io.WriteString(w, "not-json")
		case "/bad-data":
			_, _ = io.WriteString(w, "{\"success\":true,\"data\":{\"name\":3}}")
		case "/null-data":
			_, _ = io.WriteString(w, "{\"success\":true,\"data\":null}")
		case "/oversized":
			_, _ = io.WriteString(w, `{"success":true,"data":"`+strings.Repeat("x", maxJSONResponseBytes)+`"}`)
		case "/status":
			w.WriteHeader(http.StatusBadGateway)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := New(nil)
	var output struct{ Name string }
	if err := client.DoJSON(context.Background(), app.Upstream{BaseURL: server.URL}, http.MethodPost, "/input", map[string]string{"name": "Gem"}, &output); err != nil || output.Name != "Ada" {
		t.Fatalf("input DoJSON() = %+v, %v", output, err)
	}

	for _, tc := range []struct {
		path string
		want string
	}{
		{path: "/failed", want: "aio request failed: request failed"},
		{path: "/invalid", want: "decode aio response"},
		{path: "/bad-data", want: "decode aio data"},
		{path: "/status", want: "aio returned status 502"},
		{path: "/oversized", want: "aio response is too large"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			if err := client.DoJSON(context.Background(), app.Upstream{BaseURL: server.URL}, http.MethodGet, tc.path, nil, &output); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("DoJSON() error = %v, want %q", err, tc.want)
			}
		})
	}

	output.Name = "unchanged"
	if err := client.DoJSON(context.Background(), app.Upstream{BaseURL: server.URL}, http.MethodGet, "/null-data", nil, &output); err != nil || output.Name != "unchanged" {
		t.Fatalf("null data changed output = %+v, %v", output, err)
	}
	if err := client.DoJSON(context.Background(), app.Upstream{BaseURL: server.URL}, "bad method", "/input", nil, nil); err == nil || !strings.Contains(err.Error(), "build aio request") {
		t.Fatalf("invalid request error = %v", err)
	}
	if err := client.DoJSON(context.Background(), app.Upstream{BaseURL: server.URL}, http.MethodGet, "/input", make(chan int), nil); err == nil || !strings.Contains(err.Error(), "encode aio request") {
		t.Fatalf("unencodable input error = %v", err)
	}
}

func TestTerminalSessionIDAcceptsOnlyFixedSameOriginURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/shell/terminal-url" {
			t.Errorf("terminal request = %s %s", r.Method, r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"success":true,"data":"/terminal?session_id=550e8400-e29b-41d4-a716-446655440000"}`)
	}))
	defer server.Close()

	sessionID, err := New(nil).TerminalSessionID(context.Background(), app.Upstream{BaseURL: server.URL})
	if err != nil || sessionID != "550e8400-e29b-41d4-a716-446655440000" {
		t.Fatalf("TerminalSessionID() = %q, %v", sessionID, err)
	}

	for _, raw := range []string{
		"https://evil.example/terminal?session_id=abc",
		"/other?session_id=abc",
		"//evil.example/terminal?session_id=abc",
		"/terminal?session_id=abc&token=secret",
		"/terminal?session_id=abc&session_id=def",
		"/terminal?session_id=bad%2Fid",
		"/terminal?session_id=abc#fragment",
		"/terminal/%2e%2e?session_id=abc",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseTerminalSessionID(server.URL, raw); err == nil {
				t.Fatalf("parseTerminalSessionID(%q) succeeded", raw)
			}
		})
	}

	accepted, err := parseTerminalSessionID(server.URL, server.URL+"/terminal?session_id=abc_-.~")
	if err != nil || accepted != "abc_-.~" {
		t.Fatalf("same-origin absolute URL = %q, %v", accepted, err)
	}
}

func TestDoAndReadyReportInvalidAndCanceledRequests(t *testing.T) {
	client := New(nil)
	for _, request := range []*http.Request{nil, {Method: http.MethodGet}} {
		response, err := client.Do(context.Background(), app.Upstream{BaseURL: "http://127.0.0.1"}, request)
		if response != nil {
			_ = response.Body.Close()
		}
		if err == nil || response != nil {
			t.Fatalf("Do(%v) = %v, %v", request, response, err)
		}
	}

	request, err := http.NewRequest(http.MethodGet, "/relative", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, upstream := range []app.Upstream{
		{BaseURL: "ftp://example.com"},
		{BaseURL: "http://example.com"},
	} {
		if upstream.BaseURL == "http://example.com" {
			request.URL.Host = "elsewhere.example"
		}
		response, err := client.Do(context.Background(), upstream, request)
		if response != nil {
			_ = response.Body.Close()
		}
		if err == nil || response != nil {
			t.Fatalf("Do(%+v) = %v, %v", upstream, response, err)
		}
		request.URL.Host = ""
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.Ready(ctx, app.Upstream{BaseURL: "http://127.0.0.1:1"}); err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("Ready(canceled) error = %v", err)
	}
	if err := client.Ready(context.Background(), app.Upstream{BaseURL: "invalid"}); err == nil || !strings.Contains(err.Error(), "check aio readiness") {
		t.Fatalf("Ready(invalid upstream) error = %v", err)
	}
}
