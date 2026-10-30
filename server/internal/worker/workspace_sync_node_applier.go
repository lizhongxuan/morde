package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"morde/server/internal/domain"
	"morde/server/internal/domain/workspace"
)

const (
	workspaceSyncNodeFolderRoot = "/workspaces/"
	workspaceSyncDeviceWait     = 10 * time.Second
)

type syncthingFolderConfig struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Path    string `json:"path"`
	Type    string `json:"type"`
	Paused  bool   `json:"paused"`
	Devices []struct {
		DeviceID string `json:"deviceID"`
	} `json:"devices"`
}

type syncthingDeviceConfig struct {
	DeviceID string `json:"deviceID"`
	Paused   bool   `json:"paused"`
}

type syncthingConnections struct {
	Connections map[string]struct {
		Connected bool `json:"connected"`
		Paused    bool `json:"paused"`
	} `json:"connections"`
}

func applyWorkspaceSyncNodeDesiredState(ctx context.Context, client *SyncthingAPIClient, state workspace.SyncNodeDesiredState, localDeviceID string) error {
	if ctx == nil || client == nil || !domain.ValidUserID(state.OwnerID) || state.Revision < 1 || !validSyncthingDeviceID(localDeviceID) {
		return errors.New("invalid workspace sync node desired state")
	}

	desiredFolders := make(map[string]workspace.SyncNodeFolder, len(state.Folders))
	desiredDevices := make(map[string]struct{})
	desiredMembership := make(map[string]map[string]struct{})
	for _, folder := range state.Folders {
		if !domain.ValidWorkspaceID(folder.WorkspaceID) || len(folder.FolderID) != 38 || !strings.HasPrefix(folder.FolderID, "morde-") ||
			!validSyncthingPathSegment(folder.FolderID) || len(folder.DeviceIDs) == 0 {
			return errors.New("invalid Workspace sync folder in desired state")
		}
		if _, exists := desiredFolders[folder.FolderID]; exists {
			return errors.New("duplicate Workspace sync folder in desired state")
		}
		ids := slices.Clone(folder.DeviceIDs)
		slices.Sort(ids)
		ids = slices.Compact(ids)
		if len(ids) == 0 {
			return errors.New("Workspace sync folder has no authorized devices")
		}
		if slices.Contains(ids, localDeviceID) {
			return errors.New("local Syncthing device cannot be registered as a remote device")
		}
		folder.DeviceIDs = append(ids, localDeviceID)
		slices.Sort(folder.DeviceIDs)
		folder.DeviceIDs = slices.Compact(folder.DeviceIDs)
		desiredFolders[folder.FolderID] = folder
		for _, deviceID := range ids {
			if !validSyncthingDeviceID(deviceID) {
				return errors.New("invalid Syncthing device in desired state")
			}
			desiredDevices[deviceID] = struct{}{}
			if desiredMembership[deviceID] == nil {
				desiredMembership[deviceID] = make(map[string]struct{})
			}
			desiredMembership[deviceID][folder.FolderID] = struct{}{}
		}
		if desiredMembership[localDeviceID] == nil {
			desiredMembership[localDeviceID] = make(map[string]struct{})
		}
		desiredMembership[localDeviceID][folder.FolderID] = struct{}{}
	}

	existingFolders, err := client.GetFolders(ctx)
	if err != nil {
		return fmt.Errorf("list Syncthing folders: %w", err)
	}
	existingDevices, err := client.GetDevices(ctx)
	if err != nil {
		return fmt.Errorf("list Syncthing devices: %w", err)
	}
	folders := make(map[string]syncthingFolderConfig, len(existingFolders))
	devices := make(map[string]syncthingDeviceConfig, len(existingDevices))
	oldMembership := make(map[string]map[string]struct{})
	for _, raw := range existingFolders {
		var folder syncthingFolderConfig
		if err := json.Unmarshal(raw, &folder); err != nil || !validSyncthingPathSegment(folder.ID) {
			return errors.New("Syncthing returned an invalid folder configuration")
		}
		if _, exists := folders[folder.ID]; exists {
			return errors.New("Syncthing returned duplicate folder configurations")
		}
		folders[folder.ID] = folder
		for _, item := range folder.Devices {
			if !validSyncthingDeviceID(item.DeviceID) {
				return errors.New("Syncthing returned an invalid folder device")
			}
			if oldMembership[item.DeviceID] == nil {
				oldMembership[item.DeviceID] = make(map[string]struct{})
			}
			oldMembership[item.DeviceID][folder.ID] = struct{}{}
		}
	}
	for _, raw := range existingDevices {
		var device syncthingDeviceConfig
		if err := json.Unmarshal(raw, &device); err != nil || !validSyncthingDeviceID(device.DeviceID) {
			return errors.New("Syncthing returned an invalid device configuration")
		}
		if _, exists := devices[device.DeviceID]; exists {
			return errors.New("Syncthing returned duplicate device configurations")
		}
		devices[device.DeviceID] = device
	}

	affectedDevices := make([]string, 0)
	for deviceID, folders := range oldMembership {
		if deviceID == localDeviceID {
			continue
		}
		if _, configured := devices[deviceID]; !configured || sameSet(folders, desiredMembership[deviceID]) {
			continue
		}
		affectedDevices = append(affectedDevices, deviceID)
	}
	slices.Sort(affectedDevices)
	for _, deviceID := range affectedDevices {
		if err := client.PauseDevice(ctx, deviceID); err != nil {
			return fmt.Errorf("pause changed Syncthing device: %w", err)
		}
		if err := waitSyncthingDevicePaused(ctx, client, deviceID); err != nil {
			return err
		}
	}

	defaultDevice, err := client.GetDefaultDevice(ctx)
	if err != nil {
		return fmt.Errorf("read Syncthing default device: %w", err)
	}
	for _, deviceID := range sortedKeys(desiredDevices) {
		_, exists := devices[deviceID]
		raw := defaultDevice
		if exists {
			raw, err = client.GetDevice(ctx, deviceID)
			if err != nil {
				return fmt.Errorf("read Syncthing device configuration: %w", err)
			}
		}
		configured, err := configureSyncthingDevice(raw, deviceID)
		if err != nil {
			return err
		}
		if !exists || !currentMatches(configured, raw) {
			if err := client.PutDevice(ctx, deviceID, configured); err != nil {
				return fmt.Errorf("apply Syncthing device configuration: %w", err)
			}
		}
	}

	defaultFolder, err := client.GetDefaultFolder(ctx)
	if err != nil {
		return fmt.Errorf("read Syncthing default folder: %w", err)
	}
	for _, folderID := range sortedKeys(desiredFolders) {
		desired := desiredFolders[folderID]
		current, exists := folders[folderID]
		raw := defaultFolder
		if exists {
			raw, err = client.GetFolder(ctx, folderID)
			if err != nil {
				return fmt.Errorf("read Syncthing folder configuration: %w", err)
			}
		}
		configured, err := configureSyncthingFolder(raw, desired)
		if err != nil {
			return err
		}
		if !exists || !currentFolderMatches(configured, current) {
			if err := client.PutFolder(ctx, folderID, configured); err != nil {
				return fmt.Errorf("apply Syncthing folder configuration: %w", err)
			}
		}
	}

	for _, folderID := range sortedKeys(folders) {
		if _, keep := desiredFolders[folderID]; keep {
			continue
		}
		if err := client.PauseFolder(ctx, folderID); err != nil {
			return fmt.Errorf("pause revoked Syncthing folder: %w", err)
		}
		if err := client.RemoveFolder(ctx, folderID); err != nil {
			return fmt.Errorf("remove revoked Syncthing folder: %w", err)
		}
	}

	for _, deviceID := range sortedKeys(devices) {
		if deviceID == localDeviceID {
			continue
		}
		if _, keep := desiredDevices[deviceID]; keep {
			continue
		}
		if _, alreadyPaused := slices.BinarySearch(affectedDevices, deviceID); !alreadyPaused {
			if err := client.PauseDevice(ctx, deviceID); err != nil {
				return fmt.Errorf("pause revoked Syncthing device: %w", err)
			}
			if err := waitSyncthingDevicePaused(ctx, client, deviceID); err != nil {
				return err
			}
		}
		if err := client.RemoveDevice(ctx, deviceID); err != nil {
			return fmt.Errorf("remove revoked Syncthing device: %w", err)
		}
	}

	for _, deviceID := range sortedKeys(desiredDevices) {
		if err := client.ResumeDevice(ctx, deviceID); err != nil {
			return fmt.Errorf("resume authorized Syncthing device: %w", err)
		}
	}

	if err := verifySyncthingNodeConfig(ctx, client, desiredFolders, desiredDevices, localDeviceID); err != nil {
		return err
	}
	return nil
}

