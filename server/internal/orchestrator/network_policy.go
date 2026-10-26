package orchestrator

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"morde/server/internal/app"
)

const networkPolicyCheckTimeout = 10 * time.Second

func CheckNetworkPolicy(ctx context.Context, path, baseURL string) error {
	if ctx == nil || strings.TrimSpace(path) == "" || !filepath.IsAbs(path) || strings.TrimSpace(baseURL) == "" {
		return networkPolicyError()
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return networkPolicyError()
	}
	checkCtx, cancel := context.WithTimeout(ctx, networkPolicyCheckTimeout)
	defer cancel()
	if err := checkCtx.Err(); err != nil {
		return networkPolicyError()
	}
	command := exec.CommandContext(checkCtx, path, "check")
	command.Env = make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "CH_OSB_URL=") {
			continue
		}
		command.Env = append(command.Env, entry)
	}
	command.Env = append(command.Env, "CH_OSB_URL="+baseURL)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return networkPolicyError()
	}
	return nil
}

func networkPolicyError() error {
	return fmt.Errorf("network policy guard check failed: %w", app.ErrSandboxUnavailable)
}
