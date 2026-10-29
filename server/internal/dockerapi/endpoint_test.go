package dockerapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestEndpointNegotiatesHighestCompatibleAPIVersion(t *testing.T) {
	for _, test := range []struct {
		name, serverMax, serverMin, want string
	}{
		{name: "Docker 24", serverMax: "1.43", serverMin: "1.12", want: "http://docker/v1.43/info"},
		{name: "Docker 29 with raised minimum", serverMax: "1.52", serverMin: "1.44", want: "http://docker/v1.44/info"},
		{name: "newer Docker", serverMax: "1.55", serverMin: "1.40", want: "http://docker/v1.44/info"},
	} {
		t.Run(test.name, func(t *testing.T) {
			versionRequests := 0
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Path != "/version" {
					t.Fatalf("unexpected request path %q", request.URL.Path)
				}
				versionRequests++
				body := fmt.Sprintf(`{"ApiVersion":%q,"MinAPIVersion":%q}`, test.serverMax, test.serverMin)
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			resolver := NewEndpointResolver(client)
			for range 2 {
				got, err := resolver.Endpoint(context.Background(), "/info")
				if err != nil || got != test.want {
					t.Fatalf("Endpoint() = %q, %v; want %q", got, err, test.want)
				}
			}
			if versionRequests != 1 {
				t.Fatalf("Docker version requests = %d, want 1", versionRequests)
			}
		})
	}
}

func TestEndpointRejectsUnsupportedAndMalformedVersions(t *testing.T) {
	for _, body := range []string{
		`{"ApiVersion":"1.42","MinAPIVersion":"1.12"}`,
		`{"ApiVersion":"1.52","MinAPIVersion":"1.45"}`,
		`{"ApiVersion":"not-a-version"}`,
		`{"ApiVersion":"1.52","MinAPIVersion":"invalid"}`,
		"not-json",
	} {
		t.Run(body, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			if _, err := NewEndpointResolver(client).Endpoint(context.Background(), "/info"); err == nil {
				t.Fatal("Endpoint() accepted an unsupported or malformed Engine API version")
			}
		})
	}
}