func configureSyncthingDevice(raw json.RawMessage, deviceID string) (json.RawMessage, error) {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil || len(value) == 0 {
		return nil, errors.New("invalid Syncthing default device configuration")
	}
	encoded, _ := json.Marshal(deviceID)
	value["deviceID"] = encoded
	name, _ := json.Marshal("Morde")
	value["name"] = name
	addresses, _ := json.Marshal([]string{"dynamic"})
	value["addresses"] = addresses
	value["autoAcceptFolders"] = json.RawMessage("false")
	value["introducer"] = json.RawMessage("false")
	value["paused"] = json.RawMessage("false")
	return json.Marshal(value)
}

func configureSyncthingFolder(raw json.RawMessage, desired workspace.SyncNodeFolder) (json.RawMessage, error) {
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil || len(value) == 0 {
		return nil, errors.New("invalid Syncthing default folder configuration")
	}
	for key, item := range map[string]string{
		"id": desired.FolderID, "label": "Morde Workspace", "path": workspaceSyncNodeFolderRoot + desired.WorkspaceID, "type": "sendreceive",
	} {
		encoded, _ := json.Marshal(item)
		value[key] = encoded
	}
	value["paused"] = json.RawMessage("false")
	devices := make([]struct {
		DeviceID string `json:"deviceID"`
	}, 0, len(desired.DeviceIDs))
	for _, id := range desired.DeviceIDs {
		devices = append(devices, struct {
			DeviceID string `json:"deviceID"`
		}{DeviceID: id})
	}
	encoded, err := json.Marshal(devices)
	if err != nil {
		return nil, errors.New("encode Syncthing folder device list")
	}
	value["devices"] = encoded
	return json.Marshal(value)
}

