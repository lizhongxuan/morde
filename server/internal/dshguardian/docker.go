package dshguardian

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"time"
)

const dockerAPIVersion = "/v1.43"

var (
	errContainerNotFound = errors.New("docker container was not found")
	errDockerConflict    = errors.New("docker container state changed")
)

type dockerAPI interface {
	inspect(context.Context, string) (containerInspect, error)
	kill(context.Context, string) error
	wait(context.Context, string) error
	remove(context.Context, string) error
}

type containerInspect struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Running *bool `json:"Running"`
		PID     *int  `json:"Pid"`
	} `json:"State"`
}

type dockerEngine struct {
	client *http.Client
}

func newDockerEngine(socket string) (*dockerEngine, error) {
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || socket == "/" {
		return nil, errors.New("docker socket must be a clean absolute path")
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", socket)
		},
		DisableCompression: true,
	}
	return &dockerEngine{client: &http.Client{Transport: transport, Timeout: 8 * time.Second}}, nil
}

func (d *dockerEngine) inspect(ctx context.Context, id string) (containerInspect, error) {
	var result containerInspect
	status, err := d.request(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", &result)
	if err != nil {
		return result, err
	}
	if status == http.StatusNotFound {
		return result, errContainerNotFound
	}
	if status != http.StatusOK {
		return result, fmt.Errorf("docker inspect returned status %d", status)
	}
	return result, nil
}

func (d *dockerEngine) kill(ctx context.Context, id string) error {
	status, err := d.request(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/kill?signal=SIGKILL", nil)
	if err != nil {
		return err
	}
	if status == http.StatusConflict {
		return errDockerConflict
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("docker kill returned status %d", status)
	}
	return nil
}

func (d *dockerEngine) wait(ctx context.Context, id string) error {
	var result struct {
		StatusCode *int `json:"StatusCode"`
		Error      *struct {
			Message string `json:"Message"`
		} `json:"Error"`
	}
	status, err := d.request(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/wait?condition=not-running", &result)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return errContainerNotFound
	}
	if status != http.StatusOK || result.StatusCode == nil || result.Error != nil && result.Error.Message != "" {
		return errors.New("docker wait did not confirm process exit")
	}
	return nil
}

func (d *dockerEngine) remove(ctx context.Context, id string) error {
	status, err := d.request(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id)+"?force=0&v=0", nil)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return errContainerNotFound
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("docker remove returned status %d", status)
	}
	return nil
}

func (d *dockerEngine) request(ctx context.Context, method, path string, result any) (int, error) {
	request, err := http.NewRequestWithContext(ctx, method, "http://docker"+dockerAPIVersion+path, nil)
	if err != nil {
		return 0, errors.New("build docker request")
	}
	response, err := d.client.Do(request)
	if err != nil {
		return 0, fmt.Errorf("docker request failed: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxRequestBytes+1))
	if err != nil || len(data) > maxRequestBytes {
		return response.StatusCode, errors.New("docker response is too large or unreadable")
	}
	if result != nil && response.StatusCode >= 200 && response.StatusCode < 300 {
		if err := json.Unmarshal(data, result); err != nil {
			return response.StatusCode, errors.New("docker response JSON is invalid")
		}
	}
	return response.StatusCode, nil
}
