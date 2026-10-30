package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// StopOwner stops the existing owner node without removing its persistent data.
func (s *DockerWorkspaceSyncNodeSupervisor) StopOwner(ctx context.Context, ownerID string) error {
	if s == nil || ctx == nil || !dockerWorkspaceSyncImageDigest.MatchString(s.options.ImageDigest) ||
		ownerID == "" || len(ownerID) > 256 || strings.TrimSpace(ownerID) != ownerID || strings.ContainsAny(ownerID, "\x00\r\n") {
		return errors.New("invalid workspace sync node owner")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	ownerHash := sha256.Sum256([]byte(ownerID))
	ownerHashHex := hex.EncodeToString(ownerHash[:])
	containerName := "morde-sync-" + ownerHashHex[:32]
	networkName := "morde-sync-net-" + ownerHashHex[:32]
	if s.options.RelayContainerName == containerName {
		return errors.New("workspace sync node conflicts with relay container")
	}

	node, err := s.inspectContainer(ctx, containerName)
	if errors.Is(err, errDockerObjectNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect workspace sync node before stop: %w", err)
	}
	if err := s.validateOwnerNodeIdentity(ctx, node, ownerHashHex, containerName, networkName); err != nil {
		return err
	}
	if !validWorkspaceSyncStopState(node.State) {
		return errors.New("workspace sync node state is unavailable")
	}
	if !*node.State.Running && !*node.State.Restarting && !*node.State.Paused {
		return nil
	}

	stopErr := s.requestJSON(ctx, http.MethodPost, "/containers/"+url.PathEscape(node.ID)+"/stop?t=3", nil, http.StatusNoContent, nil)
	stopped, inspectErr := s.inspectContainer(ctx, node.ID)
	if errors.Is(inspectErr, errDockerObjectNotFound) {
		return nil
	}
	if inspectErr != nil {
		return fmt.Errorf("inspect workspace sync node after stop: %w", inspectErr)
	}
	if err := s.validateOwnerNodeIdentity(ctx, stopped, ownerHashHex, containerName, networkName); err != nil {
		return err
	}
	if !validWorkspaceSyncStopState(stopped.State) {
		return errors.New("workspace sync node state is unavailable after stop")
	}
	if !*stopped.State.Running && !*stopped.State.Restarting && !*stopped.State.Paused {
		return nil
	}
	if stopErr != nil {
		return fmt.Errorf("stop workspace sync node: %w", stopErr)
	}
	return errors.New("workspace sync node remained active after stop")
}

func (s *DockerWorkspaceSyncNodeSupervisor) validateOwnerNodeIdentity(ctx context.Context, node dockerWorkspaceSyncInspect, ownerHash, containerName, networkName string) error {
	if !dockerWorkspaceSyncContainerID.MatchString(node.ID) || node.Name != "/"+containerName ||
		node.Config.Image != s.options.ImageDigest || node.Config.Labels[dockerWorkspaceSyncLabelManaged] != "true" ||
		node.Config.Labels[dockerWorkspaceSyncLabelOwner] != ownerHash || node.Config.Labels[dockerWorkspaceSyncLabelImage] != s.options.ImageDigest ||
		node.HostConfig.NetworkMode != networkName || len(node.NetworkSettings.Networks) != 1 ||
		!hasWorkspaceSyncNetworkAlias(node.NetworkSettings.Networks, networkName, dockerWorkspaceSyncContainerAlias) {
		return errors.New("workspace sync node identity mismatch")
	}
	if _, ok := dockerWorkspaceSyncPrivateIPv4(node, networkName); !ok {
		return errors.New("workspace sync node network identity mismatch")
	}
	network, err := s.inspectNetwork(ctx, networkName)
	if err != nil {
		return fmt.Errorf("inspect workspace sync node network: %w", err)
	}
	if !validDockerWorkspaceSyncNetwork(network, networkName, ownerHash, dockerWorkspaceSyncBridgeName(ownerHash)) ||
		validWorkspaceSyncNetworkMembers(network, node.ID, containerName, true) != nil {
		return errors.New("workspace sync node network identity mismatch")
	}
	return nil
}

func validWorkspaceSyncStopState(state *dockerWorkspaceSyncContainerState) bool {
	return state != nil && state.Running != nil && state.Restarting != nil && state.Paused != nil
}
