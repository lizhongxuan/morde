package runtimeclient

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"morde-runtime/runtimeidentity"
)

func TestNewHostHTTPClientPinsHostTLSAndAddsRouteHeaders(t *testing.T) {
	serverTLS, err := runtimeidentity.ServerTLSConfig(runtimeCredential, hostID)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Route") != "sandbox-1" {
			t.Errorf("route header = %q", request.Header.Get("X-Route"))
		}
		if request.Header.Get("Authorization") != "Bearer runtime-secret" {
			t.Errorf("authorization header = %q", request.Header.Get("Authorization"))
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	server.TLS = serverTLS
	server.StartTLS()
	defer server.Close()

	client, err := NewHostHTTPClient(runtimeCredential, hostID, map[string]string{"X-Route": "sandbox-1"})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(mustRequest(t, server.URL))
	if err != nil {
		t.Fatalf("pinned TLS request failed: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", response.StatusCode)
	}

	wrongHostClient, err := NewHostHTTPClient(runtimeCredential, "habc1234", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err = wrongHostClient.Do(mustRequest(t, server.URL))
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil {
		t.Fatal("client with another Host's TLS identity was accepted")
	}
}

func TestNewHostHTTPClientRejectsProtocolHeaderOverrides(t *testing.T) {
	for _, header := range []string{"Authorization", "X-Morde-Runtime-Protocol", "Host", "Content-Length", "Connection"} {
		if _, err := NewHostHTTPClient(runtimeCredential, hostID, map[string]string{header: "override"}); err == nil {
			t.Errorf("NewHostHTTPClient accepted route header %q", header)
		}
	}
	if _, err := NewHostHTTPClient(runtimeCredential, hostID, map[string]string{"X-Route": "bad\r\nvalue"}); err == nil {
		t.Fatal("NewHostHTTPClient accepted a malformed route header")
	}
}

func TestNewHostHTTPClientDoesNotForwardHeadersAcrossRedirect(t *testing.T) {
	var targetSawRequest atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		targetSawRequest.Store(true)
		if request.Header.Get("Authorization") != "" || request.Header.Get("X-Route") != "" {
			t.Errorf("redirect target received sensitive headers: %#v", request.Header)
		}
	}))
	defer target.Close()

	serverTLS, err := runtimeidentity.ServerTLSConfig(runtimeCredential, hostID)
	if err != nil {
		t.Fatal(err)
	}
	source := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusFound)
	}))
	source.TLS = serverTLS
	source.StartTLS()
	defer source.Close()

	client, err := NewHostHTTPClient(runtimeCredential, hostID, map[string]string{"X-Route": "sandbox-1"})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(mustRequest(t, source.URL))
	if err != nil {
		t.Fatalf("pinned TLS request failed: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusFound {
		t.Fatalf("redirect status = %d, want %d", response.StatusCode, http.StatusFound)
	}
	if targetSawRequest.Load() {
		t.Fatal("client followed redirect and forwarded Host-scoped headers")
	}
}

func mustRequest(t *testing.T, rawURL string) *http.Request {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer runtime-secret")
	return request
}
