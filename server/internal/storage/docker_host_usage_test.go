package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"morde/server/internal/app"
)

func TestDockerSampleHostUsageSelectsOneLabeledRunningContainer(t *testing.T) {
	const containerID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	stats := `{"read":"2026-11-03T00:00:00.123456789Z","cpu_stats":{"cpu_usage":{"total_usage":7500000000}},"memory_stats":{"usage":536870912,"stats":{"inactive_file":100}}}`
	client := newUnixDockerForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1.44/containers/json":
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("all") != "1" {
				t.Errorf("container list query = %q, want all=1", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode([]dockerUsageContainer{{
				ID: containerID, State: "running", Labels: map[string]string{
					"opensandbox.io/id": "sandbox-a", "ch-host": "h1234567", "ch-generation": "4", "ch-env": "local",
				},
			}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1.44/containers/"+containerID+"/stats":
			w.Header().Set("Content-Type", "application/json")
			if r.URL.RawQuery != "stream=false" {
				t.Errorf("stats query = %q, want stream=false", r.URL.RawQuery)
			}
			_, _ = fmt.Fprint(w, stats)
		default:
			t.Errorf("unexpected Docker request %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
			http.NotFound(w, r)
		}
	}))

	metrics, err := client.SampleHostUsage(context.Background(), "unused", "local", "h1234567", 4, "sandbox-a")
	if err != nil {
		t.Fatalf("SampleHostUsage() = %v", err)
	}
	wantAt := time.Date(2026, 11, 3, 0, 0, 0, 123456789, time.UTC)
	if !metrics.SampledAt.Equal(wantAt) || metrics.CPUUsageTotalNS != 7_500_000_000 || metrics.MemoryUsageBytes != 536_870_912 {
		t.Fatalf("metrics = %#v", metrics)
	}
	if got, want := string(metrics.RawPayload), `{"cpu_usage_total_ns":7500000000,"memory_usage_bytes":536870912}`; got != want {
		t.Fatalf("fixed raw payload = %s, want %s", got, want)
	}
	if err := metrics.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestDockerSampleHostUsageFailsClosedOnContainerIdentity(t *testing.T) {
	const idA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const idB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	base := dockerUsageContainer{ID: idA, State: "running", Labels: map[string]string{
		"opensandbox.io/id": "sandbox-a", "ch-host": "h1234567", "ch-generation": "4", "ch-env": "local",
	}}
	for _, test := range []struct {
		name       string
		containers []dockerUsageContainer
		wantErr    error
	}{
		{name: "no running container", containers: []dockerUsageContainer{{ID: idA, State: "exited", Labels: base.Labels}}, wantErr: app.ErrHostUsageUnavailable},
		{name: "duplicate running container", containers: []dockerUsageContainer{base, {ID: idB, State: "running", Labels: base.Labels}}, wantErr: app.ErrHostUsageUnavailable},
		{name: "wrong host", containers: []dockerUsageContainer{{ID: idA, State: "running", Labels: map[string]string{"opensandbox.io/id": "sandbox-a", "ch-host": "h9999999", "ch-generation": "4", "ch-env": "local"}}}, wantErr: app.ErrHostUsageUnavailable},
		{name: "wrong environment", containers: []dockerUsageContainer{{ID: idA, State: "running", Labels: map[string]string{"opensandbox.io/id": "sandbox-a", "ch-host": "h1234567", "ch-generation": "4", "ch-env": "other"}}}, wantErr: app.ErrHostUsageUnavailable},
		{name: "wrong generation", containers: []dockerUsageContainer{{ID: idA, State: "running", Labels: map[string]string{"opensandbox.io/id": "sandbox-a", "ch-host": "h1234567", "ch-generation": "3", "ch-env": "local"}}}, wantErr: app.ErrHostUsageGenerationChanged},
		{name: "unsafe container id", containers: []dockerUsageContainer{{ID: "../../stats", State: "running", Labels: base.Labels}}, wantErr: app.ErrHostUsageUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			statsRequested := false
			client := newUnixDockerForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1.44/containers/json" {
					statsRequested = true
					t.Errorf("unexpected request before identity selection: %s", r.URL.Path)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(test.containers)
			}))
			_, err := client.SampleHostUsage(context.Background(), "unused", "local", "h1234567", 4, "sandbox-a")
			if err == nil || !errors.Is(err, test.wantErr) {
				t.Fatalf("SampleHostUsage() error = %v, want %v", err, test.wantErr)
			}
			if statsRequested {
				t.Fatal("stats requested without a unique trusted running container")
			}
		})
	}
}

func TestDockerSampleHostUsageRejectsBadStatsAndRedirects(t *testing.T) {
	const containerID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, test := range []struct {
		name        string
		status      int
		contentType string
		body        string
	}{
		{name: "missing counter", status: http.StatusOK, contentType: "application/json", body: `{"read":"2026-11-03T00:00:00Z","cpu_stats":{"cpu_usage":{}},"memory_stats":{"usage":10}}`},
		{name: "negative counter", status: http.StatusOK, contentType: "application/json", body: `{"read":"2026-11-03T00:00:00Z","cpu_stats":{"cpu_usage":{"total_usage":-1}},"memory_stats":{"usage":10}}`},
		{name: "fractional memory", status: http.StatusOK, contentType: "application/json", body: `{"read":"2026-11-03T00:00:00Z","cpu_stats":{"cpu_usage":{"total_usage":1}},"memory_stats":{"usage":1.5}}`},
		{name: "bad timestamp", status: http.StatusOK, contentType: "application/json", body: `{"read":"yesterday","cpu_stats":{"cpu_usage":{"total_usage":1}},"memory_stats":{"usage":10}}`},
		{name: "wrong content type", status: http.StatusOK, contentType: "text/plain", body: `{"read":"2026-11-03T00:00:00Z","cpu_stats":{"cpu_usage":{"total_usage":1}},"memory_stats":{"usage":10}}`},
		{name: "redirect", status: http.StatusTemporaryRedirect, contentType: "application/json", body: `{"read":"2026-11-03T00:00:00Z","cpu_stats":{"cpu_usage":{"total_usage":1}},"memory_stats":{"usage":10}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := newUnixDockerForTest(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1.44/containers/json" {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode([]dockerUsageContainer{{ID: containerID, State: "running", Labels: map[string]string{
						"opensandbox.io/id": "sandbox-a", "ch-host": "h1234567", "ch-generation": "4", "ch-env": "local",
					}}})
					return
				}
				if r.URL.Path == "/v1.44/containers/"+containerID+"/stats" {
					if test.contentType != "" {
						w.Header().Set("Content-Type", test.contentType)
					}
					if test.name == "redirect" {
						w.Header().Set("Location", "http://docker/redirected")
					}
					w.WriteHeader(test.status)
					_, _ = fmt.Fprint(w, test.body)
					return
				}
				t.Errorf("unexpected Docker request %s", r.URL.String())
			}))
			if _, err := client.SampleHostUsage(context.Background(), "unused", "local", "h1234567", 4, "sandbox-a"); err == nil {
				t.Fatal("SampleHostUsage() accepted invalid stats response")
			}
		})
	}
}
