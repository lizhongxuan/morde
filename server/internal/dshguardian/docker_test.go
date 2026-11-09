package dshguardian

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"morde/server/internal/dockerapi"
)

type dockerRoundTripFunc func(*http.Request) (*http.Response, error)

func (f dockerRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestDockerEngineListsContainersUsingExactOpenSandboxLabel(t *testing.T) {
	client := &http.Client{Transport: dockerRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path == "/version" {
			return dockerTestResponse(http.StatusOK, `{"ApiVersion":"1.44","MinAPIVersion":"1.24"}`), nil
		}
		if request.URL.Path != "/v1.44/containers/json" || request.URL.Query().Get("all") != "1" {
			t.Fatalf("unexpected Docker Engine list request: %s", request.URL.String())
		}
		filters := request.URL.Query().Get("filters")
		if !strings.Contains(filters, `"label":["opensandbox.io/id=sbx_123456"]`) {
			t.Fatalf("Docker Engine filter did not pin the OpenSandbox identity label: %q", filters)
		}
		return dockerTestResponse(http.StatusOK,
			`[{"Id":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","Names":["/sandbox-sbx_123456"],"Labels":{"opensandbox.io/id":"sbx_123456"}}]`), nil
	})}
	engine := &dockerEngine{client: client, api: dockerapi.NewEndpointResolver(client)}
	values, err := engine.listByLabel(context.Background(), openSandboxContainerIDLabel, "sbx_123456")
	if err != nil {
		t.Fatalf("list OpenSandbox Task containers: %v", err)
	}
	if len(values) != 1 || values[0].ID != strings.Repeat("c", 64) || len(values[0].Names) != 1 || values[0].Names[0] != "/sandbox-sbx_123456" {
		t.Fatalf("Docker Engine returned unexpected Task container summary: %+v", values)
	}
}

func dockerTestResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
