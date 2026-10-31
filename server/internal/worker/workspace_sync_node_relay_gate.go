package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"morde/server/internal/domain"
)

const workspaceSyncNetworkPolicyScript = "/opt/cloudhost/network-policy.sh"

type commandWorkspaceSyncRelayGate struct{}

func NewCommandWorkspaceSyncRelayGate() WorkspaceSyncRelayGate {
	return commandWorkspaceSyncRelayGate{}
}

func (commandWorkspaceSyncRelayGate) Enable(ctx context.Context, ownerID string) error {
	return runWorkspaceSyncRelayGate(ctx, "relay-enable-stdin", ownerID)
}

func (commandWorkspaceSyncRelayGate) Disable(ctx context.Context, ownerID string) error {
	return runWorkspaceSyncRelayGate(ctx, "relay-disable-stdin", ownerID)
}

func runWorkspaceSyncRelayGate(ctx context.Context, action, ownerID string) error {
	if ctx == nil || (action != "relay-enable-stdin" && action != "relay-disable-stdin") || !domain.ValidUserID(ownerID) {
		return errors.New("invalid Workspace sync relay gate request")
	}
	ownerHash := sha256.Sum256([]byte(ownerID))
	command := exec.CommandContext(ctx, workspaceSyncNetworkPolicyScript, action)
	if os.Geteuid() != 0 {
		command = exec.CommandContext(ctx, "/usr/bin/sudo", "-n", workspaceSyncNetworkPolicyScript, action)
	}
	command.Stdin = strings.NewReader(hex.EncodeToString(ownerHash[:]) + "\n")
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("workspace sync relay gate %s failed: %w (%s)", strings.TrimPrefix(action, "relay-"), err, strings.TrimSpace(string(output)))
	}
	return nil
}