func waitSyncthingDevicePaused(ctx context.Context, client *SyncthingAPIClient, deviceID string) error {
	deadline := time.NewTimer(workspaceSyncDeviceWait)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		connections, err := client.Connections(ctx)
		if err != nil {
			return fmt.Errorf("read Syncthing connection state: %w", err)
		}
		var state syncthingConnections
		if err := json.Unmarshal(connections, &state); err != nil || state.Connections == nil {
			return errors.New("Syncthing returned invalid connection state")
		}
		connection, exists := state.Connections[deviceID]
		if !exists {
			return errors.New("Syncthing connection state omitted a configured device")
		}
		if connection.Paused && !connection.Connected {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("Syncthing device connection did not stop after pause")
		case <-ticker.C:
		}
	}
}

func verifySyncthingNodeConfig(ctx context.Context, client *SyncthingAPIClient, folders map[string]workspace.SyncNodeFolder, devices map[string]struct{}, localDeviceID string) error {
	actualFolders, err := client.GetFolders(ctx)
	if err != nil {
		return fmt.Errorf("verify Syncthing folders: %w", err)
	}
	actualDevices, err := client.GetDevices(ctx)
	if err != nil {
		return fmt.Errorf("verify Syncthing devices: %w", err)
	}
	if len(actualFolders) != len(folders) || len(actualDevices) != len(devices)+1 {
		return errors.New("Syncthing configuration does not match the authorized Workspace snapshot")
	}
	for _, raw := range actualFolders {
		var actual syncthingFolderConfig
		if err := json.Unmarshal(raw, &actual); err != nil {
			return errors.New("Syncthing returned invalid folder configuration")
		}
		want, exists := folders[actual.ID]
		if !exists || actual.Label != "Morde Workspace" || actual.Path != workspaceSyncNodeFolderRoot+want.WorkspaceID ||
			actual.Type != "sendreceive" || actual.Paused || !sameDeviceIDs(actual.Devices, want.DeviceIDs) {
			return errors.New("Syncthing folder configuration does not match the authorized Workspace snapshot")
		}
	}
	seenDevices := make(map[string]struct{}, len(actualDevices))
	for _, raw := range actualDevices {
		var actual syncthingDeviceConfig
		if err := json.Unmarshal(raw, &actual); err != nil {
			return errors.New("Syncthing returned invalid device configuration")
		}
		if _, duplicate := seenDevices[actual.DeviceID]; duplicate {
			return errors.New("Syncthing returned duplicate device configurations")
		}
		seenDevices[actual.DeviceID] = struct{}{}
		if actual.DeviceID == localDeviceID {
			if actual.Paused {
				return errors.New("Syncthing local device configuration is paused")
			}
			continue
		}
		if _, exists := devices[actual.DeviceID]; !exists || actual.Paused {
			return errors.New("Syncthing device configuration does not match the authorized Workspace snapshot")
		}
	}
	return nil
}

func sameDeviceIDs(actual []struct {
	DeviceID string `json:"deviceID"`
}, expected []string) bool {
	actualIDs := make([]string, 0, len(actual))
	for _, device := range actual {
		actualIDs = append(actualIDs, device.DeviceID)
	}
	slices.Sort(actualIDs)
	return slices.Equal(actualIDs, expected)
}

func sameSet(first, second map[string]struct{}) bool {
	if len(first) != len(second) {
		return false
	}
	for value := range first {
		if _, exists := second[value]; !exists {
			return false
		}
	}
	return true
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func currentMatches(want, current json.RawMessage) bool {
	var wantValue, currentValue any
	return json.Unmarshal(want, &wantValue) == nil && json.Unmarshal(current, &currentValue) == nil && reflect.DeepEqual(wantValue, currentValue)
}

func currentFolderMatches(want json.RawMessage, current syncthingFolderConfig) bool {
	var expected syncthingFolderConfig
	if json.Unmarshal(want, &expected) != nil {
		return false
	}
	return expected.ID == current.ID && expected.Label == current.Label && expected.Path == current.Path &&
		expected.Type == current.Type && expected.Paused == current.Paused && sameDeviceIDs(expected.Devices, deviceIDsFromConfig(current.Devices))
}

func deviceIDsFromConfig(input []struct {
	DeviceID string `json:"deviceID"`
}) []string {
	ids := make([]string, 0, len(input))
	for _, device := range input {
		ids = append(ids, device.DeviceID)
	}
	slices.Sort(ids)
	return ids
}

func validSyncthingDeviceID(value string) bool {
	if len(value) != 63 {
		return false
	}
	for index, char := range value {
		if (index+1)%8 == 0 {
			if char != '-' {
				return false
			}
			continue
		}
		if (char < 'A' || char > 'Z') && (char < '2' || char > '7') {
			return false
		}
	}
	return true
}
